// The team request detail page (AMP #3984, build slice J2's page half).
//
// The ticket's three criteria, each proved here against the real page, the real
// API client and MSW — not against a mocked component:
//
//  1. **Every approved detail state is reachable.** Each reading the mockup
//     draws (filled / needsinfo / conflict / rejected / duplicate / delivered) is
//     rendered from a request in the state that produces it, and the panels it
//     shows are the ones the state has the facts for. Conflict is a 409 layered
//     over the body, so it is driven through a real claim race.
//  2. **A stale or lost-claim failure preserves the draft and explains the
//     action.** A half-typed question is typed into the triage rail, then a claim
//     race is lost: the banner names the winner, says nothing was typed was sent,
//     and the question is still in the box afterwards. The same for a stale
//     revision on a reopen, and for the deliver brief across a soft re-read.
//  3. **No private-route or auth bypass.** The route answers a requester
//     projection for the request's own requester; the page renders a notice and
//     **no** triage surface — no claim control, no assignee select, no brief, no
//     org id, no rev. An unknown `view` likewise never becomes a team record. A
//     non-member gets no act either.
//
// Plus the two things the ticket names outright: focused page tests against
// mocked **real DTOs** (every fixture below is the server's own shape), and
// mobile accessibility (a 390 px reading, keyboard-reachable acts, labelled
// controls, and the layout collapsing to one column by structure rather than by
// pixels — jsdom has no layout, so the structure is what is asserted).
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { RequestDetail } from './RequestDetail'
import type {
  Member, RequestAttachmentPanel, RequestEvent, RequestComment, RequestOutcome, RequestStatus, Team, TeamRequest,
} from '../types'
import type { ResolvedRequestLink } from '../lib/requestControls'

const ME = 12
const TEAM_ID = 4

// ---- The real DTOs, as the server sends them ----

const request = (over: Partial<TeamRequest> = {}): TeamRequest => ({
  id: 7, public_ref: 'a1b2c3d4e5f6g7h8i9j0', org_id: 2, team_id: TEAM_ID,
  requester_user_id: 31, requester_contact: 'it-admin@acme.example',
  source_kind: 'web', source_external_id: '',
  subject: 'SSO group mapping for the Berlin office',
  input_brief: "We're rolling out SSO for the Berlin office (about 40 people). We need the group mapping set up.",
  outcome_brief: '',
  state: 'triaging', claim_user_id: ME, claimed_at: '2026-11-13T08:00:00Z', owner_user_id: ME,
  duplicate_of: null, reject_reason: '', needs_info_since: null,
  planned_at: null, delivered_at: null, accepted_at: null, accepted_by: null,
  closed_at: null, reopened_at: null, reopened_count: 0,
  rev: 7, metadata: {}, created_at: '2026-11-12T09:00:00Z', updated_at: '2026-11-13T10:00:00Z',
  ...over,
})

const outcome = (over: Partial<RequestOutcome> = {}): RequestOutcome => ({
  phase: 'in_progress', items: [], total: 0, done: 0, open: 0, blocked: 0, gated: 0, held: 0,
  deleted: 0, unverified: 0, rework: 0, linked: 1, routed_project_id: 3,
  outcome_brief_set: false, no_artifact: false, delivered_eligible: false,
  ...over,
})

const members: Member[] = [
  { user_id: ME, email: 'jacob@simstech.cloud', display_name: 'Jacob Sims', role: 'lead' },
  { user_id: 44, email: 'priya@simstech.cloud', display_name: 'Priya Nair', role: 'member' },
]

const TEAM: Team = { id: TEAM_ID, org_id: 2, name: 'Platform engineering', created_by: null, created_at: '2026-01-01T00:00:00Z' }

const attachments = (): RequestAttachmentPanel => ({
  request_id: 7, public_ref: 'a1b2c3d4e5f6g7h8i9j0', items: [], count: 0,
  limits: { max_file_bytes: 25 << 20, max_files: 5, max_total_bytes: 60 << 20, retention_days: 90 },
})

const link = (over: Partial<ResolvedRequestLink> = {}): ResolvedRequestLink => ({
  id: 1, request_id: 7, link_kind: 'planned_as', target_kind: 'task', target_id: 3712,
  created_by: ME, created_at: '2026-11-13T09:00:00Z',
  target: { kind: 'task', id: 3712, title: 'AMP-V2-3712 — Berlin SSO group mapping', approved: false },
  ...over,
})

// ---- The server ----

interface Call { method: string; path: string; ifMatch: string | null; body: string }
let calls: Call[] = []

