import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { AuditRow, BeforeAfter, VerifyStatus } from './AuditLogPanel'
import type { AuditEntry, AuditVerifyReport } from '../types'

const entry = (over: Partial<AuditEntry> = {}): AuditEntry => ({
  id: 41, at: '2026-10-26T12:00:00Z', actor_email: 'jo@example.com', action: 'plan.approve.denied', summary: 'tried to approve the plan of Apollo',
  schema_version: 2, actor_type: 'agent', outcome: 'denied', reason: 'plan approval is only from the web UI', client: 'mcp',
  source_ip: '203.0.113.7', user_agent: 'claude-code/2.1', chain_key: 'org:3', chain_seq: 9, hash: 'ab'.repeat(32), project_id: 5, ...over,
})

describe('AuditRow', () => {
  it('shows who, the outcome, the client and the address on the line itself', () => {
    const html = renderToStaticMarkup(<ul><AuditRow e={entry()} showProject first /></ul>)
    expect(html).toContain('jo@example.com')
    expect(html).toContain('plan.approve.denied')
    expect(html).toContain('denied')
    expect(html).toContain('MCP')
    expect(html).toContain('203.0.113.7')
    expect(html).toContain('project 5')
    expect(html).toContain('aria-expanded="false"')
  })
  it('shows on the line itself that a webhook change replaced its auth header, without any value (#2441)', () => {
    const html = renderToStaticMarkup(<ul><AuditRow e={entry({ action: 'webhook.update', outcome: 'success', details: { auth_header_changed: true },
      before: { delivery_header: 'Authorization' }, after: { delivery_header: 'Authorization' } })} showProject first /></ul>)
    expect(html).toContain('auth header replaced')
    const plain = renderToStaticMarkup(<ul><AuditRow e={entry({ action: 'webhook.update', outcome: 'success', details: { url: 'https://x' } })} showProject first /></ul>)
    expect(plain).not.toContain('auth header')
  })
  it('shows a v1 entry with only its original fields', () => {
    const html = renderToStaticMarkup(<ul><AuditRow e={{ id: 2, at: '2026-01-01T00:00:00Z', actor_email: '', action: 'org.rename', summary: 'renamed' }} showProject={false} first /></ul>)
    expect(html).toContain('AMP')
    expect(html).not.toContain('denied')
    expect(html).not.toContain('aria-expanded') // nothing more to show
  })
})

describe('BeforeAfter', () => {
  it('marks what changed', () => {
    const html = renderToStaticMarkup(<BeforeAfter before={{ role: 'member' }} after={{ role: 'admin' }} />)
    expect(html).toContain('<del')
    expect(html).toContain('member')
    expect(html).toContain('<ins')
    expect(html).toContain('admin')
  })
})

describe('VerifyStatus', () => {
  const report = (over: Partial<AuditVerifyReport> = {}): AuditVerifyReport => ({
    ok: true, checked_at: '2026-10-26T12:00:00Z', signing: { configured: true, key_id: 'k1' },
    chains: [{ chain_key: 'org:3', ok: true, rows: 1204, head_seq: 1204, checkpoints_matched: 3, signed_checkpoints: 3, problems: [] }], ...over,
  })
  const render = (r: AuditVerifyReport | null, error: string | null = null) =>
    renderToStaticMarkup(<VerifyStatus report={r} error={error} loading={false} orgId={3} onRecheck={() => {}} />)
  it('says verified, with the row count and a link to the report', () => {
    const html = render(report())
    expect(html).toMatch(/Chain verified · 1[,.\s]?204 rows/)
    expect(html).toContain('href="/api/audit/verify?org_id=3"')
  })
  it('says broken with the number of problems', () => {
    const html = render(report({ ok: false, chains: [{ chain_key: 'org:3', ok: false, rows: 10, head_seq: 10, checkpoints_matched: 0, signed_checkpoints: 0,
      problems: [{ kind: 'hash_mismatch', seq: 4, detail: 'row 4 altered' }, { kind: 'gap', seq: 7, detail: 'seq 7 missing' }] }] }))
    expect(html).toContain('Chain broken: 2 problems')
  })
  it('says unproven, not verified, when nothing signed vouches for the chain (#2439)', () => {
    const unproven = report({ ok: false, status: 'unproven', signing: { configured: false },
      chains: [{ chain_key: 'org:3', ok: false, status: 'unproven', rows: 12, head_seq: 12, checkpoints_matched: 0, signed_checkpoints: 0, problems: [],
        warnings: ['unproven: no signing key is configured (AUDIT_SIGNING_KEY): the chain can\'t be proven against someone with database access'] }] })
    const html = render(unproven)
    expect(html).toContain('Unproven · 12 rows')
    expect(html).not.toContain('Chain verified')
    expect(html).not.toContain('Chain broken')
    expect(html).toContain('no signing key')
    // A key, but no signed checkpoint yet: still unproven.
    expect(render(report({ ok: false, status: 'unproven' }))).toContain('Unproven')
  })
  it('says when a check read only the new entries, and when everything was last re-read (#2441)', () => {
    const html = render(report({ chains: [{ chain_key: 'org:3', ok: true, rows: 1204, head_seq: 1204, checkpoints_matched: 3, signed_checkpoints: 3, problems: [],
      rows_read: 2, incremental: true, full_check_at: '2026-10-26T11:15:00Z' }] }))
    expect(html).toMatch(/Chain verified · 1[,.\s]?204 rows/)
    expect(html).toContain('Only the 2 entries written since the last check were read; every entry was last re-read in full')
    expect(render(report())).not.toContain('Only the')
  })
  it('treats an unsigned "ok" from an older server as unproven, and says when the check failed', () => {
    expect(render(report({ signing: { configured: false } }))).toContain('Unproven')
    expect(render(null, 'forbidden')).toContain("Couldn&#x27;t check the chain")
  })
})
