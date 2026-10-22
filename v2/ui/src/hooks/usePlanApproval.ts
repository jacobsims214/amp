import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import { useSSE } from './useSSE'
import type { Project, SSEEvent } from '../types'

/**
 * The project's approval state, kept live (project.updated arrives when
 * anyone approves), and the person's one-click approve. Approving is only
 * accepted from this web UI session; the server refuses agents and MCP.
 */
export function usePlanApproval(projectId: number) {
  const [project, setProject] = useState<Project | null>(null)
  const [approving, setApproving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    api.getProject(projectId).then(p => { if (live) setProject(p) }).catch(() => {})
    return () => { live = false }
  }, [projectId])

  useSSE(projectId, useCallback((e: SSEEvent) => {
    if (e.type !== 'project.updated') return
    const p = e.payload as Project | undefined
    if (p && p.id === projectId) setProject(prev => ({ ...prev, ...p }))
  }, [projectId]))

  const approve = useCallback(async () => {
    setApproving(true); setError(null)
    try {
      const p = await api.approvePlan(projectId)
      setProject(p)
      return p
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not approve the plan')
      return null
    } finally { setApproving(false) }
  }, [projectId])

  return { project, approving, error, approve }
}
