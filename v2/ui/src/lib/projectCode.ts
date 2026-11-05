/**
 * Project codes in the New project dialog (#128): A-Z and 0-9 only, at most
 * 12 characters, and suggested from the name until you type one yourself.
 * The server accepts any code, so existing codes are left as they are.
 */
export const CODE_MAX = 12

/** What the code field keeps of what was typed or pasted. */
export function cleanCode(raw: string): string {
  return raw.toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, CODE_MAX)
}

/**
 * A code from a project name: the initials of a name of several words
 * ("Pelican One" -> "PO", "Mobile app v2" -> "MAV2"), or the start of a
 * one-word name ("Pelican" -> "PELI"). Digits are kept whole.
 */
export function suggestCode(name: string): string {
  const words = name.toUpperCase().replace(/[^A-Z0-9]+/g, ' ').trim().split(' ').filter(Boolean)
  if (words.length === 0) return ''
  if (words.length === 1) return cleanCode(words[0].slice(0, 4))
  return cleanCode(words.map(w => (/^\d+$/.test(w) ? w : /^[A-Z]\d+$/.test(w) ? w : w[0])).join(''))
}
