import { describe, expect, it } from 'vitest'
import { ago, approvalOf, approvalOwner, awaitingApproval, canApproveOnBehalf, changedSince, changedSinceNotice, isYours, listNames, splitAwaiting, ticketApproved, compareInstants, isRelayed, newestPlannedAt, planPath, plannedAt, relayedFor } from './approval'
import type { Task } from '../types'

const task = (id: number, over: Partial<Task> = {}): Task => ({
  id, project_id: 1, epic_id: 1, story_id: 1, name: `t${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_at: '2026-10-20T10:00:00Z', updated_at: '2026-10-20T10:00:00Z', ...over,
})

describe('awaitingApproval (per ticket, #2198)', () => {
  it('is every open ticket nobody has approved', () => {
    const ts = [task(1), task(2, { state: 'completed' }), task(3, { state: 'blocked' })]
    expect(awaitingApproval(ts, {}).map(t => t.id)).toEqual([1, 3])
  })

  it('is the open tickets not approved since they were last planned, oldest plan first', () => {
    const ts = [
      task(1, { planned_at: '2026-10-21T09:00:00Z', approved_at: '2026-10-22T10:00:00Z' }), // approved
      task(2, { planned_at: '2026-10-22T12:00:00Z' }),                                       // never approved
      task(3, { planned_at: '2026-10-22T11:00:00Z', approved_at: '2026-10-22T10:00:00Z' }), // re-planned after
      task(4, { planned_at: '2026-10-22T13:00:00Z', state: 'completed' }),
    ]
    expect(awaitingApproval(ts, {}).map(t => t.id)).toEqual([3, 2])
  })

  it("ignores the project's last approval: it no longer gates anything", () => {
    const t = task(5, { planned_at: '2026-10-22T09:00:00Z' })
    expect(awaitingApproval([t], { plan_approved_at: '2026-10-22T10:00:00Z' } as object)).toHaveLength(1)
  })

  it('counts a ticket approved at exactly its plan time as approved, to the microsecond', () => {
    const at = '2026-10-22T21:40:12.345678Z'
    expect(ticketApproved(task(6, { planned_at: at, approved_at: at }))).toBe(true)
    expect(ticketApproved(task(6, { planned_at: '2026-10-22T21:40:12.345679Z', approved_at: at }))).toBe(false)
    expect(ticketApproved(task(6, { approved_at: '0001-01-01T00:00:00Z' }))).toBe(false)
  })

  it('falls back to created_at for the plan time', () => {
    const t = task(7, { created_at: '2026-10-22T12:00:00Z', approved_at: '2026-10-22T11:00:00Z' })
    expect(plannedAt(t)).toBe('2026-10-22T12:00:00Z')
    expect(awaitingApproval([t], {})).toHaveLength(1)
  })

  it('is nothing until the project has loaded', () => {
    expect(awaitingApproval([task(1)], null)).toEqual([])
  })
})

describe('whose approval a ticket waits for', () => {
  it('is the owner, else whoever planned it', () => {
    expect(approvalOwner(task(1, { owner_user_id: 9, created_by: 7 }))).toBe(9)
    expect(approvalOwner(task(1, { owner_user_id: null, created_by: 7 }))).toBe(7)
    expect(approvalOwner(task(1))).toBeNull()
  })

  it('splits what waits into yours and others, keeping order', () => {
    const ts = [task(1, { owner_user_id: 9 }), task(2, { owner_user_id: 7 }), task(3, { created_by: 7 }), task(4, { created_by: 9 })]
    const { yours, others } = splitAwaiting(ts, 7)
    expect(yours.map(t => t.id)).toEqual([2, 3])
    expect(others.map(t => t.id)).toEqual([1, 4])
  })

  it("treats a ticket with no owner and no planner as local dev's own only", () => {
    expect(isYours(task(1), undefined)).toBe(true)
    expect(isYours(task(1), 0)).toBe(true)
    expect(isYours(task(1), 7)).toBe(false)
  })

  it('lets only leads and above approve on behalf', () => {
    expect(canApproveOnBehalf('member')).toBe(false)
    expect(canApproveOnBehalf(null)).toBe(false)
    for (const a of ['lead', 'owner', 'org_admin', 'global_admin'] as const) expect(canApproveOnBehalf(a)).toBe(true)
  })

  it('names owners naturally', () => {
    expect(listNames(['Bob'])).toBe('Bob')
    expect(listNames(['Bob', 'Carol', 'Bob'])).toBe('Bob and Carol')
    expect(listNames(['Bob', 'Carol', 'Dee'])).toBe('Bob, Carol and Dee')
  })
})

describe('who approved a ticket', () => {
  const at = '2026-10-22T12:00:00Z'
  it('is the approver, and the owner when they approved it on their behalf', () => {
    expect(approvalOf(task(1, { owner_user_id: 7, approved_at: at, approved_by: 7 }))).toEqual({ at, approvedBy: 7, onBehalfOf: null })
    expect(approvalOf(task(1, { owner_user_id: 9, approved_at: at, approved_by: 7 }))).toEqual({ at, approvedBy: 7, onBehalfOf: 9 })
    expect(approvalOf(task(1, { created_by: 9, approved_at: at, approved_by: 7 }))?.onBehalfOf).toBe(9)
  })
  it('is a migration when approved_by is empty', () => {
    expect(approvalOf(task(1, { owner_user_id: 9, approved_at: at, approved_by: null }))).toEqual({ at, approvedBy: null, onBehalfOf: null })
  })
  it('is nothing while the current plan is not approved', () => {
    expect(approvalOf(task(1, { planned_at: '2026-10-22T13:00:00Z', approved_at: at, approved_by: 7 }))).toBeNull()
  })
})

describe('changedSince (the notice after a click)', () => {
  it('counts your own tickets the page did not show, and shown ones re-planned or re-owned', () => {
    const r = { awaitingYours: [4, 5, 6], skipped: { '2': 'it was planned after what was approved (as_of)', '3': 'it is completed', '8': 'its owner changed' } }
    // 5 was shown and deliberately left unticked; 4 and 6 arrived after the page loaded.
    expect(changedSince(r, [1, 2, 3, 5, 8])).toEqual([2, 4, 6, 8])
    expect(changedSince({ awaitingYours: [], skipped: {} }, [1])).toEqual([])
  })
})

describe('plan times (as_of, #2192)', () => {
  it('ignores a zero planned_at and falls back to created_at', () => {
    expect(plannedAt(task(1, { planned_at: '0001-01-01T00:00:00Z' }))).toBe('2026-10-20T10:00:00Z')
  })

  it('compares to the microsecond, across fraction lengths and offsets', () => {
    expect(compareInstants('2026-10-22T21:40:12.345678Z', '2026-10-22T21:40:12.345Z')).toBeGreaterThan(0)
    expect(compareInstants('2026-10-22T21:40:12Z', '2026-10-22T21:40:12.5Z')).toBeLessThan(0)
    expect(compareInstants('2026-10-22T21:40:12.1Z', '2026-10-22T21:40:12.100000Z')).toBe(0)
    expect(compareInstants('2026-10-22T23:40:12.000001+02:00', '2026-10-22T21:40:12Z')).toBeGreaterThan(0)
  })

  it('newestPlannedAt is the newest shown plan time, verbatim (no Date round trip)', () => {
    const ts = [
      task(1, { planned_at: '2026-10-22T21:40:12.345678Z' }),
      task(2, { planned_at: '2026-10-22T21:40:12.345Z' }),
      task(3, { planned_at: '2026-10-22T21:40:11.999999Z' }),
    ]
    expect(newestPlannedAt(ts)).toBe('2026-10-22T21:40:12.345678Z')
    expect(newestPlannedAt([])).toBeUndefined()
  })

  it('keeps a ticket awaiting when it was planned microseconds after its approval', () => {
    const t = task(4, { planned_at: '2026-10-22T21:40:12.345679Z' })
    expect(awaitingApproval([{ ...t, approved_at: '2026-10-22T21:40:12.345678Z' }], {})).toHaveLength(1)
    expect(awaitingApproval([{ ...t, approved_at: '2026-10-22T21:40:12.345679Z' }], {})).toHaveLength(0)
  })

  it('tells the person how many came in after the page loaded', () => {
    expect(changedSinceNotice(1)).toContain('1 ticket was added or changed since this page loaded')
    expect(changedSinceNotice(3)).toContain('3 tickets were added or changed')
  })
})

describe('relayed answers', () => {
  it('recognises the server suffix and recovers the person', () => {
    expect(isRelayed('jo@example.com (relayed by agent)')).toBe(true)
    expect(relayedFor('jo@example.com (relayed by agent)')).toBe('jo@example.com')
  })
  it('leaves a person\'s own answer alone', () => {
    expect(isRelayed('jo@example.com')).toBe(false)
    expect(isRelayed('')).toBe(false)
    expect(isRelayed(undefined)).toBe(false)
    expect(relayedFor('jo@example.com')).toBe('jo@example.com')
  })
})

describe('links and times', () => {
  it('points at the Flight plan, the approve_url the server hands agents', () => {
    expect(planPath(42)).toBe('/project/42/plan')
  })
  it('says how long ago', () => {
    const now = Date.parse('2026-10-22T12:00:00Z')
    expect(ago('2026-10-22T11:59:40Z', now)).toBe('just now')
    expect(ago('2026-10-22T11:55:00Z', now)).toBe('5m ago')
    expect(ago('2026-10-22T09:00:00Z', now)).toBe('3h ago')
    expect(ago('2026-10-19T12:00:00Z', now)).toBe('3d ago')
    expect(ago(undefined, now)).toBe('')
  })
})