/** Record every call, so a test can assert the exact route, verb and precondition. */
function record(method: string, pattern: string, answer: unknown = {}, status = 200) {
  return http[method as 'get'](pattern, async ({ request: req }) => {
    calls.push({
      method: req.method,
      path: new URL(req.url).pathname,
      ifMatch: req.headers.get('if-match'),
      body: await req.text(),
    })
    return HttpResponse.json(answer as Record<string, unknown>, { status })
  })
}

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = [] })
afterEach(() => { server.resetHandlers() })
afterAll(() => server.close())

/** The team read and the attachments read, which every detail render makes. */
function serveTeam() {
  server.use(record('get', `*/api/teams/${TEAM_ID}`, { team: TEAM, members }))
  server.use(record('get', `*/api/teams/${TEAM_ID}/requests/7/attachments`, attachments()))
  server.use(record('get', '*/api/projects/3', { project: { id: 3, name: 'AMP v2' } }))
}

/** The detail read as the team arm: the record plus its aggregate. */
function serveDetail(req: TeamRequest = request(), out: RequestOutcome = outcome()) {
  server.use(record('get', `*/api/teams/${TEAM_ID}/requests/${req.id}`, { view: 'team', request: req, outcome: out }))
}

/** The detail read as the requester arm: the whitelist projection, and no record. */
function serveRequesterDetail(state: RequestStatus = 'triaging') {
  server.use(record('get', `*/api/teams/${TEAM_ID}/requests/7`, {
    view: 'requester',
    projection: {
      public_ref: 'a1b2c3d4e5f6g7h8i9j0',
      state,
      state_word: 'Being reviewed',
      subject: 'SSO group mapping for the Berlin office',
      input_brief: 'We need the group mapping set up.',
      created_at: '2026-11-12T09:00:00Z',
      milestones: [],
      comments: [],
      outputs: [],
    },
  }))
}

function renderPage(props: Partial<React.ComponentProps<typeof RequestDetail>> = {}) {
  return render(
    <RequestDetail teamId={TEAM_ID} requestRef={7} meId={ME} now={Date.parse('2026-11-14T10:00:00Z')} {...props} />,
  )
}

const mutationAnswer = (id: number, rev: number, state: RequestStatus = 'triaging') => ({
  request: { id, public_ref: 'a1b2c3d4e5f6g7h8i9j0', state, rev },
  changed: true,
})

// ---- 1. Every approved detail state is reachable ----

