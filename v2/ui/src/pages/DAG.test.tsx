// The DAG's "What changed" overlay reloads the project's activity feed as
// tickets change. Every live event changes the tickets, so those reloads
// share one load per 5 s, and a hidden tab makes none until it is shown
// (AMP #2511).
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { DAGView } from './DAG'
import { AuthContext } from '../contexts/auth-context'
import { applyTask } from '../lib/boardStore'
import type { ActivityLog, Task } from '../types'

// jsdom can't lay out or measure the graph, and these tests are about loads,
// not drawing, so the canvas renders nothing. The rest of React Flow is real.
vi.mock('@xyflow/react', async importOriginal => ({
  ...await importOriginal<typeof import('@xyflow/react')>(),
  ReactFlow: () => null,
}))

const at = '2026-11-18T10:00:00Z'
function task(id: number, over: Partial<Task> = {}): Task {
  return {
    id, project_id: 2, epic_id: 172, story_id: 413, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
    state: 'backlog', priority: '1', dependency_ids: [], created_at: at, updated_at: at, planned_at: at, approved_at: at, ...over,
  }
}
const tasks = [task(2470), task(2471, { state: 'in_progress', dependency_ids: [2470] })]
const activity: ActivityLog[] = [{ id: 1, task_id: 2471, project_id: 2, actor: 'agent', action: 'dispatched', to_state: 'in_progress', created_at: at }]

let inFlight = 0
// The `since` of every activity load since open() rendered the page.
let loads: string[] = []
const server = setupServer(
  http.get('*/api/projects/2/epics', () => HttpResponse.json({ epics: [{ id: 172, project_id: 2, name: 'Live updates', description: '', priority: '1', created_at: at, updated_at: at }] })),
  http.get('*/api/projects/2/stories', () => HttpResponse.json({ stories: [{ id: 413, epic_id: 172, name: 'Background tabs', description: '', priority: '1', created_at: at, updated_at: at }] })),
  http.get('*/api/projects/2/tasks', () => HttpResponse.json({ tasks })),
  http.get('*/api/projects/2/members', () => HttpResponse.json({ my_access: 'lead', members: [] })),
  http.get('*/api/projects/2/activity', ({ request }) => {
    loads.push(new URL(request.url).searchParams.get('since') ?? '')
    return HttpResponse.json({ activity, count: activity.length })
  }),
  http.get('*/api/projects/2', () => HttpResponse.json({ project: { id: 2, name: 'AMP', code: 'AMP', require_plan_approval: true } })),
  http.get('*/api/*', () => HttpResponse.json({})),
)
server.events.on('request:start', () => { inFlight++ })
server.events.on('request:end', () => { inFlight-- })
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
afterAll(() => { server.close(); vi.unstubAllGlobals() })

const realSetTimeout = globalThis.setTimeout
let visibility: DocumentVisibilityState = 'visible'
const setVisibility = (state: DocumentVisibilityState) => act(() => {
  visibility = state
  document.dispatchEvent(new Event('visibilitychange'))
})
const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms))
// Let any request that was started reach the server, then wait for all of them to finish.
const quiet = async () => {
  await act(() => new Promise(r => realSetTimeout(r, 50)))
  await waitFor(() => expect(inFlight).toBe(0))
}
// A live event: one ticket changed, so the board's tickets change.
let edits = 0
const ticketChanges = () => act(() => { applyTask(2, task(2470, { name: `Ticket 2470 (edit ${++edits})` })) })

beforeEach(() => {
  // Fake time still moves with real time, so findBy and waitFor keep working.
  vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
  visibility = 'visible'
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
})
afterEach(async () => {
  await waitFor(() => expect(inFlight).toBe(0))
  cleanup()
  vi.useRealTimers()
  server.resetHandlers()
  // Drop the override so jsdom's own getter shows through again.
  delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
})

async function open() {
  loads = []
  const router = createMemoryRouter([{ path: '/project/:projectId/dag', element: <DAGView /> }], { initialEntries: ['/project/2/dag'] })
  render(
    <AuthContext.Provider value={{ me: null, loading: false, error: null, isAdmin: false }}>
      <RouterProvider router={router} />
    </AuthContext.Provider>,
  )
  // The board is loaded once the toolbar can expand its one story.
  await screen.findByRole('button', { name: 'Expand all' })
  await quiet()
  return userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
}
/** Turns the overlay on and waits for its first load. */
async function showChanges(user: Awaited<ReturnType<typeof open>>, window: 'Today' | 'This week' = 'Today') {
  await user.click(screen.getByRole('button', { name: window }))
  await screen.findByText(/ticket changed/)
  await quiet()
}

