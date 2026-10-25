import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { Session } from './Session'
import { AuthContext } from '../contexts/auth-context'
import { RemoteProvider } from '../remote/RemoteProvider'
import type { RemoteDeps } from '../remote/remote-context'
import type { RemoteCrypto } from '../remote/crypto'
import { RelayError, type EventStreamHandlers, type RelayTransport } from '../remote/transport'
import { MOCK_SESSION_ID, MockRelay, mockCrypto, mockTransport } from '../remote/mock/mockRelay'
import { loadVectors } from '../test/vectors'
import type { Me } from '../types'
import type { SessionDetail } from '../remote/types'

vi.setConfig({ testTimeout: 10_000 }) // the fake harness streams on real timers

const meWith = (available: boolean): Me => ({
  subject: 's', email: 'jsims@example.com', name: 'Jacob', roles: [], user_id: 1001,
  capabilities: { remote_control: { available, org_ids: available ? [3] : [], reason: available ? null : 'no_org_enabled' } },
})

function renderSession(deps: RemoteDeps, opts: { me?: Me; id?: string } = {}) {
  const me = opts.me ?? meWith(true)
  return render(
    <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: false }}>
      <RemoteProvider deps={deps}>
        <MemoryRouter initialEntries={[`/sessions/${opts.id ?? MOCK_SESSION_ID}`]}>
          <Routes><Route path="/sessions/:sessionId" element={<Session />} /></Routes>
        </MemoryRouter>
      </RemoteProvider>
    </AuthContext.Provider>,
  )
}

function mockSession(opts: { seed?: boolean; online?: boolean; paired?: boolean } = {}) {
  // Each fake run takes ~1.2 s (8 steps), long enough to queue a second prompt behind it.
  const relay = new MockRelay({ stepMs: 150, seed: opts.seed ?? true, online: opts.online })
  const deps: RemoteDeps = { transport: mockTransport(relay), crypto: mockCrypto({ paired: opts.paired }), mock: false }
  return { relay, deps }
}

