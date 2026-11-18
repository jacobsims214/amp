import { StrictMode } from 'react'
import { act, fireEvent, render, screen } from '@testing-library/react'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Artifact, SSEEvent, Task } from '../types'

const TASK_ID = 11
const artifact: Artifact = {
  id: 1, project_id: 3, task_id: TASK_ID, task_ref: 'P-11', task_name: 'Build login', title: 'Login mockup', kind: 'svg',
  status: 'in_review', current_version: 1, approved_version: null, reviewed_by: null, reviewed_at: null, created_by: 1,
  created_at: '2026-11-18T00:00:00Z', updated_at: '2026-11-18T00:00:00Z', keep: false, kept_by: null, kept_at: null,
  expires_at: null, expiry_dismissed_at: null,
}

const api = vi.hoisted(() => ({
  getArtifact: vi.fn(),
  artifactActivity: vi.fn(),
  artifactContentUrl: vi.fn(() => '/content'),
  getTask: vi.fn(),
  getComments: vi.fn(),
  addComment: vi.fn(),
}))
vi.mock('../api/client', () => ({ api }))

const { ArtifactView } = await import('./ArtifactView')
const { HIDDEN_CLOSE_MS, subscribeSSE } = await import('../hooks/useSSE')
const { resetSession } = await import('../lib/session')

// This tab's visibility. useSSE lets a tab's streams go HIDDEN_CLOSE_MS after it is hidden.
let visibility: DocumentVisibilityState = 'visible'
function setVisibility(state: DocumentVisibilityState) {
  visibility = state
  document.dispatchEvent(new Event('visibilitychange'))
}

// The real useSSE, over a fake stream: events reach the page as they would live.
let sources: FakeEventSource[] = []
class FakeEventSource {
  constructor() { sources.push(this) }
  onmessage: ((e: { data: string }) => void) | null = null
  onerror = null
  readyState = 1
  close() {}
}
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource) })

beforeEach(() => {
  visibility = 'visible'
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
  api.getArtifact.mockResolvedValue({ artifact, versions: [] })
  api.artifactActivity.mockResolvedValue([])
  api.getTask.mockResolvedValue({ id: TASK_ID, name: 'Build login', state: 'in_progress' } as Task)
  api.getComments.mockResolvedValue([])
  api.addComment.mockResolvedValue({})
})
afterEach(() => {
  // A paused tab resumes, so the next test starts on a visible one.
  if (visibility !== 'visible') setVisibility('visible')
  delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
  vi.useRealTimers()
  vi.clearAllMocks()
  resetSession()
  sources = []
})

/** How many times the panel had loaded its ticket when renderPanel returned. */
let initialLoads = 1

/** Lets pending promise callbacks run, such as the .finally that marks a read settled. */
async function settle() {
  await act(async () => { for (let i = 0; i < 50; i++) await Promise.resolve() })
}

function renderPage({ strict = false } = {}) {
  const router = createMemoryRouter([{ path: '/artifacts/:artifactId', element: <ArtifactView /> }], { initialEntries: ['/artifacts/1'] })
  const page = <RouterProvider router={router} />
  return render(strict ? <StrictMode>{page}</StrictMode> : page)
}

/** A promise the test resolves or rejects later, to hold a read in flight. */
function held<T>() {
  let resolve!: (v: T) => void, reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

/** Renders the page, waits for the ticket panel's first load to settle, then switches to fake timers. */
async function renderPanel({ strict = false } = {}) {
  const view = renderPage({ strict })
  await screen.findByRole('link', { name: 'Build login' })
  await settle()
  // StrictMode runs each mount effect twice, so the panel loads twice.
  initialLoads = strict ? 2 : 1
  expect(api.getTask).toHaveBeenCalledTimes(initialLoads)
  expect(api.getComments).toHaveBeenCalledTimes(initialLoads)
  vi.useFakeTimers()
  return view
}

function send(data: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify(data) }))
}

function emit(type: SSEEvent['type'], payload: unknown) {
  send({ type, project_id: 3, payload })
}

/** The server's first frame on every new stream. It carries no project. */
function connect() {
  send({ type: 'connected' })
}

async function advance(ms: number) {
  await act(async () => { vi.advanceTimersByTime(ms) })
}

const reloads = () => api.getTask.mock.calls.length - initialLoads

