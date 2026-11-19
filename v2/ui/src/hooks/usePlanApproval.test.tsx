// After the live stream reconnects, usePlanApproval re-reads the project
// (#2535): a project.updated sent while the stream was down (a dropped
// connection, or a tab hidden for 30 s) never arrives, so an approval made in
// that gap would otherwise never reach the approval banner and its counts.
import { act, renderHook, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi, type MockInstance } from 'vitest'
import { usePlanApproval } from './usePlanApproval'
import { subscribeSSE } from './useSSE'
import { api } from '../api/client'
import type { Project } from '../types'

const project = (over: Partial<Project> = {}): Project => ({
  id: 3, name: 'Mission Control', code: 'MC', description: '', state: 'active',
  created_at: '2026-11-01T10:00:00Z', updated_at: '2026-11-01T10:00:00Z', require_plan_approval: true, ...over,
})

/** What a project read answers with, for the id it was asked for. */
let answer: (id: number) => Project = id => project({ id })
/** While set, each project read waits here for its own release, in call order. */
let held: Array<() => void> | null = null
/** Set to make the next project read fail. */
let failNext = false

// The real useSSE over a fake stream (jsdom has no navigator.locks, so each
// test's tab opens its own): frames reach the hook as they would live.
let sources: FakeEventSource[] = []
class FakeEventSource {
  constructor() { sources.push(this) }
  onmessage: ((e: { data: string }) => void) | null = null
  onerror = null
  readyState = 1
  close() {}
}

let projectReads: MockInstance<typeof api.getProject>
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource) })
beforeEach(() => {
  projectReads = vi.spyOn(api, 'getProject').mockImplementation(async (id: number) => {
    const fail = failNext
    failNext = false
    if (held) { const queue = held; await new Promise<void>(r => { queue.push(r) }) }
    if (fail) throw new Error('database unavailable')
    return answer(id)
  })
})
afterEach(() => {
  held?.forEach(release => release())
  held = null
  failNext = false
  answer = id => project({ id })
  sources = []
  vi.restoreAllMocks()
})
afterAll(() => { vi.unstubAllGlobals() })

const reads = () => projectReads.mock.calls.length

function send(data: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify(data) }))
}

/** The server's first frame on every new stream. It carries no project. */
const connect = () => send({ type: 'connected' })

/** Lets every read already answered reach the hook's state. */
const flush = () => act(() => new Promise(r => setTimeout(r, 20)))

type View = { result: { current: ReturnType<typeof usePlanApproval> } }
/** Waits until the hook shows project id, read and settled. */
async function loaded(view: View, id = 3) {
  await waitFor(() => expect(view.result.current.project?.id).toBe(id))
  await flush()
}

describe('usePlanApproval re-reads the project after a stream reconnect (#2535)', () => {
  it('a connected during the first read adds no request', async () => {
    held = []
    const view = renderHook(() => usePlanApproval(3))
    expect(reads()).toBe(1)
    // The opening frame of the stream the hook opened, while its read is in flight.
    connect()
    await flush()
    expect(reads()).toBe(1)

    held[0]()
    await loaded(view)
    expect(reads()).toBe(1)
  })

  it('a connected after the first read has settled re-reads the project exactly once, and shows the approval made in the gap', async () => {
    const view = renderHook(() => usePlanApproval(3))
    await loaded(view)
    expect(view.result.current.project?.plan_approved_at).toBeUndefined()
    expect(reads()).toBe(1)

    // Someone approved while the stream was down; its project.updated was lost.
    answer = id => project({ id, plan_approved_at: '2026-11-18T12:00:00Z', plan_approved_by: 9 })
    held = []
    connect()
    expect(reads()).toBe(2)
    expect(projectReads).toHaveBeenLastCalledWith(3)
    // Quiet: the project stays as it was until the answer arrives.
    expect(view.result.current.project?.id).toBe(3)
    expect(view.result.current.project?.plan_approved_at).toBeUndefined()

    held[0]()
    await waitFor(() => expect(view.result.current.project?.plan_approved_at).toBe('2026-11-18T12:00:00Z'))
    expect(view.result.current.project?.plan_approved_by).toBe(9)
    await flush()
    expect(reads()).toBe(2)

    // Each later reconnect re-reads once more.
    held = null
    connect()
    expect(reads()).toBe(3)
  })

  it('a hook that mounts on a stream another view already holds re-reads on the first connected it sees', async () => {
    // The board holds the stream and heard its opening frame before this hook mounted.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      const view = renderHook(() => usePlanApproval(3))
      expect(sources).toHaveLength(1)
      await loaded(view)
      expect(reads()).toBe(1)

      connect()
      expect(reads()).toBe(2)
      view.unmount()
    } finally {
      unsubscribe()
    }
  })

  it('a failed first read counts as settled, so a reconnect retries it', async () => {
    failNext = true
    const view = renderHook(() => usePlanApproval(3))
    // Nothing on screen tells a failed read from one in flight, so wait on the read itself.
    await act(() => (projectReads.mock.results[0].value as Promise<unknown>).catch(() => {}))
    await flush()
    expect(view.result.current.project).toBeNull()
    expect(reads()).toBe(1)

    connect()
    expect(reads()).toBe(2)
    await loaded(view)
  })

  it('after a switch to another project, a reconnect waits for its first read, and a late answer for the old one is dropped', async () => {
    const view = renderHook(({ id }) => usePlanApproval(id), { initialProps: { id: 3 } })
    await loaded(view, 3)

    // A reconnect's re-read of project 3 is still in flight when the page moves to project 4.
    held = []
    connect()
    expect(reads()).toBe(2)
    view.rerender({ id: 4 })
    expect(reads()).toBe(3)
    expect(projectReads).toHaveBeenLastCalledWith(4)
    // During project 4's first read: dropped.
    connect()
    expect(reads()).toBe(3)

    const [lateAnswerFor3, firstReadOf4] = held
    firstReadOf4()
    await loaded(view, 4)
    lateAnswerFor3()
    await flush()
    expect(view.result.current.project?.id).toBe(4)

    held = null
    connect()
    expect(reads()).toBe(4)
    expect(projectReads).toHaveBeenLastCalledWith(4)
  })

  it('ordinary events are not reconnects, and project.updated still merges without a read', async () => {
    const view = renderHook(() => usePlanApproval(3))
    await loaded(view)

    send({ type: 'task.updated', project_id: 3, payload: { id: 12, state: 'completed' } })
    send({ type: 'project.updated', project_id: 3, payload: { id: 3, plan_approved_at: '2026-11-18T12:00:00Z' } })
    await flush()
    expect(reads()).toBe(1)
    expect(view.result.current.project?.plan_approved_at).toBe('2026-11-18T12:00:00Z')
    expect(view.result.current.project?.name).toBe('Mission Control')
  })
})
