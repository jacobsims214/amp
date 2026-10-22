import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import { changedSinceNotice } from '../lib/approval'
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
  // Set when an approval didn't cover everything: tickets arrived after the page loaded.
  const [notice, setNotice] = useState<string | null>(null)

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

  /**
   * Approve what the person was shown: asOf is the newest planned_at among the
   * tickets on screen (newestPlannedAt). Resolves to whether it went through
   * and whether the plan changed under the person (tickets added or re-scoped
   * since the page loaded), in which case the page should refresh its tickets.
   */
  const approve = useCallback(async (asOf?: string): Promise<{ ok: boolean; changed: boolean }> => {
    setApproving(true); setError(null); setNotice(null)
    try {
      const r = await api.approvePlan(projectId, asOf)
      setProject(r.project)
      if (r.awaitingApproval.length > 0) setNotice(changedSinceNotice(r.awaitingApproval.length))
      return { ok: true, changed: r.awaitingApproval.length > 0 }
    } catch (e) {
      // The server stamps rather than refusing (#2192), but if one ever answers
      // 409 (the plan changed), say so and refresh rather than show a failure.
      if ((e as { status?: number })?.status === 409) {
        setNotice('The plan changed since this page loaded, so nothing was approved. The list is refreshed: review it and approve again.')
        api.getProject(projectId).then(setProject).catch(() => {})
        return { ok: false, changed: true }
      }
      setError(e instanceof Error ? e.message : 'Could not approve the plan')
      return { ok: false, changed: false }
    } finally { setApproving(false) }
  }, [projectId])

  return { project, approving, error, notice, approve }
}
