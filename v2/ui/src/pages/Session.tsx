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
import { ArrowLeft, MonitorSmartphone, MoreHorizontal, ScanLine, Unplug } from 'lucide-react'
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
  AmpConnectionBadge, Banner, Composer, HeaderFacts, OutgoingRow, PresenceBadge, QueueStrip, StatusLine, TicketChip, TicketLine, ToolGroup,
  TranscriptRow, WorkerChips, WorkingLine,
} from '../components/remote/SessionParts'
import { groupRows, useNow } from '../remote/transcriptView'

const SESSION_ID = /^rs_[A-Za-z0-9_-]{22}$/

/**
 * Whether a soft keyboard is covering the screen, read the only way a browser reports it:
 * `visualViewport` shrinks while `window.innerHeight` (the layout viewport) does not. A `window`
 * resize listener would never fire — the page size doesn't change when a phone keyboard opens.
 *
 * The layout viewport's width rules it out above 1024px: a desktop has no soft keyboard, so a
 * shrunken visual viewport there can only be a pinch-zoom, which should not cost the status line.
 */
function useKeyboardOpen() {
  const [open, setOpen] = useState(false)
  useEffect(() => {
    const vv = window.visualViewport
    if (!vv) return
    const read = () => {
      const layout = window.innerHeight
      // A soft keyboard takes roughly a third of the screen or more; a URL bar or a pinch-zoom by a
      // few percent never crosses this. With no `visualViewport` at all (older browsers), never.
      setOpen(window.innerWidth <= 1024 && layout > 0 && vv.height < layout * 0.7)
    }
    read()
    vv.addEventListener('resize', read)
    return () => vv.removeEventListener('resize', read)
  }, [])
  return open
}

/** The header's ⋯ menu: on a phone it is where Disconnect lives, so the header stays one row
 *  (audit #3026 F4: three header rows plus the ticket strip were 157px of chrome). Two steps, as
 *  the header's own Disconnect has always been — a deliberate tap, then the confirm. */
