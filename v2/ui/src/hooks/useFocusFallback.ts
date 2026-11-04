import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import { projectLink, type ProjectView } from '../lib/projectLink'
import type { Task } from '../types'

/**
 * What to do when `?focus=<id>` names a ticket that isn't in this project's
 * loaded tickets, so the view would otherwise show nothing:
 *
 * - it belongs to another project → redirect (replace) to the same view there;
 * - the server says it doesn't exist or isn't visible → return its id, so the
 *   page can say so (FocusNotFound) instead of failing silently.
 *
 * Returns null while loading, while looking it up, and when the ticket is here.
 */
export function useFocusFallback(projectId: number, view: ProjectView, focusId: number | null, tasks: Task[], loading: boolean): number | null {
  const navigate = useNavigate()
  const [missing, setMissing] = useState<number | null>(null)
  const lookUp = focusId != null && !loading && !tasks.some(t => t.id === focusId)

  useEffect(() => {
    if (!lookUp || focusId == null) return
    let live = true
    api.getTask(focusId)
      .then(t => {
        if (!live) return
        if (t.project_id !== projectId) navigate(projectLink(t.project_id, view, t.id), { replace: true })
        // Same project but not loaded yet (just created): the next refresh shows it.
      })
      .catch(() => { if (live) setMissing(focusId) })
    return () => { live = false }
  }, [lookUp, focusId, projectId, view, navigate])

  return lookUp && missing === focusId ? focusId : null
}
