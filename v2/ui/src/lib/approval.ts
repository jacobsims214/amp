/**
 * Plan approval, as the person sees it. Only a person in AMP's web UI can
 * approve a plan (POST /api/projects/{id}/approve-plan from this session);
 * agents, and managers acting for the person over MCP, are refused. These
 * helpers mirror the server's rule so the board can show what waits on the
 * person without polling the plan check.
 */
import type { Project, Task } from '../types'

/** Go's zero time ("0001-01-01T00:00:00Z"): what an unset time.Time encodes as. Not a real plan time. */
export function isZeroTime(iso: string | null | undefined): boolean {
  return !iso || iso.startsWith('0001-01-01')
}

/** When a ticket was last planned: planned_at (#2167), or created_at on an older server (or a zero planned_at). */
export function plannedAt(t: Pick<Task, 'created_at'> & { planned_at?: string }): string {
  return t.planned_at && !isZeroTime(t.planned_at) ? t.planned_at : t.created_at
}

/**
 * An RFC 3339 time as [whole seconds since the epoch, nanoseconds], so two
 * times compare exactly. The server sends microseconds; Date.parse keeps only
 * milliseconds, which would make two different plan times look equal.
 */
function instant(iso: string): [number, number] | null {
  const m = /^(.*T\d{2}:\d{2}:\d{2})(?:[.,](\d+))?(Z|[+-]\d{2}:?\d{2})$/i.exec(iso.trim())
  if (m) {
    const ms = Date.parse(m[1] + m[3])
    if (!Number.isNaN(ms)) return [Math.floor(ms / 1000), Number((m[2] ?? '').padEnd(9, '0').slice(0, 9))]
  }
  const ms = Date.parse(iso)
  return Number.isNaN(ms) ? null : [Math.floor(ms / 1000), (((ms % 1000) + 1000) % 1000) * 1e6]
}

/** Negative, zero or positive as a is before, at or after b, to the nanosecond. Unparseable times sort first. */
export function compareInstants(a: string, b: string): number {
  const x = instant(a), y = instant(b)
  if (!x || !y) return (x ? 1 : 0) - (y ? 1 : 0)
  return x[0] - y[0] || x[1] - y[1]
}

/**
 * The newest plan time among the tickets the person was shown, exactly as the
 * API sent it: the as_of an approval covers (#2192). Never round-trip it
 * through Date, which would drop the microseconds and leave that newest ticket
 * still waiting. Undefined when nothing is shown.
 */
export function newestPlannedAt(tasks: Array<Pick<Task, 'created_at'> & { planned_at?: string }>): string | undefined {
  let newest: string | undefined
  for (const t of tasks) {
    const at = plannedAt(t)
    if (at && (newest === undefined || compareInstants(at, newest) > 0)) newest = at
  }
  return newest
}

/**
 * Open tickets planned since the plan was last approved, oldest plan first.
 * Mirrors domain.Project.AwaitsApproval: never approved, or planned after
 * plan_approved_at. Done tickets don't wait on anyone.
 */
export function awaitingApproval(tasks: Task[], project: Pick<Project, 'plan_approved_at'> | null | undefined): Task[] {
  if (!project) return []
  const approved = project.plan_approved_at && !isZeroTime(project.plan_approved_at) ? project.plan_approved_at : null
  return tasks
    .filter(t => t.state !== 'completed')
    .filter(t => approved == null || compareInstants(plannedAt(t), approved) > 0)
    .sort((a, b) => compareInstants(plannedAt(a), plannedAt(b)) || a.id - b.id)
}

/** What the person hears when their approval didn't cover everything: tickets came in after the page loaded. */
export function changedSinceNotice(n: number): string {
  return `Your approval covered the tickets you saw. ${n} ticket${n === 1 ? ' was' : 's were'} added or changed since this page loaded, so ${n === 1 ? 'it still waits' : 'they still wait'} for your approval. The list is refreshed: review ${n === 1 ? 'it' : 'them'} and approve again.`
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
