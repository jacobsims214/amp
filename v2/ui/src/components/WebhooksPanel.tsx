import { useCallback, useEffect, useState } from 'react'
import { AlertTriangle, ChevronDown, ChevronRight, KeyRound, Loader2, Pencil, Plus, Power, Send, Trash2 } from 'lucide-react'
import { api } from '../api/client'
import type { Webhook, WebhookChange, WebhookDelivery, WebhookFormat } from '../types'
import { Notice, Pill, SecretReveal, When } from './admin'
import { dangerBtnCls, errMsg, fieldCls, primaryBtnCls, quietBtnCls, smallSelectCls } from '../lib/adminStyles'
import { labelCls } from './CrudModal'
import { stoppedAuditStreams } from '../lib/audit'

// Webhooks for a project (project owners), an org (org admins), or the
// whole instance (global admins: audit entries only, all of them, the SIEM
// stream). AMP POSTs each matching event, signed with the webhook's secret;
// Slack format sends a readable line to a Slack incoming webhook instead.
// A webhook may also send one static header (e.g. a Splunk HEC token); its
// value is write-only.

const EVENT_CHOICES: { value: string; label: string }[] = [
  { value: 'task.created', label: 'Ticket created' },
  { value: 'task.dispatched', label: 'Ticket started' },
  { value: 'task.completed', label: 'Ticket completed' },
  { value: 'task.blocked', label: 'Ticket blocked' },
  { value: 'task.unblocked', label: 'Ticket ready' },
  { value: 'task.assigned', label: 'Ticket reassigned' },
  { value: 'task.updated', label: 'Ticket edited or deleted' },
  { value: 'comment.added', label: 'Comment' },
  { value: 'story.*', label: 'Stories' },
  { value: 'epic.*', label: 'Epics' },
  { value: 'project.*', label: 'Project' },
  { value: 'audit.*', label: 'Audit log entries' },
]

// What an instance webhook can carry: audit entries only.
const INSTANCE_EVENT_CHOICES: { value: string; label: string }[] = [
  { value: 'audit.*', label: 'Every audit entry' },
  { value: 'audit.auth.*', label: 'Sign-ins and failed auth' },
  { value: 'audit.access.*', label: 'Refused access' },
  { value: 'audit.plan.*', label: 'Plan approval' },
  { value: 'audit.user.*', label: 'Accounts' },
  { value: 'audit.token.*', label: 'API tokens' },
]

type Scope = { orgId?: number; projectId?: number; instance?: boolean }

function describeEvents(events: string[]) {
  if (events.length === 0) return 'All ticket and project events'
  if (events.includes('*')) return 'Everything, audit entries included'
  return events.map(e => [...EVENT_CHOICES, ...INSTANCE_EVENT_CHOICES].find(c => c.value === e)?.label ?? e).join(', ')
}

type HeaderMode = 'keep' | 'set' | 'remove'

