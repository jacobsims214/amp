import { useCallback, useEffect, useState } from 'react'
import {
  Bot, ChevronDown, ChevronRight, Cog, Download, ExternalLink, KeyRound, Loader2, RotateCw, ShieldAlert, ShieldCheck,
  SlidersHorizontal, User, UserX, X,
} from 'lucide-react'
import { api } from '../api/client'
import type { AuditEntry, AuditFilter, AuditVerifyReport } from '../types'
import { Pill, When } from './admin'
import { errMsg, fieldCls, quietBtnCls, smallSelectCls } from '../lib/adminStyles'
import {
  ACTOR_TYPE_LABEL, AUTH_METHOD_LABEL, CLIENT_LABEL, activeFilterCount, actorName, auditExportUrl, auditVerifyUrl, diffValues,
  hasDetails, isoToLocal, localToISO, targetLabel, type AuditScope,
} from '../lib/audit'

// The audit log for one scope: an org (its own changes and its projects'),
// a project, one person, or everything (global admins). Newest first, paged
// backwards. Filters narrow it; the export downloads exactly what the
// filters select (every row, not just the loaded pages), and is itself
// recorded in the log. With `verify`, the hash chain of the scope is
// checked and its status shown, linking to the full report.

const CATEGORIES: { value: string; label: string }[] = [
  { value: '', label: 'Every action' },
  { value: 'plan.', label: 'Plan approval' },
  { value: 'auth.', label: 'Sign-ins and failed auth' },
  { value: 'access.', label: 'Refused access' },
  { value: 'org.', label: 'Org and members' },
  { value: 'team.', label: 'Teams' },
  { value: 'invite.', label: 'Invites' },
  { value: 'project.', label: 'Projects and access' },
  { value: 'webhook.', label: 'Webhooks' },
  { value: 'user.', label: 'Accounts' },
  { value: 'agent.', label: 'Agent accounts' },
  { value: 'token.', label: 'API tokens' },
  { value: 'kb.', label: 'Knowledge base' },
  { value: 'audit.', label: 'The audit log itself' },
  { value: 'task.delete', label: 'Deleted tickets' },
  { value: 'story.delete', label: 'Deleted stories' },
  { value: 'epic.delete', label: 'Deleted epics' },
]

const PAGE = 50

function actionTone(e: AuditEntry) {
  if (e.outcome === 'denied' || e.outcome === 'failed') return 'red' as const
  const a = e.action
  if (a.endsWith('.delete') || a.endsWith('.remove') || a.endsWith('.revoke') || a === 'user.deactivate' || a === 'project.reset') return 'red' as const
  if (a.startsWith('token.') || a.startsWith('user.') || a.startsWith('agent.') || a.startsWith('auth.')) return 'amber' as const
  return 'indigo' as const
}

function ActorIcon({ type }: { type?: string }) {
  const props = { size: 13, 'aria-hidden': true, className: 'text-ink-3 flex-shrink-0' }
  if (type === 'agent') return <Bot {...props} />
  if (type === 'system') return <Cog {...props} />
  if (type === 'anonymous') return <UserX {...props} />
  return <User {...props} />
}

/** One field of the expanded entry; nothing when empty. */
function Field({ label, children, mono }: { label: string; children?: React.ReactNode; mono?: boolean }) {
  if (children == null || children === '') return null
  return (
    <div className="min-w-0">
      <dt className="amp-label !text-[10px] mb-0.5">{label}</dt>
      <dd className={`m-0 text-xs text-ink break-words ${mono ? 'amp-num' : ''}`}>{children}</dd>
    </div>
  )
}

