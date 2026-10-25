/**
 * /sessions/:sessionId — control a harness session running on your laptop
 * (remote control M1: chat, Stop, tool calls and status). The laptop does the
 * work; AMP only relays. Standalone (no sidebar) so it works on a phone.
 *
 * Served with a strict CSP (src/remote/csp.ts). Links into this page are
 * full page loads so the header applies.
 */
import { useEffect, useLayoutEffect, useRef } from 'react'
import { useParams } from 'react-router-dom'
import { ArrowLeft, MonitorSmartphone, Unplug } from 'lucide-react'
import { useAuth } from '../hooks/useAuth'
import { remoteAvailable, useRemote } from '../remote/remote-context'
import { useRemoteSession } from '../remote/useRemoteSession'
import { endReasonText, isBusy } from '../remote/session'
import type { SessionStatus } from '../remote/types'
import { BandLoader } from '../components/brand/BandLoader'
import { PairingPanel } from '../components/remote/PairingPanel'
import { ConfirmButton } from '../components/ui/ConfirmButton'
import {
  AmpConnectionBadge, Banner, Composer, HeaderFacts, OutgoingRow, PresenceBadge, QueueStrip, StatusLine, TicketLine, ToolGroup,
  TranscriptRow, WorkerChips, WorkingLine,
} from '../components/remote/SessionParts'
import { groupRows, useNow } from '../remote/transcriptView'

const SESSION_ID = /^rs_[A-Za-z0-9_-]{22}$/

function Frame({ title, children, right, sub }: { title: React.ReactNode; children: React.ReactNode; right?: React.ReactNode; sub?: React.ReactNode }) {
  return (
    <div className="flex flex-col h-dvh bg-bg">
      <header className="bg-surface border-b border-line px-3 sm:px-5 py-2 flex items-center gap-x-3 gap-y-1 flex-wrap">
        {/* Full page loads: this page has its own strict CSP, the rest of the app doesn't. */}
        <a href="/sessions" className="amp-btn amp-btn-ghost amp-btn-sm" aria-label="All sessions"><ArrowLeft size={14} />Sessions</a>
        <div className="min-w-0 flex items-baseline gap-x-2.5 gap-y-0 flex-wrap">
          <h1 className="amp-heading m-0 text-[17px] text-ink min-w-0 truncate inline-flex items-center gap-1.5"><MonitorSmartphone size={15} className="shrink-0 self-center" aria-hidden="true" />{title}</h1>
          {sub}
        </div>
        {right && <div className="ml-auto flex items-center gap-2 flex-wrap">{right}</div>}
      </header>
      {children}
    </div>
  )
}

function Message({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Frame title="Remote session">
      <div className="p-5 sm:p-8 max-w-[560px]">
        <span className="amp-label">Remote control</span>
        <h2 className="amp-heading m-0 mt-2 text-[20px] text-ink">{title}</h2>
        <div className="text-[13.5px] text-ink-2 mt-2 leading-relaxed grid gap-2">{children}</div>
      </div>
    </Frame>
  )
}

export function Session() {
  const { sessionId = '' } = useParams()
  const { me, loading } = useAuth()
  const deps = useRemote()
  if (loading) return <div className="flex justify-center py-16"><BandLoader label="Loading" /></div>
  if (!remoteAvailable(me, deps)) {
    return <Message title="Remote control isn't available">
      <p className="m-0">Remote control of harness sessions is off on this AMP server or for your organization. An org admin can turn it on in the org's settings.</p>
    </Message>
  }
  // The page takes only the session id from its URL (T-UI-8).
  if (!SESSION_ID.test(sessionId)) return <Message title="No such session"><p className="m-0">That isn't a remote session link.</p></Message>
  return <SessionView sessionId={sessionId} />
}

