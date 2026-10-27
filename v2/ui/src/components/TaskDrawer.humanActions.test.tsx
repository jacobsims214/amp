// The TaskDrawer uses HumanActions for Answer, Merge and Do, and never
// approves: approval is a link to My work (KB decisions/human-work-ux.md §2).
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { TaskDrawer } from './TaskDrawer'
import type { ProjectAccess, Task } from '../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 5, project_id: 2, epic_id: 1, story_id: 1, name: 'Board Mine filter', description: 'd', acceptance_criteria: 'a', state: 'backlog',
  priority: '1', dependency_ids: [], created_at: '2026-10-27T10:00:00Z', updated_at: '2026-10-27T10:00:00Z',
  planned_at: '2026-10-27T10:00:00Z', owner_user_id: 9, created_by: 9, ...over,
})
const posts: string[] = []
const server = setupServer(
  http.get('*/api/tasks/5/runs', () => HttpResponse.json({ runs: [{ id: 1, task_id: 5, project_id: 2, outcome: 'completed', pr_url: 'https://github.com/o/r/pull/88', branch: 'fix/mine', created_at: '2026-10-27T10:00:00Z' }], usage: { runs: 1 } })),
  http.get('*/api/tasks/5/history', () => HttpResponse.json({ history: [], count: 0 })),
  http.get('*/api/tasks/5/comments', () => HttpResponse.json({ comments: [] })),
  http.get('*/api/projects/2/artifacts', () => HttpResponse.json({ artifacts: [] })),
  http.get('*/api/*', () => HttpResponse.json({})),
  http.post('*/api/tasks/5/merge', ({ request }) => {
    posts.push(new URL(request.url).pathname)
    return HttpResponse.json({ task_id: 5, merge_state: 'merged', already_recorded: true, message: 'already merged, recorded by github at 2026-10-27T14:20:00Z' })
  }),
  http.post('*/api/tasks/5/unblock', ({ request }) => { posts.push(new URL(request.url).pathname); return HttpResponse.json({ task: task() }) }),
)
// The drawer's artifact list listens for live updates.
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
// The drawer keeps loading (runs, history, comments) after a synchronous test
// ends; let those requests finish before the server goes away, or under load
// a late fetch escapes to the network (ECONNREFUSED :3000, an unhandled error).
let inFlight = 0
server.events.on('request:start', () => { inFlight++ })
server.events.on('request:end', () => { inFlight-- })
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
afterEach(async () => { await waitFor(() => expect(inFlight).toBe(0)); server.resetHandlers(); posts.length = 0 })
afterAll(() => { server.close(); vi.unstubAllGlobals() })

const access = { project_id: 2, role: 'member', level: 1 } as unknown as ProjectAccess
function open(t: Task, onTaskUpdated = vi.fn()) {
  return render(<MemoryRouter><TaskDrawer task={t} onClose={() => {}} meId={9} myAccess={access} onTaskUpdated={onTaskUpdated} /></MemoryRouter>)
}