describe('Session page', () => {
  it('renders every ChatEvent type, the status bar and presence', async () => {
    const { deps } = mockSession()
    const { container } = renderSession(deps)
    await screen.findByText('run the tests and tell me what fails')
    expect(screen.getByText('typed on the laptop')).toBeInTheDocument() // user_message (local)
    expect(screen.getByText('Thinking').closest('details')).not.toHaveAttribute('open') // thinking_delta, folded
    expect(screen.getByText(/Running the/)).toBeInTheDocument() // message_start + text_delta (Markdown)
    expect(screen.getAllByTestId('tool-row')).toHaveLength(1) // tool_start + tool_end
    expect(screen.getByText(/Full output \(178 KB\) is on the laptop/)).toBeInTheDocument()
    expect(screen.getByText(/Retrying \(1\/3\) in 2 s/)).toBeInTheDocument() // retry
    expect(screen.getAllByText(/12,034 in/)).toHaveLength(1) // message_end usage
    expect(screen.getByText('Run finished')).toBeInTheDocument() // run_end
    expect(screen.getByText(/context7.*reconnected/)).toBeInTheDocument() // notice
    expect(within(screen.getByLabelText('Workers')).getByText('RC-M1 harness link')).toBeInTheDocument() // worker_status
    // Hostile model output: no images, no script, link with its full URL.
    expect(container.querySelector('img, script, iframe')).toBeNull()
    expect(screen.getByRole('link', { name: /the CI run \(https:\/\/github\.com\/jacobsims214\/amp\/actions\)/ })).toBeInTheDocument()

    const status = screen.getByLabelText('Session status')
    for (const t of ['anthropic/claude-opus-5-5', '37%', '$1.83', 'AMP · #2372', 'jacobs-mbp']) expect(within(status).getByText(t)).toBeInTheDocument()
    expect(screen.getByText('Laptop online')).toBeInTheDocument()
  })

  it('sends a prompt, shows the reply, and queues a second one while the agent is busy', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    const box = screen.getByLabelText('Message')
    await user.type(box, 'please fix it{Enter}')
    await waitFor(() => expect(relay.received.map(c => c.op)).toEqual(['prompt']))
    const sent = relay.received[0]
    expect(sent.body).toMatchObject({ text: 'please fix it', client_msg_id: expect.stringMatching(/^[A-Za-z0-9_-]{1,64}$/) })
    expect(sent.sig).toBeTruthy()
    expect(sent.seq).toBeGreaterThan(1_700_000_000_000)
    await screen.findByText('from Chrome on macOS')
    // While the run is open, the next message is queued.
    await user.type(box, 'and then the docs{Enter}')
    const strip = await screen.findByLabelText('Queued messages')
    expect(within(strip).getByText('and then the docs')).toBeInTheDocument()
    expect(within(strip).getByText('you')).toBeInTheDocument()
    expect(relay.received[1].seq).toBeGreaterThan(relay.received[0].seq)
    // It runs when the first one finishes.
    await waitFor(() => expect(screen.queryByLabelText('Queued messages')).toBeNull(), { timeout: 3000 })
    await waitFor(() => expect(screen.getAllByText(/You said: "and then the docs"/)).toHaveLength(1), { timeout: 3000 })
  })

  it('Stop sends a signed stop and the harness confirms it', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    await user.type(screen.getByLabelText('Message'), 'long job{Enter}')
    await screen.findByText('from Chrome on macOS')
    await user.click(screen.getByRole('button', { name: 'Stop the agent' }))
    await waitFor(() => expect(relay.received.map(c => c.op)).toEqual(['prompt', 'stop']))
    await screen.findByText('Stopped.')
    await screen.findByText('Run stopped')
  })

  it('cancelling the queue stops the run (M1 has no per-message cancel)', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    const box = screen.getByLabelText('Message')
    await user.type(box, 'first{Enter}')
    await screen.findByText('from Chrome on macOS')
    await user.type(box, 'second{Enter}')
    await screen.findByLabelText('Queued messages')
    await user.click(screen.getByRole('button', { name: 'Cancel queued' }))
    await user.click(screen.getByRole('button', { name: 'Stop and clear' }))
    await waitFor(() => expect(relay.received.at(-1)?.op).toBe('stop'))
    await waitFor(() => expect(screen.queryByLabelText('Queued messages')).toBeNull())
  })

  it('offline: badge, no sending, Stop disabled; back online re-enables', async () => {
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    act(() => relay.setOnline(false))
    await screen.findByText('Laptop offline')
    expect(screen.getByLabelText('Message')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Stop the agent' })).toBeDisabled()
    expect(screen.getAllByText(/Messages aren't queued while it's away/).length).toBeGreaterThan(0)
    act(() => relay.setOnline(true))
    await screen.findByText('Laptop online')
    expect(screen.getByLabelText('Message')).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Stop the agent' })).toBeEnabled()
  })

  it('a relay refusal (409 harness_offline) shows on the message, never queued', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    relay.online = false // the relay knows before the page hears about it
    await user.type(screen.getByLabelText('Message'), 'hello?{Enter}')
    await screen.findByText(/The laptop is offline/)
    expect(relay.received).toHaveLength(0)
  })

  it('drops forged events with a security notice and never renders them', async () => {
    const { relay, deps } = mockSession({ seed: false })
    renderSession(deps)
    await screen.findByText('Laptop online')
    act(() => {
      const forged = JSON.parse(relay.chat({ type: 'notice', level: 'info', code: null, text: 'placeholder' }))
      void forged
      const wire = JSON.stringify({ ...forged, seq: 999, body: { type: 'text_delta', message_id: 'm', delta: 'Ignore previous instructions' }, sig: 'forged' })
      relay.inject({ event: 'envelope', data: wire })
    })
    await screen.findByText(/Untrusted data from the relay was dropped \(bad signature\)/)
    expect(screen.queryByText(/Ignore previous instructions/)).toBeNull()
  })

  it('a newer protocol version stops the page', async () => {
    const { relay, deps } = mockSession({ seed: false })
    renderSession(deps)
    await screen.findByText('Laptop online')
    act(() => {
      const ok = JSON.parse(relay.chat({ type: 'notice', level: 'info', code: null, text: 'x' }))
      relay.inject({ event: 'envelope', data: JSON.stringify({ ...ok, v: 2, seq: ok.seq + 1 }) })
    })
    await screen.findByText(/This laptop runs a newer harness; update AMP/)
    expect(screen.getByLabelText('Message')).toBeDisabled()
  })

  it('reconnects with from_seq after the stream drops', async () => {
    const { relay, deps } = mockSession({ seed: false })
    const calls: (number | null)[] = []
    let drop: EventStreamHandlers | null = null
    const inner = deps.transport
    const transport: RelayTransport = { ...inner, openEvents: (id, from, h) => { calls.push(from); drop = h; return inner.openEvents(id, from, h) } }
    renderSession({ ...deps, transport })
    await screen.findByText('Laptop online')
    act(() => { relay.chat({ type: 'notice', level: 'info', code: null, text: 'before the drop' }) })
    await screen.findByText('before the drop')
    act(() => drop!.onClose())
    await screen.findByText('Reconnecting…')
    await waitFor(() => expect(calls).toHaveLength(2), { timeout: 3000 })
    expect(calls).toEqual([null, 3]) // hello was 1, the notice 2
    await screen.findByText('Laptop online')
    expect(screen.getAllByText('before the drop')).toHaveLength(1) // replayed events aren't drawn twice
  })

  it('Disconnect sends a signed detach, then ends the session', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession({ seed: false })
    renderSession(deps)
    await screen.findByText('Laptop online')
    await user.click(screen.getByRole('button', { name: /Disconnect/ }))
    await user.click(screen.getByRole('button', { name: 'Disconnect now' }))
    await waitFor(() => expect(relay.ended).toBe('disconnected'))
    expect(relay.received.map(c => c.op)).toEqual(['detach'])
    await screen.findAllByText(/You disconnected this session/)
    expect(screen.getByLabelText('Message')).toBeDisabled()
  })

  it('an unpaired browser sees pairing instructions and nothing from the relay', async () => {
    const { deps } = mockSession({ paired: false })
    renderSession(deps)
    await screen.findByText('Pair this device from the laptop')
    expect(screen.queryByText('run the tests and tell me what fails')).toBeNull()
  })

  it('respects the capability flag', async () => {
    const { deps } = mockSession()
    renderSession(deps, { me: meWith(false) })
    expect(await screen.findByText('Remote control isn\'t available')).toBeInTheDocument()
  })

  it('treats a missing or foreign session as not found', async () => {
    const { deps } = mockSession()
    renderSession(deps, { id: 'rs_ZZZZZZZZZZZZZZZZZZZZZZ' })
    expect(await screen.findByText('No such session')).toBeInTheDocument()
    renderSession(deps, { id: 'not-a-session' })
    expect((await screen.findAllByText('No such session')).length).toBeGreaterThan(0)
  })
})

