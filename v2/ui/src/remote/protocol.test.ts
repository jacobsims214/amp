/**
 * The shared protocol vectors (v2/testdata/remote-control), byte for byte where
 * the protocol is deterministic: canonical bytes, fingerprints, verification and
 * the pairing derivations. Signing is randomized, so it is tested as a round trip,
 * checked by this code's verifier and by node:crypto independently.
 */
import { describe, expect, it } from 'vitest'
import {
  b64uDecode, b64uEncode, decodeRawPublicKey, decodeSignature, deriveMacKey, deriveSasKey, displayFingerprint,
  envelopeSigningInput, fingerprint, hex, hmacSha256, importVerifyKey, jcs, pairingId, pairRequestSigningInput,
  sasCode, signES256, utf8, verifyEnvelope, verifyES256,
} from './protocol'
import { checkEvent, type EventReceiver } from './envelope'
import { preparePairing } from './pairing'
import { loadVectors } from '../test/vectors'
import { importSigningKey, nodeHmacB64u, nodeVerifyES256, testKey, testKeys } from '../test/signedHarness'

const dec = new TextDecoder()

describe('keys.json and fingerprints.json', () => {
  it.each(testKeys.map(k => [k.label, k] as const))('%s: fingerprint and display fingerprint', async (_l, k) => {
    const raw = decodeRawPublicKey(k.public_raw_b64u)
    expect(await fingerprint(raw)).toBe(k.fingerprint)
    expect(await displayFingerprint(raw)).toBe(k.fingerprint_display)
  })

  const cases = loadVectors<{ cases: { public_raw_b64u: string; sha256_hex: string; fingerprint: string; fingerprint_display: string }[] }>('fingerprints.json').cases
  it.each(cases.map(c => [c.fingerprint, c] as const))('%s', async (_f, c) => {
    const raw = b64uDecode(c.public_raw_b64u)
    expect(hex(new Uint8Array(await crypto.subtle.digest('SHA-256', raw)))).toBe(c.sha256_hex)
    expect(await fingerprint(raw)).toBe(c.fingerprint)
    expect(await displayFingerprint(raw)).toBe(c.fingerprint_display)
  })

  it('refuses compressed or short public keys', () => {
    const raw = b64uDecode(testKeys[0].public_raw_b64u)
    const compressed = new Uint8Array(33); compressed.set(raw.slice(0, 33)); compressed[0] = 0x02
    expect(() => decodeRawPublicKey(b64uEncode(compressed))).toThrow()
    expect(() => decodeRawPublicKey(b64uEncode(raw.slice(0, 64)))).toThrow()
  })
})

describe('jcs.json (protocol 3.3)', () => {
  const file = loadVectors<{ canonical: { name: string; input: string; canonical: string }[]; reject: { name: string; input: string }[] }>('jcs.json')
  it.each(file.canonical.map(c => [c.name, c] as const))('%s', (_n, c) => {
    expect(jcs(JSON.parse(c.input))).toBe(c.canonical)
  })
  // JSON.parse can't see duplicate keys or tell 1e3 from 1000; protocol 3.3 accepts that on the browser.
  it.each(['non-integer number', 'integer outside +-(2^53-1)', 'lone surrogate'])('rejects %s', name => {
    const r = file.reject.find(x => x.name === name)!
    expect(() => jcs(JSON.parse(r.input))).toThrow()
  })
})

describe('strict base64url (protocol 2)', () => {
  it('round-trips and rejects padding, stray characters and non-canonical trailing bits', () => {
    const bytes = Uint8Array.from([0xfb, 0xff, 0x01])
    expect(b64uEncode(bytes)).toBe('-_8B')
    expect(Array.from(b64uDecode('-_8B'))).toEqual([0xfb, 0xff, 0x01])
    expect(() => b64uDecode('AQ==')).toThrow() // padding
    expect(() => b64uDecode('A+8B')).toThrow() // standard alphabet
    expect(() => b64uDecode('AR')).toThrow() // trailing bits set ("AQ" is canonical)
    expect(Array.from(b64uDecode('AQ'))).toEqual([1])
    expect(() => b64uDecode('A')).toThrow()
  })
})

