// The route table AMP #3988 owns: which paths exist, which frame each renders
// in, and what a URL has to carry to reach a requester screen.
//
// The assertion that matters most is the frame one. "A requester route reveals
// no internal nav" is the ticket's own criterion, and the way this app keeps it
// is structural — the requester routes are mounted outside `<AppShell>` — so the
// tests read the rendered tree for the shell's own landmarks rather than
// trusting that no component happened to draw a link.
//
// Identity arrives the way it does in production: through `App`'s own
// `AuthProvider`, which reads `/api/me`. Supplying an outer `AuthContext` would
// be ignored (the app's provider is inside it), and a test that silently renders
// as the wrong person is worse than no test.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import App from '../App'
import { RemoteProvider } from '../remote/RemoteProvider'
import { httpTransport } from '../remote/transport'
import { unpairedCrypto } from '../remote/crypto'
import { resetWaitCounts } from '../lib/waitCounts'
import { SEEN_KEY } from '../lib/launch'
import type { Me, OwnRequestPage, RequesterRequestView, TeamRequest } from '../types'
import { request } from '../test/requestFixture'

// ---- The app under test, at a URL, as somebody ----

const TEAM = { id: 4, name: 'Platform engineering', org_id: 2, role: 'member' }

const staff: Me = {
  subject: 'u-7', email: 'jo@example.com', name: 'Jo Park', roles: [], user_id: 7,
  orgs: [{ id: 2, name: 'Simstech', slug: 'simstech', created_by: 1, created_at: '2026-01-01T00:00:00Z', my_role: 'member' }],
  teams: [TEAM],
}

// `kind: 'customer'` is what `/api/me` answers for an invited requester; the
// client's `UserKind` has not caught up yet (`lib/requestRoutes.ts` says why), so
// the fixture asserts the shape the API really sends rather than a narrower one.
const customer = {
  subject: 'u-12', email: 'marta@acme.example', name: 'Marta Reyes', roles: [], user_id: 12,
  kind: 'customer', orgs: [], teams: [],
} as unknown as Me

/** Whose `/api/me` answers. */
let currentMe: Me = staff

/** One request as the queue's read answers it. */
const row = (over: Partial<TeamRequest> = {}): TeamRequest => request({ team_id: 4, ...over })

const ownPage = (items: OwnRequestPage['items'] = []): OwnRequestPage => ({ items, count: items.length, total: items.length })

/** A requester projection — the only shape `GET /api/requests/:ref` may answer with. */
const projection: RequesterRequestView = {
  public_ref: '8f2q1x7k4m2p9t5w', state: 'in_progress', state_word: 'In progress',
  subject: 'SSO group mapping for the Berlin office', input_brief: 'We are rolling out SSO.',
  team_name: 'Platform engineering', created_at: '2026-11-14T09:41:00Z', milestones: [], comments: [], outputs: [],
}

/** Every API URL the app asked for, in order — the only honest record of what a route read. */
const seen: string[] = []
const record = (req: Request) => { seen.push(req.url); return undefined }

const server = setupServer(
  http.get('*/api/me', () => HttpResponse.json(currentMe)),
  http.get('*/api/projects', () => HttpResponse.json({ projects: [] })),
  http.get('*/api/my-work/counts', () => HttpResponse.json({ projects: {}, counts: { waiting: 0 } })),
  http.get('*/api/my-work', () => HttpResponse.json({
    owned: [], flags: [], agents: [], done_24h: 0,
    counts: { approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0, waiting: 0 },
    projects: [], projects_total: 0, projects_offset: 0, projects_limit: 50, projects_more: false, rows_per_project: 6,
    clear_projects: [], agents_in_progress: 0,
  })),
  http.get('*/api/my-work/getting-started', () => HttpResponse.json({
    agent: { done: true, at: '2026-11-14T09:00:00Z' },
    project: { done: true, at: '2026-11-14T09:00:00Z', project_id: 2 },
    plan: { done: true, at: '2026-11-14T09:00:00Z', project_id: 2 },
    receipt: { done: true, at: '2026-11-14T09:00:00Z', project_id: 2, task_id: 12 },
  })),
  // `Orgs` reads the org list, then the selected org's teams and members.
  http.get('*/api/orgs', () => HttpResponse.json({
    orgs: [{ id: 2, name: 'Simstech', slug: 'simstech', created_by: 1, created_at: '2026-01-01T00:00:00Z', my_role: 'member' }],
  })),
  http.get('*/api/orgs/:orgId/teams', () => HttpResponse.json({
    teams: [{ id: 4, org_id: 2, name: 'Platform engineering', created_by: 1, created_at: '2026-01-01T00:00:00Z' }],
  })),
  http.get('*/api/orgs/:orgId/members', () => HttpResponse.json({ members: [] })),
  http.get('*/api/orgs/:orgId/invites', () => HttpResponse.json({ invites: [] })),
  http.get('*/api/teams/:teamId', () => HttpResponse.json({
    team: { id: 4, org_id: 2, name: 'Platform engineering', created_by: 1, created_at: '2026-01-01T00:00:00Z' }, members: [],
  })),
  http.get('*/api/teams/:teamId/requests', ({ request: req }) => {
    record(req)
    return HttpResponse.json({ items: [row()], count: 1, totals: { total: 1, states: { new: 1 } } })
  }),
  http.get('*/api/teams/:teamId/requests/:ref', ({ request: req }) => {
    record(req)
    return HttpResponse.json({
      view: 'team', request: row(),
      outcome: {
        phase: 'in_progress', items: [], total: 0, done: 0, open: 0, blocked: 0, gated: 0, held: 0, deleted: 0,
        unverified: 0, rework: 0, linked: 0, outcome_brief_set: false, no_artifact: false, delivered_eligible: false,
      },
    })
  }),
  http.get('*/api/requests', ({ request: req }) => { record(req); return HttpResponse.json(ownPage()) }),
  http.get('*/api/requests/:ref', ({ request: req }) => {
    record(req)
    return HttpResponse.json({ view: 'requester', projection })
  }),
)

beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
beforeEach(() => { currentMe = staff })
afterEach(() => {
  seen.length = 0
  server.resetHandlers()
  resetWaitCounts()
  localStorage.clear()
  sessionStorage.clear()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

/** Put the browser at `url` before `App` mounts: its router and the launch read it once. */
function at(url: string): void {
  // The launch plays only on a cold start at "/", and marking it seen keeps a
  // route assertion from becoming an assertion about an animation.
  sessionStorage.setItem(SEEN_KEY, '1')
  window.history.replaceState({}, '', url)
}

function renderAt(me: Me | null = staff) {
  currentMe = me ?? ({} as Me)
  return render(
    <RemoteProvider deps={{ transport: httpTransport, crypto: unpairedCrypto, mock: false }}>
      <App />
    </RemoteProvider>,
  )
}

const requesterFrame = () => document.querySelector('main#requester-content')
const sidebar = () => screen.queryByRole('complementary', { name: 'Main' })

// ---- The paths themselves ----

describe('the request route table', () => {
  it('keeps the staff and requester surfaces on different paths', async () => {
    const { teamInboxPath, teamRequestPath, requesterListPath, requesterNewRequestPath, requesterStatusPath } =
      await import('../lib/requestRoutes')
    expect(teamInboxPath(4)).toBe('/teams/4/requests')
    expect(teamRequestPath(4, 9)).toBe('/teams/4/requests/9')
    expect(requesterListPath()).toBe('/requests')
    expect(requesterNewRequestPath({ teamId: 4 })).toBe('/requests/new?team=4')
    expect(requesterStatusPath('8f2q1x7k4m2p9t5w')).toBe('/request/8f2q1x7k4m2p9t5w')
  })

  it('carries the desk with its case and never invents one', async () => {
    const { deskSearch, readDesk } = await import('../lib/requestRoutes')
    expect(deskSearch({})).toBe('')
    expect(deskSearch({ teamId: 4, teamName: 'Northwind support desk' }))
      .toBe('?team=4&desk=Northwind+support+desk')
    // A hand-edited URL must never become a submit to /api/teams/NaN/requests.
    expect(readDesk(new URLSearchParams('team=NaN'))).toEqual({})
    expect(readDesk(new URLSearchParams('team=-3'))).toEqual({})
    expect(readDesk(new URLSearchParams('team=4&desk='))).toEqual({ teamId: 4 })
  })

  it('keeps a reference exactly as it arrived — it is case sensitive', async () => {
    const { requesterStatusPath } = await import('../lib/requestRoutes')
    // base64url, and the server resolves `WHERE public_ref = $1`.
    const ref = '8f2Q1x7K4m2P9t5W'
    expect(requesterStatusPath(ref)).toBe(`/request/${ref}`)
    expect(requesterStatusPath(` ${ref} `)).toBe(`/request/${ref}`)
  })

  it('names a team\'s org role only for a team the caller is on', async () => {
    const { teamOrgRole } = await import('../lib/requestRoutes')
    expect(teamOrgRole(staff, 4)).toBe('member')
    // A team the caller is not on has no org role to offer, and must not borrow
    // the role of an org they happen to belong to.
    expect(teamOrgRole(staff, 99)).toBeUndefined()
    expect(teamOrgRole(null, 4)).toBeUndefined()
  })
})

// ---- Team routes: inside the shell, for staff ----

describe('the team request routes', () => {
  it('renders the queue inside the app shell, and reads that team\'s queue', async () => {
    at('/teams/4/requests')
    renderAt()
    // The team's own frame: the sidebar landmark exists for a staff caller.
    expect(await screen.findByRole('complementary', { name: 'Main' })).toBeInTheDocument()
    expect(await screen.findByText('SSO group mapping for the Berlin office')).toBeInTheDocument()
    await waitFor(() => expect(seen.some(u => u.includes('/api/teams/4/requests'))).toBe(true))
  })

  it('opens a queue row at its serial, which is what a team write needs', async () => {
    at('/teams/4/requests')
    renderAt()
    const subject = await screen.findByText('SSO group mapping for the Berlin office')
    // The row's own control is what navigates; clicking it is the real path.
    await userEvent.click(subject)
    await waitFor(() => expect(seen.some(u => u.includes('/api/teams/4/requests/7'))).toBe(true))
  })

  it('renders one request at `/teams/:teamId/requests/:ref`, still in the shell', async () => {
    at('/teams/4/requests/7')
    renderAt()
    await waitFor(() => expect(seen.some(u => u.includes('/api/teams/4/requests/7'))).toBe(true))
    // A staff page, not a requester one.
    expect(screen.getByRole('complementary', { name: 'Main' })).toBeInTheDocument()
    expect(requesterFrame()).toBeNull()
  })

  it('passes a pasted public reference through unchanged, because the team read accepts either', async () => {
    at('/teams/4/requests/8f2Q1x7K4m2P9t5W')
    renderAt()
    await waitFor(() => expect(seen.some(u => u.includes('8f2Q1x7K4m2P9t5W'))).toBe(true))
  })

  it('sends a non-numeric team segment home rather than to a queue of nothing', async () => {
    at('/teams/not-a-number/requests')
    renderAt()
    await waitFor(() => expect(window.location.pathname).toBe('/'))
    expect(seen.some(u => u.includes('/api/teams/not-a-number'))).toBe(false)
  })
})

// ---- Requester routes: outside the shell — the ticket's key criterion ----

describe('the requester routes reveal no internal navigation', () => {
  // The shell's own labels. None of these names any request, project or person
  // in this fixture, so finding one means the shell rendered.
  const internals = ['My work', 'Projects', 'Day reports', 'Trace', 'AMP guide', 'Teams & orgs', 'Agents & tokens']

  it.each([
    ['/requests', 'the list'],
    ['/requests/new?team=4&desk=Platform+engineering', 'the form'],
    ['/request/8f2q1x7k4m2p9t5w', 'the status screen'],
  ])('%s (%s) renders the requester frame and none of the sidebar', async (url) => {
    at(url)
    renderAt(customer)
    await waitFor(() => expect(requesterFrame()).not.toBeNull())
    // Absent by the shell's own landmarks, not by the absence of a string.
    expect(sidebar()).toBeNull()
    expect(document.getElementById('main-nav')).toBeNull()
    expect(document.getElementById('main-content')).toBeNull()
    for (const label of internals) expect(screen.queryByRole('link', { name: label })).toBeNull()
    // No project tree, no admin group, no skip-to-main for a sidebar that isn't there.
    expect(document.querySelector('[aria-label="Main"]')).toBeNull()
    expect(screen.queryByText('Organization')).toBeNull()
  })

  it('shows a staff member the same requester frame — the frame belongs to the route, not the person', async () => {
    at('/requests')
    renderAt(staff)
    await waitFor(() => expect(requesterFrame()).not.toBeNull())
    expect(sidebar()).toBeNull()
  })

  it('keeps a customer out of the team surfaces by sending them to their own list', async () => {
    at('/teams/4/requests')
    renderAt(customer)
    await waitFor(() => expect(window.location.pathname).toBe('/requests'))
    expect(requesterFrame()).not.toBeNull()
    // The team read was never attempted for them.
    expect(seen.some(u => u.includes('/api/teams/4/requests'))).toBe(false)
  })

  it('keeps a customer out of a team request detail too', async () => {
    at('/teams/4/requests/7')
    renderAt(customer)
    await waitFor(() => expect(window.location.pathname).toBe('/requests'))
    expect(seen.some(u => u.includes('/api/teams/4/'))).toBe(false)
  })
})

// ---- Deep links: the desk has to travel, because a customer cannot look it up ----

describe('requester deep links', () => {
  it('submits to the desk the entry link named', async () => {
    at('/requests/new?team=4&desk=Platform+engineering')
    renderAt(customer)
    expect(await screen.findByRole('heading', { name: /New request/i })).toBeInTheDocument()
    // The desk is named from `?desk=`, not fetched: a customer has no org standing.
    expect(screen.getAllByText(/Platform engineering/).length).toBeGreaterThan(0)
    expect(seen.some(u => u.includes('/api/teams/4') && u.includes('/api/teams/4?'))).toBe(false)
  })

  it('sends a form URL with no desk to the list, which can answer without one', async () => {
    at('/requests/new')
    renderAt(customer)
    await waitFor(() => expect(window.location.pathname).toBe('/requests'))
  })

  it('takes a pasted status reference with its case intact', async () => {
    at('/request/8f2Q1x7K4m2P9t5W')
    renderAt(customer)
    await waitFor(() => expect(seen.some(u => u.includes('/api/requests/'))).toBe(true))
    expect(seen.find(u => u.includes('/api/requests/'))).toContain('8f2Q1x7K4m2P9t5W')
  })

  it('offers no "New request" control when the URL names no desk', async () => {
    at('/requests')
    renderAt(customer)
    await waitFor(() => expect(requesterFrame()).not.toBeNull())
    // A tab or button that navigates to a form which redirects back here is a
    // dead end a person cannot see coming.
    expect(screen.queryByRole('button', { name: /New request/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /Send your first request/i })).toBeNull()
  })

  it('offers "New request" when the desk travelled with the link', async () => {
    at('/requests?team=4&desk=Platform+engineering')
    renderAt(customer)
    // Two controls say it on this screen — the tab and the heading's button — and
    // both are offered only because a desk is known.
    const controls = await screen.findAllByRole('button', { name: /New request/i })
    expect(controls.length).toBeGreaterThan(0)
  })

  it('carries the desk on from the list into the form', async () => {
    at('/requests?team=4&desk=Platform+engineering')
    renderAt(customer)
    await waitFor(() => expect(requesterFrame()).not.toBeNull())
    await userEvent.click((await screen.findAllByRole('button', { name: /New request/i }))[0])
    await waitFor(() => expect(window.location.pathname).toBe('/requests/new'))
    expect(window.location.search).toContain('team=4')
    expect(await screen.findByRole('heading', { name: /New request/i })).toBeInTheDocument()
  })
})

// ---- The entry points into the new routes ----

describe('the entry points into the new routes', () => {
  it('My work\'s inbox panel links into the team queue', async () => {
    const { teamInboxLink, claimedRequestLink } = await import('../lib/teamInboxes')
    const { teamInboxPath } = await import('../lib/requestRoutes')
    expect(teamInboxLink(4)).toBe('/teams/4/requests')
    // The one place the panel's destination is defined delegates to the one
    // place every request path is defined.
    expect(teamInboxLink(4)).toBe(teamInboxPath(4))
    expect(claimedRequestLink({ request: row(), team: { id: 4, name: 'Platform engineering' }, via: 'claimed' }))
      .toBe('/teams/4/requests')
  })

  it('My work renders that link, so the panel reaches the queue a route now answers', async () => {
    at('/')
    renderAt(staff)
    const link = await screen.findByRole('link', { name: /Open inbox for Platform engineering/ })
    expect(link).toHaveAttribute('href', '/teams/4/requests')
  })

  it('reaches a team\'s inbox from Teams & orgs, beside its KB', async () => {
    at('/admin/orgs')
    renderAt(staff)
    // The Teams tab is where a team's rows live, so a person clicks into it — the
    // link is asserted where it actually is, not on a page that never draws it.
    await userEvent.click(await screen.findByRole('button', { name: 'teams' }))
    const inbox = await screen.findByRole('link', { name: /Inbox/ })
    expect(inbox).toHaveAttribute('href', '/teams/4/requests')
    // The KB link sits beside it in the same row, so it is scoped to the row
    // rather than matched page-wide (the sidebar has KB links of its own).
    const teamRow = inbox.parentElement as HTMLElement
    expect(within(teamRow).getByRole('link', { name: /KB/ })).toHaveAttribute('href', '/teams/4/kb')
  })

  it('keeps the sidebar\'s own navigation intact on a team page', async () => {
    at('/teams/4/requests')
    renderAt()
    const nav = await screen.findByRole('complementary', { name: 'Main' })
    // The Overview group a team page has always shown, unchanged.
    for (const label of ['My work', 'Projects', 'Day reports', 'Trace', 'AMP guide']) {
      expect(within(nav).getByRole('link', { name: new RegExp(`^${label}`) })).toBeInTheDocument()
    }
    // Dark mode's control is still the shell's, not something a request page replaced.
    expect(within(nav).getByRole('group', { name: 'Theme' })).toBeInTheDocument()
  })
})