// HumanActions (KB decisions/human-work-ux.md §2): each action and the API call it makes.
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { HumanActions } from './HumanActions'
import type { Task } from '../../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 5, project_id: 2, epic_id: 1, story_id: 1, name: 'T', description: '', acceptance_criteria: '', state: 'backlog',
  priority: '1', dependency_ids: [], created_at: '2026-10-27T10:00:00Z', updated_at: '2026-10-27T10:00:00Z', owner_user_id: 9, ...over,
})

const calls: { method: string; path: string; body: unknown }[] = []
const record = async (request: Request) => {
  const text = await request.text()
  calls.push({ method: request.method, path: new URL(request.url).pathname, body: text ? JSON.parse(text) : undefined })
}
let mergeReply: Record<string, unknown> = {}
const server = setupServer(
  http.post('*/api/tasks/:id/unblock', async ({ request }) => { await record(request); return HttpResponse.json({ task: task({ state: 'backlog' }) }) }),
  http.post('*/api/tasks/:id/merge', async ({ request }) => { await record(request); return HttpResponse.json(mergeReply) }),
  http.post('*/api/tasks/:id/merge/not-needed', async ({ request }) => {
    await record(request)
    return HttpResponse.json({ task_id: 5, merge_state: 'none', already_recorded: false, message: 'not needed', task: task({ state: 'completed', merge_state: 'none' }) })
  }),
  http.post('*/api/tasks/:id/reopen', async ({ request }) => { await record(request); return HttpResponse.json({ task: task({ state: 'backlog' }) }) }),
  http.post('*/api/tasks/:id/start', async ({ request }) => { await record(request); return HttpResponse.json({ task: task({ state: 'in_progress', performer: 'human' }) }) }),
  http.post('*/api/tasks/:id/complete', async ({ request }) => { await record(request); return HttpResponse.json({ task: task({ state: 'completed', performer: 'human' }) }) }),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => { server.resetHandlers(); calls.length = 0 })
afterAll(() => server.close())

describe('Answer', () => {
  const q = task({ state: 'blocked', block_reason: 'Should I fall back to a file, or fail with a clear error?', agent_id: 'amp-worker-backend' })

  it('quotes the question, fills the box from a suggested answer, and sends it', async () => {
    const onChanged = vi.fn()
    render(<HumanActions task={q} canAct onChanged={onChanged} />)
    expect(screen.getByText('amp-worker-backend asked')).toBeInTheDocument()
    expect(screen.getByText(q.block_reason!)).toBeInTheDocument()
    const send = screen.getByRole('button', { name: /send answer/i })
    expect(send).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: 'Fail with a clear error' }))
    expect(screen.getByLabelText('Your answer')).toHaveValue('Fail with a clear error')
    await userEvent.click(send)
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(calls).toEqual([{ method: 'POST', path: '/api/tasks/5/unblock', body: { answer: 'Fail with a clear error' } }])
    expect(screen.getByLabelText('Your answer')).toHaveValue('')
  })

  it('is read-only for someone who can\'t work the ticket', () => {
    render(<HumanActions task={q} canAct={false} />)
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.getByText(/only people who can work this ticket/i)).toBeInTheDocument()
  })
})

