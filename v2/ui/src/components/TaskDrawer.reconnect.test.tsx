// After the live stream reconnects, the drawer re-reads its ticket's receipt
// (#2531): a receipt.updated sent while the stream was down (a dropped
// connection, or a tab hidden for 30 s) never arrives. The ticket's own fields
// come from the board store, which re-reads itself (#1570).
import { act, render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { TaskDrawer } from './TaskDrawer'
import { api } from '../api/client'
import { subscribeSSE } from '../hooks/useSSE'
import type { ProjectAccess, ProjectMember, Task } from '../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 1, project_id: 2, epic_id: 1, story_id: 3, name: 'Import leads', description: 'Write the import.', acceptance_criteria: '',
  state: 'in_progress', priority: '1', dependency_ids: [], created_at: '2026-11-05T10:00:00Z', updated_at: '2026-11-05T10:00:00Z',
  planned_at: '2026-11-05T10:00:00Z', owner_user_id: 9, created_by: 9, agent_id: 'amp-worker', ...over,
})
const members: ProjectMember[] = [{ user_id: 9, email: 'jo@example.com', display_name: 'Jo Owner', access: 'member' as ProjectAccess }]

// A project that requires receipts, so the header's "Requires receipt" shows
// once the receipt read has answered.
const receipt = {
  receipt: null, mode: 'require',
  settings: { mode: 'require', min_level: 'self_checked', project_mode: '', org_mode: 'require' },
}
/** While set, the receipt read waits for it: the drawer's first load is in flight. */
let hold: Promise<void> | null = null
const server = setupServer(
  http.get('*/api/tasks/1/receipt', async () => {
    if (hold) await hold
    return HttpResponse.json(receipt)
  }),
  http.get('*/api/projects/2/tasks', () => HttpResponse.json({ tasks: [] })),
  http.get('*/api/projects/2/epics', () => HttpResponse.json({ epics: [{ id: 1, project_id: 2, name: 'Leads', description: '' }] })),
  http.get('*/api/epics/1/stories', () => HttpResponse.json({ stories: [{ id: 3, epic_id: 1, name: 'Import', description: '', acceptance_criteria: '' }] })),
  http.get('*/api/tasks/:id/history', () => HttpResponse.json({ history: [], count: 0 })),
  http.get('*/api/tasks/:id/comments', () => HttpResponse.json({ comments: [] })),
  http.get('*/api/tasks/:id/runs', () => HttpResponse.json({ runs: [], usage: { runs: 0 } })),
  http.get('*/api/*', () => HttpResponse.json({})),
)

// The real useSSE over a fake stream (jsdom has no navigator.locks, so each
// test's tab opens its own): frames reach the drawer as they would live.
let sources: FakeEventSource[] = []
class FakeEventSource {
  constructor() { sources.push(this) }
  onmessage: ((e: { data: string }) => void) | null = null
  onerror = null
  readyState = 1
  close() {}
}

let receiptReads: ReturnType<typeof vi.spyOn>
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen() })
beforeEach(() => { receiptReads = vi.spyOn(api, 'getTaskReceipt') })
afterEach(() => {
  hold = null
  sources = []
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

const reads = () => receiptReads.mock.calls.length

function open(t: Task = task()) {
  const router = createMemoryRouter([{ path: '*', element: <TaskDrawer task={t} onClose={() => {}} members={members} meId={9}
    myAccess={'member' as ProjectAccess} onTaskUpdated={() => {}} /> }],
    { initialEntries: ['/project/2/board?focus=1&tab=details'] })
  render(<RouterProvider router={router} />)
}

function send(data: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify(data) }))
}

/** The server's first frame on every new stream. It carries no project. */
const connect = () => send({ type: 'connected' })

/** Lets any read a frame started reach the server and answer. */
const flush = () => act(() => new Promise(r => setTimeout(r, 20)))

/** Waits for the drawer's first receipt read to answer. */
const loaded = () => screen.findByTestId('requires-receipt')

describe('TaskDrawer re-reads its receipt after a stream reconnect (#2531)', () => {
  it('a connected during the first receipt read adds no request', async () => {
    let release!: () => void
    hold = new Promise(r => { release = r })
    open()
    await waitFor(() => expect(reads()).toBe(1))
    // The opening frame of the stream the drawer opened, while its read is in flight.
    connect()
    await flush()
    expect(reads()).toBe(1)

    release()
    await loaded()
    await flush()
    expect(reads()).toBe(1)
  })

  it('a connected after the first read has settled re-reads the receipt exactly once', async () => {
    open()
    await loaded()
    expect(reads()).toBe(1)

    connect()
    await flush()
    expect(reads()).toBe(2)

    // Each later reconnect re-reads once more.
    connect()
    await flush()
    expect(reads()).toBe(3)
  })

  it('a drawer that opens on a stream another view already holds re-reads on the first connected it sees', async () => {
    // The Board holds the stream and heard its opening frame before the drawer opened.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      open()
      await loaded()
      expect(sources).toHaveLength(1)
      expect(reads()).toBe(1)

      connect()
      await flush()
      expect(reads()).toBe(2)
    } finally {
      unsubscribe()
    }
  })

  it('a failed first read counts as settled, so a reconnect retries it', async () => {
    server.use(http.get('*/api/tasks/1/receipt', () => HttpResponse.json({ error: 'down' }, { status: 500 }), { once: true }))
    open()
    await waitFor(() => expect(reads()).toBe(1))
    await flush()
    expect(screen.queryByTestId('requires-receipt')).toBeNull()

    connect()
    await loaded()
    expect(reads()).toBe(2)
  })

  it('ordinary events are not reconnects, and receipt.updated still re-reads once', async () => {
    open()
    await loaded()
    send({ type: 'task.updated', project_id: 2, payload: { id: 1, state: 'in_progress' } })
    send({ type: 'receipt.updated', project_id: 2, payload: { task_id: 7 } })
    await flush()
    expect(reads()).toBe(1)

    send({ type: 'receipt.updated', project_id: 2, payload: { task_id: 1 } })
    await flush()
    expect(reads()).toBe(2)
  })

  it('a human ticket has no receipt to read, before or after a reconnect', async () => {
    open(task({ performer: 'human', agent_id: undefined }))
    await screen.findByText('Import leads')
    connect()
    await flush()
    expect(reads()).toBe(0)
  })
})
