import { safeUrl } from './safeUrl'

/**
 * Where a link in rendered markdown goes:
 *  - internal: an AMP route, opened in-app by react-router (`to` is the
 *    path + query + hash). A root-relative path ('/project/3/kb', not
 *    '//host') or an absolute URL on this origin. `fullLoad` marks paths the
 *    SPA must not route client-side: /sessions/ (its strict CSP comes with the
 *    HTTP response, ui/nginx.conf), /api/ and the static /home pages.
 *  - external: an http(s) or mailto URL elsewhere, opened in a new tab.
 *  - blocked: anything else (javascript:, data:, bare relative paths, empty),
 *    shown as text only.
 */
export type LinkTarget =
  | { kind: 'internal'; to: string; fullLoad?: true }
  | { kind: 'external'; href: string }
  | { kind: 'blocked' }

/** Paths served by a full page load, never by client-side routing. */
const FULL_LOAD = /^\/(sessions\/|api\/|home(\/|$))/

function internal(u: URL): LinkTarget {
  const to = `${u.pathname}${u.search}${u.hash}`
  return FULL_LOAD.test(u.pathname) ? { kind: 'internal', to, fullLoad: true } : { kind: 'internal', to }
}

export function classifyHref(href: string | null | undefined, origin: string = window.location.origin): LinkTarget {
  const h = (href ?? '').trim()
  if (!h) return { kind: 'blocked' }
  if (h.startsWith('/')) {
    // '//host' and '/\host' are protocol-relative: another origin.
    if (h.startsWith('//') || h.startsWith('/\\')) return { kind: 'blocked' }
    let u: URL
    try {
      u = new URL(h, origin)
    } catch {
      return { kind: 'blocked' }
    }
    if (u.origin !== new URL(origin).origin) return { kind: 'blocked' }
    // Keep the path as written unless it needs a full load (checked on the
    // normalised path, so '/x/../sessions/1' is caught too).
    return FULL_LOAD.test(u.pathname) ? internal(u) : { kind: 'internal', to: h }
  }
  const safe = safeUrl(h)
  if (!safe) return { kind: 'blocked' }
  let u: URL
  try {
    u = new URL(safe)
  } catch {
    return { kind: 'blocked' }
  }
  if ((u.protocol === 'http:' || u.protocol === 'https:') && u.origin === origin) return internal(u)
  return { kind: 'external', href: safe }
}
