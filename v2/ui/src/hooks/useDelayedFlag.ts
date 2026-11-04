import { useEffect, useRef, useState } from 'react'

/** How long a wait runs before any indicator shows: shorter waits show nothing. */
export const INDICATOR_DELAY_MS = 250

/**
 * Turns "a wait is in flight" into "show the indicator": true only once `flag`
 * has been true for `ms`, so a fast load never flashes a loader. With
 * `minVisible`, an indicator that did appear stays up at least that long, so it
 * does not blink out the moment it shows.
 */
export function useDelayedFlag(flag: boolean, ms = INDICATOR_DELAY_MS, minVisible = 0): boolean {
  const [shown, setShown] = useState(false)
  const shownAt = useRef(0)

  useEffect(() => {
    if (flag && !shown) {
      const t = setTimeout(() => { shownAt.current = Date.now(); setShown(true) }, ms)
      return () => clearTimeout(t)
    }
    if (!flag && shown) {
      const left = Math.max(0, minVisible - (Date.now() - shownAt.current))
      const t = setTimeout(() => setShown(false), left)
      return () => clearTimeout(t)
    }
  }, [flag, shown, ms, minVisible])

  // Without a minimum, hide in the same render the wait ends.
  return shown && (flag || minVisible > 0)
}
