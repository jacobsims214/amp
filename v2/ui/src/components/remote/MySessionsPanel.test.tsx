import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { MyWork } from '../../pages/MyWork'
import { MySessionsPanel } from './MySessionsPanel'
import { AuthContext } from '../../contexts/auth-context'
import { RemoteProvider } from '../../remote/RemoteProvider'
import { httpTransport } from '../../remote/transport'
import { unpairedCrypto } from '../../remote/crypto'
import { relayHandlers } from '../../remote/mock/handlers'
import { MOCK_SESSION_ID, MockRelay } from '../../remote/mock/mockRelay'
import type { Me } from '../../types'

const relay = new MockRelay({ stepMs: 0 })
const server = setupServer(
  ...relayHandlers(relay),
  http.get('*/api/my-work', () => HttpResponse.json({ owned: [], flags: [], agents: [], done_24h: 0 })),
  http.get('*/api/velocity', () => HttpResponse.json({ days: [] })),
  http.get('*/api/day-reports/*', () => HttpResponse.json({ error: 'none' }, { status: 404 })),
  http.get('*/api/artifacts/expiring', () => HttpResponse.json({ artifacts: [] })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
afterAll(() => server.close())

const me = (available?: boolean): Me => ({
  subject: 's', email: 'jsims@example.com', name: 'Jacob', roles: [], user_id: 1001,
  ...(available === undefined ? {} : { capabilities: { remote_control: { available, reason: available ? null : 'no_org_enabled' } } }),
})

function renderMyWork(who: Me) {
  return render(
    <AuthContext.Provider value={{ me: who, loading: false, error: null, isAdmin: false }}>
      <RemoteProvider deps={{ transport: httpTransport, crypto: unpairedCrypto, mock: false }}>
        <MemoryRouter><MyWork /></MemoryRouter>
      </RemoteProvider>
    </AuthContext.Provider>,
  )
}

describe('My Work → My sessions', () => {
  it('lists live sessions with full-page links when remote control is available', async () => {
    renderMyWork(me(true))
    const link = await screen.findByRole('link', { name: /jacobs-mbp/ }, { timeout: 4000 })
    expect(link).toHaveAttribute('href', `/sessions/${MOCK_SESSION_ID}`)
    expect(link).toHaveTextContent('online')
    expect(screen.getByText('My sessions')).toBeInTheDocument()
  })

  it('is hidden when the capability flag says unavailable, or the API has no flag', async () => {
    for (const who of [me(false), me()]) {
      const { unmount } = renderMyWork(who)
      await screen.findByText(/Nothing here acts on a ticket/)
      expect(screen.queryByText('My sessions')).toBeNull()
      unmount()
    }
  })
})

// The home page often sits in a background tab: the 30 s refresh makes no
// fetch while the tab is hidden, and one when it is shown again.
describe('My sessions polling', () => {
  const realSetTimeout = globalThis.setTimeout
  let visibility: DocumentVisibilityState = 'visible'
  let fetches = 0
  const countSessionFetches = ({ request }: { request: Request }) => {
    if (new URL(request.url).pathname === '/api/sessions') fetches++
  }
  const setVisibility = (state: DocumentVisibilityState) => act(() => {
    visibility = state
    document.dispatchEvent(new Event('visibilitychange'))
  })
  const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms))
  // Give any fetch that was started time to reach the server.
  const settle = () => act(() => new Promise(r => realSetTimeout(r, 50)))

  const renderPanel = () => render(
    <RemoteProvider deps={{ transport: httpTransport, crypto: unpairedCrypto, mock: false }}>
      <MemoryRouter><MySessionsPanel /></MemoryRouter>
    </RemoteProvider>,
  )

  beforeEach(() => {
    // Fake time still moves with real time, so findBy and waitFor keep working.
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
    visibility = 'visible'
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
    fetches = 0
    server.events.on('request:start', countSessionFetches)
  })
  afterEach(() => {
    cleanup()
    server.events.removeListener('request:start', countSessionFetches)
    vi.useRealTimers()
    // Drop the override so jsdom's own getter shows through again.
    delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
  })

  it('loads on mount, refreshes every 30 s while visible, and stops on unmount', async () => {
    const { unmount } = renderPanel()
    await screen.findByRole('link', { name: /jacobs-mbp/ })
    expect(fetches).toBe(1)

    await advance(30_000)
    await waitFor(() => expect(fetches).toBe(2))
    await advance(30_000)
    await waitFor(() => expect(fetches).toBe(3))

    unmount()
    await advance(90_000)
    await settle()
    expect(fetches).toBe(3)
  })

  it('makes no fetch while the tab is hidden, then one when it is shown', async () => {
    renderPanel()
    await screen.findByRole('link', { name: /jacobs-mbp/ })
    expect(fetches).toBe(1)

    await setVisibility('hidden')
    await advance(3 * 30_000)
    await settle()
    expect(fetches).toBe(1)

    // One catch-up fetch, and the next refresh is a full 30 s after it.
    await setVisibility('visible')
    await waitFor(() => expect(fetches).toBe(2))
    await advance(29_000)
    await settle()
    expect(fetches).toBe(2)
    await advance(1_000)
    await waitFor(() => expect(fetches).toBe(3))
  })
})
