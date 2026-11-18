// TicketArtifacts: the ticket drawer's list of a ticket's artifacts.
// Shared fixtures and stream helpers are at the top; each behaviour has its own
// describe block below.
import { act, render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi, type MockInstance } from 'vitest'
import { TicketArtifacts } from './ArtifactBits'
import { api } from '../../api/client'
import { subscribeSSE } from '../../hooks/useSSE'
import { resetSession } from '../../lib/session'
import type { Artifact } from '../../types'

const PROJECT = 2

const artifact = (over: Partial<Artifact> = {}): Artifact => ({
  id: 70, project_id: PROJECT, task_id: 1, title: 'Lead import mockup', kind: 'html', status: 'in_review',
  current_version: 1, approved_version: null, reviewed_by: null, reviewed_at: null, created_by: 9,
  created_at: '2026-11-05T10:00:00Z', updated_at: '2026-11-05T10:00:00Z',
  keep: false, kept_by: null, kept_at: null, expires_at: null, expiry_dismissed_at: null, ...over,
})

/** What the server lists for each ticket next. Tickets not in here have none. */
let artifactsByTask: Record<number, Artifact[]> = {}
/** While set, the list read waits for it: a read is in flight. */
let hold: Promise<void> | null = null
const server = setupServer(
  http.get(`*/api/projects/${PROJECT}/artifacts`, async ({ request }) => {
    if (hold) await hold
    const taskId = Number(new URL(request.url).searchParams.get('task_id'))
    return HttpResponse.json({ artifacts: artifactsByTask[taskId] ?? [] })
  }),
)

// The real useSSE over a fake stream (jsdom has no navigator.locks, so each
// test's tab opens its own): frames reach the list as they would live.
let sources: FakeEventSource[] = []
class FakeEventSource {
  constructor() { sources.push(this) }
  onmessage: ((e: { data: string }) => void) | null = null
  onerror = null
  readyState = 1
  close() {}
}

let listReads: MockInstance<typeof api.listArtifacts>
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
beforeEach(() => {
  artifactsByTask = { 1: [artifact()] }
  listReads = vi.spyOn(api, 'listArtifacts')
})
afterEach(() => {
  hold = null
  sources = []
  server.resetHandlers()
  vi.restoreAllMocks()
  resetSession()
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

/** How many list reads the component has started. */
const reads = () => listReads.mock.calls.length

/** Mounts the list for a ticket. Returns a function that switches it to another ticket. */
function mount(taskId = 1) {
  const view = (id: number) => <MemoryRouter><TicketArtifacts projectId={PROJECT} taskId={id} /></MemoryRouter>
  const { rerender } = render(view(taskId))
  return (id: number) => rerender(view(id))
}

/** Holds every list read until the returned function is called. */
function holdReads(): () => void {
  let release!: () => void
  hold = new Promise(r => { release = r })
  return () => { hold = null; release() }
}

/** Waits for the nth list read (0-based) to settle, whether it answered or failed. */
const settled = (n = 0) => act(() => listReads.mock.results[n].value.catch(() => {}))

function send(data: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify(data) }))
}

/** The server's first frame on every new stream. It carries no project. */
const connect = () => send({ type: 'connected' })

/** Lets any read a frame started reach the server and answer. */
const flush = () => act(() => new Promise(r => setTimeout(r, 20)))

describe('TicketArtifacts re-reads after a stream reconnect (#2539)', () => {
  it('a connected during the first read adds no request', async () => {
    const release = holdReads()
    mount()
    await waitFor(() => expect(reads()).toBe(1))
    // The opening frame of the stream the list opened, while its read is in flight.
    connect()
    await flush()
    expect(reads()).toBe(1)

    release()
    await screen.findByText('Lead import mockup')
    await flush()
    expect(reads()).toBe(1)
  })

  it('a connected after the first read has settled re-reads the list exactly once', async () => {
    mount()
    await screen.findByText('Lead import mockup')
    expect(reads()).toBe(1)

    // Published while the stream was down: no artifact.updated ever arrives.
    artifactsByTask[1] = [artifact(), artifact({ id: 71, title: 'Import flow diagram', kind: 'svg' })]
    connect()
    expect(reads()).toBe(2)
    await screen.findByText('Import flow diagram')
    // The list stayed on screen while it re-read.
    expect(screen.getByText('Lead import mockup')).toBeInTheDocument()

    // Each later reconnect re-reads once more.
    connect()
    await flush()
    expect(reads()).toBe(3)
  })

  it('a list that mounts on a stream another view already holds re-reads on the first connected it sees', async () => {
    // The Board holds the stream and heard its opening frame before the drawer opened.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      mount()
      await screen.findByText('Lead import mockup')
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
    server.use(http.get(`*/api/projects/${PROJECT}/artifacts`, () => HttpResponse.json({ error: 'down' }, { status: 500 }), { once: true }))
    mount()
    await settled(0)
    expect(screen.queryByText('Lead import mockup')).toBeNull()

    connect()
    await screen.findByText('Lead import mockup')
    expect(reads()).toBe(2)
  })

  it('after switching tickets, a connected during the new ticket\'s first read adds no request', async () => {
    artifactsByTask[2] = [artifact({ id: 80, task_id: 2, title: 'Billing copy' })]
    const show = mount(1)
    await screen.findByText('Lead import mockup')

    const release = holdReads()
    show(2)
    await waitFor(() => expect(reads()).toBe(2))
    connect()
    await flush()
    expect(reads()).toBe(2)

    release()
    await screen.findByText('Billing copy')
    connect()
    await flush()
    expect(reads()).toBe(3)
    expect(listReads.mock.calls.at(-1)).toEqual([PROJECT, 2])
  })

  it('an ordinary event is not a reconnect', async () => {
    mount()
    await screen.findByText('Lead import mockup')
    send({ type: 'task.updated', project_id: PROJECT, payload: { id: 1, state: 'in_progress' } })
    await flush()
    expect(reads()).toBe(1)
  })
})
