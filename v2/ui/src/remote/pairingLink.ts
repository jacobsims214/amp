/**
 * The pairing link's fragment (protocol 7.1):
 *   https://<amp>/sessions/<session_id>#p=<pairing_secret>&h=<harness_id>
 *
 * Two ways in, one validation:
 *
 *  - `capturePairingFragment` runs in main.tsx before React or the router start, so nothing else
 *    ever sees the fragment of a page load: it reads `p` and `h`, then immediately replaces the URL
 *    with the bare path (dropping the fragment and any query, which the page ignores, T-UI-8).
 *  - `acceptPairingLink` is the same capture from user action (#4902). An installed/home-screen app
 *    is never handed the pairing URL as its page load — no https link can be routed into a home-screen
 *    web app on iOS, and Android only routes verified in-scope links into a WebAPK this build doesn't
 *    ship (`discoveries/remote-pairing-pwa-divergence.md`, #4900) — so the unpaired screen lets the
 *    person paste or scan the link instead. It runs exactly the checks below and keeps the secret in
 *    exactly the same place.
 *
 * ⚠ THE SECRET STAYS IN THIS MODULE'S MEMORY, ON PURPOSE. Do not put it in localStorage,
 * sessionStorage, IndexedDB, a cookie, or anywhere else that outlives the page: at rest it would be
 * readable by any script injected into this origin, and it derives the pair MAC and the matching code
 * shown on both ends. #4900 weighed persisting it — it would have survived an SPA sign-in redirect —
 * and rejected that trade. A link that went stale is answered by the laptop with a clear message
 * ("This link has expired…"), and `/remote pair` on the laptop mints a new one. Nothing here is ever
 * logged, stored or sent; only values derived from the secret leave the page.
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

/** Why a fragment isn't a usable pairing link. */
type Refusal = 'no_secret' | 'bad_secret' | 'no_laptop_key'

/**
 * The one place a pairing secret is accepted: `p` must be a canonical 32-byte base64url secret and
 * `h` a harness fingerprint, both in the fragment. Both entries go through this.
 */
function readFragment(sessionId: string, hash: string): { link: PairingLink } | { refusal: Refusal } {
  const params = new URLSearchParams(hash.replace(/^#/, ''))
  const p = params.get('p')
  if (!p) return { refusal: 'no_secret' }
  let secret: Uint8Array
  try { secret = b64uDecode(p) } catch { return { refusal: 'bad_secret' } }
  if (secret.length !== PAIRING_SECRET_BYTES) return { refusal: 'bad_secret' }
  const h = params.get('h')
  if (!h || !FP.test(h)) return { refusal: 'no_laptop_key' }
  return { link: { session_id: sessionId, secret, h } }
}

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
  const read = readFragment(m[1], hash)
  if ('refusal' in read) return false
  forgetPairingLink()
  held = read.link
  return true
}

/** What a pasted or scanned link turned out to be. */
export type PairingLinkEntry =
  /** A usable link for this session: held now, exactly as a captured fragment would be. */
  | { kind: 'link'; link: PairingLink }
  /**
   * A pairing URL for another session on this same AMP: loading `href` captures it there as a page
   * load, which is how the laptop's link works in a browser (protocol 7.1).
   */
  | { kind: 'other_session'; href: string }
  /** Not usable here; `message` says why, in words the person can act on. */
  | { kind: 'refused'; message: string }

/** A URL anywhere in what someone pasted: the harness prints words around the link. */
const URL_IN_TEXT = /https?:\/\/[^\s"'<>]+/i
/** The same link without its scheme, e.g. copied out of a narrow terminal. */
const BARE_LINK = /(?:[a-z0-9-]+\.)+[a-z]{2,}\/sessions\/rs_[A-Za-z0-9_-]{22}[^\s"'<>]*/i

export const NOT_A_PAIRING_LINK = 'That doesn\'t look like a pairing link. Paste the whole link the laptop printed: https://…/sessions/rs_… ending in #p=…&h=…'
export const LINK_NEEDS_SCHEME = 'Paste the whole link, starting with https://.'
export const LINK_HAS_NO_SECRET = 'That link has no pairing secret. Copy the whole link, including everything after the #.'
export const LINK_SECRET_INVALID = 'The pairing secret in that link isn\'t valid. Copy the whole link again from the laptop.'
export const LINK_NO_LAPTOP_KEY = 'That link is missing the laptop key on the end (the h=… part). Copy the whole link again.'
export const LINK_OTHER_AMP = 'That link is for another AMP server. Open it there instead.'

const REFUSAL_TEXT: Record<Refusal, string> = {
  no_secret: LINK_HAS_NO_SECRET,
  bad_secret: LINK_SECRET_INVALID,
  no_laptop_key: LINK_NO_LAPTOP_KEY,
}

function urlFrom(text: string, here: string): URL | null {
  const trimmed = text.trim()
  if (!trimmed) return null
  const absolute = trimmed.match(URL_IN_TEXT)?.[0]
  const bare = trimmed.match(BARE_LINK)?.[0]
  const candidates = [absolute, bare && `https://${bare}`]
  for (const candidate of candidates) {
    if (!candidate) continue
    try { return new URL(candidate) } catch { /* try the next shape */ }
  }
  // No host at all: a path as the address bar shows it, resolved against this page.
  try { return new URL(trimmed, here) } catch { return null }
}

/**
 * Takes a pairing link from user action (pasted, or decoded from a QR code) and holds its secret in
 * memory, so the page pairs exactly as it does when the link was its own page load. Nothing is
 * stored (#4902; see the note at the top of this file).
 *
 * `here` is this page's origin: a link for another session on this origin is worth loading, but one
 * for another server is refused — following it would leave this AMP, and its secret would be
 * meaningless here.
 */
export function acceptPairingLink(text: string, sessionId: string, here: string = window.location.origin): PairingLinkEntry {
  if (!text.trim()) return { kind: 'refused', message: NOT_A_PAIRING_LINK }
  const url = urlFrom(text, here)
  if (!url) return { kind: 'refused', message: NOT_A_PAIRING_LINK }
  if (url.origin !== here) return { kind: 'refused', message: LINK_OTHER_AMP }
  const m = SESSION_PATH.exec(url.pathname)
  if (!m) return { kind: 'refused', message: NOT_A_PAIRING_LINK }
  if (m[1] !== sessionId) {
    if (!url.hash) return { kind: 'refused', message: LINK_HAS_NO_SECRET }
    return { kind: 'other_session', href: `${url.pathname}${url.hash}` }
  }
  const read = readFragment(sessionId, url.hash)
  if ('refusal' in read) return { kind: 'refused', message: REFUSAL_TEXT[read.refusal] }
  forgetPairingLink() // an earlier link's secret is zeroed before this one takes its place
  held = read.link
  return { kind: 'link', link: read.link }
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