describe('TaskDrawer human actions', () => {
  it('has no approve button: an awaiting ticket links to My work', async () => {
    open(task())
    const link = await screen.findByRole('link', { name: /awaiting your approval/i })
    expect(link).toHaveAttribute('href', '/project/2/my-work?focus=5')
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()
    expect(screen.queryByText(/approve in flight plan/i)).toBeNull()
  })

  it('shows "Done · Merge needed" and records the merge, reporting an earlier record', async () => {
    open(task({ state: 'completed', completed_at: '2026-10-27T11:00:00Z', merge_state: 'needed', approved_at: '2026-10-27T10:30:00Z', approved_by: 9 }))
    expect(screen.getByTestId('merge-status')).toHaveTextContent('Done · Merge needed')
    expect(await screen.findByRole('link', { name: /review pr #88/i })).toHaveAttribute('href', 'https://github.com/o/r/pull/88')
    await userEvent.click(screen.getByRole('button', { name: /mark merged/i }))
    expect(await screen.findByRole('status')).toHaveTextContent('already merged, recorded by github')
    expect(posts).toEqual(['/api/tasks/5/merge'])
  })

  it('shows who recorded a merge', () => {
    open(task({ state: 'completed', completed_at: '2026-10-27T11:00:00Z', merge_state: 'merged', merge_source: 'github', approved_at: '2026-10-27T10:30:00Z', approved_by: 9 }))
    expect(screen.getByTestId('merge-status')).toHaveTextContent(/^Merged \(recorded by GitHub/)
    expect(screen.queryByRole('button', { name: /mark merged/i })).toBeNull()
  })

  it('answers a question with the same Answer action as My work', async () => {
    const onTaskUpdated = vi.fn()
    open(task({ state: 'blocked', block_reason: 'Avalara or Stripe Tax?', agent_id: 'amp-worker', approved_at: '2026-10-27T10:30:00Z', approved_by: 9 }), onTaskUpdated)
    const group = screen.getByRole('group', { name: 'Suggested answers' })
    await userEvent.click(within(group).getByRole('button', { name: 'Stripe Tax' }))
    await userEvent.click(screen.getByRole('button', { name: /send answer/i }))
    await waitFor(() => expect(onTaskUpdated).toHaveBeenCalled())
    expect(posts).toEqual(['/api/tasks/5/unblock'])
  })

  it('offers Start and Mark done… on your own human ticket', () => {
    open(task({ performer: 'human', approved_at: '2026-10-27T10:30:00Z', approved_by: 9 }))
    expect(screen.getByRole('button', { name: /^start$/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /mark done…/i })).toBeInTheDocument()
  })
})

describe('TaskDrawer merge gate exceptions (#2597)', () => {
  let runReads = 0
  beforeEach(() => { runReads = 0 })
  const runsAt = (branch: string) => http.get('*/api/tasks/5/runs', () => (runReads++, HttpResponse.json({ runs: [{ id: 1, task_id: 5, project_id: 2, outcome: 'completed', branch, pushed: true, created_at: '2026-10-27T10:00:00Z' }], usage: { runs: 1 } })))
  // The runs were read and rendered: a missing line is really missing.
  const settled = async () => {
    await waitFor(() => expect(runReads).toBeGreaterThan(0))
    await waitFor(() => expect(inFlight).toBe(0))
    await act(() => new Promise(r => setTimeout(r, 20)))
  }
  const inherited = (m: object) => http.get('*/api/metadata/inherited', () => HttpResponse.json({ inherited: m }))
  const doneTask = (over: Partial<Task> = {}) => task({ state: 'completed', completed_at: '2026-10-27T11:00:00Z', merge_state: 'none', approved_at: '2026-10-27T10:30:00Z', approved_by: 9, ...over })

  it('shows "Merged into <branch>" for a run pushed to the inherited integration branch, with no merge actions', async () => {
    server.use(inherited({ integration_branch: 'fix/merge-gate' }), runsAt('fix/merge-gate'))
    open(doneTask())
    expect(await screen.findByTestId('merge-integration')).toHaveTextContent('Merged into fix/merge-gate, waiting on the merge to main')
    expect(screen.queryByTestId('merge-status')).toBeNull()
    expect(screen.queryByText(/merge needed/i)).toBeNull()
    expect(screen.queryByRole('button', { name: /mark merged/i })).toBeNull()
  })

  it('uses the ticket\'s own integration_branch over the inherited one', async () => {
    server.use(inherited({ integration_branch: 'fix/other' }), runsAt('fix/merge-gate'))
    open(doneTask({ metadata: { integration_branch: 'fix/merge-gate' } }))
    expect(await screen.findByTestId('merge-integration')).toHaveTextContent('Merged into fix/merge-gate')
  })

  it('says nothing when the run went to another branch', async () => {
    server.use(inherited({ integration_branch: 'fix/merge-gate' }), runsAt('fix/unrelated'))
    open(doneTask())
    await settled()
    expect(screen.queryByTestId('merge-integration')).toBeNull()
  })

  it('shows Merged, not the integration line, once the merge to main is recorded', async () => {
    server.use(inherited({ integration_branch: 'fix/merge-gate' }), runsAt('fix/merge-gate'))
    open(doneTask({ merge_state: 'merged', merge_source: 'github', merged_at: '2026-10-27T14:20:00Z' }))
    expect(screen.getByTestId('merge-status')).toHaveTextContent(/^Merged/)
    await settled()
    expect(screen.queryByTestId('merge-integration')).toBeNull()
  })

  it('says a waits_for_merge: false ticket starts when its dependencies are done', async () => {
    server.use(inherited({ waits_for_merge: false }))
    open(task({ dependency_ids: [4], approved_at: '2026-10-27T10:30:00Z', approved_by: 9 }))
    expect(await screen.findByTestId('merge-no-wait')).toHaveTextContent("Starts when its dependencies are done; doesn't wait for their merge")
  })

  it('says nothing about merges for a dependent that waits as usual', async () => {
    server.use(inherited({ waits_for_merge: true }))
    open(task({ dependency_ids: [4], approved_at: '2026-10-27T10:30:00Z', approved_by: 9 }))
    await waitFor(() => expect(inFlight).toBe(0))
    await act(() => new Promise(r => setTimeout(r, 20)))
    expect(screen.queryByTestId('merge-no-wait')).toBeNull()
  })
})
