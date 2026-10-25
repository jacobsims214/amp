/**
 * The session page's state: a reducer over verified events (protocol 6),
 * relay frames (unsigned presence/status from AMP) and this browser's own
 * outgoing commands. Pure, so it is tested without React.
 */
import type {
  AckBody, ByeBody, ChatEvent, Envelope, EventVerdict, HelloBody, PairResultBody, QueueItem, RefusedBody, RelayFrame,
  SessionStatus, Usage, WorkerInfo,
} from './types'

/** A run of thinking or text inside one assistant message, with when it started and ended
 *  (harness clock, the envelope's `ts`), so thinking reads "Thought for 9s" as in the TUI. */
export interface Block { kind: 'thinking' | 'text'; text: string; startedAt: number; endedAt: number | null }

export type TranscriptItem =
  | { kind: 'user'; key: string; prompt_id: string; text: string; origin: 'local' | 'remote'; device_id: string | null; device_name: string | null; at: number }
  | { kind: 'assistant'; key: string; message_id: string; model: string; provider: string; blocks: Block[];
      done: boolean; stop_reason: string | null; error: string | null; usage: Usage | null; at: number
      /** The TUI's one sentence for `error` (#2388); null from an older harness. */
      error_summary: string | null }
  | { kind: 'tool'; key: string; message_id: string | null; tool_call_id: string; name: string; arguments_json: string; args_truncated: boolean;
      startedAt: number | null; endedAt: number | null;
      result: { text: string; is_error: boolean; truncated: boolean; full_bytes: number; refused_line: string | null } | null }
  /** `covered`: the TUI shows nothing for it (completed), or a notice/error line already said it. */
  | { kind: 'run_end'; key: string; run_id: string; outcome: string; error: string | null; covered: boolean; at: number; startedAt: number | null }
  | { kind: 'notice'; key: string; level: 'info' | 'warn' | 'error'; code: string | null; text: string; at: number }
  | { kind: 'retry'; key: string; attempt: number; max_retries: number; delay_ms: number; error: string; at: number }
  | { kind: 'gap'; key: string }
  | { kind: 'security'; key: string; verdict: EventVerdict }

/** All the text of one kind in an assistant message. */
export const blockText = (a: { blocks: Block[] }, kind: Block['kind']) => a.blocks.filter(b => b.kind === kind).map(b => b.text).join('')

/** Token totals from the `message_end` events this page has seen. */
export interface Totals { input: number; output: number; cache_read: number; cache_write: number; cost_usd_micros: number; messages: number }
const noTotals: Totals = { input: 0, output: 0, cache_read: 0, cache_write: 0, cost_usd_micros: 0, messages: 0 }

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
  /** When that run started (harness clock). */
  runStartedAt: number | null
  totals: Totals
  /** When the last worker_status arrived (harness clock). */
  workersAt: number | null
  /** The harness is compacting the context (#2388): since when (harness clock), and whether asked for. */
  compaction: { manual: boolean; since: number } | null
  ended: { reason: string } | null
  /** The harness speaks a newer protocol: stop applying events (5.7 step 3). */
  versionMismatch: boolean
  lastAppliedSeq: number
  /** The seq of the envelope the shown `hello` / `status` came from (0: none yet). A snapshot or
   *  event older than what's shown is ignored, so a relay can't replay a stale status (#2393). */
  helloSeq: number
  statusSeq: number
}

