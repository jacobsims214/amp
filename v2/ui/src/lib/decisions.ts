import { ago } from './approval'
import type { Task } from '../types'

/** An option's index, 'other' for Something else, or null before a choice. */
export type OptionChoice = number | 'other' | null

/**
 * How long Record decision's claim holds (docs/specs/human-decision-docs.md
 * §3): after it, a record that didn't finish can be retried, and the retry
 * finishes with the stored values. The server's DecisionClaimTTL.
 */
export const CLAIM_STALE_MS = 60_000

/**
 * "blocks N · waiting Xd", or "Nothing waits on it · waiting Xd": how many
 * open tickets wait on a Do item (transitively) and for how long it has
 * waited (docs/specs/human-decision-docs.md §4, D9). My work's Do cards and
 * the decision card's header both say it.
 */
export function waitLine(blocks: number, since: string | null | undefined, now = Date.now()): string {
  const age = ago(since, now).replace(/ ago$/, '')
  const head = blocks > 0 ? `blocks ${blocks}` : 'Nothing waits on it'
  return age ? `${head} · waiting ${age}` : head
}

/**
 * The open tickets downstream of a ticket, transitively, from each ticket's
 * dependency_ids: the server's holding_up (Repo.OpenDependents), worked out
 * from the plan the drawer already has.
 */
export function openDependents(id: number, tasks: Pick<Task, 'id' | 'state' | 'dependency_ids'>[]): number[] {
  const out: number[] = []
  const seen = new Set([id])
  const queue = [id]
  while (queue.length) {
    const cur = queue.shift()!
    for (const t of tasks) {
      if (seen.has(t.id) || t.state === 'completed' || !(t.dependency_ids ?? []).includes(cur)) continue
      seen.add(t.id)
      out.push(t.id)
      queue.push(t.id)
    }
  }
  return out
}

/** When a person's ticket started waiting on them: as My work's Do item's since. */
export function waitingSince(t: Pick<Task, 'state' | 'dispatched_at' | 'ready_at' | 'created_at'>): string {
  if (t.state === 'in_progress') return t.dispatched_at ?? t.ready_at ?? t.created_at
  return t.ready_at ?? t.created_at
}

/**
 * What an upstream decision says after its option: the server's decision is
 * "<option label> — <decided text>", or the label alone when the text only
 * repeated it (Accept recommendation). The strip shows the label in bold, so
 * this is the rest, without the label again.
 */
export function decisionAfterOption(option: string, decision: string): string {
  if (decision.trim() === option.trim()) return ''
  const prefix = `${option} — `
  return decision.startsWith(prefix) ? decision.slice(prefix.length) : decision
}