function SessionMenu({ onDisconnect }: { onDisconnect: () => void }) {
  const [open, setOpen] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const wrap = useRef<HTMLDivElement>(null)
  // Close on a tap outside the menu, or Escape. Both listeners exist only while it is open.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (!wrap.current?.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.preventDefault(); setOpen(false) } }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey) }
  }, [open])
  const close = () => { setOpen(false); setConfirming(false) }
  return (
    <div ref={wrap} className="relative shrink-0 sm:hidden">
      <button type="button" onClick={() => setOpen(o => !o)} aria-haspopup="menu" aria-expanded={open}
        aria-label="Session menu" title="More" data-testid="session-menu"
        className="inline-flex items-center justify-center w-8 h-[30px] border border-line-str bg-surface text-ink-2 hover:bg-surface-3 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent">
        <MoreHorizontal size={15} aria-hidden="true" />
      </button>
      {open && (
        <div role="menu" aria-label="Session actions" data-testid="session-menu-panel"
          className="absolute right-0 top-[calc(100%+4px)] z-30 min-w-[190px] p-1 bg-surface border border-line"
          style={{ borderRadius: 3, boxShadow: '0 8px 24px color-mix(in srgb, var(--ink) 16%, transparent)' }}>
          {!confirming ? (
            <button role="menuitem" type="button" onClick={() => setConfirming(true)}
              className="flex items-center gap-2 w-full text-left px-2.5 py-2 text-[12.5px] text-danger-ink hover:bg-surface-3">
              <Unplug size={13} aria-hidden="true" />Disconnect
            </button>
          ) : (
            <div className="flex gap-1.5 p-1">
              <button type="button" className="amp-btn amp-btn-sm amp-btn-accent flex-1"
                onClick={() => { close(); onDisconnect() }}>Disconnect now</button>
              <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={() => setConfirming(false)}>Cancel</button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * The page frame. From `sm` up it is the header it has always been: back, host and facts, then the
 * right-hand cluster, wrapping when it must. On a phone it is one row — back, host, the right
 * cluster held on the same line — plus, when there is one, the ticket as a chip on the second row.
 * `sm:contents` hands the row's children back to the header's own flex line, so the desktop header
 * keeps its exact markup and wrapping.
 */
function Frame({ title, children, right, sub, ticket }: {
  title: React.ReactNode; children: React.ReactNode; right?: React.ReactNode; sub?: React.ReactNode; ticket?: React.ReactNode
}) {
  return (
    <div className="flex flex-col h-dvh bg-bg">
      <header className="bg-surface border-b border-line px-3 sm:px-5 py-1.5 sm:py-2 flex items-center gap-x-3 gap-y-1 flex-wrap">
        <div className="flex items-center gap-x-2 gap-y-1 min-w-0 w-full sm:contents">
          {/* Full page loads: this page has its own strict CSP, the rest of the app doesn't. */}
          <a href="/sessions" className="amp-btn amp-btn-ghost amp-btn-sm shrink-0" aria-label="All sessions"><ArrowLeft size={14} /><span className="hidden sm:inline">Sessions</span></a>
          <div className="min-w-0 flex-1 flex items-baseline gap-x-2.5 gap-y-0 flex-nowrap sm:flex-none sm:flex-wrap">
            {/* The host never shrinks; the facts after it give up their room instead (the mockup's
                priority order): a phone shows the whole laptop name and lets the detail truncate.
                `min-w-0` on the host wrapper lets the facts below it shrink to nothing. */}
            <h1 className="amp-heading m-0 text-[15px] sm:text-[17px] text-ink shrink-0 truncate inline-flex items-center gap-1.5"><MonitorSmartphone size={15} className="shrink-0 self-center" aria-hidden="true" />{title}</h1>
            {sub}
          </div>
          {right && <div className="ml-auto flex items-center gap-1.5 sm:gap-2 flex-nowrap shrink-0 sm:flex-wrap sm:shrink">{right}</div>}
        </div>
        {ticket && <div className="flex items-center gap-2 min-w-0 w-full pl-[42px] sm:hidden">{ticket}</div>}
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
  // A phone keyboard covers the bottom of the screen; what it covers should be the rows a person is
  // not using while typing (audit #3026 F2/F6, the approved mockup's keyboard state). Called up here
  // with the other hooks: the render has early returns below, and a hook may never come after one.
  const keyboard = useKeyboardOpen()
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
  const hasTicket = status?.ticket_id != null && !!(status.ticket_name || status.ticket_state)

  return (
    <Frame title={host} sub={<HeaderFacts status={status} os={state.hello?.harness.os ?? detail.os} />}
      ticket={hasTicket && !keyboard ? <TicketChip status={status} /> : undefined}
      right={<>
        <AmpConnectionBadge status={status} />
        <PresenceBadge online={ended ? false : state.online} connection={state.connection} />
        {/* Two ways to disconnect, one per layout: the button belongs to the desktop header (from
            `sm` up), the ⋯ menu to the phone, where the header has to stay one row. */}
        {!ended && <div className="hidden sm:block"><ConfirmButton label="Disconnect" confirmLabel="Disconnect now" icon={<Unplug size={13} />}
          onConfirm={() => { disconnect().catch(() => {}) }} /></div>}
        {!ended && <SessionMenu onDisconnect={() => { disconnect().catch(() => {}) }} />}
      </>}>
      <TicketLine status={status} />
      {ended && <Banner tone="info">{endReasonText(ended.reason, state.hello?.idle_timeout_s)} The laptop session keeps running locally.</Banner>}
      {state.versionMismatch && <Banner tone="danger">This laptop runs a newer harness; update AMP. Nothing more from it is shown.</Banner>}
      {!ended && state.online === false && <Banner tone="warn">Laptop offline. It may be asleep or off the network; this page catches up when it's back.</Banner>}
      {!ended && state.connection === 'reconnecting' && state.online !== false && <Banner tone="info">Reconnecting to AMP… Nothing is lost: the page picks up where it left off.</Banner>}

      {/* The one region that grows and shrinks: every dock row below is `shrink-0`, so the chat takes
          whatever the dock leaves (audit #3026 F2: the transcript was the row that lost every fight). */}
      <div ref={scroller} className="flex-1 min-h-0 overflow-y-auto" aria-label="Transcript" aria-live="polite" data-testid="transcript">
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

      {/* With a phone keyboard up, only the header's first row, the transcript and the composer keep
          the screen (audit #3026 F2/F6, approved mockup item 5): the ticket chip, the roster, the
          working line, the queue strip and the status line are all still there when the keyboard
          goes away, and until then every pixel belongs to the chat. The room is already known from
          the composer's reason line ("Agent busy — your message joins the queue (2 ahead)") and the
          chat itself. */}
      {!keyboard && <WorkerChips workers={state.workers} now={now} />}
      {!keyboard && <WorkingLine busy={busy && !ended} runStartedAt={state.runStartedAt} retry={retry} compaction={ended ? null : state.compaction} now={now} />}
      {!keyboard && <QueueStrip queue={state.queue} myDeviceId={phase.deviceId} online={online} onClear={stop} />}
      {/* The bottom dock row carries the safe-area inset, and with the keyboard up that is the
          composer: the status line is hidden then, and padding on a hidden row is no padding at all
          (audit F6 — the inset used to live only on the status line). */}
      <Composer disabled={composerDisabled} disabledReason={disabledReason} busy={busy} queued={state.queue.length}
        online={online && canSign} onSend={sendPrompt} onStop={stop} safeArea={keyboard} />
      {!keyboard && (
        <div className="pb-[env(safe-area-inset-bottom)] bg-surface-2">
          <StatusLine status={status} totals={state.totals} startedAt={state.hello?.started_at_ms ?? detail.started_at_ms} workers={state.workers} now={now} />
        </div>
      )}
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
