/**
 * The launch: AMP's Go / No-Go poll, played over the app as it starts.
 *
 * The flight director polls four stations on a CRT; each GO lights one of
 * AMP's bands, and "ALL STATIONS GO" brings up the wordmark. The last
 * station waits for /api/me, so the launch ends when the app is ready
 * (never more than a few seconds later).
 *
 * - Full (3.6 s): arriving from the sign-in redirect, or the first visit in
 *   12 hours. Everything else that loads the app (a refresh, a new tab) gets
 *   the short version (1 s). Moving between pages never replays it.
 * - Any click or key skips it. With reduced motion, the logo just fades in.
 * - `?launch=full` or `?launch=short` forces a version (for demos).
 */
import { useEffect, useRef, useState } from 'react'
import { useAuth } from '../../hooks/useAuth'

const BANDS = ['#D9482B', '#EB8A2E', '#EBC34B', '#2C66B8']
const INK = '#E8ECF2', INK2 = '#9AA4B2', INK3 = '#5E6876', CRT = '#060A0C'
const STATIONS = ['PLAN CHECK', 'AGENTS', 'KNOWLEDGE BASE', 'TELEMETRY']

const DUR = { full: 3600, short: 1000, reduced: 700 } as const
const goAt = (i: number) => 800 + i * 420 + 280 // full version
const HOLD = goAt(3) // the last station waits for the app
const HOLD_CAP = 4000
const LAST_KEY = 'amp.launch.last'
const FULL_EVERY = 12 * 3600 * 1000

type Mode = keyof typeof DUR

function pickMode(): Mode {
  if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return 'reduced'
  const forced = new URLSearchParams(window.location.search).get('launch')
  if (forced === 'full' || forced === 'short') return forced
  let last = 0
  try { last = Number(localStorage.getItem(LAST_KEY)) || 0 } catch { /* storage blocked: treat as first visit */ }
  const fromSignIn = /\/(oauth2|dex|auth|login|sso|saml|oidc)\b/i.test(document.referrer)
  return fromSignIn || Date.now() - last > FULL_EVERY ? 'full' : 'short'
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

function draw(ctx: CanvasRenderingContext2D, w: number, h: number, t: number, mode: Mode, now: number) {
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
  mono(ctx, type('AMP FLIGHT DIRECTOR', 0, 18), left, y, px, INK, 600)
  y += lh
  mono(ctx, type('LAUNCH STATUS CHECK', 360, 18), left, y, px * 0.85, INK3)
  y += lh * 1.35
  const bands = [0, 0, 0, 0]
  STATIONS.forEach((st, i) => {
    const at = full ? 800 + i * 420 : 0
    const go = full ? goAt(i) : i * 110
    const label = (st + ' ').padEnd(17, '.')
    mono(ctx, type(label, at, 12), left, y, px * 0.9, INK2)
    const g = seg(t, go, go + 120)
    if (g > 0) mono(ctx, 'GO', left + ctx.measureText(label).width + px * 0.6, y, px * 0.9, rgba(BANDS[i], g), 600)
    bands[i] = eOut(seg(t, go, go + 260))
    y += lh
  })
  const all = seg(t, full ? 2550 : 500, (full ? 2550 : 500) + 150)
  if (all > 0) mono(ctx, 'ALL STATIONS GO', left, y + lh * 0.4, px * 0.9, rgba(INK, all), 600)
  if (Math.floor(now / 420) % 2 === 0) {
    ctx.fillStyle = rgba(INK, 0.8)
    ctx.fillRect(left, y + lh * 1.05, px * 0.55, px * 0.12)
  }
  logo(ctx, logoX, logoY, s, bands, full ? seg(t, 2700, 3200) * 3 : 3, full ? 1 : eOut(seg(t, 550, 950)))

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
  const { loading } = useAuth()
  const [mode] = useState(pickMode)
  const [phase, setPhase] = useState<'run' | 'out' | 'done'>('run')
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const ready = useRef(!loading)
  const skip = useRef(false)

  useEffect(() => { ready.current = !loading }, [loading])

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
    if (mode === 'full') try { localStorage.setItem(LAST_KEY, String(Date.now())) } catch { /* fine: next load plays full again */ }

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
      else if (mode === 'full' && t >= HOLD && !ready.current && held < HOLD_CAP) held += dt
      else t = Math.min(dur, t + dt)
      draw(ctx, w, h, t, mode, now)
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
