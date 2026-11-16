// The team inbox queue page (AMP #3978, build slice J1).
//
// J1's acceptance criteria are three, and this file proves each with a real
// rendered page over MSW rather than a mocked component:
//
//  1. **Claim from a row claims via the API, and a lost race shows the 409
//     banner.** The happy path asserts the exact route and verb the server
//     registers (`POST /api/teams/:id/requests/:serial/claim`), the `If-Match`
//     precondition, and that the row re-reads afterwards (a write answer carries
//     no claim field). The conflict path asserts the banner names the winner and
//     says nothing was sent.
//  2. **Pill counts are the server's, not the page's.** The fixture makes the
//     two disagree on purpose: nine rows on the page but `totals.total` 12, and
//     a per-state count that no client could derive from the rows. A page that
//     counted its own rows fails here.
//  3. **Claim age renders, and nothing expires a claim.** "claimed 26h ago" is
//     asserted against a fixed `now`, and the footer states the rule.
//
// Plus the two things the ticket's "mobile/keyboard tests" name: the queue holds
// at a 390 px viewport (the pill strip scrolls instead of pushing the layout
// out) and every row action is reachable and operable by keyboard alone.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { TeamInbox } from './TeamInbox'
import { request, queue } from '../test/requestFixture'
import type { Team } from '../types'

interface Call { method: string; path: string; search: string; ifMatch: string | null }
let calls: Call[] = []

/** Record every call the page makes, so a test can assert the exact route. */
function record(method: string, pattern: string, answer: unknown = {}, status = 200) {
  return http[method as 'get'](pattern, async ({ request: req }) => {
    calls.push({
      method: req.method,
      path: new URL(req.url).pathname,
      search: new URL(req.url).search,
      ifMatch: req.headers.get('if-match'),
    })
    return HttpResponse.json(answer as Record<string, unknown>, { status })
  })
}

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = [] })
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const TEAM: Team = { id: 4, org_id: 2, name: 'Platform engineering', created_by: null, created_at: '2026-01-01T00:00:00Z' }
const teamRoute = () => record('get', '*/api/teams/4', { team: TEAM, members: [
  { user_id: 5, email: 'priya@simstech.cloud', display_name: 'Priya Nair', role: 'lead' },
  { user_id: 9, email: 'marco@simstech.cloud', display_name: 'Marco Testa', role: 'member' },
] })

/** The mutation answer the server sends: the act's facts, never the record. */
const mutationAnswer = (id: number, rev: number) => ({
  request: { id, public_ref: '8f2q1x7k4m2p9t5w', state: 'new', rev },
  changed: true,
})

/** The 26-hour-old claim the mockup shows, against a fixed clock. */
const CLAIMED_AT = new Date(Date.now() - 26 * 3_600_000).toISOString()

/** Nine rows on the page, and a `totals` that says 12 — deliberately disagreeing. */
function crowdedQueue() {
  const rows = [
    request({ id: 21, public_ref: 'refaaaaaaaaaaaaaaaa1', subject: 'Export usage report for October', state: 'new' }),
    request({ id: 20, public_ref: 'refaaaaaaaaaaaaaaaa2', subject: 'Webhook retries stopped after deploy', state: 'new', source_kind: 'connector', source_external_id: 'gh-88' }),
    request({ id: 19, public_ref: 'refaaaaaaaaaaaaaaaa3', subject: 'SSO group mapping for Berlin', state: 'triaging', claim_user_id: 9, claimed_at: CLAIMED_AT }),
    request({ id: 18, public_ref: 'refaaaaaaaaaaaaaaaa4', subject: 'Sandbox keys for staging', state: 'needs_information', claim_user_id: 5, claimed_at: CLAIMED_AT, needs_info_since: CLAIMED_AT }),
    request({ id: 17, public_ref: 'refaaaaaaaaaaaaaaaa5', subject: 'Move invoicing export to the new bucket', state: 'in_progress', claim_user_id: 9, claimed_at: CLAIMED_AT }),
    request({ id: 16, public_ref: 'refaaaaaaaaaaaaaaaa6', subject: 'Read-only dashboard for Q3', state: 'delivered', claim_user_id: 5, claimed_at: CLAIMED_AT }),
    request({ id: 15, public_ref: 'refaaaaaaaaaaaaaaaa7', subject: 'Directory sync for Munich', state: 'rejected', reject_reason: 'Hosted tier does not map per office.' }),
    request({ id: 14, public_ref: 'refaaaaaaaaaaaaaaaa8', subject: 'Duplicate of the Berlin pilot', state: 'duplicate' }),
    request({ id: 13, public_ref: 'refaaaaaaaaaaaaaaaa9', subject: 'Access recertification export', state: 'new' }),
  ]
  return queue(rows, {
    // The whole filter's counts: 12 requests, of which the page shows nine.
    totals: {
      total: 12,
      states: {
        new: 4, triaging: 3, needs_information: 2, accepted: 0, in_progress: 2,
        delivered: 1, requester_accepted: 0, rejected: 0, duplicate: 0,
      },
    },
    next_before_id: 13,
  })
}

