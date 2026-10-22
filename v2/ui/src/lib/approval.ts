/**
 * Plan approval, as the person sees it. Only a person in AMP's web UI can
 * approve a plan (POST /api/projects/{id}/approve-plan from this session);
 * agents, and managers acting for the person over MCP, are refused. These
 * helpers mirror the server's rule so the board can show what waits on the
 * person without polling the plan check.
 */
import type { Project, Task } from '../types'

/** When a ticket was last planned: planned_at (#2167), or created_at on an older server. */
export function plannedAt(t: Pick<Task, 'created_at'> & { planned_at?: string }): string {
  return t.planned_at || t.created_at
}

/**
 * Open tickets planned since the plan was last approved, oldest plan first.
 * Mirrors domain.Project.AwaitsApproval: never approved, or planned after
 * plan_approved_at. Done tickets don't wait on anyone.
 */
export function awaitingApproval(tasks: Task[], project: Pick<Project, 'plan_approved_at'> | null | undefined): Task[] {
  if (!project) return []
  const approved = project.plan_approved_at ? Date.parse(project.plan_approved_at) : null
  return tasks
    .filter(t => t.state !== 'completed')
    .filter(t => approved == null || Date.parse(plannedAt(t)) > approved)
    .sort((a, b) => Date.parse(plannedAt(a)) - Date.parse(plannedAt(b)) || a.id - b.id)
}

/** The marker the server appends when a manager passes on its person's answer over MCP. */
export const RELAYED_SUFFIX = ' (relayed by agent)'

/** Whether a comment author or history actor was relayed by an agent rather than typed in the UI. */
export function isRelayed(who: string | null | undefined): boolean {
  return !!who && who.trimEnd().endsWith(RELAYED_SUFFIX.trim())
}

/** The person behind a relayed author ("a@b.c (relayed by agent)" -> "a@b.c"). */
export function relayedFor(who: string): string {
  const s = who.trimEnd()
  return isRelayed(s) ? s.slice(0, s.length - RELAYED_SUFFIX.trim().length).trimEnd() : s
}

/** The Flight plan, where the person approves. Agents' approve_url points here. */
export const planPath = (projectId: number) => `/project/${projectId}/plan`

/** "Wed, Oct 22, 09:14 PM": when the plan was last approved. */
export function formatWhen(iso: string | null | undefined): string | null {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  return d.toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

/** "just now", "5m ago", "3h ago", "2d ago". */
export function ago(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return ''
  const mins = Math.floor((now - Date.parse(iso)) / 60000)
  if (Number.isNaN(mins)) return ''
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.floor(mins / 60)
  if (hrs < 48) return `${hrs}h ago`
  return `${Math.floor(hrs / 24)}d ago`
}
