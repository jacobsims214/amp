/**
 * The signing hook. Everything cryptographic about remote control sits behind
 * this interface so the session page never touches keys itself.
 *
 * The real implementation is `webRemoteCrypto` (webCrypto.ts, #2375):
 *  - a non-extractable WebCrypto ECDSA P-256 device key in IndexedDB
 *    (`deviceId` = b64u(SHA-256(raw public key)));
 *  - harness pins written only after a pairing confirmed at the laptop, never
 *    from a relay-supplied key (protocol 7.5 and 7.7), for at most 30 days;
 *  - `signCommand`: signing input "amp-rc/1 envelope\n" ‖ JCS(envelope without
 *    sig), ES256, 64-byte P1363 signature as b64u (protocol 3.2);
 *  - `verifyEvent`: the same input checked against `pin.harness_pub`, accepting
 *    high-S (protocol 3.4).
 *
 * `unpairedCrypto` (no pins, no key) is what tests and browsers without
 * WebCrypto get: the page shows "Pair this device from the laptop" and renders
 * nothing from the relay.
 */
import type { PairRequest } from './protocol'
import type { CommandEnvelope, Envelope, HarnessPin, UnsignedCommand } from './types'

/** This browser's public identity for remote control. */
export interface DeviceIdentity { device_id: string; device_pub: string }

export interface RemoteCrypto {
  /** The pin for this harness if this browser paired with it and the pin is unexpired, else null. */
  harnessPin(harnessId: string): Promise<HarnessPin | null>
  /** This browser's device id (fingerprint of its device public key), or null if it has no device key yet. */
  deviceId(): Promise<string | null>
  /** Signs a command envelope with the device key (protocol 3.2). Rejects if the device can't sign. */
  signCommand(cmd: UnsignedCommand): Promise<CommandEnvelope>
  /** Verifies a harness event's `sig` with the pinned harness key (protocol 5.7 step 7). */
  verifyEvent(env: Envelope, pin: HarnessPin): Promise<boolean>
  /** Creates the device key on first use (non-extractable, persisted), then reuses it. */
  ensureDevice(): Promise<DeviceIdentity>
  /** Signs a pair request (protocol 7.2) with the device key; returns the b64u P1363 signature. */
  signPairRequest(request: PairRequest): Promise<string>
  /**
   * Pins a harness after the laptop accepted this device (protocol 7.5). Refuses a key whose
   * fingerprint isn't `harness_id`, and caps the pin at 30 days.
   */
  pinHarness(pin: HarnessPin): Promise<HarnessPin>
}

export class NotPairedError extends Error {
  constructor() {
    super('This browser isn\'t paired with the laptop yet.')
    this.name = 'NotPairedError'
  }
}

/** Nothing is pinned, nothing verifies, nothing signs. */
export const unpairedCrypto: RemoteCrypto = {
  harnessPin: async () => null,
  deviceId: async () => null,
  signCommand: async () => { throw new NotPairedError() },
  verifyEvent: async () => false,
  ensureDevice: async () => { throw new NotPairedError() },
  signPairRequest: async () => { throw new NotPairedError() },
  pinHarness: async () => { throw new NotPairedError() },
}
