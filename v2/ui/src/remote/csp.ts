/**
 * The Content-Security-Policy for /sessions/* (threat model T-UI-4, R-M10):
 * no inline script, no eval, same-origin fetches only, no images from other
 * origins, no framing, and Trusted Types so no string can become HTML or
 * script. The only third-party origins are the Google Fonts stylesheet and
 * font files that index.html already loads.
 *
 * Served by the UI's nginx (v2/ui/Dockerfile) and by `vite preview`
 * (vite.config.ts). csp.test.ts checks the Dockerfile carries this exact value.
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

/** Paths that get SESSION_CSP. */
export const SESSION_PATH_PREFIX = '/sessions/'
