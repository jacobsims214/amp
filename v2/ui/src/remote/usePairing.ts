/**
 * Pairs this browser with the laptop from a pairing link (protocol 7, sequence 14.2):
 * check the link against the session, create or reuse the device key, open the
 * event stream, POST the signed and MACed pair request, show the matching code,
 * and wait for a verified `pair_result` for this device and this pairing_id.
 * On `accepted` the harness key is pinned (only because its fingerprint equals
 * the link's `h`) and the secret is forgotten.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { useRemote } from './remote-context'
import { checkEvent, isSecurityVerdict, newNonce } from './envelope'
import { forgetPairingLink, type PairingLink } from './pairingLink'
import { pairPostText, pairResultText, preparePairing, type PreparedPairing } from './pairing'
import { keyMatchesId } from './protocol'
import { deviceNameFromUA } from './deviceName'
import { RelayError } from './transport'
import { MAX_PIN_MS } from './webCrypto'
import type { HarnessPin, PairResultBody, SessionDetail } from './types'

/** A pairing secret lives 5 minutes on the laptop (protocol 7.5). */
export const PAIRING_TTL_MS = 5 * 60_000

export type PairingState =
  | { kind: 'preparing' }
  | { kind: 'waiting'; code: string; device_name: string; posted: boolean }
  | { kind: 'paired' }
  | { kind: 'failed'; title: string; message: string; canRetry: boolean }

