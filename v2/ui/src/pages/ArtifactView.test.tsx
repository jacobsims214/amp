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
  api.getArtifact.mockResolvedValue({ artifact, versions: [] })
  api.artifactActivity.mockResolvedValue([])
  api.getTask.mockResolvedValue({ id: TASK_ID, name: 'Build login', state: 'in_progress' } as Task)
  api.getComments.mockResolvedValue([])
  api.addComment.mockResolvedValue({})
})
afterEach(() => {
  vi.useRealTimers()
  vi.clearAllMocks()
  sources = []
})

/** Renders the page, waits for the ticket panel's first load, then switches to fake timers. */
async function renderPanel() {
  const router = createMemoryRouter([{ path: '/artifacts/:artifactId', element: <ArtifactView /> }], { initialEntries: ['/artifacts/1'] })
  const view = render(<RouterProvider router={router} />)
  await screen.findByRole('link', { name: 'Build login' })
  expect(api.getTask).toHaveBeenCalledTimes(1)
  expect(api.getComments).toHaveBeenCalledTimes(1)
  vi.useFakeTimers()
  return view
}

function emit(type: SSEEvent['type'], payload: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify({ type, project_id: 3, payload }) }))
}

async function advance(ms: number) {
  await act(async () => { vi.advanceTimersByTime(ms) })
}

const reloads = () => api.getTask.mock.calls.length - 1

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
