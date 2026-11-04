import type { Metadata, MergeStepFold, Run, Task, WaitItem } from '../types'

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

/*
 * The Merge card's contract (AMP #3011, approved artifact #27 v2,
 * architecture/merge-card-contract.md). Everything a card says about where a
 * merge goes, who really waits, and whether the plan's step is due is derived
 * here, so ProjectMyWork, HumanActions and the TaskDrawer can't disagree.
 *
 * The one fact AMP cannot compute is whether the target is the repository's
 * default branch or covered by a review rule: amp-api holds no repository
 * metadata (no slug, default branch, branch protection or CODEOWNERS) and has
 * no host API client. So `targetSource: 'unknown'` is shown as unknown — with
 * a warning and a resolution path — never assumed to be "main" (which would
 * over-alert) and never assumed internal (which would hide a real merge).
 */

export interface MergeRoute {
  /** "PR #88", or "PR" when the URL names none */
  pr: string
  /** the head branch the work is on, when a run reported it */
  head: string | null
  /** the branch the merge would land on, when a run reported it */
  target: string | null
  /** false when nothing reported the target: unknown, never a guess */
  known: boolean
}

/**
 * Where a Merge item goes: PR number, head, and target — or an explicit
 * unknown. The target is only a branch a run reported (`target_source: "run"`);
 * there is no fallback value, because AMP has nothing to fall back to (above).
 * A base_branch that arrived without that provenance is not trusted.
 */
export function mergeRoute(
  it: Pick<WaitItem, 'pr_url' | 'branch' | 'base_branch' | 'target_source'>,
): MergeRoute {
  const m = it.pr_url?.match(/\/pull\/(\d+)/)
  const target = it.target_source === 'run' ? it.base_branch?.trim() || null : null
  return {
    pr: m ? `PR #${m[1]}` : 'PR',
    head: it.branch?.trim() || null,
    target,
    known: target !== null,
  }
}

/** "PR #88 · feat/x → main", or "PR #88 · feat/x · target not verified". */
export function mergeRouteText(r: MergeRoute): string {
  const parts = [r.pr, r.head].filter(Boolean) as string[]
  return r.known ? `${parts.join(' · ')} → ${r.target}` : `${parts.join(' · ')} · target not verified`
}

/**
 * What a Merge card says about who waits on it. Split in two so every surface
 * can emphasise the fact (the count, or that nothing waits) and keep the rest
 * quiet, without restating the wording in a second place.
 *
 * Only `holding_up` is counted: `not_waiting` holds the dependents that opted
 * out with waits_for_merge: false, so naming them as held up would overstate
 * every card that has one (#2978's bug: "1 ticket waits" when it didn't).
 */
export interface MergeWaiters {
  /** the fact, emphasised: "2 tickets wait on this merge" / "Nothing waits on this merge" */
  lead: string
  /** the rest: who waits, what that means for the action */
  detail: string
}

export function mergeWaiters(
  it: Pick<WaitItem, 'holding_up' | 'not_waiting' | 'target_source'>,
): MergeWaiters {
  if (!hasMergeContract(it)) {
    return { lead: 'Who waits on this merge', detail: " isn't reported by this server." }
  }
  const heldUp = it.holding_up ?? []
  const opted = it.not_waiting ?? []
  const names = (ids: number[]) => ids.slice(0, 4).map(id => `#${id}`).join(', ')
  if (heldUp.length > 0) {
    return {
      lead: `${heldUp.length} ${heldUp.length === 1 ? 'ticket waits' : 'tickets wait'} on this merge`,
      detail: `: ${names(heldUp)}${heldUp.length > 4 ? ` and ${heldUp.length - 4} more` : ''}. They start when the merge is recorded.`,
    }
  }
  const why = opted.length > 0
    ? ` — ${names(opted)} opted out (waits_for_merge: false), so didn't wait for it`
    : ''
  return {
    lead: 'Nothing waits on this merge',
    detail: `${why}. Recording it here is only for a merge you already made.`,
  }
}

