import { describe, expect, it, vi } from 'vitest'
import { buildCommand, checkEvent, nextSeq, parseEnvelope, promptProblem, type EventReceiver } from './envelope'
import type { Envelope, HarnessPin } from './types'
import { loadVectors } from '../test/vectors'

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const load = (f: string) => loadVectors<any>(f)

interface EventVector { name: string; wire: string; expect: string; receiver: EventReceiver & { now_ms: number } }
const vectors: EventVector[] = load('events.json').vectors

/**
 * The real ES256 verifier is #2375's (crypto.ts hook). Until it lands, these
 * tests stand in a verifier that answers from the vector's expectation, and
 * check everything around it: the order of the checks (5.7), that only the
 * pinned key is used, and what happens on each verdict. #2375 swaps in the
 * real verifier here and drops the stand-in.
 */
const standInVerifier = (v: EventVector) => vi.fn(async (...args: [Envelope, HarnessPin]) => args.length === 2 && v.expect !== 'bad_signature')

describe('event vectors (protocol 5.7)', () => {
  for (const v of vectors) {
    it(`${v.name} → ${v.expect}`, async () => {
      const verify = standInVerifier(v)
      const r = await checkEvent(v.wire, v.receiver, verify)
      expect(r.verdict).toBe(v.expect)
      // Steps 1-6 come before the signature check: those failures never reach the verifier.
      const beforeSig = ['too_large', 'malformed', 'unsupported_version', 'unsupported_alg', 'wrong_session', 'wrong_harness']
      if (beforeSig.includes(v.expect)) expect(verify).not.toHaveBeenCalled()
      else {
        expect(verify).toHaveBeenCalledTimes(1)
        // Verified with the pinned key only, never one from the envelope or the relay.
        expect(verify.mock.calls[0][1]).toBe(v.receiver.pinned_harness)
      }
    })
  }

  it('a failed verification is bad_signature even when the vector is otherwise fine', async () => {
    const v = vectors.find(x => x.name === 'hello_valid')!
    expect((await checkEvent(v.wire, v.receiver, async () => false)).verdict).toBe('bad_signature')
    expect((await checkEvent(v.wire, v.receiver, async () => { throw new Error('boom') })).verdict).toBe('bad_signature')
  })

  it('snapshots stop after the signature check and ignore seq', async () => {
    const v = vectors.find(x => x.name === 'duplicate_seq_dropped')!
    const r = await checkEvent(v.wire, v.receiver, async () => true, { snapshot: true })
    expect(r.verdict).toBe('ok')
  })
})

describe('I-JSON profile (protocol 3.3, browser side)', () => {
  const rejects: { name: string; input: string }[] = load('jcs.json').reject
  const wrap = (body: string) => `{"v":1,"alg":"ES256","session_id":"s","harness_id":"h","device_id":null,"seq":1,"ts":1,"nonce":"n","op":"status","body":${body},"sig":"x"}`

  // JSON.parse can't see duplicate keys or tell 1e3 from 1000; protocol 3.3 accepts that on the browser.
  for (const name of ['non-integer number', 'integer outside +-(2^53-1)', 'lone surrogate']) {
    it(`rejects ${name}`, () => {
      const r = rejects.find(x => x.name === name)!
      expect(parseEnvelope(wrap(r.input))).toBeNull()
    })
  }

  it('rejects a missing or extra envelope member', () => {
    expect(parseEnvelope(wrap('{}'))).not.toBeNull()
    expect(parseEnvelope(wrap('{}').replace('"sig":"x"', '"sig":"x","extra":1'))).toBeNull()
    expect(parseEnvelope(wrap('{}').replace(',"sig":"x"', ''))).toBeNull()
    expect(parseEnvelope('[]')).toBeNull()
    expect(parseEnvelope('not json')).toBeNull()
  })

  it('reports oversize events as too_large before parsing', async () => {
    const rx = vectors[0].receiver
    expect((await checkEvent('x'.repeat(1_048_577), rx, async () => true)).verdict).toBe('too_large')
  })
})

describe('commands', () => {
  it('seq follows max(last_sent + 1, now) (5.3)', () => {
    expect(nextSeq(0, 1_792_948_800_000)).toBe(1_792_948_800_000)
    expect(nextSeq(1_792_948_800_000, 1_792_948_800_000)).toBe(1_792_948_800_001)
    expect(nextSeq(1_792_948_900_000, 1_792_948_800_000)).toBe(1_792_948_900_001)
  })

  it('builds the 10 unsigned members with a fresh 16-byte nonce', () => {
    const cmd = buildCommand({ session_id: 'rs_x', harness_id: 'h', device_id: 'd', seq: 5, ts: 6, op: 'prompt', body: { text: 'hi', client_msg_id: 'm-1' } })
    expect(Object.keys(cmd).sort()).toEqual(['alg', 'body', 'device_id', 'harness_id', 'nonce', 'op', 'seq', 'session_id', 'ts', 'v'])
    expect(cmd).toMatchObject({ v: 1, alg: 'ES256', op: 'prompt', seq: 5, ts: 6 })
    expect(cmd.nonce).toMatch(/^[A-Za-z0-9_-]{22}$/)
    expect(buildCommand({ session_id: 's', harness_id: 'h', device_id: 'd', seq: 1, ts: 1, op: 'stop', body: {} }).nonce).not.toBe(cmd.nonce)
  })

  it('checks prompt text before sending (5.1)', () => {
    expect(promptProblem('   \n')).toMatch(/Type a message/)
    expect(promptProblem('é'.repeat(24_577))).toMatch(/too long/) // 49 154 bytes
    expect(promptProblem('é'.repeat(24_576))).toBeNull() // exactly 49 152 bytes
    expect(promptProblem('/quit')).toBeNull() // literal text; the harness never treats it as a command
  })
})
