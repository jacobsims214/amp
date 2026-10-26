/**
 * The device name in a pair request (protocol 7.3): "<browser> on <platform>",
 * 1..64 characters, from the user agent. The laptop shows it in its y/N prompt
 * (after stripping control characters), so it only has to be recognisable.
 */
export function deviceNameFromUA(ua: string): string {
  const browser =
    /Edg(A|iOS)?\//.test(ua) ? 'Edge'
    : /OPR\/|Opera/.test(ua) ? 'Opera'
    : /Firefox\/|FxiOS\//.test(ua) ? 'Firefox'
    : /SamsungBrowser\//.test(ua) ? 'Samsung Internet'
    : /Chrome\/|CriOS\/|Chromium\//.test(ua) ? 'Chrome'
    : /Safari\//.test(ua) ? 'Safari'
    : 'Browser'
  const platform =
    /iPhone/.test(ua) ? 'iPhone'
    : /iPad/.test(ua) ? 'iPad'
    : /Android/.test(ua) ? 'Android'
    : /CrOS/.test(ua) ? 'ChromeOS'
    : /Mac OS X|Macintosh/.test(ua) ? 'macOS'
    : /Windows/.test(ua) ? 'Windows'
    : /Linux/.test(ua) ? 'Linux'
    : 'this device'
  return `${browser} on ${platform}`.slice(0, 64)
}
