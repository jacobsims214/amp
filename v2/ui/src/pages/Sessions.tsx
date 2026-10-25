/**
 * /sessions — every harness session of yours that is open for remote control, across all your
 * projects and orgs: where it runs, its project, whether the laptop is online, when it started,
 * its last activity and when it expires. Open one, disconnect it, or revoke them all.
 *
 * The list comes from GET /api/sessions (protocol 8.3), which returns the caller's live sessions
 * only. Ended sessions, the model, the paired devices and the current ticket aren't in it; the
 * session page shows the last three once it's open.
 */
import { useCallback, useEffect, useState } from 'react'
import { ExternalLink, MonitorSmartphone, RefreshCw, ShieldOff, Unplug } from 'lucide-react'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { ConfirmButton } from '../components/ui/ConfirmButton'
import { BandLoader } from '../components/brand/BandLoader'
import { useAuth } from '../hooks/useAuth'
import { remoteAvailable, useRemote } from '../remote/remote-context'
import type { SessionSummary } from '../remote/types'
import { ago, inTime } from '../remote/toolText'

const when = (ms: number) => new Date(ms).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })

function StateBadge({ online }: { online: boolean }) {
  return (
    <span className={`inline-flex items-center gap-1.5 border px-2 py-0.5 text-[11px] font-semibold whitespace-nowrap ${online ? 'border-success text-success-ink' : 'border-danger text-danger-ink'}`}>
      <span className="w-1.5 h-1.5 rounded-full bg-current" aria-hidden="true" />{online ? 'Laptop online' : 'Laptop offline'}
    </span>
  )
}

function Fact({ label, children, title }: { label: string; children: React.ReactNode; title?: string }) {
  return (
    <div className="min-w-0" title={title}>
      <dt className="amp-label !text-[10px]">{label}</dt>
      <dd className="m-0 mt-0.5 amp-num text-[12px] text-ink truncate">{children}</dd>
    </div>
  )
}

function SessionRow({ s, now, onDisconnect, busy }: { s: SessionSummary; now: number; onDisconnect: () => void; busy: boolean }) {
  // A full page load (not client-side routing), so the session page gets its strict CSP header.
  const href = `/sessions/${encodeURIComponent(s.session_id)}`
  return (
    <li className="px-4 py-3 border-t border-line first:border-t-0 grid gap-2.5" data-testid="session-row">
      <div className="flex items-center gap-2.5 flex-wrap min-w-0">
        <MonitorSmartphone size={18} className="text-ink shrink-0" aria-hidden="true" />
        <a href={href} className="text-[14.5px] font-semibold text-ink hover:underline truncate min-w-0">{s.host}</a>
        <StateBadge online={s.online} />
        <span className="text-[12px] text-ink-2 truncate min-w-0">{s.project ? `${s.project.name} #${s.project.id}` : 'no project'}</span>
        <span className="ml-auto flex items-center gap-1.5">
          <a href={href} className="amp-btn amp-btn-sm amp-btn-primary"><ExternalLink size={12} />Open</a>
          <ConfirmButton label="Disconnect" confirmLabel="Disconnect now" icon={<Unplug size={12} />} busy={busy} onConfirm={onDisconnect} />
        </span>
      </div>
      <dl className="m-0 grid grid-cols-2 sm:grid-cols-4 lg:grid-cols-6 gap-x-4 gap-y-2">
        <Fact label="Started" title={when(s.started_at_ms)}>{ago(s.started_at_ms, now)}</Fact>
        <Fact label="Last activity" title={when(s.last_activity_at_ms)}>{ago(s.last_activity_at_ms, now)}</Fact>
        <Fact label="Expires" title={when(s.expires_at_ms)}>{inTime(s.expires_at_ms, now)}</Fact>
        <Fact label="Watching">{s.viewers === 1 ? '1 browser' : `${s.viewers} browsers`}</Fact>
        <Fact label="Laptop">{s.os} · harness {s.harness_version}</Fact>
        <Fact label="Laptop key" title="Check it matches the key the harness prints">{s.harness_fp_display}</Fact>
      </dl>
    </li>
  )
}

