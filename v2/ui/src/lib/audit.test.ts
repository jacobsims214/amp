import { describe, expect, it } from 'vitest'
import { activeFilterCount, auditExportUrl, auditQuery, auditVerifyUrl, diffValues, isoToLocal, localToISO, actorName, targetLabel, stoppedAuditStreams, verifyVerdict } from './audit'
import type { AuditEntry, AuditVerifyReport, Webhook } from '../types'

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

describe('verifyVerdict', () => {
  const chain = (over: Partial<AuditVerifyReport['chains'][number]> = {}) =>
    ({ chain_key: 'global', ok: true, rows: 5, head_seq: 5, checkpoints_matched: 1, signed_checkpoints: 1, problems: [], ...over })
  const rep = (over: Partial<AuditVerifyReport> = {}): AuditVerifyReport =>
    ({ ok: true, status: 'ok', checked_at: '2026-10-26T12:00:00Z', signing: { configured: true, key_id: 'k' }, chains: [chain()], ...over })
  it('follows the server status', () => {
    expect(verifyVerdict(rep()).status).toBe('ok')
    const v = verifyVerdict(rep({ ok: false, status: 'unproven', chains: [chain({ ok: false, status: 'unproven', warnings: ['unproven: no signed checkpoint covers this chain yet (they are written hourly)'] })] }))
    expect(v).toMatchObject({ status: 'unproven', problems: 0, rows: 5, reasons: ['no signed checkpoint covers this chain yet (they are written hourly)'] })
    expect(verifyVerdict(rep({ ok: false, status: 'failed', chains: [chain({ problems: [{ kind: 'altered', detail: 'x' }] })] }))).toMatchObject({ status: 'failed', problems: 1 })
  })
  it('never calls an unsigned chain from an older server ok', () => {
    expect(verifyVerdict(rep({ status: undefined, signing: { configured: false } }))).toMatchObject({ status: 'unproven', reasons: ['no signing key is configured'] })
    expect(verifyVerdict(rep({ status: undefined })).status).toBe('ok')
  })
})

describe('stoppedAuditStreams', () => {
  const hook = (over: Partial<Webhook>): Webhook => ({ id: 1, name: 'SIEM', url: 'https://siem.example', format: 'json', events: ['audit.*'], active: true,
    created_at: '', updated_at: '', consecutive_failures: 0, ...over })
  it('lists audit webhooks that are off or failing, and ignores healthy and non-audit ones', () => {
    const got = stoppedAuditStreams([
      hook({ id: 1 }),
      hook({ id: 2, active: false, consecutive_failures: 20 }),
      hook({ id: 3, consecutive_failures: 3, last_error: 'HTTP 503' }),
      hook({ id: 4, events: ['task.completed'], active: false }),
      hook({ id: 5, instance: true, events: [], consecutive_failures: 1 }),
    ])
    expect(got.map(s => [s.hook.id, s.why])).toEqual([
      [2, 'turned off after 20 failed deliveries'],
      [3, '3 deliveries in a row failed: HTTP 503'],
      [5, '1 deliveries in a row failed'],
    ])
  })
})