describe('ArtifactView ticket panel live reloads', () => {
  it('ignores events for other tickets', async () => {
    await renderPanel()
    emit('task.updated', { id: 12, name: 'Another ticket' })
    emit('task.completed', { id: 12 })
    // A comment whose own id happens to equal this ticket's id, on another ticket.
    emit('comment.added', { id: TASK_ID, task_id: 12, body: 'hi' })
    emit('artifact.updated', { id: 2, task_id: 12 })
    emit('task.updated', { task_id: 12, deleted: true })
    emit('task.updated', { task_ids: [12, 13], deleted: true })
    // An event type that never changes the panel, even for this ticket.
    emit('task.dispatched', { id: TASK_ID })
    await advance(5000)
    expect(reloads()).toBe(0)
    expect(api.getComments).toHaveBeenCalledTimes(1)
  })

  it('reloads once for a burst of events on its own ticket within 1 s', async () => {
    await renderPanel()
    emit('comment.added', { id: 99, task_id: TASK_ID, body: 'Done' })
    await advance(300)
    emit('task.updated', { id: TASK_ID, state: 'in_progress' })
    await advance(300)
    emit('task.completed', { id: TASK_ID, state: 'completed' })
    await advance(399)
    expect(reloads()).toBe(0)
    await advance(1)
    expect(reloads()).toBe(1)
    expect(api.getComments).toHaveBeenCalledTimes(2)
    await advance(5000)
    expect(reloads()).toBe(1)

    // A later event schedules the next reload.
    emit('artifact.updated', { id: 1, task_id: TASK_ID })
    await advance(1000)
    expect(reloads()).toBe(2)
  })

  it('reloads for server signals that name its ticket', async () => {
    await renderPanel()
    emit('task.updated', { task_id: TASK_ID, deleted: true })
    await advance(1000)
    expect(reloads()).toBe(1)
    emit('task.updated', { restored: [TASK_ID, 40] })
    await advance(1000)
    expect(reloads()).toBe(2)
  })

  it('drops a scheduled reload when the panel unmounts', async () => {
    const view = await renderPanel()
    emit('task.blocked', { id: TASK_ID, state: 'blocked' })
    view.unmount()
    await advance(5000)
    expect(reloads()).toBe(0)
  })

  it('a comment sent from the panel reloads now and replaces a scheduled reload', async () => {
    await renderPanel()
    emit('task.updated', { id: TASK_ID, state: 'in_progress' })
    fireEvent.change(screen.getByRole('textbox', { name: 'Comment' }), { target: { value: 'Looks good' } })
    fireEvent.click(screen.getByRole('button', { name: 'Comment' }))
    await act(async () => {})
    expect(api.addComment).toHaveBeenCalledWith(TASK_ID, 'Looks good')
    expect(reloads()).toBe(1)
    await advance(5000)
    expect(reloads()).toBe(1)
  })
})


// Both handlers on this page use useStreamReconnect: a connected while the
// view's first load is in flight is dropped, and each one after it re-reads.
describe('ArtifactView ticket panel after a stream reconnect', () => {
  it('drops a connected that arrives during its first load', async () => {
    const task = held<Task>()
    api.getTask.mockReturnValueOnce(task.promise)
    renderPage()
    // The panel is mounted and its ticket read is in flight.
    await screen.findByRole('textbox', { name: 'Comment' })
    // Fake timers before the frame, so a reload it wrongly scheduled would run below.
    vi.useFakeTimers()
    connect()
    task.resolve({ id: TASK_ID, name: 'Build login', state: 'in_progress' } as Task)
    await settle()
    expect(screen.getByRole('link', { name: 'Build login' })).toBeInTheDocument()
    await advance(5000)
    expect(api.getTask).toHaveBeenCalledTimes(1)
    expect(api.getComments).toHaveBeenCalledTimes(1)
    // The first load has settled now: the next connected is a reconnect.
    connect()
    await advance(1000)
    expect(api.getTask).toHaveBeenCalledTimes(2)
  })

  it('reloads once on each connected after its first load', async () => {
    await renderPanel()
    // A reconnect: what was sent while the stream was down is lost.
    connect()
    await advance(999)
    expect(reloads()).toBe(0)
    await advance(1)
    expect(reloads()).toBe(1)
    expect(api.getComments).toHaveBeenCalledTimes(2)
    await advance(5000)
    expect(reloads()).toBe(1)

    connect()
    await advance(1000)
    expect(reloads()).toBe(2)
  })

  it('a failed first load still counts as settled, so a reconnect retries it', async () => {
    api.getTask.mockRejectedValueOnce(new Error('offline'))
    renderPage()
    await screen.findByRole('textbox', { name: 'Comment' })
    await settle()
    expect(screen.queryByRole('link', { name: 'Build login' })).toBeNull()
    vi.useFakeTimers()
    connect()
    await advance(1000)
    expect(api.getTask).toHaveBeenCalledTimes(2)
    expect(screen.getByRole('link', { name: 'Build login' })).toBeInTheDocument()
  })

  it('a reconnect during a burst of its own ticket events gives one reload', async () => {
    await renderPanel()
    connect()
    emit('comment.added', { id: 99, task_id: TASK_ID, body: 'Done' })
    await advance(300)
    connect()
    await advance(300)
    emit('task.completed', { id: TASK_ID, state: 'completed' })
    await advance(5000)
    expect(reloads()).toBe(1)

    // The other way round: the reconnect first, then the burst.
    connect()
    await advance(200)
    emit('comment.added', { id: 100, task_id: TASK_ID, body: 'Again' })
    emit('task.updated', { id: TASK_ID, state: 'in_progress' })
    await advance(5000)
    expect(reloads()).toBe(2)
  })

  it('a panel that joins a stream another view holds reloads on the first connected it sees', async () => {
    // Another view (the board store lingers for 30 s) opened the stream and
    // heard its opening frame before this page mounted.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      await renderPanel()
      expect(sources).toHaveLength(1)
      connect()
      await advance(1000)
      expect(reloads()).toBe(1)
      connect()
      await advance(1000)
      expect(reloads()).toBe(2)
    } finally {
      unsubscribe()
    }
  })

  it('a panel opened in a background tab with no leader catches up on its first connected after being shown', async () => {
    // The tab has been hidden long enough to let its stream go. No tab holds
    // one (jsdom has no Web Locks, so this tab would open its own), and
    // nothing opens one while it is paused.
    vi.useFakeTimers()
    setVisibility('hidden')
    vi.advanceTimersByTime(HIDDEN_CLOSE_MS)
    vi.useRealTimers()
    await renderPanel()
    expect(sources).toHaveLength(0)
    // Shown: a new stream opens, and its connected is the panel's first.
    setVisibility('visible')
    expect(sources).toHaveLength(1)
    connect()
    await advance(1000)
    expect(reloads()).toBe(1)
    expect(api.getComments).toHaveBeenCalledTimes(2)
    // The page's artifact catches up too.
    expect(api.getArtifact).toHaveBeenCalledTimes(2)
  })

  it('StrictMode re-running the subscription still gives one reload per reconnect', async () => {
    await renderPanel({ strict: true })
    connect()
    await advance(5000)
    expect(reloads()).toBe(1)
    connect()
    await advance(1000)
    expect(reloads()).toBe(2)
  })
})

