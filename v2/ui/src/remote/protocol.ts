/**
 * The cryptographic core of remote control protocol v1 (v2/docs/remote-control/protocol.md
 * sections 2, 3 and 7.2), on WebCrypto only. Pure functions: no storage, no network.
 *
 * - b64u: base64url without padding; decoding is strict (no padding, canonical trailing bits).
 * - JCS: RFC 8785 under the protocol's I-JSON profile (integers only, no lone surrogates).
 * - Fingerprints: b64u(SHA-256(65-byte raw public key)).
 * - ES256: ECDSA P-256 / SHA-256, 64-byte IEEE P1363 r‖s signatures. High-S is accepted.
 * - Pairing: pairing_id, HKDF-derived k_mac / k_sas, the request MAC and the 8-digit SAS code.
 *
 * Every function that takes or returns key material works on public keys or on
 * non-extractable CryptoKeys; nothing here logs.
 */

const enc = new TextEncoder()
const subtle = () => globalThis.crypto.subtle

export const ENVELOPE_PREFIX = 'amp-rc/1 envelope\n'
export const PAIR_REQUEST_PREFIX = 'amp-rc/1 pair-request\n'
const PAIRING_ID_PREFIX = 'amp-rc/1 pairing-id\n'
const ECDSA = { name: 'ECDSA', namedCurve: 'P-256' } as const
const ES256 = { name: 'ECDSA', hash: 'SHA-256' } as const

/** Thrown for input that breaks the protocol's encoding rules (the receiver's `malformed`). */
export class ProtocolError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'ProtocolError'
  }
}

// ── Bytes ───────────────────────────────────────────────────────────────────

/** Copies into a fresh ArrayBuffer-backed array, so WebCrypto accepts it in every realm (jsdom included). */
function buf(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(bytes.length)
  out.set(bytes)
  return out
}

export function utf8(s: string): Uint8Array<ArrayBuffer> {
  return buf(enc.encode(s))
}

export function concat(...parts: Uint8Array[]): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let i = 0
  for (const p of parts) { out.set(p, i); i += p.length }
  return out
}

export function b64uEncode(bytes: Uint8Array): string {
  let s = ''
  for (const b of bytes) s += String.fromCharCode(b)
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

const B64U = /^[A-Za-z0-9_-]*$/

/** Strict base64url decoding (protocol 2): no padding, no stray characters, canonical trailing bits. */
export function b64uDecode(s: string): Uint8Array<ArrayBuffer> {
  if (typeof s !== 'string' || !B64U.test(s) || s.length % 4 === 1) throw new ProtocolError('not base64url')
  const std = s.replace(/-/g, '+').replace(/_/g, '/')
  const bin = atob(std + '='.repeat((4 - (std.length % 4)) % 4))
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  if (b64uEncode(out) !== s) throw new ProtocolError('non-canonical base64url')
  return out
}

export function hex(bytes: Uint8Array): string {
  return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('')
}

/** Constant-time comparison for equal-length byte strings. */
export function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false
  let d = 0
  for (let i = 0; i < a.length; i++) d |= a[i] ^ b[i]
  return d === 0
}

export async function sha256(bytes: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
  return new Uint8Array(await subtle().digest('SHA-256', buf(bytes)))
}

// ── JCS (protocol 3.3) ──────────────────────────────────────────────────────

const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/

function jcsString(s: string): string {
  if (LONE_SURROGATE.test(s)) throw new ProtocolError('lone surrogate')
  // ECMAScript's JSON.stringify escapes exactly as JCS requires (lower-case \u00XX, raw U+2028/9).
  return JSON.stringify(s)
}

/**
 * RFC 8785 canonical JSON of a parsed value. Object members are sorted by UTF-16
 * code units (the default `sort()`), numbers must be safe integers.
 */
