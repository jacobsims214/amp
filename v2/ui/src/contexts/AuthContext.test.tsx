// AuthProvider sends the browser to sign-in only on a 401 or a redirect
// (#153). An HTML error page from the gateway (nginx's 502 while amp-api
// starts) shows the load error: redirecting on it would loop.
import { render, screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { useContext } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider } from './AuthContext'
import { AuthContext } from './auth-context'
import { isSessionEnded, resetSession } from '../lib/session'

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => resetSession())
afterEach(() => { server.resetHandlers(); vi.restoreAllMocks() })
afterAll(() => server.close())

function Probe() {
  const auth = useContext(AuthContext)
  if (auth.loading) return <p>loading</p>
  return <p>{auth.error ?? auth.me?.email}</p>
}

describe('AuthProvider', () => {
  it("a 502 HTML page shows the load error and doesn't redirect to sign-in", async () => {
    const replace = vi.fn()
    vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, pathname: '/oauth2/start', search: '?rd=%2F', replace })
    server.use(http.get('/api/me', () => new HttpResponse('<html><h1>502 Bad Gateway</h1></html>', { status: 502, headers: { 'Content-Type': 'text/html' } })))
    render(<AuthProvider><Probe /></AuthProvider>)
    expect(await screen.findByText('Failed to load identity (502)')).toBeInTheDocument()
    expect(replace).not.toHaveBeenCalled()
    expect(isSessionEnded()).toBe(false)
  })

  it('a 401 sends the browser to sign-in, back to this page', async () => {
    const replace = vi.fn()
    vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, pathname: '/project/3/board', search: '', replace })
    server.use(http.get('/api/me', () => new HttpResponse('Unauthorized', { status: 401 })))
    render(<AuthProvider><Probe /></AuthProvider>)
    await vi.waitFor(() => expect(replace).toHaveBeenCalledWith('/oauth2/start?rd=%2Fproject%2F3%2Fboard'))
  })
})
