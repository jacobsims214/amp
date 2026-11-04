import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { api } from '../api/client'
import { LOAD_TIMEOUT_MS } from '../lib/timeout'
import { useBoardData } from './useBoardData'
import type { Epic, Task } from '../types'

class QuietEventSource {
  onmessage = null
  onerror = null
  readyState = 0
  close() {}
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.stubGlobal('EventSource', QuietEventSource)
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const never = <T,>() => new Promise<T>(() => {})

describe('useBoardData', () => {
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
})