export function Sessions() {
  const { me, loading } = useAuth()
  const deps = useRemote()
  const { transport } = deps
  const [sessions, setSessions] = useState<SessionSummary[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const available = remoteAvailable(me, deps)

  const load = useCallback(() => transport.listSessions()
    .then(s => { setSessions(s); setError(null); setNow(Date.now()) })
    .catch(e => setError(e instanceof Error ? e.message : 'Failed to load sessions')), [transport])

  useEffect(() => {
    if (!available) return
    load()
    const t = setInterval(load, 15_000)
    return () => clearInterval(t)
  }, [available, load])

  const disconnect = async (id: string) => {
    setBusy(id)
    try { await transport.disconnect(id); setNote('Session disconnected. The laptop session keeps running locally.') }
    catch (e) { setError(e instanceof Error ? e.message : 'Disconnect failed') }
    finally { setBusy(null); load() }
  }
  const revokeAll = async () => {
    setBusy('all')
    try {
      const r = await transport.revokeAll()
      setNote(`Ended ${r.ended} session${r.ended === 1 ? '' : 's'} and forgot ${r.devices_revoked} paired device${r.devices_revoked === 1 ? '' : 's'}. Pair again from the laptop to use remote control.`)
    } catch (e) { setError(e instanceof Error ? e.message : 'Revoke failed') }
    finally { setBusy(null); load() }
  }

  if (loading) return <div className="flex justify-center py-16"><BandLoader label="Loading" /></div>
  if (!available) {
    return (
      <div className="flex flex-col h-full">
        <PageHeader crumb="Remote control" title="Sessions" />
        <p className="m-0 px-4 sm:px-7 py-6 text-[13.5px] text-ink-2 max-w-[640px]">Remote control of harness sessions is off on this AMP server or for your organization. An org admin can turn it on in the org's settings.</p>
      </div>
    )
  }

  const online = sessions?.filter(s => s.online).length ?? 0
  return (
    <div className="flex flex-col min-h-full bg-bg">
      <PageHeader crumb="Remote control" title="Sessions"
        meta={<>Harness sessions you opened with <code className="font-mono">/remote</code>, in every project and org.</>}
        actions={<>
          <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => load()} aria-label="Refresh"><RefreshCw size={12} />Refresh</button>
          {!!sessions?.length && <ConfirmButton danger label="Revoke all" confirmLabel="End all and forget devices" icon={<ShieldOff size={12} />}
            busy={busy === 'all'} onConfirm={() => { revokeAll() }} />}
        </>} />

      <div className="px-4 sm:px-7 py-5 grid gap-4 max-w-[1100px]">
        {note && <p className="m-0 border-l-[3px] border-success bg-surface px-3 py-2 text-[13px] text-ink-2" role="status">{note}</p>}
        {error && <p className="m-0 border-l-[3px] border-danger bg-surface px-3 py-2 text-[13px] text-danger-ink" role="alert">{error}</p>}
        <Panel label="Live" title={sessions == null ? 'Loading…' : sessions.length ? `${sessions.length} session${sessions.length === 1 ? '' : 's'}` : 'No live sessions'}
          right={sessions?.length ? <span>{online} online · {sessions.length - online} offline</span> : undefined}>
          {sessions == null && !error && <div className="flex justify-center py-8"><BandLoader label="Loading sessions" /></div>}
          {sessions?.length === 0 && (
            <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Run <code className="font-mono">/remote</code> in the harness on your laptop, then open the link it prints on this device or your phone.</p>
          )}
          {!!sessions?.length && (
            <ul className="m-0 p-0 list-none" aria-label="Sessions">
              {sessions.map(s => <SessionRow key={s.session_id} s={s} now={now} busy={busy === s.session_id} onDisconnect={() => { disconnect(s.session_id) }} />)}
            </ul>
          )}
        </Panel>
        <p className="m-0 text-[12px] text-ink-3 max-w-[720px]">
          Ended sessions aren't listed: AMP keeps only live ones here. The model, current ticket and paired devices show inside each session.
          Disconnecting ends remote control; the session keeps running on the laptop.
        </p>
      </div>
    </div>
  )
}
