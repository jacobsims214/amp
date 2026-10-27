// Integration branches and waits_for_merge: false (#2597). Neither is a merge
// waiting on you: no Board badge, no Merge kind, no Merge needed readout.
import { describe, expect, it } from 'vitest'
import { effectiveMetadata, integrationBranch, mergeBadge, mergedIntoIntegration, needsMerge, runWentTo, waitsForMerge } from './merge'
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
