/** An option's index, 'other' for Something else, or null before a choice. */
export type OptionChoice = number | 'other' | null

/**
 * How long Record decision's claim holds (docs/specs/human-decision-docs.md
 * §3): after it, a record that didn't finish can be retried, and the retry
 * finishes with the stored values. The server's DecisionClaimTTL.
 */
export const CLAIM_STALE_MS = 60_000
