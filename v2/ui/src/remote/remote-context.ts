import { createContext, useContext } from 'react'
import { unpairedCrypto, type RemoteCrypto } from './crypto'
import { httpTransport, type RelayTransport } from './transport'
import type { Me } from '../types'

/** What the remote-control UI talks to. Tests and the dev mock swap both. */
export interface RemoteDeps {
  transport: RelayTransport
  crypto: RemoteCrypto
  /** True when running against the in-memory mock relay (VITE_REMOTE_MOCK=1 builds only). */
  mock: boolean
}

export const defaultRemoteDeps: RemoteDeps = { transport: httpTransport, crypto: unpairedCrypto, mock: false }

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
