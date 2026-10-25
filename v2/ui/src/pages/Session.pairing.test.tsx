/**
 * Pairing end to end on the session page, with the real WebCrypto implementation
 * on the browser side and an independent fake laptop (keys.json harness-1, node:crypto
 * checks of protocol 7.5) on the other.
 */
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Session } from './Session'
import { AuthContext } from '../contexts/auth-context'
import { RemoteProvider } from '../remote/RemoteProvider'
import { memoryStore } from '../remote/deviceStore'
import { webRemoteCrypto } from '../remote/webCrypto'
import { forgetPairingLink, pairingLinkFor, setPairingLinkForTest } from '../remote/pairingLink'
import { b64uDecode, deriveMacKey, deriveSasKey, envelopeSigningInput, fingerprint, pairingId, pairRequestSigningInput, sasCode } from '../remote/protocol'
import type { EventStreamHandlers, RelayTransport } from '../remote/transport'
import type { CommandEnvelope, SessionDetail } from '../remote/types'
import type { PairMessage } from '../remote/protocol'
import { nodeHmacB64u, nodeVerifyES256, SignedHarness, testKey } from '../test/signedHarness'
import { loadVectors } from '../test/vectors'
import type { Me } from '../types'

vi.setConfig({ testTimeout: 10_000 })

const vec = loadVectors<{ inputs: { pairing_secret_b64u: string; session_id: string } }>('pairing.json').inputs
const SESSION = vec.session_id
const harnessKey = testKey('harness-1')
const me: Me = { subject: 's', email: 'jsims@example.com', name: 'Jacob', roles: [], user_id: 1001,
  capabilities: { remote_control: { available: true, org_ids: [3], reason: null } } }

type Verdict = 'accept' | 'reject' | { failed: string; tries_left: number }

/** A laptop: signs events with harness-1, checks pair requests and commands on its own. */
function fakeLaptop(opts: { verdict?: Verdict; detail?: Partial<SessionDetail> } = {}) {
  const h = new SignedHarness(SESSION)
  const listeners = new Set<EventStreamHandlers>()
  const buffer: string[] = []
  const calls: unknown[] = []
  const pairs: { msg: PairMessage; code: string; macOk: boolean; sigOk: boolean }[] = []
  const commands: { env: CommandEnvelope; sigOk: boolean }[] = []
  let devicePub: string | null = null
  const publish = async (op: string, body: unknown, device: string | null = null) => {
    const wire = await h.event(op, body, device)
    buffer.push(wire)
    for (const l of listeners) l.onEnvelope(wire)
  }
  const detail: SessionDetail = {
    session_id: SESSION, harness_id: harnessKey.fingerprint, harness_pub: harnessKey.public_raw_b64u, harness_fp_display: harnessKey.fingerprint_display,
    host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' }, online: true, viewers: 0,
    started_at_ms: 0, expires_at_ms: 0, last_activity_at_ms: 0, protocol_version: 1, ...opts.detail,
  }
  const transport: RelayTransport = {
    listSessions: async () => [detail],
    getSession: async id => { calls.push(['getSession', id]); return detail },
    openEvents: (id, from, l) => {
      calls.push(['openEvents', id, from])
      queueMicrotask(() => {
        l.onOpen()
        l.onRelay({ type: 'attached', online: true, oldest_seq: buffer.length ? 1 : null, latest_seq: buffer.length || null })
        buffer.forEach((w, i) => { if (from == null || i + 1 >= from) l.onEnvelope(w) })
        listeners.add(l)
      })
      return () => { listeners.delete(l) }
    },
    postCommand: async (id, env) => {
      calls.push(['postCommand', id, env])
      const sigOk = devicePub != null && nodeVerifyES256(devicePub, envelopeSigningInput(env), env.sig)
      commands.push({ env, sigOk })
      if (sigOk) await publish('ack', { ref: { device_id: env.device_id, seq: env.seq }, op: env.op, result: 'started', prompt_id: 'p_9', queue_position: null }, env.device_id)
    },
    disconnect: async () => {},
    pair: async (id, msg) => {
      calls.push(['pair', id, msg])
      // Protocol 7.5, from the laptop's side, with the secret it minted.
      const secret = b64uDecode(vec.pairing_secret_b64u)
      const r = msg.request
      const input = pairRequestSigningInput(r)
      const macOk = r.pairing_id === await pairingId(secret) && nodeHmacB64u(await deriveMacKey(secret, SESSION), input) === msg.mac
      const sigOk = await fingerprint(b64uDecode(r.device_pub)) === r.device_id && nodeVerifyES256(r.device_pub, input, msg.sig)
      const code = await sasCode(await deriveSasKey(secret, SESSION), b64uDecode(harnessKey.public_raw_b64u), b64uDecode(r.device_pub))
      pairs.push({ msg, code, macOk, sigOk })
      const v = opts.verdict ?? 'accept'
      setTimeout(() => {
        if (!macOk || !sigOk) void publish('pair_result', { pairing_id: r.pairing_id, result: 'failed', code: macOk ? 'bad_signature' : 'bad_mac', tries_left: 4, pinned_until_ms: null }, r.device_id)
        else if (v === 'accept') { devicePub = r.device_pub; void publish('pair_result', { pairing_id: r.pairing_id, result: 'accepted', code: null, tries_left: null, pinned_until_ms: Date.now() + 30 * 86_400_000 }, r.device_id) }
        else if (v === 'reject') void publish('pair_result', { pairing_id: r.pairing_id, result: 'rejected', code: 'rejected', tries_left: null, pinned_until_ms: null }, r.device_id)
        else void publish('pair_result', { pairing_id: r.pairing_id, result: 'failed', code: v.failed, tries_left: v.tries_left, pinned_until_ms: null }, r.device_id)
      }, 50)
      return { expires_at_ms: Date.now() + 300_000 }
    },
  }
  const inject = (wire: string) => { for (const l of listeners) l.onEnvelope(wire) }
  return { transport, publish, inject, calls, pairs, commands, h }
}

