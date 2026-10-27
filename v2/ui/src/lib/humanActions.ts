/**
 * Helpers for HumanActions (components/human/HumanActions.tsx): which action a
 * ticket asks of a person, the kinds' labels and colours, and a done ticket's
 * merge status line (KB decisions/human-work-ux.md §2, §4, §5).
 */
import type { Task, WaitKind } from '../types'

export type ActionKind = Exclude<WaitKind, 'approve'>

/** What a ticket asks of a person now, from the ticket alone (the drawer has no WaitItem). */
export function actionKindOf(t: Pick<Task, 'state' | 'block_reason' | 'performer' | 'merge_state'>): ActionKind | null {
  if (t.state === 'blocked' && t.block_reason?.trim() && !t.block_reason.startsWith('scheduled:')) return 'answer'
  if (t.state === 'completed') return t.merge_state === 'needed' || t.merge_state === 'abandoned' ? 'merge' : null
  if (t.performer === 'human') return 'do'
  return null
}

export const KIND_LABEL: Record<WaitKind, string> = { approve: 'Approve', answer: 'Answer', merge: 'Merge', do: 'Do' }
export const KIND_COLOR: Record<WaitKind, string> = { approve: 'var(--violet)', answer: 'var(--st-flag)', merge: 'var(--teal)', do: 'var(--ink)' }

/**
 * A done ticket's merge, in one line: "Done · Merge needed", "Done · PR
 * closed without merging", or "Merged (recorded by github · Oct 27 14:20)".
 * Null when there's nothing to say.
 */
export function mergeStatusText(t: Pick<Task, 'state' | 'merge_state' | 'merge_source' | 'merged_at'>): string | null {
  if (t.state !== 'completed') return null
  switch (t.merge_state) {
    case 'needed': return 'Done · Merge needed'
    case 'abandoned': return 'Done · PR closed without merging'
    case 'merged': {
      const when = t.merged_at ? new Date(t.merged_at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : ''
      const by = t.merge_source === 'ui' ? 'a person in AMP' : t.merge_source === 'ci' ? 'CI' : t.merge_source === 'github' ? 'GitHub' : t.merge_source
      return `Merged${by || when ? ` (recorded by ${[by, when].filter(Boolean).join(' · ')})` : ''}`
    }
    default: return null
  }
}
