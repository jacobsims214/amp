// Teams & orgs — the page's own behaviour, and its one inbox-action entry point.
//
// Two concerns live here:
//
//  * **Members (#124)**: the current user is listed, and inviting has a visible,
//    labelled Invite button next to the email and role fields.
//  * **Inbox access (#4877)**: a manager opens `RequestAccessPanel` from a team's
//    expanded row. The approved mockup (artifact 112 v1) puts it *inline* in the
//    row, beneath the members, and the action beside the Inbox and KB links. The
//    cases below are this ticket's three criteria:
//
//     1. A team lead **or** an org owner/admin opens the panel from that team's
//        row; a viewer with neither standing sees no action at all. Both ways of
//        being a manager are asserted, because they arrive through different
//        fields — `me.teams[].role` and `me.orgs[].my_role` — and a gate that
//        read only one would pass a test written against the other.
//     2. The panel opens for the **right** team (team B's row opens team B's
//        grants) and closing it collapses only the section: the row stays
//        expanded, and the members and the action are still there. A close that
//        also collapsed the row, or reset the page, would be a page that moves
//        under the reader.
//     3. `npx vitest run src/pages/Orgs.test.tsx` and `npx tsc -b` pass, with a
//        non-manager case — that is the last two tests here.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import { Orgs } from './Orgs'
import { AuthContext } from '../contexts/auth-context'
import type { Me, Org, Team, TeamSubmitGrantee } from '../types'

let invited: unknown = null

/** The org the page lands on, and the two teams in it. */
const ORG: Org = { id: 1, name: 'Default', slug: 'default', created_by: null, created_at: '2026-01-01T00:00:00Z', my_role: 'admin' }
const PLATFORM: Team = { id: 4, org_id: 1, name: 'Platform', created_by: null, created_at: '2026-01-01T00:00:00Z' }
const SUPPORT: Team = { id: 5, org_id: 1, name: 'Support', created_by: null, created_at: '2026-01-01T00:00:00Z' }

const grant = (team: number, over: Partial<TeamSubmitGrantee> = {}): TeamSubmitGrantee => ({
  team_id: team, org_id: 1, user_id: 31, granted_by: 1, granted_at: '2026-11-13T10:00:00Z',
  email: 'sam@outside.example', display_name: 'Sam Rivera', kind: 'customer', ...over,
})

/** Which team ids had their grants read — the fact that proves the *right* panel opened. */
let grantReads: number[] = []

const server = setupServer(
  http.get('*/api/orgs', () => HttpResponse.json({ orgs: [ORG] })),
  http.get('*/api/orgs/1/members', () => HttpResponse.json({ members: [{ user_id: 1, email: 'local-dev@amp.local', display_name: 'Local Dev', role: 'admin', kind: 'human' }] })),
  http.post('*/api/orgs/1/members', async ({ request }) => { invited = await request.json(); return HttpResponse.json({ invited: true }) }),
  http.get('*/api/orgs/1/teams', () => HttpResponse.json({ teams: [PLATFORM, SUPPORT] })),
  http.get('*/api/teams/4', () => HttpResponse.json({ team: PLATFORM, members: [{ user_id: 1, email: 'local-dev@amp.local', display_name: 'Local Dev', role: 'lead' }] })),
  http.get('*/api/teams/5', () => HttpResponse.json({ team: SUPPORT, members: [{ user_id: 9, email: 'other@simstech.cloud', display_name: 'Other', role: 'member' }] })),
  // Each team answers its own grantee, so a panel wired to the wrong id fails on
  // the *content*, not only on the path.
  http.get('*/api/teams/4/request-grants', () => {
    grantReads.push(4)
    return HttpResponse.json({ team_id: 4, grants: [grant(4, { display_name: 'Platform grantee' })] })
  }),
  http.get('*/api/teams/5/request-grants', () => {
    grantReads.push(5)
    return HttpResponse.json({ team_id: 5, grants: [grant(5, { user_id: 44, display_name: 'Support grantee' })] })
  }),
  http.get('*/api/teams/4/request-invites', () => HttpResponse.json({ team_id: 4, invites: [] })),
  http.get('*/api/teams/5/request-invites', () => HttpResponse.json({ team_id: 5, invites: [] })),
  http.get('*/api/users/me/tokens', () => HttpResponse.json({ tokens: [] })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
beforeEach(() => { grantReads = [] })
afterEach(() => { server.resetHandlers(); invited = null })
afterAll(() => server.close())

const localDev: Me = { subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: ['admin'], user_id: 1, local_dev: true }

/** The member fixture's own identity: an ordinary account, no admin anywhere. */
const plainMember: Me = { subject: 's', email: 'plain@simstech.cloud', name: 'Plain Member', roles: [], user_id: 7, orgs: [], teams: [] }

/** A lead of Platform (team 4) but not of Support, and on no org role. */
const platformLead: Me = {
  ...plainMember, user_id: 11, email: 'lead@simstech.cloud', name: 'Platform Lead',
  orgs: [{ ...ORG, my_role: 'member' }],
  teams: [{ id: 4, name: 'Platform', org_id: 1, role: 'lead' }],
}

/** An admin of the org, on neither team: the other way a person is a manager. */
const orgAdmin: Me = {
  ...plainMember, user_id: 12, email: 'admin@simstech.cloud', name: 'Org Admin',
  orgs: [{ ...ORG, my_role: 'admin' }],
  teams: [],
}

const mount = (me: Me, isAdmin = false) => render(
  <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin }}>
    <MemoryRouter><Orgs /></MemoryRouter>
  </AuthContext.Provider>,
)

