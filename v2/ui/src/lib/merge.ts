import type { Task } from '../types'

/**
 * The merge signal on a done ticket (#2475, decisions/human-work-ux.md §4).
 * needed: done with an unmerged PR or pushed branch; its dependents wait.
 * abandoned: the PR was closed without merging; its dependents still wait.
 * Null when there's nothing to show (no merge needed, merged, or not done).
 */
export function mergeBadge(t: Pick<Task, 'state' | 'merge_state'>): { label: string; hint: string } | null {
  if (t.state !== 'completed') return null
  if (t.merge_state === 'needed') {
    return { label: 'Merge needed', hint: 'Done, but its PR is not merged yet. Tickets that depend on it wait for the merge.' }
  }
  if (t.merge_state === 'abandoned') {
    return { label: 'PR closed', hint: 'Its PR was closed without merging. Tickets that depend on it still wait: reopen it, or mark the merge not needed in My work.' }
  }
  return null
}

/** Why a waiting ticket waits on a merge ("Waiting on merge of #N (PR …)"), or null. */
export function mergeWait(t: Pick<Task, 'state' | 'waiting_detail'>): string | null {
  if (t.state === 'completed') return null
  const d = t.waiting_detail?.trim()
  return d ? d : null
}

/** Done with a PR nobody merged yet (or one closed without merging): it holds its dependents. Mission's Merge needed readout. */
export function needsMerge(t: Pick<Task, 'state' | 'merge_state'>): boolean {
  return t.state === 'completed' && (t.merge_state === 'needed' || t.merge_state === 'abandoned')
}
