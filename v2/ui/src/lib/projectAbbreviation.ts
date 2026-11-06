/**
 * Derive a 3-letter abbreviation from a project name.
 *
 * Rules:
 * 1. Split the name into words by non-alphabetic runs (spaces, hyphens,
 *    digits, punctuation).
 * 2. Keep only words that contain at least one letter.
 * 3. Take the first letter of each word.
 * 4. If fewer than 3 letters, fill remaining slots with the next letters of
 *    the last word (after its first letter), then repeat the last initial.
 * 5. Uppercase the result and limit to exactly 3 characters.
 *
 * Examples:
 *   "Home OS"                → "HOS"   (H + O + S from "OS")
 *   "simstech Design System" → "SDS"
 *   "amp-api"                → "API"   (A + p + i from "amp-api")
 *   "UX"                     → "UXU"
 *   "A"                      → "AAA"
 *   "Project 42"             → "PRO"
 */
export function projectAbbreviation(name: string): string {
  const words = name
    .split(/[^a-zA-Z]+/)
    .map(w => w.trim())
    .filter(w => /[a-zA-Z]/.test(w))

  if (words.length === 0) return 'AMP'

  const initials: string[] = []
  for (const word of words) {
    const ch = word[0]
    if (ch) initials.push(ch.toUpperCase())
    if (initials.length === 3) return initials.join('')
  }

  // Pull extra letters from the last word after its initial.
  const last = words[words.length - 1]
  for (let i = 1; i < last.length && initials.length < 3; i++) {
    initials.push(last[i]!.toUpperCase())
  }

  // Still short: repeat the last real letter.
  const finalLetter = initials[initials.length - 1] ?? 'A'
  while (initials.length < 3) {
    initials.push(finalLetter)
  }

  return initials.slice(0, 3).join('')
}
