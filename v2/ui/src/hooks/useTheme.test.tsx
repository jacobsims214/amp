import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useTheme } from './useTheme'
import { THEME_KEY } from '../lib/theme'

function mockSystem(dark: boolean) {
  const listeners = new Set<() => void>()
  const mq = {
    get matches() { return dark },
    addEventListener: (_: string, f: () => void) => listeners.add(f),
    removeEventListener: (_: string, f: () => void) => listeners.delete(f),
  }
  window.matchMedia = vi.fn().mockReturnValue(mq) as unknown as typeof window.matchMedia
  return { flip(v: boolean) { dark = v; listeners.forEach(f => f()) } }
}

const attr = () => document.documentElement.getAttribute('data-theme')

describe('useTheme', () => {
  beforeEach(() => { localStorage.clear(); document.documentElement.removeAttribute('data-theme') })

  it('defaults to System and follows prefers-color-scheme', () => {
    mockSystem(true)
    const { result } = renderHook(() => useTheme())
    expect(result.current.pref).toBe('system')
    expect(result.current.resolved).toBe('dark')
    expect(attr()).toBe('dark')
  })

  it('resolves System to light when the system is light', () => {
    mockSystem(false)
    renderHook(() => useTheme())
    expect(attr()).toBe('light')
  })

  it('persists an explicit choice and sets data-theme', () => {
    mockSystem(false)
    const { result, unmount } = renderHook(() => useTheme())
    act(() => result.current.setPref('dark'))
    expect(localStorage.getItem(THEME_KEY)).toBe('dark')
    expect(attr()).toBe('dark')
    unmount()
    const again = renderHook(() => useTheme())
    expect(again.result.current.pref).toBe('dark')
  })

  it('an explicit Light beats a dark system, and System clears the stored choice', () => {
    mockSystem(true)
    const { result } = renderHook(() => useTheme())
    act(() => result.current.setPref('light'))
    expect(attr()).toBe('light')
    act(() => result.current.setPref('system'))
    expect(localStorage.getItem(THEME_KEY)).toBeNull()
    expect(attr()).toBe('dark')
  })

  it('tracks a system change while on System', () => {
    const sys = mockSystem(false)
    renderHook(() => useTheme())
    expect(attr()).toBe('light')
    act(() => sys.flip(true))
    expect(attr()).toBe('dark')
  })
})
