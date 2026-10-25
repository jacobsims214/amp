/**
 * Remote control protocol v1 types (v2/docs/remote-control/protocol.md).
 * Every number is an integer: money is `cost_usd_micros`, tool arguments travel
 * as `arguments_json` text. Event bodies may grow new members (ignore them);
 * command bodies are exact.
 */

/** The 11-member envelope (protocol 4). */
export interface Envelope<B = unknown> {
  v: number
  alg: string
  session_id: string
  harness_id: string
  device_id: string | null
  seq: number
  ts: number
  nonce: string
  op: string
  body: B
  sig: string
}

export type UnsignedEnvelope<B = unknown> = Omit<Envelope<B>, 'sig'>

// ── Commands (browser → harness, protocol 5.1) ──────────────────────────────

export type CommandOp = 'prompt' | 'stop' | 'detach'
export interface PromptBody { text: string; client_msg_id: string }
export type CommandBody = PromptBody | Record<string, never>
export type UnsignedCommand = UnsignedEnvelope<CommandBody> & { device_id: string }
export type CommandEnvelope = Envelope<CommandBody> & { device_id: string }

// ── Events (harness → browser, protocol 6) ──────────────────────────────────

export interface ProjectRef { id: number; name: string }
export interface DeviceRef { device_id: string; name: string }

export interface SessionStatus {
  model: string | null
  busy: boolean
  context_pct: number | null
  cost_usd_micros: number | null
  project: ProjectRef | null
  ticket_id: number | null
  /** The current ticket's name (#2389). Optional: a harness that predates it omits it. */
  ticket_name?: string | null
  branch: string | null
  queue_len: number
  workers_running: number
  devices: DeviceRef[]
}

export interface HelloBody {
  harness: { harness_id: string; harness_pub: string; host: string; os: string; version: string }
  user_id: number
  ops: string[]
  started_at_ms: number
  expires_at_ms: number
  idle_timeout_s: number
  status: SessionStatus
}

export interface CommandRef { device_id: string; seq: number }

export interface AckBody {
  ref: CommandRef
  op: string
  result: 'started' | 'queued' | 'done'
  prompt_id: string | null
  queue_position: number | null
}

export interface RefusedBody {
  ref: CommandRef | null
  op: string | null
  code: string
  message: string
  last_seq: number | null
}

export interface PairResultBody {
  pairing_id: string
  result: 'accepted' | 'rejected' | 'failed' | 'expired' | 'locked'
  code: string | null
  tries_left: number | null
  pinned_until_ms: number | null
}

export interface ByeBody { reason: string }

export interface Usage {
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost_usd_micros: number
}

export interface QueueItem { prompt_id: string; origin: 'local' | 'remote'; device_id: string | null; preview: string }
export type WorkerState = 'running' | 'done' | 'failed' | 'stopped' | 'blocked'
export interface WorkerInfo { task_id: number; name: string; role: string; state: WorkerState; model: string | null; started_at_ms: number | null }

/** ChatEvent (protocol 6.4), selected by `type`. */
export type ChatEvent =
  | { type: 'user_message'; prompt_id: string; text: string; origin: 'local' | 'remote'; device_id: string | null; device_name: string | null }
  | { type: 'run_start'; run_id: string; prompt_id: string }
  | { type: 'message_start'; message_id: string; model: string; provider: string }
  | { type: 'text_delta'; message_id: string; delta: string }
  | { type: 'thinking_delta'; message_id: string; delta: string }
  | { type: 'tool_start'; message_id: string; tool_call_id: string; name: string; arguments_json: string; truncated: boolean }
  | { type: 'tool_end'; tool_call_id: string; name: string; is_error: boolean; text: string; truncated: boolean; full_bytes: number }
  | { type: 'message_end'; message_id: string; stop_reason: string; error: string | null; model: string; provider: string; usage: Usage }
  | { type: 'retry'; attempt: number; max_retries: number; delay_ms: number; error: string }
  | { type: 'run_end'; run_id: string; outcome: 'completed' | 'aborted' | 'failed' | 'suspended'; error: string | null }
  | { type: 'notice'; level: 'info' | 'warn' | 'error'; code: string | null; text: string }
  | { type: 'queue'; items: QueueItem[] }
  | { type: 'worker_status'; workers: WorkerInfo[] }

export const CHAT_EVENT_TYPES = [
  'user_message', 'run_start', 'message_start', 'text_delta', 'thinking_delta', 'tool_start', 'tool_end',
  'message_end', 'retry', 'run_end', 'notice', 'queue', 'worker_status',
] as const

// ── Relay frames (unsigned status from AMP, protocol 8.3) ───────────────────

export type RelayFrame =
  | { type: 'attached'; online: boolean; oldest_seq: number | null; latest_seq: number | null }
  | { type: 'gap'; requested_seq: number; oldest_seq: number }
  | { type: 'online' }
  | { type: 'offline' }
  | { type: 'ended'; reason: string }

// ── HTTP (browser leg, protocol 8.3) ────────────────────────────────────────

export interface SessionSummary {
  session_id: string
  harness_id: string
  harness_fp_display: string
  host: string
  os: string
  harness_version: string
  project: ProjectRef | null
  online: boolean
  viewers: number
  started_at_ms: number
  expires_at_ms: number
  last_activity_at_ms: number
  protocol_version: number
  // Added by #2389 (protocol 8.3). Optional, because a relay that predates them leaves them out.
  /** `active`, or `ended` for a session that ended in the last 7 days. */
  state?: 'active' | 'ended'
  /** Why it ended (protocol 12.2), for an ended session. */
  end_reason?: string | null
  ended_at_ms?: number | null
  /** The last model the harness's status named. */
  model?: string | null
  /** The current ticket from the harness's status; `name` is null when neither the harness nor AMP has it. */
  ticket?: { id: number; name: string | null } | null
  /** Browsers paired with this harness install that are neither revoked nor expired. */
  devices?: SessionDevice[]
}

/** A browser paired with a harness, as GET /api/sessions lists it (#2389). */
export interface SessionDevice { device_id: string; name: string; paired_at_ms: number; expires_at_ms: number }

export interface SessionDetail extends SessionSummary { harness_pub: string }

/** What this browser holds for a paired harness (protocol 7.7). */
export interface HarnessPin { harness_id: string; harness_pub: string; host?: string; pinned_until_ms: number }

/** Browser-side event verdicts (protocol 9.4). */
export type EventVerdict =
  | 'ok' | 'duplicate' | 'ignored_unknown_op' | 'too_large' | 'malformed' | 'unsupported_version'
  | 'unsupported_alg' | 'wrong_session' | 'wrong_harness' | 'bad_signature'
