import { describe, expect, it } from 'vitest'
import { ago, awaitingApproval, isRelayed, planPath, plannedAt, relayedFor } from './approval'
import type { Task } from '../types'

const task = (id: number, over: Partial<Task> = {}): Task => ({
  id, project_id: 1, epic_id: 1, story_id: 1, name: `t${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_at: '2026-10-20T10:00:00Z', updated_at: '2026-10-20T10:00:00Z', ...over,
})

describe('awaitingApproval', () => {
  it('is every open ticket when the plan was never approved', () => {
    const ts = [task(1), task(2, { state: 'completed' }), task(3, { state: 'blocked' })]
    expect(awaitingApproval(ts, {}).map(t => t.id)).toEqual([1, 3])
  })

  it('is the open tickets planned after the last approval, oldest plan first', () => {
    const ts = [
      task(1, { planned_at: '2026-10-21T09:00:00Z' }),            // before approval
      task(2, { planned_at: '2026-10-22T12:00:00Z' }),            // after
      task(3, { planned_at: '2026-10-22T11:00:00Z' }),            // after, earlier
      task(4, { planned_at: '2026-10-22T13:00:00Z', state: 'completed' }),
    ]
    expect(awaitingApproval(ts, { plan_approved_at: '2026-10-22T10:00:00Z' }).map(t => t.id)).toEqual([3, 2])
  })

  it('counts a ticket rewritten after approval (planned_at moved) even if created before', () => {
    const t = task(5, { created_at: '2026-10-01T00:00:00Z', planned_at: '2026-10-22T12:00:00Z' })
    expect(awaitingApproval([t], { plan_approved_at: '2026-10-22T10:00:00Z' })).toHaveLength(1)
  })

  it('falls back to created_at on a server without planned_at', () => {
    const t = task(6, { created_at: '2026-10-22T12:00:00Z' })
    expect(plannedAt(t)).toBe('2026-10-22T12:00:00Z')
    expect(awaitingApproval([t], { plan_approved_at: '2026-10-22T10:00:00Z' })).toHaveLength(1)
    expect(awaitingApproval([t], { plan_approved_at: '2026-10-22T13:00:00Z' })).toHaveLength(0)
  })

  it('is nothing until the project has loaded', () => {
    expect(awaitingApproval([task(1)], null)).toEqual([])
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
