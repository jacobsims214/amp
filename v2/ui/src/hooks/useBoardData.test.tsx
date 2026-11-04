// useBoardData splits a first load (`loading`, nothing on screen) from a
// refetch (`refreshing`, the loaded board stays).
import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
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
