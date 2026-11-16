/**
 * Fixtures for the team inbox queue tests (AMP #3978, build slice J1).
 *
 * A `TeamRequest` has a lot of fields and a test that builds one by hand in
 * each case drifts: one case forgets `rev`, another forgets `claim_user_id`,
 * and a test that meant to prove "an unclaimed row offers Claim" passes against
 * a row that is claimed. So the base row is here once, with the fields the
 * queue actually reads, and a test overrides only what it is about.
 */
import type { RequestQueue, RequestStateTotals, RequestStatus, TeamRequest } from '../types'
import { REQUEST_STATUSES } from '../types'

/** A request in the queue, unclaimed and unassigned unless the test says otherwise. */
export function request(over: Partial<TeamRequest> = {}): TeamRequest {
  return {
    id: 7,
    public_ref: '8f2q1x7k4m2p9t5w',
    org_id: 2,
    team_id: 4,
    requester_user_id: 12,
    requester_contact: 'it-admin@acme.example',
    source_kind: 'web',
    source_external_id: '',
    subject: 'SSO group mapping for the Berlin office',
    input_brief: "We're rolling out SSO for the Berlin office.",
    outcome_brief: '',
    state: 'new',
    claim_user_id: null,
    claimed_at: null,
    owner_user_id: null,
    duplicate_of: null,
    reject_reason: '',
    needs_info_since: null,
    planned_at: null,
    delivered_at: null,
    accepted_at: null,
    accepted_by: null,
    closed_at: null,
    reopened_at: null,
    reopened_count: 0,
    rev: 3,
    metadata: {},
    created_at: '2026-11-14T09:41:00Z',
    updated_at: '2026-11-14T09:41:00Z',
    ...over,
  }
}

/** Per-state counts with all nine keys present and zeroed but for `states`. */
export function totals(states: Partial<Record<RequestStatus, number>> = {}): RequestStateTotals {
  const full = Object.fromEntries(REQUEST_STATUSES.map(s => [s, states[s] ?? 0])) as Record<RequestStatus, number>
  return { total: Object.values(full).reduce((a, b) => a + b, 0), states: full }
}

/** A queue answer around `items`, with counts that agree with them unless a test overrides. */
export function queue(items: TeamRequest[], over: Partial<RequestQueue> = {}): RequestQueue {
  const byState = totals(
    items.reduce<Partial<Record<RequestStatus, number>>>((acc, r) => {
      acc[r.state] = (acc[r.state] ?? 0) + 1
      return acc
    }, {}),
  )
  return { items, count: items.length, totals: byState, ...over }
}