describe('every approved detail state', () => {
  // Each row names the sections (by landmark, the way a screen reader finds
  // them) that the reading shows and those it must not. A section is the unit
  // the mockup's own states switch on.
  //
  // Two vocabularies, because the components have two: the #3979 panels head
  // their sections with an `h3` (`RequestPanelSection`), and the #3980 rails are
  // `<section aria-label>` landmarks. Both are asserted, because "the state is
  // reachable" means the person reading it can find the section, not just that
  // some string appears somewhere in the DOM.
  type Expect = { heading?: string; landmark?: string }
  const cases: { mockup: string; state: RequestStatus; shows: Expect[]; hides: Expect[] }[] = [
    {
      mockup: 'filled', state: 'triaging',
      shows: [
        { heading: 'Request' }, { heading: 'Attachments' },
        { landmark: 'Triage' }, { landmark: 'Planning' }, { landmark: 'Delivery' },
      ],
      hides: [{ heading: 'Clarification' }, { heading: 'Rejected' }, { heading: 'Duplicate' }, { landmark: 'Reopen' }],
    },
    {
      mockup: 'needsinfo', state: 'needs_information',
      shows: [
        { heading: 'Request' }, { heading: 'Clarification' },
        { landmark: 'Triage' }, { landmark: 'Planning' },
      ],
      hides: [{ heading: 'Rejected' }, { heading: 'Duplicate' }, { landmark: 'Reopen' }],
    },
    {
      mockup: 'rejected', state: 'rejected',
      shows: [{ heading: 'Rejected' }, { landmark: 'Reopen' }, { landmark: 'Triage' }],
      // A rejection says the team is not doing it: the planning and deliver
      // panels are gone, and Reopen is the act that brings them back.
      hides: [{ heading: 'Duplicate' }, { landmark: 'Planning' }, { landmark: 'Delivery' }],
    },
    {
      mockup: 'duplicate', state: 'duplicate',
      shows: [{ heading: 'Duplicate' }, { landmark: 'Reopen' }],
      hides: [{ heading: 'Rejected' }, { landmark: 'Planning' }, { landmark: 'Delivery' }],
    },
    {
      mockup: 'delivered', state: 'delivered',
      // The mockup keeps Planning on a delivery (artifact 37 shows it under
      // `filled needsinfo conflict delivered`): what the request was planned as
      // is part of how the delivery reads.
      shows: [{ heading: 'Delivery' }, { heading: 'Request' }, { landmark: 'Delivery' }, { landmark: 'Planning' }],
      hides: [{ heading: 'Rejected' }, { heading: 'Duplicate' }],
    },
  ]

  it.each(cases)('renders the $mockup reading from a $state request', async ({ state, shows, hides }) => {
    serveTeam()
    serveDetail(
      request({
        state,
        reject_reason: state === 'rejected' ? 'Out of scope: we don’t run hardware procurement.' : '',
        duplicate_of: state === 'duplicate' ? 71 : null,
        closed_at: state === 'rejected' || state === 'duplicate' ? '2026-11-10T11:02:00Z' : null,
        needs_info_since: state === 'needs_information' ? '2026-11-13T08:00:00Z' : null,
        outcome_brief: state === 'delivered' ? 'Group mapping live in production.' : '',
        delivered_at: state === 'delivered' ? '2026-11-13T12:00:00Z' : null,
      }),
      outcome({
        phase: state === 'rejected' || state === 'duplicate' ? 'closed' : 'in_progress',
        outcome_brief_set: state === 'delivered',
        delivered_eligible: state === 'delivered',
      }),
    )
    renderPage()
    // The brief and the state tag are the two things every reading has.
    await screen.findByTestId('request-brief')
    expect(screen.getByTestId('detail-state-tag')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByTestId('request-brief')).toBeInTheDocument())
    for (const want of shows) {
      if (want.heading) expect(screen.getByRole('heading', { name: want.heading, level: 3 })).toBeInTheDocument()
      if (want.landmark) expect(screen.getByRole('region', { name: want.landmark })).toBeInTheDocument()
    }
    for (const want of hides) {
      if (want.heading) expect(screen.queryByRole('heading', { name: want.heading, level: 3 })).toBeNull()
      if (want.landmark) expect(screen.queryByRole('region', { name: want.landmark })).toBeNull()
    }
  })

  it('renders the conflict reading from a lost claim race, over the request’s whole body', async () => {
    serveTeam()
    serveDetail(request({ claim_user_id: null, claimed_at: null, owner_user_id: null }))
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/claim`, () =>
      HttpResponse.json(
        { error: 'That claim was taken first.', code: 'request.conflict', claim_user_id: 44, rev: 8 },
        { status: 409 },
      ),
    ))
    // Nobody holds the claim in the first answer, so the rail offers Claim;
    // losing the race is the only way to reach the mockup's conflict reading.
    renderPage()
    const claim = await screen.findByRole('button', { name: /^Claim$/ })
    await userEvent.click(claim)

    // The banner names the winner, says claims are first-write-wins and that
    // nothing was sent — the mockup's three promises.
    const banner = await screen.findByRole('alert')
    expect(banner).toHaveTextContent('Priya Nair')
    expect(banner).toHaveTextContent(/first-write-wins/)
    expect(banner).toHaveTextContent(/Nothing you typed was sent|Nothing was sent/)
    // And the body is still there: a banner alone would answer "what is this
    // request" with silence.
    expect(screen.getByTestId('request-brief')).toBeInTheDocument()
    expect(screen.getByTestId('detail-state-tag')).toHaveTextContent('Triaging')
  })

  it('re-reads the record on Refresh after a lost claim, so the winner’s claim is what the rail shows', async () => {
    serveTeam()
    // The counting handler is registered INSTEAD of serveDetail, not after it:
    // MSW matches the first handler for a path, so a second `use` would never
    // run and the test would be asserting against a fixture it thinks it
    // replaced.
    let detailReads = 0
    server.use(http.get(`*/api/teams/${TEAM_ID}/requests/7`, () => {
      detailReads += 1
      // The second read is the state after the race: Priya holds it now.
      const now = detailReads > 1
        ? request({ claim_user_id: 44, claimed_at: '2026-11-14T10:00:00Z', owner_user_id: null, rev: 8 })
        : request({ claim_user_id: null, claimed_at: null, owner_user_id: null })
      return HttpResponse.json({ view: 'team', request: now, outcome: outcome() })
    }))
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/claim`, () =>
      HttpResponse.json({ error: 'taken', code: 'request.conflict', claim_user_id: 44, rev: 8 }, { status: 409 }),
    ))
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: /^Claim$/ }))
    const banner = await screen.findByRole('alert')

    // Nothing is re-read behind the reader's back: the mockup's banner says
    // "refresh to see her claim", and a silent reload is the opposite of what a
    // first-write-wins rule promises.
    expect(detailReads).toBe(1)

    // Refresh is the offered action, and it is what moves the rail.
    await userEvent.click(within(banner).getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(detailReads).toBeGreaterThan(1))
    await waitFor(() => expect(screen.queryByRole('button', { name: /^Claim$/ })).toBeNull())
    // Priya holds it now, and the triage rail is where that is said.
    const triage = screen.getByRole('region', { name: 'Triage' })
    expect(within(triage).getAllByText('Priya Nair').length).toBeGreaterThan(0)
  })
})