function freshLink(h = harnessKey.fingerprint) {
  setPairingLinkForTest({ session_id: SESSION, secret: b64uDecode(vec.pairing_secret_b64u), h })
}

function renderPage(transport: RelayTransport, crypto = webRemoteCrypto(memoryStore())) {
  render(
    <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: false }}>
      <RemoteProvider deps={{ transport, crypto, mock: false }}>
        <MemoryRouter initialEntries={[`/sessions/${SESSION}`]}>
          <Routes><Route path="/sessions/:sessionId" element={<Session />} /></Routes>
        </MemoryRouter>
      </RemoteProvider>
    </AuthContext.Provider>,
  )
  return crypto
}

afterEach(() => forgetPairingLink())

describe('pairing from the link (protocol 7)', () => {
  it('shows the laptop\'s code, pins the harness on accept, then signs commands the laptop verifies', async () => {
    const user = userEvent.setup()
    const laptop = fakeLaptop()
    freshLink()
    const crypto = renderPage(laptop.transport)

    const code = await screen.findByTestId('pairing-code')
    expect(screen.getByText('Check the laptop shows the same code and press y there.')).toBeInTheDocument()
    await waitFor(() => expect(laptop.pairs).toHaveLength(1))
    const pair = laptop.pairs[0]
    expect(pair.macOk).toBe(true)
    expect(pair.sigOk).toBe(true)
    expect(code.textContent).toBe(pair.code) // the same code on both ends
    expect(pair.msg.request.device_name).toMatch(/ on /)
    // The stream was open before the request was posted (7.4).
    const order = laptop.calls.map(c => (c as unknown[])[0])
    expect(order.indexOf('openEvents')).toBeLessThan(order.indexOf('pair'))

    // Accepted: the harness is pinned and the session opens.
    await screen.findByText('Laptop online')
    expect(await crypto.harnessPin(harnessKey.fingerprint)).toMatchObject({ harness_id: harnessKey.fingerprint, harness_pub: harnessKey.public_raw_b64u })
    expect(pairingLinkFor(SESSION)).toBeNull() // the secret is forgotten

    await user.type(screen.getByLabelText('Message'), 'run the tests{Enter}')
    await waitFor(() => expect(laptop.commands).toHaveLength(1))
    expect(laptop.commands[0].sigOk).toBe(true)
    expect(laptop.commands[0].env.device_id).toBe(pair.msg.request.device_id)

    // The secret never left the page: not in any request, not in the URL.
    const everything = JSON.stringify(laptop.calls)
    expect(everything).not.toContain(vec.pairing_secret_b64u)
    expect(window.location.href).not.toContain(vec.pairing_secret_b64u)
  })

  it('says so when the laptop declines', async () => {
    const laptop = fakeLaptop({ verdict: 'reject' })
    freshLink()
    const crypto = renderPage(laptop.transport)
    expect(await screen.findByText(/declined on the laptop/)).toBeInTheDocument()
    expect(await crypto.harnessPin(harnessKey.fingerprint)).toBeNull()
    expect(pairingLinkFor(SESSION)).toBeNull()
  })

  it('offers another try after a failed try with tries left', async () => {
    const user = userEvent.setup()
    const laptop = fakeLaptop({ verdict: { failed: 'stale_ts', tries_left: 3 } })
    freshLink()
    renderPage(laptop.transport)
    expect(await screen.findByText(/clock is more than a minute off.*3 tries left/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(laptop.pairs).toHaveLength(2))
    expect(laptop.pairs[1].msg.request.nonce).not.toBe(laptop.pairs[0].msg.request.nonce)
  })

  it('stops when the link names another laptop, sending nothing', async () => {
    const laptop = fakeLaptop()
    freshLink(testKey('harness-2').fingerprint)
    renderPage(laptop.transport)
    expect(await screen.findByText('This link doesn\'t match that laptop')).toBeInTheDocument()
    expect(laptop.pairs).toHaveLength(0)
  })

  it('never trusts a relay-supplied key the link doesn\'t vouch for (T-R-1)', async () => {
    // AMP claims the right harness id but hands over another key.
    const laptop = fakeLaptop({ detail: { harness_pub: testKey('harness-2').public_raw_b64u } })
    freshLink()
    const crypto = renderPage(laptop.transport)
    expect(await screen.findByText('The laptop\'s key doesn\'t match the link')).toBeInTheDocument()
    expect(laptop.pairs).toHaveLength(0)
    expect(await crypto.harnessPin(harnessKey.fingerprint)).toBeNull()
  })

  it('ignores a forged pair_result and keeps waiting', async () => {
    const laptop = fakeLaptop({ verdict: 'reject' })
    freshLink()
    const crypto = renderPage(laptop.transport)
    await waitFor(() => expect(laptop.pairs).toHaveLength(1))
    const req = laptop.pairs[0].msg.request
    // The relay forges an "accepted" result for this device, claiming the pinned harness id but signed by another key.
    const forger = new SignedHarness(SESSION, 'harness-2')
    laptop.inject(await forger.event('pair_result', { pairing_id: req.pairing_id, result: 'accepted', code: null, tries_left: null,
      pinned_until_ms: Date.now() + 1e9 }, req.device_id, { harness_id: harnessKey.fingerprint, seq: 1 }))
    expect(await screen.findByText(/Untrusted data from the relay was dropped/)).toBeInTheDocument()
    // The real laptop's answer still decides.
    expect(await screen.findByText(/declined on the laptop/)).toBeInTheDocument()
    expect(await crypto.harnessPin(harnessKey.fingerprint)).toBeNull()
  })

  it('opens directly when this browser is already paired, without using the link', async () => {
    const laptop = fakeLaptop()
    const store = memoryStore()
    const crypto = webRemoteCrypto(store)
    await crypto.ensureDevice()
    await crypto.pinHarness({ harness_id: harnessKey.fingerprint, harness_pub: harnessKey.public_raw_b64u, host: 'jacobs-mbp', pinned_until_ms: Date.now() + 86_400_000 })
    freshLink()
    renderPage(laptop.transport, crypto)
    await screen.findByText('Laptop online')
    expect(laptop.pairs).toHaveLength(0)
    expect(pairingLinkFor(SESSION)).toBeNull()
  })
})
