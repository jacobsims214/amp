// The shared board store (#132): a tab switch within a project renders from
// cache on its first frame, live events patch the store for every view, and
// the board loads in a fixed number of requests.
import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Epic, SSEEvent, Story, Task } from '../types'

let sse: ((e: SSEEvent) => void)[] = []
vi.mock('../hooks/useSSE', () => ({
  useSSE: () => {},
  subscribeSSE: (_url: string, l: (e: SSEEvent) => void) => { sse.push(l); return () => { sse = sse.filter(x => x !== l) } },
}))
const calls = { epics: 0, tasks: 0, projectStories: 0, epicStories: 0 }
let projectStoriesSupported = true
const t11 = { id: 11, project_id: 1, state: 'backlog', name: 'T' } as Task
let serverTasks: Task[] = [t11]
let tasksImpl: (pid: number) => Promise<Task[]> = () => Promise.resolve(serverTasks)
vi.mock('../api/client', () => ({
  api: {
    listEpics: () => { calls.epics++; return Promise.resolve([{ id: 1, project_id: 1, name: 'E1' }, { id: 2, project_id: 1, name: 'E2' }] as Epic[]) },
    listTasks: (pid: number) => { calls.tasks++; return tasksImpl(pid) },
    listProjectStories: () => {
      calls.projectStories++
      return projectStoriesSupported ? Promise.resolve([{ id: 5, epic_id: 1 }, { id: 6, epic_id: 2 }] as Story[]) : Promise.reject(new Error('404'))
    },
    listStories: (epicId: number) => { calls.epicStories++; return Promise.resolve([{ id: epicId * 10, epic_id: epicId }] as Story[]) },
  },
}))
const { useBoardData } = await import('../hooks/useBoardData')
const { FRESH_MS, LINGER_MS, invalidateBoard } = await import('./boardStore')
const emit = (e: Partial<SSEEvent>) => act(() => { sse.forEach(l => l({ project_id: 1, ...e } as SSEEvent)) })
const deferred = <T,>() => { let resolve!: (v: T) => void; let reject!: (e: unknown) => void; const promise = new Promise<T>((a, b) => { resolve = a; reject = b }); return { promise, resolve, reject } }

const flush = () => act(async () => { await new Promise(r => setTimeout(r, 0)) })

beforeEach(() => { sse = []; projectStoriesSupported = true; serverTasks = [t11]; tasksImpl = () => Promise.resolve(serverTasks); Object.assign(calls, { epics: 0, tasks: 0, projectStories: 0, epicStories: 0 }) })
afterEach(() => vi.useRealTimers())

