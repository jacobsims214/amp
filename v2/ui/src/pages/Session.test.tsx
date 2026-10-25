import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { Session } from './Session'
import { AuthContext } from '../contexts/auth-context'
import { RemoteProvider } from '../remote/RemoteProvider'
import type { RemoteDeps } from '../remote/remote-context'
import type { RemoteCrypto } from '../remote/crypto'
import { memoryStore } from '../remote/deviceStore'
import { webRemoteCrypto } from '../remote/webCrypto'
import { httpTransport, RelayError, type EventStreamHandlers, type RelayTransport } from '../remote/transport'
import { MOCK_SESSION_ID, MockRelay, mockCrypto, mockTransport, workerScene } from '../remote/mock/mockRelay'
import parityStream from '../remote/mock/parityStream.json'
import { loadVectors } from '../test/vectors'
import type { Me } from '../types'
import type { ChatEvent, SessionDetail } from '../remote/types'

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
    expect(screen.getByText(/^Thought for \d+s$/).closest('details')).not.toHaveAttribute('open') // thinking_delta, folded
    expect(screen.getByText(/Running the/)).toBeInTheDocument() // message_start + text_delta (Markdown)
    expect(screen.getAllByTestId('tool-row')).toHaveLength(1) // tool_start + tool_end
    expect(screen.getByText(/Full output \(178 KB\) is on the laptop/)).toBeInTheDocument()
    // retry: shown only while it counts down (the TUI's status slot), so not once the run went on.
    expect(screen.queryByText(/Retrying/)).toBeNull()
    expect(within(screen.getByLabelText('Session status')).getByTitle(/25,154 in/)).toBeInTheDocument() // message_end usage
    expect(screen.queryByTestId('run-end')).toBeNull() // run_end: a completed run draws nothing, as in the TUI
    expect(screen.getByText(/context7.*reconnected/)).toBeInTheDocument() // notice
    expect(within(screen.getByLabelText('Workers')).getByText('RC-M1 harness link')).toBeInTheDocument() // worker_status
    // Hostile model output: no images, no script, link with its full URL.
    expect(container.querySelector('img, script, iframe')).toBeNull()
    expect(screen.getByRole('link', { name: /the CI run \(https:\/\/github\.com\/jacobsims214\/amp\/actions\)/ })).toBeInTheDocument()

    const status = screen.getByLabelText('Session status')
    for (const t of ['claude-opus-5-5', '37%', '$1.83']) expect(within(status).getByText(t)).toBeInTheDocument()
    expect(screen.getByTestId('header-facts')).toHaveTextContent('AMP #2 · ticket #2372 · remote-m1-2372 · macos')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('jacobs-mbp')
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
    await screen.findByText(/Chrome on macOS \(this device\)/)
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

  it('sends slash text as a literal prompt, never as a command or another op (R-M6, #2379)', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    await user.type(screen.getByLabelText('Message'), '/remote off{Enter}')
    await waitFor(() => expect(relay.received).toHaveLength(1))
    expect(relay.received[0].op).toBe('prompt')
    expect(relay.received[0].body).toMatchObject({ text: '/remote off' })
    // Still connected: the page didn't treat it as Disconnect.
    expect(screen.getByText('Laptop online')).toBeInTheDocument()
    expect(relay.received.map(c => c.op)).toEqual(['prompt'])
  })

  it('Stop sends a signed stop and the harness confirms it', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    await user.type(screen.getByLabelText('Message'), 'long job{Enter}')
    await screen.findByText(/Chrome on macOS \(this device\)/)
    await user.click(screen.getByRole('button', { name: 'Stop the agent' }))
    await waitFor(() => expect(relay.received.map(c => c.op)).toEqual(['prompt', 'stop']))
    await screen.findByText('Stopped.')
    // The shell's "Stopped." says it once: run_end adds no second line.
    expect(screen.getAllByText('Stopped.')).toHaveLength(1)
  })

  it('cancelling the queue stops the run (M1 has no per-message cancel)', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    const box = screen.getByLabelText('Message')
    await user.type(box, 'first{Enter}')
    await screen.findByText(/Chrome on macOS \(this device\)/)
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
  it('renders the valid vectors and drops the forged ones', async () => {
    // The real WebCrypto implementation, with the vectors' harness pinned in an in-memory store.
    const store = memoryStore()
    await store.putPin({ ...rx.pinned_harness, pinned_until_ms: Date.now() + 86_400_000 })
    const crypto: RemoteCrypto = webRemoteCrypto(store)
    const detail: SessionDetail = {
      session_id: rx.session_id, harness_id: rx.pinned_harness.harness_id, harness_pub: rx.pinned_harness.harness_pub,
      harness_fp_display: '29CA-E6B5-00C2-66CB', host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' },
      online: true, viewers: 1, started_at_ms: 0, expires_at_ms: 0, last_activity_at_ms: 0, protocol_version: 1,
    }
    const order = ['hello_valid', 'chat_text_delta_valid', 'chat_tool_start_valid', 'chat_message_end_valid', 'ack_queued_valid',
      'refused_valid', 'pair_result_accepted_valid', 'duplicate_seq_dropped', 'bad_signature_body_changed',
      'wrong_harness_fake_registration', 'bad_signature_claims_pinned_harness', 'wrong_session', 'unknown_op_ignored']
    const transport: RelayTransport = {
      listSessions: async () => [detail],
      revokeAll: async () => ({ ended: 0, devices_revoked: 0 }),
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
      pair: async () => { throw new Error('not used') },
    }
    const { container } = renderSession({ transport, crypto, mock: false }, { id: rx.session_id })
    await screen.findByText(/Running the tests/) // text_delta
    // Real ES256 verification is asynchronous: wait until the last vector has been through the check.
    await waitFor(() => expect(screen.getAllByRole('alert').filter(a => /Untrusted data/.test(a.textContent ?? ''))).toHaveLength(4))
    expect(screen.getByText('bash')).toBeInTheDocument() // tool_start
    expect(within(screen.getByLabelText('Session status')).getByTitle(/12,034 in/)).toBeInTheDocument() // message_end usage
    expect(within(screen.getByLabelText('Session status')).getByText('$1.83')).toBeInTheDocument() // hello status
    expect(screen.getAllByText(/Running the tests/)).toHaveLength(1) // the duplicate seq is dropped silently
    expect(screen.queryByText(/Ignore previous instructions/)).toBeNull()
    expect(screen.queryByText('hi')).toBeNull() // fake harness / wrong session never render
    expect(screen.queryByText(/Which branch/)).toBeNull() // unknown op is skipped
    expect(screen.getAllByRole('alert').filter(a => /Untrusted data/.test(a.textContent ?? ''))).toHaveLength(4)
    // pair_result for another device: a visible warning, not silence.
    expect(screen.getByText(/paired another device/)).toBeInTheDocument()
    expect(container.querySelector('img, script')).toBeNull()
  })
})