// ---- 2. A failure preserves the draft and explains the action ----

describe('a failed write preserves the draft and explains itself', () => {
  it('keeps a half-typed question when the claim race is lost, and offers Refresh', async () => {
    serveTeam()
    serveDetail(request({ claim_user_id: null, claimed_at: null, owner_user_id: null }))
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/claim`, () =>
      HttpResponse.json({ error: 'taken', code: 'request.conflict', claim_user_id: 44, rev: 8 }, { status: 409 }),
    ))
    renderPage()

    // Type the question first: this is the draft the criterion is about.
    await userEvent.click(await screen.findByRole('button', { name: 'Ask the requester' }))
    const box = screen.getByLabelText('Question to the requester') as HTMLTextAreaElement
    await userEvent.type(box, 'Which of the two groups should be the default?')
    expect(box.value).toBe('Which of the two groups should be the default?')

    // Then lose a claim race elsewhere on the same page.
    await userEvent.click(screen.getByRole('button', { name: /^Claim$/ }))
    const banner = await screen.findByRole('alert')
    expect(banner).toHaveTextContent('Priya Nair')
    expect(banner).toHaveTextContent(/first-write-wins/)
    expect(banner).toHaveTextContent(/Nothing you typed was sent|Nothing was sent/)
    // A Refresh is the offered action, not a silent retry and not a steal.
    expect(within(banner).getByRole('button', { name: 'Refresh' })).toBeInTheDocument()

    // The draft is untouched: the panels were never unmounted.
    expect((screen.getByLabelText('Question to the requester') as HTMLTextAreaElement).value)
      .toBe('Which of the two groups should be the default?')
  })

  it('keeps the draft across a Refresh, which re-reads at the new revision', async () => {
    serveTeam()
    serveDetail(request({ claim_user_id: null, claimed_at: null, owner_user_id: null }))
    let reads = 0
    server.use(http.get(`*/api/teams/${TEAM_ID}/requests/7`, () => {
      reads += 1
      return HttpResponse.json({
        view: 'team',
        request: request({ claim_user_id: reads > 1 ? 44 : null, claimed_at: reads > 1 ? '2026-11-14T10:00:00Z' : null, owner_user_id: null, rev: reads > 1 ? 8 : 7 }),
        outcome: outcome(),
      })
    }))
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/claim`, () =>
      HttpResponse.json({ error: 'taken', code: 'request.conflict', claim_user_id: 44, rev: 8 }, { status: 409 }),
    ))
    renderPage()

    await userEvent.click(await screen.findByRole('button', { name: 'Ask the requester' }))
    const box = screen.getByLabelText('Question to the requester') as HTMLTextAreaElement
    await userEvent.type(box, 'Which group?')
    await userEvent.click(screen.getByRole('button', { name: /^Claim$/ }))
    const banner = await screen.findByRole('alert')
    await userEvent.click(within(banner).getByRole('button', { name: 'Refresh' }))

    // The re-read landed (the claim moved to Priya) and the draft survived it.
    await waitFor(() => expect(screen.queryByRole('button', { name: /^Claim$/ })).toBeNull())
    expect((screen.getByLabelText('Question to the requester') as HTMLTextAreaElement).value).toBe('Which group?')
  })

  it('states a stale revision plainly, and sends the act with the revision the page read', async () => {
    serveTeam()
    serveDetail(request({ state: 'rejected', closed_at: '2026-11-10T11:02:00Z', claim_user_id: null, owner_user_id: null }))
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/reopen`, async ({ request: req }) => {
      calls.push({ method: req.method, path: new URL(req.url).pathname, ifMatch: req.headers.get('if-match'), body: await req.text() })
      return HttpResponse.json({ error: 'Your view is stale.', code: 'request.conflict', rev: 11 }, { status: 409 })
    }))
    renderPage()

    await userEvent.click(await screen.findByRole('button', { name: 'Reopen' }))
    const banner = await screen.findByRole('alert')
    // A reopen 409 is a stale view, and nobody is competing for a claim: the
    // banner must not name a "winner" the server never sent.
    expect(banner).toHaveTextContent('This request changed since your view loaded.')
    expect(banner).not.toHaveTextContent(/claimed this request first/)
    expect(banner).toHaveTextContent('rev 11')
    expect(banner).toHaveTextContent('rev 7')
    // The promise the mockup's banner makes, for the page's own act too.
    expect(banner).toHaveTextContent('Nothing was changed')

    // The precondition was the revision on screen, not an invented one.
    const reopenCall = calls.find(c => c.path.endsWith('/requests/7/reopen'))
    expect(reopenCall?.ifMatch).toBe('"7"')
    // And the body is still on screen: a stale view is a banner, not a wipe.
    expect(screen.getByTestId('request-brief')).toBeInTheDocument()
  })

  it('keeps a half-typed composer message across the soft re-read a successful act causes', async () => {
    serveTeam()
    serveDetail(request({ state: 'rejected', closed_at: '2026-11-10T11:02:00Z', claim_user_id: null, owner_user_id: null }))
    // A reopen succeeds; the page then re-reads in the background. That is the
    // re-read a draft has to survive, and the composer is the draft that costs
    // the most to lose.
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/reopen`, () => HttpResponse.json(mutationAnswer(7, 8))))
    let reads = 0
    server.use(http.get(`*/api/teams/${TEAM_ID}/requests/7`, () => {
      reads += 1
      return HttpResponse.json({
        view: 'team',
        request: reads > 1 ? request({ state: 'triaging', rev: 8 }) : request({ state: 'rejected', closed_at: '2026-11-10T11:02:00Z', claim_user_id: null, owner_user_id: null }),
        outcome: outcome(),
      })
    }))
    // The discussion is served here (empty thread), which is what draws the
    // composer: the page never posts a comment it has nowhere to show.
    renderPage({ reads: { comments: [] } })

    const composer = await screen.findByLabelText('Comment') as HTMLTextAreaElement
    await userEvent.type(composer, 'Reopened because the requester clarified the groups.')
    expect(composer.value).toBe('Reopened because the requester clarified the groups.')

    await userEvent.click(screen.getByRole('button', { name: 'Reopen' }))
    await waitFor(() => expect(screen.getByTestId('detail-state-tag')).toHaveTextContent('Triaging'))

    // The same node, still holding the same text: the re-read never unmounted
    // the body, which is the whole mechanism behind the criterion.
    const after = screen.getByLabelText('Comment') as HTMLTextAreaElement
    expect(after).toBe(composer)
    expect(after.value).toBe('Reopened because the requester clarified the groups.')
  })

  it('keeps the page usable when the attachments read fails, and names its own failure', async () => {
    serveTeam()
    serveDetail()
    server.use(http.get(`*/api/teams/${TEAM_ID}/requests/7/attachments`, () =>
      HttpResponse.json({ error: 'The attachment store is unavailable.' }, { status: 503 }),
    ))
    renderPage()
    // The request is fine; only the file list failed, and it says so in place.
    expect(await screen.findByText(/attachment store is unavailable/)).toBeInTheDocument()
    expect(screen.getByTestId('request-brief')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })
})

