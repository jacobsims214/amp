/**
 * When the launch animation plays, and what its status lines are allowed to say.
 *
 * - It plays at most once per browser session (a sessionStorage flag), and only
 *   when the app is opened at "/". sessionStorage is per tab, so "session" means
 *   this tab: a refresh, or a pasted deep link (/project/3/board?focus=7), goes
 *   straight to the page. A brand-new tab opened at "/" plays it again.
 * - `?launch=full` or `?launch=short` forces it (for demos).
 * - Its status lines only report what the app actually checked. Today that's the
 *   /api/me call: whether amp-api answered, and who you are signed in as.
 *   Nothing else (plan check, agents, KB, telemetry) is checked at startup, so
 *   nothing else gets a line.
 */
import type { AuthState } from '../contexts/auth-context'

export const SEEN_KEY = 'amp.launch.seen'

type Where = Pick<Location, 'pathname' | 'search'>

/**
 * How long the launch runs, in ms (KB design/loading.md, tier 0). It is the
 * app's one long wait, and only on a cold start:
 *
 * - short: at least SHORT (a floor, so it reads as a beat rather than a
 *   flash), then it ends as soon as /api/me has answered; it waits for the
 *   answer up to SHORT_MAX in all.
 * - full: FULL, played only right after sign-in (or `?launch=full`); it waits
 *   for /api/me up to FULL_HOLD_CAP longer.
 * - reduced motion: the logo fades in over REDUCED.
 */
export const LAUNCH_MS = { full: 5000, short: 600, shortMax: 2000, fullHoldCap: 4000, reduced: 700 } as const

export type LaunchMode = 'full' | 'short' | 'reduced'

/** Sign-in pages a browser comes back from: the launch after them is the full one. */
const SIGN_IN_REFERRER = /\/(oauth2|dex|auth|login|sso|saml|oidc)\b/i

/** Which launch plays: reduced motion wins, then `?launch=`, then full only right after sign-in. */
export function launchMode(env: { reducedMotion: boolean; search: string; referrer: string }): LaunchMode {
  if (env.reducedMotion) return 'reduced'
  return forcedLaunch(env.search) ?? (SIGN_IN_REFERRER.test(env.referrer) ? 'full' : 'short')
}

export function forcedLaunch(search: string): 'full' | 'short' | null {
  const forced = new URLSearchParams(search).get('launch')
  return forced === 'full' || forced === 'short' ? forced : null
}

/** Whether this page load should play the launch. Read once, when the app mounts. */
export function shouldLaunch(where: Where = window.location): boolean {
  if (forcedLaunch(where.search)) return true
  if (where.pathname !== '/') return false
  try {
    return sessionStorage.getItem(SEEN_KEY) !== '1'
  } catch {
    return false // storage blocked: we can't tell a refresh from a cold start, so don't replay it
  }
}

/** Record that the launch played, so the rest of this session skips it. */
export function markLaunched() {
  try { sessionStorage.setItem(SEEN_KEY, '1') } catch { /* storage blocked: shouldLaunch already says no */ }
}

export type Tone = 'go' | 'nogo' | 'info' | 'pending'

export interface Station {
  label: string
  /** Shown after the dots. Empty while pending. */
  value: string
  tone: Tone
}

/** The status lines, from what /api/me told us. */
export function stations(auth: Pick<AuthState, 'me' | 'loading' | 'error'>): Station[] {
  if (auth.loading) return [{ label: 'AMP API', value: '', tone: 'pending' }]
  if (auth.error) return [{ label: 'AMP API', value: 'NO ANSWER', tone: 'nogo' }]
  // No error and no identity: amp-api answered "signed out" and the app is
  // redirecting to sign-in. That isn't a failure, so don't say NO ANSWER.
  if (!auth.me) return [{ label: 'AMP API', value: '', tone: 'pending' }]
  const who = (auth.me.name || auth.me.email || auth.me.subject || '').trim()
  const out: Station[] = [{ label: 'AMP API', value: 'GO', tone: 'go' }]
  if (who) out.push({ label: 'SIGNED IN AS', value: who.toUpperCase().slice(0, 28), tone: 'info' })
  return out
}

/** The closing line: "ALL STATIONS GO" only when every line is GO or plain information. */
export function summary(lines: Station[]): { text: string; tone: Tone } {
  if (lines.some(l => l.tone === 'nogo')) return { text: 'API NOT ANSWERING · OPENING ANYWAY', tone: 'nogo' }
  if (lines.some(l => l.tone === 'pending')) return { text: 'STANDING BY', tone: 'pending' }
  return { text: 'ALL STATIONS GO', tone: 'go' }
}
