/**
 * The session page's state: a reducer over verified events (protocol 6),
 * relay frames (unsigned presence/status from AMP) and this browser's own
 * outgoing commands. Pure, so it is tested without React.
 */
import type {
  AckBody, ByeBody, ChatEvent, Envelope, EventVerdict, HelloBody, PairResultBody, QueueItem, RefusedBody, RelayFrame,
  SessionStatus, Usage, WorkerInfo,
} from './types'

export type TranscriptItem =
  | { kind: 'user'; key: string; prompt_id: string; text: string; origin: 'local' | 'remote'; device_name: string | null }
  | { kind: 'assistant'; key: string; message_id: string; model: string; provider: string; text: string; thinking: string;
      done: boolean; stop_reason: string | null; error: string | null; usage: Usage | null }
  | { kind: 'tool'; key: string; tool_call_id: string; name: string; arguments_json: string; args_truncated: boolean;
      result: { text: string; is_error: boolean; truncated: boolean; full_bytes: number } | null }
  | { kind: 'run_end'; key: string; run_id: string; outcome: string; error: string | null }
  | { kind: 'notice'; key: string; level: 'info' | 'warn' | 'error'; code: string | null; text: string }
  | { kind: 'retry'; key: string; attempt: number; max_retries: number; delay_ms: number; error: string }
  | { kind: 'gap'; key: string }
  | { kind: 'security'; key: string; verdict: EventVerdict }

/** A prompt or command this browser sent, until the harness answers. */
export interface Outgoing {
  seq: number
  op: 'prompt' | 'stop' | 'detach'
  client_msg_id: string | null
  text: string | null
  state: 'sending' | 'forwarded' | 'started' | 'queued' | 'done' | 'refused' | 'no_answer' | 'failed'
  prompt_id: string | null
  /** Why it failed, in words for people. */
  problem: string | null
}

export type Connection = 'connecting' | 'live' | 'reconnecting'

export interface SessionState {
  hello: HelloBody | null
  status: SessionStatus | null
  items: TranscriptItem[]
  queue: QueueItem[]
  workers: WorkerInfo[]
  outgoing: Outgoing[]
  /** From relay frames; null until the stream attaches. */
  online: boolean | null
  connection: Connection
  /** The run the harness is in, from run_start/run_end. */
  running: string | null
  ended: { reason: string } | null
  /** The harness speaks a newer protocol: stop applying events (5.7 step 3). */
  versionMismatch: boolean
  lastAppliedSeq: number
}

export const initialSessionState: SessionState = {
  hello: null, status: null, items: [], queue: [], workers: [], outgoing: [], online: null,
  connection: 'connecting', running: null, ended: null, versionMismatch: false, lastAppliedSeq: 0,
}

export type SessionAction =
  | { type: 'event'; env: Envelope; myDeviceId: string | null }
  | { type: 'skip'; seq: number }
  | { type: 'snapshot'; env: Envelope }
  | { type: 'relay'; frame: RelayFrame }
  | { type: 'connection'; connection: Connection }
  | { type: 'untrusted'; verdict: EventVerdict }
  | { type: 'version_mismatch' }
  | { type: 'outgoing'; out: Outgoing }
  | { type: 'outgoing_update'; seq: number; patch: Partial<Outgoing> }
  | { type: 'outgoing_timeout'; seq: number }
  | { type: 'ended'; reason: string }

let keyCounter = 0
const nextKey = (p: string) => `${p}-${++keyCounter}`

/** Human words for harness refusal codes (protocol 9.2) and relay errors. */
export function refusalText(code: string, message?: string | null): string {
  const known: Record<string, string> = {
    harness_offline: 'The laptop is offline. Messages aren\'t queued while it\'s away.',
    queue_full: 'Five messages are already queued. Wait for the agent, or press Stop.',
    rate_limited: 'Too many messages too quickly. Try again in a minute.',
    unknown_device: 'The laptop doesn\'t know this browser. Pair it again from the laptop.',
    device_expired: 'This browser\'s pairing has expired. Pair it again from the laptop.',
    stale_ts: 'This device\'s clock is off by more than a minute.',
    session_ended: 'This session has ended.',
    too_large: 'That message is too large.',
  }
  return known[code] ?? (message ? message : `The laptop refused it (${code}).`)
}

