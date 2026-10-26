import type { AuditEntry, AuditFilter } from '../types'

// Pure helpers for the audit log screens: the query string the list,
// export and verify endpoints share, the before/after diff, and labels.

export interface AuditScope { orgId?: number; projectId?: number; actorId?: number }

/** The query for GET /api/audit and /api/audit/export: the scope, then the filter. */
export function auditQuery(scope: AuditScope, f: AuditFilter = {}, extra: Record<string, string | number | undefined> = {}): URLSearchParams {
  const p = new URLSearchParams()
  if (scope.orgId) p.set('org_id', String(scope.orgId))
  // A project picked in the filter narrows an org's (or the whole) log.
  const project = scope.projectId || f.project_id
  if (project) p.set('project_id', String(project))
  if (scope.actorId) p.set('actor_id', String(scope.actorId))
  const text: (keyof AuditFilter)[] = ['action', 'actor', 'actor_type', 'target_type', 'target_id', 'ip', 'outcome', 'client', 'from', 'to']
  for (const k of text) {
    const v = f[k]
    if (v != null && String(v).trim() !== '') p.set(k, String(v).trim())
  }
  for (const [k, v] of Object.entries(extra)) if (v != null && v !== '') p.set(k, String(v))
  return p
}

/** How many filters are set (the scope doesn't count). */
export function activeFilterCount(f: AuditFilter): number {
  return Object.values(f).filter(v => v != null && String(v).trim() !== '').length
}

export type ExportFormat = 'csv' | 'ndjson'

/** The download link for the current scope and filter. */
export function auditExportUrl(scope: AuditScope, f: AuditFilter, format: ExportFormat): string {
  return `/api/audit/export?${auditQuery(scope, f, { format })}`
}

/** The raw verification report (admins open it in a new tab). */
export function auditVerifyUrl(orgId?: number): string {
  return orgId ? `/api/audit/verify?org_id=${orgId}` : '/api/audit/verify'
}

/** A datetime-local value ("2026-10-26T14:05") as ISO, in the viewer's time zone; '' stays ''. */
export function localToISO(v: string): string {
  if (!v) return ''
  const d = new Date(v)
  return isNaN(d.getTime()) ? '' : d.toISOString()
}

/** ISO back to a datetime-local value, in the viewer's time zone. */
export function isoToLocal(v?: string): string {
  if (!v) return ''
  const d = new Date(v)
  if (isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export interface DiffRow { key: string; before: string; after: string; changed: boolean }

const show = (v: unknown): string => {
  if (v === undefined) return ''
  if (v === null) return 'null'
  if (typeof v === 'string') return v
  return JSON.stringify(v)
}

/** Field-by-field before/after, changed fields first, then the rest; keys sorted within each. */
export function diffValues(before?: Record<string, unknown>, after?: Record<string, unknown>): DiffRow[] {
  const keys = [...new Set([...Object.keys(before ?? {}), ...Object.keys(after ?? {})])].sort()
  const rows = keys.map(key => {
    const b = before && key in before ? show(before[key]) : ''
    const a = after && key in after ? show(after[key]) : ''
    const present = (o: Record<string, unknown> | undefined) => !!o && key in o
    return { key, before: b, after: a, changed: b !== a || present(before) !== present(after) }
  })
  return [...rows.filter(r => r.changed), ...rows.filter(r => !r.changed)]
}

export const ACTOR_TYPE_LABEL: Record<string, string> = { human: 'Person', agent: 'Agent', system: 'AMP', anonymous: 'Not signed in' }

export const CLIENT_LABEL: Record<string, string> = {
  web: 'Web', mcp: 'MCP', harness: 'Harness', api: 'API', relay: 'Relay', cli: 'CLI', system: 'AMP',
}

export const AUTH_METHOD_LABEL: Record<string, string> = {
  dex_session: 'Web sign-in', oauth_mcp: 'MCP sign-in', api_token: 'API token', cluster: 'In-cluster', local_dev: 'Local dev', none: 'None',
}

/** Who did it, as one line: "jo@x.com", or "AMP" for the system. */
export function actorName(e: AuditEntry): string {
  if (e.actor_email) return e.actor_email
  return e.actor_type === 'anonymous' ? 'Someone not signed in' : 'AMP'
}

/** The target as "project 12 · Apollo". */
export function targetLabel(e: AuditEntry): string {
  const id = [e.target_type, e.target_id].filter(Boolean).join(' ')
  return [id, e.target_name].filter(Boolean).join(' · ')
}

/** True when the entry has anything the expanded view shows. */
export function hasDetails(e: AuditEntry): boolean {
  return !!(e.source_ip || e.user_agent || e.request_id || e.auth_method || e.reason || e.before || e.after ||
    (e.details && Object.keys(e.details).length) || e.chain_key || e.target_type)
}
