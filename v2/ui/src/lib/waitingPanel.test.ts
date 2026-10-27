import { describe, expect, it } from 'vitest'
import { countsSummary, idList, kindSummary, panelOrder, personLink, ticketsWaitingOn } from './waitingPanel'
import type { PersonWaiting, WaitCounts } from '../types'

const at = '2026-10-27T10:00:00Z'
const wc = (o: Partial<WaitCounts>): WaitCounts => ({ waiting: 0, approve: 0, answer: 0, merge: 0, do: 0, coming_up: 0, ...o })
// As GET /projects/:id/waiting-on-people returns them (user id order).
const people: PersonWaiting[] = [
  { user_id: 7, counts: wc({ waiting: 3, answer: 1, merge: 2 }), coming_up: [], items: [
    { kind: 'answer', task_ids: [1], since: at }, { kind: 'merge', task_ids: [4], since: at }, { kind: 'merge', task_ids: [3], since: at }] },
  { user_id: 8, counts: wc({ waiting: 4, approve: 2, do: 2, coming_up: 2 }), coming_up: [8, 9], items: [
    { kind: 'approve', task_ids: [9, 10], epic_id: 172, epic_name: 'E', since: at }, { kind: 'approve', task_ids: [11], epic_id: 173, since: at },
    { kind: 'do', task_ids: [6], since: at }, { kind: 'do', task_ids: [7], since: at }] },
  { user_id: 9, counts: wc({ coming_up: 1 }), coming_up: [12], items: [] },
]

describe('waitingPanel (wording, order and links for the server items)', () => {
  it('summarises a person by kind', () => {
    expect(kindSummary(people[0])).toBe('Answer 1 (#1) · Merge 2 (#4, #3)')
    expect(kindSummary(people[1])).toBe('Approve 2 (3 tickets) · Do 2 (#6, #7) · 2 coming up')
    expect(kindSummary(people[2])).toBe('1 coming up')
    expect(idList([1, 2, 3, 4])).toBe('#1, #2 +2')
    expect(countsSummary(wc({ waiting: 3, answer: 1, merge: 2, coming_up: 4 }))).toBe('Answer 1 · Merge 2')
  })

  it('orders the viewer first, then most waiting, and does not reorder its input', () => {
    expect(panelOrder(people, 9).map(p => p.user_id)).toEqual([9, 8, 7])
    expect(panelOrder(people, null).map(p => p.user_id)).toEqual([8, 7, 9])
    expect(people.map(p => p.user_id)).toEqual([7, 8, 9])
  })

  it('collects the tickets that wait on a person', () => {
    expect([...ticketsWaitingOn(people[1])].sort((x, y) => x - y)).toEqual([6, 7, 9, 10, 11])
    expect(ticketsWaitingOn(undefined).size).toBe(0)
  })

  it('links rows into My work, or the Board for a member', () => {
    expect(personLink(2, people[0], { meId: 7, canReadOthers: false })).toBe('/project/2/my-work?focus=1')
    expect(personLink(2, people[0], { meId: 8, canReadOthers: true })).toBe('/project/2/my-work?user=7&focus=1')
    expect(personLink(2, people[0], { meId: 8, canReadOthers: false })).toBe('/project/2/board?focus=1')
    expect(personLink(2, people[2], { meId: 8, canReadOthers: true })).toBe('/project/2/my-work?user=9&focus=12')
  })
})