describe('DAG "What changed" activity loads', () => {
  it('off, ticket changes load nothing', async () => {
    await open()
    for (let i = 0; i < 5; i++) await ticketChanges()
    await advance(10_000)
    await quiet()
    expect(loads).toHaveLength(0)
  })

  it('turned on, loads straight away and lists what changed', async () => {
    const user = await open()
    await showChanges(user)
    expect(loads).toHaveLength(1)
    expect(screen.getByText('1 ticket changed today')).toBeInTheDocument()
  })

  it('rapid ticket changes share one load per 5 s', async () => {
    const user = await open()
    await showChanges(user)

    // A burst right after a load: no load until 5 s after it, then one.
    for (let i = 0; i < 10; i++) await ticketChanges()
    await quiet()
    expect(loads).toHaveLength(1)
    await advance(5_000)
    await quiet()
    expect(loads).toHaveLength(2)

    // A change a second for 12 s: one load each 5 s, never one per change.
    for (let i = 0; i < 12; i++) {
      await ticketChanges()
      await advance(1_000)
    }
    await quiet()
    expect(loads).toHaveLength(4)
    // The changes since the last load make one more at the end of its 5 s, then nothing.
    await advance(5_000)
    await quiet()
    expect(loads).toHaveLength(5)
    await advance(20_000)
    await quiet()
    expect(loads).toHaveLength(5)

    // After a quiet spell a change loads straight away.
    await ticketChanges()
    await quiet()
    expect(loads).toHaveLength(6)
  })

  it('a hidden tab loads nothing as tickets change, then loads once when shown', async () => {
    const user = await open()
    await showChanges(user)

    await setVisibility('hidden')
    // Changes inside the first load's 5 s, and long after it.
    for (let i = 0; i < 3; i++) await ticketChanges()
    await advance(10_000)
    for (let i = 0; i < 3; i++) await ticketChanges()
    await advance(60_000)
    await quiet()
    expect(loads).toHaveLength(1)

    await setVisibility('visible')
    await quiet()
    expect(loads).toHaveLength(2)
    // The catch-up is a load like any other: changes right after it wait 5 s.
    await ticketChanges()
    await quiet()
    expect(loads).toHaveLength(2)
    await advance(5_000)
    await quiet()
    expect(loads).toHaveLength(3)
  })

  it('shown again with nothing changed, or hidden and shown many times, loads nothing extra', async () => {
    const user = await open()
    await showChanges(user)

    await setVisibility('hidden')
    await advance(30_000)
    await setVisibility('visible')
    await quiet()
    expect(loads).toHaveLength(1)

    // One change while hidden, then the tab flips back and forth: one catch-up.
    await setVisibility('hidden')
    await ticketChanges()
    for (let i = 0; i < 3; i++) {
      await setVisibility('visible')
      await setVisibility('hidden')
    }
    await setVisibility('visible')
    await quiet()
    expect(loads).toHaveLength(2)
  })

  it('a new window loads straight away, and turning it off stops the reloads', async () => {
    const user = await open()
    await showChanges(user)
    await ticketChanges()

    await user.click(screen.getByRole('button', { name: 'This week' }))
    await quiet()
    expect(loads).toHaveLength(2)
    const sixDaysAgo = Date.now() - 6 * 86_400_000
    expect(Date.parse(loads[1])).toBeLessThan(sixDaysAgo)

    await user.click(screen.getByRole('button', { name: 'Off' }))
    await ticketChanges()
    await advance(10_000)
    await ticketChanges()
    await quiet()
    expect(loads).toHaveLength(2)
  })

  it('after unmount, showing the tab loads nothing', async () => {
    const user = await open()
    await showChanges(user)
    await setVisibility('hidden')
    await ticketChanges()
    await advance(10_000)
    cleanup()
    await setVisibility('visible')
    await quiet()
    expect(loads).toHaveLength(1)
  })
})