function applyChat(s: SessionState, ev: ChatEvent): SessionState {
  const items = s.items
  const replaceWhere = <K extends TranscriptItem['kind']>(kind: K, pred: (i: Extract<TranscriptItem, { kind: K }>) => boolean,
    fn: (i: Extract<TranscriptItem, { kind: K }>) => TranscriptItem): TranscriptItem[] | null => {
    for (let n = items.length - 1; n >= 0; n--) {
      const it = items[n]
      if (it.kind === kind && pred(it as Extract<TranscriptItem, { kind: K }>)) {
        const copy = items.slice()
        copy[n] = fn(it as Extract<TranscriptItem, { kind: K }>)
        return copy
      }
    }
    return null
  }
  const assistant = (message_id: string, model = '', provider = ''): TranscriptItem => ({
    kind: 'assistant', key: nextKey('a'), message_id, model, provider, text: '', thinking: '', done: false,
    stop_reason: null, error: null, usage: null,
  })

  switch (ev.type) {
    case 'user_message':
      return {
        ...s,
        items: [...items, { kind: 'user', key: nextKey('u'), prompt_id: ev.prompt_id, text: ev.text, origin: ev.origin, device_name: ev.device_name ?? null }],
        // Our own prompt is now in the transcript: drop its outgoing placeholder.
        outgoing: s.outgoing.filter(o => o.prompt_id !== ev.prompt_id),
      }
    case 'run_start':
      return { ...s, running: ev.run_id }
    case 'message_start':
      return { ...s, items: [...items, assistant(ev.message_id, ev.model, ev.provider)] }
    case 'text_delta':
    case 'thinking_delta': {
      const field = ev.type === 'text_delta' ? 'text' : 'thinking'
      const next = replaceWhere('assistant', a => a.message_id === ev.message_id, a => ({ ...a, [field]: a[field] + ev.delta }))
      if (next) return { ...s, items: next }
      // A delta whose message_start was before our buffer (a gap): start the block here.
      const a = assistant(ev.message_id) as Extract<TranscriptItem, { kind: 'assistant' }>
      return { ...s, items: [...items, { ...a, [field]: ev.delta }] }
    }
    case 'tool_start':
      return { ...s, items: [...items, { kind: 'tool', key: nextKey('t'), tool_call_id: ev.tool_call_id, name: ev.name,
        arguments_json: ev.arguments_json, args_truncated: !!ev.truncated, result: null }] }
    case 'tool_end': {
      const result = { text: ev.text, is_error: !!ev.is_error, truncated: !!ev.truncated, full_bytes: ev.full_bytes }
      const next = replaceWhere('tool', t => t.tool_call_id === ev.tool_call_id, t => ({ ...t, result }))
      return { ...s, items: next ?? [...items, { kind: 'tool', key: nextKey('t'), tool_call_id: ev.tool_call_id, name: ev.name,
        arguments_json: '', args_truncated: false, result }] }
    }
    case 'message_end': {
      const patch = { done: true, stop_reason: ev.stop_reason, error: ev.error ?? null, usage: ev.usage ?? null, model: ev.model, provider: ev.provider }
      const next = replaceWhere('assistant', a => a.message_id === ev.message_id, a => ({ ...a, ...patch }))
      return { ...s, items: next ?? [...items, { ...(assistant(ev.message_id) as Extract<TranscriptItem, { kind: 'assistant' }>), ...patch }] }
    }
    case 'retry':
      return { ...s, items: [...items, { kind: 'retry', key: nextKey('r'), attempt: ev.attempt, max_retries: ev.max_retries, delay_ms: ev.delay_ms, error: ev.error }] }
    case 'run_end':
      return { ...s, running: null, items: [...items, { kind: 'run_end', key: nextKey('e'), run_id: ev.run_id, outcome: ev.outcome, error: ev.error ?? null }] }
    case 'notice':
      return { ...s, items: [...items, { kind: 'notice', key: nextKey('n'), level: ev.level, code: ev.code ?? null, text: ev.text }] }
    case 'queue':
      return { ...s, queue: Array.isArray(ev.items) ? ev.items : [] }
    case 'worker_status':
      return { ...s, workers: Array.isArray(ev.workers) ? ev.workers : [] }
    default:
      return s // unknown types are ignored (6.4)
  }
}

