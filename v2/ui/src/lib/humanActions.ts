/**
 * Helpers for HumanActions (components/human/HumanActions.tsx): which action a
 * ticket asks of a person, the kinds' labels and colours, and a done ticket's
 * merge status line (KB decisions/human-work-ux.md §2, §4, §5), and the one
 * line that says what a My work item needs from the person (needsLine).
 */
import type { Task, WaitItem, WaitKind } from '../types'

export type ActionKind = Exclude<WaitKind, 'approve'>

/** What a ticket asks of a person now, from the ticket alone (the drawer has no WaitItem). */
export function actionKindOf(t: Pick<Task, 'state' | 'block_reason' | 'performer' | 'merge_state'>): ActionKind | null {
  if (t.state === 'blocked' && t.block_reason?.trim() && !t.block_reason.startsWith('scheduled:')) return 'answer'
  if (t.state === 'completed') return t.merge_state === 'needed' || t.merge_state === 'abandoned' ? 'merge' : null
  if (t.performer === 'human') return 'do'
  return null
}

export const KIND_LABEL: Record<WaitKind, string> = { approve: 'Approve', answer: 'Answer', merge: 'Merge', start: 'Start', do: 'Do' }
export const KIND_COLOR: Record<WaitKind, string> = {
  approve: 'var(--violet)', answer: 'var(--st-flag)', merge: 'var(--teal)', start: 'var(--warning)', do: 'var(--ink)',
}

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

/** The verb a human ticket's reason asks for, for the "needs from you" line. */
export const REASON_VERB: Record<string, string> = {
  judgment: 'Decide', approval: 'Sign off', access: 'Do with your access', review: 'Review',
  external: 'Get from someone outside', physical: 'Do offline', other: 'Do',
}
export const reasonVerb = (r?: string) => (r && REASON_VERB[r]) || 'Do'

const NEEDS_MAX = 140

/**
 * The first sentence of a brief or question: its first non-empty line, up to
 * the first ".", "?" or "!" followed by a space, without a leading Markdown
 * heading, bullet or quote, capped at NEEDS_MAX characters. Empty when there
 * is no text.
 */
export function firstSentence(text?: string | null): string {
  const line = (text ?? '').split('\n')
    .map(l => l.replace(/^\s*(?:#{1,6}\s+|[-*+]\s+|\d+[.)]\s+|>\s*)/, '').trim())
    .find(Boolean) ?? ''
  const s = (line.match(/^.*?[.!?](?=\s|$)/)?.[0] ?? line).trim()
  return s.length > NEEDS_MAX ? `${s.slice(0, NEEDS_MAX - 1).trimEnd()}…` : s
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

/**
 * What a My work item needs from the person, in one line: the ask, not the
 * state. Shared by the project's My work cards and the summary rows.
 *
 * - approve: "Review and approve 5 tickets"
 * - answer: "Answer: <the question's first sentence>"
 * - merge: "Merge PR #88, 2 tickets wait"; a closed PR asks to reopen or release it
 * - start: "Launch its run, or return #2803 to ready"
 * - do: the reason's verb and the brief's first sentence ("Decide: Pick the
 *   Q1 targets."), "<verb>: see the acceptance criteria in the brief" when
 *   only the criteria are written, or "No instructions: open the ticket" when
 *   the ticket has neither. Summary rows carry no ticket (the server sends it only
 *   to the project tab), so there it points at the ticket for the brief.
 */
export function needsLine(it: WaitItem & { task?: Pick<Task, 'description' | 'acceptance_criteria' | 'block_reason' | 'human_reason'> }): string {
  switch (it.kind) {
    case 'approve':
      return `Review and approve ${plural(it.count ?? it.task_ids.length, 'ticket')}`
    case 'answer': {
      const q = firstSentence(it.question || it.task?.block_reason)
      return q ? `Answer: ${q}` : 'Answer: open the ticket for the question'
    }
    case 'merge': {
      const n = it.pr_url?.match(/\/pull\/(\d+)/)?.[1]
      if (it.merge_state === 'abandoned') {
        return `${n ? `PR #${n}` : 'The PR'} closed without merging: reopen the ticket or mark it not needed`
      }
      const what = n ? `PR #${n}` : it.pr_url ? 'the PR' : it.branch ? `branch ${it.branch}` : 'its work'
      const waiting = it.holding_up?.length ?? 0
      return `Merge ${what}${waiting ? `, ${plural(waiting, 'ticket')} wait${waiting === 1 ? 's' : ''}` : ''}`
    }
    case 'start':
      return `Launch its run, or return #${it.task_id} to ready`
    case 'do': {
      const verb = reasonVerb(it.human_reason || it.task?.human_reason)
      if (!it.task) return `${verb}: open the ticket for the brief`
      const brief = firstSentence(it.task.description)
      if (brief) return `${verb}: ${brief}`
      // No description but acceptance criteria: the brief has something to read.
      return it.task.acceptance_criteria?.trim() ? `${verb}: see the acceptance criteria in the brief` : 'No instructions: open the ticket'
    }
    default:
      return ''
  }
}