export function jcs(value: unknown): string {
  if (value === null) return 'null'
  switch (typeof value) {
    case 'boolean': return value ? 'true' : 'false'
    case 'number':
      if (!Number.isSafeInteger(value)) throw new ProtocolError('numbers must be safe integers')
      return String(value)
    case 'string': return jcsString(value)
    case 'object': {
      if (Array.isArray(value)) return `[${value.map(jcs).join(',')}]`
      const o = value as Record<string, unknown>
      return `{${Object.keys(o).sort().map(k => `${jcsString(k)}:${jcs(o[k])}`).join(',')}}`
    }
    default: throw new ProtocolError(`not JSON: ${typeof value}`)
  }
}

/** "amp-rc/1 envelope\n" ‖ JCS(envelope without `sig`) (protocol 3.2). */
export function envelopeSigningInput(env: object): Uint8Array<ArrayBuffer> {
  const rest: Record<string, unknown> = { ...(env as Record<string, unknown>) }
  delete rest.sig
  return utf8(ENVELOPE_PREFIX + jcs(rest))
}

export function pairRequestSigningInput(request: object): Uint8Array<ArrayBuffer> {
  return utf8(PAIR_REQUEST_PREFIX + jcs(request))
}

// ── Keys and fingerprints (protocol 2) ──────────────────────────────────────

/** b64u(SHA-256(raw public key)): `harness_id` and `device_id`. */
export async function fingerprint(rawPub: Uint8Array): Promise<string> {
  return b64uEncode(await sha256(rawPub))
}

/** First 8 bytes of SHA-256(raw pub), upper-case hex in groups of 4 ("29CA-E6B5-00C2-66CB"). For people only. */
export async function displayFingerprint(rawPub: Uint8Array): Promise<string> {
  const h = hex((await sha256(rawPub)).slice(0, 8)).toUpperCase()
  return h.match(/.{4}/g)!.join('-')
}

/** Decodes a 65-byte uncompressed SEC1 point; compressed or odd-length keys are refused. */
export function decodeRawPublicKey(b64u: string): Uint8Array<ArrayBuffer> {
  const raw = b64uDecode(b64u)
  if (raw.length !== 65 || raw[0] !== 0x04) throw new ProtocolError('public key must be a 65-byte uncompressed P-256 point')
  return raw
}

export async function importVerifyKey(rawPubB64u: string): Promise<CryptoKey> {
  return subtle().importKey('raw', decodeRawPublicKey(rawPubB64u), ECDSA, true, ['verify'])
}

/** True when `fp(pub) == id`: keys are self-certifying, so check this wherever a key is learned. */
export async function keyMatchesId(rawPubB64u: string, id: string): Promise<boolean> {
  try {
    return (await fingerprint(decodeRawPublicKey(rawPubB64u))) === id
  } catch {
    return false
  }
}

/** A fresh non-extractable device key pair (protocol 3.1). The public half is always exportable. */
export async function generateDeviceKey(): Promise<CryptoKeyPair> {
  return subtle().generateKey(ECDSA, false, ['sign', 'verify']) as Promise<CryptoKeyPair>
}

export async function exportRawPublicKey(key: CryptoKey): Promise<Uint8Array<ArrayBuffer>> {
  return new Uint8Array(await subtle().exportKey('raw', key))
}

// ── ES256 (protocol 3.1, 3.4) ───────────────────────────────────────────────

const P256_N = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551n

function toBigInt(bytes: Uint8Array): bigint {
  return bytes.reduce((n, b) => (n << 8n) | BigInt(b), 0n)
}

/** Decodes a P1363 signature; null unless it is exactly 64 bytes with r and s in [1, n−1]. */
export function decodeSignature(sigB64u: string): Uint8Array<ArrayBuffer> | null {
  let sig: Uint8Array<ArrayBuffer>
  try { sig = b64uDecode(sigB64u) } catch { return null }
  if (sig.length !== 64) return null
  const r = toBigInt(sig.subarray(0, 32))
  const s = toBigInt(sig.subarray(32))
  if (r < 1n || r >= P256_N || s < 1n || s >= P256_N) return null
  return sig
}

