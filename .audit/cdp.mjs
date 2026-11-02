#!/usr/bin/env node
// cdp.mjs — minimal CDP driver for the mobile audit: navigate (with splash
// skipped), measure, screenshot. No packages; talks to headless Chrome's
// DevTools websocket directly.
//
//   node cdp.mjs shot <url> <outfile> [--width 390] [--dpr 2] [--delay 700] [--click "selector"] [--js "expr"] [--scroll selector]
//   node cdp.mjs eval <url> <jsfile> [--width 390]    (prints JSON result of the expression in jsfile)
import { spawn } from "node:child_process"
import { existsSync, mkdirSync, mkdtempSync, writeFileSync, readFileSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, dirname } from "node:path"

const [cmd, url, arg3, ...rest] = process.argv.slice(2)
const opt = (name, def) => { const i = rest.indexOf(`--${name}`); return i >= 0 ? rest[i + 1] : def }
const WIDTH = Number(opt("width", "390"))
const DPR = Number(opt("dpr", "2"))
const DELAY = Number(opt("delay", "700"))
const CLICK = opt("click", "")
const JS = opt("js", "")
const SCROLLSEL = opt("scroll", "")
const HEIGHT = 844

const chrome = process.env.CHROME || [
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
].find(existsSync)
if (!chrome) { console.error("no Chrome"); process.exit(1) }

const profile = mkdtempSync(join(tmpdir(), "amp-audit-cdp-"))
const proc = spawn(chrome, ["--headless=new", "--disable-gpu", "--hide-scrollbars", "--remote-debugging-port=0",
  `--user-data-dir=${profile}`, "about:blank"], { stdio: ["ignore", "ignore", "pipe"] })
const browserWS = await new Promise((ok, fail) => {
  let buf = ""
  proc.stderr.on("data", (d) => {
    buf += d
    const m = buf.match(/DevTools listening on (ws:\/\/\S+)/)
    if (m) ok(m[1])
  })
  proc.on("exit", () => fail(new Error("chrome exited")))
  setTimeout(() => fail(new Error("no ws")), 15000)
})
const ws = new WebSocket(browserWS)
await new Promise((ok) => ws.addEventListener("open", ok, { once: true }))
let seq = 0
const pending = new Map()
ws.addEventListener("message", (ev) => {
  const m = JSON.parse(ev.data)
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id) }
})
const send = (method, params = {}, sessionId) => new Promise((ok) => {
  const id = ++seq
  pending.set(id, ok)
  ws.send(JSON.stringify(sessionId ? { id, method, params, sessionId } : { id, method, params }))
})
const { result: { targetId } } = await send("Target.createTarget", { url: "about:blank" })
const { result: { sessionId } } = await send("Target.attachToTarget", { targetId, flatten: true })
const s = (method, params = {}) => send(method, params, sessionId)
const evaluate = async (expression) =>
  (await s("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true, timeout: 10000 })).result?.result?.value

await s("Page.enable")
// prefers-reduced-motion puts the launch splash in its 700ms reduced mode,
// and seeded localStorage (below) keeps it out of the 6.6s full mode.
await s("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "reduce" }] })
await s("Page.addScriptToEvaluateOnNewDocument", { source: "try{localStorage.setItem('amp.launch.last', String(Date.now()))}catch(e){}" })
await s("Emulation.setDeviceMetricsOverride", { width: WIDTH, height: HEIGHT, deviceScaleFactor: DPR, mobile: WIDTH < 600 })
await s("Emulation.setTouchEmulationEnabled", { enabled: WIDTH < 600, maxTouchPoints: 5 })
const loaded = new Promise((ok) => {
  const h = (ev) => { const m = JSON.parse(ev.data); if (m.method === "Page.loadEventFired") { ws.removeEventListener("message", h); ok() } }
  ws.addEventListener("message", h)
  setTimeout(ok, 15000)
})
await s("Page.navigate", { url })
await loaded
// Skip any splash still up (sends a keydown its window listener catches).
await s("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape" })
await s("Input.dispatchKeyEvent", { type: "keyUp", key: "Escape", code: "Escape" })
await evaluate(`new Promise(r => setTimeout(r, ${DELAY}))`)
if (CLICK) {
  const r = await evaluate(`(el => { const el2 = document.querySelector(${JSON.stringify(CLICK)}); if (!el2) return 'not-found:'+${JSON.stringify(CLICK)}; el2.scrollIntoView({block:'center'}); el2.click(); return 'clicked' })()`)
  if (String(r).startsWith("not-found")) console.error(r)
  await evaluate(`new Promise(r => setTimeout(r, 600))`)
}
if (JS) console.log(JSON.stringify(await evaluate(JS)))
if (cmd === "eval") {
  const expr = readFileSync(arg3, "utf8")
  console.log(JSON.stringify(await evaluate(expr), null, 1))
  cleanup(); process.exit(0)
}
if (SCROLLSEL) {
  await evaluate(`(() => { const el = document.querySelector(${JSON.stringify(SCROLLSEL)}); if (el) el.scrollTop = el.scrollHeight; return true })()`)
  await evaluate(`new Promise(r => setTimeout(r, 300))`)
}
const { result: shot } = await s("Page.captureScreenshot", { format: "png" })
mkdirSync(dirname(arg3), { recursive: true })
writeFileSync(arg3, Buffer.from(shot.data, "base64"))
console.log(arg3)
cleanup()
function cleanup() { try { rmSync(profile, { recursive: true, force: true }) } catch {}; proc.kill(); }
