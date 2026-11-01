/**
 * The org's remote control policy (protocol 8.3 "Org policy"):
 * GET / PUT /api/orgs/:id/remote-control with {enabled, idle_timeout_min, max_lifetime_hours, device_pin_days}.
 * Org admins only; the API is authoritative. PUT sends only what changed.
 */
import { RelayError } from './transport'

/**
 * `idle_timeout_min` and `max_lifetime_hours` are 0 for "no limit", the default (#2424).
 * `max_lifetime_hours` is missing from an API that predates it.
 */
export interface OrgRemotePolicy { enabled: boolean; idle_timeout_min: number; max_lifetime_hours?: number; device_pin_days: number }

async function failure(res: Response): Promise<RelayError> {
  const body = await res.json().catch(() => ({})) as { error?: string; code?: string }
  return new RelayError(res.status, body.code ?? `http_${res.status}`, body.error ?? `Request failed (${res.status})`)
}

export async function getOrgRemotePolicy(orgId: number): Promise<OrgRemotePolicy> {
  const res = await fetch(`/api/orgs/${orgId}/remote-control`, { headers: { Accept: 'application/json' } })
  if (!res.ok) throw await failure(res)
  return res.json()
}

export async function setOrgRemotePolicy(orgId: number, patch: Partial<OrgRemotePolicy>): Promise<OrgRemotePolicy> {
  const res = await fetch(`/api/orgs/${orgId}/remote-control`, {
    method: 'PUT', headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: JSON.stringify(patch),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}
