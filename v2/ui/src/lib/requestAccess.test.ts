// lib/requestAccess (AMP #3982): the decisions the submit-access panel makes
// before it renders anything.
//
// The three criteria again, at the level where they are cheapest to get right:
// the manager gate mirrors `authz.RequireTeamManager` exactly (so the panel does
// not offer a control the server would refuse, and does not hide one it would
// allow); a token belongs in this inbox's credential list only when it is
// submit-scoped *and* bound here; and the grant listing order answers "who can
// send" before "who was taken away".
import { describe, expect, it } from 'vitest'
import {
  activeGrantCount, canManageSubmitAccess, cannotManageReason, credentialExpired, dayLabel, grantRows,
  grantState, granteeName, isSubmitScopedToken, submitScopedTokens, tokenPrefix,
} from './requestAccess'
import type { APIToken, Me, Team, TeamSubmitGrantee } from '../types'

const TEAM: Team = { id: 4, org_id: 2, name: 'Platform', created_by: 11, created_at: '2026-10-01T09:00:00Z' }

const me = (over: Partial<Me> = {}): Me => ({
  subject: 'dex|me', email: 'me@simstech.cloud', name: 'Me', roles: [], user_id: 11, kind: 'human', ...over,
})

const org = (role: 'owner' | 'admin' | 'member' | '') =>
  ({ id: 2, name: 'simstech', slug: 'simstech', created_by: 11, created_at: '2026-10-01T09:00:00Z', my_role: role })

const grant = (over: Partial<TeamSubmitGrantee> = {}): TeamSubmitGrantee => ({
  team_id: 4, org_id: 2, user_id: 31, granted_at: '2026-11-13T10:00:00Z', email: 'sam@outside.example', ...over,
})

const token = (over: Partial<APIToken> = {}): APIToken => ({
  id: 41, user_id: 11, name: 'connector', prefix: 'amp_ab12', created_at: '2026-11-13T09:00:00Z', ...over,
})

describe('canManageSubmitAccess mirrors RequireTeamManager', () => {
  it('passes for a lead of this team, and not for a lead of another one', () => {
    expect(canManageSubmitAccess(me({ teams: [{ id: 4, name: 'Platform', org_id: 2, role: 'lead' }] }), TEAM)).toBe(true)
    expect(canManageSubmitAccess(me({ teams: [{ id: 9, name: 'Other', org_id: 2, role: 'lead' }] }), TEAM)).toBe(false)
  })

  it('passes for an owner or admin of the team’s own org, and not of another org', () => {
    for (const role of ['owner', 'admin'] as const) {
      expect(canManageSubmitAccess(me({ orgs: [org(role)] }), TEAM)).toBe(true)
    }
    expect(canManageSubmitAccess(me({ orgs: [org('member')] }), TEAM)).toBe(false)
    expect(canManageSubmitAccess(me({ orgs: [{ ...org('owner'), id: 7 }] }), TEAM)).toBe(false)
  })

  it('passes for a global admin and for the local-dev identity', () => {
    expect(canManageSubmitAccess(me({ roles: ['admin'] }), TEAM)).toBe(true)
    expect(canManageSubmitAccess(me({ local_dev: true }), TEAM)).toBe(true)
  })

  it('refuses a plain team member, a signed-out viewer and a team that has not loaded', () => {
    expect(canManageSubmitAccess(me({ teams: [{ id: 4, name: 'Platform', org_id: 2, role: 'member' }] }), TEAM)).toBe(false)
    expect(canManageSubmitAccess(null, TEAM)).toBe(false)
    expect(canManageSubmitAccess(me({ orgs: [org('owner')] }), null)).toBe(false)
  })

  it('states who can, rather than only that the viewer cannot', () => {
    expect(cannotManageReason(me())).toMatch(/Only this team’s leads and its org’s owners and admins can change who may submit/)
    expect(cannotManageReason(null)).toMatch(/Sign in to see who can send requests/)
  })
})

