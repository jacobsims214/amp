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
import { ArrowLeft, Unplug } from 'lucide-react'
import { useAuth } from '../hooks/useAuth'
import { remoteAvailable, useRemote } from '../remote/remote-context'
import { useRemoteSession } from '../remote/useRemoteSession'
import { endReasonText, isBusy } from '../remote/session'
import { BandLoader } from '../components/brand/BandLoader'
import { PairingPanel } from '../components/remote/PairingPanel'
import { ConfirmButton } from '../components/ui/ConfirmButton'
import {
  Banner, Composer, OutgoingRow, PresenceBadge, QueueStrip, StatusBar, TranscriptRow, WorkerChips,
} from '../components/remote/SessionParts'

const SESSION_ID = /^rs_[A-Za-z0-9_-]{22}$/

function Frame({ title, children, right }: { title: React.ReactNode; children: React.ReactNode; right?: React.ReactNode }) {
  return (
    <div className="flex flex-col h-dvh bg-bg">
      <header className="bg-surface border-b border-line px-3 sm:px-5 py-2.5 flex items-center gap-3 flex-wrap">
        <a href="/" className="amp-btn amp-btn-ghost amp-btn-sm" aria-label="Back to My work"><ArrowLeft size={14} />My work</a>
        <h1 className="amp-heading m-0 text-[17px] text-ink min-w-0 truncate">{title}</h1>
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
  const busy = isBusy(state)
  const ended = state.ended
  const disabledReason = ended ? endReasonText(ended.reason)
    : state.versionMismatch ? 'Update AMP to talk to this laptop.'
    : !canSign ? 'This browser can\'t sign messages yet. Pair it from the laptop.'
    : state.online === false ? 'The laptop is offline. Messages aren\'t queued while it\'s away.'
    : state.online === null ? 'Connecting…' : null
  const composerDisabled = disabledReason !== null
  const visibleOutgoing = state.outgoing.filter(o => o.state !== 'queued')

  return (
    <Frame title={host} right={<>
      <PresenceBadge online={ended ? false : state.online} connection={state.connection} />
      {!ended && <ConfirmButton label="Disconnect" confirmLabel="Disconnect now" icon={<Unplug size={13} />}
        onConfirm={() => { disconnect().catch(() => {}) }} />}
    </>}>
      <div className="bg-surface border-b border-line px-3 sm:px-5 py-2 grid gap-1.5">
        <StatusBar status={state.status} host={host} />
        <WorkerChips workers={state.workers} />
      </div>
      {ended && <Banner tone="info">{endReasonText(ended.reason)} The laptop session keeps running locally.</Banner>}
      {state.versionMismatch && <Banner tone="danger">This laptop runs a newer harness; update AMP. Nothing more from it is shown.</Banner>}
      {!ended && state.online === false && <Banner tone="warn">Laptop offline. It may be asleep or off the network; this page catches up when it's back.</Banner>}

      <div ref={scroller} className="flex-1 min-h-0 overflow-y-auto" aria-label="Transcript" aria-live="polite">
        <div className="max-w-[860px] mx-auto px-3 sm:px-5 py-4 grid grid-cols-[minmax(0,1fr)] gap-3">
          {state.items.length === 0 && visibleOutgoing.length === 0 && (
            <p className="m-0 text-[13px] text-ink-3 text-center py-8">{state.connection === 'live' ? 'Nothing here yet. Send a message to the agent.' : 'Connecting…'}</p>
          )}
          {state.items.map(it => <TranscriptRow key={it.key} item={it} />)}
          {visibleOutgoing.map(o => <OutgoingRow key={`${o.op}-${o.seq}`} out={o} />)}
          {busy && <div className="text-[12px] text-ink-3" role="status">The agent is working…</div>}
        </div>
      </div>

      <QueueStrip queue={state.queue} myDeviceId={phase.deviceId} online={online} onClear={stop} />
      <Composer disabled={composerDisabled} disabledReason={disabledReason} busy={busy} online={online && canSign}
        onSend={sendPrompt} onStop={stop} />
    </Frame>
  )
}
