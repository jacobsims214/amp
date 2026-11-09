// Teams & orgs > Members (#124): the current user is listed, and inviting
// has a visible, labelled Invite button next to the email and role fields.
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { Orgs } from './Orgs'
import { AuthContext } from '../contexts/auth-context'
import type { Me } from '../types'

let invited: unknown = null
const server = setupServer(
  http.get('*/api/orgs', () => HttpResponse.json({ orgs: [{ id: 1, name: 'Default', slug: 'default', created_by: null, created_at: '2026-01-01T00:00:00Z', my_role: 'admin' }] })),
  http.get('*/api/orgs/1/members', () => HttpResponse.json({ members: [{ user_id: 1, email: 'local-dev@amp.local', display_name: 'Local Dev', role: 'admin', kind: 'human' }] })),
  http.post('*/api/orgs/1/members', async ({ request }) => { invited = await request.json(); return HttpResponse.json({ invited: true }) }),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
afterEach(() => { server.resetHandlers(); invited = null })
afterAll(() => server.close())

const me: Me = { subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: ['admin'], user_id: 1, local_dev: true }

describe('Teams & orgs > Members', () => {
  it('lists you and invites with an explicit Invite button', async () => {
    render(
      <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: true }}>
        <MemoryRouter><Orgs /></MemoryRouter>
      </AuthContext.Provider>,
    )
    const row = (await screen.findByText('local-dev@amp.local', { exact: false })).closest('tr')!
    expect(within(row).getByText('(you)')).toBeInTheDocument()
    expect(screen.queryByText('No members')).toBeNull()

    const form = screen.getByRole('form', { name: 'Invite to Default' })
    const invite = within(form).getByRole('button', { name: 'Invite' })
    expect(invite.className).toContain('amp-btn-primary')
    await userEvent.type(within(form).getByRole('textbox', { name: 'Email to invite' }), 'new@x.io')
    await userEvent.selectOptions(within(form).getByRole('combobox', { name: 'Role' }), 'admin')
    await userEvent.click(invite)
    expect(await screen.findByText(/Invited new@x.io/)).toBeInTheDocument()
    expect(invited).toEqual({ user: 'new@x.io', role: 'admin' })
  })
})
