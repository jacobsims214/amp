import { describe, expect, it } from 'vitest'
import { decisionAfterOption, openDependents, waitingSince, waitLine } from './decisions'

const now = Date.parse('2026-11-04T12:00:00Z')

describe('waitLine', () => {
  it('says what it blocks and how long it has waited, as the spec words it', () => {
    expect(waitLine(5, '2026-11-02T12:00:00Z', now)).toBe('blocks 5 · waiting 2d')
    expect(waitLine(0, '2026-11-04T09:00:00Z', now)).toBe('Nothing waits on it · waiting 3h')
    expect(waitLine(2, undefined, now)).toBe('blocks 2')
  })
})

describe('openDependents', () => {
  it('counts open tickets downstream, transitively, once each', () => {
    const tasks = [
      { id: 41, state: 'blocked', dependency_ids: [34] },
      { id: 42, state: 'blocked', dependency_ids: [34, 41] },
      { id: 44, state: 'blocked', dependency_ids: [42] },
      { id: 45, state: 'completed', dependency_ids: [34] },
      { id: 46, state: 'blocked', dependency_ids: [45] },
      { id: 50, state: 'backlog', dependency_ids: [] },
    ] as const
    expect(openDependents(34, tasks.map(t => ({ ...t, dependency_ids: [...t.dependency_ids] }))).sort()).toEqual([41, 42, 44])
  })
})

describe('waitingSince', () => {
  it('is when it was dispatched or became ready, as My work\'s Do item', () => {
    expect(waitingSince({ state: 'in_progress', dispatched_at: 'd', ready_at: 'r', created_at: 'c' })).toBe('d')
    expect(waitingSince({ state: 'backlog', dispatched_at: 'd', ready_at: 'r', created_at: 'c' })).toBe('r')
    expect(waitingSince({ state: 'blocked', created_at: 'c' })).toBe('c')
  })
})

describe('decisionAfterOption', () => {
  it('is the decided text without the label again', () => {
    expect(decisionAfterOption('B: Activation', 'B: Activation — Go with B')).toBe('Go with B')
    expect(decisionAfterOption('B: Activation', 'B: Activation')).toBe('')
    expect(decisionAfterOption('B: Activation', 'Something else')).toBe('Something else')
  })
})
