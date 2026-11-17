// "Submit access" — the inbox's manager-only grant, invitation and
// submit-only-credential screen (AMP #3982).
//
// The three acceptance criteria are one `describe` each:
//
//  1. **Manager-only, and the flow works.** A team lead grants, revokes, invites,
//     withdraws and mints through the exact routes the server registers; every
//     act is asserted against the method, path and body, so a control wired to a
//     neighbouring route or a body that grows a role fails here. A viewer who is
//     not a manager gets no control at all and one sentence saying who can.
//  2. **Submit access is never membership.** The grant body is `{user}` and
//     nothing else — the route refuses a role, so a body that carried one would
//     be a 400 rather than access. The copy is asserted too, because a screen
//     that granted the right thing while saying "add to team" would be the same
//     bug in the words instead of the payload.
//  3. **The secret is not retained or read back.** The minted secret renders
//     inside `SecretReveal`, and once dismissed it is gone from the DOM, absent
//     from the list the panel re-reads, and absent from every storage the page
//     could reach. The list shows the server's prefix, which is the only trace
//     of a credential AMP keeps.
//
// #4878 adds the requester entry link above the three sections, tested in its own
// `describe`: the URL comes from `requesterNewRequestPath` (both the team id and
// the desk name, since a requester's own projection carries neither the id nor a
// team they can look up), the copy reports success *and* failure, and the copy
// says that nothing is emailed — the invitation route mails nobody either, so
// the link is the delivery.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { RequestAccessPanel } from './RequestAccessPanel'
import { requesterNewRequestPath } from '../../lib/requestRoutes'
import type { APIToken, Invite, Me, NewAPIToken, Team, TeamSubmitGrantee } from '../../types'

const TEAM: Team = { id: 4, org_id: 2, name: 'Platform engineering', created_by: 11, created_at: '2026-10-01T09:00:00Z' }

const grant = (over: Partial<TeamSubmitGrantee> = {}): TeamSubmitGrantee => ({
  team_id: 4, org_id: 2, user_id: 31, granted_by: 11, granted_at: '2026-11-13T10:00:00Z',
  email: 'sam@outside.example', display_name: 'Sam Rivera', kind: 'customer', ...over,
})

const invite = (over: Partial<Invite> = {}): Invite => ({
  id: 8, email: 'dana@outside.example', org_id: 2, org_role: 'member', team_id: 4, team_role: 'member',
  invited_by: 11, created_at: '2026-11-13T12:00:00Z', accepted_at: null, ...over,
})

/** A live submit-only credential of this inbox — the shape `GET /users/me/tokens` answers (no secret). */
const credential = (over: Partial<APIToken> = {}): APIToken => ({
  id: 41, user_id: 11, name: 'support-portal connector', prefix: 'amp_ab12cd', scope: 'submit', scope_team_id: 4,
  created_at: '2026-11-13T09:00:00Z', expires_at: '2027-02-11T09:00:00Z', ...over,
})

/** The mint's answer: the stored row plus the secret, which no later read carries. */
const minted = (over: Partial<NewAPIToken> = {}): NewAPIToken => ({ ...credential(), token: SECRET, ...over })

const SECRET = 'amp_ab12cd_SECRET_THAT_MUST_NOT_SURVIVE'

/** A team lead: a lead of team 4, so `canManageSubmitAccess` passes without an org role. */
const lead: Me = {
  subject: 'dex|me', email: 'me@simstech.cloud', name: 'Jacob Sims', roles: [], user_id: 11,
  orgs: [{ id: 2, name: 'simstech', slug: 'simstech', created_by: 11, created_at: '2026-10-01T09:00:00Z', my_role: 'member' }],
  teams: [{ id: 4, name: 'Platform engineering', org_id: 2, role: 'lead' }],
  kind: 'human',
}

/** A plain team member of another role: on the team, on no org role, not a lead. */
const member: Me = { ...lead, teams: [{ id: 4, name: 'Platform engineering', org_id: 2, role: 'member' }] }

interface Call { method: string; path: string; body: unknown }
let calls: Call[] = []

