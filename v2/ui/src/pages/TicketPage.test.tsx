// The full-page ticket view (#157): the drawer's sections in a wide column,
// reached from the drawer's "Open full page" and from /t/<ref>?view=page.
import { act, render, screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { TicketPage } from './TicketPage'
import { TaskDrawer } from '../components/TaskDrawer'
import { ticketLanding } from '../lib/ticketRef'
import type { Task } from '../types'

const at = '2026-10-27T10:00:00Z'
const task = (over: Partial<Task> = {}): Task => ({
  id: 34, project_id: 2, epic_id: 1, story_id: 3, name: 'Confirm targets', description: 'The **brief**', acceptance_criteria: 'Signed off',
  state: 'in_progress', priority: '1', dependency_ids: [], created_at: at, updated_at: at, planned_at: at, approved_at: at, ...over,
})
let status = 200
const server = setupServer(
  http.get('*/api/tasks/34', () => status === 200 ? HttpResponse.json({ task: task() }) : HttpResponse.json({ error: 'nope' }, { status })),
  http.get('*/api/tasks/35', () => HttpResponse.json({ task: task({ id: 35, project_id: 3, name: 'Second ticket' }) })),
  http.get('*/api/tasks/:id/history', () => HttpResponse.json({ history: [], count: 0 })),
  http.get('*/api/tasks/:id/comments', () => HttpResponse.json({ comments: [] })),
  http.get('*/api/tasks/:id/runs', () => HttpResponse.json({ runs: [], usage: { runs: 0 } })),
  http.get('*/api/projects/:p/members', () => HttpResponse.json({ my_access: 'member', members: [] })),
  http.get('*/api/projects/:p/tasks', () => HttpResponse.json({ tasks: [] })),
  http.get('*/api/projects/:p/epics', () => HttpResponse.json({ epics: [] })),
  http.get('*/api/projects/:p/artifacts', () => HttpResponse.json({ artifacts: [] })),
  http.get('*/api/projects/:p', () => HttpResponse.json({ project: { id: 2, name: 'Scorecard', code: 'ab' } })),
  http.get('*/api/*', () => HttpResponse.json({})),
)
let sources: FakeEventSource[] = []
class FakeEventSource { constructor() { sources.push(this) } onmessage: ((e: { data: string }) => void) | null = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen() })
afterEach(() => { cleanup(); sources = []; server.resetHandlers(); status = 200 })
afterAll(() => server.close())
import { cleanup } from '@testing-library/react'

function Where() { const l = useLocation(); return <output data-testid="where">{l.pathname + l.search}</output> }
function renderAt(path: string) {
  const router = createMemoryRouter([
    { path: '/project/:projectId/ticket/:ticketId', element: <TicketPage /> },
    { path: '*', element: <Where /> },
  ], { initialEntries: [path] })
  render(<RouterProvider router={router} />)
  return router
}

describe('full-page ticket', () => {
  it('renders the ticket with the drawer sections and a way back to the board', async () => {
    renderAt('/project/2/ticket/34')
    expect(await screen.findByRole('heading', { name: 'Confirm targets' })).toBeInTheDocument()
    expect(screen.getByTestId('ticket-page')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'history' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Back to board/ })).toHaveAttribute('href', '/project/2/board?focus=34')
    expect(screen.queryByRole('button', { name: 'Close' })).toBeNull()
  })

  it('says a missing ticket was not found, and a forbidden one is no access', async () => {
    status = 404
    renderAt('/project/2/ticket/34')
    expect(await screen.findByRole('heading', { name: /Ticket #34 was not found, or you can't see it/ })).toBeInTheDocument()
    cleanup()
    status = 403
    renderAt('/project/2/ticket/34')
    expect(await screen.findByRole('heading', { name: /don't have access to ticket #34/ })).toBeInTheDocument()
  })

  it('moves a ticket under the wrong project to its own project URL', async () => {
    const router = renderAt('/project/2/ticket/35?tab=details')
    expect(await screen.findByTestId('ticket-page')).toBeInTheDocument()
    expect(router.state.location.pathname + router.state.location.search).toBe('/project/3/ticket/35?tab=details')
  })
})

describe('live ticket page', () => {
  it('re-reads when this ticket changes elsewhere', async () => {
    renderAt('/project/2/ticket/34')
    await screen.findByRole('heading', { name: 'Confirm targets' })
    server.use(http.get('*/api/tasks/34', () => HttpResponse.json({ task: task({ name: 'Renamed elsewhere' }) })))
    const src = sources[sources.length - 1]
    act(() => { src.onmessage?.({ data: JSON.stringify({ type: 'task.updated', payload: task() }) }) })
    expect(await screen.findByRole('heading', { name: 'Renamed elsewhere' })).toBeInTheDocument()
  })

  it('a slow read for the previous ticket does not overwrite the one navigated to', async () => {
    let release: () => void = () => {}
    const gate = new Promise<void>(r => { release = r })
    server.use(http.get('*/api/tasks/34', async () => { await gate; return HttpResponse.json({ task: task() }) }))
    const router = renderAt('/project/2/ticket/34')
    await act(async () => { await router.navigate('/project/3/ticket/35') })
    expect(await screen.findByRole('heading', { name: 'Second ticket' })).toBeInTheDocument()
    await act(async () => { release(); await new Promise(r => setTimeout(r, 20)) })
    expect(router.state.location.pathname).toBe('/project/3/ticket/35')
    expect(screen.getByRole('heading', { name: 'Second ticket' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Confirm targets' })).toBeNull()
  })

  it('a 500 or a dropped connection is a load failure with a retry, not "not found"', async () => {
    status = 500
    renderAt('/project/2/ticket/34')
    expect(await screen.findByText(/didn't load/)).toBeInTheDocument()
    expect(screen.queryByText(/was not found/)).toBeNull()
    status = 200
    await act(async () => { screen.getByRole('button', { name: 'Try again' }).click() })
    expect(await screen.findByRole('heading', { name: 'Confirm targets' })).toBeInTheDocument()
  })

  it('a 401 says the session may have expired', async () => {
    status = 401
    renderAt('/project/2/ticket/34')
    expect(await screen.findByText(/session may have expired/)).toBeInTheDocument()
  })

  it('a network failure is a load failure too', async () => {
    server.use(http.get('*/api/tasks/34', () => HttpResponse.error()))
    renderAt('/project/2/ticket/34')
    expect(await screen.findByText(/didn't load/)).toBeInTheDocument()
  })
})

describe('drawer to full page', () => {
  it('the drawer has an Open full page link that keeps its tab', async () => {
    const router = createMemoryRouter([{ path: '/project/:projectId/board', element: <TaskDrawer task={task()} onClose={() => {}} /> }],
      { initialEntries: ['/project/2/board?focus=34&tab=details'] })
    render(<RouterProvider router={router} />)
    expect(await screen.findByRole('link', { name: 'Open full page' })).toHaveAttribute('href', '/project/2/ticket/34?tab=details')
  })

  it('/t/<ref>?view=page lands on the full page; without it, on the board drawer', () => {
    expect(ticketLanding(2, 34, '?view=page&tab=details')).toBe('/project/2/ticket/34?tab=details')
    expect(ticketLanding(2, 34, '?tab=details')).toBe('/project/2/board?focus=34&tab=details')
  })
})
