import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { Orgs } from '../../pages/Orgs'
import { AuthContext } from '../../contexts/auth-context'
import type { Me, Org, OrgRole } from '../../types'

let policy = { enabled: false, idle_timeout_min: 0, max_lifetime_hours: 0, device_pin_days: 30 }
let puts: unknown[] = []
let role: OrgRole = 'admin'
let available = true

const org = (): Org => ({ id: 3, name: 'simstech', slug: 'simstech', created_by: 1, created_at: '2026-10-16T00:00:00Z', my_role: role })

const server = setupServer(
  http.get('*/api/orgs', () => HttpResponse.json({ orgs: [org()] })),
  http.get('*/api/orgs/3/members', () => HttpResponse.json({ members: [] })),
  http.get('*/api/orgs/3/teams', () => HttpResponse.json({ teams: [] })),
  http.get('*/api/orgs/3/remote-control', () => {
    if (!available) return HttpResponse.text('404 page not found', { status: 404 })
    if (role !== 'admin' && role !== 'owner') return HttpResponse.json({ error: 'forbidden', code: 'forbidden' }, { status: 403 })
    return HttpResponse.json(policy)
  }),
  http.put('*/api/orgs/3/remote-control', async ({ request }) => {
    const body = await request.json() as Partial<typeof policy>
    puts.push(body)
    policy = { ...policy, ...body }
    return HttpResponse.json(policy)
  }),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
afterEach(() => { policy = { enabled: false, idle_timeout_min: 0, max_lifetime_hours: 0, device_pin_days: 30 }; puts = []; role = 'admin'; available = true })
afterAll(() => server.close())

const me: Me = { subject: 's', email: 'jsims@example.com', name: 'Jacob', roles: [], user_id: 1001 }

function renderOrgs() {
  return render(
    <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: false }}>
      <MemoryRouter><Orgs /></MemoryRouter>
    </AuthContext.Provider>,
  )
}

describe('Org settings → Allow remote control of harness sessions', () => {
  it('is off by default and an org admin can turn it on and off', async () => {
    const user = userEvent.setup()
    renderOrgs()
    await user.click(await screen.findByRole('button', { name: 'settings' }))
    const box = await screen.findByRole('checkbox', { name: /Allow remote control of harness sessions/ })
    await waitFor(() => expect(box).toBeEnabled())
    expect(box).not.toBeChecked()
    expect(screen.getByText(/Off \(the default\)/)).toBeInTheDocument()

    await user.click(box)
    await waitFor(() => expect(box).toBeChecked())
    expect(puts).toEqual([{ enabled: true }])
    expect(screen.getByText(/ends every live remote session/)).toBeInTheDocument()

    await user.click(box)
    await waitFor(() => expect(box).not.toBeChecked())
    expect(puts).toEqual([{ enabled: true }, { enabled: false }])
  })

  it('has no time limit by default, and an admin can opt into an idle limit and a lifetime (#2424)', async () => {
    const user = userEvent.setup()
    policy = { ...policy, enabled: true }
    renderOrgs()
    await user.click(await screen.findByRole('button', { name: 'settings' }))
    expect(await screen.findByText(/has no time limit: it reconnects by itself after sleep/)).toBeInTheDocument()
    const idle = screen.getByRole('combobox', { name: /no remote activity for/ })
    const life = screen.getByRole('combobox', { name: /this long after it started/ })
    expect(idle).toHaveValue('0')
    expect(life).toHaveValue('0')
    expect(within(idle).getByRole('option', { name: 'No limit (default)' })).toBeInTheDocument()

    await user.selectOptions(idle, '30')
    await waitFor(() => expect(puts).toEqual([{ idle_timeout_min: 30 }]))
    await user.selectOptions(life, '12')
    await waitFor(() => expect(puts).toEqual([{ idle_timeout_min: 30 }, { max_lifetime_hours: 12 }]))
    expect(await screen.findByText(/ends after 30 min without remote activity, or 12 h after it started/)).toBeInTheDocument()

    await user.selectOptions(idle, '0')
    await user.selectOptions(life, '0')
    await waitFor(() => expect(puts).toHaveLength(4))
    expect(puts.slice(2)).toEqual([{ idle_timeout_min: 0 }, { max_lifetime_hours: 0 }])
    expect(await screen.findByText(/has no time limit/)).toBeInTheDocument()
  })

  it('shows an idle limit set through the API that the list doesn\'t have', async () => {
    const user = userEvent.setup()
    policy = { ...policy, enabled: true, idle_timeout_min: 45 }
    renderOrgs()
    await user.click(await screen.findByRole('button', { name: 'settings' }))
    const idle = await screen.findByRole('combobox', { name: /no remote activity for/ })
    expect(idle).toHaveValue('45')
    expect(screen.getByText(/ends after 45 min without remote activity/)).toBeInTheDocument()
  })

  it('is not offered to org members who aren\'t admins', async () => {
    role = 'member'
    renderOrgs()
    await screen.findByRole('heading', { name: 'simstech' })
    const tabs = screen.getByRole('button', { name: 'members' }).parentElement!
    expect(within(tabs).queryByRole('button', { name: 'settings' })).toBeNull()
    expect(screen.queryByRole('checkbox', { name: /Allow remote control/ })).toBeNull()
  })

  it('says so when the server has no remote control yet', async () => {
    const user = userEvent.setup()
    available = false
    renderOrgs()
    await user.click(await screen.findByRole('button', { name: 'settings' }))
    expect(await screen.findByText(/doesn't offer remote control yet/)).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: /Allow remote control/ })).toBeDisabled()
  })
})