export function WebhookForm({ initial, instance, submitLabel, onSubmit, onCancel }: {
  initial: WebhookChange & { auth_header_name?: string; auth_header_set?: boolean }
  instance?: boolean
  submitLabel: string
  onSubmit: (c: WebhookChange) => Promise<void>
  onCancel?: () => void
}) {
  const [name, setName] = useState(initial.name ?? '')
  const [url, setUrl] = useState(initial.url ?? '')
  const [format, setFormat] = useState<WebhookFormat>(initial.format ?? 'json')
  const [events, setEvents] = useState<string[]>(initial.events ?? (instance ? ['audit.*'] : []))
  const [headerMode, setHeaderMode] = useState<HeaderMode>(initial.auth_header_set ? 'keep' : 'remove')
  const [headerName, setHeaderName] = useState(initial.auth_header_name || 'Authorization')
  const [headerValue, setHeaderValue] = useState('')
  // Audit entries' request context is personal data: org and project
  // webhooks opt in, instance webhooks (the SIEM stream) default to it.
  const [includeContext, setIncludeContext] = useState(initial.include_context ?? !!instance)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const toggle = (v: string) => setEvents(es => es.includes(v) ? es.filter(e => e !== v) : [...es, v])
  const choices = instance ? INSTANCE_EVENT_CHOICES : EVENT_CHOICES
  const sendsAudit = !!instance || events.some(e => e === '*' || e.startsWith('audit.'))
  // A stored header goes wherever the URL points, so moving the webhook needs it entered again.
  const urlMoved = !!initial.auth_header_set && !!initial.url && url.trim() !== initial.url
  const headerNeeded = urlMoved && headerMode === 'keep'

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (headerNeeded) { setError('The URL changed: enter the auth header again, or remove it.'); return }
    if (headerMode === 'set' && (!headerName.trim() || !headerValue)) { setError('Enter the header name and its value, or choose no header.'); return }
    setBusy(true); setError(null)
    const change: WebhookChange = { name: name.trim(), url: url.trim(), format, events, include_context: includeContext }
    if (headerMode === 'set') change.auth_header = `${headerName.trim()}: ${headerValue}`
    else if (headerMode === 'remove' && initial.auth_header_set) change.auth_header = ''
    try { await onSubmit(change); setHeaderValue('') }
    catch (err) { setError(errMsg(err, 'Save failed')) }
    finally { setBusy(false) }
  }

  return (
    <form onSubmit={submit} className="space-y-3 rounded-xl p-4" style={{ background: 'var(--bg)', border: '1px solid var(--line)' }}>
      <div className="grid grid-cols-1 sm:grid-cols-[1fr_2fr] gap-3">
        <div>
          <label className={labelCls}>Name</label>
          <input value={name} onChange={e => setName(e.target.value)} placeholder="e.g. #eng-amp Slack" className={`${fieldCls} w-full`} />
        </div>
        <div>
          <label className={labelCls}>URL</label>
          <input value={url} onChange={e => setUrl(e.target.value)} required type="url" placeholder="https://hooks.slack.com/services/…" className={`${fieldCls} w-full`} />
        </div>
      </div>
      <div>
        <label className={labelCls}>Format</label>
        <select value={format} onChange={e => setFormat(e.target.value as WebhookFormat)} className={smallSelectCls}>
          <option value="json">JSON, signed (for your own service)</option>
          <option value="slack">Slack message</option>
        </select>
      </div>
      <fieldset className="m-0 p-0 border-0">
        <legend className={labelCls}>Auth header <span className="text-ink-3 font-normal normal-case tracking-normal">(optional, for SIEM collectors that want a token, e.g. Splunk HEC or Datadog)</span></legend>
        <div className="flex flex-wrap items-center gap-3 text-xs text-ink-2 mb-2">
          {initial.auth_header_set && (
            <label className="inline-flex items-center gap-1.5 cursor-pointer min-w-0">
              <input type="radio" name="auth-header" checked={headerMode === 'keep'} onChange={() => setHeaderMode('keep')} />
              <span>Keep <span className="amp-num text-ink break-all">{initial.auth_header_name}: ••••••</span></span>
            </label>
          )}
          <label className="inline-flex items-center gap-1.5 cursor-pointer">
            <input type="radio" name="auth-header" checked={headerMode === 'set'} onChange={() => setHeaderMode('set')} />
            {initial.auth_header_set ? 'Replace it' : 'Send a header'}
          </label>
          <label className="inline-flex items-center gap-1.5 cursor-pointer">
            <input type="radio" name="auth-header" checked={headerMode === 'remove'} onChange={() => setHeaderMode('remove')} />
            {initial.auth_header_set ? 'Remove it' : 'No header'}
          </label>
        </div>
        {headerMode === 'set' && (
          <div className="grid grid-cols-1 sm:grid-cols-[1fr_2fr] gap-3">
            <input value={headerName} onChange={e => setHeaderName(e.target.value)} aria-label="Header name" placeholder="Authorization"
              className={`${fieldCls} w-full amp-num`} autoComplete="off" />
            <input value={headerValue} onChange={e => setHeaderValue(e.target.value)} aria-label="Header value" placeholder="Splunk 1234-abcd…"
              type="password" autoComplete="new-password" className={`${fieldCls} w-full amp-num`} />
          </div>
        )}
        {headerNeeded && <p className="text-[11px] text-warning-ink mt-1 mb-0">The URL changed: the header would go to the new address, so enter it again or remove it.</p>}
        {headerMode === 'set' && <p className="text-[11px] text-ink-3 mt-1 mb-0">Sent only to https URLs. The value can't be shown again; AMP never logs it.</p>}
      </fieldset>
      <div>
        <label className={labelCls}>Events {events.length === 0 && <span className="text-ink-3 font-normal">({instance ? 'none picked: every audit entry' : 'none picked: every ticket and project event'})</span>}</label>
        <div className="flex flex-wrap gap-1.5">
          {choices.map(c => {
            const on = events.includes(c.value)
            return (
              <button type="button" key={c.value} onClick={() => toggle(c.value)}
                className="px-2 py-1 rounded-lg text-[11px] font-medium cursor-pointer transition-colors"
                style={on ? { background: 'color-mix(in srgb, var(--link) 18%, transparent)', color: 'var(--link)', border: '1px solid color-mix(in srgb, var(--link) 45%, transparent)' }
                  : { color: 'var(--ink-2)', border: '1px solid var(--line)' }}>
                {c.label}
              </button>
            )
          })}
        </div>
      </div>
      {sendsAudit && format === 'json' && (
        <label className="flex items-start gap-2 text-xs text-ink-2 cursor-pointer">
          <input type="checkbox" checked={includeContext} onChange={e => setIncludeContext(e.target.checked)} className="mt-0.5" />
          <span>
            <span className="text-ink">Include request context</span> in audit entries: client IP, user agent, session and token ids, and
            before/after values. This is personal data; send it only to a receiver you are allowed to share it with.
          </span>
        </label>
      )}
      <div className="flex items-center gap-2">
        <button type="submit" disabled={busy || !url.trim()} className={primaryBtnCls}>
          {busy && <Loader2 size={12} className="animate-spin" />}{submitLabel}
        </button>
        {onCancel && <button type="button" onClick={onCancel} className={quietBtnCls}>Cancel</button>}
      </div>
      <Notice error={error} />
    </form>
  )
}