export const initialSessionState: SessionState = {
  hello: null, status: null, items: [], queue: [], workers: [], outgoing: [], online: null,
  connection: 'connecting', running: null, runStartedAt: null, totals: noTotals, workersAt: null, compaction: null, ended: null, versionMismatch: false, lastAppliedSeq: 0,
  helloSeq: 0, statusSeq: 0,
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

function applyChat(s: SessionState, ev: ChatEvent, ts: number): SessionState {
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
  type Assistant = Extract<TranscriptItem, { kind: 'assistant' }>
  const assistant = (message_id: string, model = '', provider = ''): Assistant => ({
    kind: 'assistant', key: nextKey('a'), message_id, model, provider, blocks: [], done: false,
    stop_reason: null, error: null, usage: null, at: ts, error_summary: null,
  })
  /** Closes the open block (a thinking section ends when anything else starts). */
  const closeBlocks = (blocks: Block[]): Block[] => blocks.map(b => b.endedAt == null ? { ...b, endedAt: ts } : b)

  switch (ev.type) {
    case 'user_message':
      return {
        ...s,
        items: [...items, { kind: 'user', key: nextKey('u'), prompt_id: ev.prompt_id, text: ev.text, origin: ev.origin,
          device_id: ev.device_id ?? null, device_name: ev.device_name ?? null, at: ts }],
        // Our own prompt is now in the transcript: drop its outgoing placeholder.
        outgoing: s.outgoing.filter(o => o.prompt_id !== ev.prompt_id),
      }
    case 'run_start':
      return { ...s, running: ev.run_id, runStartedAt: ts }
    case 'message_start':
      return { ...s, items: [...items, assistant(ev.message_id, ev.model, ev.provider)] }
    case 'text_delta':
    case 'thinking_delta': {
      const kind: Block['kind'] = ev.type === 'text_delta' ? 'text' : 'thinking'
      const add = (a: Assistant): Assistant => {
        const last = a.blocks.at(-1)
        if (last && last.kind === kind && last.endedAt == null) {
          return { ...a, blocks: [...a.blocks.slice(0, -1), { ...last, text: last.text + ev.delta }] }
        }
        return { ...a, blocks: [...closeBlocks(a.blocks), { kind, text: ev.delta, startedAt: ts, endedAt: null }] }
      }
      const next = replaceWhere('assistant', a => a.message_id === ev.message_id, add)
      if (next) return { ...s, items: next }
      // A delta whose message_start was before our buffer (a gap): start the block here.
      return { ...s, items: [...items, add(assistant(ev.message_id))] }
    }
    case 'tool_start': {
      // The message's thinking ends when its first call starts.
      const closed = replaceWhere('assistant', a => a.message_id === ev.message_id, a => ({ ...a, blocks: closeBlocks(a.blocks) })) ?? items
      return { ...s, items: [...closed, { kind: 'tool', key: nextKey('t'), message_id: ev.message_id, tool_call_id: ev.tool_call_id, name: ev.name,
        arguments_json: ev.arguments_json, args_truncated: !!ev.truncated, startedAt: ts, endedAt: null, result: null }] }
    }
    case 'tool_end': {
      const result = { text: ev.text, is_error: !!ev.is_error, truncated: !!ev.truncated, full_bytes: ev.full_bytes,
        refused_line: typeof ev.refused_line === 'string' && ev.refused_line ? ev.refused_line : null }
      const next = replaceWhere('tool', t => t.tool_call_id === ev.tool_call_id, t => ({ ...t, result, endedAt: ts }))
      return { ...s, items: next ?? [...items, { kind: 'tool', key: nextKey('t'), message_id: null, tool_call_id: ev.tool_call_id, name: ev.name,
        arguments_json: '', args_truncated: false, startedAt: null, endedAt: ts, result }] }
    }
    case 'message_end': {
      const patch = { done: true, stop_reason: ev.stop_reason, error: ev.error ?? null, usage: ev.usage ?? null, model: ev.model, provider: ev.provider,
        error_summary: typeof ev.error_summary === 'string' && ev.error_summary ? ev.error_summary : null }
      const next = replaceWhere('assistant', a => a.message_id === ev.message_id, a => ({ ...a, ...patch, blocks: closeBlocks(a.blocks) }))
      const u = ev.usage
      const totals = u ? {
        input: s.totals.input + (u.input_tokens || 0), output: s.totals.output + (u.output_tokens || 0),
        cache_read: s.totals.cache_read + (u.cache_read_tokens || 0), cache_write: s.totals.cache_write + (u.cache_write_tokens || 0),
        cost_usd_micros: s.totals.cost_usd_micros + (u.cost_usd_micros || 0), messages: s.totals.messages + 1,
      } : s.totals
      return { ...s, totals, items: next ?? [...items, { ...assistant(ev.message_id), ...patch }] }
    }
    case 'retry':
      return { ...s, items: [...items, { kind: 'retry', key: nextKey('r'), attempt: ev.attempt, max_retries: ev.max_retries, delay_ms: ev.delay_ms, error: ev.error, at: ts }] }
    case 'run_end': {
      // The TUI shows nothing when a run completes; a stopped or failed run ends with the shell's
      // "Stopped." or error line, which arrives as a notice (or the reply's own error line).
      let said = false
      for (let n = items.length - 1; n >= 0; n--) {
        const it = items[n]
        if (it.kind === 'user') break
        if ((it.kind === 'notice' && (it.level !== 'info' || it.text.trim() === 'Stopped.')) || (it.kind === 'assistant' && it.error && it.stop_reason !== 'aborted')) { said = true; break }
      }
      return { ...s, running: null, runStartedAt: null, compaction: null, items: [...items, { kind: 'run_end', key: nextKey('e'), run_id: ev.run_id, outcome: ev.outcome,
        error: ev.error ?? null, covered: ev.outcome === 'completed' || said, at: ts, startedAt: s.runStartedAt }] }
    }
    case 'notice': {
      // The shell's run-outcome line may come just after run_end: then it says it, not run_end.
      const last = items.at(-1)
      const base = last?.kind === 'run_end' && !last.covered && (ev.level !== 'info' || ev.text.trim() === 'Stopped.')
        ? [...items.slice(0, -1), { ...last, covered: true }] : items
      return { ...s, items: [...base, { kind: 'notice', key: nextKey('n'), level: ev.level, code: ev.code ?? null, text: ev.text, at: ts }] }
    }
    case 'queue':
      return { ...s, queue: Array.isArray(ev.items) ? ev.items : [] }
    case 'worker_status':
      return { ...s, workers: Array.isArray(ev.workers) ? ev.workers : [], workersAt: ts }
    case 'compaction':
      return { ...s, compaction: ev.active ? { manual: !!ev.manual, since: ts } : null }
    default:
      return s // unknown types are ignored (6.4)
  }
}

/**
 * A `hello` or `status`, from the stream or a snapshot, with the same freshness rule for both:
 * nothing older than what's shown replaces it (#2381 info, #2393). Snapshots are signed but carry
 * no seq check of their own (5.7), so without this a relay could replay an old, genuine status.
 * A hello's embedded status counts only when it is newer than the status shown.
 */
function applyHelloOrStatus(s: SessionState, env: Envelope): SessionState {
  if (env.op === 'status') {
    if (env.seq < s.statusSeq) return s
    return { ...s, status: env.body as SessionStatus, statusSeq: env.seq }
  }
  if (env.seq < s.helloSeq) return s
  const hello = env.body as HelloBody
  const newer = !!hello.status && env.seq > s.statusSeq
  return { ...s, hello, helloSeq: env.seq, ...(newer ? { status: hello.status!, statusSeq: env.seq } : {}) }
}

function applyEvent(s: SessionState, env: Envelope, myDeviceId: string | null): SessionState {
  const base = { ...s, lastAppliedSeq: env.seq }
  switch (env.op) {
    case 'hello':
    case 'status':
      return applyHelloOrStatus(base, env)
    case 'chat_event':
      return applyChat(base, env.body as ChatEvent, typeof env.ts === 'number' ? env.ts : Date.now())
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
      return { ...base, items: [...base.items, { kind: 'notice', key: nextKey('n'), level: 'warn', code: 'device_paired', at: env.ts,
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
      return a.env.op === 'hello' || a.env.op === 'status' ? applyHelloOrStatus(s, a.env) : s
    case 'relay': {
      const f = a.frame
      if (f.type === 'attached') return { ...s, online: f.online, connection: 'live' }
      if (f.type === 'online') return { ...s, online: true }
      if (f.type === 'offline') return { ...s, online: false, running: null, runStartedAt: null, compaction: null }
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

/** The tokens of the replies this page has seen (the fallback for a harness whose status has no
 *  totals, before #2388). */
export function tokenTotal(t: Totals): number {
  return t.input + t.output + t.cache_read + t.cache_write
}

/** The session's tokens for the status line: the harness's own totals when its status carries
 *  them (#2388), else what this page has seen (which undercounts after a gap). */
export function sessionTokens(status: SessionStatus | null, t: Totals):
  { input: number; output: number; cache_read: number; cache_write: number; total: number; fromHarness: boolean } | null {
  const u = status?.usage
  if (u && typeof u.input_tokens === 'number') {
    const v = { input: u.input_tokens, output: u.output_tokens || 0, cache_read: u.cache_read_tokens || 0, cache_write: u.cache_write_tokens || 0 }
    return { ...v, total: v.input + v.output + v.cache_read + v.cache_write, fromHarness: true }
  }
  const total = tokenTotal(t)
  return total ? { input: t.input, output: t.output, cache_read: t.cache_read, cache_write: t.cache_write, total, fromHarness: false } : null
}

/**
 * People-facing words for end reasons (protocol 12.2). `idleTimeoutS` is the session's idle limit
 * from the harness's hello, when known: the org sets it (5-240 min), so the text never assumes 30.
 */
export function endReasonText(reason: string, idleTimeoutS?: number | null): string {
  const idleMin = idleTimeoutS ? Math.round(idleTimeoutS / 60) : null
  const m: Record<string, string> = {
    harness_ended: 'Remote control was turned off on the laptop.',
    detached: 'You disconnected this session.',
    disconnected: 'You disconnected this session.',
    revoked: 'All your remote sessions were revoked.',
    policy_off: 'Remote control was turned off for your organization.',
    idle: idleMin
      ? `The session ended after ${idleMin} minute${idleMin === 1 ? '' : 's'} without remote activity.`
      : 'The session ended after its idle time without remote activity (your org sets it).',
    expired: 'The session reached its 12-hour limit.',
    killed: 'An admin ended this session.',
  }
  return m[reason] ?? 'This session has ended.'
}

/** Short words for an end reason (protocol 12.2), for the Sessions page's badge; endReasonText is the full sentence. */
export function endReasonLabel(reason: string | null | undefined): string {
  const m: Record<string, string> = {
    harness_ended: 'Turned off on the laptop',
    detached: 'Detached on the laptop',
    disconnected: 'Disconnected',
    revoked: 'Revoked',
    policy_off: 'Turned off by your org',
    idle: 'Idle timeout',
    expired: '12-hour limit',
    killed: 'Ended by an admin',
  }
  return (reason && m[reason]) || 'Ended'
}

/** Integer micro-USD for people: $1.83, or $12 once it's ten dollars or more. */
export function formatCost(micros: number | null | undefined): string {
  return micros == null ? '—' : `$${(micros / 1_000_000).toFixed(micros < 10_000_000 ? 2 : 0)}`
}
