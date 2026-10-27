// Mission is informational (KB decisions/human-work-ux.md §3, artifact #23):
// a Waiting-on-you strip into My work, Merge needed and "for you" readouts,
// outlined squares, and Waiting on people. It has no action buttons.
import { render, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { Mission } from './Mission'
import { AuthContext } from '../contexts/auth-context'
import { resetWaitCounts } from '../lib/waitCounts'
import type { MyWorkCounts, ProjectAccess, Task } from '../types'

const ME = 7
const at = '2026-10-27T10:00:00Z'
function task(id: number, over: Partial<Task> = {}): Task {
  return {
    id, project_id: 2, epic_id: 172, story_id: 413, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
    state: 'completed', priority: '1', dependency_ids: [], created_at: at, updated_at: at, planned_at: at, approved_at: at, ...over,
  }
}
const tasks: Task[] = [
  task(2470),
  task(2471, { owner_user_id: ME, merge_state: 'needed' }),
  task(2462, { owner_user_id: 8, merge_state: 'needed' }),
  task(2473, { owner_user_id: ME, state: 'blocked', block_reason: 'Which copy?' }),
  task(2477, { owner_user_id: 8, state: 'blocked', block_reason: 'Which API?' }),
  task(2481, { owner_user_id: ME, performer: 'human', state: 'backlog' }),
  task(2490, { owner_user_id: 9, performer: 'human', state: 'blocked', dependency_ids: [2481] }),
  task(2491, { owner_user_id: 8, state: 'in_progress', agent_id: 'amp:amp-worker-frontend', dispatched_at: at }),
]
let counts: MyWorkCounts = { counts: { waiting: 3, approve: 0, answer: 1, merge: 1, do: 1, coming_up: 0 }, projects: { 2: { waiting: 3, approve: 0, answer: 1, merge: 1, do: 1, coming_up: 0 } } }
let access: ProjectAccess = 'lead'
let inFlight = 0

const server = setupServer(
  http.get('*/api/projects/2/epics', () => HttpResponse.json({ epics: [{ id: 172, project_id: 2, name: 'Human work UX', description: '', priority: '1', created_at: at, updated_at: at }] })),
  http.get('*/api/epics/172/stories', () => HttpResponse.json({ stories: [{ id: 413, epic_id: 172, name: 'Mission', description: '', priority: '1', created_at: at, updated_at: at }] })),
  http.get('*/api/projects/2/tasks', () => HttpResponse.json({ tasks })),
  http.get('*/api/projects/2/members', () => HttpResponse.json({ my_access: access, members: [
    { user_id: ME, email: 'jacob@x.io', display_name: 'Jacob Sims', access: 'owner' },
    { user_id: 8, email: 'sam@x.io', display_name: 'Sam Ortiz', access: 'member' },
    { user_id: 9, email: 'priya@x.io', display_name: 'Priya Kumar', access: 'member' },
  ] })),
  http.get('*/api/projects/2/activity', () => HttpResponse.json({ activity: [], count: 0 })),
  http.get('*/api/audit', () => HttpResponse.json({ entries: [] })),
  http.get('*/api/my-work/counts', () => HttpResponse.json(counts)),
  http.get('*/api/projects/2', () => HttpResponse.json({ project: { id: 2, name: 'AMP v2', code: 'AMPV2', require_plan_approval: true } })),
  http.get('*/api/*', () => HttpResponse.json({})),
)
server.events.on('request:start', () => { inFlight++ })
server.events.on('request:end', () => { inFlight-- })
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
beforeEach(() => { resetWaitCounts(); access = 'lead' })
afterEach(async () => {
  await waitFor(() => expect(inFlight).toBe(0))
  server.resetHandlers()
  counts = { counts: { waiting: 3, approve: 0, answer: 1, merge: 1, do: 1, coming_up: 0 }, projects: { 2: { waiting: 3, approve: 0, answer: 1, merge: 1, do: 1, coming_up: 0 } } }
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

async function open() {
  // The strip reads the shared counts store, which AppShell fills; do it here.
  const { refreshWaitCounts } = await import('../lib/waitCounts')
  await refreshWaitCounts()
  const router = createMemoryRouter([{ path: '/project/:projectId/mission', element: <Mission /> }], { initialEntries: ['/project/2/mission'] })
  render(
    <AuthContext.Provider value={{ me: { user_id: ME, email: 'jacob@x.io' } as never, loading: false, error: null, isAdmin: false }}>
      <RouterProvider router={router} />
    </AuthContext.Provider>,
  )
  return await screen.findByText('Waiting on people')
}

describe('Mission (informational)', () => {
  it('shows the Waiting on you strip, linking to My work', async () => {
    await open()
    const strip = await screen.findByRole('link', { name: /^Waiting on you: 3\. Answer 1 · Merge 1 · Do 1\. Open My work$/ })
    expect(strip).toHaveAttribute('href', '/project/2/my-work')
  })

  it('shows the quiet strip when nothing waits on you', async () => {
    counts = { counts: { waiting: 0, approve: 0, answer: 0, merge: 0, do: 0, coming_up: 0 }, projects: {} }
    await open()
    expect(await screen.findByText('Nothing waits on you here')).toBeInTheDocument()
    expect(screen.getByText('Nothing waits on you here').closest('a')).toHaveAttribute('href', '/project/2/my-work')
  })

  it('readouts: Questions N · M for you, Merge needed N · M yours', async () => {
    await open()
    const q = screen.getByText('Questions').parentElement!
    expect(q).toHaveTextContent('2')
    expect(q).toHaveTextContent('1 for you')
    const m = screen.getByText('Merge needed').parentElement!
    expect(m).toHaveTextContent('2')
    expect(m).toHaveTextContent('1 yours')
  })

  it('outlines the squares that wait on you', async () => {
    await open()
    const epic = await screen.findByRole('region', { name: 'Epic Human work UX' })
    await waitFor(() => expect(epic.querySelectorAll('[data-for-you]')).toHaveLength(3))
    expect(within(epic).getByRole('button', { name: /Ticket 2471: .*waits on you/ })).toBeInTheDocument()
    expect(within(epic).getByRole('button', { name: /Ticket 2477: .*Question$/ })).toBeInTheDocument()
    expect(within(epic).getByText('3 for you')).toBeInTheDocument()
  })

  it('Waiting on people: you first, counts by kind, rows link into each My work', async () => {
    const panel = (await open()).closest('section')!
    const rows = within(panel).getAllByRole('link')
    expect(rows.map(r => r.getAttribute('aria-label'))).toEqual([
      'You: 3 waiting. Answer 1 (#2473) · Merge 1 (#2471) · Do 1 (#2481)',
      'Sam Ortiz: 2 waiting. Answer 1 (#2477) · Merge 1 (#2462)',
      'Priya Kumar: 0 waiting. 1 coming up',
    ])
    expect(rows[0]).toHaveAttribute('href', '/project/2/my-work?focus=2473')
    expect(rows[1]).toHaveAttribute('href', '/project/2/my-work?user=8&focus=2477')
    expect(rows[2]).toHaveAttribute('href', '/project/2/my-work?user=9&focus=2490')
  })

  it("for a member, others' rows open the ticket on the Board", async () => {
    access = 'member'
    const panel = (await open()).closest('section')!
    await waitFor(() => expect(within(panel).getAllByRole('link')[1]).toHaveAttribute('href', '/project/2/board?focus=2477'))
  })

  it('has no action buttons: no Answer, Merged, Approve, Start or Mark done', async () => {
    await open()
    await screen.findByRole('region', { name: 'Epic Human work UX' })
    const names = screen.getAllByRole('button').map(b => b.getAttribute('aria-label') || b.textContent || '')
    for (const n of names) expect(n).not.toMatch(/^(Answer|Merged|Mark merged|Approve|Start|Mark done|Send answer)/)
    expect(document.querySelector('textarea')).toBeNull()
    expect(screen.queryByText(/done, not merged/)).toBeNull()
  })
})
