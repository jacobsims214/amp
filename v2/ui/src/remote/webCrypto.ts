/**
 * The real RemoteCrypto (protocol 3, 7.7): a non-extractable P-256 device key
 * and harness pins in IndexedDB, ES256 signing and verification on WebCrypto.
 */
import { NotPairedError, type DeviceIdentity, type RemoteCrypto } from './crypto'
import { idbStore, type RemoteStore, type StoredDevice } from './deviceStore'
import {
  b64uEncode, envelopeSigningInput, exportRawPublicKey, fingerprint, generateDeviceKey, keyMatchesId,
  pairRequestSigningInput, signES256, verifyEnvelope,
} from './protocol'
import type { HarnessPin } from './types'

/** Device trust lasts at most 30 days (D1); the harness may give less (org `device_pin_days`). */
export const MAX_PIN_MS = 30 * 86_400_000

export function webRemoteCrypto(store: RemoteStore = idbStore(), now: () => number = Date.now): RemoteCrypto {
  let creating: Promise<StoredDevice> | null = null

  const loadDevice = async (): Promise<StoredDevice | null> => {
    const d = await store.getDevice()
    return d && d.keyPair?.privateKey && d.keyPair?.publicKey ? d : null
  }

  const device = async (): Promise<StoredDevice> => {
    const existing = await loadDevice()
    if (existing) return existing
    creating ??= (async () => {
      const keyPair = await generateDeviceKey()
      const raw = await exportRawPublicKey(keyPair.publicKey)
      const d: StoredDevice = { keyPair, device_id: await fingerprint(raw), device_pub: b64uEncode(raw), created_at_ms: now() }
      await store.putDevice(d)
      return d
    })().finally(() => { creating = null })
    return creating
  }

  const signer = async (): Promise<StoredDevice> => {
    const d = await loadDevice()
    if (!d) throw new NotPairedError()
    return d
  }

  return {
    async harnessPin(harnessId) {
      const pin = await store.getPin(harnessId)
      if (!pin) return null
      if (pin.harness_id !== harnessId || pin.pinned_until_ms <= now() || !(await keyMatchesId(pin.harness_pub, harnessId))) {
        await store.deletePin(harnessId).catch(() => {})
        return null
      }
      return pin
    },

    async deviceId() {
      return (await loadDevice())?.device_id ?? null
    },

    async signCommand(cmd) {
      const d = await signer()
      if (cmd.device_id !== d.device_id) throw new Error('The command names another device.')
      return { ...cmd, sig: await signES256(d.keyPair.privateKey, envelopeSigningInput(cmd)) }
    },

    verifyEvent: (env, pin) => verifyEnvelope(env, pin),

    async ensureDevice(): Promise<DeviceIdentity> {
      const d = await device()
      return { device_id: d.device_id, device_pub: d.device_pub }
    },

    async signPairRequest(request) {
      const d = await signer()
      if (request.device_id !== d.device_id || request.device_pub !== d.device_pub) throw new Error('The pair request names another device.')
      return signES256(d.keyPair.privateKey, pairRequestSigningInput(request))
    },

    async pinHarness(pin) {
      if (!(await keyMatchesId(pin.harness_pub, pin.harness_id))) throw new Error('The laptop key doesn\'t match its fingerprint.')
      const capped: HarnessPin = { ...pin, pinned_until_ms: Math.min(pin.pinned_until_ms, now() + MAX_PIN_MS) }
      await store.putPin(capped)
      return capped
    },
  }
}

/** The app's default: WebCrypto with IndexedDB when the browser has both, else nothing trusted. */
export function hasWebCrypto(): boolean {
  return typeof globalThis.crypto?.subtle?.generateKey === 'function' && typeof globalThis.indexedDB !== 'undefined'
}
