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
 * (v2/docs/url-state.md).
 *
 * - `?ticket=` and `?task=` are aliases: they're read at once and rewritten
 *   to `?focus=` with replace, so old links keep working.
 * - The param stays in the URL until dismissed, so reload and back work.
 *   Focusing a ticket pushes a history entry (back returns to the previous
 *   ticket, or to none); dismissing removes the param with replace.
 * - Other query params are left alone, except the drawer's `?tab=`: it
 *   belongs to the focused ticket, so focusing another one or dismissing it
 *   drops the tab too (the next ticket opens on its own default tab).
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
      next.delete('tab')
      if (id != null && id > 0) next.set('focus', String(id))
      else next.delete('focus')
      return next
    }, { replace: id == null })
  }, [focusId, alias, setParams])

  return [focusId, setFocus]
}
