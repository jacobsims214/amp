import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'
import { Sessions } from './Sessions'
import { ago, inTime } from '../remote/toolText'
import { AppShell } from '../components/shell/AppShell'
import { AuthContext } from '../contexts/auth-context'
import { RemoteProvider } from '../remote/RemoteProvider'
import type { RemoteDeps } from '../remote/remote-context'
import { MOCK_SESSION_ID, MockRelay, mockCrypto, mockTransport } from '../remote/mock/mockRelay'
import type { SessionSummary } from '../remote/types'
import type { Me } from '../types'

vi.setConfig({ testTimeout: 10_000 })

const server = setupServer(http.get('*/api/projects', () => HttpResponse.json({ projects: [] })))
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
afterAll(() => server.close())

const me = (available: boolean): Me => ({
  subject: 's', email: 'jsims@example.com', name: 'Jacob', roles: [], user_id: 1001,
  capabilities: { remote_control: { available, org_ids: available ? [3] : [], reason: available ? null : 'no_org_enabled' } },
})

const other: SessionSummary = {
  session_id: 'rs_OtherSessionBBBBBBBBBB', harness_id: 'h2', harness_fp_display: '8F1D-02AA-71C3-9E40', host: 'build-box',
  os: 'linux', harness_version: '0.4.0', project: { id: 158, name: 'QA 2365 plan' }, online: false, viewers: 0,
  started_at_ms: Date.now() - 5 * 3_600_000, expires_at_ms: Date.now() + 7 * 3_600_000 + 10 * 60_000,
  last_activity_at_ms: Date.now() - 42 * 60_000, protocol_version: 1,
}

function setup(available = true) {
  const relay = new MockRelay({ stepMs: 0, others: [{ ...other }] })
  const deps: RemoteDeps = { transport: mockTransport(relay), crypto: mockCrypto(), mock: false }
  const view = render(
    <AuthContext.Provider value={{ me: me(available), loading: false, error: null, isAdmin: false }}>
      <RemoteProvider deps={deps}>
        <MemoryRouter initialEntries={['/sessions']}>
          <Routes><Route element={<AppShell />}><Route path="/sessions" element={<Sessions />} /></Route></Routes>
        </MemoryRouter>
      </RemoteProvider>
    </AuthContext.Provider>,
  )
  return { relay, ...view }
}

describe('Sessions page', () => {
  it('is a top-level nav entry and lists every live session with its state', async () => {
    setup()
    const nav = screen.getByRole('complementary', { name: 'Main' })
    expect(within(nav).getByRole('link', { name: /Sessions/ })).toHaveAttribute('href', '/sessions')
    const rows = await screen.findAllByTestId('session-row')
    expect(rows).toHaveLength(2)
    const [mine, box] = rows
    // Full page loads to the session page, so it gets its CSP header.
    expect(within(mine).getAllByRole('link').every(a => a.getAttribute('href') === `/sessions/${MOCK_SESSION_ID}`)).toBe(true)
    expect(within(mine).getByText('jacobs-mbp')).toBeInTheDocument()
    expect(within(mine).getByText('Laptop online')).toBeInTheDocument()
    expect(within(mine).getByText('AMP #2')).toBeInTheDocument()
    expect(within(box).getByText('Laptop offline')).toBeInTheDocument()
    expect(within(box).getByText('QA 2365 plan #158')).toBeInTheDocument()
    expect(within(box).getByText('5h ago')).toBeInTheDocument() // started
    expect(within(box).getByText('42m ago')).toBeInTheDocument() // last activity
    expect(within(box).getByText('in 7h 10m')).toBeInTheDocument() // expires
    expect(within(box).getByText('linux · harness 0.4.0')).toBeInTheDocument()
    expect(within(box).getByText('8F1D-02AA-71C3-9E40')).toBeInTheDocument()
    expect(screen.getByText(/Ended sessions aren't listed/)).toBeInTheDocument()
  })

  it('disconnects one session after a confirm', async () => {
    const user = userEvent.setup()
    setup()
    const rows = await screen.findAllByTestId('session-row')
    await user.click(within(rows[1]).getByRole('button', { name: /Disconnect/ }))
    await user.click(within(rows[1]).getByRole('button', { name: 'Disconnect now' }))
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1))
    expect(screen.getByRole('status')).toHaveTextContent(/keeps running locally/)
  })

  it('revokes all after a confirm', async () => {
    const user = userEvent.setup()
    const { relay } = setup()
    await screen.findAllByTestId('session-row')
    await user.click(screen.getByRole('button', { name: /Revoke all/ }))
    await user.click(screen.getByRole('button', { name: 'End all and forget devices' }))
    await screen.findByText('No live sessions')
    expect(relay.ended).toBe('revoked')
    expect(screen.getByRole('status')).toHaveTextContent('Ended 2 sessions and forgot 1 paired device.')
  })

  it('has no nav entry and no list when remote control is unavailable', async () => {
    setup(false)
    expect(within(screen.getByRole('complementary', { name: 'Main' })).queryByRole('link', { name: /Sessions/ })).toBeNull()
    expect(screen.getByText(/Remote control of harness sessions is off/)).toBeInTheDocument()
    expect(screen.queryByTestId('session-row')).toBeNull()
  })

  it('words times for people', () => {
    const now = 1_000_000_000
    expect(ago(now - 10_000, now)).toBe('just now')
    expect(ago(now - 3 * 60_000, now)).toBe('3m ago')
    expect(ago(now - 26 * 3_600_000, now)).toBe('1d ago')
    expect(inTime(now + 25 * 60_000, now)).toBe('in 25m')
    expect(inTime(now + (4 * 60 + 5) * 60_000, now)).toBe('in 4h 05m')
    expect(inTime(now, now)).toBe('now')
  })
})
