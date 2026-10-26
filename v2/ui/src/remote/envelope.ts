/**
 * The browser's event check (protocol 5.7) and command building (5.3).
 * Signature verification and signing are delegated to RemoteCrypto (crypto.ts,
 * implemented on WebCrypto in webCrypto.ts / protocol.ts).
 */
import type { RemoteCrypto } from './crypto'
import { b64uEncode } from './protocol'
import type { CommandBody, CommandOp, Envelope, EventVerdict, HarnessPin, UnsignedCommand } from './types'

export const MAX_EVENT_BYTES = 1_048_576
export const MAX_PROMPT_BYTES = 49_152
export const ENVELOPE_KEYS = ['v', 'alg', 'session_id', 'harness_id', 'device_id', 'seq', 'ts', 'nonce', 'op', 'body', 'sig'] as const
/** Event ops a v1 browser applies; anything else is verified and skipped (5.7 step 9). */
export const KNOWN_EVENT_OPS = new Set(['hello', 'status', 'chat_event', 'ack', 'refused', 'pair_result', 'bye'])

const encoder = new TextEncoder()
export const utf8Length = (s: string) => encoder.encode(s).length

/** The receiver state the check needs: the shape of `receiver` in events.json. */
export interface EventReceiver {
  session_id: string
  pinned_harness: HarnessPin
  last_applied_seq: number
}

export type EventCheck = { verdict: EventVerdict; env?: Envelope }

const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/

/** Protocol 3.3 as far as the browser can see after JSON.parse: integers only, safe range, no lone surrogates. */
function profileOk(value: unknown): boolean {
  if (typeof value === 'number') return Number.isSafeInteger(value)
  if (typeof value === 'string') return !LONE_SURROGATE.test(value)
  if (value === null || typeof value === 'boolean') return true
  if (Array.isArray(value)) return value.every(profileOk)
  if (typeof value === 'object') {
    return Object.entries(value as Record<string, unknown>).every(([k, v]) => !LONE_SURROGATE.test(k) && profileOk(v))
  }
  return false
}

function isObject(x: unknown): x is Record<string, unknown> {
  return typeof x === 'object' && x !== null && !Array.isArray(x)
}

/** Parses `wire` and checks the envelope shape (5.7 step 2). Returns null if malformed. */
export function parseEnvelope(wire: string): Envelope | null {
  let parsed: unknown
  try { parsed = JSON.parse(wire) } catch { return null }
  if (!isObject(parsed)) return null
  const keys = Object.keys(parsed)
  if (keys.length !== ENVELOPE_KEYS.length || !ENVELOPE_KEYS.every(k => k in parsed)) return null
  const e = parsed
  const int = (x: unknown) => typeof x === 'number' && Number.isSafeInteger(x)
  const str = (x: unknown) => typeof x === 'string'
  if (!int(e.v) || !str(e.alg) || !str(e.session_id) || !str(e.harness_id) || !(e.device_id === null || str(e.device_id))
    || !int(e.seq) || (e.seq as number) < 1 || !int(e.ts) || !str(e.nonce) || !str(e.op) || !isObject(e.body) || !str(e.sig)) return null
  if (!profileOk(parsed)) return null
  return parsed as unknown as Envelope
}

/**
 * Runs the browser event check in the normative order of protocol 5.7.
 * `snapshot` frames stop after step 7 (no seq handling).
 */
export async function checkEvent(
  wire: string,
  rx: EventReceiver,
  verify: RemoteCrypto['verifyEvent'],
  opts: { snapshot?: boolean } = {},
): Promise<EventCheck> {
  if (utf8Length(wire) > MAX_EVENT_BYTES) return { verdict: 'too_large' }
  const env = parseEnvelope(wire)
  if (!env) return { verdict: 'malformed' }
  if (env.v !== 1) return { verdict: 'unsupported_version' }
  if (env.alg !== 'ES256') return { verdict: 'unsupported_alg' }
  if (env.session_id !== rx.session_id) return { verdict: 'wrong_session' }
  if (env.harness_id !== rx.pinned_harness.harness_id) return { verdict: 'wrong_harness' }
  let good = false
  try { good = await verify(env, rx.pinned_harness) } catch { good = false }
  if (!good) return { verdict: 'bad_signature' }
  if (opts.snapshot) return { verdict: 'ok', env }
  if (env.seq <= rx.last_applied_seq) return { verdict: 'duplicate' }
  if (!KNOWN_EVENT_OPS.has(env.op)) return { verdict: 'ignored_unknown_op', env }
  return { verdict: 'ok', env }
}

/** Verdicts that mean the relay sent something untrusted (5.7: steps 2-7 are security events). */
export function isSecurityVerdict(v: EventVerdict): boolean {
  return v === 'too_large' || v === 'malformed' || v === 'unsupported_alg' || v === 'wrong_session'
    || v === 'wrong_harness' || v === 'bad_signature'
}

// ── Commands ────────────────────────────────────────────────────────────────

/** Browser seq rule (5.3): clock-based so it survives reloads without storage. */
export function nextSeq(lastSent: number, now: number): number {
  return Math.max(lastSent + 1, now)
}

export function newNonce(): string {
  return b64uEncode(crypto.getRandomValues(new Uint8Array(16)))
}

/** A browser-chosen id matching `^[A-Za-z0-9_-]{1,64}$`. */
export function newClientMsgId(): string {
  return `m-${b64uEncode(crypto.getRandomValues(new Uint8Array(9)))}`
}

export function buildCommand(args: {
  session_id: string; harness_id: string; device_id: string; seq: number; ts: number; op: CommandOp; body: CommandBody; nonce?: string
}): UnsignedCommand {
  return {
    v: 1, alg: 'ES256',
    session_id: args.session_id, harness_id: args.harness_id, device_id: args.device_id,
    seq: args.seq, ts: args.ts, nonce: args.nonce ?? newNonce(),
    op: args.op, body: args.body,
  }
}

/** Why a prompt can't be sent, or null if it can (5.1: 1..49152 bytes, not whitespace-only). */
export function promptProblem(text: string): string | null {
  if (!text.trim()) return 'Type a message first.'
  if (utf8Length(text) > MAX_PROMPT_BYTES) return 'That message is too long (48 KiB at most).'
  return null
}
