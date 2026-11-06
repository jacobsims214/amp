import { useCallback, useEffect, useState } from 'react'
import { applyTheme, readPref, resolve, writePref, type ResolvedTheme, type ThemePref } from '../lib/theme'

/** The theme preference (System / Light / Dark) and the theme it resolves to. */
export function useTheme(): { pref: ThemePref; resolved: ResolvedTheme; setPref: (p: ThemePref) => void } {
  const [pref, setPrefState] = useState<ThemePref>(readPref)
  const [resolved, setResolved] = useState<ResolvedTheme>(() => resolve(readPref()))

  useEffect(() => {
    const sync = () => { const r = resolve(pref); setResolved(r); applyTheme(r) }
    sync()
    if (pref !== 'system' || !window.matchMedia) return
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    mq.addEventListener('change', sync)
    return () => mq.removeEventListener('change', sync)
  }, [pref])

  // Another tab changed it.
  useEffect(() => {
    const onStorage = () => setPrefState(readPref())
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  const setPref = useCallback((p: ThemePref) => { writePref(p); setPrefState(p) }, [])
  return { pref, resolved, setPref }
}