describe('a credential belongs to this inbox only when it is submit-scoped and bound here', () => {
  it('accepts a live submit-scoped token of this team', () => {
    expect(isSubmitScopedToken(token({ scope: 'submit', scope_team_id: 4 }), 4)).toBe(true)
  })

  it('refuses a full-scope token, another team’s token, and a token with no scope recorded', () => {
    expect(isSubmitScopedToken(token({ scope: 'full', scope_team_id: null }), 4)).toBe(false)
    expect(isSubmitScopedToken(token({ scope: 'submit', scope_team_id: 9 }), 4)).toBe(false)
    expect(isSubmitScopedToken(token(), 4)).toBe(false) // an older API: absent scope reads as full
  })

  it('drops revoked tokens from the list and keeps the rest in order', () => {
    const list = submitScopedTokens([
      token({ id: 1, scope: 'submit', scope_team_id: 4 }),
      token({ id: 2, scope: 'submit', scope_team_id: 4, revoked_at: '2026-11-14T09:00:00Z' }),
      token({ id: 3, scope: 'full', scope_team_id: null }),
      token({ id: 4, scope: 'submit', scope_team_id: 4 }),
    ], 4)
    expect(list.map(t => t.id)).toEqual([1, 4])
  })

  it('names a moment as people say it, in the past or the future', () => {
    const now = Date.parse('2026-11-14T12:00:00Z')
    expect(dayLabel(undefined, now)).toBe('never')
    expect(dayLabel('not a date', now)).toBe('never')
    expect(dayLabel('2026-11-14T11:59:30Z', now)).toBe('just now')
    expect(dayLabel('2026-11-14T11:41:00Z', now)).toBe('19m ago')
    expect(dayLabel('2026-11-14T06:00:00Z', now)).toBe('6h ago')
    // A future moment is a date, not a timestamp: `ago` would print
    // "2/11/2027, 12:24:27 PM", which reads like an error on an expiry row.
    expect(dayLabel('2027-02-11T09:00:00Z', now)).toMatch(/2027/)
    expect(dayLabel('2027-02-11T09:00:00Z', now)).not.toMatch(/:/)
    expect(dayLabel('2026-11-12T10:00:00Z', now)).toMatch(/2026/)
  })

  it('labels an expired credential from a clock the caller supplies', () => {
    const at = Date.parse('2026-11-14T12:00:00Z')
    expect(credentialExpired(token({ expires_at: '2026-11-14T11:59:59Z' }), at)).toBe(true)
    expect(credentialExpired(token({ expires_at: '2026-11-14T12:00:01Z' }), at)).toBe(false)
    expect(credentialExpired(token(), at)).toBe(false) // no expiry is not expired
  })

  it('shows a prefix, never a secret, and still names a token that has none', () => {
    expect(tokenPrefix(token({ prefix: 'amp_ab12' }))).toBe('amp_ab12…')
    expect(tokenPrefix(token({ prefix: '', id: 77 }))).toBe('#77')
  })
})

describe('the grant listing puts who can send first', () => {
  it('orders in force before revoked, keeping each group’s order', () => {
    const rows = grantRows([
      grant({ user_id: 1, revoked_at: '2026-11-12T10:00:00Z' }),
      grant({ user_id: 2 }),
      grant({ user_id: 3, revoked_at: '2026-11-11T10:00:00Z' }),
      grant({ user_id: 4 }),
    ])
    expect(rows.map(g => g.user_id)).toEqual([2, 4, 1, 3])
    expect(rows.map(grantState)).toEqual(['active', 'active', 'revoked', 'revoked'])
    expect(activeGrantCount(rows)).toBe(2)
  })

  it('counts nothing for a null listing and names a person however the row identifies them', () => {
    expect(activeGrantCount(null)).toBe(0)
    expect(grantRows(null)).toEqual([])
    expect(granteeName(grant({ display_name: 'Sam Rivera' }))).toBe('Sam Rivera')
    expect(granteeName(grant({ display_name: '', email: 'sam@outside.example' }))).toBe('sam@outside.example')
    expect(granteeName(grant({ display_name: ' ', email: '', user_id: 42 }))).toBe('User #42')
  })
})
