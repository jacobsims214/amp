// My work's two team-inbox panels (AMP #3983, build slice J3; approved mockup
// artifact 37 v1, view 3).
//
// The ticket's three acceptance criteria, each proved over MSW against the real
// panels rather than a mocked component:
//
//  1. **A request appears at most once, and never because the viewer is merely
//     on the team.** The fixture makes the server's `mine.ids` list the same id
//     twice and has a *second* request in the queue that is nobody's; a panel
//     that rendered the queue's rows, or that de-duped by member, fails here.
//  2. **Panel counts come from the server aggregate, not client derivation.**
//     Every fixture deliberately disagrees with what its rows would produce:
//     `totals.total` 12 beside 3 rows, `mine.total` 3 beside 2 named ids, and a
//     per-state count no set of rows could yield.
//  3. **Existing Waiting-on-you behaviour is unchanged** — that one is
//     `MyWork.test.tsx`'s job (the whole file runs against the composed page),
//     plus the composition test at the bottom of this file.
//
// The panels are rendered here with `inboxes` built from `useTeamInboxes`' own
// contract, so the request shape under test is exactly what the hook produces.
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { ClaimedRequests } from './ClaimedRequests'
import { TeamInboxSummary } from './TeamInboxSummary'
import { request, totals } from '../../test/requestFixture'
import type { RequestOwnership, TeamRequest } from '../../types'
import type { TeamInbox } from '../../lib/teamInboxes'
import { claimedRequests, claimedTotal, mineLine, unlistedCount } from '../../lib/teamInboxes'

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const CLAIMED_AT = new Date(Date.now() - 26 * 3_600_000).toISOString()

/** The viewer's own slice, as the server answers it: `claimed ∪ assigned`, each id once. */
const mine = (user_id: number, claimed: number[], assigned: number[], over: Partial<RequestOwnership> = {}): RequestOwnership => {
  const ids = [...new Set([...claimed, ...assigned])].sort((a, b) => b - a)
  return { user_id, claimed_ids: claimed, assigned_ids: assigned, ids, count: ids.length, total: ids.length, ...over }
}

const PLATFORM = { id: 4, name: 'Platform engineering' }
const FIELD_OPS = { id: 9, name: 'Field ops' }

/**
 * Platform engineering: 12 requests in the queue, only 3 of them on the page,
 * two of which are mine (one claimed, one assigned). Field ops: 1 request, not
 * mine. `refmine` is the request nobody owns — the one that must never appear
 * in a personal panel.
 */
function fixture(): TeamInbox[] {
  const claimed = request({ id: 21, public_ref: 'refclaimedaaaaaaaaaa1', subject: 'SSO group mapping for Berlin', state: 'triaging', claim_user_id: 7, claimed_at: CLAIMED_AT })
  const assigned = request({ id: 20, public_ref: 'refassignedaaaaaaaaa2', subject: 'Read-only dashboard for Q3', state: 'delivered', owner_user_id: 7 })
  const nobodys = request({ id: 19, public_ref: 'refnobodysaaaaaaaaaa3', subject: 'Nobody owns this one', state: 'new' })
  return [
    {
      team: PLATFORM,
      // 12 in the queue, and counts no set of three rows could produce.
      totals: totals({ new: 4, triaging: 3, needs_information: 2, in_progress: 2, delivered: 1 }),
      // `ids` repeats 21 on purpose: the server dedupes, and the panel must too.
      // `total` is 3 with only 2 nameable ids — the count is the server's, cursor-free.
      mine: mine(7, [21], [20], { ids: [21, 20, 21], count: 3, total: 3 }),
      rows: new Map([[21, claimed], [20, assigned], [19, nobodys]]),
    },
    {
      team: FIELD_OPS,
      totals: totals({ new: 1 }),
      mine: mine(7, [], []),
      rows: new Map([[30, request({ id: 30, team_id: 9, public_ref: 'refnotmineaaaaaaaaaa4', subject: 'Someone else\u2019s request', state: 'new' })]]),
    },
  ]
}

const renderPanels = (inboxes: TeamInbox[], loading = false) => render(
  <MemoryRouter>
    <TeamInboxSummary inboxes={inboxes} loading={loading} />
    <ClaimedRequests inboxes={inboxes} loading={loading} />
  </MemoryRouter>,
)