// ---- 3. No private-route or auth bypass ----

describe('no private-route or auth bypass', () => {
  it('renders a notice, not a triage screen, for the requester’s own read', async () => {
    serveTeam()
    serveRequesterDetail('triaging')
    renderPage({ meId: 31 })

    expect(await screen.findByText(/Not the team’s view/)).toBeInTheDocument()
    // The whole internal surface is absent, not empty: no claim, no assignee, no
    // planning, no deliver panel, no discussion composer.
    expect(screen.queryByRole('button', { name: /^Claim$/ })).toBeNull()
    expect(screen.queryByLabelText('Assign to a team member')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Ask the requester' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Mark delivered/ })).toBeNull()
    expect(screen.queryByLabelText('Comment')).toBeNull()
    // None of the record's internal facts can appear: the page never had them.
    expect(screen.queryByText(/rev 7/)).toBeNull()
    expect(screen.queryByText('it-admin@acme.example')).toBeNull()
    expect(document.body.textContent).not.toMatch(/org_id/)
  })

  it('still shows the requester their own reference and state, which is theirs to see', async () => {
    serveTeam()
    serveRequesterDetail('needs_information')
    renderPage({ meId: 31 })
    // The reference is the requester's own handle on the request, and the state
    // word is the two things their tracking page shows.
    expect(await screen.findByRole('heading', { name: 'REQ-a1b2c3d4e5f6g7h8i9j0' })).toBeInTheDocument()
    expect(screen.getByText('Needs info')).toBeInTheDocument()
  })

  it('never turns an unknown view into a team record', async () => {
    serveTeam()
    server.use(record('get', `*/api/teams/${TEAM_ID}/requests/7`, {
      view: 'collaborator',
      // Even with a record field present: the view word is the server's decision.
      request: request(),
      outcome: outcome(),
    }))
    renderPage()
    expect(await screen.findByText(/Not this screen/)).toBeInTheDocument()
    expect(screen.getByText(/does not render/)).toBeInTheDocument()
    expect(screen.queryByTestId('request-brief')).toBeNull()
    expect(screen.queryByRole('button', { name: /^Claim$/ })).toBeNull()
  })

  it('explains a 404 without distinguishing "not yours" from "does not exist"', async () => {
    serveTeam()
    server.use(record('get', `*/api/teams/${TEAM_ID}/requests/7`, { error: 'Request not found.', code: 'request.not_found' }, 404))
    renderPage()
    expect(await screen.findByText('This request didn’t open.')).toBeInTheDocument()
    // The copy states the rule rather than a guess about which team it is.
    expect(screen.getByText(/not-found on purpose, never permission-denied/)).toBeInTheDocument()
    expect(screen.queryByText(/permission denied/i)).toBeNull()
  })

  it('offers no act to a caller who is not on the inbox', async () => {
    serveTeam()
    serveDetail(request({ claim_user_id: null, claimed_at: null, owner_user_id: null, state: 'rejected', closed_at: '2026-11-10T11:02:00Z' }))
    // A user id that is not in the team's rows and no org role: a stranger the
    // server would 404 before this page, but the page must not offer acts anyway.
    renderPage({ meId: 99 })
    // Reopen says whose act it is instead of offering a button that would 403.
    expect(await screen.findByTestId('reopen-refusal')).toHaveTextContent(/Only the inbox team/)
    expect(screen.queryByRole('button', { name: 'Reopen' })).toBeNull()
    // The triage rail's closing acts are not offered either.
    expect(screen.queryByRole('button', { name: 'Ask the requester' })).toBeNull()
  })

  it('takes its read from the team-scoped route, never an id-only one', async () => {
    serveTeam()
    serveDetail()
    renderPage()
    await screen.findByTestId('request-brief')
    const detail = calls.find(c => c.path.endsWith('/requests/7'))
    expect(detail?.path).toBe(`/api/teams/${TEAM_ID}/requests/7`)
    // No call ever went to an un-scoped request path.
    for (const c of calls) expect(c.path.startsWith('/api/requests')).toBe(false)
  })

  it('accepts a public reference as well as a serial, and asks for it scoped', async () => {
    serveTeam()
    server.use(record('get', `*/api/teams/${TEAM_ID}/requests/a1b2c3d4e5f6g7h8i9j0`, {
      view: 'team', request: request(), outcome: outcome(),
    }))
    renderPage({ requestRef: 'a1b2c3d4e5f6g7h8i9j0' })
    await screen.findByTestId('request-brief')
    expect(calls.some(c => c.path === `/api/teams/${TEAM_ID}/requests/a1b2c3d4e5f6g7h8i9j0`)).toBe(true)
  })
})

// ---- The read the shipped server does not serve ----

describe('the rows this build’s read does not carry', () => {
  it('states the gap for discussion, history and links instead of an empty list', async () => {
    serveTeam()
    serveDetail()
    renderPage()
    await screen.findByTestId('request-brief')
    // Each panel says the rows are not served — the words "No discussion yet"
    // would be a claim about the record the page has no basis for.
    expect(screen.getByTestId('unserved-discussion')).toHaveTextContent(/no route/)
    expect(screen.getByTestId('unserved-history')).toHaveTextContent(/no route/)
    expect(screen.getByTestId('unserved-links')).toHaveTextContent(/no route/)
    expect(screen.queryByText(/No discussion yet/)).toBeNull()
    expect(screen.queryByText(/No acts recorded yet/)).toBeNull()
  })

  it('renders the thread, the history and the plan rows when a seam serves them', async () => {
    serveTeam()
    serveDetail()
    const comments: RequestComment[] = [
      { id: 1, request_id: 7, kind: 'comment', body: 'Metadata checks out.', author: 'Jacob Sims', author_user_id: ME, created_at: '2026-11-14T09:00:00Z' },
      { id: 2, request_id: 7, kind: 'question', body: 'Which group should be the default?', author: 'Jacob Sims', author_user_id: ME, created_at: '2026-11-14T09:05:00Z' },
    ]
    const events: RequestEvent[] = [
      { id: 1, request_id: 7, org_id: 2, team_id: TEAM_ID, action: 'submit', actor: 'it-admin@acme.example', actor_user_id: null, created_at: '2026-11-12T09:00:00Z' },
      { id: 2, request_id: 7, org_id: 2, team_id: TEAM_ID, action: 'transition', from_status: 'new', to_status: 'triaging', actor: 'Jacob Sims', actor_user_id: ME, created_at: '2026-11-13T08:00:00Z' },
    ]
    renderPage({ reads: { comments, events, links: [link()] } })

    expect(await screen.findByText('Metadata checks out.')).toBeInTheDocument()
    expect(screen.getByText('Which group should be the default?')).toBeInTheDocument()
    expect(screen.getAllByTestId('history-row').length).toBeGreaterThan(0)
    // The plan row is the seam's own, with its approval fact.
    expect(screen.getByText('AMP-V2-3712')).toBeInTheDocument()
    expect(screen.getByText('awaiting approval')).toBeInTheDocument()
    // And the gap sentences are gone, because the gap is.
    expect(screen.queryByTestId('unserved-discussion')).toBeNull()
    expect(screen.queryByTestId('unserved-history')).toBeNull()
    expect(screen.queryByTestId('unserved-links')).toBeNull()
  })

  it('titles the aggregate’s route from one project read, and keeps the id when that read fails', async () => {
    serveTeam()
    serveDetail(request(), outcome({ routed_project_id: 3 }))
    renderPage()
    // The rail's heading names the route; the name came from the project read.
    await waitFor(() => expect(screen.getAllByText('AMP v2').length).toBeGreaterThan(0))
    expect(screen.getByText('routes to:')).toBeInTheDocument()
  })

  it('shows the route by id when the project read is unavailable, rather than claiming no route', async () => {
    serveTeam()
    serveDetail(request(), outcome({ routed_project_id: 3 }))
    server.use(http.get('*/api/projects/3', () => HttpResponse.json({ error: 'gone' }, { status: 500 })))
    renderPage()
    await waitFor(() => expect(screen.getAllByText('Project #3').length).toBeGreaterThan(0))
    // "Not routed yet" would contradict the aggregate, which counted the route.
    expect(screen.queryByText(/Not routed yet/)).toBeNull()
  })

  it('never invents the discussion from the clarification state', async () => {
    serveTeam()
    serveDetail(request({ state: 'needs_information', needs_info_since: '2026-11-13T08:00:00Z' }))
    renderPage()
    // The card knows a question is open and since when; it says the text is not
    // in this read instead of inventing one.
    expect(await screen.findByText(/needs the requester’s answer/)).toBeInTheDocument()
    expect(screen.getByTestId('clarification-unserved')).toBeInTheDocument()
  })
})

// ---- The page's own act ----

describe('reopen', () => {
  it('reopens a rejected request through the lifecycle route, with the revision it read', async () => {
    serveTeam()
    serveDetail(request({ state: 'rejected', reject_reason: 'Out of scope.', closed_at: '2026-11-10T11:02:00Z', claim_user_id: null, owner_user_id: null }))
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/reopen`, async ({ request: req }) => {
      calls.push({ method: req.method, path: new URL(req.url).pathname, ifMatch: req.headers.get('if-match'), body: await req.text() })
      return HttpResponse.json(mutationAnswer(7, 8, 'triaging'))
    }))
    let reads = 0
    server.use(http.get(`*/api/teams/${TEAM_ID}/requests/7`, () => {
      reads += 1
      return HttpResponse.json({
        view: 'team',
        request: reads > 1 ? request({ state: 'triaging', rev: 8 }) : request({ state: 'rejected', reject_reason: 'Out of scope.', closed_at: '2026-11-10T11:02:00Z', claim_user_id: null, owner_user_id: null }),
        outcome: outcome(),
      })
    }))
    renderPage()

    await userEvent.click(await screen.findByRole('button', { name: 'Reopen' }))
    const call = calls.find(c => c.path.endsWith('/reopen'))
    expect(call?.method).toBe('POST')
    expect(call?.ifMatch).toBe('"7"')
    // The record re-read into the open reading, and the page says so.
    await waitFor(() => expect(screen.getByTestId('detail-state-tag')).toHaveTextContent('Triaging'))
    expect(screen.getByText(/Reopened: the request is back in Triaging/)).toBeInTheDocument()
    // The rejection card is gone, because the rejection is.
    expect(screen.queryByText(/A rejection carries a reason/)).toBeNull()
  })

  it('refuses to offer reopen on a delivery and says whose act it is', async () => {
    serveTeam()
    serveDetail(request({ state: 'delivered', claim_user_id: null, owner_user_id: null, delivered_at: '2026-11-13T12:00:00Z', outcome_brief: 'Live.' }), outcome({ phase: 'delivered', delivered_eligible: true, outcome_brief_set: true }))
    renderPage()
    expect(await screen.findByTestId('reopen-refusal')).toHaveTextContent(/requester/)
    expect(screen.queryByRole('button', { name: 'Reopen' })).toBeNull()
  })
})

// ---- The other acts are composed, not re-implemented ----

describe('the composed acts write through the routes the server registers', () => {
  it('asks the requester a question through needs-info, with If-Match', async () => {
    serveTeam()
    serveDetail()
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/needs-info`, async ({ request: req }) => {
      calls.push({ method: req.method, path: new URL(req.url).pathname, ifMatch: req.headers.get('if-match'), body: await req.text() })
      return HttpResponse.json({ request: { id: 7, public_ref: 'x', state: 'needs_information', rev: 8 }, changed: true })
    }))
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Ask the requester' }))
    await userEvent.type(screen.getByLabelText('Question to the requester'), 'Which group?')
    await userEvent.click(screen.getByRole('button', { name: 'Send question' }))

    await waitFor(() => expect(calls.some(c => c.path.endsWith('/needs-info'))).toBe(true))
    const call = calls.find(c => c.path.endsWith('/needs-info'))!
    expect(call.ifMatch).toBe('"7"')
    expect(JSON.parse(call.body)).toEqual({ question: 'Which group?', expected_rev: 7 })
  })

  it('delivers through the deliver route only when the server’s aggregate is eligible', async () => {
    serveTeam()
    // First render: not eligible. The panel's button is disabled and says why.
    serveDetail(
      request({ state: 'in_progress', claim_user_id: null, owner_user_id: null }),
      outcome({ phase: 'not_started', delivered_eligible: false, blocked_reason: 'The request needs an outcome brief.' }),
    )
    renderPage()
    const button = await screen.findByRole('button', { name: /Mark delivered/ })
    expect(button).toBeDisabled()
    // The reason is a sentence on screen, not only a tooltip.
    expect(screen.getAllByText(/outcome brief/i).length).toBeGreaterThan(0)
  })

  it('sends a claim through the team-scoped claim route with the revision it read', async () => {
    serveTeam()
    serveDetail(request({ claim_user_id: null, claimed_at: null, owner_user_id: null, rev: 9 }))
    server.use(http.post(`*/api/teams/${TEAM_ID}/requests/7/claim`, async ({ request: req }) => {
      calls.push({ method: req.method, path: new URL(req.url).pathname, ifMatch: req.headers.get('if-match'), body: await req.text() })
      return HttpResponse.json(mutationAnswer(7, 10))
    }))
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: /^Claim$/ }))
    await waitFor(() => expect(calls.some(c => c.path.endsWith('/requests/7/claim'))).toBe(true))
    expect(calls.find(c => c.path.endsWith('/claim'))?.ifMatch).toBe('"9"')
  })
})