describe('board store', () => {
  it('loads epics, tickets and every story in three requests, with no request per epic', async () => {
    const { result } = renderHook(() => useBoardData(1))
    await flush()
    expect(result.current.stories.map(s => s.id)).toEqual([5, 6])
    expect(calls).toEqual({ epics: 1, tasks: 1, projectStories: 1, epicStories: 0 })
  })

  it('falls back to stories per epic on an API without the project endpoint', async () => {
    projectStoriesSupported = false
    const { result } = renderHook(() => useBoardData(1))
    await flush()
    expect(result.current.stories.map(s => s.id)).toEqual([10, 20])
    expect(calls.epicStories).toBe(2)
  })

  it('a tab switch renders from cache on its first frame, without another fetch set', async () => {
    const mission = renderHook(() => useBoardData(1))
    await flush()
    mission.unmount()
    const firstFrames: boolean[] = []
    const board = renderHook(() => { const d = useBoardData(1); firstFrames.push(d.loading || d.tasks.length === 0); return d })
    expect(firstFrames[0]).toBe(false) // data on the very first render
    await flush()
    board.unmount()
    renderHook(() => useBoardData(1)) // Flight plan
    await flush()
    expect(calls).toEqual({ epics: 1, tasks: 1, projectStories: 1, epicStories: 0 })
  })

  it('re-reads in the background, keeping the data, once the cache is stale', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    const a = renderHook(() => useBoardData(1))
    await flush()
    a.unmount()
    vi.setSystemTime(Date.now() + FRESH_MS + 1)
    const b = renderHook(() => useBoardData(1))
    expect(b.result.current).toMatchObject({ loading: false, refreshing: true })
    expect(b.result.current.tasks).toHaveLength(1)
    await flush()
    expect(calls.epics).toBe(2)
    expect(b.result.current.refreshing).toBe(false)
  })

  it('a live task.updated seen on the Board is there when Mission opens, without a refetch', async () => {
    const board = renderHook(() => useBoardData(1))
    await flush()
    act(() => { sse.forEach(l => l({ type: 'task.updated', project_id: 1, payload: { id: 11, project_id: 1, state: 'in_progress', name: 'T' } } as SSEEvent)) })
    expect(board.result.current.tasks[0].state).toBe('in_progress')
    board.unmount()
    const mission = renderHook(() => useBoardData(1))
    expect(mission.result.current.tasks[0].state).toBe('in_progress')
    expect(calls.tasks).toBe(1)
  })

  it('listens once per project, however many views are mounted', async () => {
    renderHook(() => useBoardData(1))
    renderHook(() => useBoardData(1))
    await flush()
    expect(sse).toHaveLength(1)
  })

  // The server's "re-read" signals are task.updated events whose payload isn't a ticket (#132 review I1).
  it('restore from Trash, then back to the Board within 15 s: the restored tickets show', async () => {
    const board = renderHook(() => useBoardData(1))
    await flush()
    board.unmount()
    // Settings → Trash → Restore: the server restores 12 and says so.
    serverTasks = [t11, { id: 12, project_id: 1, state: 'backlog', name: 'Restored' } as Task]
    invalidateBoard(1)
    const back = renderHook(() => useBoardData(1))
    await flush()
    expect(back.result.current.tasks.map(t => t.id)).toEqual([11, 12])
  })

  it('the restored signal re-reads a project that is on screen', async () => {
    const board = renderHook(() => useBoardData(1))
    await flush()
    serverTasks = [t11, { id: 12, project_id: 1, state: 'backlog', name: 'Restored' } as Task]
    await emit({ type: 'task.updated', payload: { restored: [12] } })
    await flush()
    expect(board.result.current.tasks.map(t => t.id)).toEqual([11, 12])
    expect(calls.tasks).toBe(2)
  })

  it('a handover signal re-reads; a deleted signal removes the ticket; a run signal changes nothing', async () => {
    const board = renderHook(() => useBoardData(1))
    await flush()
    await emit({ type: 'task.updated', payload: { task_id: 11, run: 5 } })
    await flush()
    expect(calls.tasks).toBe(1)
    await emit({ type: 'task.updated', payload: { handover: true } })
    await flush()
    expect(calls.tasks).toBe(2)
    await emit({ type: 'task.updated', payload: { task_id: 11, deleted: true } })
    expect(board.result.current.tasks).toEqual([])
  })

  it('a deleted story or epic re-reads the project', async () => {
    renderHook(() => useBoardData(1))
    await flush()
    await emit({ type: 'task.updated', payload: { story_id: 5, task_ids: [11], deleted: true } })
    await flush()
    await emit({ type: 'task.updated', payload: { epic_id: 1, task_ids: [11], deleted: true } })
    await flush()
    expect(calls.tasks).toBe(3)
  })

  it('an event during a load is replayed on top of its answer, so a slow load never undoes it', async () => {
    const board = renderHook(() => useBoardData(1))
    await flush()
    const slow = deferred<Task[]>()
    tasksImpl = () => slow.promise
    act(() => board.result.current.refresh())
    await emit({ type: 'task.completed', payload: { ...t11, state: 'completed' } })
    expect(board.result.current.tasks[0].state).toBe('completed')
    // The server read the tickets before the completion.
    await act(async () => { slow.resolve([t11]) })
    expect(board.result.current.tasks[0].state).toBe('completed')
  })

  it('the newest load wins; a forced load while one is in flight replaces it', async () => {
    const first = deferred<Task[]>()
    tasksImpl = () => first.promise
    const board = renderHook(() => useBoardData(1))
    tasksImpl = () => Promise.resolve([{ ...t11, name: 'new' }])
    act(() => board.result.current.refresh())
    await flush()
    expect(board.result.current.tasks[0].name).toBe('new')
    await act(async () => { first.resolve([{ ...t11, name: 'old' }]) })
    expect(board.result.current.tasks[0].name).toBe('new')
    expect(board.result.current.refreshing).toBe(false)
  })

  it('a failed refetch keeps the data; a 404 or 403 drops it', async () => {
    const board = renderHook(() => useBoardData(1))
    await flush()
    tasksImpl = () => Promise.reject(new Error('down'))
    act(() => board.result.current.refresh())
    await flush()
    expect(board.result.current).toMatchObject({ error: 'down', loading: false })
    expect(board.result.current.tasks).toHaveLength(1)
    tasksImpl = () => Promise.reject(Object.assign(new Error('project 1 not found'), { status: 404 }))
    act(() => board.result.current.refresh())
    await flush()
    expect(board.result.current.tasks).toEqual([])
    expect(board.result.current.error).toBe('project 1 not found')
  })

  it("another project's event doesn't touch this project", async () => {
    const board = renderHook(() => useBoardData(1))
    await flush()
    await emit({ type: 'task.updated', project_id: 2, payload: { ...t11, project_id: 2, state: 'completed' } })
    await emit({ type: 'task.updated', project_id: 2, payload: { handover: true } })
    expect(board.result.current.tasks[0].state).toBe('backlog')
    expect(calls.tasks).toBe(1)
  })

  it('stops listening LINGER_MS after the last view of the project leaves', async () => {
    vi.useFakeTimers()
    const board = renderHook(() => useBoardData(1))
    await act(() => vi.advanceTimersByTimeAsync(0))
    board.unmount()
    expect(sse).toHaveLength(1)
    await act(() => vi.advanceTimersByTimeAsync(LINGER_MS - 1))
    expect(sse).toHaveLength(1)
    // A view coming back inside the window keeps the same listener.
    const again = renderHook(() => useBoardData(1))
    again.unmount()
    await act(() => vi.advanceTimersByTimeAsync(LINGER_MS + 1))
    expect(sse).toHaveLength(0)
  })
})
