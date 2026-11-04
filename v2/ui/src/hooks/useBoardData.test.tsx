// useBoardData splits a first load (`loading`, nothing on screen) from a
// refetch (`refreshing`, the loaded board stays).
import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LOAD_TIMEOUT_MS } from '../lib/timeout'
import type { Epic, Task } from '../types'

const epic = (id: number, project_id: number) => ({ id, project_id, name: `E${id}` }) as Epic
const task = (id: number, project_id: number) => ({ id, project_id, epic_id: 1, story_id: 1, name: `T${id}`, state: 'backlog' }) as Task

let pending: { resolve: () => void; reject: (e: Error) => void } | null = null
let tasksFor: Record<number, Task[]> = {}
vi.mock('./useSSE', () => ({ useSSE: () => 'connected' }))
vi.mock('../api/client', () => ({
  api: {
    listEpics: (pid: number) => new Promise<Epic[]>((resolve, reject) => {
      pending = { resolve: () => resolve([epic(pid * 10, pid)]), reject }
    }),
    listTasks: (pid: number) => Promise.resolve(tasksFor[pid] ?? []),
    listStories: () => Promise.resolve([]),
  },
}))
const { useBoardData } = await import('./useBoardData')
const { api } = await import('../api/client')

const flush = () => new Promise(r => setTimeout(r, 0))
const finish = () => act(async () => { pending!.resolve(); await flush() })

beforeEach(() => { pending = null; tasksFor = { 1: [task(11, 1)], 2: [task(21, 2)] } })

describe('useBoardData', () => {
  it('reports the first load as loading, and a refetch as refreshing with the data kept', async () => {
    const { result } = renderHook(() => useBoardData(1))
    expect(result.current).toMatchObject({ loading: true, refreshing: false, tasks: [] })
    await finish()
    expect(result.current).toMatchObject({ loading: false, refreshing: false })
    const tasks = result.current.tasks
    expect(tasks.map(t => t.id)).toEqual([11])

    act(() => { result.current.refresh() })
    expect(result.current).toMatchObject({ loading: false, refreshing: true })
    expect(result.current.tasks).toBe(tasks)
    await finish()
    expect(result.current).toMatchObject({ loading: false, refreshing: false })
  })

  it('keeps the board when a refetch fails, and reports the error', async () => {
    const { result } = renderHook(() => useBoardData(1))
    await finish()
    act(() => { result.current.refresh() })
    await act(async () => { pending!.reject(new Error('down')); await flush() })
    expect(result.current).toMatchObject({ loading: false, refreshing: false, error: 'down' })
    expect(result.current.tasks.map(t => t.id)).toEqual([11])
  })

  it('a project switch is a first load again, never the old project\'s board', async () => {
    const { result, rerender } = renderHook(({ pid }) => useBoardData(pid), { initialProps: { pid: 1 } })
    await finish()
    rerender({ pid: 2 })
    expect(result.current).toMatchObject({ loading: true, refreshing: false, tasks: [], epics: [] })
    await finish()
    expect(result.current.tasks.map(t => t.id)).toEqual([21])
  })

  it('the very first render after a project switch is already a first load', async () => {
    const renders: { pid: number; loading: boolean; tasks: number }[] = []
    const { rerender } = renderHook(({ pid }) => {
      const d = useBoardData(pid)
      renders.push({ pid, loading: d.loading, tasks: d.tasks.length })
      return d
    }, { initialProps: { pid: 1 } })
    await finish()
    rerender({ pid: 2 })
    // Every render for project 2 before its data lands reads as loading, so the
    // Board never paints its empty state in between.
    expect(renders.filter(r => r.pid === 2 && r.tasks === 0).every(r => r.loading)).toBe(true)
    await finish()
  })

  it('a slow earlier load cannot overwrite a newer one', async () => {
    const { result } = renderHook(() => useBoardData(1))
    await finish()
    act(() => { result.current.refresh() })
    const first = pending!
    tasksFor[1] = [task(11, 1), task(12, 1)]
    act(() => { result.current.refresh() })
    await finish()
    expect(result.current.tasks.map(t => t.id)).toEqual([11, 12])
    await act(async () => { first.resolve(); await flush() })
    expect(result.current.tasks.map(t => t.id)).toEqual([11, 12])
    expect(result.current.refreshing).toBe(false)
  })
})

