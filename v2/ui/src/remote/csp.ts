/**
 * Content-Security-Policies for the AMP web app (threat model T-UI-4, R-M10; #2381 M6, #2393).
 *
 * Two policies, served by the UI's nginx (v2/ui/nginx.conf) and by `vite preview`
 * (vite.config.ts); csp.test.ts checks nginx carries these exact values.
 *
 *  - SESSION_CSP on /sessions/*: the remote session page, which signs prompts with
 *    the device key. No inline script, no eval, same-origin fetches only, no images
 *    from other origins, no framing, and Trusted Types so no string can become HTML
 *    or script.
 *  - BASELINE_CSP on every other page. The device key lives in this origin's
 *    IndexedDB, so script injected anywhere on the site could use it; the baseline
 *    keeps injected script from running at all: script-src 'self' (no inline script,
 *    no eval, no third-party script), no plugins, no <base> rewriting, no framing.
 *    It is looser than SESSION_CSP only where the rest of the app needs it:
 *    `default-src 'self'` so artifact previews (same-origin iframes and images from
 *    /api/artifacts/…) still load, and no Trusted Types.
 *
 * The only third-party origins are the Google Fonts stylesheet and font files that
 * index.html loads. Neither policy allows 'unsafe-inline' or 'unsafe-eval': React
 * sets `style` through the CSSOM, which style-src doesn't govern.
 */
export const SESSION_CSP = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self' https://fonts.googleapis.com",
  'font-src https://fonts.gstatic.com',
  "img-src 'self' data:",
  "connect-src 'self'",
  "manifest-src 'self'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
  "object-src 'none'",
  "require-trusted-types-for 'script'",
  "trusted-types 'none'",
].join('; ')

export const BASELINE_CSP = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self' https://fonts.googleapis.com",
  "font-src 'self' https://fonts.gstatic.com",
  "img-src 'self' data:",
  "connect-src 'self'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
  "object-src 'none'",
].join('; ')

/** Paths that get SESSION_CSP. */
export const SESSION_PATH_PREFIX = '/sessions/'

/** The policy a page at `path` gets. */
export function cspFor(path: string): string {
  return path.startsWith(SESSION_PATH_PREFIX) ? SESSION_CSP : BASELINE_CSP
}
