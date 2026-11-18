/**
 * /sessions — every harness session of yours that is open for remote control, across all your
 * projects and orgs: where it runs, its project, whether the laptop is online, when it started,
 * its last activity and when it expires, the model, the current ticket, the paired devices and
 * how many workers it is running. Open one, disconnect it, or revoke them all.
 *
 * This page is where remote control starts on a phone, so the live sessions come first and the
 * ones that ended in the last 7 days are folded away behind a count (#4898): reaching a live
 * session never means scrolling past a long history. A row is a phone card — the host and its
 * actions on one line, the facts in a two-up grid — rather than the six-column desktop strip.
 *
 * The list comes from GET /api/sessions (protocol 8.3). A relay that predates #2389 leaves out
 * state, model, ticket and devices: every session it lists is live, and the facts it lacks show
 * "—". A relay that predates #4898 also leaves out workers_running, which then shows "—" too.
 */
import { useCallback, useEffect, useState } from 'react'
import { ChevronDown, ExternalLink, History, MonitorSmartphone, RefreshCw, ShieldOff, Unplug } from 'lucide-react'
import { Link } from 'react-router-dom'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { ConfirmButton } from '../components/ui/ConfirmButton'
import { BandLoader } from '../components/brand/BandLoader'
import { useAuth } from '../hooks/useAuth'
import { useVisibleInterval } from '../hooks/useVisibleInterval'
import { remoteAvailable, useRemote } from '../remote/remote-context'
import type { SessionDevice, SessionSummary } from '../remote/types'
import { ago, inTime } from '../remote/toolText'
import { endReasonLabel, endReasonText } from '../remote/session'
import { projectLink } from '../lib/projectLink'

const when = (ms: number) => new Date(ms).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })

const isEnded = (s: SessionSummary) => s.state === 'ended'

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

function StateBadge({ s }: { s: SessionSummary }) {
  if (isEnded(s)) {
    return (
      <span className="inline-flex items-center gap-1.5 border border-line-str px-2 py-0.5 text-[11px] font-semibold whitespace-nowrap text-ink-2"
        title={endReasonText(s.end_reason ?? '')}>
        <span className="w-1.5 h-1.5 rounded-full bg-current" aria-hidden="true" />Ended · {endReasonLabel(s.end_reason)}
      </span>
    )
  }
  return (
    <span className={`inline-flex items-center gap-1.5 border px-2 py-0.5 text-[11px] font-semibold whitespace-nowrap ${s.online ? 'border-success text-success-ink' : 'border-danger text-danger-ink'}`}>
      <span className="w-1.5 h-1.5 rounded-full bg-current" aria-hidden="true" />{s.online ? 'Laptop online' : 'Laptop offline'}
    </span>
  )
}

function Fact({ label, children, title, wide }: { label: string; children: React.ReactNode; title?: string; wide?: boolean }) {
  return (
    <div className={`min-w-0 ${wide ? 'col-span-full' : ''}`} title={title}>
      <dt className="amp-label !text-[10px]">{label}</dt>
      {/* A wide fact is the one thing a person must be able to read in full, so it wraps
          instead of truncating (the "why it ended" copy is the only place they learn it). */}
      <dd className={`m-0 mt-0.5 amp-num text-[12px] text-ink ${wide ? '' : 'truncate'}`}>{children}</dd>
    </div>
  )
}

function TicketFact({ s }: { s: SessionSummary }) {
  const t = s.ticket
  if (!t) return <Fact label="Ticket">—</Fact>
  const text = t.name ? `#${t.id} ${t.name}` : `#${t.id}`
  return (
    <Fact label="Ticket" title={text}>
      {s.project
        ? <Link to={projectLink(s.project.id, 'board', t.id)} className="text-link hover:underline">{text}</Link>
        : text}
    </Fact>
  )
}

function DevicesFact({ devices, now }: { devices: SessionDevice[] | undefined; now: number }) {
  if (devices == null) return <Fact label="Paired devices">—</Fact>
  if (devices.length === 0) return <Fact label="Paired devices">None</Fact>
  const title = devices.map(d => `${d.name || 'Unnamed browser'}: paired ${when(d.paired_at_ms)}, trusted until ${when(d.expires_at_ms)}`).join('\n')
  return (
    <Fact label={devices.length === 1 ? 'Paired device' : `Paired devices (${devices.length})`} title={title}>
      {devices.map(d => d.name || 'Unnamed browser').join(', ')}
      {devices.length === 1 && <span className="text-ink-3"> · {inTime(devices[0].expires_at_ms, now)}</span>}
    </Fact>
  )
}

