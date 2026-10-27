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
import { httpTransport } from '../remote/transport'
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
  state: 'active', end_reason: null, ended_at_ms: null, model: 'openai/gpt-5.5', ticket: { id: 2389, name: 'Sessions list' },
  devices: [{ device_id: 'd-pixel', name: 'Chrome on Pixel 9', paired_at_ms: Date.now() - 3 * 86_400_000, expires_at_ms: Date.now() + 27 * 86_400_000 + 60_000 }],
}

const old: SessionSummary = {
  session_id: 'rs_EndedSessionCCCCCCCCCC', harness_id: 'h3', harness_fp_display: '41B0-77E2-0C5A-D913', host: 'old-laptop',
  os: 'macos', harness_version: '0.3.9', project: { id: 2, name: 'AMP' }, online: false, viewers: 0,
  started_at_ms: Date.now() - 30 * 3_600_000, expires_at_ms: Date.now() - 18 * 3_600_000, last_activity_at_ms: Date.now() - 26 * 3_600_000,
  protocol_version: 1, state: 'ended', end_reason: 'idle', ended_at_ms: Date.now() - 25 * 3_600_000, model: 'anthropic/claude-opus-5-5',
  ticket: { id: 77, name: null }, devices: [],
}

/** A session as a relay from before #2389 lists it: no state, model, ticket or devices. */
const legacy: SessionSummary = {
  session_id: 'rs_LegacySessionDDDDDDDDD', harness_id: 'h4', harness_fp_display: '0000-1111-2222-3333', host: 'legacy-box',
  os: 'linux', harness_version: '0.4.0', project: null, online: true, viewers: 1,
  started_at_ms: Date.now() - 3_600_000, expires_at_ms: Date.now() + 11 * 3_600_000, last_activity_at_ms: Date.now(), protocol_version: 1,
}