describe('Merge', () => {
  const done = task({ state: 'completed', merge_state: 'needed' })

  it('offers the PR and records Mark merged', async () => {
    mergeReply = { task_id: 5, merge_state: 'merged', merge_source: 'ui', already_recorded: false, message: 'merged', task: task({ state: 'completed', merge_state: 'merged' }) }
    const onChanged = vi.fn()
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="fix/x" onChanged={onChanged} />)
    expect(screen.getByRole('link', { name: /review pr #88/i })).toHaveAttribute('href', 'https://github.com/o/r/pull/88')
    await userEvent.click(screen.getByRole('button', { name: /mark merged/i }))
    expect(await screen.findByText(/Merged\. What waited on it can start\./)).toBeInTheDocument()
    expect(calls).toEqual([{ method: 'POST', path: '/api/tasks/5/merge', body: undefined }])
    expect(onChanged).toHaveBeenCalledWith(expect.objectContaining({ merge_state: 'merged' }), undefined)
    expect(screen.queryByRole('button', { name: /mark merged/i })).toBeNull()
  })

  it('is idempotent: a merge already recorded says who recorded it first', async () => {
    mergeReply = { task_id: 5, merge_state: 'merged', merge_source: 'github', merged_at: '2026-10-27T14:20:00Z', already_recorded: true,
      message: 'already merged, recorded by github at 2026-10-27T14:20:00Z' }
    render(<HumanActions task={done} canAct />)
    await userEvent.click(screen.getByRole('button', { name: /mark merged/i }))
    expect(await screen.findByRole('status')).toHaveTextContent('Already recorded: already merged, recorded by github at 2026-10-27T14:20:00Z.')
  })

  it('marks a merge not needed', async () => {
    render(<HumanActions task={done} canAct />)
    await userEvent.click(screen.getByRole('button', { name: /not needed/i }))
    expect(await screen.findByText(/Marked not needed/)).toBeInTheDocument()
    expect(calls.map(c => c.path)).toEqual(['/api/tasks/5/merge/not-needed'])
  })

  it('offers Reopen and Not needed when the PR was closed without merging', async () => {
    const onChanged = vi.fn()
    render(<HumanActions task={task({ state: 'completed', merge_state: 'abandoned' })} canAct onChanged={onChanged} />)
    expect(screen.getByText(/PR closed without merging/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /mark merged/i })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: /reopen ticket/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalledWith(expect.objectContaining({ state: 'backlog' }), undefined))
    expect(calls.map(c => c.path)).toEqual(['/api/tasks/5/reopen'])
  })

  it('shows only the PR link to someone who can\'t act', () => {
    render(<HumanActions task={done} canAct={false} prUrl="https://github.com/o/r/pull/88" />)
    expect(screen.getAllByRole('link')).toHaveLength(1)
    expect(screen.queryByRole('button')).toBeNull()
  })

  // AMP #3013: one truthful card per PR. The card names the route and who waits,
  // offers exactly one merge action, and says what recording it did to the plan.
  it('renders the route and only the real waiters, with one Mark merged', async () => {
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="feat/x"
      baseBranch="main" targetSource="run" holdingUp={[2490, 2491]} />)
    expect(screen.getByTestId('merge-route')).toHaveTextContent('PR #88 · feat/x → main')
    expect(screen.getByTestId('merge-waiters')).toHaveTextContent('2 tickets wait on this merge: #2490, #2491.')
    expect(screen.getByText(/Why you're asked/)).toBeInTheDocument()
    expect(screen.getByText(/reported main as the target/)).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: /mark merged/i })).toHaveLength(1)
    expect(screen.getByRole('button', { name: /mark merged/i })).toHaveClass('amp-btn-primary')
    // The guard keeps the label honest: it records a merge, it doesn't make one.
    expect(screen.getByText(/only after you merged it on GitHub/)).toBeInTheDocument()
  })

  it('an unknown target is shown as unknown, with a warning chip — never as main', () => {
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="feat/x" targetSource="unknown" />)
    expect(screen.getByTestId('merge-route')).toHaveTextContent('PR #88 · feat/x · target not verified')
    expect(screen.queryByText(/→ main/)).toBeNull()
    expect(screen.getByText(/AMP reads no repository settings/)).toBeInTheDocument()
    expect(screen.getByText(/shown rather than hidden/)).toBeInTheDocument()
  })

  it('nothing waits: Mark merged stays, demoted, and the card says why it is quiet', () => {
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="feat/x"
      baseBranch="main" targetSource="run" holdingUp={[]} notWaiting={[2492]} />)
    const btn = screen.getByRole('button', { name: /mark merged/i })
    expect(btn).not.toHaveClass('amp-btn-primary')
    expect(screen.getByTestId('merge-waiters')).toHaveTextContent('Nothing waits on this merge — #2492 opted out')
    expect(screen.getByTestId('merge-waiters')).toHaveTextContent('Recording it here is only for a merge you already made')
  })

  it('a pre-contract server: no waiter claim is invented, and the action stays ordinary', () => {
    // No WaitItem data at all (the TaskDrawer's case, and a pre-#3011 server):
    // the card must not say "nothing waits" — it says nothing about waiters.
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="fix/x" />)
    expect(screen.getByTestId('merge-route')).toHaveTextContent('PR #88 · fix/x · target not verified')
    expect(screen.queryByTestId('merge-waiters')).toBeNull()
    expect(screen.getByRole('button', { name: /mark merged/i })).toHaveClass('amp-btn-primary')
  })

  it('linked and gated: no Mark merged, and the missing prerequisites are named', () => {
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="feat/x"
      baseBranch="main" targetSource="run" holdingUp={[2490]}
      trackedBy={{ task_id: 9, name: 'Merge PR', state: 'backlog', ready: false, waiting_on: [3, 4], reason: 'its merge step still waits on #3, #4' }} />)
    // Absent, not disabled: the backend refuses a merge report on a gated step.
    expect(screen.queryByRole('button', { name: /mark merged/i })).toBeNull()
    expect(screen.getByTestId('merge-step')).toHaveTextContent('Tracked by #9 Merge PR')
    expect(screen.getByTestId('merge-step')).toHaveTextContent('#3 still open')
    expect(screen.getByTestId('merge-step')).toHaveTextContent('#4 still open')
    expect(screen.getByText(/No Mark merged while its merge step/)).toHaveTextContent(/still waits on #3, #4/)
  })

  it('linked and ready: Mark merged records the merge and completes the step in one act', () => {
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="feat/x"
      baseBranch="main" targetSource="run" holdingUp={[2490]}
      trackedBy={{ task_id: 9, name: 'Merge PR', state: 'backlog', ready: true }} />)
    expect(screen.getByTestId('merge-step')).toHaveTextContent('Mark merged records the merge and completes #9 in one step')
    expect(screen.getByRole('button', { name: /mark merged/i })).toBeInTheDocument()
  })

  it('reports what the merge did to the linked step, so the person never marks it twice', async () => {
    mergeReply = { task_id: 5, merge_state: 'merged', already_recorded: false, message: 'merged',
      linked_step: { task_id: 9, state: 'completed', completed: true, already_done: false, message: 'completed by the merge' },
      task: task({ state: 'completed', merge_state: 'merged' }) }
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" branch="feat/x"
      baseBranch="main" targetSource="run" trackedBy={{ task_id: 9, name: 'Merge PR', state: 'backlog', ready: true }} />)
    await userEvent.click(screen.getByRole('button', { name: /mark merged/i }))
    const status = await screen.findAllByRole('status')
    expect(status.map(n => n.textContent).join(' ')).toContain('Its merge step #9 was completed by this record: nothing left to mark done.')
    expect(calls.map(c => c.path)).toEqual(['/api/tasks/5/merge'])
  })

  it('leaves a started linked step open and says why, rather than closing someone\'s work', async () => {
    mergeReply = { task_id: 5, merge_state: 'merged', already_recorded: false, message: 'merged',
      linked_step: { task_id: 9, state: 'in_progress', completed: false, already_done: false, message: "its owner has started it" },
      task: task({ state: 'completed', merge_state: 'merged' }) }
    render(<HumanActions task={done} canAct prUrl="https://github.com/o/r/pull/88" baseBranch="main" targetSource="run"
      trackedBy={{ task_id: 9, name: 'Merge PR', state: 'in_progress', ready: true }} />)
    await userEvent.click(screen.getByRole('button', { name: /mark merged/i }))
    const status = await screen.findAllByRole('status')
    expect(status.map(n => n.textContent).join(' ')).toContain('Its merge step #9 stays open: its owner has started it.')
  })
})