/**
 * "3 workers · 2 watching": what a session is doing right now, in the words a person would use.
 * This is the line the list is scanned for, so it sits right under the host name. A relay that
 * predates #4898 sends no worker count, so that half reads "—" instead of a made-up 0.
 */
function LiveCounts({ s }: { s: SessionSummary }) {
  const workers = s.workers_running
  return (
    <span className="flex items-center gap-2 flex-wrap text-[12px] text-ink-2 amp-num" data-testid="live-counts">
      <span className={`inline-flex items-center gap-1 whitespace-nowrap ${workers && workers > 0 ? 'font-semibold text-ink' : ''}`}
        title="Workers this session is running right now">
        {workers == null ? '— workers' : plural(workers, 'worker')}
      </span>
      <span className="text-ink-3" aria-hidden="true">·</span>
      <span className="whitespace-nowrap" title="Browsers watching this session right now">
        {plural(s.viewers, 'watching', 'watching')}
      </span>
    </span>
  )
}

function SessionRow({ s, now, onDisconnect, busy }: { s: SessionSummary; now: number; onDisconnect: () => void; busy: boolean }) {
  const ended = isEnded(s)
  // A full page load (not client-side routing), so the session page gets its strict CSP header.
  const href = `/sessions/${encodeURIComponent(s.session_id)}`
  return (
    <li className="px-3 sm:px-4 py-3 border-t border-line first:border-t-0 grid gap-2.5" data-testid={ended ? 'ended-session-row' : 'session-row'}>
      {/* The host and the actions share one line, so Open and Disconnect are visible targets with
          no hover and no overflow menu to find. On the narrowest phones the actions drop below the
          name instead of squeezing it to "jacob…". */}
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 min-w-0">
        <MonitorSmartphone size={18} className={`${ended ? 'text-ink-3' : 'text-ink'} shrink-0`} aria-hidden="true" />
        <div className="min-w-0 flex-1 grid gap-0.5">
          {ended
            ? <span className="text-[14.5px] font-semibold text-ink-2 truncate">{s.host}</span>
            : <a href={href} className="text-[14.5px] font-semibold text-ink hover:underline truncate">{s.host}</a>}
          <span className="text-[12px] text-ink-2 truncate">{s.project ? `${s.project.name} #${s.project.id}` : 'no project'}</span>
        </div>
        {!ended && (
          <div className="flex items-center gap-1.5 shrink-0 [&_.amp-btn]:min-h-[40px] sm:[&_.amp-btn]:min-h-0 max-[359px]:w-full max-[359px]:order-last">
            {/* 40px tall on a phone: a thumb target, not a desktop 26px chip. */}
            <a href={href} className="amp-btn amp-btn-sm amp-btn-primary"><ExternalLink size={12} />Open</a>
            <ConfirmButton label="Disconnect" confirmLabel="Disconnect now" icon={<Unplug size={12} />} busy={busy} onConfirm={onDisconnect} />
          </div>
        )}
      </div>
      <div className="flex items-start gap-2 min-w-0 flex-wrap">
        <StateBadge s={s} />
        {!ended && <LiveCounts s={s} />}
      </div>
      <dl className="m-0 grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-x-4 gap-y-2">
        {ended && <Fact wide label="Why" title={endReasonText(s.end_reason ?? '')}>{endReasonText(s.end_reason ?? '')}</Fact>}
        <Fact label="Model" title={s.model ?? undefined}>{s.model ?? '—'}</Fact>
        <TicketFact s={s} />
        <DevicesFact devices={s.devices} now={now} />
        {ended
          ? <Fact label="Ended" title={s.ended_at_ms ? when(s.ended_at_ms) : undefined}>{s.ended_at_ms ? ago(s.ended_at_ms, now) : '—'}</Fact>
          : <Fact label="Last activity" title={when(s.last_activity_at_ms)}>{ago(s.last_activity_at_ms, now)}</Fact>}
        <Fact label="Started" title={when(s.started_at_ms)}>{ago(s.started_at_ms, now)}</Fact>
        {!ended && (s.expires_at_ms == null
          ? <Fact label="Expires" title="No time limit: it stays on, reconnecting after sleep, until you disconnect it or turn it off on the laptop">Never</Fact>
          : <Fact label="Expires" title={when(s.expires_at_ms)}>{inTime(s.expires_at_ms, now)}</Fact>)}
        <Fact label="Laptop">{s.os} · harness {s.harness_version}</Fact>
        <Fact label="Laptop key" title="Check it matches the key the harness prints">{s.harness_fp_display}</Fact>
      </dl>
    </li>
  )
}

