/**
 * Drives one remote session: loads it, checks the pin, keeps the event stream
 * open (reconnecting with from_seq, reopening after 60 s of silence), runs every
 * frame through the event check in order, and sends signed commands.
 */
import { useCallback, useEffect, useLayoutEffect, useReducer, useRef, useState } from 'react'
import { useRemote } from './remote-context'
import { buildCommand, checkEvent, isSecurityVerdict, newClientMsgId, nextSeq, type EventCheck } from './envelope'
import { NotPairedError } from './crypto'
import { RelayError } from './transport'
import { forgetPairingLink, pairingLinkFor, type PairingLink } from './pairingLink'
import { initialSessionState, refusalText, sessionReducer, type SessionState } from './session'
import type { CommandBody, CommandOp, HarnessPin, RefusedBody, SessionDetail } from './types'

/** Reopen the stream after this long without a byte (keepalives come every 20 s). */
export const SILENCE_MS = 60_000
/** Show "no answer from the laptop" after this long without an ack or refusal (5.2). */
export const ACK_TIMEOUT_MS = 10_000

export type Phase =
  | { kind: 'loading' }
  | { kind: 'not_found' }
  | { kind: 'gone'; reason: string }
  | { kind: 'error'; message: string }
  | { kind: 'unpaired'; detail: SessionDetail }
  /** Not paired yet, but the page was opened from a pairing link (protocol 7.1). */
  | { kind: 'pairing'; detail: SessionDetail; link: PairingLink }
  | { kind: 'ready'; detail: SessionDetail; pin: HarnessPin; deviceId: string | null }

interface Sent { op: CommandOp; body: CommandBody; retried: boolean }

