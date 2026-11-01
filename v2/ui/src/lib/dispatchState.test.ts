// Dispatch integrity in the UI (AMP #2801, epic 180, story 444): "Dispatched ·
// not started", "Returned", and the words My work and the drawer use.
//
// The rule these tests protect: the browser never decides any of it. `phase`,
// the flag, and every "N min" are the server's; a server that sends no phase
// (an older one) gets no badge, and a ticket that already started never looks
// unstarted however its timestamps read.
import { describe, expect, it } from 'vitest'
import {
  isNotStarted, notStartedBadge, notStartedHint, returnedHint, returnedLine,
  sinceMinutes, startItemText, wasReturned,
} from './dispatchState'
import type { Task } from '../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 2803, project_id: 2, epic_id: 180, story_id: 444, name: 'UI: not started', description: '', acceptance_criteria: '',
  state: 'in_progress', priority: '1', dependency_ids: [], created_at: '', updated_at: '', ...over,
})

describe('isNotStarted: phase is the server\'s, never recomputed', () => {
  it('is true only for in_progress + phase not_started', () => {
    expect(isNotStarted(task({ phase: 'not_started', started_at: null }))).toBe(true)
    expect(isNotStarted(task({ phase: 'started', started_at: '2026-10-29T14:00:00Z' }))).toBe(false)
  })

  it('does not read started_at at all: a server that sends no phase gets no badge', () => {
    expect(isNotStarted(task({ started_at: null }))).toBe(false)      // older server: no phase
    expect(isNotStarted(task({ started_at: null, phase: undefined }))).toBe(false)
  })

  it('is false on every other state even when started_at is present', () => {
    for (const state of ['blocked', 'completed', 'backlog', 'scheduled'] as const) {
      expect(isNotStarted(task({ state, started_at: '2026-10-29T14:00:00Z' }))).toBe(false)
      expect(isNotStarted(task({ state, started_at: null, phase: undefined }))).toBe(false)
    }
  })
})

describe('notStartedBadge: quiet, flagged, and the project that turned the flag off', () => {
  const dispatched = (over: Partial<Task> = {}) => task({
    phase: 'not_started', started_at: null, not_started_minutes: 4, agent_id: 'amp:amp-worker-frontend', ...over,
  })

  it('shows the badge, quiet, before the flag timeout', () => {
    const b = notStartedBadge(dispatched(), { flagMinutes: 10 })
    expect(b).toEqual({ label: 'Dispatched · not started', flagged: false, minutes: 4 })
  })

  it('fills in once the flag timeout is reached', () => {
    const b = notStartedBadge(dispatched({ not_started_minutes: 12 }), { flagMinutes: 10 })
    expect(b?.flagged).toBe(true)
    expect(b?.minutes).toBe(12)
  })

  it("takes the item's minutes when My work passes its own", () => {
    expect(notStartedBadge(dispatched(), { minutes: 42 })?.minutes).toBe(42)
  })

  it('keeps the badge, quiet, when the project set the flag to 0 (off)', () => {
    const b = notStartedBadge(dispatched({ not_started_minutes: 600 }), { flagMinutes: 0 })
    expect(b?.flagged).toBe(false)
    expect(b?.label).toBe('Dispatched · not started')
  })

  it('shows the badge but never fills it when the server sent no minutes', () => {
    const b = notStartedBadge(dispatched({ not_started_minutes: undefined }), { flagMinutes: 1 })
    expect(b?.minutes).toBeUndefined()
    expect(b?.flagged).toBe(false)
  })

  it('is null for a started ticket, a done one, and any non-in_progress state', () => {
    expect(notStartedBadge(task({ phase: 'started' }), { minutes: 12 })).toBeNull()
    expect(notStartedBadge(task({ state: 'completed' }), { minutes: 12 })).toBeNull()
    expect(notStartedBadge(task({ state: 'blocked', phase: undefined }), { minutes: 12 })).toBeNull()
  })
})

describe('returned: the badge, the line, and a re-dispatched ticket', () => {
  const returned = { returned_at: '2026-10-29T14:30:00Z', returned_reason: 'not started within 30 min of dispatch to amp:amp-worker-backend' }

  it('has a line with the server\'s reason, and is a returned ticket', () => {
    expect(wasReturned(returned)).toBe(true)
    expect(returnedLine(returned)).toBe('Returned: not started within 30 min of dispatch to amp:amp-worker-backend')
  })

  it('says nothing once it is dispatched again (the server clears both)', () => {
    expect(wasReturned({ returned_at: null })).toBe(false)
    expect(returnedLine({ returned_at: null, returned_reason: '' })).toBeNull()
    expect(returnedLine({ returned_at: undefined })).toBeNull()
  })

  it('falls back to a plain line when the server sent no reason', () => {
    expect(returnedLine({ returned_at: '2026-10-29T14:30:00Z', returned_reason: '  ' })).toBe('Returned to ready')
  })

  it('explains both what happened and what to do', () => {
    expect(returnedHint(returned.returned_reason)).toContain('Returned to ready: not started within 30 min')
    expect(returnedHint(returned.returned_reason)).toContain('Dispatch it again')
    expect(returnedHint('')).toContain('no worker started it')
  })
})

describe('the wording My work and the drawer use', () => {
  it('says who it went to and for how long, from the server\'s fields', () => {
    expect(startItemText({ agent_id: 'amp:amp-worker-backend', not_started_minutes: 12 }))
      .toBe('Dispatched to amp:amp-worker-backend 12 min ago; no worker has started it.')
    expect(startItemText({ not_started_minutes: 3 })).toBe('Dispatched 3 min ago; no worker has started it.')
    expect(startItemText({ agent_id: 'a' })).toBe('Dispatched to a; no worker has started it.')
  })

  it('shows "since N min" only from a server number', () => {
    expect(sinceMinutes(12)).toBe('since 12 min')
    expect(sinceMinutes(undefined)).toBe('')
  })

  it('names the agent, that nothing started it, and what happens next', () => {
    const hint = notStartedHint({ agent_id: 'amp:amp-worker-frontend' })
    expect(hint).toContain('Dispatched to amp:amp-worker-frontend')
    expect(hint).toMatch(/no worker has started it/)
    expect(hint).toMatch(/returns it to ready by itself/)
    expect(notStartedHint({ assigned_to: 'amp-worker' })).toContain('Dispatched to amp-worker')
    expect(notStartedHint({})).toContain('Dispatched; no worker')
  })
})
