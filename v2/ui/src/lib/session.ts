/**
 * A sign-in session that oauth2-proxy ended (#153). Envoy checks every /api
 * call with oauth2-proxy (ext_authz); once the session cookie expires or is
 * revoked, the call never reaches amp-api. It comes back as a 401, a redirect
 * to sign-in, or the sign-in page (a 200 in HTML) instead of JSON, which the pages used to
 * show as a wall of "not found" and load errors.
 *
 * The API client (api/client.ts), the remote-control transport and the live
 * event stream (hooks/useSSE.ts) run responses through sessionLost and call
 * markSessionEnded; SessionEndedGate then puts one "Your
 * session ended" prompt over the app (the pages stay mounted, their text kept),
 * and the client and transport stop sending. The UI adds no auth of its own: it only notices that
 * the proxy stopped letting requests through.
 *
 * A 403 with a JSON reason is amp-api refusing this account (#69: no AMP
 * account, or a missing role). That is not a lost session, and keeps its
 * sentence (lib/apiError.ts).
 */
import { useSyncExternalStore } from 'react'

let ended = false
// Sign-in is off on this AMP (Local Dev, #124): there is no session to lose
// and no /oauth2/start to send anyone to.
let signInOff = false
const listeners = new Set<() => void>()

function emit() { listeners.forEach(l => l()) }

/**
 * Whether res is the proxy turning a signed-out request away rather than an
 * answer from AMP: a 401, a redirect (opaque under redirect: 'manual', a 3xx,
 * or one fetch followed off /api), or a 2xx HTML page where JSON was expected
 * (the sign-in page). An HTML error page (4xx/5xx, such as nginx's "502 Bad
 * Gateway" while amp-api restarts) is an outage, not a lost session.
 */
export function sessionLost(res: Response): boolean {
  if (res.type === 'opaqueredirect' || res.status === 401) return true
  if (res.status >= 300 && res.status < 400) return true
  if (res.redirected && res.url) {
    try {
      const p = new URL(res.url, window.location.origin).pathname
      if (!p.startsWith('/api/') && !p.startsWith('/authadmin/')) return true
    } catch { /* not a URL we can read: judge by the body */ }
  }
  if (!res.ok) return false
  return (res.headers.get('content-type') ?? '').toLowerCase().includes('text/html')
}

/** Record that the session ended. Ignored when sign-in is off. */
export function markSessionEnded() {
  if (ended || signInOff) return
  ended = true
  emit()
}

/** AuthProvider: sign-in is off on this AMP (me.local_dev), so never prompt. */
export function setSignInOff(off: boolean) {
  signInOff = off
  if (off && ended) { ended = false; emit() }
}

export function isSessionEnded(): boolean {
  return ended
}

/** Call l once the session ends. Returns the unsubscribe. */
export function onSessionEnded(l: () => void): () => void {
  const wrapped = () => { if (ended) l() }
  listeners.add(wrapped)
  return () => { listeners.delete(wrapped) }
}

/**
 * Ask the proxy whether the session is still good (after the event stream
 * fails, which can't show its status). Marks it ended when it isn't; a
 * network failure says nothing either way.
 */
export async function checkSession(): Promise<boolean> {
  try {
    const res = await fetch('/api/me', { redirect: 'manual', headers: { Accept: 'application/json' } })
    if (sessionLost(res)) { markSessionEnded(); return false }
  } catch { /* offline or the stream's own blip: not a lost session */ }
  return true
}

/** Sign in again, then come back here (oauth2-proxy's start URL). */
export function signInAgainUrl(): string {
  const here = window.location.pathname + window.location.search
  return `/oauth2/start?rd=${encodeURIComponent(here)}`
}

function subscribe(l: () => void) {
  listeners.add(l)
  return () => { listeners.delete(l) }
}

/** Whether the session ended (re-renders when it does). */
export function useSessionEnded(): boolean {
  return useSyncExternalStore(subscribe, () => ended, () => ended)
}

/** For tests: a live session again, with sign-in on. */
export function resetSession() {
  ended = false
  signInOff = false
  emit()
}