export function useRemoteSession(sessionId: string) {
  const { transport, crypto } = useRemote()
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' })
  const [state, dispatch] = useReducer(sessionReducer, initialSessionState)
  const stateRef = useRef<SessionState>(state)
  useLayoutEffect(() => { stateRef.current = state })
  const lastApplied = useRef(0)
  const lastSent = useRef(0)
  const sent = useRef(new Map<number, Sent>())
  const ackTimers = useRef(new Map<number, ReturnType<typeof setTimeout>>())
  /** Bumped after a pairing succeeds, to load the new pin. */
  const [loads, setLoads] = useState(0)

  // ── Load the session and the pin ──────────────────────────────────────────
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const detail = await transport.getSession(sessionId)
        const [pin, deviceId] = await Promise.all([crypto.harnessPin(detail.harness_id), crypto.deviceId()])
        if (cancelled) return
        // Only a pin for exactly this harness counts; a relay-supplied key is never trusted (7.7, T-R-1).
        if (!pin || pin.harness_id !== detail.harness_id || pin.pinned_until_ms <= Date.now()) {
          const link = pairingLinkFor(sessionId)
          setPhase(link ? { kind: 'pairing', detail, link } : { kind: 'unpaired', detail })
        } else {
          // Already paired with this laptop: a pairing link isn't needed (pins last up to 30 days, D1).
          forgetPairingLink()
          setPhase({ kind: 'ready', detail, pin, deviceId })
        }
      } catch (e) {
        if (cancelled) return
        if (e instanceof RelayError && e.status === 404) setPhase({ kind: 'not_found' })
        else if (e instanceof RelayError && e.status === 410) setPhase({ kind: 'gone', reason: 'session_ended' })
        else setPhase({ kind: 'error', message: e instanceof Error ? e.message : 'Failed to load the session' })
      }
    })()
    return () => { cancelled = true }
  }, [sessionId, transport, crypto, loads])

  const paired = useCallback(() => setLoads(n => n + 1), [])

  const deviceId = phase.kind === 'ready' ? phase.deviceId : null
  const pin = phase.kind === 'ready' ? phase.pin : null
  const harnessId = phase.kind === 'ready' ? phase.detail.harness_id : null

  // ── Sending ───────────────────────────────────────────────────────────────
  const post = useCallback(async (seq: number, op: CommandOp, body: CommandBody) => {
    if (!deviceId || !harnessId) throw new NotPairedError()
    const env = await crypto.signCommand(buildCommand({ session_id: sessionId, harness_id: harnessId, device_id: deviceId, seq, ts: Date.now(), op, body }))
    await transport.postCommand(sessionId, env)
    dispatch({ type: 'outgoing_update', seq, patch: { state: 'forwarded' } })
    clearTimeout(ackTimers.current.get(seq))
    ackTimers.current.set(seq, setTimeout(() => dispatch({ type: 'outgoing_timeout', seq }), ACK_TIMEOUT_MS))
  }, [crypto, transport, sessionId, deviceId, harnessId])

  const sendCommand = useCallback(async (op: CommandOp, body: CommandBody, text: string | null = null) => {
    const seq = nextSeq(lastSent.current, Date.now())
    lastSent.current = seq
    const client_msg_id = op === 'prompt' ? (body as { client_msg_id: string }).client_msg_id : null
    const base = { seq, op, client_msg_id, text, prompt_id: null }
    if (stateRef.current.online === false) {
      // Never queued for later (R-M8): refuse straight away.
      dispatch({ type: 'outgoing', out: { ...base, state: 'refused', problem: refusalText('harness_offline') } })
      return
    }
    dispatch({ type: 'outgoing', out: { ...base, state: 'sending', problem: null } })
    sent.current.set(seq, { op, body, retried: false })
    try {
      await post(seq, op, body)
    } catch (e) {
      const problem = e instanceof NotPairedError ? 'This browser isn\'t paired with the laptop yet.'
        : e instanceof RelayError ? refusalText(e.code, e.message)
        : 'Couldn\'t reach AMP. Check your connection.'
      dispatch({ type: 'outgoing_update', seq, patch: { state: 'failed', problem } })
    }
  }, [post])

  const sendPrompt = useCallback((text: string) => sendCommand('prompt', { text, client_msg_id: newClientMsgId() }, text), [sendCommand])
  const stop = useCallback(() => sendCommand('stop', {}), [sendCommand])

  /** Handles a refused `seq_not_increasing` by re-signing once with last_seq + 1 (5.3). */
  const maybeResend = useCallback((b: RefusedBody) => {
    if (b.code !== 'seq_not_increasing' || !b.ref || b.last_seq == null) return false
    const orig = sent.current.get(b.ref.seq)
    if (!orig || orig.retried) return false
    const seq = Math.max(b.last_seq + 1, lastSent.current + 1)
    lastSent.current = seq
    sent.current.set(seq, { ...orig, retried: true })
    dispatch({ type: 'outgoing_update', seq: b.ref.seq, patch: { seq, state: 'sending' } })
    post(seq, orig.op, orig.body).catch(() => dispatch({ type: 'outgoing_update', seq, patch: { state: 'failed', problem: 'Couldn\'t resend.' } }))
    return true
  }, [post])

  // ── The event stream ──────────────────────────────────────────────────────
  useEffect(() => {
    if (!pin) return
    let stopped = false
    let close: (() => void) | null = null
    let silence: ReturnType<typeof setTimeout> | undefined
    let retry: ReturnType<typeof setTimeout> | undefined
    let backoff = 1000
    let chain: Promise<void> = Promise.resolve()
    const enqueue = (fn: () => Promise<void> | void) => { chain = chain.then(fn).catch(() => {}) }
    const rx = () => ({ session_id: sessionId, pinned_harness: pin, last_applied_seq: lastApplied.current })

    const halt = () => { stopped = true; clearTimeout(silence); clearTimeout(retry); close?.(); close = null }

    const handle = (r: EventCheck) => {
      if (stopped) return
      if (r.verdict === 'ok' && r.env) {
        const env = r.env
        lastApplied.current = env.seq
        if ((env.op === 'ack' || env.op === 'refused') && env.device_id === deviceId) {
          const ref = (env.body as { ref?: { device_id: string; seq: number } | null }).ref
          if (ref && ref.device_id === deviceId) {
            clearTimeout(ackTimers.current.get(ref.seq))
            if (env.op === 'refused' && maybeResend(env.body as RefusedBody)) return dispatch({ type: 'skip', seq: env.seq })
          }
        }
        dispatch({ type: 'event', env, myDeviceId: deviceId })
        if (env.op === 'bye') halt()
      } else if (r.verdict === 'ignored_unknown_op' && r.env) {
        lastApplied.current = r.env.seq
        dispatch({ type: 'skip', seq: r.env.seq })
      } else if (r.verdict === 'unsupported_version') {
        dispatch({ type: 'version_mismatch' })
        halt()
      } else if (isSecurityVerdict(r.verdict)) {
        dispatch({ type: 'untrusted', verdict: r.verdict })
      }
      // 'duplicate' is dropped silently
    }

    const armSilence = () => {
      clearTimeout(silence)
      silence = setTimeout(() => { close?.(); close = null; reconnect() }, SILENCE_MS)
    }

    const connect = () => {
      const from = lastApplied.current > 0 ? lastApplied.current + 1 : null
      close = transport.openEvents(sessionId, from, {
        onOpen: () => { backoff = 1000; armSilence() },
        onActivity: armSilence,
        onEnvelope: wire => enqueue(async () => handle(await checkEvent(wire, rx(), crypto.verifyEvent))),
        onSnapshot: wire => enqueue(async () => {
          const r = await checkEvent(wire, rx(), crypto.verifyEvent, { snapshot: true })
          if (stopped) return
          if (r.verdict === 'ok' && r.env) dispatch({ type: 'snapshot', env: r.env })
          else if (r.verdict === 'unsupported_version') { dispatch({ type: 'version_mismatch' }); halt() }
          else if (isSecurityVerdict(r.verdict)) dispatch({ type: 'untrusted', verdict: r.verdict })
        }),
        onRelay: frame => enqueue(() => {
          if (stopped) return
          dispatch({ type: 'relay', frame })
          if (frame.type === 'ended') halt()
        }),
        onClose: err => {
          clearTimeout(silence)
          if (stopped) return
          if (err && (err.status === 404 || err.status === 410)) {
            enqueue(() => { dispatch({ type: 'ended', reason: err.status === 404 ? 'session_ended' : err.code }) })
            halt()
            return
          }
          reconnect()
        },
      })
    }

    const reconnect = () => {
      if (stopped) return
      dispatch({ type: 'connection', connection: 'reconnecting' })
      clearTimeout(retry)
      retry = setTimeout(connect, backoff * (0.8 + Math.random() * 0.4))
      backoff = Math.min(backoff * 2, 30_000)
    }

    dispatch({ type: 'connection', connection: 'connecting' })
    connect()
    return halt
  }, [pin, sessionId, transport, crypto, deviceId, maybeResend])

  useEffect(() => {
    const timers = ackTimers.current
    return () => { for (const t of timers.values()) clearTimeout(t) }
  }, [])

  /** Disconnect: a signed `detach` first when we can, then the relay endpoint (8.3). */
  const disconnect = useCallback(async () => {
    if (stateRef.current.online && deviceId) await sendCommand('detach', {}).catch(() => {})
    try {
      await transport.disconnect(sessionId)
      dispatch({ type: 'ended', reason: 'disconnected' })
    } catch (e) {
      if (e instanceof RelayError && (e.status === 404 || e.status === 410)) dispatch({ type: 'ended', reason: 'disconnected' })
      else throw e
    }
  }, [transport, sessionId, deviceId, sendCommand])

  return { phase, state, sendPrompt, stop, disconnect, paired, canSign: !!deviceId }
}
