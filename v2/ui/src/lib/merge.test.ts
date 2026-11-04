// Integration branches and waits_for_merge: false (#2597). Neither is a merge
// waiting on you: no Board badge, no Merge kind, no Merge needed readout.
import { describe, expect, it } from 'vitest'
import {
  effectiveMetadata, hasMergeContract, integrationBranch, markMergedRank, mergeBadge, mergeCardState, MERGE_MARK_GUARD,
  mergedIntoIntegration, mergePrerequisites, mergeRoute, mergeRouteText, mergeTargetChip, mergeWaiters, mergeWhyText,
  needsMerge, runWentTo, waitsForMerge,
} from './merge'
import { actionKindOf, mergeStatusText } from './humanActions'
import type { Task } from '../types'

const done = (over: Partial<Task> = {}) => ({ state: 'completed' as const, merge_state: 'none' as const, ...over })
const run = (branch: string, over: object = {}) => ({ outcome: 'completed' as const, branch, ...over })

describe('effective metadata', () => {
  it('puts the ticket\'s own keys on top of the inherited ones', () => {
    expect(effectiveMetadata({ integration_branch: 'fix/a', x: 1 }, { integration_branch: 'fix/b' })).toEqual({ integration_branch: 'fix/b', x: 1 })
    expect(effectiveMetadata(undefined, undefined)).toEqual({})
  })
  it('reads the integration branch, ignoring blanks and non-text', () => {
    expect(integrationBranch({ integration_branch: ' fix/merge-gate ' })).toBe('fix/merge-gate')
    expect(integrationBranch({ integration_branch: '  ' })).toBeNull()
    expect(integrationBranch({ integration_branch: 3 })).toBeNull()
    expect(integrationBranch(undefined)).toBeNull()
  })
  it('waits for merges unless waits_for_merge is false (boolean or text)', () => {
    expect(waitsForMerge({ waits_for_merge: false })).toBe(false)
    expect(waitsForMerge({ waits_for_merge: 'False' })).toBe(false)
    expect(waitsForMerge({ waits_for_merge: true })).toBe(true)
    expect(waitsForMerge({})).toBe(true)
    expect(waitsForMerge(undefined)).toBe(true)
  })
})

describe('merged into the integration branch', () => {
  const meta = { integration_branch: 'fix/merge-gate' }
  it('names the branch when a run pushed to it or opened a PR into it', () => {
    expect(mergedIntoIntegration(done(), meta, [run('fix/merge-gate')])).toBe('fix/merge-gate')
    expect(mergedIntoIntegration(done(), meta, [run('fix/merge-gate-F', { base_branch: 'fix/merge-gate' })])).toBe('fix/merge-gate')
    expect(mergedIntoIntegration(done({ merge_state: '' }), meta, [run('fix/merge-gate')])).toBe('fix/merge-gate')
  })
  it('is null when the run went elsewhere, was abandoned, or nothing names a branch', () => {
    expect(mergedIntoIntegration(done(), meta, [run('fix/other')])).toBeNull()
    expect(mergedIntoIntegration(done(), meta, [])).toBeNull()
    expect(runWentTo(run('fix/merge-gate', { outcome: 'abandoned' }), 'fix/merge-gate')).toBe(false)
    expect(mergedIntoIntegration(done(), {}, [run('fix/merge-gate')])).toBeNull()
  })
  it('is null before done, and once the merge to main is recorded or gates', () => {
    expect(mergedIntoIntegration({ state: 'in_progress', merge_state: 'none' }, meta, [run('fix/merge-gate')])).toBeNull()
    expect(mergedIntoIntegration(done({ merge_state: 'merged' }), meta, [run('fix/merge-gate')])).toBeNull()
    expect(mergedIntoIntegration(done({ merge_state: 'needed' }), meta, [run('fix/merge-gate')])).toBeNull()
  })
  it('is not a merge waiting on you: no Board badge, no Merge kind, no Merge needed line', () => {
    // The server keeps merge_state none until the merge to main (ticket #2596).
    const t = { ...done({ merge_required: true }), block_reason: '', performer: 'agent' as const }
    expect(mergeBadge(t)).toBeNull()
    expect(needsMerge(t)).toBe(false)
    expect(actionKindOf(t)).toBeNull()
    expect(mergeStatusText(t)).toBeNull()
  })
})