function setup(available = true, others: SessionSummary[] = [{ ...other }]) {
  const relay = new MockRelay({ stepMs: 0, others: others.map(o => ({ ...o })),
    status: { model: 'anthropic/claude-opus-5-5', ticket_id: 2369, ticket_name: 'Remote control M1', devices: [{ device_id: 'd-1', name: 'Safari on iPhone' }] } })
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
    for (const name of ['jacobs-mbp', /Open/]) {
      expect(within(mine).getByRole('link', { name })).toHaveAttribute('href', `/sessions/${MOCK_SESSION_ID}`)
    }
    expect(within(mine).getByText('jacobs-mbp')).toBeInTheDocument()
    expect(within(mine).getByText('Laptop online')).toBeInTheDocument()
    expect(within(mine).getByText('AMP #2')).toBeInTheDocument()
    expect(within(box).getByText('Laptop offline')).toBeInTheDocument()
    expect(within(box).getByText('QA 2365 plan #158')).toBeInTheDocument()
    expect(within(box).getByText('5h ago')).toBeInTheDocument() // started
    expect(within(box).getByText('42m ago')).toBeInTheDocument() // last activity
    expect(within(box).getByText('in 7h 10m')).toBeInTheDocument() // expires: its org set a lifetime
    // #2424: by default a session has no lifetime.
    expect(within(mine).getByText('Expires', { selector: 'dt' }).nextSibling).toHaveTextContent('Never')
    expect(within(box).getByText('linux · harness 0.4.0')).toBeInTheDocument()
    expect(within(box).getByText('8F1D-02AA-71C3-9E40')).toBeInTheDocument()
    expect(screen.queryByText(/Ended sessions aren't listed/)).toBeNull()
    expect(screen.getByText(/Ended sessions stay listed for 7 days/)).toBeInTheDocument()
  })

  it('shows the model, current ticket and paired devices of each session', async () => {
    setup()
    const [mine, box] = await screen.findAllByTestId('session-row')
    expect(within(mine).getByText('anthropic/claude-opus-5-5')).toBeInTheDocument()
    expect(within(mine).getByRole('link', { name: '#2369 Remote control M1' })).toHaveAttribute('href', '/project/2/board?focus=2369')
    expect(within(mine).getByText('Safari on iPhone')).toBeInTheDocument()
    expect(within(box).getByText('openai/gpt-5.5')).toBeInTheDocument()
    expect(within(box).getByRole('link', { name: '#2389 Sessions list' })).toHaveAttribute('href', '/project/158/board?focus=2389')
    expect(within(box).getByText('Chrome on Pixel 9')).toBeInTheDocument()
    expect(within(box).getByText(/in 27 days|in 28 days/)).toBeInTheDocument() // the device's pin expiry
    expect(within(box).getByText('Paired device').closest('div')).toHaveAttribute('title', expect.stringMatching(/^Chrome on Pixel 9: paired .*, trusted until /))
  })

  it('lists ended sessions of the last 7 days below the live ones, with why they ended', async () => {
    setup(true, [{ ...other }, { ...old }])
    await screen.findAllByTestId('session-row')
    const endedList = screen.getByRole('list', { name: 'Ended sessions' })
    const [row] = within(endedList).getAllByTestId('ended-session-row')
    expect(screen.getAllByTestId('session-row')).toHaveLength(2)
    expect(screen.getByText('1 in the last 7 days')).toBeInTheDocument()
    expect(within(row).getByText('old-laptop')).toBeInTheDocument()
    expect(within(row).getByText(/Ended · Idle timeout/)).toBeInTheDocument()
    expect(within(row).getByText(/ended after its idle time without remote activity/)).toBeInTheDocument()
    expect(within(row).getByText('Ended', { selector: 'dt' }).nextSibling).toHaveTextContent('1d ago')
    expect(within(row).getByText('anthropic/claude-opus-5-5')).toBeInTheDocument()
    expect(within(row).getByRole('link', { name: '#77' })).toBeInTheDocument() // a ticket AMP has no name for
    expect(within(row).getByText('None')).toBeInTheDocument() // no paired devices
    // An ended session can't be opened or disconnected.
    expect(within(row).queryByRole('button', { name: /Disconnect/ })).toBeNull()
    expect(within(row).queryByRole('link', { name: /Open/ })).toBeNull()
    expect(within(row).queryByRole('link', { name: 'old-laptop' })).toBeNull()
  })

  it('reads a list from a relay that predates ended sessions', async () => {
    setup(true, [{ ...legacy }])
    const rows = await screen.findAllByTestId('session-row')
    expect(rows).toHaveLength(2)
    const row = rows[1]
    expect(within(row).getByText('legacy-box')).toBeInTheDocument()
    expect(within(row).getByText('Laptop online')).toBeInTheDocument()
    expect(within(row).getAllByText('—')).toHaveLength(3) // model, ticket, devices
    expect(screen.queryByRole('list', { name: 'Ended sessions' })).toBeNull()
  })

  it('disconnects one session after a confirm', async () => {
    const user = userEvent.setup()
    setup()
    const rows = await screen.findAllByTestId('session-row')
    await user.click(within(rows[1]).getByRole('button', { name: /Disconnect/ }))
    await user.click(within(rows[1]).getByRole('button', { name: 'Disconnect now' }))
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1))
    expect(screen.getByRole('status')).toHaveTextContent(/keeps running locally/)
    // It moves to the ended list.
    const [ended] = within(screen.getByRole('list', { name: 'Ended sessions' })).getAllByTestId('ended-session-row')
    expect(within(ended).getByText('build-box')).toBeInTheDocument()
    expect(within(ended).getByText(/Ended · Disconnected/)).toBeInTheDocument()
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
    expect(screen.getAllByTestId('ended-session-row')).toHaveLength(2)
    expect(screen.getAllByText(/Ended · Revoked/)).toHaveLength(2)
    expect(screen.queryByRole('button', { name: /Revoke all/ })).toBeNull()
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
    expect(inTime(now + 6 * 3_600_000 + 59.7 * 60_000, now)).toBe('in 7h 00m')
    expect(inTime(now + 30 * 86_400_000 - 9 * 60_000, now)).toBe('in 30 days')
    expect(inTime(now + 47 * 3_600_000, now)).toBe('in 47h 00m')
  })
})

describe('Sessions page while amp-relay is down', () => {
  it('says remote control is unavailable instead of a raw 503', async () => {
    // Envoy answers for a relay with no replicas: 503, a text body, no relay code.
    server.use(http.get('*/api/sessions', () => new HttpResponse('no healthy upstream', { status: 503, headers: { 'Content-Type': 'text/plain' } })))
    const deps: RemoteDeps = { transport: httpTransport, crypto: mockCrypto(), mock: false }
    render(
      <AuthContext.Provider value={{ me: me(true), loading: false, error: null, isAdmin: false }}>
        <RemoteProvider deps={deps}>
          <MemoryRouter initialEntries={['/sessions']}>
            <Routes><Route element={<AppShell />}><Route path="/sessions" element={<Sessions />} /></Route></Routes>
          </MemoryRouter>
        </RemoteProvider>
      </AuthContext.Provider>,
    )
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent("Remote control is unavailable right now (the relay service isn't running).")
    expect(screen.queryByText(/Request failed/)).toBeNull()
    server.resetHandlers()
  })
})
