/**
 * The launch: AMP's Go / No-Go poll, played over the app as it starts.
 *
 * The flight director runs a status check on a CRT while AMP's bands light
 * up, then the wordmark comes in. The status lines are real: they come from
 * the /api/me call (lib/launch.ts), and a check that wasn't run gets no line.
 * The API line waits for /api/me, so the launch ends when the app is ready
 * (never more than a few seconds later).
 *
 * - It plays at most once per browser session, and only when the app is
 *   opened at "/" (see shouldLaunch in lib/launch.ts). Refreshes, deep links
 *   and moving between pages never replay it.
 * - Full (5 s): only right after the sign-in redirect. Otherwise the short
 *   version: at least 600 ms, ending as soon as /api/me answers (2 s at
 *   most). Timings: LAUNCH_MS in lib/launch.ts, decided in KB
 *   design/loading.md (tier 0).
 * - Any click or key skips it. With reduced motion, the logo just fades in.
 * - `?launch=full` or `?launch=short` forces a version (for demos).
 */
import { useEffect, useRef, useState } from 'react'
import { useAuth } from '../../hooks/useAuth'
import { LAUNCH_MS, launchMode, markLaunched, stations, summary, type LaunchMode, type Station, type Tone } from '../../lib/launch'

const BANDS = ['#D9482B', '#EB8A2E', '#EBC34B', '#2C66B8']
const INK = '#E8ECF2', INK2 = '#9AA4B2', INK3 = '#5E6876', CRT = '#060A0C'
const TONE: Record<Tone, string> = { go: BANDS[3], nogo: BANDS[0], info: INK, pending: INK3 }

const DUR: Record<LaunchMode, number> = { full: LAUNCH_MS.full, short: LAUNCH_MS.short, reduced: LAUNCH_MS.reduced }
// Full version: each line is typed, then answers a beat later (once its check is in).
const stationAt = (i: number) => 1800 + i * 800
const goAt = (i: number) => stationAt(i) + 600
const bandAt = (i: number) => goAt(0) + i * 350 // the four bands light in turn
const ALL_GO = goAt(1) + 500 // the summary line
const WORDMARK = ALL_GO + 200 // wordmark letters, over 600 ms
// Short version: the bands light and the summary lands inside the 600 ms floor.
const SHORT_GO = (i: number) => 60 + i * 80
const SHORT_ALL = SHORT_GO(3) + 80
// Where each version waits for /api/me (its API line), and for how long at most.
const HOLD: Record<LaunchMode, number> = { full: goAt(0), short: SHORT_GO(0), reduced: Infinity }
const HOLD_CAP: Record<LaunchMode, number> = { full: LAUNCH_MS.fullHoldCap, short: LAUNCH_MS.shortMax - LAUNCH_MS.short, reduced: 0 }

type Mode = LaunchMode

function pickMode(): Mode {
  return launchMode({
    reducedMotion: !!window.matchMedia?.('(prefers-reduced-motion: reduce)').matches,
    search: window.location.search,
    referrer: document.referrer,
  })
}

