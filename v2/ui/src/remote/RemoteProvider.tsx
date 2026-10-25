import { type ReactNode, useEffect, useState } from 'react'
import { defaultRemoteDeps, RemoteContext, type RemoteDeps } from './remote-context'

/**
 * Provides the relay transport and the signing hook. With VITE_REMOTE_MOCK=1
 * at build or dev time (`VITE_REMOTE_MOCK=1 npm run dev`) it swaps in the
 * in-memory mock relay and harness, so the page can be worked on before the
 * relay exists. Normal builds never include the mock: the flag is replaced at
 * build time, the branch is dead code and the dynamic import is dropped.
 */
export function RemoteProvider({ children, deps }: { children: ReactNode; deps?: RemoteDeps }) {
  const [mockDeps, setMockDeps] = useState<RemoteDeps | null>(null)
  useEffect(() => {
    if (deps) return
    if (import.meta.env.VITE_REMOTE_MOCK === '1') {
      import('./mock/mockRelay').then(m => setMockDeps(m.devMockDeps()))
    }
  }, [deps])
  return <RemoteContext.Provider value={deps ?? mockDeps ?? defaultRemoteDeps}>{children}</RemoteContext.Provider>
}
