import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import { changedSince, changedSinceNotice } from '../lib/approval'
import { useSSE } from './useSSE'
import type { ApproveRequest, Project, SSEEvent } from '../types'

/**
 * The project's approval state, kept live (project.updated arrives when
 * anyone approves), and the person's approve (their own tickets, or others'
 * on their behalf). Approving is only accepted from this web UI session;
 * the server refuses agents and MCP.
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
   * Approve the tickets the person ticked (#2198): taskIds, asOf = the newest
   * planned_at among them, onBehalf when any isn't theirs. shown is every ticket
   * id the page showed, so own tickets that arrived after it loaded (or that
   * changed under the person) are reported in the notice. Resolves to whether
   * it went through and whether the page should refresh its tickets.
   */
  const approve = useCallback(async (req: ApproveRequest & { shown?: number[] } = {}): Promise<{ ok: boolean; changed: boolean }> => {
    setApproving(true); setError(null); setNotice(null)
    const { shown, ...body } = req
    try {
      const r = await api.approvePlan(projectId, body)
      setProject(r.project)
      const changed = changedSince(r, shown ?? body.taskIds ?? [])
      if (changed.length > 0) setNotice(changedSinceNotice(changed.length))
      // Refresh whenever something was approved too, so each ticket shows its approver.
      return { ok: true, changed: changed.length > 0 || r.approved.length > 0 }
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
