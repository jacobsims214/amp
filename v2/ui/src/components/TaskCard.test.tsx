import { cleanup, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { TaskCard } from './TaskCard'
import { mergeBadge, mergeWait } from '../lib/merge'
import type { Task } from '../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 2471, project_id: 3, epic_id: 1, story_id: 1, name: 'Build it', description: '', acceptance_criteria: '',
  state: 'completed', priority: '1', dependency_ids: [], created_at: '', updated_at: '', ...over,
})
const card = (t: Task, notStartedFlagMinutes?: number, requireApproval = true) =>
  render(<TaskCard task={t} onClick={() => {}} notStartedFlagMinutes={notStartedFlagMinutes} requireApproval={requireApproval} />)

describe('merge signal helpers', () => {
  it('badges only done tickets that need a merge or whose PR closed', () => {
    expect(mergeBadge(task({ merge_state: 'needed' }))?.label).toBe('Merge needed')
    expect(mergeBadge(task({ merge_state: 'abandoned' }))?.label).toBe('PR closed')
    expect(mergeBadge(task({ merge_state: 'merged' }))).toBeNull()
    expect(mergeBadge(task({ merge_state: 'none' }))).toBeNull()
    expect(mergeBadge(task({}))).toBeNull() // an older server sends no merge_state
    expect(mergeBadge(task({ state: 'in_progress', merge_state: 'needed' }))).toBeNull()
  })
  it('reads waiting_detail on open tickets only', () => {
    expect(mergeWait(task({ state: 'blocked', waiting_detail: ' Waiting on merge of #2471 (PR #88) ' }))).toBe('Waiting on merge of #2471 (PR #88)')
    expect(mergeWait(task({ state: 'blocked' }))).toBeNull()
    expect(mergeWait(task({ state: 'completed', waiting_detail: 'x' }))).toBeNull()
  })
})

describe('TaskCard', () => {
  it('shows "Merge needed" on a done ticket with an unmerged PR', () => {
    card(task({ merge_state: 'needed' }))
    expect(screen.getByText('Merge needed')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Done · Merge needed/ })).toBeInTheDocument()
  })
  it('shows no merge badge once merged', () => {
    card(task({ merge_state: 'merged' }))
    expect(screen.queryByText('Merge needed')).toBeNull()
  })
  it('a waiting card says which merge it waits on', () => {
    card(task({ id: 2480, state: 'blocked', blocked_by_ids: [2471], waiting_detail: 'Waiting on merge of #2471 (PR #88)' }))
    expect(screen.getByText('Waiting on merge of #2471 (PR #88)')).toBeInTheDocument()
    expect(screen.queryByText('Merge needed')).toBeNull()
  })
  it('a question card never shows waiting_detail', () => {
    card(task({ state: 'blocked', block_reason: 'Which API?', waiting_detail: 'Waiting on merge of #1 (PR #2)' }))
    expect(screen.queryByText(/Waiting on merge/)).toBeNull()
  })
})

// Dispatch integrity (AMP #2801): a dispatched ticket isn't started until its
// worker claims it, and the server's `phase` says which it is.
describe('TaskCard dispatch integrity', () => {
  const open = (over: Partial<Task> = {}) => task({ state: 'in_progress', started_at: null, phase: 'not_started', ...over })

  it('shows the amber "Dispatched · not started" badge on an unstarted ticket', () => {
    card(open({ agent_id: 'amp:amp-worker-frontend', dispatched_at: new Date().toISOString() }))
    const badge = screen.getByTestId('not-started')
    expect(badge).toHaveTextContent('Dispatched · not started')
    expect(badge).toHaveAttribute('title', expect.stringContaining('no worker has started it'))
  })

  it('shows nothing about dispatch on a started ticket', () => {
    card(task({ state: 'in_progress', phase: 'started', started_at: new Date().toISOString() }))
    expect(screen.queryByTestId('not-started')).toBeNull()
  })

  it('shows nothing when the server sends no phase at all (an older one)', () => {
    card(task({ state: 'in_progress', started_at: null }))
    expect(screen.queryByTestId('not-started')).toBeNull()
  })

  it('fills the badge once the project flag timeout is reached, and stays quiet at 0 (off)', () => {
    const t = open({ not_started_minutes: 40 })
    const { unmount } = card(t, 10)
    expect(screen.getByTestId('not-started')).toHaveAttribute('data-flagged', 'true')
    unmount()
    card(t, 0)
    expect(screen.getByTestId('not-started')).not.toHaveAttribute('data-flagged')
  })

  it('shows the returned tag and the reason on a ticket the watchdog sent back', () => {
    card(task({ state: 'backlog', returned_at: '2026-10-29T14:30:00Z', returned_reason: 'not started within 30 min of dispatch to amp:amp-worker-backend' }))
    expect(screen.getByTestId('returned')).toBeInTheDocument()
    expect(screen.getByText('Returned: not started within 30 min of dispatch to amp:amp-worker-backend')).toBeInTheDocument()
  })

  it('shows no returned tag once the ticket is dispatched again', () => {
    card(task({ state: 'in_progress', phase: 'not_started', returned_at: null }))
    expect(screen.queryByTestId('returned')).toBeNull()
  })
})

describe('TaskCard approval tag (#129)', () => {
  it('a ready ticket nobody approved says Needs approval', () => {
    card(task({ state: 'backlog', created_at: '2026-11-05T10:00:00Z' }))
    expect(screen.getByTestId('needs-approval')).toHaveTextContent('Needs approval')
  })
  it('not when the project does not require approval, nor on a ticket already in progress', () => {
    card(task({ state: 'backlog', created_at: '2026-11-05T10:00:00Z' }), undefined, false)
    expect(screen.queryByTestId('needs-approval')).toBeNull()
    cleanup()
    card(task({ state: 'in_progress', created_at: '2026-11-05T10:00:00Z' }))
    expect(screen.queryByTestId('needs-approval')).toBeNull()
  })
  it('an approved or done ticket does not', () => {
    card(task({ state: 'backlog', created_at: '2026-11-05T10:00:00Z', approved_at: '2026-11-05T10:01:00Z' }))
    expect(screen.queryByTestId('needs-approval')).toBeNull()
    cleanup()
    card(task({ state: 'completed' }))
    expect(screen.queryByTestId('needs-approval')).toBeNull()
  })
})

describe('TaskCard question options (#130)', () => {
  it('a question with options lists them, the recommended one marked', () => {
    card(task({ state: 'blocked', block_reason: 'HubSpot or Pipedrive?',
      question: { options: [{ label: 'HubSpot', consequence: '' }, { label: 'Pipedrive', consequence: '' }], recommended: 1 } }))
    const opts = screen.getByTestId('card-options')
    expect(opts).toHaveTextContent('HubSpotPipedrive (recommended)')
  })
  it('a question in words only lists none', () => {
    card(task({ state: 'blocked', block_reason: 'HubSpot or Pipedrive?' }))
    expect(screen.queryByTestId('card-options')).toBeNull()
  })
})

describe('TaskCard question text (#133)', () => {
  it('shows a markdown question as plain words, no asterisks or brackets', () => {
    card(task({ state: 'blocked', block_reason: '**Pick** [one](https://x)' }))
    expect(screen.getByText('Pick one')).toBeInTheDocument()
    expect(screen.queryByText(/\*\*|\[one\]/)).toBeNull()
  })
})