interface EventVector { name: string; wire: string; expect: string; signing_input?: string; receiver: EventReceiver & { now_ms: number } }
const events = loadVectors<{ vectors: EventVector[] }>('events.json').vectors

describe('events.json with the real ES256 verifier (protocol 5.7)', () => {
  it.each(events.map(v => [v.name, v] as const))('%s', async (_n, v) => {
    const r = await checkEvent(v.wire, v.receiver, verifyEnvelope)
    expect(r.verdict).toBe(v.expect)
    if (v.signing_input) expect(dec.decode(envelopeSigningInput(JSON.parse(v.wire)))).toBe(v.signing_input)
  })

  it('never verifies with a key other than the pinned one', async () => {
    const v = events.find(x => x.name === 'hello_valid')!
    const other = testKey('harness-2')
    // A pin whose key doesn't hash to its id is refused even if the id matches.
    expect(await verifyEnvelope(JSON.parse(v.wire), { harness_id: v.receiver.pinned_harness.harness_id, harness_pub: other.public_raw_b64u })).toBe(false)
    expect(await verifyEnvelope(JSON.parse(v.wire), v.receiver.pinned_harness)).toBe(true)
  })
})

interface CommandVector {
  name: string; wire: string; expect: string; signing_input?: string
  receiver: { pinned_devices: { device_id: string; device_pub: string }[] }
}
const commands = loadVectors<{ vectors: CommandVector[] }>('commands.json').vectors

describe('commands.json: canonical bytes and signatures (protocol 3.2, 3.4)', () => {
  // Vectors that get past the signature check (step 9) carry a valid signature; bad_signature ones don't.
  const afterSig = new Set(['ok', 'stale_ts', 'replayed_nonce', 'seq_not_increasing', 'unknown_op', 'invalid_body', 'rate_limited', 'queue_full'])
  const checkable = commands.filter(v => afterSig.has(v.expect) || v.expect === 'bad_signature')

  it('covers the high-S and ASCII-escaped vectors', () => {
    expect(checkable.map(v => v.name)).toEqual(expect.arrayContaining(['prompt_valid_high_s', 'prompt_unicode_ascii_escaped_wire', 'bad_signature_wrong_key']))
  })

  it.each(checkable.map(v => [v.name, v] as const))('%s', async (_n, v) => {
    const env = JSON.parse(v.wire)
    if (v.signing_input) expect(dec.decode(envelopeSigningInput(env))).toBe(v.signing_input)
    const dev = v.receiver.pinned_devices.find(d => d.device_id === env.device_id)!
    const ok = await verifyES256(await importVerifyKey(dev.device_pub), envelopeSigningInput(env), env.sig)
    expect(ok).toBe(v.expect !== 'bad_signature')
  })
})

describe('signatures', () => {
  it('rejects malformed signatures before verifying (protocol 3.4)', () => {
    const n = 'FFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551'
    const sig = (r: string, s: string) => b64uEncode(Uint8Array.from((r + s).match(/../g)!.map(b => parseInt(b, 16))))
    const one = '00'.repeat(31) + '01'
    expect(decodeSignature(sig(one, one))).not.toBeNull()
    expect(decodeSignature(sig('00'.repeat(32), one))).toBeNull() // r = 0
    expect(decodeSignature(sig(one, n))).toBeNull() // s = n
    expect(decodeSignature(b64uEncode(new Uint8Array(63).fill(1)))).toBeNull()
    expect(decodeSignature('not base64!')).toBeNull()
  })

  it('signs commands that this verifier and node:crypto both accept (round trip)', async () => {
    const k = testKey('device-a')
    const priv = await importSigningKey(k)
    const env = {
      v: 1, alg: 'ES256', session_id: 'rs_YIL0d-r7J-AHLPpZ00o2bA', harness_id: testKey('harness-1').fingerprint, device_id: k.fingerprint,
      seq: 1_792_948_800_000, ts: 1_792_948_800_000, nonce: 'dPTjCr8zMqrmOeC3G1VLhA', op: 'prompt',
      body: { text: 'é   🚀 run the tests', client_msg_id: 'm-01' },
    }
    for (let i = 0; i < 8; i++) { // randomized nonces: about half of these are high-S
      const sig = await signES256(priv, envelopeSigningInput(env))
      expect(b64uDecode(sig)).toHaveLength(64)
      expect(await verifyES256(await importVerifyKey(k.public_raw_b64u), envelopeSigningInput({ ...env, sig }), sig)).toBe(true)
      expect(nodeVerifyES256(k.public_raw_b64u, envelopeSigningInput(env), sig)).toBe(true)
      // A changed body fails.
      expect(nodeVerifyES256(k.public_raw_b64u, envelopeSigningInput({ ...env, seq: env.seq + 1 }), sig)).toBe(false)
    }
  })
})