function Deliveries({ webhookId }: { webhookId: number }) {
  const [list, setList] = useState<WebhookDelivery[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let live = true
    const load = () => api.listDeliveries(webhookId, 20).then(d => { if (live) setList(d) }).catch(e => { if (live) setError(errMsg(e, 'Failed to load')) })
    load()
    const t = setInterval(load, 4000)
    return () => { live = false; clearInterval(t) }
  }, [webhookId])
  if (error) return <p className="text-xs text-danger-ink">{error}</p>
  if (!list) return <Loader2 size={13} className="text-primary animate-spin" />
  if (list.length === 0) return <p className="text-xs text-ink-3">No deliveries yet. Send a test to try it.</p>
  return (
    <ul className="space-y-1">
      {list.map(d => (
        <li key={d.id} className="flex items-center gap-2 text-xs">
          <Pill tone={d.status === 'delivered' ? 'green' : d.status === 'failed' ? 'red' : 'amber'}>{d.status}</Pill>
          <span className="font-mono text-ink">{d.event_type}</span>
          <span className="text-ink-3"><When at={d.created_at} /></span>
          <span className="text-ink-3">{d.attempts} attempt{d.attempts === 1 ? '' : 's'}</span>
          {d.last_status != null && <span className="text-ink-2">HTTP {d.last_status}</span>}
          {d.last_error && <span className="text-danger-ink truncate" title={d.last_error}>{d.last_error}</span>}
          {d.status === 'pending' && d.attempts > 0 && <span className="text-ink-2">retry <When at={d.next_attempt_at} /></span>}
        </li>
      ))}
    </ul>
  )
}