function applyEvent(s: SessionState, env: Envelope, myDeviceId: string | null): SessionState {
  const base = { ...s, lastAppliedSeq: env.seq }
  switch (env.op) {
    case 'hello': {
      const hello = env.body as HelloBody
      return { ...base, hello, status: hello.status ?? base.status }
    }
    case 'status':
      return { ...base, status: env.body as SessionStatus }
    case 'chat_event':
      return applyChat(base, env.body as ChatEvent)
    case 'ack': {
      const b = env.body as AckBody
      if (!myDeviceId || env.device_id !== myDeviceId || b.ref?.device_id !== myDeviceId) return base
      return { ...base, outgoing: base.outgoing.map(o => o.seq === b.ref.seq
        ? { ...o, state: b.result, prompt_id: b.prompt_id ?? o.prompt_id, problem: null } : o)
        // stop/detach results need no placeholder once answered
        .filter(o => !(o.seq === b.ref.seq && o.op !== 'prompt')) }
    }
    case 'refused': {
      const b = env.body as RefusedBody
      if (!myDeviceId || env.device_id !== myDeviceId || !b.ref || b.ref.device_id !== myDeviceId) return base
      return { ...base, outgoing: base.outgoing.map(o => o.seq === b.ref!.seq
        ? { ...o, state: 'refused', problem: refusalText(b.code, b.message) } : o) }
    }
    case 'bye':
      return { ...base, ended: { reason: (env.body as ByeBody).reason ?? 'harness_ended' } }
    case 'pair_result': {
      // This device's own pairing is handled by the pairing screen (usePairing). Here, tell
      // the reader when the laptop accepted another device, so a pairing they didn't make shows.
      const b = env.body as PairResultBody
      if (b.result !== 'accepted' || env.device_id === myDeviceId) return base
      return { ...base, items: [...base.items, { kind: 'notice', key: nextKey('n'), level: 'warn', code: 'device_paired',
        text: 'The laptop paired another device for remote control. If that wasn\'t you, run /remote off on the laptop.' }] }
    }
    default:
      return base
  }
}

export function sessionReducer(s: SessionState, a: SessionAction): SessionState {
  switch (a.type) {
    case 'event':
      if (s.versionMismatch) return s
      return applyEvent(s, a.env, a.myDeviceId)
    case 'skip':
      return { ...s, lastAppliedSeq: Math.max(s.lastAppliedSeq, a.seq) }
    case 'snapshot':
      // Snapshots restore the status bar; they never move lastAppliedSeq (5.7).
      if (a.env.op === 'hello') {
        const hello = a.env.body as HelloBody
        return { ...s, hello, status: s.status ?? hello.status ?? null }
      }
      if (a.env.op === 'status') return { ...s, status: a.env.body as SessionStatus }
      return s
    case 'relay': {
      const f = a.frame
      if (f.type === 'attached') return { ...s, online: f.online, connection: 'live' }
      if (f.type === 'online') return { ...s, online: true }
      if (f.type === 'offline') return { ...s, online: false, running: null }
      if (f.type === 'gap') return { ...s, items: [...s.items, { kind: 'gap', key: nextKey('g') }] }
      if (f.type === 'ended') return { ...s, ended: { reason: f.reason } }
      return s
    }
    case 'connection':
      return { ...s, connection: a.connection }
    case 'untrusted':
      return { ...s, items: [...s.items, { kind: 'security', key: nextKey('s'), verdict: a.verdict }] }
    case 'version_mismatch':
      return { ...s, versionMismatch: true }
    case 'outgoing':
      return { ...s, outgoing: [...s.outgoing, a.out] }
    case 'outgoing_update':
      return { ...s, outgoing: s.outgoing.map(o => o.seq === a.seq ? { ...o, ...a.patch } : o)
        .filter(o => !(o.seq === a.seq && a.patch.state === 'done' && o.op !== 'prompt')) }
    case 'outgoing_timeout':
      return { ...s, outgoing: s.outgoing.map(o => o.seq === a.seq && (o.state === 'sending' || o.state === 'forwarded')
        ? { ...o, state: 'no_answer', problem: 'No answer from the laptop.' } : o) }
    case 'ended':
      return { ...s, ended: { reason: a.reason } }
  }
}

/** The agent is working: a run is open, or the harness says busy. */
export function isBusy(s: SessionState): boolean {
  return s.running !== null || !!s.status?.busy
}

/** People-facing words for end reasons (protocol 12.2). */
export function endReasonText(reason: string): string {
  const m: Record<string, string> = {
    harness_ended: 'Remote control was turned off on the laptop.',
    detached: 'You disconnected this session.',
    disconnected: 'You disconnected this session.',
    revoked: 'All your remote sessions were revoked.',
    policy_off: 'Remote control was turned off for your organization.',
    idle: 'The session ended after 30 minutes without remote activity.',
    expired: 'The session reached its 12-hour limit.',
    killed: 'An admin ended this session.',
  }
  return m[reason] ?? 'This session has ended.'
}

/** Integer micro-USD for people: $1.83, or $12 once it's ten dollars or more. */
export function formatCost(micros: number | null | undefined): string {
  return micros == null ? '—' : `$${(micros / 1_000_000).toFixed(micros < 10_000_000 ? 2 : 0)}`
}
