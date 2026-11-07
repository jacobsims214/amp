/**
 * What the UI says when an API call fails (#124). The server's own reason is
 * kept when it is a sentence ("only project leads … can read another person's
 * My work"); a bare status word ("forbidden", "Unauthorized", "403") never
 * reaches the page. A 401 or 403 without a reason becomes a sentence that
 * says what is missing and what to do.
 *
 * The API client (api/client.ts) and the remote-control transport run every
 * failed response through apiErrorMessage, and errMsg runs caught errors
 * through friendlyError, so every place that shows an API error gets this.
 */

/** Said for a 401: the session has no (valid) sign-in. */
export const NOT_SIGNED_IN =
  'You are not signed in to AMP, or your sign-in has expired. Sign in again, then retry.'

/** Said for a 403 the server gave no reason for. */
export const NO_PERMISSION =
  "Your account doesn't have permission for this. It needs an AMP admin, or a role on this org, team or project that you don't have: ask an AMP admin for access."

// A message that is only a status: a word or two, a code, or the client's
// own "GET /path: 403" fallback.
const BARE = /^(?:forbidden|unauthori[sz]ed|not authenticated|access denied|permission denied|denied|error|\d{3}(?:\s+\w+)?|request failed(?: \(\d{3}\))?|(?:GET|POST|PUT|PATCH|DELETE) \S+: \d{3})\.?$/i

/** Whether message is only a status word, not a reason a person can act on. */
export function isBareStatus(message?: string | null): boolean {
  const m = (message ?? '').trim()
  return m === '' || BARE.test(m)
}

/**
 * The message to show for a failed API response with this status and the
 * server's error text (the JSON body's "error", if any).
 */
export function apiErrorMessage(status: number, serverMessage?: string | null, fallback?: string): string {
  const m = (serverMessage ?? '').trim()
  if (status === 401 && isBareStatus(m)) return NOT_SIGNED_IN
  if (status === 403 && isBareStatus(m)) return NO_PERMISSION
  if (!isBareStatus(m)) return m
  return fallback ?? `The request failed (HTTP ${status}).`
}

/** message, unless it is a bare permission word, which becomes a sentence. */
export function friendlyError(message: string): string {
  const m = message.trim()
  if (/^(?:unauthori[sz]ed|not authenticated|401(?:\s+\w+)?)\.?$/i.test(m)) return NOT_SIGNED_IN
  if (/^(?:forbidden|access denied|permission denied|denied|403(?:\s+\w+)?)\.?$/i.test(m)) return NO_PERMISSION
  return message
}
