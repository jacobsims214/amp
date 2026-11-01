#!/usr/bin/env node
// capture.mjs — screenshot every state of a mockup in one call.
//
//   node capture.mjs <page.html|url> [--out dir] [--widths 1440,390]
//                    [--states a,b] [--themes light,dark]
//
// States and themes are discovered from the page's switcher buttons
// ([data-setstate="…"], [data-settheme="…"]) unless given. Every
// width × theme × state is captured full-page as <width>-<theme>-<state>.png
// (390 and other widths under 600 as a 2x mobile viewport). Prints one line
// per file. Drives headless Chrome over the DevTools protocol; no packages.
// Chrome: $CHROME, else the usual install paths.
import { spawn } from "node:child_process"
import { existsSync, mkdirSync, mkdtempSync, writeFileSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { pathToFileURL } from "node:url"

const args = process.argv.slice(2)
const opt = (name, def) => { const i = args.indexOf(`--${name}`); return i >= 0 ? args[i + 1] : def }
const target = args.find((a, i) => !a.startsWith("--") && !(i > 0 && args[i - 1].startsWith("--")))
if (!target) { console.error("usage: capture.mjs <page.html|url> [--out dir] [--widths 1440,390] [--states a,b] [--themes x,y]"); process.exit(2) }
const url = /^[a-z]+:\/\//.test(target) ? target : pathToFileURL(resolve(target)).href
const out = resolve(opt("out", "shots")); mkdirSync(out, { recursive: true })
const widths = opt("widths", "1440,390").split(",").map(Number)
const list = (s) => (s ? s.split(",").map((x) => x.trim()).filter(Boolean) : null)

const chrome = process.env.CHROME || [
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/Applications/Chromium.app/Contents/MacOS/Chromium",
  "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
].find(existsSync)
if (!chrome) { console.error("no Chrome found: set CHROME"); process.exit(1) }

const deadline = setTimeout(() => { console.error("capture timed out after 180s"); process.exit(1) }, 180000)
const profile = mkdtempSync(join(tmpdir(), "amp-capture-"))
const proc = spawn(chrome, ["--headless=new", "--disable-gpu", "--hide-scrollbars", "--remote-debugging-port=0",
  `--user-data-dir=${profile}`, "about:blank"], { stdio: ["ignore", "ignore", "pipe"] })
const browserWS = await new Promise((ok, fail) => {
  let buf = ""
  proc.stderr.on("data", (d) => { buf += d; const m = buf.match(/DevTools listening on (ws:\/\/\S+)/); if (m) ok(m[1]) })
  proc.on("exit", () => fail(new Error("Chrome exited: " + buf.slice(-300))))
})

const ws = new WebSocket(browserWS)
await new Promise((ok) => ws.addEventListener("open", ok, { once: true }))
let seq = 0
const pending = new Map(), waiters = []
ws.addEventListener("message", (e) => {
  const m = JSON.parse(e.data)
  if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.fail(new Error(m.error.message)) : p.ok(m.result) }
  else if (m.method) for (const w of [...waiters]) if (w.method === m.method) { waiters.splice(waiters.indexOf(w), 1); w.ok(m.params) }
})
const send = (method, params = {}, sessionId) => new Promise((ok, fail) => {
  const id = ++seq; pending.set(id, { ok, fail }); ws.send(JSON.stringify({ id, method, params, sessionId }))
})
const once = (method, ms = 15000) => new Promise((ok) => { const w = { method, ok }; waiters.push(w); setTimeout(() => ok(null), ms) })

const { targetId } = await send("Target.createTarget", { url: "about:blank" })
const { sessionId } = await send("Target.attachToTarget", { targetId, flatten: true })
const s = (method, params) => send(method, params, sessionId)
await s("Page.enable")
const evaluate = async (expression) =>
  (await s("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true, timeout: 10000 })).result?.value

async function load(width) {
  const mobile = width < 600
  await s("Emulation.setDeviceMetricsOverride", { width, height: mobile ? 844 : 1000, deviceScaleFactor: mobile ? 2 : 1, mobile })
  const loaded = once("Page.loadEventFired")
  await s("Page.navigate", { url })
  await loaded
  await evaluate("document.fonts ? document.fonts.ready.then(() => true) : true")
}

await load(widths[0])
const found = await evaluate(`({
  states: [...new Set([...document.querySelectorAll('[data-setstate]')].map(e => e.dataset.setstate))],
  themes: [...new Set([...document.querySelectorAll('[data-settheme]')].map(e => e.dataset.settheme))] })`)
const states = list(opt("states")) || (found.states.length ? found.states : ["default"])
const themes = list(opt("themes")) || (found.themes.length ? found.themes : ["default"])

for (const width of widths) {
  await load(width)
  for (const theme of themes) for (const state of states) {
    await evaluate(`(() => {
      const click = (sel) => { const el = document.querySelector(sel); if (el) el.click() };
      ${theme !== "default" ? `click('[data-settheme="${theme}"]');` : ""}
      ${state !== "default" ? `click('[data-setstate="${state}"]');` : ""}
      return new Promise(r => requestAnimationFrame(() => setTimeout(r, 250)));
    })()`)
    const { cssContentSize } = await s("Page.getLayoutMetrics")
    const h = Math.ceil(Math.min(cssContentSize.height, 12000))
    const shot = await s("Page.captureScreenshot", { format: "png", captureBeyondViewport: true,
      clip: { x: 0, y: 0, width, height: h, scale: 1 } })
    const file = join(out, `${width}-${theme}-${state}.png`)
    writeFileSync(file, Buffer.from(shot.data, "base64"))
    console.log(file)
  }
}
clearTimeout(deadline)
ws.close()
await new Promise((ok) => { proc.once("exit", ok); proc.kill(); setTimeout(ok, 3000) })
try { rmSync(profile, { recursive: true, force: true }) } catch { /* Chrome still letting go; the OS cleans tmp */ }
