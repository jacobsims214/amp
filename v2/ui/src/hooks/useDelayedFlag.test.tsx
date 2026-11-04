import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useDelayedFlag } from './useDelayedFlag'

beforeEach(() => { vi.useFakeTimers() })
afterEach(() => { vi.useRealTimers() })
const tick = (ms: number) => act(() => { vi.advanceTimersByTime(ms) })

describe('useDelayedFlag', () => {
  it('is false at 249ms and true at 250ms', () => {
    const { result } = renderHook(() => useDelayedFlag(true))
    expect(result.current).toBe(false)
    tick(249)
    expect(result.current).toBe(false)
    tick(1)
    expect(result.current).toBe(true)
  })

  it('never turns on when the wait ends before the delay', () => {
    const { result, rerender } = renderHook(({ on }) => useDelayedFlag(on), { initialProps: { on: true } })
    tick(200)
    rerender({ on: false })
    tick(1000)
    expect(result.current).toBe(false)
  })

  it('drops in the same render the wait ends, without a minimum', () => {
    const { result, rerender } = renderHook(({ on }) => useDelayedFlag(on), { initialProps: { on: true } })
    tick(300)
    expect(result.current).toBe(true)
    rerender({ on: false })
    expect(result.current).toBe(false)
  })

  it('holds for minVisible once shown, so it does not blink', () => {
    const { result, rerender } = renderHook(({ on }) => useDelayedFlag(on, 250, 400), { initialProps: { on: true } })
    tick(260)
    expect(result.current).toBe(true)
    rerender({ on: false })
    tick(389)
    expect(result.current).toBe(true)
    tick(1)
    expect(result.current).toBe(false)
  })

  it('restarts the delay for the next wait', () => {
    const { result, rerender } = renderHook(({ on }) => useDelayedFlag(on), { initialProps: { on: true } })
    tick(300)
    rerender({ on: false })
    rerender({ on: true })
    expect(result.current).toBe(true) // still the same wait as far as the eye can tell
    rerender({ on: false })
    tick(10)
    rerender({ on: true })
    tick(249)
    expect(result.current).toBe(false)
    tick(1)
    expect(result.current).toBe(true)
  })
})
