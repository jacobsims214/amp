import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useVisibleInterval } from './useVisibleInterval'

let visibility: DocumentVisibilityState = 'visible'

beforeEach(() => {
  vi.useFakeTimers()
  visibility = 'visible'
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
})
afterEach(() => {
  vi.useRealTimers()
  // Drop the own-property override so jsdom's prototype getter shows through again.
  delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
})

const tick = (ms: number) => act(() => { vi.advanceTimersByTime(ms) })
const setVisibility = (state: DocumentVisibilityState) => act(() => {
  visibility = state
  document.dispatchEvent(new Event('visibilitychange'))
})

describe('useVisibleInterval', () => {
  it('calls the callback every interval while the tab is visible', () => {
    const cb = vi.fn()
    renderHook(() => useVisibleInterval(cb, 1000))
    expect(cb).not.toHaveBeenCalled()
    tick(999)
    expect(cb).not.toHaveBeenCalled()
    tick(1)
    expect(cb).toHaveBeenCalledTimes(1)
    tick(2000)
    expect(cb).toHaveBeenCalledTimes(3)
  })

  it('makes no calls while the tab is hidden', () => {
    const cb = vi.fn()
    renderHook(() => useVisibleInterval(cb, 1000))
    setVisibility('hidden')
    tick(5000)
    expect(cb).not.toHaveBeenCalled()
  })

  it('catches up with exactly one immediate call when it comes back after skipped ticks', () => {
    const cb = vi.fn()
    renderHook(() => useVisibleInterval(cb, 1000))
    setVisibility('hidden')
    tick(3500) // three ticks skipped
    expect(cb).not.toHaveBeenCalled()
    setVisibility('visible')
    expect(cb).toHaveBeenCalledTimes(1)
    // The interval restarts from the catch-up call: nothing at the old 4000ms mark.
    tick(999)
    expect(cb).toHaveBeenCalledTimes(1)
    tick(1)
    expect(cb).toHaveBeenCalledTimes(2)
  })

  it('makes no extra call when it comes back before any tick was skipped', () => {
    const cb = vi.fn()
    renderHook(() => useVisibleInterval(cb, 1000))
    tick(1000)
    expect(cb).toHaveBeenCalledTimes(1)
    setVisibility('hidden')
    tick(500)
    setVisibility('visible')
    expect(cb).toHaveBeenCalledTimes(1)
    tick(500) // the regular 2000ms tick
    expect(cb).toHaveBeenCalledTimes(2)
  })

  it('catches up once per hidden spell', () => {
    const cb = vi.fn()
    renderHook(() => useVisibleInterval(cb, 1000))
    setVisibility('hidden')
    tick(2000)
    setVisibility('visible')
    setVisibility('visible') // a second visible event with no new skip adds nothing
    expect(cb).toHaveBeenCalledTimes(1)
    setVisibility('hidden')
    tick(1000)
    setVisibility('visible')
    expect(cb).toHaveBeenCalledTimes(2)
  })

  it('pauses while intervalMs is null and resumes when it is set', () => {
    const cb = vi.fn()
    const { rerender } = renderHook(({ ms }) => useVisibleInterval(cb, ms), {
      initialProps: { ms: null as number | null },
    })
    tick(5000)
    setVisibility('hidden')
    setVisibility('visible')
    expect(cb).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)

    rerender({ ms: 1000 })
    tick(1000)
    expect(cb).toHaveBeenCalledTimes(1)

    rerender({ ms: null })
    tick(5000)
    expect(cb).toHaveBeenCalledTimes(1)
  })

  it('calls the latest callback without resetting the timer', () => {
    const first = vi.fn()
    const second = vi.fn()
    const { rerender } = renderHook(({ cb }) => useVisibleInterval(cb, 1000), {
      initialProps: { cb: first },
    })
    tick(600)
    rerender({ cb: second })
    tick(400) // the original 1000ms tick, not 600 + 1000
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
  })

  it('clears its timer and visibility listener on unmount', () => {
    const cb = vi.fn()
    const add = vi.spyOn(document, 'addEventListener')
    const remove = vi.spyOn(document, 'removeEventListener')
    const { unmount } = renderHook(() => useVisibleInterval(cb, 1000))
    const listener = add.mock.calls.find(([type]) => type === 'visibilitychange')?.[1]
    expect(listener).toBeDefined()
    expect(vi.getTimerCount()).toBe(1)

    setVisibility('hidden')
    tick(2000) // a skipped tick is pending a catch-up
    unmount()

    expect(remove).toHaveBeenCalledWith('visibilitychange', listener)
    expect(vi.getTimerCount()).toBe(0)
    setVisibility('visible')
    tick(5000)
    expect(cb).not.toHaveBeenCalled()
  })
})
