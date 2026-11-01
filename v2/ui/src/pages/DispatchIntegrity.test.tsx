// Dispatch integrity in My work (AMP #2801, epic 180, story 444).
//
// A ticket of yours dispatched to an agent that no worker started shows as a
// "start" item: the server decided it (waiting_on_people kind "start", with
// agent_id and not_started_minutes), and My work only renders it and offers
// the action — return it to ready, or keep waiting for the watchdog.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { ProjectMyWork } from './ProjectMyWork'
import { resetWaitCounts } from '../lib/waitCounts'
import { myWork, startItem } from '../test/myWorkFixture'
import type { MyWorkCounts, ProjectMyWork as Data } from '../types'

const START_COUNTS = { waiting: 1, approve: 0, answer: 0, merge: 0, start: 1, do: 0, coming_up: 0 }
const counts: MyWorkCounts = { counts: START_COUNTS, projects: { 2: START_COUNTS } }
const posts: string[] = []
const returned: string[] = []
let data: Data = myWork('busy', {
  counts: START_COUNTS,
  items: { approve: [], answer: [], merge: [], start: [startItem()], do: [] },
  coming_up: [],
  done_today: [],
})

const server = setupServer(
  http.get('*/api/projects/2/my-work', () => HttpResponse.json(data)),
  http.get('*/api/my-work/counts', () => HttpResponse.json(counts)),
  http.get('*/api/projects/2/members', () => HttpResponse.json({ members: [], my_access: 'member' })),
  http.get('*/api/projects/:id', () => HttpResponse.json({ project: { id: 2, name: 'AMP v2', code: 'AMPV2' } })),
  http.post('*/api/tasks/:id/return', async ({ request }) => {
    posts.push(new URL(request.url).pathname)
    returned.push(((await request.json()) as { reason: string }).reason)
    return HttpResponse.json({ task: startItem().task })
  }),
)
server.events.on('request:start', () => {})
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
beforeEach(() => { resetWaitCounts() })
afterEach(() => { server.resetHandlers(); posts.length = 0; returned.length = 0 })
afterAll(() => server.close())

function at(url: string) {
  const router = createMemoryRouter([
    { path: '/project/:projectId/my-work', element: <ProjectMyWork /> },
  ], { initialEntries: [url] })
  render(<RouterProvider router={router} />)
  return router
}

describe('My work: a dispatched ticket no worker started', () => {
  it('counts it as its own kind, in the readouts and the tab', async () => {
    at('/project/2/my-work')
    const ro = await screen.findByRole('group', { name: 'Waiting on you in AMP v2' })
    // Start is its own readout, counted in Waiting on you like the other kinds.
    expect(within(ro).getByText('Start').closest('.ro')).toHaveTextContent('1')
    expect(within(ro).getByText('Waiting on you').nextSibling).toHaveTextContent('1')
    expect(within(ro).getByText('Oldest: 30m, #2803, not started')).toBeInTheDocument()
  })

  it('says who it went to and how long, from the server\'s minutes', async () => {
    at('/project/2/my-work')
    const item = await screen.findByRole('article', { name: /Start: #2803/ })
    expect(item).toHaveTextContent('Dispatched to amp:amp-worker-frontend 32 min ago; no worker has started it.')
    expect(within(item).getByText('Not started')).toBeInTheDocument()
  })

  it('offers Return to ready, which clears the dispatch, and says the watchdog is the alternative', async () => {
    at('/project/2/my-work')
    const item = await screen.findByRole('article', { name: /Start: #2803/ })
    expect(within(item).getByText(/AMP returns it by itself if no worker starts it/)).toBeInTheDocument()
    await userEvent.click(within(item).getByRole('button', { name: /return to ready/i }))
    await waitFor(() => expect(posts).toEqual(['/api/tasks/2803/return']))
    expect(returned).toEqual(['no worker started it'])
  })

  it('is read-only for a lead reading someone else\'s list', async () => {
    data = { ...myWork('busy', { counts: START_COUNTS, items: { approve: [], answer: [], merge: [], start: [startItem()], do: [] }, coming_up: [], done_today: [] }), read_only: true, user_id: 7 }
    at('/project/2/my-work?user=7')
    const item = await screen.findByRole('article', { name: /Start: #2803/ })
    expect(within(item).queryByRole('button', { name: /return to ready/i })).toBeNull()
    expect(within(item).getByText(/only its owner can return it to ready/i)).toBeInTheDocument()
  })

  it('an older server with no start kind renders the rest, and no Start readout count', async () => {
    data = myWork('busy', { counts: { ...START_COUNTS, waiting: 1, start: 0, answer: 1 }, items: { approve: [], answer: [], merge: [], start: [], do: [] }, coming_up: [], done_today: [] })
    at('/project/2/my-work')
    const ro = await screen.findByRole('group', { name: 'Waiting on you in AMP v2' })
    expect(within(ro).getByText('Start').closest('.ro')).toHaveTextContent('0')
  })
})

describe('My work: no browser-side recomputation', () => {
  it('takes the item\'s minutes from the server, not the ticket\'s timestamps', async () => {
    const it0 = startItem()
    // The server says 32 minutes; the timestamps would say 30.
    data = myWork('busy', { counts: START_COUNTS, items: { approve: [], answer: [], merge: [], start: [{ ...it0, not_started_minutes: 999 }], do: [] }, coming_up: [], done_today: [] })
    at('/project/2/my-work')
    const item = await screen.findByRole('article', { name: /Start: #2803/ })
    expect(item).toHaveTextContent('999 min ago')
  })
})