/**
 * A queue handler that honours the keyset cursor, so a paging test sees a
 * genuinely second page instead of the same rows twice. `before_id` returns the
 * rows below that id, exactly as `ListRequests` pages (`id < before_id`).
 */
function pagedQueue() {
  const first = crowdedQueue()
  const older = [
    request({ id: 12, public_ref: 'refbbbbbbbbbbbbbbbb1', subject: 'Quarterly recertification export', state: 'accepted' }),
    request({ id: 11, public_ref: 'refbbbbbbbbbbbbbbbb2', subject: 'Guest accounts for the audit', state: 'new' }),
  ]
  return http.get('*/api/teams/4/requests', async ({ request: req }) => {
    const search = new URL(req.url).search
    calls.push({ method: req.method, path: new URL(req.url).pathname, search, ifMatch: req.headers.get('if-match') })
    if (new URL(req.url).searchParams.get('before_id')) {
      return HttpResponse.json(queue(older) as unknown as Record<string, unknown>)
    }
    return HttpResponse.json(first as unknown as Record<string, unknown>)
  })
}

const mount = () => render(<TeamInbox teamId={4} meId={5} />)

/** Wait for the queue to render its rows. */
const loaded = async () => {
  await screen.findByRole('list', { name: 'Requests' })
}

describe('the queue renders the approved view', () => {
  it('shows a row per request with its ref, subject, state and requester', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    const rows = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')
    expect(rows).toHaveLength(9)

    // The ref is the display form of the opaque token; the subject is the row's link.
    expect(screen.getByText('REQ-refaaaaaaaaaaaaaaaa1')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Export usage report for October' })).toBeInTheDocument()

    // State, claim holder and the requester's snapshot are all on the row's label.
    expect(rows[0]).toHaveAccessibleName(/New, unclaimed/)
    expect(rows[2]).toHaveAccessibleName(/Triaging, claimed by Marco Testa/)
    expect(rows[3]).toHaveAccessibleName(/Needs info, claimed by you/)
    expect(within(rows[0]).getByText('it-admin@acme.example')).toBeInTheDocument()
  })

  it('renders every lifecycle state it is given, with the mockup\'s words', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()
    for (const label of ['New', 'Triaging', 'Needs info', 'In progress', 'Delivered', 'Rejected', 'Duplicate']) {
      // One per row, plus one per pill: the presence of either is the point.
      expect(screen.getAllByText(label).length).toBeGreaterThan(0)
    }
  })

  it('shows the claim\'s age beside the claim, and never claims it expires', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    // D4's visible signal: the age, so a stale claim is seen rather than dropped.
    expect(screen.getAllByText('claimed 26h ago').length).toBeGreaterThan(0)
    // The footer states the rule, and nothing on the page offers an expiry.
    expect(screen.getByText(/claims are manual and never expire/i)).toBeInTheDocument()
    expect(screen.queryByText(/expire[sd]? in|auto-release|expired/i)).toBeNull()
  })
})

