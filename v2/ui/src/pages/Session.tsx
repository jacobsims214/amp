/**
 * /sessions/:sessionId — control a harness session running on your laptop
 * (remote control M1: chat, Stop, tool calls and status). The laptop does the
 * work; AMP only relays. Standalone (no sidebar) so it works on a phone.
 *
 * Served with a strict CSP (src/remote/csp.ts). Links into this page are
 * full page loads so the header applies.
 */
import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { ArrowLeft, MonitorSmartphone, ScanLine, Unplug } from 'lucide-react'
import { useAuth } from '../hooks/useAuth'
import { remoteAvailable, useRemote } from '../remote/remote-context'
import { acceptPairingLink } from '../remote/pairingLink'
import { useRemoteSession } from '../remote/useRemoteSession'
import { endReasonText, isBusy } from '../remote/session'
import type { SessionDetail, SessionStatus } from '../remote/types'
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
  const { phase, state, sendPrompt, stop, disconnect, paired, retryLoad, canSign } = useRemoteSession(sessionId)
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
  if (phase.kind === 'error' && phase.retrying) {
    // A passing failure (relay down, network): the page keeps trying and opens the session by itself (#2455).
    return <Message title="Waiting to open the session">
      <div role="status" className="grid gap-2">
        <p className="m-0">{phase.message}</p>
        <p className="m-0">This page keeps trying and opens the session as soon as it can. Nothing on the laptop is lost.</p>
      </div>
      <div><button type="button" className="amp-btn amp-btn-sm" onClick={retryLoad}>Try now</button></div>
    </Message>
  }
  if (phase.kind === 'error') return <Message title="Couldn't open the session"><p className="m-0">{phase.message}</p></Message>
  if (phase.kind === 'pairing') {
    return <Frame title="Remote session"><PairingPanel detail={phase.detail} link={phase.link} onPaired={paired} /></Frame>
  }
  if (phase.kind === 'unpaired') {
    // `relink` re-reads the session with the link now held in memory: the same `pairing` phase a
    // page load from the laptop's link reaches (`useRemoteSession`), so both entries pair identically.
    return <Message title="Pair this device from the laptop">
      <p className="m-0">This browser isn't paired with <b className="text-ink">{phase.detail.host}</b> yet, so it can't show or send anything for this session.</p>
      <LinkEntry detail={phase.detail} onAccepted={retryLoad} />
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

/**
 * Getting the pairing link into this page by hand (#4902).
 *
 * Pairing normally starts from the fragment of the page load that runs this screen (protocol 7.1),
 * which is what a QR scan by the phone's camera gives you in a browser. An installed app is never
 * handed that page load — no https link can be routed into a home-screen web app on iOS, and this
 * build ships no manifest for Android to verify (discoveries/remote-pairing-pwa-divergence.md) — so
 * the same link can be pasted or scanned here instead. `acceptPairingLink` runs the identical checks
 * and keeps the secret in module memory only; nothing about it is stored.
 */
function LinkEntry({ detail, onAccepted }: { detail: SessionDetail; onAccepted: () => void }) {
  const [text, setText] = useState('')
  const [problem, setProblem] = useState<string | null>(null)
  const [elsewhere, setElsewhere] = useState<{ href: string } | null>(null)
  const inputId = useId()

  const take = (raw: string) => {
    const result = acceptPairingLink(raw, detail.session_id)
    if (result.kind === 'refused') { setProblem(result.message); return }
    // Another session on this AMP: its own URL is a fresh page load, which captures the link there
    // exactly as the laptop's link does in a browser (protocol 7.1).
    if (result.kind === 'other_session') { setProblem(null); setElsewhere(result); return }
    setProblem(null)
    setElsewhere(null)
    setText('') // held in memory now: don't leave the secret sitting in a form field
    onAccepted()
  }

  return (
    <div className="grid gap-2 mt-1">
      <p className="m-0">On the laptop, run <code className="font-mono">/remote</code> in the harness. Then paste the link it prints here — in an installed app the link can't open by itself.</p>
      <form className="grid gap-1.5" onSubmit={e => { e.preventDefault(); take(text) }}>
        <label htmlFor={inputId} className="amp-label">Pairing link</label>
        <input id={inputId} className="amp-input amp-num !text-[12.5px]" value={text} onChange={e => setText(e.target.value)}
          placeholder="https://…/sessions/rs_…#p=…&h=…" autoComplete="off" autoCapitalize="none" autoCorrect="off" spellCheck={false} inputMode="url" />
        {problem && <p role="alert" className="m-0 text-[12.5px] text-danger-ink leading-snug">{problem}</p>}
        {elsewhere && <p className="m-0 text-[12.5px] text-ink-2 leading-snug">That link is for another session. <a className="underline" href={elsewhere.href}>Open that session</a> to pair it.</p>}
        <div className="flex items-center gap-2 flex-wrap">
          <button type="submit" className="amp-btn amp-btn-primary amp-btn-sm" disabled={!text.trim()}>Pair this device</button>
          <ScanForLink onText={take} />
        </div>
      </form>
      <p className="m-0">Then press <b>y</b> on the laptop when the codes match.</p>
    </div>
  )
}

/** The `BarcodeDetector` shape this build uses, where the platform has it (Chrome on Android does; Safari doesn't). */
interface DetectedBarcode { rawValue: string }
interface BarcodeDetectorLike { detect(source: CanvasImageSource): Promise<DetectedBarcode[]> }
type BarcodeDetectorCtor = new (opts?: { formats?: string[] }) => BarcodeDetectorLike
const barcodeDetector = (): BarcodeDetectorCtor | undefined => (window as unknown as { BarcodeDetector?: BarcodeDetectorCtor }).BarcodeDetector

/**
 * Scanning the QR code with this device's camera, offered only where the platform can decode it —
 * the paste field above is the entry that works everywhere. Nothing is captured anywhere: each frame
 * is read for a QR code and dropped.
 */
function ScanForLink({ onText }: { onText: (text: string) => void }) {
  const [scanning, setScanning] = useState(false)
  const [problem, setProblem] = useState<string | null>(null)
  const video = useRef<HTMLVideoElement>(null)
  // Held in a ref: the parent's handler is a new function each render, and the camera must not
  // restart (or stop) because of that.
  const onTextRef = useRef(onText)
  useEffect(() => { onTextRef.current = onText })
  const supported = !!barcodeDetector() && !!navigator.mediaDevices?.getUserMedia

  useEffect(() => {
    if (!scanning) return
    const Ctor = barcodeDetector()
    let stream: MediaStream | undefined
    let timer: ReturnType<typeof setInterval> | undefined
    let stopped = false
    const stop = () => {
      stopped = true
      clearInterval(timer)
      stream?.getTracks().forEach(t => t.stop())
    }
    const started = navigator.mediaDevices.getUserMedia({ video: { facingMode: 'environment' }, audio: false }).then(s => {
      if (stopped) { s.getTracks().forEach(t => t.stop()); return }
      stream = s
      if (video.current) { video.current.srcObject = s; void video.current.play().catch(() => {}) }
      if (!Ctor) return
      const detector = new Ctor({ formats: ['qr_code'] })
      timer = setInterval(() => {
        const el = video.current
        if (!el || !el.videoWidth) return
        detector.detect(el).then(codes => {
          const found = codes[0]?.rawValue
          if (!found || stopped) return
          stop()
          setScanning(false)
          onTextRef.current(found)
        }).catch(() => { /* a frame that can't be read: try the next one */ })
      }, 250)
    }, () => {
      if (stopped) return
      setScanning(false)
      setProblem('This browser wouldn\'t open the camera. Allow camera access for AMP, or paste the link above.')
    })
    return () => { stopped = true; clearInterval(timer); void started.finally(() => stream?.getTracks().forEach(t => t.stop())) }
  }, [scanning])

  if (!supported) return null
  return (
    <>
      {!scanning && <button type="button" className="amp-btn amp-btn-sm" onClick={() => { setProblem(null); setScanning(true) }}><ScanLine size={13} aria-hidden="true" />Scan the QR code</button>}
      {scanning && <button type="button" className="amp-btn amp-btn-sm" onClick={() => setScanning(false)}>Stop the camera</button>}
      <video ref={video} className={scanning ? 'w-full max-w-[320px] border border-line bg-surface' : 'hidden'} muted playsInline aria-label="Camera, looking for the QR code" />
      {problem && <p role="alert" className="m-0 text-[12.5px] text-danger-ink leading-snug">{problem}</p>}
    </>
  )
}
