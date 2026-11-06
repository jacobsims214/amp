import { type ReactNode, useEffect, useState } from 'react'
import type { Me } from '../types'
import { AuthContext } from './auth-context'
import { setSignInOff, signInAgainUrl } from '../lib/session'

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
        // Only a 401 or a redirect sends the browser to sign-in. An HTML error
        // page (nginx's 502 while amp-api starts) is an outage: redirecting on
        // it would loop, since nothing serves /oauth2/start without sign-in.
        if (res.type === 'opaqueredirect' || res.status === 401) {
          window.location.replace(window.location.pathname === '/' && !window.location.search ? '/home/' : signInAgainUrl())
          return
        }
        if (!res.ok) throw new Error(`Failed to load identity (${res.status})`)
        const who: Me = await res.json()
        // Sign-in off (Local Dev): there is no session to lose, so never prompt.
        setSignInOff(!!who.local_dev)
        setMe(who)
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
