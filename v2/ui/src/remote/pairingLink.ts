/**
 * The pairing link's fragment (protocol 7.1):
 *   https://<amp>/sessions/<session_id>#p=<pairing_secret>&h=<harness_id>
 *
 * `capturePairingFragment` runs in main.tsx before React or the router start, so
 * nothing else ever sees the fragment: it reads `p` and `h`, then immediately
 * replaces the URL with the bare path (dropping the fragment and any query, which
 * the page ignores, T-UI-8). The secret stays in this module's memory only. It is
 * never logged, stored or sent; only values derived from it leave the page.
 */
import { b64uDecode, PAIRING_SECRET_BYTES } from './protocol'

export interface PairingLink {
  session_id: string
  /** The 32-byte pairing secret. */
  secret: Uint8Array
  /** The harness fingerprint from the link: the only key the browser may pin (T-R-1). */
  h: string
}

const SESSION_PATH = /^\/sessions\/(rs_[A-Za-z0-9_-]{22})\/?$/
const FP = /^[A-Za-z0-9_-]{43}$/

let held: PairingLink | null = null

/**
 * Reads and scrubs the fragment of a /sessions/:id URL. Returns true when a
 * usable pairing link was captured. Any fragment on a session URL is removed,
 * usable or not.
 */
export function capturePairingFragment(loc: Pick<Location, 'pathname' | 'hash' | 'search'> = window.location, hist: Pick<History, 'replaceState'> = window.history): boolean {
  const m = SESSION_PATH.exec(loc.pathname)
  if (!m || (!loc.hash && !loc.search)) return false
  const hash = loc.hash
  hist.replaceState(null, '', loc.pathname)
  const params = new URLSearchParams(hash.replace(/^#/, ''))
  const p = params.get('p')
  const h = params.get('h')
  if (!p || !h || !FP.test(h)) return false
  let secret: Uint8Array
  try { secret = b64uDecode(p) } catch { return false }
  if (secret.length !== PAIRING_SECRET_BYTES) return false
  held = { session_id: m[1], secret, h }
  return true
}

/** The captured link for this session, if there is one. */
export function pairingLinkFor(sessionId: string): PairingLink | null {
  return held && held.session_id === sessionId ? held : null
}

/** Forgets the secret (after a pairing ends, or when it isn't needed). */
export function forgetPairingLink(): void {
  if (held) held.secret.fill(0)
  held = null
}

/** Tests only. */
export function setPairingLinkForTest(link: PairingLink | null): void {
  held = link
}
