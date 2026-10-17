import { useCallback, useEffect, useState } from 'react'
import { ChevronDown, ChevronRight, KeyRound, Loader2, Pencil, Plus, Power, Send, Trash2 } from 'lucide-react'
import { api } from '../api/client'
import type { Webhook, WebhookChange, WebhookDelivery, WebhookFormat } from '../types'
import { Notice, Pill, SecretReveal, When } from './admin'
import { dangerBtnCls, errMsg, fieldCls, primaryBtnCls, quietBtnCls, smallSelectCls } from '../lib/adminStyles'
import { labelCls } from './CrudModal'

// Webhooks for a project (project owners) or an org (org admins). AMP POSTs
// each matching event, signed with the webhook's secret; Slack format sends
// a readable line to a Slack incoming webhook instead.

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

type Scope = { orgId?: number; projectId?: number }

function describeEvents(events: string[]) {
  if (events.length === 0) return 'All ticket and project events'
  if (events.includes('*')) return 'Everything, audit entries included'
  return events.map(e => EVENT_CHOICES.find(c => c.value === e)?.label ?? e).join(', ')
}

function WebhookForm({ initial, submitLabel, onSubmit, onCancel }: {
  initial: WebhookChange
  submitLabel: string
  onSubmit: (c: WebhookChange) => Promise<void>
  onCancel?: () => void
}) {
  const [name, setName] = useState(initial.name ?? '')
  const [url, setUrl] = useState(initial.url ?? '')
  const [format, setFormat] = useState<WebhookFormat>(initial.format ?? 'json')
  const [events, setEvents] = useState<string[]>(initial.events ?? [])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const toggle = (v: string) => setEvents(es => es.includes(v) ? es.filter(e => e !== v) : [...es, v])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true); setError(null)
    try { await onSubmit({ name: name.trim(), url: url.trim(), format, events }) }
    catch (err) { setError(errMsg(err, 'Save failed')) }
    finally { setBusy(false) }
  }

  return (
    <form onSubmit={submit} className="space-y-3 rounded-xl p-4" style={{ background: '#08101F', border: '1px solid #1E2C45' }}>
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
      <div>
        <label className={labelCls}>Events {events.length === 0 && <span className="text-[#3D5068] font-normal">(none picked: every ticket and project event)</span>}</label>
        <div className="flex flex-wrap gap-1.5">
          {EVENT_CHOICES.map(c => {
            const on = events.includes(c.value)
            return (
              <button type="button" key={c.value} onClick={() => toggle(c.value)}
                className="px-2 py-1 rounded-lg text-[11px] font-medium cursor-pointer transition-colors"
                style={on ? { background: 'rgba(99,102,241,0.18)', color: '#C7D2FE', border: '1px solid rgba(99,102,241,0.45)' }
                  : { color: '#7E91A8', border: '1px solid #1E2C45' }}>
                {c.label}
              </button>
            )
          })}
        </div>
      </div>
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
  if (error) return <p className="text-xs text-[#F87171]">{error}</p>
  if (!list) return <Loader2 size={13} className="text-[#6366F1] animate-spin" />
  if (list.length === 0) return <p className="text-xs text-[#3D5068]">No deliveries yet. Send a test to try it.</p>
  return (
    <ul className="space-y-1">
      {list.map(d => (
        <li key={d.id} className="flex items-center gap-2 text-xs">
          <Pill tone={d.status === 'delivered' ? 'green' : d.status === 'failed' ? 'red' : 'amber'}>{d.status}</Pill>
          <span className="font-mono text-[#DDE6F0]">{d.event_type}</span>
          <span className="text-[#3D5068]"><When at={d.created_at} /></span>
          <span className="text-[#3D5068]">{d.attempts} attempt{d.attempts === 1 ? '' : 's'}</span>
          {d.last_status != null && <span className="text-[#7E91A8]">HTTP {d.last_status}</span>}
          {d.last_error && <span className="text-[#F87171] truncate" title={d.last_error}>{d.last_error}</span>}
          {d.status === 'pending' && d.attempts > 0 && <span className="text-[#7E91A8]">retry <When at={d.next_attempt_at} /></span>}
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
        <button onClick={() => setOpen(o => !o)} className="text-[#3D5068] hover:text-[#DDE6F0] cursor-pointer" aria-label="Show deliveries">
          {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </button>
        <span className={`w-1.5 h-1.5 rounded-full flex-shrink-0 ${!hook.active ? 'bg-[#3D5068]' : failing ? 'bg-[#F59E0B]' : 'bg-[#10B981]'}`} />
        <div className="flex-1 min-w-0">
          <p className="text-sm text-[#DDE6F0] truncate">{hook.name || hook.url}</p>
          <p className="text-[11px] text-[#3D5068] truncate">
            {hook.name && <span className="font-mono">{hook.url} · </span>}
            {hook.format === 'slack' ? 'Slack' : 'JSON'} · {describeEvents(hook.events)}
          </p>
        </div>
        {!hook.active && <Pill tone="gray">off</Pill>}
        {failing && <Pill tone="amber" title={hook.last_error}>{hook.consecutive_failures} failed</Pill>}
        {hook.last_delivery_at && <span className="text-[11px] text-[#3D5068] hidden sm:inline">last <When at={hook.last_delivery_at} /></span>}
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
          <WebhookForm initial={hook} submitLabel="Save" onCancel={() => setEditing(false)}
            onSubmit={async c => { onChanged(await api.updateWebhook(hook.id, c)); setEditing(false) }} />
        </div>
      )}
      {open && <div className="pl-6"><Deliveries webhookId={hook.id} /></div>}
    </li>
  )
}

export function WebhooksPanel({ orgId, projectId, canManage }: Scope & { canManage: boolean }) {
  const [hooks, setHooks] = useState<Webhook[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [secret, setSecret] = useState<Webhook | null>(null)

  const load = useCallback(() => {
    api.listWebhooks({ orgId, projectId }).then(setHooks).catch(e => setError(errMsg(e, 'Failed to load webhooks')))
  }, [orgId, projectId])
  useEffect(() => { if (canManage) load() }, [canManage, load])

  if (!canManage) {
    return <p className="text-xs text-[#3D5068]">{orgId ? 'Org owners and admins' : 'Project owners'} manage webhooks.</p>
  }
  const replace = (id: number) => (h: Webhook | null) =>
    setHooks(list => h ? (list ?? []).map(x => x.id === id ? h : x) : (list ?? []).filter(x => x.id !== id))

  return (
    <div className="space-y-3">
      {error && <p className="text-xs text-[#F87171]">{error}</p>}
      {secret?.secret && (
        <SecretReveal title={`Signing secret for ${secret.name || secret.url}`} secret={secret.secret} onDone={() => setSecret(null)}
          note="Copy it now: it's only shown when the webhook is created or its secret rotated.">
          {secret.format === 'json' ? (
            <p className="text-xs text-[#7E91A8] leading-relaxed">
              Each POST has <code className="text-[#DDE6F0]">X-AMP-Signature: t=&lt;unix&gt;,v1=&lt;hex&gt;</code>, where v1 is
              HMAC-SHA256 of <code className="text-[#DDE6F0]">&lt;t&gt;.&lt;body&gt;</code> with this secret. Reject old t values to stop replays.
            </p>
          ) : (
            <p className="text-xs text-[#7E91A8]">Slack doesn't check signatures; keep this in case you switch the format to JSON.</p>
          )}
        </SecretReveal>
      )}
      {hooks == null ? (
        <Loader2 size={16} className="text-[#6366F1] animate-spin" />
      ) : hooks.length > 0 ? (
        <ul className="rounded-xl border border-[#1E2C45] divide-y divide-[#1E2C45]">
          {hooks.map(h => <WebhookRow key={h.id} hook={h} canManage={canManage} onChanged={replace(h.id)} onSecret={w => { replace(h.id)(w); setSecret(w) }} />)}
        </ul>
      ) : !creating && (
        <p className="text-xs text-[#3D5068]">No webhooks yet.</p>
      )}
      {creating ? (
        <WebhookForm initial={{ format: 'json', events: [] }} submitLabel="Add webhook" onCancel={() => setCreating(false)}
          onSubmit={async c => {
            const h = await api.createWebhook({ orgId, projectId }, c)
            setHooks(list => [...(list ?? []), h]); setSecret(h); setCreating(false)
          }} />
      ) : (
        <button onClick={() => setCreating(true)} className={quietBtnCls}><Plus size={12} />Add webhook</button>
      )}
      {hooks && hooks.length > 0 && <p className="text-[11px] text-[#3D5068]">Failed deliveries are retried for a few hours. After 20 deliveries in a row fail, a webhook turns itself off.</p>}
    </div>
  )
}
