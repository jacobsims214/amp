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
  ChatEvent, CommandEnvelope, HarnessPin, HelloBody, PromptBody, QueueItem, RelayFrame, SessionDetail, SessionStatus, SessionSummary,
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
  /** Replay a recorded harness feed instead (`at_ms` becomes each envelope's `ts`). */
  stream?: { at_ms: number; event: ChatEvent }[]
  /** Status fields to use instead of the defaults. */
  status?: Partial<SessionStatus>
  /** More sessions for the list (the Sessions page in dev), besides this one. */
  others?: SessionSummary[]
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
    // #2388: a compaction that started and finished.
    { type: 'compaction', active: true, manual: false },
    { type: 'compaction', active: false, manual: false },
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
  readonly others: SessionSummary[]
  /** Set by revoke-all. */
  revoked = false

  constructor(opts: MockRelayOptions = {}) {
    this.stepMs = opts.stepMs ?? 250
    this.online = opts.online ?? true
    this.others = opts.others ?? []
    if (opts.status) this.statusBody = { ...this.statusBody, ...opts.status }
    const hello: HelloBody = {
      harness: { harness_id: MOCK_HARNESS_ID, harness_pub: MOCK_HARNESS_PUB, host: 'jacobs-mbp', os: 'macos', version: '0.4.0' },
      user_id: 1001, ops: ['prompt', 'stop', 'detach'],
      started_at_ms: Date.now() - 3_600_000, expires_at_ms: null, idle_timeout_s: null,
      status: this.statusBody,
    }
    this.emit('hello', hello)
    if (opts.stream) for (const e of opts.stream) this.emit('chat_event', e.event, null, e.at_ms)
    else if (opts.seed) {
      // A little time between the seeded events, so durations read like a real run.
      const t0 = Date.now() - 120_000
      seedChatEvents().forEach((ev, i) => this.emit('chat_event', ev, null, t0 + i * 900))
    }
  }

  /** What GET /api/sessions returns. */
  list(): SessionSummary[] {
    const others = this.revoked ? this.others.map(o => o.state === 'ended' ? o : endedCopy(o, 'revoked')) : this.others
    const live = others.filter(o => o.state !== 'ended')
    const ended = others.filter(o => o.state === 'ended')
    const mine: SessionSummary = this.ended ? endedCopy(this.summary(), this.ended) : this.summary()
    // Live first, then the ended ones (protocol 8.3).
    return mine.state === 'ended' ? [...live, mine, ...ended] : [mine, ...live, ...ended]
  }

  /** A replayed feed that stops inside a run: the fake harness counts as busy, so prompts queue. */
  markRunning(run_id: string) { this.running = run_id; this.statusBody = { ...this.statusBody, busy: true } }

  /** POST /api/sessions/revoke-all. */
  revokeAll(): { ended: number; devices_revoked: number } {
    const ended = this.list().filter(s => s.state !== 'ended').length
    this.revoked = true
    if (!this.ended) this.end('revoked')
    return { ended, devices_revoked: 1 }
  }

  summary(): SessionDetail {
    return {
      session_id: this.sessionId, harness_id: MOCK_HARNESS_ID, harness_fp_display: '29CA-E6B5-00C2-66CB', harness_pub: MOCK_HARNESS_PUB,
      host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' }, online: this.online, viewers: this.listeners.size,
      started_at_ms: Date.now() - 3_600_000, expires_at_ms: null, last_activity_at_ms: Date.now(), protocol_version: 1,
      state: 'active', end_reason: null, ended_at_ms: null, model: this.statusBody.model,
      ticket: this.statusBody.ticket_id == null ? null : { id: this.statusBody.ticket_id, name: this.statusBody.ticket_name ?? null },
      devices: this.statusBody.devices.map(d => ({ ...d, paired_at_ms: Date.now() - 2 * 86_400_000, expires_at_ms: Date.now() + 28 * 86_400_000 })),
    }
  }

  /** Builds and publishes a harness event (the wire text a real harness would sign). */
  emit(op: string, body: unknown, device_id: string | null = null, ts = Date.now()): string {
    const env = { v: 1, alg: 'ES256', session_id: this.sessionId, harness_id: MOCK_HARNESS_ID, device_id,
      seq: ++this.seq, ts, nonce: `n${this.seq}`, op, body, sig: MOCK_SIG }
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

/** A session as GET /api/sessions lists it once it has ended. */
function endedCopy(s: SessionSummary, reason: string): SessionSummary {
  const out: SessionSummary & { harness_pub?: string } = { ...s, state: 'ended', end_reason: reason, ended_at_ms: Date.now(), online: false, viewers: 0 }
  delete out.harness_pub // the list never carries the key
  return out
}

/** A RelayTransport over a MockRelay (no HTTP at all). */
export function mockTransport(relay: MockRelay): RelayTransport {
  const check = (id: string) => { if (id !== relay.sessionId) throw new RelayError(404, 'not_found', 'No such session.') }
  return {
    listSessions: async () => relay.list(),
    revokeAll: async () => relay.revokeAll(),
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
    disconnect: async id => {
      const other = relay.others.findIndex(o => o.session_id === id && o.state !== 'ended')
      if (other >= 0) { relay.others[other] = endedCopy(relay.others[other], 'disconnected'); return }
      check(id); relay.end('disconnected')
    },
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
/**
 * The worker scene of the TUI capture qa-3d U120-05 (two dispatched workers running, #2347), as
 * ChatEvents: the harness feed that produced the capture wasn't recorded, so this one is
 * rebuilt from the capture's text. `t0` is when the prompt was sent.
 */
export function workerScene(t0: number): { at_ms: number; event: ChatEvent }[] {
  const m = 'msg_w1'
  const ev = (dt: number, event: ChatEvent) => ({ at_ms: t0 + dt, event })
  const worker = (task_id: number, name: string) => ({ task_id, name, role: 'amp:amp-worker-backend', state: 'running' as const,
    model: 'wf-1', started_at_ms: t0 + 900 })
  return [
    ev(0, { type: 'user_message', prompt_id: 'p_w1', text: 'dispatch both', origin: 'local', device_id: null, device_name: null }),
    ev(0, { type: 'run_start', run_id: 'run_w1', prompt_id: 'p_w1' }),
    ev(10, { type: 'message_start', message_id: m, model: 'boss-1', provider: 'boss' }),
    ev(200, { type: 'thinking_delta', message_id: m, delta: 'Both tickets are ready; dispatch them together.' }),
    ev(600, { type: 'message_end', message_id: m, stop_reason: 'tool_use', error: null, model: 'boss-1', provider: 'boss',
      usage: { input_tokens: 7400, output_tokens: 120, cache_read_tokens: 0, cache_write_tokens: 0, cost_usd_micros: 0 } }),
    ev(610, { type: 'tool_start', message_id: m, tool_call_id: 'c_218', name: 'amp_run_ticket', arguments_json: '{"task_id":218}', truncated: false }),
    ev(611, { type: 'tool_start', message_id: m, tool_call_id: 'c_219', name: 'amp_run_ticket', arguments_json: '{"task_id":219}', truncated: false }),
    ev(900, { type: 'notice', level: 'info', code: null, text: '▶ worker started  #218 Add the clamp helper with a fairly long… · QA3d epic › Watch workers · worker-backend (wf-1)' }),
    ev(901, { type: 'notice', level: 'info', code: null, text: '▶ worker started  #219 Write the README line · QA3d epic › Watch workers · worker-backend (wf-1)' }),
    ev(902, { type: 'worker_status', workers: [worker(218, 'Add the clamp helper with a fairly long ticket name to test cuts'), worker(219, 'Write the README line')] }),
  ]
}

export function devMockDeps(kind: string = '1', stream?: { at_ms: number; event: ChatEvent }[], opts: { cut?: number; scene?: string; offline?: boolean } = {}): RemoteDeps {
  const now = Date.now()
  const others: SessionSummary[] = [
    { session_id: 'rs_OtherSessionBBBBBBBBBB', harness_id: 'h-other', harness_fp_display: '8F1D-02AA-71C3-9E40', host: 'build-box',
      os: 'linux', harness_version: '0.4.0', project: { id: 158, name: 'QA 2365 plan' }, online: false, viewers: 0,
      started_at_ms: now - 5 * 3_600_000, expires_at_ms: now + 7 * 3_600_000, last_activity_at_ms: now - 42 * 60_000, protocol_version: 1,
      state: 'active', end_reason: null, ended_at_ms: null, model: 'openai/gpt-5.5', ticket: { id: 2389, name: 'Sessions list with ended sessions' },
      devices: [{ device_id: 'd-pixel', name: 'Chrome on Pixel 9', paired_at_ms: now - 3 * 86_400_000, expires_at_ms: now + 27 * 86_400_000 }] },
    { session_id: 'rs_EndedSessionCCCCCCCCCC', harness_id: 'h-old', harness_fp_display: '41B0-77E2-0C5A-D913', host: 'jacobs-mbp',
      os: 'macos', harness_version: '0.3.9', project: { id: 2, name: 'AMP' }, online: false, viewers: 0,
      started_at_ms: now - 30 * 3_600_000, expires_at_ms: now - 18 * 3_600_000, last_activity_at_ms: now - 26 * 3_600_000, protocol_version: 1,
      state: 'ended', end_reason: 'idle', ended_at_ms: now - 25 * 3_600_000, model: 'anthropic/claude-opus-5-5', ticket: null, devices: [] },
  ]
  let replay: { at_ms: number; event: ChatEvent }[] | undefined
  if (kind === 'parity') {
    if (opts.scene === 'workers') replay = workerScene(now - 3_400)
    else if (stream?.length) {
      // Replay the recorded feed as if it had just happened (cut part way to see a run in flight).
      const cut = opts.cut && opts.cut > 0 ? stream.slice(0, opts.cut) : stream
      const shift = now - 1_500 - cut[cut.length - 1].at_ms
      replay = cut.map(e => ({ at_ms: e.at_ms + shift, event: e.event }))
    }
  }
  const relay = replay
    ? new MockRelay({ stream: replay, others, online: !opts.offline, status: { model: 'faux/faux-1', context_pct: 4, cost_usd_micros: 177_000,
        project: { id: 158, name: 'QA 2365 plan' }, ticket_id: 514, branch: 'remote-m1', workers_running: opts.scene === 'workers' ? 2 : 0 } })
    : new MockRelay({ seed: true, others })
  // Stopped inside a run (a cut replay, the worker scene): the fake harness is busy.
  const open = replay?.reduce<string | null>((run, e) => e.event.type === 'run_start' ? e.event.run_id : e.event.type === 'run_end' ? null : run, null)
  if (open) relay.markRunning(open)
  return { transport: mockTransport(relay), crypto: mockCrypto({ paired: !pairingLinkFor(MOCK_SESSION_ID) }), mock: true }
}

/**
 * Dev wiring for `VITE_REMOTE_MOCK=parity2388` (#2388): the run recorded from the real harness
 * (parity2388.json) with the status its remote link sent. `cut` stops the replay after that many
 * events (a compaction in flight); `workers` adds the recorded worker_status of two real workers.
 */
export function devMockDeps2388(rec: { status: SessionStatus; stream: { at_ms: number; event: ChatEvent }[]; workers: { at_ms: number; event: ChatEvent } },
  opts: { cut?: number; workers?: boolean } = {}): RemoteDeps {
  const now = Date.now()
  const cut = opts.cut && opts.cut > 0 ? rec.stream.slice(0, opts.cut) : rec.stream
  const shift = now - 1_500 - cut[cut.length - 1].at_ms
  const replay = cut.map(e => ({ at_ms: e.at_ms + shift, event: e.event }))
  let status = rec.status
  if (opts.workers && rec.workers.event.type === 'worker_status') {
    // As captured: both workers running for 2 s.
    const started = now - 2_400
    replay.push({ at_ms: now - 400, event: { ...rec.workers.event, workers: rec.workers.event.workers.map(w => ({ ...w, started_at_ms: started })) } })
    status = { ...status, workers_running: rec.workers.event.workers.length }
  }
  const relay = new MockRelay({ stream: replay, status })
  const open = replay.reduce<string | null>((run, e) => e.event.type === 'run_start' ? e.event.run_id : e.event.type === 'run_end' ? null : run, null)
  if (open || opts.workers) relay.markRunning(open ?? 'run_workers')
  return { transport: mockTransport(relay), crypto: mockCrypto({ paired: true }), mock: true }
}