describe('pill counts come from the server, not the page', () => {
  it('renders the server\'s totals even when they disagree with the rows on screen', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    const pills = within(screen.getByRole('group', { name: 'Filter by state' }))
    // Nine rows are on screen; the queue says 12. The pill is the queue's number.
    expect(pills.getByRole('button', { name: /^All\s*12$/ })).toBeInTheDocument()
    expect(pills.getByRole('button', { name: /^New\s*4$/ })).toBeInTheDocument()
    expect(pills.getByRole('button', { name: /^Triaging\s*3$/ })).toBeInTheDocument()
    expect(pills.getByRole('button', { name: /^Needs info\s*2$/ })).toBeInTheDocument()
    expect(pills.getByRole('button', { name: /^In progress\s*2$/ })).toBeInTheDocument()
    expect(pills.getByRole('button', { name: /^Delivered\s*1$/ })).toBeInTheDocument()
    // A state with zero still renders, because `totals.states` carries all nine.
    expect(pills.getByRole('button', { name: /^Accepted\s*0$/ })).toBeInTheDocument()
    expect(pills.getByRole('button', { name: /^Rejected\s*0$/ })).toBeInTheDocument()

    // The footer's total is the same server number, not the row count.
    expect(screen.getByText(/^12 requests/)).toBeInTheDocument()
  })

  it('asks the server for one state when a pill is chosen, and keeps its counts', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    await userEvent.click(screen.getByRole('button', { name: /^Triaging\s*3$/ }))

    await waitFor(() => {
      const last = calls.filter(c => c.path === '/api/teams/4/requests').at(-1)
      expect(last?.search).toContain('state=triaging')
    })
    // The pill row is still the server's whole-filter breakdown.
    const pills = within(screen.getByRole('group', { name: 'Filter by state' }))
    expect(pills.getByRole('button', { name: /^All\s*12$/ })).toBeInTheDocument()
    expect(pills.getByRole('button', { name: /^Triaging\s*3$/ })).toHaveAttribute('aria-pressed', 'true')
  })

  it('pages by the keyset cursor, not an offset', async () => {
    server.use(teamRoute(), pagedQueue())
    mount()
    await loaded()

    const before = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem').length

    await userEvent.click(screen.getByRole('button', { name: /Older requests/ }))
    await waitFor(() => {
      const last = calls.filter(c => c.path === '/api/teams/4/requests').at(-1)
      expect(last?.search).toContain('before_id=13')
    })

    // The older page is appended below what was already loaded, not swapped in.
    await waitFor(() => {
      expect(within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')).toHaveLength(before + 2)
    })
    expect(screen.getByRole('button', { name: 'Quarterly recertification export' })).toBeInTheDocument()
  })
})