describe('Session page reads like the harness TUI (#2386)', () => {
  const recorded = parityStream as { at_ms: number; event: ChatEvent }[]
  const withStream = (stream: { at_ms: number; event: ChatEvent }[]) => {
    const relay = new MockRelay({ stepMs: 0, stream })
    return { relay, deps: { transport: mockTransport(relay), crypto: mockCrypto(), mock: false } as RemoteDeps }
  }

  it('renders the recorded harness feed with the TUI\'s structure', async () => {
    const { deps } = withStream(recorded)
    renderSession(deps)
    await screen.findByText('try again')
    // Thinking folded with its duration, both sections.
    expect(screen.getAllByText(/^Thought for \d+s$/).map(e => e.textContent)).toEqual(['Thought for 4s', 'Thought for 1s'])
    // Tool calls grouped; the six-call group folds its first three successful calls.
    const groups = screen.getAllByTestId('tool-group')
    expect(groups).toHaveLength(3)
    const fold = within(groups[1]).getByTestId('tool-fold')
    expect(fold).toHaveTextContent('3 calls·create_epic, create_story, create_tasks')
    const failed = within(groups[1]).getAllByTestId('tool-row').find(r => r.dataset.state === 'failed')!
    expect(failed).toHaveTextContent('assign_task #526')
    expect(within(failed).getByTestId('tool-summary')).toHaveTextContent('"me" needs a signed-in AMP user')
    expect(failed).toHaveTextContent('6 ms')
    expect(within(groups[2]).getByTestId('tool-summary')).toHaveTextContent('30 lines')
    // The remote turns carry their device; the local ones say so.
    expect(screen.getAllByTestId('remote-tag').map(t => t.textContent)).toEqual(['RemoteSafari on iPhone', 'RemoteSafari on iPhone'])
    // Stopped once; the failed run's two error lines; no extra run-end lines.
    expect(screen.getAllByText('Stopped.')).toHaveLength(1)
    expect(screen.getByText('✗ Error: 429 Too Many Requests: rate limit exceeded for this key')).toBeInTheDocument()
    expect(screen.getByText('✗ Error: No more faux responses queued')).toBeInTheDocument()
    expect(screen.queryByTestId('run-end')).toBeNull()
    // Markdown table and the link's full URL.
    expect(screen.getByRole('table')).toHaveTextContent('Research JetStream APIs')
    expect(screen.getByRole('link', { name: /amp\.example\.test\/project\/158\/plan/ })).toBeInTheDocument()
  })

  it('opens a tool call to its arguments and pretty-printed result', async () => {
    const user = userEvent.setup()
    const { deps } = withStream(recorded)
    renderSession(deps)
    await screen.findByText('try again')
    const row = screen.getAllByTestId('tool-row').find(r => r.textContent?.includes('get_task #514'))!
    await user.click(within(row).getByText('get_task #514'))
    expect(row).toHaveAttribute('open')
    expect(within(row).getByText(/"comments_total": 3/)).toBeInTheDocument()
  })

  it('shows a run in flight: working timer, running call, and the dispatched workers', async () => {
    const t0 = Date.now() - 3_400
    const { deps } = withStream(workerScene(t0))
    renderSession(deps)
    await screen.findByText('dispatch both')
    expect(screen.getByTestId('working')).toHaveTextContent(/Working… 3s/)
    const rows = screen.getAllByTestId('tool-row')
    expect(rows.map(r => r.dataset.state)).toEqual(['running', 'running'])
    expect(within(rows[0]).getByTestId('worker-live')).toHaveTextContent('Add the clamp helper with a fairly long ticket name to test cuts · worker-backend (wf-1) · running 2s')
    const chips = within(screen.getByLabelText('Workers')).getAllByTestId('worker-chip')
    expect(chips.map(c => c.textContent)).toEqual([
      '#218Add the clamp helper with a fairly long ticket name to test cuts· worker-backend (wf-1)· running 2s',
      '#219Write the README line· worker-backend (wf-1)· running 2s',
    ])
    expect(screen.getByText(/▶ worker started #218/)).toBeInTheDocument()
  })

  it('counts a retry down in the working line while it is pending', async () => {
    const now = Date.now()
    const { deps } = withStream([
      { at_ms: now - 500, event: { type: 'user_message', prompt_id: 'p_1', text: 'go', origin: 'local', device_id: null, device_name: null } },
      { at_ms: now - 500, event: { type: 'run_start', run_id: 'run_1', prompt_id: 'p_1' } },
      { at_ms: now - 400, event: { type: 'retry', attempt: 1, max_retries: 10, delay_ms: 2000, error: '429 Too Many Requests' } },
    ])
    renderSession(deps)
    await screen.findByText('go')
    expect(screen.getByTestId('working')).toHaveTextContent(/Retrying \(1\/10\) in 2s…429 Too Many RequestsStop to cancel/)
  })
})

describe('Session page shows what the TUI shows besides (#2388)', () => {
  interface V { name: string; wire: string; receiver: { session_id: string; pinned_harness: { harness_id: string; harness_pub: string } } }
  const vectors = loadVectors<{ vectors: V[] }>('events.json').vectors
  const rx = vectors[0].receiver
  it('renders the #2388 vectors: ticket line, board, AMP connection, worker chips, compaction, error sentence, refused line', async () => {
    const store = memoryStore()
    await store.putPin({ ...rx.pinned_harness, pinned_until_ms: Date.now() + 86_400_000 })
    const detail: SessionDetail = {
      session_id: rx.session_id, harness_id: rx.pinned_harness.harness_id, harness_pub: rx.pinned_harness.harness_pub,
      harness_fp_display: '29CA-E6B5-00C2-66CB', host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' },
      online: true, viewers: 1, started_at_ms: 0, expires_at_ms: 0, last_activity_at_ms: 0, protocol_version: 1,
    }
    const order = ['hello_valid', 'status_tui_facts_valid', 'chat_worker_status_tui_facts_valid', 'chat_compaction_valid',
      'chat_message_end_error_summary_valid', 'chat_tool_end_refused_valid']
    const transport: RelayTransport = {
      listSessions: async () => [detail],
      revokeAll: async () => ({ ended: 0, devices_revoked: 0 }),
      getSession: async () => detail,
      openEvents: (_id, _from, h) => {
        queueMicrotask(() => {
          h.onOpen()
          h.onRelay({ type: 'attached', online: true, oldest_seq: 42, latest_seq: 53 })
          for (const n of order) h.onEnvelope(vectors.find(v => v.name === n)!.wire)
        })
        return () => {}
      },
      postCommand: async () => {},
      disconnect: async () => {},
      pair: async () => { throw new Error('not used') },
    }
    renderSession({ transport, crypto: webRemoteCrypto(store), mock: false }, { id: rx.session_id })
    // The last vector (refused line) has been through the real ES256 check.
    const refused = await screen.findByTestId('tool-summary')
    expect(refused).toHaveTextContent('AMP refused: this needs a person in the AMP web UI (below)')
    expect(refused.closest('[data-testid=tool-row]')).toHaveAttribute('data-state', 'failed')
    // Gap 2: the TUI's ticket line, header facts and status line.
    const ticket = screen.getByTestId('ticket-line')
    expect(ticket).toHaveTextContent('In progress')
    expect(ticket).toHaveTextContent('#2388')
    expect(ticket).toHaveTextContent('RC-M1 harness + UI: ChatEvent carries everything the TUI shows')
    expect(within(ticket).getByTestId('ticket-epic')).toHaveTextContent('Remote control › M1')
    expect(screen.getByTestId('header-facts')).toHaveTextContent('AMP #2 · amp (remote-m1-2388) · manager · macos')
    const status = screen.getByLabelText('Session status')
    expect(within(status).getByText('• high')).toBeInTheDocument()
    expect(within(status).getByText('1.0M')).toBeInTheDocument() // the harness's totals, not this page's sum
    expect(within(status).getByTitle(/120,340 in · 8,311 out · 901,200 cached, the session's totals on the laptop/)).toBeInTheDocument()
    expect(within(status).getByText('$2.10')).toBeInTheDocument()
    const board = screen.getByTestId('board-line')
    expect(board).toHaveTextContent('1 question')
    expect(board).toHaveTextContent('3 In progress')
    expect(board).toHaveTextContent('5 Ready')
    expect(within(board).getByTestId('plan-approval')).toHaveTextContent('plan: 2 waiting for your approval')
    // Gap 6: the harness's own AMP connection.
    expect(screen.getByTestId('amp-connection')).toHaveTextContent('slow 812 ms')
    // Gap 1: worker chips with epic › story, tokens, context and cost.
    const chips = screen.getAllByTestId('worker-chip')
    expect(within(chips[0]).getByTestId('worker-epic')).toHaveTextContent('QA3d epic › Watch workers')
    expect(within(chips[0]).getByTestId('worker-tokens')).toHaveTextContent('7.8k tok')
    expect(within(chips[0]).getByTestId('worker-ctx')).toHaveTextContent('ctx 8%')
    expect(within(chips[0]).getByTestId('worker-cost')).toHaveTextContent('$0.04')
    expect(within(chips[1]).getByTestId('worker-state')).toHaveTextContent('completed after 3m10s')
    // Gap 3: compaction in the working line.
    expect(screen.getByTestId('working')).toHaveTextContent('Auto-compacting…')
    // Gap 4: the TUI's one sentence, the raw provider text one click away.
    expect(screen.getByTestId('reply-error')).toHaveTextContent('✗ Error: OpenRouter rate-limited the request (429): Rate limit exceeded: free-models-per-day. Wait a minute and try again, or pick another model.')
    expect(within(screen.getByTestId('reply-error-detail')).getByText(/^http error 429:/)).toBeInTheDocument()
  })
})

describe('Session page while amp-relay is down', () => {
  it('says remote control is unavailable instead of a raw 503', async () => {
    const fetchMock = vi.fn(async () => new Response('no healthy upstream', { status: 503, headers: { 'Content-Type': 'text/plain' } }))
    vi.stubGlobal('fetch', fetchMock)
    try {
      renderSession({ transport: httpTransport, crypto: mockCrypto(), mock: false })
      expect(await screen.findByText("Remote control is unavailable right now (the relay service isn't running).")).toBeInTheDocument()
      expect(screen.queryByText(/Request failed/)).toBeNull()
      expect(fetchMock).toHaveBeenCalled()
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
