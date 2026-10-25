import { IDBFactory } from 'fake-indexeddb'
import { describe, expect, it } from 'vitest'
import { idbStore, memoryStore } from './deviceStore'
import { MAX_PIN_MS, webRemoteCrypto } from './webCrypto'
import { buildCommand, checkEvent } from './envelope'
import { NotPairedError } from './crypto'
import { envelopeSigningInput, pairRequestSigningInput } from './protocol'
import { nodeVerifyES256, SignedHarness, testKey } from '../test/signedHarness'
import { loadVectors } from '../test/vectors'

const harness = testKey('harness-1')
const pin = (until: number) => ({ harness_id: harness.fingerprint, harness_pub: harness.public_raw_b64u, host: 'jacobs-mbp', pinned_until_ms: until })

describe('device key (protocol 3.1)', () => {
  it('is created once, non-extractable, and persisted in IndexedDB across page loads', async () => {
    const idb = new IDBFactory()
    const first = webRemoteCrypto(idbStore(idb))
    expect(await first.deviceId()).toBeNull() // nothing until pairing needs it
    const dev = await first.ensureDevice()
    expect(dev.device_id).toMatch(/^[A-Za-z0-9_-]{43}$/)
    expect(dev.device_pub).toMatch(/^[A-Za-z0-9_-]{87}$/)

    const stored = await idbStore(idb).getDevice()
    expect(stored!.keyPair.privateKey.extractable).toBe(false)
    await expect(crypto.subtle.exportKey('pkcs8', stored!.keyPair.privateKey)).rejects.toThrow()
    await expect(crypto.subtle.exportKey('jwk', stored!.keyPair.privateKey)).rejects.toThrow()

    // A new page load (new instance, same origin's IndexedDB) reuses the same key.
    const again = webRemoteCrypto(idbStore(idb))
    expect(await again.deviceId()).toBe(dev.device_id)
    expect(await again.ensureDevice()).toEqual(dev)
    // Another origin (another IndexedDB) gets its own key.
    expect((await webRemoteCrypto(idbStore(new IDBFactory())).ensureDevice()).device_id).not.toBe(dev.device_id)
  })

  it('creates one key even when asked twice at once', async () => {
    const store = memoryStore()
    const c = webRemoteCrypto(store)
    const [a, b] = await Promise.all([c.ensureDevice(), c.ensureDevice()])
    expect(a.device_id).toBe(b.device_id)
  })

  it('signs commands the harness verifies with the device key (round trip, node:crypto)', async () => {
    const c = webRemoteCrypto(memoryStore())
    const dev = await c.ensureDevice()
    const cmd = buildCommand({ session_id: 'rs_YIL0d-r7J-AHLPpZ00o2bA', harness_id: harness.fingerprint, device_id: dev.device_id,
      seq: 1_792_948_800_000, ts: 1_792_948_800_000, op: 'prompt', body: { text: '/quit is literal   é', client_msg_id: 'm-1' } })
    const env = await c.signCommand(cmd)
    expect(Object.keys(env).sort()).toEqual(['alg', 'body', 'device_id', 'harness_id', 'nonce', 'op', 'seq', 'session_id', 'sig', 'ts', 'v'])
    expect(nodeVerifyES256(dev.device_pub, envelopeSigningInput(cmd), env.sig)).toBe(true)
    // The relay may reorder members or escape non-ASCII on the wire: the harness verifies JCS of the parsed value.
    const reordered = JSON.parse(JSON.stringify(Object.fromEntries(Object.entries(env).reverse())))
    expect(nodeVerifyES256(dev.device_pub, envelopeSigningInput(reordered), env.sig)).toBe(true)
    await expect(c.signCommand({ ...cmd, device_id: 'someone-else' })).rejects.toThrow()
  })

  it('signs pair requests (protocol 7.2)', async () => {
    const c = webRemoteCrypto(memoryStore())
    const dev = await c.ensureDevice()
    const req = { type: 'pair_request' as const, v: 1 as const, session_id: 'rs_YIL0d-r7J-AHLPpZ00o2bA', harness_id: harness.fingerprint,
      pairing_id: '8D2MNaiyK5zBsUK22WHj_A', device_id: dev.device_id, device_pub: dev.device_pub, device_name: 'Chrome on macOS', ts: 1, nonce: 'n' }
    const sig = await c.signPairRequest(req)
    expect(nodeVerifyES256(dev.device_pub, pairRequestSigningInput(req), sig)).toBe(true)
  })

  it('refuses to sign before a device key exists', async () => {
    const c = webRemoteCrypto(memoryStore())
    await expect(c.signCommand(buildCommand({ session_id: 's', harness_id: 'h', device_id: 'd', seq: 1, ts: 1, op: 'stop', body: {} }))).rejects.toBeInstanceOf(NotPairedError)
  })
})

describe('harness pins (protocol 7.7)', () => {
  it('stores a pin capped at 30 days and returns it until it expires', async () => {
    let now = 1_792_948_800_000
    const store = memoryStore()
    const c = webRemoteCrypto(store, () => now)
    const saved = await c.pinHarness(pin(now + 90 * 86_400_000))
    expect(saved.pinned_until_ms).toBe(now + MAX_PIN_MS)
    expect(await c.harnessPin(harness.fingerprint)).toMatchObject({ harness_id: harness.fingerprint, host: 'jacobs-mbp' })
    // The harness may give less (org device_pin_days); that is kept.
    await c.pinHarness(pin(now + 86_400_000))
    expect((await c.harnessPin(harness.fingerprint))!.pinned_until_ms).toBe(now + 86_400_000)
    now += 86_400_000
    expect(await c.harnessPin(harness.fingerprint)).toBeNull()
    expect(await store.getPin(harness.fingerprint)).toBeNull() // expired pins are deleted
  })

  it('never pins a key that doesn\'t hash to the harness id', async () => {
    const c = webRemoteCrypto(memoryStore())
    await expect(c.pinHarness({ ...pin(Date.now() + 1000), harness_pub: testKey('harness-2').public_raw_b64u })).rejects.toThrow()
    expect(await c.harnessPin(harness.fingerprint)).toBeNull()
  })

  it('persists pins in IndexedDB', async () => {
    const idb = new IDBFactory()
    await webRemoteCrypto(idbStore(idb)).pinHarness(pin(Date.now() + 86_400_000))
    expect(await webRemoteCrypto(idbStore(idb)).harnessPin(harness.fingerprint)).toMatchObject({ harness_id: harness.fingerprint })
  })

  it('verifies events from the pinned harness and rejects others', async () => {
    const c = webRemoteCrypto(memoryStore())
    const p = await c.pinHarness(pin(Date.now() + 86_400_000))
    const h = new SignedHarness('rs_YIL0d-r7J-AHLPpZ00o2bA')
    const wire = await h.event('status', { busy: false })
    const rx = { session_id: h.sessionId, pinned_harness: p, last_applied_seq: 0 }
    expect((await checkEvent(wire, rx, c.verifyEvent)).verdict).toBe('ok')
    const tampered = JSON.stringify({ ...JSON.parse(wire), body: { busy: true } })
    expect((await checkEvent(tampered, rx, c.verifyEvent)).verdict).toBe('bad_signature')
    // The vectors, through the real implementation.
    for (const v of loadVectors<{ vectors: { wire: string; expect: string; receiver: typeof rx }[] }>('events.json').vectors) {
      expect((await checkEvent(v.wire, v.receiver, c.verifyEvent)).verdict).toBe(v.expect)
    }
  })
})