describe('waits_for_merge: false dependents', () => {
  it('a done dependency that does not hold it is not a merge waiting on you', () => {
    const t = { state: 'backlog' as const, merge_state: 'none' as const, performer: 'agent' as const, block_reason: '' }
    expect(mergeBadge(t)).toBeNull()
    expect(actionKindOf(t)).toBeNull()
  })
})

// The Merge card's contract (#3011 + #3009, approved artifact #27 v2): where a
// merge goes, who really waits, and the plan's step folded in. These are the
// three cases the ticket calls out — linked, legacy and external — plus the one
// rule the whole card rests on: an unknown target is shown as unknown.
describe('mergeRoute: the PR, its head and its target', () => {
  const item = (over: Partial<Parameters<typeof mergeRoute>[0]> = {}) => ({
    pr_url: 'https://github.com/o/r/pull/88', branch: 'feat/x', base_branch: 'main', target_source: 'run' as const, ...over,
  })

  it('names the PR number, the head and the target a run reported', () => {
    expect(mergeRoute(item())).toEqual({ pr: 'PR #88', head: 'feat/x', target: 'main', known: true })
    expect(mergeRouteText(mergeRoute(item()))).toBe('PR #88 · feat/x → main')
  })

  it('shows an unknown target as unknown, never as main', () => {
    const r = mergeRoute(item({ base_branch: undefined, target_source: 'unknown' }))
    expect(r).toEqual({ pr: 'PR #88', head: 'feat/x', target: null, known: false })
    expect(mergeRouteText(r)).toBe('PR #88 · feat/x · target not verified')
  })

  it('does not trust a base_branch without target_source "run"', () => {
    // A stray base_branch is not provenance: only a run's report counts.
    expect(mergeRoute(item({ target_source: 'unknown' })).target).toBeNull()
    expect(mergeRoute(item({ target_source: undefined })).known).toBe(false)
  })

  it('degrades without a PR URL or a head, and trims blanks', () => {
    expect(mergeRoute(item({ pr_url: undefined, branch: undefined, base_branch: ' main ' })))
      .toEqual({ pr: 'PR', head: null, target: 'main', known: true })
    expect(mergeRouteText(mergeRoute(item({ pr_url: undefined, branch: '  ' })))).toBe('PR → main')
  })
})

describe('mergeCardState: linked, legacy and external merges', () => {
  const base = { merge_state: 'needed' as const, target_source: 'run' as const }

  it('linked: the plan step gates the click until its prerequisites are met', () => {
    expect(mergeCardState({ ...base, holding_up: [2], tracked_by: { task_id: 9, name: 'Merge', state: 'backlog', ready: false, waiting_on: [2] } }))
      .toBe('gated-waiting')
    expect(mergeCardState({ ...base, holding_up: [2], tracked_by: { task_id: 9, name: 'Merge', state: 'backlog', ready: true } }))
      .toBe('gated-ready')
  })

  it('legacy: without the contract an unknown waiter set is not "nothing waits"', () => {
    // Pre-#3011 servers send no target_source; holding_up was never set. Reading
    // that as "nothing waits" would demote a real merge to a quiet afterthought.
    expect(mergeCardState({ merge_state: 'needed' })).toBe('waiting')
    expect(mergeCardState({ merge_state: 'needed', target_source: undefined, holding_up: undefined })).toBe('waiting')
    // With the contract, an empty waiter list is a fact, not a gap.
    expect(mergeCardState({ merge_state: 'needed', target_source: 'run' })).toBe('quiet')
  })

  it('external: a PR closed without merging outranks everything', () => {
    expect(mergeCardState({ ...base, merge_state: 'abandoned', holding_up: [2], tracked_by: { task_id: 9, name: 'Merge', state: 'backlog', ready: true } }))
      .toBe('abandoned')
  })
})