/** Before/after as a field-by-field table, changed fields first. */
export function BeforeAfter({ before, after }: { before?: Record<string, unknown>; after?: Record<string, unknown> }) {
  const rows = diffValues(before, after)
  if (rows.length === 0) return null
  return (
    <div className="overflow-x-auto">
      <table className="w-full max-w-[760px] text-xs border-collapse">
        <caption className="sr-only">What changed</caption>
        <thead>
          <tr className="text-left text-ink-3">
            <th scope="col" className="amp-label !text-[10px] font-semibold py-1 pr-3">Field</th>
            <th scope="col" className="amp-label !text-[10px] font-semibold py-1 pr-3">Before</th>
            <th scope="col" className="amp-label !text-[10px] font-semibold py-1">After</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(r => (
            <tr key={r.key} className={`border-t border-line-sub ${r.changed ? '' : 'text-ink-3'}`}>
              <th scope="row" className="py-1 pr-3 font-medium text-left align-top amp-num">{r.key}</th>
              <td className="py-1 pr-3 align-top break-all amp-num">
                {r.changed && r.before !== '' ? <del className="no-underline text-danger-ink">{r.before}</del> : r.before || <span className="text-ink-3">—</span>}
              </td>
              <td className="py-1 align-top break-all amp-num">
                {r.changed && r.after !== '' ? <ins className="no-underline text-success-ink">{r.after}</ins> : r.after || <span className="text-ink-3">—</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** One audit row; the button opens who, where from, what changed, and its chain position. */
export function AuditRow({ e, showProject, first }: { e: AuditEntry; showProject: boolean; first: boolean }) {
  const [open, setOpen] = useState(false)
  const expandable = hasDetails(e)
  const refused = e.outcome === 'denied' || e.outcome === 'failed'
  const details = e.details && Object.keys(e.details).length ? e.details : null
  return (
    <li className={`${first ? '' : 'border-t border-line'}`}>
      <div className="px-4 py-2.5 flex items-start gap-3">
        <span className="text-[11px] text-ink-3 w-16 flex-shrink-0 pt-0.5"><When at={e.at} /></span>
        <div className="flex-1 min-w-0">
          <p className="text-sm text-ink break-words m-0 flex items-start gap-1.5">
            <span className="pt-[3px]" title={ACTOR_TYPE_LABEL[e.actor_type ?? ''] ?? undefined}><ActorIcon type={e.actor_type} /></span>
            <span className="min-w-0"><span className="text-link">{actorName(e)}</span> {e.summary}</span>
          </p>
          <div className="flex flex-wrap items-center gap-1.5 mt-1">
            <Pill tone={actionTone(e)}>{e.action}</Pill>
            {refused && <Pill tone="red" title={e.reason || undefined}><ShieldAlert size={9} />{e.outcome}</Pill>}
            {e.client && <Pill tone="gray" title="How it arrived">{CLIENT_LABEL[e.client] ?? e.client}</Pill>}
            {e.via_token && <Pill tone="gray" title="Made with an API token"><KeyRound size={9} />API token</Pill>}
            {e.source_ip && <span className="amp-num text-[11px] text-ink-2" title="Source address">{e.source_ip}</span>}
            {showProject && e.project_id != null && <span className="text-[10px] text-ink-3">project {e.project_id}</span>}
          </div>
        </div>
        {expandable && (
          <button type="button" onClick={() => setOpen(o => !o)} aria-expanded={open}
            aria-label={open ? 'Hide details' : 'Show details'} title={open ? 'Hide details' : 'Show details'}
            className="text-ink-3 hover:text-ink cursor-pointer p-1 -m-1 flex-shrink-0">
            {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          </button>
        )}
      </div>
      {open && (
        <div className="px-4 pb-3 sm:pl-[92px] space-y-3">
          {refused && e.reason && <p className="m-0 text-xs text-danger-ink">{e.reason}</p>}
          <dl className="m-0 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-x-4 gap-y-2">
            <Field label="Actor">{[ACTOR_TYPE_LABEL[e.actor_type ?? ''], e.actor_user_id != null ? `user ${e.actor_user_id}` : ''].filter(Boolean).join(' · ')}</Field>
            <Field label="On behalf of">{e.on_behalf_of_user_id != null ? `user ${e.on_behalf_of_user_id}` : ''}</Field>
            <Field label="Agent profile">{e.agent_profile}</Field>
            <Field label="Signed in with">{[AUTH_METHOD_LABEL[e.auth_method ?? ''] ?? e.auth_method, e.token_id != null ? `token ${e.token_id}` : ''].filter(Boolean).join(' · ')}</Field>
            <Field label="Session" mono>{e.session_id}</Field>
            <Field label="Source address" mono>{e.source_ip}</Field>
            <Field label="User agent">{e.user_agent}</Field>
            <Field label="Request" mono>{e.request_id}</Field>
            <Field label="Trace" mono>{e.trace_id}</Field>
            <Field label="Target">{targetLabel(e)}</Field>
            <Field label="Chain" mono>
              {e.chain_key ? <span title={e.hash}>{e.chain_key} #{e.chain_seq} · {e.hash?.slice(0, 12)}…</span> : ''}
            </Field>
            <Field label="Recorded" mono>{`${new Date(e.at).toISOString()} · #${e.id}${e.schema_version === 1 ? ' · v1' : ''}`}</Field>
          </dl>
          {(e.before || e.after) && <BeforeAfter before={e.before} after={e.after} />}
          {details && (
            <details>
              <summary className="text-[11px] text-ink-2 cursor-pointer">Details</summary>
              <pre className="mt-1 text-[11px] amp-num whitespace-pre-wrap break-all bg-surface-2 border border-line-sub p-2 m-0">{JSON.stringify(details, null, 2)}</pre>
            </details>
          )}
        </div>
      )}
    </li>
  )
}

/** The chain check of the scope: verified, problems, or unsigned, with the full report one click away. */
export function VerifyStatus({ report, error, loading, orgId, onRecheck }: {
  report: AuditVerifyReport | null
  error: string | null
  loading: boolean
  orgId?: number
  onRecheck: () => void
}) {
  const rows = report?.chains.reduce((n, c) => n + c.rows, 0) ?? 0
  const problems = report?.chains.reduce((n, c) => n + c.problems.length, 0) ?? 0
  let tone: 'green' | 'red' | 'amber' | 'gray' = 'gray'
  let label = 'Checking the chain…'
  if (error) { tone = 'amber'; label = "Couldn't check the chain" }
  else if (report && !report.ok) { tone = 'red'; label = `Chain broken: ${problems} problem${problems === 1 ? '' : 's'}` }
  else if (report) {
    tone = report.signing.configured ? 'green' : 'amber'
    label = report.chains.length === 0 ? 'Chain empty' : `Chain verified · ${rows.toLocaleString()} row${rows === 1 ? '' : 's'}`
  }
  const title = error ?? (report
    ? `${report.chains.length} chain(s) checked ${new Date(report.checked_at).toLocaleString()}.` +
      (report.signing.configured ? '' : ' Checkpoints are not signed: no signing key is configured.')
    : undefined)
  return (
    <span className="inline-flex items-center gap-1.5" role="status" aria-live="polite">
      <Pill tone={tone} title={title}>
        {loading ? <Loader2 size={9} className="animate-spin" /> : tone === 'red' || tone === 'amber' ? <ShieldAlert size={9} /> : <ShieldCheck size={9} />}
        {label}
      </Pill>
      {report && !report.signing.configured && !error && report.ok && <span className="text-[11px] text-warning-ink">unsigned checkpoints</span>}
      <a href={auditVerifyUrl(orgId)} target="_blank" rel="noreferrer" className="text-[11px] text-link hover:underline inline-flex items-center gap-0.5">
        Report<ExternalLink size={10} aria-hidden />
      </a>
      <button type="button" onClick={onRecheck} disabled={loading} className="text-ink-3 hover:text-ink cursor-pointer disabled:opacity-50"
        aria-label="Check the chain again" title="Check again">
        <RotateCw size={11} />
      </button>
    </span>
  )
}

/** A text filter that applies on Enter or when the field loses focus, not on every keystroke. */
function TextFilter({ label, value, onApply, placeholder, mono, width = 'sm:w-44' }: {
  label: string; value?: string; onApply: (v: string) => void; placeholder?: string; mono?: boolean; width?: string
}) {
  const [draft, setDraft] = useState(value ?? '')
  // Follow outside changes (Clear filters) without an effect.
  const [seen, setSeen] = useState(value)
  if (seen !== value) { setSeen(value); setDraft(value ?? '') }
  const apply = () => { if (draft.trim() !== (value ?? '')) onApply(draft.trim()) }
  return (
    <label className={`block w-full ${width}`}>
      <span className="amp-label !text-[10px] block mb-1">{label}</span>
      <input value={draft} onChange={e => setDraft(e.target.value)} onBlur={apply} placeholder={placeholder}
        onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); apply() } }}
        className={`${fieldCls} !py-1 !text-xs w-full ${mono ? 'amp-num' : ''}`} />
    </label>
  )
}

function SelectFilter({ label, value, onChange, options }: {
  label: string; value?: string; onChange: (v: string) => void; options: { value: string; label: string }[]
}) {
  return (
    <label className="block">
      <span className="amp-label !text-[10px] block mb-1">{label}</span>
      <select value={value ?? ''} onChange={e => onChange(e.target.value)} className={`${smallSelectCls} w-full sm:!w-auto`}>
        {options.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
      </select>
    </label>
  )
}

export function AuditLogPanel({ orgId, projectId, actorId, verify }: AuditScope & { verify?: boolean }) {
  const scope: AuditScope = { orgId, projectId, actorId }
  const [filter, setFilter] = useState<AuditFilter>({})
  const [showMore, setShowMore] = useState(false)
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [more, setMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [report, setReport] = useState<AuditVerifyReport | null>(null)
  const [verifyError, setVerifyError] = useState<string | null>(null)
  const [verifying, setVerifying] = useState(false)

  const set = (patch: Partial<AuditFilter>) => setFilter(f => ({ ...f, ...patch }))
  const filterKey = JSON.stringify(filter)

  const load = useCallback(async (before?: number) => {
    setLoading(true); setError(null)
    try {
      const page = await api.audit({ orgId, projectId, actorId, filter: JSON.parse(filterKey) as AuditFilter, before, limit: PAGE })
      setEntries(prev => before ? [...prev, ...page] : page)
      setMore(page.length === PAGE)
    } catch (e) {
      setError(errMsg(e, 'Failed to load the audit log'))
      if (!before) setEntries([])
    } finally {
      setLoading(false)
    }
  }, [orgId, projectId, actorId, filterKey])

  useEffect(() => { load() }, [load])

  const check = useCallback(async () => {
    setVerifying(true); setVerifyError(null)
    try { setReport(await api.auditVerify(orgId)) }
    catch (e) { setReport(null); setVerifyError(errMsg(e, 'Verification failed')) }
    finally { setVerifying(false) }
  }, [orgId])
  useEffect(() => { if (verify) check() }, [verify, check])

  const count = activeFilterCount(filter)
  const moreCount = count - (filter.action ? 1 : 0) - (filter.outcome ? 1 : 0)
  const category = CATEGORIES.some(c => c.value === (filter.action ?? '')) ? (filter.action ?? '') : 'custom'

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end gap-2">
        <SelectFilter label="Action" value={category} onChange={v => v !== 'custom' && set({ action: v })}
          options={category === 'custom' ? [...CATEGORIES, { value: 'custom', label: filter.action ?? '' }] : CATEGORIES} />
        <SelectFilter label="Outcome" value={filter.outcome} onChange={v => set({ outcome: v as AuditFilter['outcome'] })}
          options={[{ value: '', label: 'Any outcome' }, { value: 'success', label: 'Succeeded' }, { value: 'denied', label: 'Refused' }, { value: 'failed', label: 'Failed' }]} />
        <button type="button" onClick={() => setShowMore(s => !s)} aria-expanded={showMore} aria-controls="audit-more-filters"
          className={`${quietBtnCls} self-end`}>
          <SlidersHorizontal size={12} />More filters{moreCount > 0 && <span className="amp-num text-link">{moreCount}</span>}
        </button>
        {count > 0 && (
          <button type="button" onClick={() => setFilter({})} className={`${quietBtnCls} self-end`}><X size={12} />Clear filters</button>
        )}
        {loading && <Loader2 size={13} className="text-primary animate-spin self-center" aria-label="Loading" />}
        <div className="ml-auto flex flex-wrap items-center gap-2 self-end">
          {verify && <VerifyStatus report={report} error={verifyError} loading={verifying} orgId={orgId} onRecheck={check} />}
          <span className="inline-flex items-center gap-1" title="Downloads every entry the filters select, not just the loaded ones. The export is recorded in the audit log.">
            <Download size={12} className="text-ink-3" aria-hidden />
            <a href={auditExportUrl(scope, filter, 'csv')} download className="amp-btn amp-btn-sm">CSV</a>
            <a href={auditExportUrl(scope, filter, 'ndjson')} download className="amp-btn amp-btn-sm">NDJSON</a>
          </span>
        </div>
      </div>

      {showMore && (
        <div id="audit-more-filters" className="grid grid-cols-1 sm:flex sm:flex-wrap gap-3 p-3 bg-surface-2 border border-line-sub">
          <TextFilter label="Actor email contains" value={filter.actor} onApply={v => set({ actor: v })} placeholder="jo@" />
          <SelectFilter label="Actor type" value={filter.actor_type} onChange={v => set({ actor_type: v as AuditFilter['actor_type'] })}
            options={[{ value: '', label: 'Anyone' }, ...Object.entries(ACTOR_TYPE_LABEL).map(([value, label]) => ({ value, label }))]} />
          <SelectFilter label="Came through" value={filter.client} onChange={v => set({ client: v as AuditFilter['client'] })}
            options={[{ value: '', label: 'Any client' }, ...Object.entries(CLIENT_LABEL).map(([value, label]) => ({ value, label }))]} />
          <TextFilter label="IP or CIDR" value={filter.ip} onApply={v => set({ ip: v })} placeholder="203.0.113.0/24" mono />
          <TextFilter label="Exact action" value={category === 'custom' ? filter.action : ''} onApply={v => set({ action: v })} placeholder="plan.approve.denied" mono />
          <TextFilter label="Target type" value={filter.target_type} onApply={v => set({ target_type: v })} placeholder="project" width="sm:w-32" />
          <TextFilter label="Target id" value={filter.target_id} onApply={v => set({ target_id: v })} placeholder="42" width="sm:w-24" mono />
          {!projectId && (
            <TextFilter label="Project id" value={filter.project_id ? String(filter.project_id) : ''} width="sm:w-24" mono
              onApply={v => set({ project_id: /^\d+$/.test(v) ? Number(v) : undefined })} placeholder="12" />
          )}
          <label className="block">
            <span className="amp-label !text-[10px] block mb-1">From</span>
            <input type="datetime-local" value={isoToLocal(filter.from)} onChange={e => set({ from: localToISO(e.target.value) })}
              className={`${fieldCls} !py-1 !text-xs w-full`} />
          </label>
          <label className="block">
            <span className="amp-label !text-[10px] block mb-1">Until</span>
            <input type="datetime-local" value={isoToLocal(filter.to)} onChange={e => set({ to: localToISO(e.target.value) })}
              className={`${fieldCls} !py-1 !text-xs w-full`} />
          </label>
        </div>
      )}

      {error && <p className="text-xs text-danger-ink" role="alert">{error}</p>}
      <div className="rounded-xl border border-line overflow-hidden">
        {entries.length === 0 && !loading ? (
          <p className="px-4 py-6 text-center text-sm text-ink-3">
            {count > 0 ? 'Nothing matches these filters.' : 'Nothing recorded yet'}
            {count > 0 && <> <button type="button" onClick={() => setFilter({})} className="text-link hover:underline cursor-pointer">Clear filters</button></>}
          </p>
        ) : (
          <ul className="m-0 p-0 list-none">
            {entries.map((e, i) => <AuditRow key={e.id} e={e} first={i === 0} showProject={!projectId} />)}
          </ul>
        )}
      </div>
      {more && (
        <button onClick={() => load(entries[entries.length - 1]?.id)} disabled={loading}
          className="text-xs font-semibold text-link hover:underline disabled:opacity-50 cursor-pointer">
          Older entries
        </button>
      )}
    </div>
  )
}