/** The panel's reads: grants, invitations, the caller's tokens, and (when not passed in) the team. */
function reads(opts: { grants?: TeamSubmitGrantee[]; invites?: Invite[]; tokens?: APIToken[] } = {}) {
  return [
    http.get('*/api/teams/4/request-grants', () => HttpResponse.json({ team_id: 4, grants: opts.grants ?? [grant()] })),
    http.get('*/api/teams/4/request-invites', () => HttpResponse.json({ team_id: 4, invites: opts.invites ?? [invite()] })),
    http.get('*/api/users/me/tokens', () => HttpResponse.json({ tokens: opts.tokens ?? [credential()] })),
  ]
}

const record = async (request: Request) => {
  const text = await request.text()
  calls.push({ method: request.method, path: new URL(request.url).pathname, body: text ? JSON.parse(text) : undefined })
}

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => {
  calls = []
  localStorage.clear()
  sessionStorage.clear()
  server.use(...reads())
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const mount = (over: Partial<React.ComponentProps<typeof RequestAccessPanel>> = {}) =>
  render(<RequestAccessPanel teamId={4} team={TEAM} me={lead} meId={11} {...over} />)

describe('manager-only: the flow works, and only for a manager', () => {
  it('grants submit access by email, and re-reads the list', async () => {
    server.use(
      http.post('*/api/teams/4/request-grants', async ({ request }) => {
        await record(request)
        return HttpResponse.json({
          team_id: 4, grant: { team_id: 4, org_id: 2, user_id: 55, granted_at: '2026-11-14T10:00:00Z' }, active: true,
        })
      }),
      ...reads({ grants: [grant(), grant({ user_id: 55, email: 'new@outside.example', display_name: '' })] }),
    )
    mount()
    const input = await screen.findByLabelText('Give someone submit access')
    await userEvent.type(input, 'new@outside.example')
    await userEvent.click(screen.getByRole('button', { name: 'Grant submit access' }))

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toEqual({ method: 'POST', path: '/api/teams/4/request-grants', body: { user: 'new@outside.example' } })
    // The confirmation says what was granted and what was not.
    expect(await screen.findByTestId('submit-access-notice')).toHaveTextContent(
      /can now send requests to this inbox\. They are not on the team and reach no project\./,
    )
  })

  it('revokes only after the second click, then names what stays readable', async () => {
    server.use(
      http.delete('*/api/teams/4/request-grants/31', async ({ request }) => {
        await record(request)
        return HttpResponse.json({
          team_id: 4, grant: grant({ revoked_at: '2026-11-14T10:05:00Z', revoked_by: 11 }), active: false,
        })
      }),
    )
    mount()
    const row = await screen.findByRole('listitem', { name: '' }).catch(() => null)
    // The row is identified by the person it names, not by an aria-label.
    const person = await screen.findByText('Sam Rivera')
    const item = person.closest('li') as HTMLElement
    expect(item).toHaveAttribute('data-state', 'active')
    expect(row).toBeNull() // no empty-aria listitem: each row names its own contents

    await userEvent.click(within(item).getByRole('button', { name: 'Revoke' }))
    // Armed, not acted: nothing has been sent.
    expect(calls).toHaveLength(0)
    await userEvent.click(within(item).getByRole('button', { name: 'Revoke submit access' }))

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].method).toBe('DELETE')
    expect(calls[0].path).toBe('/api/teams/4/request-grants/31')
    expect(await screen.findByTestId('submit-access-notice')).toHaveTextContent(
      /submit access is revoked: their very next request is refused\. Requests they already sent stay readable\./,
    )
  })

  it('invites a requester and withdraws a pending invitation through the team routes', async () => {
    server.use(
      http.post('*/api/teams/4/request-invites', async ({ request }) => {
        await record(request)
        return HttpResponse.json({ team_id: 4, invite: invite({ id: 9, email: 'new@outside.example' }) }, { status: 201 })
      }),
      http.delete('*/api/teams/4/request-invites/8', async ({ request }) => {
        await record(request)
        return HttpResponse.json({ team_id: 4, invite_id: 8, revoked: true })
      }),
    )
    mount()
    await userEvent.type(await screen.findByLabelText('Invite a requester'), 'new@outside.example')
    await userEvent.click(screen.getByRole('button', { name: 'Invite requester' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toEqual({ method: 'POST', path: '/api/teams/4/request-invites', body: { email: 'new@outside.example' } })
    expect(await screen.findByTestId('submit-access-notice')).toHaveTextContent(
      /They get a customer account at first sign-in, with no org or team role\./,
    )

    // The pending invitation is the row naming that email; withdraw is confirmed in place.
    const inviteRow = (await screen.findByText('dana@outside.example')).closest('li') as HTMLElement
    await userEvent.click(within(inviteRow).getByRole('button', { name: 'Withdraw' }))
    await userEvent.click(within(inviteRow).getByRole('button', { name: 'Withdraw invitation' }))
    await waitFor(() => expect(calls.at(-1)).toEqual({ method: 'DELETE', path: '/api/teams/4/request-invites/8', body: undefined }))
  })

  it('gives a non-manager no control at all, and says who can instead', async () => {
    mount({ me: member })
    expect(await screen.findByTestId('submit-access-managers-only')).toHaveTextContent(
      /Only this team’s leads and its org’s owners and admins can change who may submit/,
    )
    expect(screen.queryByLabelText('Give someone submit access')).toBeNull()
    expect(screen.queryByLabelText('Invite a requester')).toBeNull()
    expect(screen.queryByLabelText('Mint a submit-only credential')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Revoke' })).toBeNull()
    // The standings are still readable: "no permission" is not "no information".
    expect(await screen.findByText('Sam Rivera')).toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })

  it('treats an org owner as a manager of any team in that org', async () => {
    const owner: Me = {
      ...lead,
      teams: [],
      orgs: [{ id: 2, name: 'simstech', slug: 'simstech', created_by: 11, created_at: '2026-10-01T09:00:00Z', my_role: 'owner' }],
    }
    mount({ me: owner })
    expect(await screen.findByLabelText('Give someone submit access')).toBeInTheDocument()
    expect(screen.queryByTestId('submit-access-managers-only')).toBeNull()
  })

  it('states a refused write in the server’s own words, and keeps the list', async () => {
    server.use(
      http.post('*/api/teams/4/request-grants', () =>
        HttpResponse.json({ error: 'no AMP user with email "nobody@outside.example" (they must sign in once, or be invited)' }, { status: 404 })),
    )
    mount()
    await userEvent.type(await screen.findByLabelText('Give someone submit access'), 'nobody@outside.example')
    await userEvent.click(screen.getByRole('button', { name: 'Grant submit access' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('no AMP user with email "nobody@outside.example"')
    // The list it could not change is still on screen: a failed write is not a failed read.
    expect(screen.getByText('Sam Rivera')).toBeInTheDocument()
  })
})

describe('submit access is never membership', () => {
  it('sends the person and nothing else — the route refuses a role', async () => {
    server.use(
      http.post('*/api/teams/4/request-grants', async ({ request }) => {
        await record(request)
        return HttpResponse.json({ team_id: 4, grant: grant(), active: true })
      }),
    )
    mount()
    await userEvent.type(await screen.findByLabelText('Give someone submit access'), 'sam@outside.example')
    await userEvent.click(screen.getByRole('button', { name: 'Grant submit access' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    // Exactly one key. A `role`, `team_role`, `org_role`, `project_id` or `scope`
    // here would be refused by name rather than applied — the body never has one.
    expect(Object.keys(calls[0].body as object)).toEqual(['user'])
  })

  it('says in words that sending is not joining, above every control', async () => {
    mount()
    // The sentence lives in one span under a bold lead-in, so the assertion reads
    // the whole rule rather than the two words of its heading.
    expect(await screen.findByText(/never team membership: a requester reaches no project, reads no ticket and holds no role/))
      .toBeInTheDocument()
    expect(screen.getByText('Sending is not joining.')).toBeInTheDocument()
    // The invitation says the same thing about the customer identity it creates.
    expect(await screen.findByText(/creates a customer identity at their first sign-in and grants no org or team role/)).toBeInTheDocument()
    // And the credential's reach is one operation, spelled out.
    expect(screen.getByText(/no reads, no lists, no access administration, no approvals/)).toBeInTheDocument()
    // No control anywhere on the screen offers team membership: that is
    // `/api/teams/{id}/members`, a different screen and a different route.
    expect(screen.queryByRole('button', { name: /add to team|make (them|.*) a member|set role/i })).toBeNull()
  })

  it('never offers a role select or a project on either form', async () => {
    mount()
    await screen.findByLabelText('Give someone submit access')
    expect(screen.queryByRole('combobox')).toBeNull()
    expect(screen.queryByLabelText(/role/i)).toBeNull()
    expect(screen.queryByLabelText(/project/i)).toBeNull()
  })
})

describe('the secret is shown once and never retained', () => {
  it('mints a submit-scoped credential for this inbox and shows the secret once', async () => {
    server.use(
      http.post('*/api/users/me/tokens', async ({ request }) => {
        await record(request)
        return HttpResponse.json({ token: minted({ name: 'portal connector' }) })
      }),
    )
    mount()
    await userEvent.type(await screen.findByLabelText('Mint a submit-only credential'), 'portal connector')
    await userEvent.click(screen.getByRole('button', { name: 'Mint credential' }))

    await waitFor(() => expect(calls).toHaveLength(1))
    // The scope is what makes it narrow, and it names this inbox.
    expect(calls[0]).toEqual({
      method: 'POST',
      path: '/api/users/me/tokens',
      body: { name: 'portal connector', expires_in_days: 90, scope: 'submit', scope_team_id: 4 },
    })
    // Shown once, in the app's one secret component, with the once-only note.
    expect(await screen.findByText(SECRET)).toBeInTheDocument()
    expect(screen.getByText(/AMP keeps only a hash, so it can't be shown again/)).toBeInTheDocument()

    // No storage of any kind: the secret lives in React state and nowhere else.
    expect(JSON.stringify(localStorage)).not.toContain(SECRET)
    expect(JSON.stringify(sessionStorage)).not.toContain(SECRET)
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })

  it('drops the secret when dismissed, and lists only the prefix afterwards', async () => {
    server.use(
      http.post('*/api/users/me/tokens', async ({ request }) => {
        await record(request)
        return HttpResponse.json({ token: minted({ name: 'portal connector' }) })
      }),
      // The re-read answers the stored row: prefix only, never a secret.
      ...reads({ tokens: [credential({ id: 42, name: 'portal connector', prefix: 'amp_zz99yy' })] }),
    )
    mount()
    await userEvent.type(await screen.findByLabelText('Mint a submit-only credential'), 'portal connector')
    await userEvent.click(screen.getByRole('button', { name: 'Mint credential' }))
    await screen.findByText(SECRET)

    await userEvent.click(screen.getByRole('button', { name: "I've copied it" }))

    // Gone from the page entirely — not hidden, not in a title, not in an aria-label.
    expect(screen.queryByText(SECRET)).toBeNull()
    expect(document.body.innerHTML).not.toContain(SECRET)
    // What remains is the prefix the server keeps, on the credential it names.
    const row = (await screen.findByText('portal connector')).closest('li') as HTMLElement
    expect(within(row).getByText('amp_zz99yy…')).toBeInTheDocument()
    expect(row.textContent).not.toContain(SECRET)
  })

  it('closing the panel drops the secret too', async () => {
    server.use(
      http.post('*/api/users/me/tokens', async ({ request }) => {
        await record(request)
        return HttpResponse.json({ token: minted() })
      }),
    )
    const onClose = vi.fn()
    mount({ onClose })
    await userEvent.type(await screen.findByLabelText('Mint a submit-only credential'), 'portal connector')
    await userEvent.click(screen.getByRole('button', { name: 'Mint credential' }))
    await screen.findByText(SECRET)
    await userEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalled()
    expect(screen.queryByText(SECRET)).toBeNull()
  })

  it('lists only credentials bound to this inbox, never a full-scope or another team’s token', async () => {
    server.use(...reads({
      tokens: [
        credential({ id: 42, name: 'this inbox connector', prefix: 'amp_this' }),
        credential({ id: 43, name: 'full scope token', prefix: 'amp_full', scope: 'full', scope_team_id: null }),
        credential({ id: 44, name: 'another inbox connector', prefix: 'amp_other', scope_team_id: 9 }),
        credential({ id: 45, name: 'revoked connector', prefix: 'amp_rev', revoked_at: '2026-11-14T09:00:00Z' }),
      ],
    }))
    mount()
    const row = await screen.findByText('this inbox connector')
    expect(row).toBeInTheDocument()
    expect(screen.queryByText('full scope token')).toBeNull()
    expect(screen.queryByText('another inbox connector')).toBeNull()
    expect(screen.queryByText('revoked connector')).toBeNull()
  })

  it('revokes a credential through the token route', async () => {
    server.use(
      http.delete('*/api/tokens/41', async ({ request }) => {
        await record(request)
        return HttpResponse.json({ revoked: 41 })
      }),
    )
    mount()
    const row = (await screen.findByText('support-portal connector')).closest('li') as HTMLElement
    await userEvent.click(within(row).getByRole('button', { name: 'Revoke' }))
    await userEvent.click(within(row).getByRole('button', { name: 'Revoke credential' }))
    await waitFor(() => expect(calls).toEqual([{ method: 'DELETE', path: '/api/tokens/41', body: undefined }]))
  })
})

describe('the requester entry link is how a manager hands out the way in', () => {
  it('builds its own team’s link from requesterNewRequestPath, carrying the team id and the desk name', async () => {
    mount()
    const code = await screen.findByTestId('requester-link-url')
    // Asserted against the builder itself, not against a string spelled here: a
    // panel that assembled `?team=…&desk=…` by hand would still have to agree
    // with this, including how the name is encoded.
    expect(code).toHaveTextContent(`${window.location.origin}${requesterNewRequestPath({ teamId: 4, teamName: TEAM.name })}`)
    // The two parameters, each for its own reason: the id is what `POST
    // /api/teams/:id/requests` needs, the name is the only desk fact a
    // requester's own projection carries (`lib/requestRoutes.ts` §2).
    const url = new URL(code.textContent ?? '')
    expect(url.pathname).toBe('/requests/new')
    expect(url.searchParams.get('team')).toBe('4')
    expect(url.searchParams.get('desk')).toBe('Platform engineering')
  })

  it('reports a successful copy, and clears the report so a second copy is possible', async () => {
    const user = userEvent.setup()
    mount()
    const url = `${window.location.origin}${requesterNewRequestPath({ teamId: 4, teamName: TEAM.name })}`
    await user.click(await screen.findByRole('button', { name: 'Copy the requester link' }))
    expect(await screen.findByTestId('requester-link-status')).toHaveTextContent(/^Link copied\. Send it to them/)
    // `userEvent.setup()` installs its own clipboard stub, so read it back rather
    // than spying on a write the stub would have replaced. The status line only
    // appears once the write resolved, so this read cannot race it.
    expect(await navigator.clipboard.readText()).toBe(url)
  })

  it('says the clipboard refused instead of claiming it copied', async () => {
    const user = userEvent.setup()
    mount()
    const deny = vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValueOnce(new Error('denied'))
    await user.click(await screen.findByRole('button', { name: 'Copy the requester link' }))
    expect(await screen.findByTestId('requester-link-status'))
      .toHaveTextContent(/refused the clipboard — select the link above and copy it/i)
    // A refusal is not a failed action: the link is still on screen, selectable.
    expect(screen.getByTestId('requester-link-url')).toBeInTheDocument()
    deny.mockRestore()
  })

  it('says nothing is emailed, and that this link is what the requester needs', async () => {
    mount()
    const note = await screen.findByTestId('requester-link-note')
    expect(note).toHaveTextContent(/Nothing is emailed/)
    expect(note).toHaveTextContent(/this link is what the requester needs/)
    // And it does not sell the link as an account: an invitation is still needed.
    expect(note).toHaveTextContent(/still has to be invited below first/)
  })

  it('offers no link to a viewer who may not manage this inbox', async () => {
    mount({ me: member })
    await screen.findByTestId('submit-access-managers-only')
    expect(screen.queryByTestId('requester-link-url')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Copy the requester link' })).toBeNull()
  })
})

describe('an empty submit says what is missing', () => {
  // design/ux-patterns.md, "Forms": disable a button only while saving. A
  // disabled submit on an empty field refuses to explain itself, so each form
  // lets the click happen and answers with the field's own error.
  it('answers an empty grant submit with the field, not a dead button', async () => {
    mount()
    const submit = await screen.findByRole('button', { name: 'Grant submit access' })
    expect(submit).toBeEnabled()
    await userEvent.click(submit)
    expect(await screen.findByText('Enter their email address or AMP user id.')).toBeInTheDocument()
    expect(screen.getByLabelText('Give someone submit access')).toHaveAttribute('aria-invalid', 'true')
    expect(calls).toHaveLength(0) // nothing was sent
    // Typing clears the error rather than making the user re-submit to find out.
    await userEvent.type(screen.getByLabelText('Give someone submit access'), 'a')
    expect(screen.queryByText('Enter their email address or AMP user id.')).toBeNull()
  })

  it('answers an empty invite and an empty mint the same way', async () => {
    mount()
    await userEvent.click(await screen.findByRole('button', { name: 'Invite requester' }))
    expect(await screen.findByText('Enter the email address to invite.')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Mint credential' }))
    expect(await screen.findByText(/Name where the credential will be used/)).toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })
})

describe('the lists answer the manager’s question', () => {
  it('puts grants in force ahead of revoked ones, and marks the revoked one', async () => {
    server.use(...reads({
      grants: [
        grant({ user_id: 40, email: 'gone@outside.example', display_name: 'Gone Person', revoked_at: '2026-11-12T10:00:00Z', revoked_by: 11 }),
        grant({ user_id: 31, email: 'sam@outside.example', display_name: 'Sam Rivera' }),
      ],
    }))
    mount()
    // The grants list is the region headed "Can send"; its rows are the ones that
    // carry a grant state (the invitation and credential lists are their own).
    const grants = await screen.findByRole('region', { name: /Can send/ })
    const rows = within(grants).getAllByRole('listitem')
    expect(rows.map(r => r.getAttribute('data-state'))).toEqual(['active', 'revoked'])
    // The tag names the revocation and when it happened, in one word-phrase: a
    // bare "revoked" leaves a manager asking "since when?".
    expect(within(rows[1]).getByText(/^revoked /)).toBeInTheDocument()
    // A revoked row offers no second revoke; it is history, not an act.
    expect(within(rows[1]).queryByRole('button', { name: 'Revoke' })).toBeNull()
    expect(screen.getByRole('heading', { name: /Can send · 1 person/ })).toBeInTheDocument()
  })

  it('says nobody has a grant yet without implying nobody can send', async () => {
    server.use(...reads({ grants: [] }))
    mount()
    expect(await screen.findByTestId('no-submit-grants')).toHaveTextContent(
      'Nobody has a submit grant on this inbox. Team members can still send requests — being on the team is enough.',
    )
    expect(screen.getByRole('heading', { name: /Can send · nobody yet/ })).toBeInTheDocument()
  })

  it('keeps a failed read as an error with a retry, never as an empty screen', async () => {
    server.use(http.get('*/api/teams/4/request-grants', () => HttpResponse.json({ error: 'boom' }, { status: 500 })))
    mount()
    expect(await screen.findByRole('alert')).toHaveTextContent(/boom/)
    expect(screen.queryByTestId('no-submit-grants')).toBeNull()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument()
  })
})