/** Renders the page and waits for the artifact's first read to settle. */
async function renderArtifact() {
  const view = renderPage()
  await screen.findByText('Login mockup')
  await settle()
  return view
}

describe('ArtifactView re-reads the artifact after a stream reconnect', () => {
  it('a connected during the first read causes no extra request', async () => {
    const read = held<{ artifact: Artifact; versions: [] }>()
    api.getArtifact.mockReturnValueOnce(read.promise)
    renderPage()
    expect(sources).toHaveLength(1)
    // The opening frame of the page's new stream, while its read is in flight.
    connect()
    read.resolve({ artifact, versions: [] })
    await screen.findByText('Login mockup')
    await settle()
    expect(api.getArtifact).toHaveBeenCalledTimes(1)
  })

  it('a connected after the first read settles re-reads the artifact exactly once', async () => {
    await renderArtifact()
    expect(api.getArtifact).toHaveBeenCalledTimes(1)
    connect()
    expect(api.getArtifact).toHaveBeenCalledTimes(2)
    await settle()
    expect(api.getArtifact).toHaveBeenCalledTimes(2)
    connect()
    expect(api.getArtifact).toHaveBeenCalledTimes(3)
  })

  it('shows a version published while the stream was down', async () => {
    await renderArtifact()
    api.getArtifact.mockResolvedValue({ artifact: { ...artifact, title: 'Login mockup v2', current_version: 2 }, versions: [] })
    connect()
    expect(await screen.findByText('Login mockup v2')).toBeInTheDocument()
  })

  it('a page that mounts on an already-open stream re-reads on the next connected', async () => {
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect() // the opening frame, heard before this page mounted
      await renderArtifact()
      expect(sources).toHaveLength(1)
      expect(api.getArtifact).toHaveBeenCalledTimes(1)
      connect()
      expect(api.getArtifact).toHaveBeenCalledTimes(2)
    } finally {
      unsubscribe()
    }
  })

  it('a failed first read still counts as settled, so a reconnect retries it', async () => {
    api.getArtifact.mockRejectedValueOnce(new Error('Could not reach AMP'))
    renderPage()
    await screen.findByText('Could not reach AMP')
    await settle()
    connect()
    expect(await screen.findByText('Login mockup')).toBeInTheDocument()
    expect(api.getArtifact).toHaveBeenCalledTimes(2)
  })

  it('ordinary events are not treated as a reconnect', async () => {
    await renderArtifact()
    emit('task.updated', { id: TASK_ID, state: 'in_progress' })
    emit('artifact.updated', { id: 2, task_id: 12 })
    await settle()
    expect(api.getArtifact).toHaveBeenCalledTimes(1)
  })
})
