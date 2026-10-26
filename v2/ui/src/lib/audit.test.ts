import { describe, expect, it } from 'vitest'
import { activeFilterCount, auditExportUrl, auditQuery, auditVerifyUrl, diffValues, isoToLocal, localToISO, actorName, targetLabel } from './audit'
import type { AuditEntry } from '../types'

describe('auditQuery', () => {
  it('puts the scope first and leaves out empty filters', () => {
    const q = auditQuery({ orgId: 4 }, { action: 'plan.', outcome: 'denied', actor: ' ', ip: '203.0.113.0/24', client: '' }, { limit: 50, before: undefined })
    expect(q.toString()).toBe('org_id=4&action=plan.&ip=203.0.113.0%2F24&outcome=denied&limit=50')
  })
  it('lets a project picked in the filter narrow an org, but never widens a project scope', () => {
    expect(auditQuery({ orgId: 4 }, { project_id: 9 }).get('project_id')).toBe('9')
    expect(auditQuery({ projectId: 3 }, { project_id: 9 }).get('project_id')).toBe('3')
  })
  it('keeps the actor scope of a person', () => {
    expect(auditQuery({ actorId: 12 }, { from: '2026-10-01T00:00:00.000Z' }).toString()).toBe('actor_id=12&from=2026-10-01T00%3A00%3A00.000Z')
  })
})

describe('export and verify links', () => {
  it('export the current filter in the chosen format', () => {
    expect(auditExportUrl({ orgId: 2 }, { outcome: 'denied' }, 'ndjson')).toBe('/api/audit/export?org_id=2&outcome=denied&format=ndjson')
    expect(auditExportUrl({}, {}, 'csv')).toBe('/api/audit/export?format=csv')
  })
  it('verify an org, or every chain', () => {
    expect(auditVerifyUrl(7)).toBe('/api/audit/verify?org_id=7')
    expect(auditVerifyUrl()).toBe('/api/audit/verify')
  })
})

describe('activeFilterCount', () => {
  it('counts only filters with a value', () => {
    expect(activeFilterCount({})).toBe(0)
    expect(activeFilterCount({ action: '', actor: 'jo', project_id: 3, outcome: 'failed' })).toBe(3)
  })
})

describe('datetime-local round trip', () => {
  it('converts to ISO and back in the local zone', () => {
    const iso = localToISO('2026-10-26T14:05')
    expect(iso).toMatch(/Z$/)
    expect(isoToLocal(iso)).toBe('2026-10-26T14:05')
    expect(localToISO('')).toBe('')
    expect(isoToLocal('nonsense')).toBe('')
  })
})

describe('diffValues', () => {
  it('lists changed fields first, with added and removed ones', () => {
    const rows = diffValues({ role: 'member', name: 'A', gone: true }, { role: 'admin', name: 'A', added: [1, 2] })
    expect(rows.map(r => [r.key, r.before, r.after, r.changed])).toEqual([
      ['added', '', '[1,2]', true],
      ['gone', 'true', '', true],
      ['role', 'member', 'admin', true],
      ['name', 'A', 'A', false],
    ])
  })
  it('handles a create (no before) and a delete (no after)', () => {
    expect(diffValues(undefined, { a: 1 })).toEqual([{ key: 'a', before: '', after: '1', changed: true }])
    expect(diffValues({ a: null }, undefined)).toEqual([{ key: 'a', before: 'null', after: '', changed: true }])
    expect(diffValues()).toEqual([])
  })
})

describe('labels', () => {
  const e = (over: Partial<AuditEntry>): AuditEntry => ({ id: 1, at: '2026-10-26T12:00:00Z', actor_email: '', action: 'x', summary: '', ...over })
  it('names the actor', () => {
    expect(actorName(e({ actor_email: 'jo@x.com' }))).toBe('jo@x.com')
    expect(actorName(e({ actor_type: 'system' }))).toBe('AMP')
    expect(actorName(e({ actor_type: 'anonymous' }))).toBe('Someone not signed in')
  })
  it('describes the target', () => {
    expect(targetLabel(e({ target_type: 'project', target_id: '12', target_name: 'Apollo' }))).toBe('project 12 · Apollo')
    expect(targetLabel(e({}))).toBe('')
  })
})