const clamp = (x: number, a = 0, b = 1) => Math.min(b, Math.max(a, x))
const seg = (t: number, a: number, b: number) => clamp((t - a) / (b - a))
const eOut = (x: number) => 1 - Math.pow(1 - x, 3)
const rgba = (hex: string, a: number) => {
  const n = parseInt(hex.slice(1), 16)
  return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${a})`
}

function mono(ctx: CanvasRenderingContext2D, txt: string, x: number, y: number, px: number, color: string, weight = 500) {
  ctx.font = `${weight} ${px}px "JetBrains Mono", ui-monospace, monospace`
  ctx.fillStyle = color
  ctx.textAlign = 'left'
  ctx.textBaseline = 'alphabetic'
  ctx.fillText(txt, x, y)
}

/** The mark and wordmark in the sidebar's proportions, centred on (cx, cy). s is one bar's height. */
function logo(ctx: CanvasRenderingContext2D, cx: number, cy: number, s: number, bands: number[], letters: number, alpha: number) {
  const barW = s * 6, gap = s * 0.667, markH = 4 * s + 3 * gap, font = s * 5.67, sp = font * 0.16
  ctx.font = `700 ${font}px Barlow, sans-serif`
  let tw = -sp
  for (const ch of 'AMP') tw += ctx.measureText(ch).width + sp
  const x0 = cx - (barW + s * 3.4 + tw) / 2, y0 = cy - markH / 2
  bands.forEach((p, i) => {
    if (p <= 0) return
    ctx.shadowColor = BANDS[i]
    ctx.shadowBlur = s * 1.8 * (1 - p * 0.5)
    ctx.fillStyle = rgba(BANDS[i], alpha)
    ctx.fillRect(x0, y0 + i * (s + gap), barW * p, s)
  })
  ctx.shadowBlur = 0
  ctx.textBaseline = 'middle'
  let x = x0 + barW + s * 3.4
  ;[...'AMP'].forEach((ch, i) => {
    const a = clamp(letters - i) * alpha
    if (a > 0) {
      ctx.fillStyle = rgba(INK, a)
      ctx.fillText(ch, x, cy + font * 0.04)
    }
    x += ctx.measureText(ch).width + sp
  })
}

function draw(ctx: CanvasRenderingContext2D, w: number, h: number, t: number, mode: Mode, now: number, lines: Station[]) {
  ctx.fillStyle = CRT
  ctx.fillRect(0, 0, w, h)
  const narrow = w < 720
  const s = clamp(h * 0.034, 9, 26)
  const W = Math.min(w * 0.86, 1080)
  const left = narrow ? 24 : (w - W) / 2
  const logoX = narrow ? w / 2 : left + W * 0.74
  const logoY = narrow ? h * 0.78 : h * 0.5

  if (mode === 'reduced') {
    logo(ctx, w / 2, h / 2, s, [1, 1, 1, 1], 3, eOut(seg(t, 0, 200)))
    return
  }
  const full = mode === 'full'
  const px = clamp(h * 0.032, 12, 22), lh = px * 1.75
  const type = (txt: string, at: number, speed: number) => (full ? txt.slice(0, Math.max(0, Math.floor((t - at) / speed))) : txt)

  let y = narrow ? h * 0.16 : h * 0.5 - lh * 3.6
  mono(ctx, type('AMP FLIGHT DIRECTOR', 0, 40), left, y, px, INK, 600)
  y += lh
  mono(ctx, type('LAUNCH STATUS CHECK', 900, 35), left, y, px * 0.85, INK3)
  y += lh * 1.35
  const bands = [0, 1, 2, 3].map(i => {
    const at = full ? bandAt(i) : SHORT_GO(i)
    return eOut(seg(t, at, at + 260))
  })
  lines.forEach((st, i) => {
    const at = full ? stationAt(i) : 0
    const go = full ? goAt(i) : SHORT_GO(i)
    const label = (st.label + ' ').padEnd(17, '.')
    mono(ctx, type(label, at, 22), left, y, px * 0.9, INK2)
    const g = seg(t, go, go + 120)
    // A check still in flight shows a blinking ellipsis, never a GO it hasn't earned.
    const value = st.tone === 'pending' ? (Math.floor(now / 300) % 2 ? '...' : '') : st.value
    if (g > 0 && value) mono(ctx, value, left + ctx.measureText(label).width + px * 0.6, y, px * 0.9, rgba(TONE[st.tone], g), 600)
    y += lh
  })
  const allAt = full ? ALL_GO : SHORT_ALL
  const all = seg(t, allAt, allAt + 150)
  const end = summary(lines)
  if (all > 0) mono(ctx, end.text, left, y + lh * 0.4, px * 0.9, rgba(end.tone === 'nogo' ? TONE.nogo : INK, all), 600)
  if (Math.floor(now / 420) % 2 === 0) {
    ctx.fillStyle = rgba(INK, 0.8)
    ctx.fillRect(left, y + lh * 1.05, px * 0.55, px * 0.12)
  }
  logo(ctx, logoX, logoY, s, bands, full ? seg(t, WORDMARK, WORDMARK + 600) * 3 : 3, full ? 1 : eOut(seg(t, SHORT_ALL, SHORT_ALL + 150)))

  // CRT scanlines and vignette
  ctx.fillStyle = 'rgba(0,0,0,0.22)'
  for (let yy = 0; yy < h; yy += 3) ctx.fillRect(0, yy, w, 1)
  const vg = ctx.createRadialGradient(w / 2, h / 2, h * 0.3, w / 2, h / 2, Math.max(w, h) * 0.75)
  vg.addColorStop(0, 'rgba(0,0,0,0)')
  vg.addColorStop(1, 'rgba(0,0,0,0.55)')
  ctx.fillStyle = vg
  ctx.fillRect(0, 0, w, h)
}

export function LaunchSequence() {
  const { me, loading, error } = useAuth()
  const [mode] = useState(pickMode)
  const [phase, setPhase] = useState<'run' | 'out' | 'done'>('run')
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const ready = useRef(!loading)
  const skip = useRef(false)
  const lines = useRef(stations({ me, loading, error }))

  useEffect(() => {
    ready.current = !loading
    lines.current = stations({ me, loading, error })
  }, [me, loading, error])

  // Once per session: a refresh or a new page load later in this tab skips it.
  // Only marked once /api/me has answered (signed in, or a real failure): a
  // signed-out visit at "/" is about to be sent to sign-in, and the launch
  // should still play when sign-in brings this tab back to "/".
  useEffect(() => { if (me || error) markLaunched() }, [me, error])

  // While the launch is up, the app's own animations wait (see .amp-launching).
  useEffect(() => {
    const root = document.documentElement
    if (phase === 'done') root.classList.remove('amp-launching')
    else root.classList.add('amp-launching')
    return () => root.classList.remove('amp-launching')
  }, [phase])

  useEffect(() => {
    const canvas = canvasRef.current
    const ctx = canvas?.getContext('2d')
    if (!canvas || !ctx) return
    let w = 0, h = 0
    const size = () => {
      const dpr = Math.min(2, window.devicePixelRatio || 1)
      w = window.innerWidth
      h = window.innerHeight
      canvas.width = Math.round(w * dpr)
      canvas.height = Math.round(h * dpr)
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    }
    size()
    window.addEventListener('resize', size)
    const onKey = () => { skip.current = true }
    window.addEventListener('keydown', onKey)

    const dur = DUR[mode]
    let t = 0, held = 0, prev = performance.now(), raf = 0, out: number | undefined
    const loop = (now: number) => {
      const dt = now - prev
      prev = now
      if (skip.current) t = dur
      else if (t >= HOLD[mode] && !ready.current && held < HOLD_CAP[mode]) held += dt
      else t = Math.min(dur, t + dt)
      draw(ctx, w, h, t, mode, now, lines.current)
      if (t >= dur) {
        setPhase('out')
        out = window.setTimeout(() => setPhase('done'), 300)
        return
      }
      raf = requestAnimationFrame(loop)
    }
    // Start once the fonts are in, so the first frames aren't in fallbacks (but don't wait long).
    let started = false
    const start = () => { if (!started) { started = true; prev = performance.now(); raf = requestAnimationFrame(loop) } }
    document.fonts?.ready.then(start)
    const fallback = window.setTimeout(start, 250)
    return () => {
      cancelAnimationFrame(raf)
      window.clearTimeout(fallback)
      window.clearTimeout(out)
      window.removeEventListener('resize', size)
      window.removeEventListener('keydown', onKey)
    }
  }, [mode])

  if (phase === 'done') return null
  return (
    <div className={`amp-launch${phase === 'out' ? ' amp-launch-out' : ''}`} aria-hidden="true"
      onPointerDown={() => { skip.current = true }}>
      <canvas ref={canvasRef} />
      {mode === 'full' && <span className="amp-launch-skip">click or press any key to skip</span>}
    </div>
  )
}