/** The page opens on Members; the teams live behind the Teams tab. */
const openTeams = async () => {
  await userEvent.click(await screen.findByRole('button', { name: 'teams' }))
  return screen.findByRole('button', { name: `Inbox access for ${PLATFORM.name}` })
}

describe('Teams & orgs > Members', () => {
  it('lists you and invites with an explicit Invite button', async () => {
    render(
      <AuthContext.Provider value={{ me: localDev, loading: false, error: null, isAdmin: true }}>
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

describe('Teams & orgs > Inbox access (AMP #4877)', () => {
  it('a team lead opens the panel from their own team’s row', async () => {
    mount(platformLead)
    const action = await openTeams()

    // The lead of Platform sees the action there...
    expect(action).toBeInTheDocument()
    // ...and not on Support, which they do not lead.
    expect(screen.queryByRole('button', { name: `Inbox access for ${SUPPORT.name}` })).toBeNull()

    await userEvent.click(action)
    const panel = await screen.findByRole('region', { name: /Team inbox · submit access/ })
    // It is the panel, in the row, showing this team's grantee.
    expect(await within(panel).findByText('Platform grantee')).toBeInTheDocument()
    expect(within(panel).queryByText('Support grantee')).toBeNull()
    expect(grantReads).toContain(4)
    expect(grantReads).not.toContain(5)
    // Opening from a collapsed row expanded it, so the section is not hidden.
    expect(action).toHaveAttribute('aria-expanded', 'true')
  })

  it('an org admin who is on neither team still sees the action, and opens that team', async () => {
    mount(orgAdmin)
    const action = await openTeams()
    await userEvent.click(action)

    expect(await screen.findByText('Platform grantee')).toBeInTheDocument()
    expect(grantReads).toEqual([4])

    // The second team's row opens the second team's grants — no stale panel. The
    // page expands one team at a time, so opening Support collapses Platform's
    // row with it: that is the page's existing model, not a state this ticket
    // changes, and what matters is that the panel now speaks for team 5.
    await userEvent.click(screen.getByRole('button', { name: `Inbox access for ${SUPPORT.name}` }))
    expect(await screen.findByText('Support grantee')).toBeInTheDocument()
    expect(screen.queryByText('Platform grantee')).toBeNull()
    expect(grantReads).toEqual([4, 5])
  })

  it('closing the panel collapses only the section: the row, its members and the action stay', async () => {
    mount(orgAdmin)
    const action = await openTeams()
    await userEvent.click(action)

    const panel = await screen.findByRole('region', { name: /Team inbox · submit access/ })
    expect(within(panel).getByText('Platform grantee')).toBeInTheDocument()

    await userEvent.click(within(panel).getByRole('button', { name: 'Close' }))

    // The section is gone...
    await waitFor(() => expect(screen.queryByRole('region', { name: /Team inbox · submit access/ })).toBeNull())
    // ...and nothing else on the page moved: the row is still expanded with its
    // members, and the action is still offered (and now says so).
    expect(screen.getByText('local-dev@amp.local', { exact: false })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: `Inbox access for ${PLATFORM.name}` })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: `Inbox access for ${PLATFORM.name}` })).toHaveAttribute('aria-expanded', 'false')
  })

  it('a viewer with neither standing sees no action — and no dead end', async () => {
    mount(plainMember)
    await userEvent.click(await screen.findByRole('button', { name: 'teams' }))
    // The list itself loaded: this is about the action's absence, not a blank page.
    expect(await screen.findByText(PLATFORM.name)).toBeInTheDocument()
    for (const team of [PLATFORM, SUPPORT]) {
      expect(screen.queryByRole('button', { name: `Inbox access for ${team.name}` })).toBeNull()
    }
    // With no action there is nothing to click, so nothing read this team's grants.
    expect(grantReads).toEqual([])
  })

  it('no action at all for a non-manager, even with the row expanded', async () => {
    mount(plainMember)
    await userEvent.click(await screen.findByRole('button', { name: 'teams' }))
    // Expand the row through its own toggle: the members load, the action still does not exist.
    await userEvent.click(await screen.findByRole('button', { name: new RegExp(PLATFORM.name) }))
    expect(await screen.findByText('local-dev@amp.local', { exact: false })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: `Inbox access for ${PLATFORM.name}` })).toBeNull()
    expect(screen.queryByRole('region', { name: /Team inbox · submit access/ })).toBeNull()
    expect(grantReads).toEqual([])
  })
})
