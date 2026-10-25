/**
 * The signing hook. Everything cryptographic about remote control sits behind
 * this interface so the session page never touches keys itself.
 *
 * #2375 (pairing, device key, signed commands) provides the real
 * implementation:
 *  - a non-extractable WebCrypto ECDSA P-256 device key in IndexedDB
 *    (`deviceId` = b64u(SHA-256(raw public key)));
 *  - harness pins written only after a pairing confirmed at the laptop, never
 *    from a relay-supplied key (protocol 7.5 and 7.7);
 *  - `signCommand`: signing input "amp-rc/1 envelope\n" ‖ JCS(envelope without
 *    sig), ES256, 64-byte P1363 signature as b64u (protocol 3.2);
 *  - `verifyEvent`: the same input checked against `pin.harness_pub`, accepting
 *    high-S (protocol 3.4).
 *
 * Until then the page uses `unpairedCrypto`: no pins, so it shows "Pair this
 * device from the laptop" and renders nothing from the relay.
 */
import type { CommandEnvelope, Envelope, HarnessPin, UnsignedCommand } from './types'

export interface RemoteCrypto {
  /** The pin for this harness if this browser paired with it and the pin is unexpired, else null. */
  harnessPin(harnessId: string): Promise<HarnessPin | null>
  /** This browser's device id (fingerprint of its device public key), or null if it has no device key yet. */
  deviceId(): Promise<string | null>
  /** Signs a command envelope with the device key (protocol 3.2). Rejects if the device can't sign. */
  signCommand(cmd: UnsignedCommand): Promise<CommandEnvelope>
  /** Verifies a harness event's `sig` with the pinned harness key (protocol 5.7 step 7). */
  verifyEvent(env: Envelope, pin: HarnessPin): Promise<boolean>
}

export class NotPairedError extends Error {
  constructor() {
    super('This browser isn\'t paired with the laptop yet.')
    this.name = 'NotPairedError'
  }
}

/** The default until #2375 lands: nothing is pinned, nothing verifies, nothing signs. */
export const unpairedCrypto: RemoteCrypto = {
  harnessPin: async () => null,
  deviceId: async () => null,
  signCommand: async () => { throw new NotPairedError() },
  verifyEvent: async () => false,
}
