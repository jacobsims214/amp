/**
 * Test helpers that use the shared vectors' test-only keys (keys.json) to act
 * as a real harness: ES256-signed events, and the harness's pairing checks
 * (protocol 7.5) done independently of the browser code under test, with
 * node:crypto rather than WebCrypto where it matters.
 */
import { createHmac, createPublicKey, verify as nodeVerify } from 'node:crypto'
import { b64uDecode, b64uEncode, envelopeSigningInput, jcs, signES256 } from '../remote/protocol'
import { loadVectors } from './vectors'

export interface TestKey {
  label: string
  private_scalar_hex: string
  public_raw_b64u: string
  fingerprint: string
  fingerprint_display: string
}

export const testKeys = loadVectors<{ keys: TestKey[] }>('keys.json').keys
export const testKey = (label: string) => testKeys.find(k => k.label === label)!

const fromHex = (h: string) => Uint8Array.from(h.match(/../g)!.map(b => parseInt(b, 16)))

/** The JWK for a keys.json key (private when `withPrivate`). */
export function jwkFor(k: TestKey, withPrivate: boolean): JsonWebKey {
  const raw = b64uDecode(k.public_raw_b64u)
  const jwk: JsonWebKey = { kty: 'EC', crv: 'P-256', x: b64uEncode(raw.slice(1, 33)), y: b64uEncode(raw.slice(33)), ext: false }
  if (withPrivate) jwk.d = b64uEncode(fromHex(k.private_scalar_hex))
  return jwk
}

/** Imports a keys.json private key into WebCrypto, non-extractable, for signing. */
export async function importSigningKey(k: TestKey): Promise<CryptoKey> {
  return crypto.subtle.importKey('jwk', jwkFor(k, true), { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign'])
}

/** An independent verifier (node:crypto, not WebCrypto): ES256 with P1363 signatures. */
export function nodeVerifyES256(pubRawB64u: string, input: Uint8Array, sigB64u: string): boolean {
  const key = createPublicKey({ key: jwkFor({ public_raw_b64u: pubRawB64u } as TestKey, false) as import('node:crypto').JsonWebKey, format: 'jwk' })
  return nodeVerify('sha256', input, { key, dsaEncoding: 'ieee-p1363' }, b64uDecode(sigB64u))
}

export function nodeHmacB64u(key: Uint8Array, data: Uint8Array): string {
  return b64uEncode(new Uint8Array(createHmac('sha256', key).update(data).digest()))
}

/** A fake harness that signs its events with a keys.json key (`harness-1` unless told otherwise). */
export class SignedHarness {
  readonly key: TestKey
  private seq = 0
  private signer: Promise<CryptoKey>
  readonly sessionId: string
  constructor(sessionId: string, label = 'harness-1') {
    this.sessionId = sessionId
    this.key = testKey(label)
    this.signer = importSigningKey(this.key)
  }

  async event(op: string, body: unknown, device_id: string | null = null, overrides: Record<string, unknown> = {}): Promise<string> {
    const env: Record<string, unknown> = {
      v: 1, alg: 'ES256', session_id: this.sessionId, harness_id: this.key.fingerprint, device_id,
      seq: ++this.seq, ts: Date.now(), nonce: b64uEncode(crypto.getRandomValues(new Uint8Array(16))), op, body, ...overrides,
    }
    env.sig = await signES256(await this.signer, envelopeSigningInput(env))
    return jcs(env)
  }
}