/**
 * The ended sessions, folded away. A person who opened this page on a phone came for the live
 * session, so a week of history stays one row tall until they ask for it (#4898).
 */
function EndedSessions({ sessions, now }: { sessions: SessionSummary[]; now: number }) {
  const [open, setOpen] = useState(false)
  return (
    <Panel label="Ended" title={plural(sessions.length, 'session') + ' in the last 7 days'} right={<History size={13} aria-hidden="true" />}>
      <button type="button" className="w-full flex items-center gap-2 px-4 py-2.5 text-left text-[12.5px] text-ink-2 hover:bg-surface-2"
        aria-expanded={open} aria-controls="ended-sessions" onClick={() => setOpen(o => !o)} data-testid="ended-toggle">
        <ChevronDown size={14} className={`shrink-0 transition-transform ${open ? 'rotate-180' : ''}`} aria-hidden="true" />
        <span>{open ? 'Hide ended sessions' : `Show ${plural(sessions.length, 'ended session')}`}</span>
        <span className="ml-auto amp-num text-[11px] text-ink-3" aria-hidden="true">{sessions.length}</span>
      </button>
      {open && (
        <ul id="ended-sessions" className="m-0 p-0 list-none border-t border-line" aria-label="Ended sessions">
          {sessions.map(s => <SessionRow key={s.session_id} s={s} now={now} busy={false} onDisconnect={() => {}} />)}
        </ul>
      )}
    </Panel>
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
    if (available) load()
  }, [available, load])
  // Refresh every 15 s while the tab is visible. The hook skips the refresh in a
  // background tab and runs it once when the tab is shown again.
  useVisibleInterval(load, available ? 15_000 : null)

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

  const live = sessions?.filter(s => !isEnded(s)) ?? null
  const ended = sessions?.filter(isEnded) ?? []
  const online = live?.filter(s => s.online).length ?? 0
  return (
    <div className="flex flex-col min-h-full bg-bg">
      <PageHeader crumb="Remote control" title="Sessions"
        meta={<>Harness sessions you opened with <code className="font-mono">/remote</code>, in every project and org.</>}
        actions={<>
          <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => load()} aria-label="Refresh"><RefreshCw size={12} />Refresh</button>
          {!!live?.length && <ConfirmButton danger label="Revoke all" confirmLabel="End all and forget devices" icon={<ShieldOff size={12} />}
            busy={busy === 'all'} onConfirm={() => { revokeAll() }} />}
        </>} />

      <div className="px-3 sm:px-7 py-4 sm:py-5 grid gap-4 max-w-[1100px]">
        {note && <p className="m-0 border-l-[3px] border-success bg-surface px-3 py-2 text-[13px] text-ink-2" role="status">{note}</p>}
        {error && <p className="m-0 border-l-[3px] border-danger bg-surface px-3 py-2 text-[13px] text-danger-ink" role="alert">{error}</p>}
        <Panel label="Live" title={live == null ? 'Loading…' : live.length ? `${live.length} session${live.length === 1 ? '' : 's'}` : 'No live sessions'}
          right={live?.length ? <span>{online} online · {live.length - online} offline</span> : undefined}>
          {sessions == null && !error && <div className="flex justify-center py-8"><BandLoader label="Loading sessions" /></div>}
          {live?.length === 0 && (
            <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Run <code className="font-mono">/remote</code> in the harness on your laptop, then open the link it prints on this device or your phone.</p>
          )}
          {!!live?.length && (
            <ul className="m-0 p-0 list-none" aria-label="Sessions">
              {live.map(s => <SessionRow key={s.session_id} s={s} now={now} busy={busy === s.session_id} onDisconnect={() => { disconnect(s.session_id) }} />)}
            </ul>
          )}
        </Panel>
        {/* Ended sessions never share the live list: a week of history is one folded row (#4898). */}
        {!!ended.length && <EndedSessions sessions={ended} now={now} />}
        <p className="m-0 text-[12px] text-ink-3 max-w-[720px]">
          Ended sessions stay listed for 7 days. Disconnecting ends remote control; the session keeps running on the laptop.
        </p>
      </div>
    </div>
  )
}