describe('Do', () => {
  it('starts a ready human ticket', async () => {
    const onChanged = vi.fn()
    render(<HumanActions task={task({ performer: 'human' })} canAct onChanged={onChanged} />)
    await userEvent.click(screen.getByRole('button', { name: /^start$/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalledWith(expect.objectContaining({ state: 'in_progress' }), undefined))
    expect(calls.map(c => c.path)).toEqual(['/api/tasks/5/start'])
  })

  it('marks it done with a note, a link and hours', async () => {
    render(<HumanActions task={task({ performer: 'human', state: 'in_progress' })} canAct />)
    expect(screen.queryByRole('button', { name: /^start$/i })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: /mark done…/i }))
    await userEvent.type(screen.getByLabelText(/what happened/i), 'Rotated')
    await userEvent.type(screen.getByLabelText(/link/i), 'https://example.com/pr/1')
    await userEvent.type(screen.getByLabelText(/hours spent/i), '1.5')
    await userEvent.click(screen.getByRole('button', { name: /^mark done$/i }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toEqual({ method: 'POST', path: '/api/tasks/5/complete', body: { note: 'Rotated', pr_url: 'https://example.com/pr/1', hours_worked: 1.5 } })
  })

  it('refuses bad hours', async () => {
    render(<HumanActions task={task({ performer: 'human', state: 'in_progress' })} canAct />)
    await userEvent.click(screen.getByRole('button', { name: /mark done…/i }))
    await userEvent.type(screen.getByLabelText(/hours spent/i), 'lots')
    expect(screen.getByRole('alert')).toHaveTextContent('Hours must be a number from 0 to 1000.')
    expect(screen.getByRole('button', { name: /^mark done$/i })).toBeDisabled()
  })

  it('says what a waiting ticket waits on, with no buttons', () => {
    render(<HumanActions task={task({ performer: 'human', state: 'blocked', dependency_ids: [3, 4] })} canAct />)
    expect(screen.getByText('Waiting on #3, #4 before it can start.')).toBeInTheDocument()
    expect(screen.queryByRole('button')).toBeNull()
  })
})