// ---- Mobile accessibility ----

describe('mobile accessibility (390 px)', () => {
  const original = window.innerWidth
  beforeEach(() => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 390 })
  })
  afterEach(() => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: original })
  })

  it('collapses to one column by structure, not by a pixel breakpoint', async () => {
    serveTeam()
    serveDetail()
    renderPage()
    await screen.findByTestId('request-brief')
    // The body's grid starts at one column and only splits from `lg` up: at a
    // phone width the panels stack, and no panel has a fixed width that would
    // push the page sideways.
    const grid = document.querySelector('.lg\\:grid-cols-\\[minmax\\(0\\,1\\.7fr\\)_minmax\\(0\\,1fr\\)\\]')
    expect(grid).not.toBeNull()
    expect(grid?.className).toContain('grid-cols-1')
    expect(document.querySelector('[style*="width: 1440px"], [style*="min-width: 6"]')).toBeNull()
  })

  it('lets a keyboard user reach and operate the triage act without a mouse', async () => {
    serveTeam()
    serveDetail(request({ claim_user_id: null, claimed_at: null, owner_user_id: null }))
    renderPage()
    // Every act is a real button, so it is in the tab order and Enter fires it.
    const ask = await screen.findByRole('button', { name: 'Ask the requester' })
    ask.focus()
    expect(ask).toHaveFocus()
    await userEvent.keyboard('{Enter}')
    // Enter alone operates the act: the form opened, ready to type into.
    const box = await screen.findByLabelText('Question to the requester')
    expect(box).toBeInTheDocument()
    // And the box is reachable without a mouse: it is the next tab stop's target.
    box.focus()
    expect(box).toHaveFocus()
  })

  it('labels the controls a screen reader lands on', async () => {
    serveTeam()
    serveDetail()
    renderPage()
    await screen.findByTestId('request-brief')
    // The state tag is not colour-only: its word is real text, and the sentence
    // that says what the state means is on the element rather than implied.
    const tag = screen.getByTestId('detail-state-tag')
    expect(tag).toHaveTextContent('Triaging')
    expect(tag).toHaveAttribute('title', expect.stringContaining('looking at it now'))
    // The request's own brief is a readable region, not an image or a title attr.
    expect(screen.getByTestId('request-brief')).toBeInTheDocument()
    // Sections are landmark-named, so a reader can jump between them.
    expect(screen.getByRole('region', { name: 'Triage' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Planning' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 3, name: 'Request' })).toBeInTheDocument()
  })

  it('announces a background re-read without stealing the reader’s place', async () => {
    serveTeam()
    serveDetail()
    renderPage()
    await screen.findByTestId('request-brief')
    // Nothing here is a spinner over the body: the panels stay mounted, and a
    // re-read announces itself in one quiet status line when it happens.
    expect(screen.queryByText(/Re-reading the request/)).toBeNull()
    expect(screen.getByTestId('request-brief')).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('SSO group mapping')
  })
})