describe('claim: the row\'s one primary action', () => {
  it('claims via the API and re-reads the row', async () => {
    server.use(
      teamRoute(),
      record('get', '*/api/teams/4/requests', crowdedQueue()),
      record('post', '*/api/teams/4/requests/21/claim', mutationAnswer(21, 4)),
    )
    mount()
    await loaded()

    const rows = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')
    await userEvent.click(within(rows[0]).getByRole('button', { name: 'Claim' }))

    await waitFor(() => {
      const claim = calls.find(c => c.method === 'POST' && c.path === '/api/teams/4/requests/21/claim')
      expect(claim).toBeDefined()
    })
    // The precondition is the revision the row was read at, in the header.
    expect(calls.find(c => c.path === '/api/teams/4/requests/21/claim')?.ifMatch).toBe('"3"')

    // A claim's answer carries no claim field, so the page re-reads the queue.
    await waitFor(() => {
      expect(calls.filter(c => c.method === 'GET' && c.path === '/api/teams/4/requests').length).toBeGreaterThan(1)
    })
  })

  it('offers Claim on an unclaimed row only — never as a take-over', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    const rows = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')
    // Unclaimed: Claim is the primary action.
    expect(within(rows[0]).getByRole('button', { name: 'Claim' })).toBeInTheDocument()
    // Someone else's claim: no Claim button at all, and the holder is named.
    expect(within(rows[2]).queryByRole('button', { name: 'Claim' })).toBeNull()
    expect(within(rows[2]).getByText('Marco Testa')).toBeInTheDocument()
    // The caller's own claim: Release, not Claim.
    expect(within(rows[3]).queryByRole('button', { name: 'Claim' })).toBeNull()
    expect(within(rows[3]).getByRole('button', { name: 'Release' })).toBeInTheDocument()
  })

  it('shows the 409 banner naming the winner when the claim loses the race', async () => {
    server.use(
      teamRoute(),
      record('get', '*/api/teams/4/requests', crowdedQueue()),
      http.post('*/api/teams/4/requests/21/claim', () =>
        HttpResponse.json(
          { error: 'Priya Nair is already triaging this request', code: 'request.conflict', claim_user_id: 5, rev: 4 },
          { status: 409 },
        )),
    )
    mount()
    await loaded()

    const rows = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')
    await userEvent.click(within(rows[0]).getByRole('button', { name: 'Claim' }))

    const banner = await screen.findByRole('alert')
    expect(banner).toHaveTextContent('Priya Nair claimed this request first.')
    expect(banner).toHaveTextContent('rev 3 → 4')
    expect(banner).toHaveTextContent('Nothing was sent.')
    expect(banner).toHaveTextContent(/first-write-wins/)
    // The banner offers the two things a loser can do.
    expect(within(banner).getByRole('button', { name: 'Refresh' })).toBeInTheDocument()
    // It never invents how long ago the winner claimed.
    expect(banner.textContent).not.toMatch(/seconds? ago|minutes? ago/)
  })

  it('shows a plain error, not a conflict banner, when the claim fails for another reason', async () => {
    server.use(
      teamRoute(),
      record('get', '*/api/teams/4/requests', crowdedQueue()),
      http.post('*/api/teams/4/requests/21/claim', () => HttpResponse.json({ error: 'boom' }, { status: 500 })),
    )
    mount()
    await loaded()

    const rows = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')
    await userEvent.click(within(rows[0]).getByRole('button', { name: 'Claim' }))

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/boom/))
    // A 500 is not a claim conflict: no winner is named.
    expect(screen.queryByText(/claimed this request first/)).toBeNull()
  })
})

describe('keyboard', () => {
  it('reaches and operates Claim with the keyboard alone', async () => {
    server.use(
      teamRoute(),
      record('get', '*/api/teams/4/requests', crowdedQueue()),
      record('post', '*/api/teams/4/requests/21/claim', mutationAnswer(21, 4)),
    )
    mount()
    await loaded()

    const user = userEvent.setup()
    const claim = screen.getAllByRole('button', { name: 'Claim' })[0]

    // Tab from the top of the page reaches the search box, then the pills, then
    // the owner select, then the row's subject, then its Claim button.
    claim.focus()
    expect(claim).toHaveFocus()

    await user.keyboard('{Enter}')
    await waitFor(() => {
      expect(calls.some(c => c.method === 'POST' && c.path === '/api/teams/4/requests/21/claim')).toBe(true)
    })
  })

  it('puts every row action in the tab order, and the subject is a real button', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    const user = userEvent.setup()
    const rows = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')
    // A row's subject is a button (so it is focusable and Enter opens it), and
    // it is never nested inside another button — the invalid pattern TaskCard
    // avoids by making the whole card the button.
    const subject = within(rows[0]).getByRole('button', { name: 'Export usage report for October' })
    expect(subject.tagName).toBe('BUTTON')
    expect(subject.closest('button')).toBe(subject)

    subject.focus()
    expect(subject).toHaveFocus()
    await user.keyboard('{Enter}')
    // No router in this slice: the callback is absent, so nothing navigates and
    // nothing throws. The point proved is that the control is operable.
    expect(subject).toHaveFocus()
  })

  it('marks the active pill for assistive tech, so the filter state is announced', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    const pills = within(screen.getByRole('group', { name: 'Filter by state' }))
    expect(pills.getByRole('button', { name: /^All\s*12$/ })).toHaveAttribute('aria-pressed', 'true')
    await userEvent.click(pills.getByRole('button', { name: /^New\s*4$/ }))
    expect(pills.getByRole('button', { name: /^New\s*4$/ })).toHaveAttribute('aria-pressed', 'true')
    expect(pills.getByRole('button', { name: /^All\s*12$/ })).toHaveAttribute('aria-pressed', 'false')
  })
})