// Loads that never answer time out with an error and a Retry; a late answer
// from an abandoned load is dropped. These drive the mocked api with spies.
describe('useBoardData timeouts', () => {
  beforeEach(() => { vi.useFakeTimers() })
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

  const never = <T,>() => new Promise<T>(() => {})

  it('stops loading with an error when the board does not answer, and Retry loads it', async () => {
    const epics = vi.spyOn(api, 'listEpics').mockImplementationOnce(never)
    vi.spyOn(api, 'listTasks').mockImplementation(never)
    vi.spyOn(api, 'listStories').mockResolvedValue([])
    const { result } = renderHook(() => useBoardData(3))
    expect(result.current.loading).toBe(true)

    await act(() => vi.advanceTimersByTimeAsync(LOAD_TIMEOUT_MS))
    expect(result.current.loading).toBe(false)
    expect(result.current.error).toMatch(/Loading the board took longer than 12 seconds/)

    epics.mockResolvedValue([{ id: 1, name: 'E' } as Epic])
    vi.mocked(api.listTasks).mockResolvedValue([{ id: 9 } as Task])
    await act(async () => { result.current.refresh() })
    expect(result.current.error).toBeNull()
    expect(result.current.loading).toBe(false)
    expect(result.current.epics).toHaveLength(1)
    expect(result.current.tasks).toHaveLength(1)
  })

  it('drops a slow answer that arrives after a newer load', async () => {
    let answerFirst: (v: Epic[]) => void = () => {}
    vi.spyOn(api, 'listEpics')
      .mockImplementationOnce(() => new Promise(r => { answerFirst = r }))
      .mockResolvedValue([{ id: 2, name: 'new' } as Epic])
    vi.spyOn(api, 'listTasks').mockResolvedValue([])
    vi.spyOn(api, 'listStories').mockResolvedValue([])
    const { result } = renderHook(() => useBoardData(3))
    await act(async () => { result.current.refresh() })
    expect(result.current.epics.map(e => e.id)).toEqual([2])
    await act(async () => { answerFirst([{ id: 1, name: 'old' } as Epic]) })
    expect(result.current.epics.map(e => e.id)).toEqual([2])
  })

  it('a refetch that times out after a good load keeps the board and reports the error; Retry clears it', async () => {
    const epics = vi.spyOn(api, 'listEpics').mockResolvedValue([{ id: 1, name: 'E' } as Epic])
    vi.spyOn(api, 'listTasks').mockResolvedValue([{ id: 9 } as Task])
    vi.spyOn(api, 'listStories').mockResolvedValue([])
    const { result } = renderHook(() => useBoardData(3))
    await act(() => vi.advanceTimersByTimeAsync(0))
    expect(result.current.tasks.map(t => t.id)).toEqual([9])

    epics.mockImplementationOnce(never)
    act(() => { result.current.refresh() })
    expect(result.current).toMatchObject({ loading: false, refreshing: true })
    await act(() => vi.advanceTimersByTimeAsync(LOAD_TIMEOUT_MS))
    expect(result.current).toMatchObject({ loading: false, refreshing: false })
    expect(result.current.error).toMatch(/Loading the board took longer/)
    expect(result.current.tasks.map(t => t.id)).toEqual([9])
    expect(result.current.epics.map(e => e.id)).toEqual([1])

    await act(async () => { result.current.refresh() })
    expect(result.current.error).toBeNull()
    expect(result.current.tasks.map(t => t.id)).toEqual([9])
  })
})
