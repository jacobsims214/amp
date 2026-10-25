import { type ReactNode, useEffect, useState } from 'react'
import { defaultRemoteDeps, RemoteContext, type RemoteDeps } from './remote-context'

/**
 * Provides the relay transport and the signing hook. With VITE_REMOTE_MOCK=1 (or =parity, the recorded harness feed)
 * at build or dev time (`VITE_REMOTE_MOCK=1 npm run dev`) it swaps in the
 * in-memory mock relay and harness, so the page can be worked on before the
 * relay exists. Normal builds never include the mock: the flag is replaced at
 * build time, the branch is dead code and the dynamic import is dropped.
 */
export function RemoteProvider({ children, deps }: { children: ReactNode; deps?: RemoteDeps }) {
  const [mockDeps, setMockDeps] = useState<RemoteDeps | null>(null)
  useEffect(() => {
    if (deps) return
    // Compared inline (not through a variable) so a normal build drops both branches and the mock.
    if (import.meta.env.VITE_REMOTE_MOCK === '1') {
      import('./mock/mockRelay').then(m => setMockDeps(m.devMockDeps()))
    } else if (import.meta.env.VITE_REMOTE_MOCK === 'parity2388') {
      // #2388: the real harness run with its status, workers, compaction, refusal and provider error.
      Promise.all([import('./mock/mockRelay'), import('./mock/parity2388.json')])
        .then(([m, r]) => setMockDeps(m.devMockDeps2388(r.default as Parameters<typeof m.devMockDeps2388>[0], {
          cut: Number(import.meta.env.VITE_PARITY_CUT ?? 0), workers: import.meta.env.VITE_PARITY_SCENE === 'workers',
        })))
    } else if (import.meta.env.VITE_REMOTE_MOCK === 'parity') {
      // The feed recorded from the real harness chat (#2386), for side-by-side checks with the TUI.
      Promise.all([import('./mock/mockRelay'), import('./mock/parityStream.json')])
        .then(([m, s]) => setMockDeps(m.devMockDeps('parity', s.default as Parameters<typeof m.devMockDeps>[1], {
          cut: Number(import.meta.env.VITE_PARITY_CUT ?? 0), scene: import.meta.env.VITE_PARITY_SCENE, offline: import.meta.env.VITE_PARITY_OFFLINE === '1',
        })))
    }
  }, [deps])
  return <RemoteContext.Provider value={deps ?? mockDeps ?? defaultRemoteDeps}>{children}</RemoteContext.Provider>
}
