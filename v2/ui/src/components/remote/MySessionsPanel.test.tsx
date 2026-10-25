import { render, screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { MyWork } from '../../pages/MyWork'
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
      await screen.findByText('Tickets I own')
      expect(screen.queryByText('My sessions')).toBeNull()
      unmount()
    }
  })
})