describe('markMergedRank: one action per PR, never a dead one', () => {
  it('offers Mark merged as the action only where it does something', () => {
    expect(markMergedRank('waiting')).toBe('primary')
    expect(markMergedRank('gated-ready')).toBe('primary')
    // Nothing waits: GitHub or CI records it, so the click is demoted, not the
    // headline action (approved artifact #27 v2, state 2).
    expect(markMergedRank('quiet')).toBe('secondary')
  })

  it('offers no Mark merged on a closed PR or a gated step: both would do nothing', () => {
    expect(markMergedRank('abandoned')).toBeNull()
    expect(markMergedRank('gated-waiting')).toBeNull()
  })
})

describe('mergePrerequisites', () => {
  it('lists only what still holds a step, and nothing once it is ready', () => {
    expect(mergePrerequisites(undefined)).toEqual([])
    expect(mergePrerequisites({ task_id: 9, name: 'Merge', state: 'backlog', ready: true, waiting_on: [2] })).toEqual([])
    expect(mergePrerequisites({ task_id: 9, name: 'Merge', state: 'blocked', ready: false, waiting_on: [2, 3] })).toEqual([2, 3])
    expect(mergePrerequisites({ task_id: 9, name: 'Merge', state: 'blocked', ready: false })).toEqual([])
  })
})

describe('mergeWaiters: only real waiters are counted', () => {
  const item = (over: object = {}) => ({ target_source: 'run' as const, holding_up: [2490, 2491], ...over })

  it('counts the tickets a merge holds and names them', () => {
    expect(mergeWaiters(item())).toEqual({
      lead: '2 tickets wait on this merge', detail: ': #2490, #2491. They start when the merge is recorded.',
    })
    expect(mergeWaiters(item({ holding_up: [2490] })).lead).toBe('1 ticket waits on this merge')
  })

  it('says nothing waits, naming the opted-out dependents instead of counting them', () => {
    const w = mergeWaiters(item({ holding_up: [], not_waiting: [2492] }))
    expect(w.lead).toBe('Nothing waits on this merge')
    expect(w.detail).toContain('#2492 opted out (waits_for_merge: false)')
    expect(w.detail).toContain('Recording it here is only for a merge you already made')
    // #2978's bug: an opted-out dependent must never be counted as a waiter.
    expect(w.lead).not.toContain('1 ticket waits')
  })

  it('truncates long waiter lists rather than crowding the card', () => {
    expect(mergeWaiters(item({ holding_up: [1, 2, 3, 4, 5, 6] })).detail).toContain('#1, #2, #3, #4 and 2 more')
  })

  it('admits when the server did not report waiters at all', () => {
    expect(mergeWaiters({ target_source: undefined }).lead).toBe('Who waits on this merge')
  })
})

describe('the why-line: unknown is shown, never assumed', () => {
  it('names a reported target and says plainly that AMP cannot verify it', () => {
    const why = mergeWhyText({ base_branch: 'main', target_source: 'run' })
    expect(why).toContain('reported main as the target')
    expect(why).toContain("can't confirm that this is the default branch")
  })

  it('says unknown when no run reported the target, and still shows the card', () => {
    const why = mergeWhyText({ target_source: 'unknown' })
    expect(why).toContain("couldn't verify this PR's target")
    expect(why).toContain('shown rather than hidden')
    // Never claim the default branch, and never render unknown as "internal".
    expect(why).not.toContain('default branch or')
    expect(why).not.toContain('internal')
  })

  it('marks the unknown chip as a warning, the reported one as plain', () => {
    expect(mergeTargetChip({ target_source: 'unknown' })).toEqual({ text: 'target & rules: unknown — AMP reads no repository settings', warn: true })
    expect(mergeTargetChip({ target_source: 'run' })).toEqual({ text: 'target: as its run reported it', warn: false })
  })

  it('keeps the Mark merged guard honest: it records a merge, it does not make one', () => {
    expect(MERGE_MARK_GUARD).toContain('only after you merged it on GitHub')
    expect(MERGE_MARK_GUARD).toContain('card clears itself and no click is owed')
  })
})

describe('hasMergeContract', () => {
  it('tells the #3011 contract from a pre-contract item', () => {
    expect(hasMergeContract({ target_source: 'run' })).toBe(true)
    expect(hasMergeContract({ target_source: 'unknown' })).toBe(true)
    expect(hasMergeContract({})).toBe(false)
  })
})