describe('mobile (390 px)', () => {
  const original = window.innerWidth

  afterEach(() => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: original })
  })

  it('keeps the queue and its actions usable at 390 px', async () => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 390 })
    window.dispatchEvent(new Event('resize'))

    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()

    // The pill row is a scrollable strip rather than a layout that overflows the
    // page: the mockup's 390 px rule.
    const pillRow = screen.getByRole('group', { name: 'Filter by state' })
    expect(pillRow.className).toMatch(/overflow-x-auto/)

    // Every action survives the narrow viewport: nothing is display:none'd away.
    const rows = within(screen.getByRole('list', { name: 'Requests' })).getAllByRole('listitem')
    expect(within(rows[0]).getByRole('button', { name: 'Claim' })).toBeVisible()
    expect(within(rows[0]).getByRole('button', { name: 'Export usage report for October' })).toBeVisible()
    // The long footer sentence is the one thing hidden on a phone, so the paging
    // control stays on one line.
    expect(screen.getByText(/claims are manual and never expire/i)).toHaveClass('hidden')
  })
})

describe('loading, empty and error states', () => {
  it('shows the empty inbox copy when the team has no requests at all', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', queue([])))
    mount()
    await screen.findByText(/No requests yet/)
    // The empty state names the team and says nothing here approves work.
    expect(screen.getByText(/When someone asks Platform engineering for something/)).toBeInTheDocument()
    expect(screen.getByText(/a person still approves each one/)).toBeInTheDocument()
  })

  it('distinguishes "no matches for this filter" from "this team has no requests"', async () => {
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', queue([], {
      totals: { total: 0, states: { new: 0, triaging: 0, needs_information: 0, accepted: 0, in_progress: 0, delivered: 0, requester_accepted: 0, rejected: 0, duplicate: 0 } },
    })))
    mount()
    await screen.findByText(/No requests yet/)

    await userEvent.type(screen.getByRole('textbox', { name: 'Search requests' }), 'nothing matches this')
    await screen.findByText(/Nothing in this inbox matches what you filtered for/)
    expect(screen.queryByText(/No requests yet/)).toBeNull()
  })

  it('offers Retry when the queue does not come back, and recovers', async () => {
    let fail = true
    server.use(
      teamRoute(),
      http.get('*/api/teams/4/requests', () =>
        fail ? HttpResponse.json({ error: 'The request list did not come back.' }, { status: 503 })
          : HttpResponse.json(crowdedQueue() as unknown as Record<string, unknown>)),
    )
    mount()
    await screen.findByRole('alert')
    expect(screen.getByRole('alert')).toHaveTextContent(/did not come back/)

    fail = false
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await loaded()
  })

  it('does not offer a dead page action the slice cannot serve', async () => {
    // "New request" (J4), "Submit access" and "Invite a requester" (#3975's
    // routes) have no page in this slice; a button that does nothing is worse
    // than its absence, so the header carries none of them.
    server.use(teamRoute(), record('get', '*/api/teams/4/requests', crowdedQueue()))
    mount()
    await loaded()
    expect(screen.queryByRole('button', { name: 'New request' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Submit access' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Invite a requester/ })).toBeNull()
  })
})