export async function signES256(privateKey: CryptoKey, input: Uint8Array): Promise<string> {
  return b64uEncode(new Uint8Array(await subtle().sign(ES256, privateKey, buf(input))))
}

/** Verifies an ES256 signature. Never requires low-S (protocol 3.4). False on any decoding problem. */
export async function verifyES256(publicKey: CryptoKey, input: Uint8Array, sigB64u: string): Promise<boolean> {
  const sig = decodeSignature(sigB64u)
  if (!sig) return false
  try {
    return await subtle().verify(ES256, publicKey, sig, buf(input))
  } catch {
    return false
  }
}

/**
 * Verifies a harness event (protocol 5.7 step 7) with a pinned key. The pin's key must
 * hash to the pin's id and to the envelope's `harness_id`, so a key from anywhere else
 * can't stand in for it.
 */
export async function verifyEnvelope(env: { harness_id: string; sig: string }, pin: { harness_id: string; harness_pub: string }): Promise<boolean> {
  if (env.harness_id !== pin.harness_id) return false
  if (!(await keyMatchesId(pin.harness_pub, pin.harness_id))) return false
  let input: Uint8Array
  try { input = envelopeSigningInput(env) } catch { return false }
  return verifyES256(await importVerifyKey(pin.harness_pub), input, env.sig)
}

// ── Pairing (protocol 7.2) ──────────────────────────────────────────────────

export const PAIRING_SECRET_BYTES = 32

export function decodePairingSecret(p: string): Uint8Array<ArrayBuffer> {
  const secret = b64uDecode(p)
  if (secret.length !== PAIRING_SECRET_BYTES) throw new ProtocolError('pairing secret must be 32 bytes')
  return secret
}

/** b64u(SHA-256("amp-rc/1 pairing-id\n" ‖ secret)[0..16]). */
export async function pairingId(secret: Uint8Array): Promise<string> {
  return b64uEncode((await sha256(concat(utf8(PAIRING_ID_PREFIX), secret))).slice(0, 16))
}

async function hkdf(secret: Uint8Array, sessionId: string, info: string): Promise<Uint8Array<ArrayBuffer>> {
  const ikm = await subtle().importKey('raw', buf(secret), 'HKDF', false, ['deriveBits'])
  const bits = await subtle().deriveBits({ name: 'HKDF', hash: 'SHA-256', salt: utf8(sessionId), info: utf8(info) }, ikm, 256)
  return new Uint8Array(bits)
}

export const deriveMacKey = (secret: Uint8Array, sessionId: string) => hkdf(secret, sessionId, 'amp-rc/1 pair-mac')
export const deriveSasKey = (secret: Uint8Array, sessionId: string) => hkdf(secret, sessionId, 'amp-rc/1 sas')

export async function hmacSha256(key: Uint8Array, data: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
  const k = await subtle().importKey('raw', buf(key), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'])
  return new Uint8Array(await subtle().sign('HMAC', k, buf(data)))
}

/** The 8-digit short authentication string, shown "NNNN-NNNN" on both ends. */
export async function sasCode(kSas: Uint8Array, harnessPubRaw: Uint8Array, devicePubRaw: Uint8Array): Promise<string> {
  const sas = await hmacSha256(kSas, concat(harnessPubRaw, devicePubRaw))
  const n = new DataView(sas.buffer, sas.byteOffset, 8).getBigUint64(0, false) % 100_000_000n
  const digits = n.toString().padStart(8, '0')
  return `${digits.slice(0, 4)}-${digits.slice(4)}`
}

/** The 10-member pair request (protocol 7.3). */
export interface PairRequest {
  type: 'pair_request'
  v: 1
  session_id: string
  harness_id: string
  pairing_id: string
  device_id: string
  device_pub: string
  device_name: string
  ts: number
  nonce: string
}

export interface PairMessage { request: PairRequest; mac: string; sig: string }
