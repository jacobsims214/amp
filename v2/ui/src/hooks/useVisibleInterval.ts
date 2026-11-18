import { useEffect, useEffectEvent } from 'react'

const isHidden = () => document.visibilityState === 'hidden'

/**
 * Calls `callback` every `intervalMs` while the tab is visible. Ticks that come
 * due while the tab is hidden are skipped, so a background tab does not keep
 * polling. When the tab comes back after one or more skipped ticks, `callback`
 * runs once straight away and the interval restarts from that call. Coming
 * back before any tick was skipped calls nothing extra.
 *
 * `intervalMs` of null pauses it. The latest `callback` is always the one
 * called, so passing a new function each render does not reset the timer.
 */
export function useVisibleInterval(callback: () => void, intervalMs: number | null): void {
  const run = useEffectEvent(callback)

  useEffect(() => {
    if (intervalMs === null) return
    let skipped = false
    const tick = () => {
      if (isHidden()) { skipped = true; return }
      run()
    }
    let timer = setInterval(tick, intervalMs)

    const onVisibilityChange = () => {
      if (isHidden() || !skipped) return
      skipped = false
      run()
      // Restart so the next tick is a full interval after the catch-up call,
      // and an overdue tick from a suspended timer cannot run it twice.
      clearInterval(timer)
      timer = setInterval(tick, intervalMs)
    }
    document.addEventListener('visibilitychange', onVisibilityChange)

    return () => {
      clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [intervalMs])
}
