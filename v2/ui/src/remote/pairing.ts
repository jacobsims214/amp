/**
 * Building a pair request (protocol 7.2, 7.3) and reading its result (7.5).
 * Pure apart from WebCrypto, so the derivations are checked against
 * pairing.json byte for byte.
 */
import {
  b64uEncode, decodeRawPublicKey, deriveMacKey, deriveSasKey, hmacSha256, pairingId, pairRequestSigningInput, sasCode,
  type PairMessage, type PairRequest,
} from './protocol'
import type { DeviceIdentity } from './crypto'
import type { PairResultBody } from './types'

export interface PairingInputs {
  secret: Uint8Array
  session_id: string
  /** From the link; already checked to equal the session's harness_id and fp(harness_pub). */
  harness_id: string
  harness_pub: string
  device: DeviceIdentity
  device_name: string
  ts: number
  nonce: string
  /** Signs the pair request's signing input with the device key. */
  sign(request: PairRequest): Promise<string>
}

export interface PreparedPairing {
  message: PairMessage
  pairing_id: string
  /** "NNNN-NNNN", the code the laptop shows too. */
  code: string
}

export async function preparePairing(i: PairingInputs): Promise<PreparedPairing> {
  const pairing_id = await pairingId(i.secret)
  const [kMac, kSas] = await Promise.all([deriveMacKey(i.secret, i.session_id), deriveSasKey(i.secret, i.session_id)])
  const request: PairRequest = {
    type: 'pair_request', v: 1,
    session_id: i.session_id, harness_id: i.harness_id, pairing_id,
    device_id: i.device.device_id, device_pub: i.device.device_pub,
    device_name: i.device_name,
    ts: i.ts, nonce: i.nonce,
  }
  const mac = b64uEncode(await hmacSha256(kMac, pairRequestSigningInput(request)))
  const sig = await i.sign(request)
  const code = await sasCode(kSas, decodeRawPublicKey(i.harness_pub), decodeRawPublicKey(i.device.device_pub))
  kMac.fill(0)
  kSas.fill(0)
  return { message: { request, mac, sig }, pairing_id, code }
}

/** Words for a pairing that didn't end in `accepted` (protocol 7.5 results, 9.3 codes). */
export function pairResultText(b: Pick<PairResultBody, 'result' | 'code' | 'tries_left'>): string {
  if (b.result === 'rejected') return 'Pairing was declined on the laptop. Nothing was paired.'
  if (b.result === 'expired') return 'Nobody answered on the laptop before the link expired. Run /remote pair on the laptop and open the new link.'
  if (b.result === 'locked') return 'Too many failed tries with this link, so the laptop locked it. Run /remote pair on the laptop for a new one.'
  const left = b.tries_left != null ? ` (${b.tries_left} ${b.tries_left === 1 ? 'try' : 'tries'} left)` : ''
  const why: Record<string, string> = {
    wrong_user: 'The laptop is signed in to AMP as someone else. Only the person who started the session can pair.',
    stale_ts: 'This device\'s clock is more than a minute off the laptop\'s. Fix the clock and try again.',
    bad_mac: 'This link doesn\'t match the laptop\'s current pairing secret. Open the newest link from the laptop.',
    unknown_pairing: 'The laptop isn\'t waiting for this link any more. Run /remote pair on the laptop and open the new link.',
    pairing_expired: 'This link has expired (links last 5 minutes). Run /remote pair on the laptop for a new one.',
    pairing_locked: 'Too many tries with this link. Run /remote pair on the laptop for a new one.',
    unsupported_version: 'The laptop runs a different harness version. Update AMP and the harness.',
    wrong_session: 'The laptop says this link is for another session.',
    wrong_harness: 'This link is for another laptop.',
  }
  const known = b.code ? why[b.code] : undefined
  return known ? `${known.replace(/\.$/, '')}${left}.` : `The laptop couldn't check this device's request (${b.code ?? 'failed'})${left}.`
}

/** Words for the relay's answer to POST /pair (protocol 8.3). */
export function pairPostText(code: string, fallback: string): string {
  const m: Record<string, string> = {
    pairing_unavailable: 'The laptop isn\'t waiting for this link any more. Run /remote pair on the laptop and open the new link.',
    pairing_expired: 'This link has expired (links last 5 minutes). Run /remote pair on the laptop for a new one.',
    pairing_locked: 'Too many tries with this link. Run /remote pair on the laptop for a new one.',
    harness_offline: 'The laptop is offline. Wake it, then open the link again.',
    audit_unavailable: 'AMP couldn\'t record the pairing, so it stopped. Try again in a moment.',
    remote_disabled: 'Remote control is turned off for this organization.',
    session_ended: 'This session has ended.',
    not_found: 'No such session.',
  }
  return m[code] ?? fallback
}
