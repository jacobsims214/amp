import { safeUrl } from './safeUrl'

/**
 * Where a link in rendered markdown goes:
 *  - internal: an AMP route, opened in-app by react-router (`to` is the
 *    path + query + hash). A root-relative path ('/project/3/kb', not
 *    '//host') or an absolute URL on this origin.
 *  - external: an http(s) or mailto URL elsewhere, opened in a new tab.
 *  - blocked: anything else (javascript:, data:, bare relative paths, empty),
 *    shown as text only.
 */
export type LinkTarget =
  | { kind: 'internal'; to: string }
  | { kind: 'external'; href: string }
  | { kind: 'blocked' }

export function classifyHref(href: string | null | undefined, origin: string = window.location.origin): LinkTarget {
  const h = (href ?? '').trim()
  if (!h) return { kind: 'blocked' }
  if (h.startsWith('/')) {
    // '//host' and '/\host' are protocol-relative: another origin.
    return h.startsWith('//') || h.startsWith('/\\') ? { kind: 'blocked' } : { kind: 'internal', to: h }
  }
  const safe = safeUrl(h)
  if (!safe) return { kind: 'blocked' }
  let u: URL
  try {
    u = new URL(safe)
  } catch {
    return { kind: 'blocked' }
  }
  if ((u.protocol === 'http:' || u.protocol === 'https:') && u.origin === origin) {
    return { kind: 'internal', to: `${u.pathname}${u.search}${u.hash}` }
  }
  return { kind: 'external', href: safe }
}
