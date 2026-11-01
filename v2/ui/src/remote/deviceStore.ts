/**
 * Where this browser keeps its remote-control trust (protocol 3.1 and 7.7), in
 * IndexedDB, which is per AMP origin:
 *  - the device key: the non-extractable WebCrypto `CryptoKeyPair` itself
 *    (structured clone keeps the private key non-extractable, so no script
 *    can ever read it out, only use it to sign on this origin);
 *  - harness pins `{harness_id, harness_pub, host, pinned_until_ms}`, written
 *    only after a pairing confirmed at the laptop.
 *
 * Never stored: the pairing secret, envelopes, transcript text.
 */
import type { HarnessPin } from './types'

export interface StoredDevice {
  keyPair: CryptoKeyPair
  /** fp(raw public key), cached so reads don't re-hash. */
  device_id: string
  device_pub: string
  created_at_ms: number
}

export interface RemoteStore {
  getDevice(): Promise<StoredDevice | null>
  putDevice(d: StoredDevice): Promise<void>
  getPin(harnessId: string): Promise<HarnessPin | null>
  putPin(pin: HarnessPin): Promise<void>
  deletePin(harnessId: string): Promise<void>
}

const DB_NAME = 'amp-remote-control'
const DB_VERSION = 1
const DEVICE = 'device'
const PINS = 'harness_pins'
const DEVICE_KEY = 'device'

function req<T>(r: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    r.onsuccess = () => resolve(r.result)
    r.onerror = () => reject(r.error)
  })
}

/** The IndexedDB store. `factory` is injectable for tests. */
export function idbStore(factory: IDBFactory = globalThis.indexedDB): RemoteStore {
  let dbp: Promise<IDBDatabase> | null = null
  const db = () => {
    if (!dbp) {
      dbp = new Promise<IDBDatabase>((resolve, reject) => {
        if (!factory) { reject(new Error('This browser has no IndexedDB, so it can\'t keep a device key.')); return }
        const open = factory.open(DB_NAME, DB_VERSION)
        open.onupgradeneeded = () => {
          const d = open.result
          if (!d.objectStoreNames.contains(DEVICE)) d.createObjectStore(DEVICE)
          if (!d.objectStoreNames.contains(PINS)) d.createObjectStore(PINS, { keyPath: 'harness_id' })
        }
        open.onsuccess = () => resolve(open.result)
        open.onerror = () => reject(open.error)
        open.onblocked = () => reject(new Error('The device key store is blocked by another tab.'))
      })
      dbp.catch(() => { dbp = null })
    }
    return dbp
  }
  const tx = async <T>(store: string, mode: IDBTransactionMode, fn: (s: IDBObjectStore) => IDBRequest<T>) => {
    const t = (await db()).transaction(store, mode)
    const done = new Promise<void>((resolve, reject) => {
      t.oncomplete = () => resolve()
      t.onerror = () => reject(t.error)
      t.onabort = () => reject(t.error)
    })
    const result = await req(fn(t.objectStore(store)))
    await done
    return result
  }

  return {
    getDevice: async () => (await tx<StoredDevice | undefined>(DEVICE, 'readonly', s => s.get(DEVICE_KEY))) ?? null,
    putDevice: async d => { await tx(DEVICE, 'readwrite', s => s.put(d, DEVICE_KEY)) },
    getPin: async id => (await tx<HarnessPin | undefined>(PINS, 'readonly', s => s.get(id))) ?? null,
    putPin: async pin => { await tx(PINS, 'readwrite', s => s.put(pin)) },
    deletePin: async id => { await tx(PINS, 'readwrite', s => s.delete(id)) },
  }
}

/** An in-memory store (tests, and browsers without IndexedDB where nothing should persist). */
export function memoryStore(): RemoteStore {
  let device: StoredDevice | null = null
  const pins = new Map<string, HarnessPin>()
  return {
    getDevice: async () => device,
    putDevice: async d => { device = d },
    getPin: async id => pins.get(id) ?? null,
    putPin: async pin => { pins.set(pin.harness_id, { ...pin }) },
    deletePin: async id => { pins.delete(id) },
  }
}