describe('pairing.json (protocol 7.2)', () => {
  const p = loadVectors<{
    inputs: { pairing_secret_b64u: string; session_id: string; harness_pub_b64u: string; harness_id: string; device_pub_b64u: string; device_id: string }
    link: string
    derived: { pairing_id: string; k_mac_hex: string; k_sas_hex: string; sas_code: string }
    request: Record<string, unknown> & { device_name: string; ts: number; nonce: string }
    signing_input: string
    message: { request: Record<string, unknown>; mac: string; sig: string }
  }>('pairing.json')
  const secret = b64uDecode(p.inputs.pairing_secret_b64u)

  it('derives pairing_id, k_mac, k_sas and the code exactly', async () => {
    expect(await pairingId(secret)).toBe(p.derived.pairing_id)
    const kMac = await deriveMacKey(secret, p.inputs.session_id)
    const kSas = await deriveSasKey(secret, p.inputs.session_id)
    expect(hex(kMac)).toBe(p.derived.k_mac_hex)
    expect(hex(kSas)).toBe(p.derived.k_sas_hex)
    expect(await sasCode(kSas, b64uDecode(p.inputs.harness_pub_b64u), b64uDecode(p.inputs.device_pub_b64u))).toBe(p.derived.sas_code)
  })

  it('reproduces the signing input and MAC, and verifies the vector signature', async () => {
    const input = pairRequestSigningInput(p.request)
    expect(dec.decode(input)).toBe(p.signing_input)
    const kMac = await deriveMacKey(secret, p.inputs.session_id)
    expect(b64uEncode(await hmacSha256(kMac, input))).toBe(p.message.mac)
    expect(await verifyES256(await importVerifyKey(p.inputs.device_pub_b64u), input, p.message.sig)).toBe(true)
  })

  it('builds the same request, MAC and code from the link, with a signature the harness accepts', async () => {
    const device = testKey('device-a')
    expect(device.fingerprint).toBe(p.inputs.device_id)
    const priv = await importSigningKey(device)
    const out = await preparePairing({
      secret, session_id: p.inputs.session_id, harness_id: p.inputs.harness_id, harness_pub: p.inputs.harness_pub_b64u,
      device: { device_id: p.inputs.device_id, device_pub: p.inputs.device_pub_b64u },
      device_name: p.request.device_name, ts: p.request.ts, nonce: p.request.nonce,
      sign: r => signES256(priv, pairRequestSigningInput(r)),
    })
    expect(out.pairing_id).toBe(p.derived.pairing_id)
    expect(out.code).toBe(p.derived.sas_code)
    expect(out.message.request).toEqual(p.message.request)
    expect(jcs(out.message.request)).toBe(jcs(p.message.request))
    expect(out.message.mac).toBe(p.message.mac)
    // The harness's view, computed independently with node:crypto.
    const input = utf8(p.signing_input)
    expect(nodeHmacB64u(await deriveMacKey(secret, p.inputs.session_id), input)).toBe(out.message.mac)
    expect(nodeVerifyES256(p.inputs.device_pub_b64u, input, out.message.sig)).toBe(true)
    expect(Object.keys(out.message.request)).toHaveLength(10)
  })

  it('parses the vector link the way the page does', () => {
    const url = new URL(p.link)
    const params = new URLSearchParams(url.hash.slice(1))
    expect(params.get('p')).toBe(p.inputs.pairing_secret_b64u)
    expect(params.get('h')).toBe(p.inputs.harness_id)
  })
})
