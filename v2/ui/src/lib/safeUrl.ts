const SAFE_SCHEMES = /^(https?:|mailto:)/i

/** Keeps http(s) and mailto URLs; anything else (javascript:, data:, relative) becomes empty. */
export function safeUrl(url: string): string {
  const u = url.trim()
  return SAFE_SCHEMES.test(u) ? u : ''
}
