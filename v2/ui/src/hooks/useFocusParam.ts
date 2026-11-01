import { useCallback, useEffect } from 'react'
import { useSearchParams } from 'react-router-dom'

/** Old names for ?focus=, still accepted and rewritten in place. */
const ALIASES = ['ticket', 'task'] as const

function parseId(v: string | null): number | null {
  const n = Number(v)
  return Number.isInteger(n) && n > 0 ? n : null
}

/**
 * The focused ticket on a project route, kept in `?focus=<ticket id>`
 * (KB decisions/human-work-ux.md §3).
 *
 * - `?ticket=` and `?task=` are aliases: they're read at once and rewritten
 *   to `?focus=` with replace, so old links keep working.
 * - The param stays in the URL until dismissed, so reload and back work.
 *   Focusing a ticket pushes a history entry (back returns to the previous
 *   ticket, or to none); dismissing removes the param with replace.
 * - Other query params are left alone.
 */
export function useFocusParam(): [number | null, (id: number | null) => void] {
  const [params, setParams] = useSearchParams()
  const alias = ALIASES.find(a => params.has(a))
  const focusId = parseId(params.get('focus')) ?? (alias ? parseId(params.get(alias)) : null)

  useEffect(() => {
    if (!alias) return
    setParams(prev => {
      const next = new URLSearchParams(prev)
      const id = parseId(next.get('focus')) ?? ALIASES.map(a => parseId(next.get(a))).find(v => v != null) ?? null
      ALIASES.forEach(a => next.delete(a))
      if (id != null) next.set('focus', String(id))
      return next
    }, { replace: true })
  }, [alias, setParams])

  const setFocus = useCallback((id: number | null) => {
    if (id === focusId && !alias) return
    setParams(prev => {
      const next = new URLSearchParams(prev)
      ALIASES.forEach(a => next.delete(a))
      if (id != null && id > 0) next.set('focus', String(id))
      else next.delete('focus')
      return next
    }, { replace: id == null })
  }, [focusId, alias, setParams])

  return [focusId, setFocus]
}