function SessionView({ sessionId }: { sessionId: string }) {
  const { phase, state, sendPrompt, stop, disconnect, paired, canSign } = useRemoteSession(sessionId)
  const scroller = useRef<HTMLDivElement>(null)
  const stick = useRef(true)
  const busy = isBusy(state)
  // Live timers (thinking, running calls, workers, the working line, uptime) tick once a second.
  const now = useNow(phase.kind === 'ready')

  // Follow the bottom while the reader is there; leave them alone if they scrolled up.
  useLayoutEffect(() => {
    const el = scroller.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [state.items, state.outgoing])
  useEffect(() => {
    const el = scroller.current
    if (!el) return
    const onScroll = () => { stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80 }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [phase.kind])

  if (phase.kind === 'loading') return <Frame title="Remote session"><div className="flex justify-center py-16"><BandLoader label="Connecting to the session" /></div></Frame>
  if (phase.kind === 'not_found') return <Message title="No such session"><p className="m-0">It may have ended, or it belongs to someone else. Only the person who started a session can open it.</p></Message>
  if (phase.kind === 'gone') return <Message title="This session has ended"><p className="m-0">Run <code className="font-mono">/remote</code> in the harness on your laptop to start a new one.</p></Message>
  if (phase.kind === 'error') return <Message title="Couldn't open the session"><p className="m-0">{phase.message}</p></Message>
  if (phase.kind === 'pairing') {
    return <Frame title="Remote session"><PairingPanel detail={phase.detail} link={phase.link} onPaired={paired} /></Frame>
  }
  if (phase.kind === 'unpaired') {
    return <Message title="Pair this device from the laptop">
      <p className="m-0">This browser isn't paired with <b className="text-ink">{phase.detail.host}</b> yet, so it can't show or send anything for this session.</p>
      <p className="m-0">On the laptop, run <code className="font-mono">/remote</code> in the harness and open the link it prints (or scan its QR code) on this device. Then press <b>y</b> on the laptop when the codes match.</p>
      <p className="m-0 amp-num text-[12px] text-ink-3">Laptop key {phase.detail.harness_fp_display}</p>
    </Message>
  }

  const { detail } = phase
  const host = state.hello?.harness.host ?? detail.host
  const online = state.online === true && !state.ended
  const ended = state.ended
  const disabledReason = ended ? endReasonText(ended.reason, state.hello?.idle_timeout_s)
    : state.versionMismatch ? 'Update AMP to talk to this laptop.'
    : !canSign ? 'This browser can\'t sign messages yet. Pair it from the laptop.'
    : state.online === false ? 'The laptop is offline. Messages aren\'t queued while it\'s away.'
    : state.online === null ? 'Connecting…' : null
  const composerDisabled = disabledReason !== null
  const visibleOutgoing = state.outgoing.filter(o => o.state !== 'queued')
  const rows = groupRows(state.items)
  // A retry shows while it is the latest thing that happened in the run (the TUI's status slot).
  const lastItem = state.items.at(-1)
  const retry = busy && lastItem?.kind === 'retry' ? lastItem : null
  const status = state.status ?? (detail.project ? { ...emptyStatus, project: detail.project } : null)

  return (
    <Frame title={host} sub={<HeaderFacts status={status} os={state.hello?.harness.os ?? detail.os} />} right={<>
      <AmpConnectionBadge status={status} />
      <PresenceBadge online={ended ? false : state.online} connection={state.connection} />
      {!ended && <ConfirmButton label="Disconnect" confirmLabel="Disconnect now" icon={<Unplug size={13} />}
        onConfirm={() => { disconnect().catch(() => {}) }} />}
    </>}>
      <TicketLine status={status} />
      {ended && <Banner tone="info">{endReasonText(ended.reason, state.hello?.idle_timeout_s)} The laptop session keeps running locally.</Banner>}
      {state.versionMismatch && <Banner tone="danger">This laptop runs a newer harness; update AMP. Nothing more from it is shown.</Banner>}
      {!ended && state.online === false && <Banner tone="warn">Laptop offline. It may be asleep or off the network; this page catches up when it's back.</Banner>}
      {!ended && state.connection === 'reconnecting' && state.online !== false && <Banner tone="info">Reconnecting to AMP… Nothing is lost: the page picks up where it left off.</Banner>}

      <div ref={scroller} className="flex-1 min-h-0 overflow-y-auto" aria-label="Transcript" aria-live="polite">
        <div className="max-w-[860px] mx-auto px-3 sm:px-5 py-4 grid grid-cols-[minmax(0,1fr)] gap-3.5">
          {state.items.length === 0 && visibleOutgoing.length === 0 && (
            <p className="m-0 text-[13px] text-ink-3 text-center py-8">{state.connection === 'live' ? 'Nothing here yet. Send a message to the agent.' : 'Connecting…'}</p>
          )}
          {rows.map(r => r.kind === 'tools'
            ? <ToolGroup key={r.key} calls={r.calls} workers={state.workers} now={now} />
            : <TranscriptRow key={r.item.key} item={r.item} now={now} myDeviceId={phase.deviceId} />)}
          {visibleOutgoing.map(o => <OutgoingRow key={`${o.op}-${o.seq}`} out={o} />)}
        </div>
      </div>

      <WorkerChips workers={state.workers} now={now} />
      <WorkingLine busy={busy && !ended} runStartedAt={state.runStartedAt} retry={retry} compaction={ended ? null : state.compaction} now={now} />
      <QueueStrip queue={state.queue} myDeviceId={phase.deviceId} online={online} onClear={stop} />
      <Composer disabled={composerDisabled} disabledReason={disabledReason} busy={busy} online={online && canSign}
        onSend={sendPrompt} onStop={stop} />
      <div className="pb-[env(safe-area-inset-bottom)] bg-surface-2">
        <StatusLine status={status} totals={state.totals} startedAt={state.hello?.started_at_ms ?? detail.started_at_ms} workers={state.workers} now={now} />
      </div>
    </Frame>
  )
}

const emptyStatus: SessionStatus = {
  model: null, busy: false, context_pct: null, cost_usd_micros: null, project: null, ticket_id: null, branch: null,
  queue_len: 0, workers_running: 0, devices: [],
}