export function usePairing(detail: SessionDetail, link: PairingLink, onPaired: () => void) {
  const { transport, crypto } = useRemote()
  const [state, setState] = useState<PairingState>({ kind: 'preparing' })
  const [untrusted, setUntrusted] = useState(0)
  const [attempt, setAttempt] = useState(0)
  // One prepared request and one POST per attempt, even when React runs the effect twice.
  const prepared = useRef(new Map<number, Promise<PreparedPairing>>())
  const posted = useRef(new Map<number, Promise<{ expires_at_ms: number | null }>>())
  const onPairedRef = useRef(onPaired)
  useEffect(() => { onPairedRef.current = onPaired })

  useEffect(() => {
    let stopped = false
    let close: (() => void) | null = null
    let expiry: ReturnType<typeof setTimeout> | undefined
    let retry: ReturnType<typeof setTimeout> | undefined
    let chain: Promise<void> = Promise.resolve()
    let lastApplied = 0
    const halt = () => { stopped = true; clearTimeout(expiry); clearTimeout(retry); close?.(); close = null }
    const fail = (title: string, message: string, canRetry = false) => {
      if (stopped) return
      halt()
      setState({ kind: 'failed', title, message, canRetry })
    }

    ;(async () => {
      // Never trust on first use: the link's h must name this session's laptop, and the key AMP
      // returned must hash to it (protocol 7.3, 7.7, T-R-1).
      if (link.h !== detail.harness_id) {
        return fail('This link doesn\'t match that laptop', 'The pairing link names a different laptop from the one running this session. Nothing was sent. Open the link the laptop printed for this session.')
      }
      if (!(await keyMatchesId(detail.harness_pub, link.h))) {
        return fail('The laptop\'s key doesn\'t match the link', 'AMP returned a key for this laptop that the link doesn\'t vouch for, so nothing was sent and nothing was trusted.')
      }
      const device_name = deviceNameFromUA(navigator.userAgent)
      let p = prepared.current.get(attempt)
      if (!p) {
        p = crypto.ensureDevice().then(device => preparePairing({
          secret: link.secret, session_id: detail.session_id, harness_id: link.h, harness_pub: detail.harness_pub,
          device, device_name, ts: Date.now(), nonce: newNonce(), sign: r => crypto.signPairRequest(r),
        }))
        prepared.current.set(attempt, p)
      }
      let pairing: PreparedPairing
      try { pairing = await p } catch (e) {
        return fail('Couldn\'t create a device key', e instanceof Error ? e.message : 'This browser couldn\'t create or use its device key.')
      }
      if (stopped) return
      setState({ kind: 'waiting', code: pairing.code, device_name, posted: false })

      const myDevice = pairing.message.request.device_id
      const provisional: HarnessPin = { harness_id: link.h, harness_pub: detail.harness_pub, pinned_until_ms: Number.MAX_SAFE_INTEGER }

      const onResult = async (b: PairResultBody) => {
        if (b.result !== 'accepted') {
          // A failed try with tries left keeps the secret for another go; anything else used it up.
          const canRetry = b.result === 'failed' && (b.tries_left ?? 0) > 0
          if (!canRetry) forgetPairingLink()
          return fail('Not paired', pairResultText(b), canRetry)
        }
        halt()
        try {
          await crypto.pinHarness({
            harness_id: link.h, harness_pub: detail.harness_pub, host: detail.host,
            pinned_until_ms: b.pinned_until_ms ?? Date.now() + MAX_PIN_MS,
          })
        } catch (e) {
          setState({ kind: 'failed', title: 'Couldn\'t save the pairing', message: e instanceof Error ? e.message : 'Saving the laptop key failed.', canRetry: false })
          return
        }
        forgetPairingLink()
        setState({ kind: 'paired' })
        onPairedRef.current()
      }

      const post = () => {
        let pp = posted.current.get(attempt)
        if (!pp) {
          pp = transport.pair(detail.session_id, pairing.message)
          posted.current.set(attempt, pp)
        }
        pp.then(r => {
          if (stopped) return
          setState(s => s.kind === 'waiting' ? { ...s, posted: true } : s)
          const until = r.expires_at_ms ?? Date.now() + PAIRING_TTL_MS
          clearTimeout(expiry)
          expiry = setTimeout(() => {
            forgetPairingLink()
            fail('Not paired', pairResultText({ result: 'expired', code: 'pairing_expired', tries_left: null }))
          }, Math.max(0, until - Date.now()) + 5_000)
        }, e => {
          const retryable = e instanceof RelayError && (e.code === 'harness_offline' || e.code === 'audit_unavailable' || e.status >= 500)
          if (!retryable) forgetPairingLink()
          fail('Not paired', e instanceof RelayError ? pairPostText(e.code, e.message) : 'Couldn\'t reach AMP. Check your connection.', retryable)
        })
      }

      // The browser opens the stream before it posts, so the result can't be missed (protocol 7.4).
      const connect = () => {
        close = transport.openEvents(detail.session_id, lastApplied > 0 ? lastApplied + 1 : null, {
          onOpen: post,
          onActivity: () => {},
          onEnvelope: wire => { chain = chain.then(async () => {
            const r = await checkEvent(wire, { session_id: detail.session_id, pinned_harness: provisional, last_applied_seq: lastApplied }, crypto.verifyEvent)
            if (stopped) return
            if ((r.verdict === 'ok' || r.verdict === 'ignored_unknown_op') && r.env) {
              lastApplied = r.env.seq
              const b = r.env.body as PairResultBody
              if (r.verdict === 'ok' && r.env.op === 'pair_result' && r.env.device_id === myDevice && b.pairing_id === pairing.pairing_id) await onResult(b)
            } else if (r.verdict === 'unsupported_version') {
              fail('Update AMP', 'This laptop runs a newer harness; update AMP.')
            } else if (isSecurityVerdict(r.verdict)) {
              setUntrusted(n => n + 1)
            }
          }).catch(() => {}) },
          onSnapshot: () => {},
          onRelay: frame => { if (frame.type === 'ended') fail('This session has ended', 'Run /remote on the laptop to start a new session, then open its link.') },
          onClose: err => {
            if (stopped) return
            if (err && (err.status === 404 || err.status === 410)) return fail('This session has ended', 'Run /remote on the laptop to start a new session, then open its link.')
            if (err && err.status === 403) return fail('Not paired', pairPostText(err.code, err.message))
            retry = setTimeout(connect, 2_000)
          },
        })
      }
      connect()
    })()
    return halt
  }, [attempt, crypto, transport, detail, link])

  const tryAgain = useCallback(() => {
    setState({ kind: 'preparing' })
    setAttempt(a => a + 1)
  }, [])

  return { state, untrusted, tryAgain }
}