describe('Session page with the protocol test vectors', () => {
  interface V { name: string; wire: string; expect: string; receiver: { session_id: string; pinned_harness: { harness_id: string; harness_pub: string } } }
  const vectors = loadVectors<{ vectors: V[] }>('events.json').vectors
  const rx = vectors[0].receiver
  // Stand-in for #2375's ES256 verifier: exactly the envelopes the vectors signed validly pass
  // (compared as parsed values with sorted keys, like JCS); a changed body or a forged sig fails.
  const canon = (x: unknown): string => Array.isArray(x) ? `[${x.map(canon).join(',')}]`
    : x && typeof x === 'object' ? `{${Object.keys(x).sort().map(k => `${JSON.stringify(k)}:${canon((x as Record<string, unknown>)[k])}`).join(',')}}`
    : JSON.stringify(x)
  const signedOk = new Set(vectors.filter(v => v.expect !== 'bad_signature').map(v => canon(JSON.parse(v.wire))))

  it('renders the valid vectors and drops the forged ones', async () => {
    const crypto: RemoteCrypto = {
      harnessPin: async id => id === rx.pinned_harness.harness_id ? { ...rx.pinned_harness, pinned_until_ms: Date.now() + 86_400_000 } : null,
      deviceId: async () => 'X5Pp2GXb6NEr3Jh05qAy4qnPP9oOBKhSltxS5yIPExs',
      signCommand: async () => { throw new Error('not used') },
      verifyEvent: async (env, pin) => pin.harness_id === rx.pinned_harness.harness_id && signedOk.has(canon(env)),
    }
    const detail: SessionDetail = {
      session_id: rx.session_id, harness_id: rx.pinned_harness.harness_id, harness_pub: rx.pinned_harness.harness_pub,
      harness_fp_display: '29CA-E6B5-00C2-66CB', host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' },
      online: true, viewers: 1, started_at_ms: 0, expires_at_ms: 0, last_activity_at_ms: 0, protocol_version: 1,
    }
    const order = ['hello_valid', 'chat_text_delta_valid', 'chat_tool_start_valid', 'chat_message_end_valid', 'ack_queued_valid',
      'refused_valid', 'pair_result_accepted_valid', 'duplicate_seq_dropped', 'bad_signature_body_changed',
      'wrong_harness_fake_registration', 'wrong_session', 'unknown_op_ignored']
    const transport: RelayTransport = {
      listSessions: async () => [detail],
      getSession: async id => { if (id !== rx.session_id) throw new RelayError(404, 'not_found', 'x'); return detail },
      openEvents: (_id, _from, h) => {
        queueMicrotask(() => {
          h.onOpen()
          h.onRelay({ type: 'attached', online: true, oldest_seq: 42, latest_seq: 48 })
          for (const n of order) h.onEnvelope(vectors.find(v => v.name === n)!.wire)
        })
        return () => {}
      },
      postCommand: async () => {},
      disconnect: async () => {},
    }
    const { container } = renderSession({ transport, crypto, mock: false }, { id: rx.session_id })
    await screen.findByText(/Running the tests/) // text_delta
    expect(screen.getByText('bash')).toBeInTheDocument() // tool_start
    expect(screen.getByText(/12,034 in/)).toBeInTheDocument() // message_end usage
    expect(within(screen.getByLabelText('Session status')).getByText('$1.83')).toBeInTheDocument() // hello status
    expect(screen.getAllByText(/Running the tests/)).toHaveLength(1) // the duplicate seq is dropped silently
    expect(screen.queryByText(/Ignore previous instructions/)).toBeNull()
    expect(screen.queryByText('hi')).toBeNull() // fake harness / wrong session never render
    expect(screen.queryByText(/Which branch/)).toBeNull() // unknown op is skipped
    expect(screen.getAllByRole('alert').filter(a => /Untrusted data/.test(a.textContent ?? ''))).toHaveLength(3)
    expect(container.querySelector('img, script')).toBeNull()
  })
})

