import type { DecisionDoc } from '../types'

/** An option's index, 'other' for Something else, or null before a choice. */
export type OptionChoice = number | 'other' | null

/**
 * The note a judgment ticket's Mark done sends (docs/specs/human-decision-docs.md
 * §5): "<option label> — <decision text>", or the text alone for Something
 * else. The label is the doc's current version's, as stored.
 */
export function decisionNote(doc: Pick<DecisionDoc, 'ask'>, choice: OptionChoice, text: string): string {
  const label = typeof choice === 'number' ? doc.ask?.options[choice]?.label : undefined
  return label ? `${label} — ${text.trim()}` : text.trim()
}