function WebhookRow({ hook, canManage, onChanged, onSecret }: {
  hook: Webhook
  canManage: boolean
  onChanged: (h: Webhook | null) => void
  onSecret: (h: Webhook) => void
}) {
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [info, setInfo] = useState<string | null>(null)

  const act = async (fn: () => Promise<void>) => {
    setBusy(true); setError(null); setInfo(null)
    try { await fn() } catch (e) { setError(errMsg(e, 'Failed')) } finally { setBusy(false) }
  }
  const failing = hook.consecutive_failures > 0
  return (
    <li className="px-4 py-3 space-y-2">
      <div className="flex items-center gap-2">
        <button onClick={() => setOpen(o => !o)} className="text-ink-3 hover:text-ink cursor-pointer" aria-label="Show deliveries">
          {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </button>
        <span className={`w-1.5 h-1.5 rounded-full flex-shrink-0 ${!hook.active ? 'bg-ink-3' : failing ? 'bg-warning' : 'bg-success'}`} />
        <div className="flex-1 min-w-0">
          <p className="text-sm text-ink truncate">{hook.name || hook.url}</p>
          <p className="text-[11px] text-ink-3 truncate">
            {hook.name && <span className="font-mono">{hook.url} · </span>}
            {hook.format === 'slack' ? 'Slack' : 'JSON'} · {describeEvents(hook.events)}
            {hook.auth_header_set && <> · <KeyRound size={10} className="inline -mt-0.5" aria-hidden /> sends {hook.auth_header_name}</>}
            {hook.include_context && <> · with request context</>}
          </p>
        </div>
        {!hook.active && <Pill tone="gray">off</Pill>}
        {failing && <Pill tone="amber" title={hook.last_error}>{hook.consecutive_failures} failed</Pill>}
        {hook.last_delivery_at && <span className="text-[11px] text-ink-3 hidden sm:inline">last <When at={hook.last_delivery_at} /></span>}
      </div>
      {canManage && (
        <div className="flex flex-wrap items-center gap-1 pl-6">
          <button disabled={busy || !hook.active} className={quietBtnCls} title={hook.active ? 'Send a test message' : 'Turn it on to test it'}
            onClick={() => act(async () => { await api.testWebhook(hook.id); setInfo('Test queued.'); setOpen(true) })}>
            <Send size={12} />Test
          </button>
          <button disabled={busy} className={quietBtnCls} onClick={() => setEditing(e => !e)}><Pencil size={12} />Edit</button>
          <button disabled={busy} className={quietBtnCls}
            onClick={() => act(async () => onChanged(await api.updateWebhook(hook.id, { active: !hook.active })))}>
            <Power size={12} />{hook.active ? 'Turn off' : 'Turn on'}
          </button>
          <button disabled={busy} className={quietBtnCls}
            onClick={() => { if (confirm('Rotate the signing secret? Receivers must switch to the new one.')) act(async () => onSecret(await api.rotateWebhookSecret(hook.id))) }}>
            <KeyRound size={12} />Rotate secret
          </button>
          <button disabled={busy} className={dangerBtnCls}
            onClick={() => { if (confirm(`Delete the webhook ${hook.name || hook.url}?`)) act(async () => { await api.deleteWebhook(hook.id); onChanged(null) }) }}>
            <Trash2 size={12} />Delete
          </button>
        </div>
      )}
      <div className="pl-6"><Notice error={error} info={info} /></div>
      {editing && (
        <div className="pl-6">
          <WebhookForm initial={hook} instance={hook.instance} submitLabel="Save" onCancel={() => setEditing(false)}
            onSubmit={async c => { onChanged(await api.updateWebhook(hook.id, c)); setEditing(false) }} />
        </div>
      )}
      {open && <div className="pl-6"><Deliveries webhookId={hook.id} /></div>}
    </li>
  )
}

/** A warning when audit webhooks have stopped carrying the stream (turned off, or failing). */
export function StoppedStreamWarning({ hooks, siem }: { hooks: Webhook[]; siem?: boolean }) {
  const stopped = stoppedAuditStreams(hooks)
  if (stopped.length === 0) return null
  return (
    <div role="alert" className="rounded-xl px-3 py-2 text-xs flex gap-2 items-start"
      style={{ background: 'color-mix(in srgb, var(--warning) 12%, transparent)', border: '1px solid color-mix(in srgb, var(--warning) 45%, transparent)' }}>
      <AlertTriangle size={14} className="text-warning-ink flex-shrink-0 mt-px" aria-hidden />
      <div className="min-w-0 space-y-0.5">
        <p className="m-0 text-ink font-medium">{siem ? 'SIEM stream stopped' : 'Audit stream stopped'}: entries are not reaching {stopped.length === 1 ? 'this receiver' : `${stopped.length} receivers`}.</p>
        {stopped.map(({ hook, why }) => (
          <p key={hook.id} className="m-0 text-ink-2 break-words">{hook.name || hook.url}: {why}</p>
        ))}
        <p className="m-0 text-ink-3">The audit log has a webhook.disable or webhook.delivery.dropped entry for each.</p>
      </div>
    </div>
  )
}

export function WebhooksPanel({ orgId, projectId, instance, canManage, onHooks }: Scope & { canManage: boolean; onHooks?: (hooks: Webhook[]) => void }) {
  const [hooks, setHooks] = useState<Webhook[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [secret, setSecret] = useState<Webhook | null>(null)

  const load = useCallback(() => {
    api.listWebhooks({ orgId, projectId }).then(setHooks).catch(e => setError(errMsg(e, 'Failed to load webhooks')))
  }, [orgId, projectId])
  useEffect(() => { if (canManage) load() }, [canManage, load])
  useEffect(() => { if (hooks) onHooks?.(hooks) }, [hooks, onHooks])

  if (!canManage) {
    return <p className="text-xs text-ink-3">{instance ? 'Global admins' : orgId ? 'Org owners and admins' : 'Project owners'} manage {instance ? 'instance ' : ''}webhooks.</p>
  }
  const replace = (id: number) => (h: Webhook | null) =>
    setHooks(list => h ? (list ?? []).map(x => x.id === id ? h : x) : (list ?? []).filter(x => x.id !== id))

  return (
    <div className="space-y-3">
      {error && <p className="text-xs text-danger-ink">{error}</p>}
      {hooks && !onHooks && <StoppedStreamWarning hooks={hooks} siem={instance} />}
      {secret?.secret && (
        <SecretReveal title={`Signing secret for ${secret.name || secret.url}`} secret={secret.secret} onDone={() => setSecret(null)}
          note="Copy it now: it's only shown when the webhook is created or its secret rotated.">
          {secret.format === 'json' ? (
            <p className="text-xs text-ink-2 leading-relaxed">
              Each POST has <code className="text-ink">X-AMP-Signature: t=&lt;unix&gt;,v1=&lt;hex&gt;</code>, where v1 is
              HMAC-SHA256 of <code className="text-ink">&lt;t&gt;.&lt;body&gt;</code> with this secret. Reject old t values to stop replays.
            </p>
          ) : (
            <p className="text-xs text-ink-2">Slack doesn't check signatures; keep this in case you switch the format to JSON.</p>
          )}
        </SecretReveal>
      )}
      {hooks == null ? (
        <Loader2 size={16} className="text-primary animate-spin" />
      ) : hooks.length > 0 ? (
        <ul className="rounded-xl border border-line divide-y divide-line">
          {hooks.map(h => <WebhookRow key={h.id} hook={h} canManage={canManage} onChanged={replace(h.id)} onSecret={w => { replace(h.id)(w); setSecret(w) }} />)}
        </ul>
      ) : !creating && (
        <p className="text-xs text-ink-3">No webhooks yet.</p>
      )}
      {creating ? (
        <WebhookForm initial={{ format: 'json', events: instance ? ['audit.*'] : [] }} instance={instance} submitLabel="Add webhook" onCancel={() => setCreating(false)}
          onSubmit={async c => {
            const h = await api.createWebhook({ orgId, projectId }, c)
            setHooks(list => [...(list ?? []), h]); setSecret(h); setCreating(false)
          }} />
      ) : (
        <button onClick={() => setCreating(true)} className={quietBtnCls}><Plus size={12} />Add webhook</button>
      )}
      {hooks && hooks.length > 0 && (
        <p className="text-[11px] text-ink-3">
          {instance
            ? 'Failed deliveries are retried for a few hours, then dropped and recorded in the audit log. Instance webhooks never turn themselves off.'
            : 'Failed deliveries are retried for a few hours. After 20 deliveries in a row fail, a webhook turns itself off; both are recorded in the audit log.'}
        </p>
      )}
    </div>
  )
}