/**
 * What a Merge card is asking for now. The order matters: a PR closed without
 * merging can't be marked merged; a plan's step gates the click until its
 * prerequisites are met; real waiters make the merge the primary action;
 * otherwise nothing is held up and recording it is a quiet second thought
 * (approved artifact #27 v2: "GitHub or CI will record it").
 *
 * `target_source` is what says the #3011 contract is present at all: a Merge
 * item always sends it, while `holding_up` is omitted when empty. Without the
 * contract (the TaskDrawer reads runs, not a WaitItem) an unknown waiter set
 * must not be read as "nothing waits" — it keeps the ordinary action.
 */
export type MergeCardState = 'abandoned' | 'gated-waiting' | 'gated-ready' | 'waiting' | 'quiet'

export function mergeCardState(
  it: Pick<WaitItem, 'merge_state' | 'holding_up' | 'tracked_by' | 'target_source'>,
): MergeCardState {
  if (it.merge_state === 'abandoned') return 'abandoned'
  if (it.tracked_by) return it.tracked_by.ready ? 'gated-ready' : 'gated-waiting'
  if (!hasMergeContract(it)) return 'waiting'
  return (it.holding_up?.length ?? 0) > 0 ? 'waiting' : 'quiet'
}

/** How prominent Mark merged is: the card's action, a quiet manual record, or absent. */
export type MarkMergedRank = 'primary' | 'secondary' | null

/**
 * Mark merged is never offered where it would do nothing: not on a closed PR,
 * and not while the plan's step is still gated (the backend refuses a merge
 * report on a step waiting on prerequisites, so the button would be a lie).
 * With nothing waiting it stays, demoted: a real merge still has to be
 * recorded, but it isn't what the person must do next.
 */
export function markMergedRank(s: MergeCardState): MarkMergedRank {
  switch (s) {
    case 'abandoned': return null
    case 'gated-waiting': return null
    case 'gated-ready': return 'primary'
    case 'waiting': return 'primary'
    case 'quiet': return 'secondary'
  }
}

/** The prerequisites still holding a folded plan step (its open dependencies). */
export function mergePrerequisites(step: MergeStepFold | undefined): number[] {
  return step && !step.ready ? step.waiting_on ?? [] : []
}

/**
 * Whether a Merge item carries the #3011 contract. `target_source` is always
 * sent for a merge item ("run" or "unknown"), while `holding_up` is omitted
 * when empty — so this, not the presence of the array, is what tells a card
 * that "nothing waits" is a fact rather than missing data.
 */
export function hasMergeContract(it: Pick<WaitItem, 'target_source'>): boolean {
  return it.target_source !== undefined
}

/**
 * Why this PR is on the person's list. Artifact #27 v2's alert policy is
 * "targets the detected default branch, or a repository rule requires your
 * approval" — and AMP can compute neither, because it holds no repository
 * metadata. So a card states the facts it has and says plainly what it could
 * not verify. It never claims the default branch, and never renders unknown as
 * "internal, nothing to do" (which would silently hide a real merge).
 */
export function mergeWhyText(it: Pick<WaitItem, 'base_branch' | 'target_source'>): string {
  if (it.target_source === 'run' && it.base_branch?.trim()) {
    return `Its run reported ${it.base_branch.trim()} as the target. AMP reads no repository settings, so it can't confirm that this is the default branch or that a rule requires your approval.`
  }
  return "AMP couldn't verify this PR's target or whether a repository rule requires your approval, so it's shown rather than hidden."
}

/** The chip that names where the target (and so the policy) stands: never assumed, never hidden. */
export function mergeTargetChip(it: Pick<WaitItem, 'target_source'>): { text: string; warn: boolean } {
  return it.target_source === 'run'
    ? { text: 'target: as its run reported it', warn: false }
    : { text: 'target & rules: unknown — AMP reads no repository settings', warn: true }
}

/**
 * Mark merged's guard copy: the click records a merge the person made
 * elsewhere, so the label must never read as the act of merging.
 */
export const MERGE_MARK_GUARD =
  'Use Mark merged only after you merged it on GitHub. If GitHub or CI records the merge first, this card clears itself and no click is owed.'
