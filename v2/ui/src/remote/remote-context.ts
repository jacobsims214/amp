import { createContext, useContext } from 'react'
import { unpairedCrypto, type RemoteCrypto } from './crypto'
import { hasWebCrypto, webRemoteCrypto } from './webCrypto'
import { httpTransport, type RelayTransport } from './transport'
import { sessionPageOnly } from './keyGate'
import type { Me } from '../types'

/** What the remote-control UI talks to. Tests and the dev mock swap both. */
export interface RemoteDeps {
  transport: RelayTransport
  crypto: RemoteCrypto
  /** True when running against the in-memory mock relay (VITE_REMOTE_MOCK=1 builds only). */
  mock: boolean
}

/**
 * The real deps: the HTTP relay client and WebCrypto with IndexedDB. A browser
 * without either (an insecure origin, an old browser) gets `unpairedCrypto`, so
 * it can never sign or trust anything. The key's private operations work only on a
 * session page loaded in full (keyGate.ts, #2393).
 */
export const defaultRemoteDeps: RemoteDeps = {
  transport: httpTransport,
  crypto: hasWebCrypto() ? sessionPageOnly(webRemoteCrypto()) : unpairedCrypto,
  mock: false,
}

export const RemoteContext = createContext<RemoteDeps>(defaultRemoteDeps)

export function useRemote(): RemoteDeps {
  return useContext(RemoteContext)
}

/**
 * The capability flag (protocol 8.3, `/api/me`). An old API lacks the member,
 * which means unavailable. The dev mock turns it on.
 */
export function remoteAvailable(me: Me | null, deps: RemoteDeps): boolean {
  if (deps.mock) return true
  return me?.capabilities?.remote_control?.available === true
}
