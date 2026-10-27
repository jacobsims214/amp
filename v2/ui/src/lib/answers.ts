/**
 * Suggested answers for an agent's question (the Answer card's chips,
 * decisions/human-work-ux.md §2). Agents usually ask either-or questions:
 * "Should I fall back to a 0600 file, or fail with a clear error?". The
 * options are read from the last question in the text. No guess is better
 * than a wrong one: anything that doesn't split cleanly gives no chips.
 */
const LEAD = /^(?:so\s+)?(?:should|shall|do|does|can|could|would|will|is|are)\s+(?:i|we|you|it|this|that)\s+(?:rather\s+)?/i
const MAX_OPTION = 60

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

export function suggestAnswers(question: string | undefined | null): string[] {
  if (!question) return []
  const qs = question.split(/(?<=[.!?])\s+/).filter(s => s.trim().endsWith('?'))
  const last = qs[qs.length - 1]?.trim()
  if (!last) return []
  let body = last.replace(/\?+$/, '').replace(LEAD, '').trim()
  // "A or B?" / "A, B, or C?" / "A, or B?"
  if (!/\bor\b/i.test(body)) return []
  body = body.replace(/^either\s+/i, '')
  const parts = body.split(/,?\s+or\s+|,\s+/i).map(p => p.trim()).filter(Boolean)
  if (parts.length < 2 || parts.length > 4) return []
  if (parts.some(p => p.length > MAX_OPTION || p.split(/\s+/).length > 10)) return []
  return parts.map(cap)
}
