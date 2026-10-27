import type { Metadata, Run, Task } from '../types'

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

/*
 * Integration branches and review tickets (#2596 server, #2597 UI). Both are
 * read from a ticket's effective metadata (its project's, epic's and story's
 * metadata with its own on top):
 *   integration_branch: <name>  a run pushed to that branch (or with a PR into
 *     it) doesn't hold its dependents. merge_state stays none until the merge
 *     to main is recorded, so it never shows as Merge needed.
 *   waits_for_merge: false  on a dependent: it starts when its dependencies are
 *     done, without waiting for their merge (review tickets).
 */

/** The effective metadata: inherited keys with the ticket's own on top. */
export function effectiveMetadata(inherited: Metadata | undefined, own: Metadata | undefined): Metadata {
  return { ...(inherited ?? {}), ...(own ?? {}) }
}

/** The integration branch the effective metadata names, or null. */
export function integrationBranch(meta: Metadata | undefined): string | null {
  const v = meta?.integration_branch
  if (typeof v !== 'string') return null
  const b = v.trim()
  return b ? b : null
}

/** False when the effective metadata says waits_for_merge: false (a boolean, or the text "false"). */
export function waitsForMerge(meta: Metadata | undefined): boolean {
  const v = meta?.waits_for_merge
  if (v === false) return false
  if (typeof v === 'string' && v.trim().toLowerCase() === 'false') return false
  return true
}

type IntegrationRun = Pick<Run, 'outcome' | 'branch'> & { base_branch?: string; pr_base?: string }

/** Whether a run's work went to the branch: pushed to it, or a PR into it. */
export function runWentTo(r: IntegrationRun, branch: string): boolean {
  if (r.outcome === 'abandoned') return false
  return [r.branch, r.base_branch, r.pr_base].some(b => b?.trim() === branch)
}

/**
 * The integration branch a done ticket's work went into, while the merge to
 * main is still to come: "Merged into <branch>, waiting on the merge to main".
 * Null once the merge to main is recorded, or when its merge state gates
 * anything (then it's a plain Merge needed).
 */
export function mergedIntoIntegration(
  t: Pick<Task, 'state' | 'merge_state'>, meta: Metadata | undefined, runs: IntegrationRun[],
): string | null {
  if (t.state !== 'completed') return null
  if (t.merge_state && t.merge_state !== 'none') return null
  const branch = integrationBranch(meta)
  if (!branch) return null
  return runs.some(r => runWentTo(r, branch)) ? branch : null
}
