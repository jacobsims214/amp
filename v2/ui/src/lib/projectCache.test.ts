import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { api } from '../api/client'
import type { Project } from '../types'
import { loadProject, primeProjects, resetProjectCache, useProject } from './projectCache'

const proj = (id: number, code = 'BA'): Project => ({ id, name: `P${id}`, code, description: '', state: 'active' } as Project)

describe('projectCache', () => {
  beforeEach(() => resetProjectCache())
  afterEach(() => vi.restoreAllMocks())

  it('shares one request between concurrent readers and serves later readers from cache', async () => {
    const spy = vi.spyOn(api, 'getProject').mockResolvedValue(proj(3))
    const a = renderHook(() => useProject(3))
    const b = renderHook(() => useProject(3))
    await waitFor(() => expect(a.result.current?.code).toBe('BA'))
    expect(b.result.current?.code).toBe('BA')
    const c = renderHook(() => useProject(3))
    expect(c.result.current?.code).toBe('BA') // first frame, from cache
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('renders primed projects without a request', () => {
    const spy = vi.spyOn(api, 'getProject').mockResolvedValue(proj(4))
    act(() => primeProjects([proj(4, 'XY')]))
    const { result } = renderHook(() => useProject(4))
    expect(result.current?.code).toBe('XY')
    expect(spy).not.toHaveBeenCalled()
  })

  it('keeps the cached project when a re-read fails', async () => {
    act(() => primeProjects([proj(5, 'OK')]))
    vi.spyOn(api, 'getProject').mockRejectedValue(new Error('down'))
    expect((await loadProject(5))?.code).toBe('OK')
  })
})