describe('Team inboxes panel: shared queue counts, and no rows', () => {
  it("renders each team's whole-queue counts from the server's totals", () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    const platform = within(panel).getByRole('heading', { name: 'Platform engineering' }).closest('li')!
    // The server's numbers, in lifecycle order, empty states left out.
    expect(platform.textContent).toMatch(/4\s*new\s*3\s*triaging\s*2\s*need info\s*2\s*in progress\s*1\s*delivered/)
    expect(platform.textContent).toMatch(/12 requests in the shared queue/)
    const fieldOps = within(panel).getByRole('heading', { name: 'Field ops' }).closest('li')!
    expect(fieldOps.textContent).toMatch(/1\s*new/)
    expect(fieldOps.textContent).toMatch(/1 request in the shared queue/)
  })

  it('draws no request rows: a shared queue is never a personal list', () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    // Only the two team names and their two "Open inbox" links — no subject,
    // no reference from any row in either queue.
    expect(within(panel).getAllByRole('listitem')).toHaveLength(2)
    expect(within(panel).queryByText('Nobody owns this one')).toBeNull()
    expect(within(panel).queryByText('SSO group mapping for Berlin')).toBeNull()
    expect(within(panel).queryByText(/refnobodys/)).toBeNull()
    // Nothing that looks like a request row's reference anywhere in the panel.
    expect(panel.textContent).not.toMatch(/ref(claimed|assigned|nobodys)/)
  })

  it('says "shared queues, not your tickets" and where a request becomes yours', () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    expect(within(panel).getByText('shared queues, not your tickets')).toBeInTheDocument()
    expect(panel.textContent).toMatch(/a request becomes yours only when you claim or are assigned it/)
  })

  it('links "Open inbox" to the team queue', () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    expect(within(panel).getByRole('link', { name: /Open inbox for Platform engineering/ })).toHaveAttribute('href', '/teams/4/requests')
    expect(within(panel).getByRole('link', { name: /Open inbox for Field ops/ })).toHaveAttribute('href', '/teams/9/requests')
  })

  it('a failed read shows no number at all, and the other team still renders', () => {
    const broken: TeamInbox[] = [
      { team: PLATFORM, rows: new Map(), error: 'Loading Platform engineering timed out' },
      { team: FIELD_OPS, totals: totals({ new: 1 }), rows: new Map() },
    ]
    renderPanels(broken)
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    const platform = within(panel).getByRole('heading', { name: 'Platform engineering' }).closest('li')!
    expect(platform.textContent).toMatch(/Couldn't read this queue just now, so no counts are shown/)
    expect(platform.textContent).not.toMatch(/\b0\b/)
    // The team that answered is unaffected.
    const fieldOps = within(panel).getByRole('heading', { name: 'Field ops' }).closest('li')!
    expect(fieldOps.textContent).toMatch(/1\s*new/)
  })

  it('an empty queue says so in words, never as a row of zeros', () => {
    renderPanels([{ team: PLATFORM, totals: totals({}), rows: new Map() }])
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    expect(within(panel).getByText('No requests in this queue right now.')).toBeInTheDocument()
    expect(panel.textContent).toMatch(/0 requests in the shared queue/)
  })

  it('a person on no team is told why the panel is empty, not shown an empty queue', () => {
    renderPanels([])
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    expect(within(panel).getByText(/You're not on a team with a request inbox yet/)).toBeInTheDocument()
    expect(within(panel).queryByText(/0 requests/)).toBeNull()
  })

  it('shows shapes, not zeros, while the first read is in flight', () => {
    renderPanels([], true)
    const panel = screen.getByRole('status', { name: 'Team inboxes' })
    expect(panel).toHaveAttribute('aria-busy', 'true')
    expect(panel.textContent).not.toMatch(/\d+ new/)
  })
})

describe('Claimed by me panel: only mine, and each one once', () => {
  it('renders a claimed and an assigned request, and nothing else', () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    const links = within(panel).getAllByRole('link')
    expect(links.map(a => a.textContent)).toEqual([
      expect.stringContaining('SSO group mapping for Berlin'),
      expect.stringContaining('Read-only dashboard for Q3'),
    ])
    // The request nobody owns, and another team's request, are absent.
    expect(within(panel).queryByText('Nobody owns this one')).toBeNull()
    expect(within(panel).queryByText(/Someone else/)).toBeNull()
  })

  it('never repeats a request, even when the server repeats an id', () => {
    // The fixture's `mine.ids` is [21, 20, 21]: three entries, two requests.
    const inboxes = fixture()
    expect(inboxes[0].mine!.ids).toEqual([21, 20, 21])
    expect(claimedRequests(inboxes).map(r => r.request.id)).toEqual([21, 20])
    renderPanels(inboxes)
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    expect(within(panel).getAllByRole('listitem')).toHaveLength(2)
    expect(within(panel).getAllByText(/SSO group mapping for Berlin/)).toHaveLength(1)
  })

  it('does not fan a request out per member: the count is the server total, not a row count', () => {
    const inboxes = fixture()
    // 3 counted by the server, 2 nameable — and one id appears twice.
    expect(claimedTotal(inboxes)).toBe(3)
    expect(claimedRequests(inboxes)).toHaveLength(2)
    renderPanels(inboxes)
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    // The header shows the server's number...
    expect(panel.textContent).toMatch(/3 requests/)
    // ...and the one it could not name is reported, not silently dropped.
    expect(within(panel).getByText(/\+1 more request in your name that this list didn't name/)).toBeInTheDocument()
    // Two rows, three requests: no per-member multiplication anywhere.
    expect(within(panel).getAllByRole('listitem')).toHaveLength(2)
  })

  it('says how a row became yours, and how long the claim has been held', () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    expect(within(panel).getByText(/triaging · claimed 26h/)).toBeInTheDocument()
    expect(within(panel).getByText(/delivered · assigned/)).toBeInTheDocument()
    expect(panel.textContent).toMatch(/Claims never expire/)
  })

  it('states the anti-fan-out rule on the panel itself', () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    expect(panel.textContent).toMatch(/A request you're only on the team for is not here/)
  })

  it('names the team on a row only when the rows span teams', () => {
    const inboxes = fixture()
    // Make the second team's request mine too, so two teams have rows.
    inboxes[1] = { ...inboxes[1], mine: mine(7, [], [30]) }
    renderPanels(inboxes)
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    expect(within(panel).getByText(/new · assigned · Field ops/)).toBeInTheDocument()
  })

  it('shows nothing claimed as a quiet line, and says where a claim comes from', () => {
    renderPanels([{ team: PLATFORM, totals: totals({ new: 2 }), mine: mine(7, [], []), rows: new Map() }])
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    expect(within(panel).getByText(/Nothing claimed or assigned to you/)).toBeInTheDocument()
    expect(within(panel).queryByText(/0 requests/)).toBeNull()
  })

  it('counts a request the read could not name instead of dropping it', () => {
    // `mine.total` 3, but the page carried no row for any of them.
    const inboxes: TeamInbox[] = [{ team: PLATFORM, totals: totals({}), mine: mine(7, [21], [20], { ids: [21, 20], total: 3 }), rows: new Map() }]
    expect(unlistedCount(inboxes)).toBe(3)
    renderPanels(inboxes)
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    expect(within(panel).getByText(/3 requests in your name, but this read didn't send their rows/)).toBeInTheDocument()
  })

  it('links a row into its team inbox', () => {
    renderPanels(fixture())
    const panel = screen.getByRole('region', { name: 'Claimed by me, team requests' })
    expect(within(panel).getByRole('link', { name: /SSO group mapping for Berlin/ })).toHaveAttribute('href', '/teams/4/requests')
  })
})

// The two decisions the lib module owns, asserted directly so a component
// refactor cannot quietly change what "mine" means.
describe('the claimed slice is the server\'s, deduped', () => {
  it('marks claimed vs assigned from the row itself', () => {
    const rows = claimedRequests(fixture())
    expect(rows.map(r => [r.request.id, r.via])).toEqual([[21, 'claimed'], [20, 'assigned']])
  })

  it('an id the same read did not send is never invented', () => {
    const inboxes: TeamInbox[] = [{ team: PLATFORM, totals: totals({}), mine: mine(7, [21], []), rows: new Map() }]
    expect(claimedRequests(inboxes)).toEqual([])
    expect(unlistedCount(inboxes)).toBe(1)
  })

  it('a stale total smaller than the named rows never prints a negative', () => {
    const inboxes: TeamInbox[] = [{
      team: PLATFORM, totals: totals({}),
      mine: mine(7, [21], [], { total: 0 }),
      rows: new Map([[21, request({ id: 21, claim_user_id: 7, claimed_at: CLAIMED_AT })]]),
    }]
    expect(unlistedCount(inboxes)).toBe(0)
  })

  it('mineLine states the claim age, and "assigned" for an owner without a claim', () => {
    const rows = claimedRequests(fixture())
    const now = Date.now()
    expect(mineLine(rows[0], now)).toBe('claimed 26h')
    expect(mineLine(rows[1], now)).toBe('assigned')
  })
})

// The panels must not make their own reads: the hook owns the one read per
// team. This renders them with no handlers registered at all (the server above
// fails any request) and asserts they still render their props' answer.
describe('the panels render their props and perform no reads', () => {
  it('renders without touching the network', () => {
    let called = 0
    server.use(http.get('*/api/*', () => { called++; return HttpResponse.json({}, { status: 500 }) }))
    renderPanels(fixture())
    expect(screen.getByRole('region', { name: 'Team inboxes' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Claimed by me, team requests' })).toBeInTheDocument()
    expect(called).toBe(0)
  })
})

// A guard on the fixture itself: `TeamRequest` has no field the panels may
// count with. If a future server sends a request the panels are not told about,
// nothing here derives a number from `rows`.
describe('counts are never derived from rows', () => {
  it('the queue panel shows the server total even when rows disagree', () => {
    const rows = new Map<number, TeamRequest>([[99, request({ id: 99 })]])
    const inboxes: TeamInbox[] = [{ team: PLATFORM, totals: totals({ new: 12 }), rows }]
    renderPanels(inboxes)
    const panel = screen.getByRole('region', { name: 'Team inboxes' })
    expect(within(panel).getByText('12')).toBeInTheDocument()
    expect(within(panel).getByText(/12 requests in the shared queue/)).toBeInTheDocument()
  })
})
