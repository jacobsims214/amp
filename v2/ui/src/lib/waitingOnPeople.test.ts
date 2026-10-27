import { describe, expect, it } from 'vitest'
import { countsSummary, idList, kindSummary, panelOrder, personLink, ticketsWaitingOn, waitingOnPeople } from './waitingOnPeople'
import type { Task } from '../types'

const APPROVED = { planned_at: '2026-10-27T10:00:00Z', approved_at: '2026-10-27T10:00:00Z' }
function task(id: number, over: Partial<Task> = {}): Task {
  return {
    id, project_id: 2, epic_id: 172, story_id: 413, name: `T${id}`, description: '', acceptance_criteria: '',
    state: 'backlog', priority: '1', dependency_ids: [], created_at: '2026-10-27T09:00:00Z', updated_at: '2026-10-27T09:00:00Z',
    ...APPROVED, ...over,
  }
}

describe('waitingOnPeople (mirrors Service.waitingOn)', () => {
  const tasks = [
    task(1, { owner_user_id: 7, state: 'blocked', block_reason: 'Which API?', updated_at: '2026-10-27T12:00:00Z' }),
    task(2, { owner_user_id: 7, state: 'blocked', block_reason: 'scheduled:2026-11-01' }),            // scheduled: not a question
    task(3, { owner_user_id: 7, state: 'completed', merge_state: 'needed', completed_at: '2026-10-27T11:00:00Z' }),
    task(4, { owner_user_id: 7, state: 'completed', merge_state: 'abandoned', completed_at: '2026-10-27T10:00:00Z' }),
    task(5, { owner_user_id: 7, state: 'completed', merge_state: 'merged' }),
    task(6, { owner_user_id: 8, performer: 'human', state: 'in_progress' }),
    task(7, { owner_user_id: 8, performer: 'human', state: 'backlog' }),
    task(8, { owner_user_id: 8, performer: 'human', state: 'blocked' }),                              // waits on deps: coming up
    task(9, { owner_user_id: 8, performer: 'human', state: 'backlog', approved_at: undefined }),      // awaits approval: approve + coming up
    task(10, { owner_user_id: 8, state: 'backlog', approved_at: undefined, epic_id: 172 }),
    task(11, { owner_user_id: 8, state: 'backlog', approved_at: undefined, epic_id: 173 }),
    task(12, { owner_user_id: null, created_by: 9, state: 'backlog', approved_at: undefined }),       // unowned: its creator approves
    task(13, { owner_user_id: 7, state: 'in_progress' }),                                            // agent work: nothing
  ]

  it('classifies each person by kind, in kind order, oldest first', () => {
    const w = waitingOnPeople(tasks, { requireApproval: true })
    const a = w.get(7)!
    expect(a.items.map(i => [i.kind, i.taskIds])).toEqual([['answer', [1]], ['merge', [4]], ['merge', [3]]])
    expect(a.counts).toEqual({ waiting: 3, approve: 0, answer: 1, merge: 2, do: 0, coming_up: 0 })

    const b = w.get(8)!
    expect(b.items.filter(i => i.kind === 'approve').map(i => [i.epicId, i.taskIds])).toEqual([[172, [9, 10]], [173, [11]]])
    expect(b.items.filter(i => i.kind === 'do').map(i => i.taskIds[0])).toEqual([6, 7])
    expect(b.comingUp).toEqual([8, 9])
    expect(b.counts).toEqual({ waiting: 4, approve: 2, answer: 0, merge: 0, do: 2, coming_up: 2 })

    expect(w.get(9)!.items).toEqual([{ kind: 'approve', taskIds: [12], epicId: 172 }])
  })

  it('without required approval there are no approve items, and awaiting human tickets are Do', () => {
    const b = waitingOnPeople(tasks, { requireApproval: false }).get(8)!
    expect(b.counts.approve).toBe(0)
    expect(b.items.map(i => i.taskIds[0])).toEqual([6, 7, 9])
    expect(waitingOnPeople(tasks, { requireApproval: false }).has(9)).toBe(false)
  })

  it('words, order and links', () => {
    const w = waitingOnPeople(tasks, { requireApproval: true })
    expect(kindSummary(w.get(7)!)).toBe('Answer 1 (#1) · Merge 2 (#4, #3)')
    expect(kindSummary(w.get(8)!)).toBe('Approve 2 (3 tickets) · Do 2 (#6, #7) · 2 coming up')
    expect(idList([1, 2, 3, 4])).toBe('#1, #2 +2')
    expect(countsSummary({ waiting: 3, approve: 0, answer: 1, merge: 2, do: 0, coming_up: 4 })).toBe('Answer 1 · Merge 2')
    expect(panelOrder(w, 9).map(p => p.userId)).toEqual([9, 8, 7])
    expect([...ticketsWaitingOn(w.get(8))].sort((x, y) => x - y)).toEqual([6, 7, 9, 10, 11])

    expect(personLink(2, w.get(7)!, { meId: 7, canReadOthers: false })).toBe('/project/2/my-work?focus=1')
    expect(personLink(2, w.get(7)!, { meId: 8, canReadOthers: true })).toBe('/project/2/my-work?user=7&focus=1')
    expect(personLink(2, w.get(7)!, { meId: 8, canReadOthers: false })).toBe('/project/2/board?focus=1')
  })
})
