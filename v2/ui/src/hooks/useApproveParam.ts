import { useCallback } from 'react'
import { useSearchParams } from 'react-router-dom'

/** Which epics the Approve view shows: ids in the order asked, or every epic awaiting you. */
export type ApproveSet = number[] | 'all'

/** `?approve=172,2` → [172, 2]; `?approve=all` → 'all'; anything else → null. */
export function parseApprove(v: string | null): ApproveSet | null {
  if (!v) return null
  if (v.trim().toLowerCase() === 'all') return 'all'
  const ids = v.split(',').map(s => Number(s.trim()))
  return ids.length > 0 && ids.every(n => Number.isInteger(n) && n > 0) ? ids : null
}

export function formatApprove(set: ApproveSet): string {
  return set === 'all' ? 'all' : set.join(',')
}

/**
 * The Approve view's route state in My work (KB decisions/human-work-ux.md §2):
 * `/project/:id/my-work?approve=<epic id>[,<epic id>…]&focus=<ticket id>`.
 *
 * - `open(set, focus?)` pushes a history entry, so Back returns to the list.
 * - `close()` removes approve and focus with a push too ("← My work").
 * - `focus(id, {replace})` moves the reader; J/K pass replace so stepping
 *   through tickets doesn't fill the history.
 * Other query params are left alone.
 */
export function useApproveParam() {
  const [params, setParams] = useSearchParams()
  const approve = parseApprove(params.get('approve'))
  const fv = Number(params.get('focus'))
  const focusId = Number.isInteger(fv) && fv > 0 ? fv : null

  const open = useCallback((set: ApproveSet, focus?: number | null) => {
    setParams(prev => {
      const next = new URLSearchParams(prev)
      next.set('approve', formatApprove(set))
      if (focus != null && focus > 0) next.set('focus', String(focus)); else next.delete('focus')
      return next
    })
  }, [setParams])

  const close = useCallback((focus?: number | null) => {
    setParams(prev => {
      const next = new URLSearchParams(prev)
      next.delete('approve')
      if (focus != null && focus > 0) next.set('focus', String(focus)); else next.delete('focus')
      return next
    })
  }, [setParams])

  const focus = useCallback((id: number | null, opts: { replace?: boolean } = {}) => {
    setParams(prev => {
      const next = new URLSearchParams(prev)
      if (id != null && id > 0) next.set('focus', String(id)); else next.delete('focus')
      return next
    }, { replace: opts.replace ?? id == null })
  }, [setParams])

  return { approve, focusId, open, close, focus }
}
