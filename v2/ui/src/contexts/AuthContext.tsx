import { type ReactNode, useEffect, useState } from 'react'
import type { Me } from '../types'
import { AuthContext } from './auth-context'

// AuthProvider resolves the caller's identity via /api/me. In a real
// deployment the request already carries the session (oauth2-proxy cookie)
// or bearer token; this just surfaces who that identity is to the UI.
// When auth is disabled (plain `make dev` / docker-compose), amp-api
// returns the anonymous "local-dev" identity so this never blocks the UI.
export function AuthProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    // Behind Envoy, "/" and the app's files are public (so signed-out
    // visitors can reach the home page); the API isn't. A signed-out request
    // to it is redirected to sign-in, which we see as an opaque redirect.
    fetch('/api/me', { redirect: 'manual', headers: { Accept: 'application/json' } })
      .then(async res => {
        if (res.type === 'opaqueredirect' || res.status === 401) {
          const here = window.location.pathname + window.location.search
          window.location.replace(here === '/' ? '/home/' : `/oauth2/start?rd=${encodeURIComponent(here)}`)
          return
        }
        if (!res.ok) throw new Error(`Failed to load identity (${res.status})`)
        setMe(await res.json())
      })
      .catch(e => setError(e instanceof Error ? e.message : 'Failed to load identity'))
      .finally(() => setLoading(false))
  }, [])

  const isAdmin = !!me?.roles?.includes('admin')

  return (
    <AuthContext.Provider value={{ me, loading, error, isAdmin }}>
      {children}
    </AuthContext.Provider>
  )
}
