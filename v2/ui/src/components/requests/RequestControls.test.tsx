// The three right-rail controls, rendered (AMP #3980, build slice J2).
//
// These are the tests the ticket asks for — control, conflict and denial — one
// describe per panel, against mocked real DTOs and the real API client:
//
//  1. **Triage:** claim and assignee are two fields with two labels; a lost claim
//     race renders the banner that names the winner, says nothing was sent, and
//     offers Refresh instead of silently taking the claim; a stale revision is a
//     different banner with no winner in it.
//  2. **Planning:** the "Handoff never auto-approves" warning is present in every
//     state (including closed and unplanned), plan rows carry their own approval
//     fact, and a refused attach says why without retrying.
//  3. **Deliver:** the button is disabled until the server's aggregate is
//     eligible, the reason is a sentence beside it (and the accessible
//     description), and a completed ticket never makes it live (I8).
//
// Nothing here asserts that the rails can approve, dispatch or complete: the
// negative is checked by the absence of such a control, which is a rendering
// fact and belongs in this file.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import { RequestTriageRail } from './RequestTriageRail'
import { RequestPlanningRail } from './RequestPlanningRail'
import { RequestDeliverPanel } from './RequestDeliverPanel'
import type { Member, RequestOutcome, RequestStatus, TeamRequest } from '../../types'
import type { ResolvedRequestLink } from '../../lib/requestControls'

const ME = 12

const request = (over: Partial<TeamRequest> = {}): TeamRequest => ({
  id: 7, public_ref: 'a1b2c3d4e5f6g7h8i9j0', org_id: 2, team_id: 4,
  requester_user_id: 31, requester_contact: 'it-admin@acme.example',
  source_kind: 'web', source_external_id: '',
  subject: 'SSO group mapping for the Berlin office',
  input_brief: 'We need the group mapping set up.',
  outcome_brief: '', state: 'triaging',
  claim_user_id: ME, claimed_at: '2026-11-13T08:00:00Z', owner_user_id: ME,
  duplicate_of: null, reject_reason: '', needs_info_since: null,
  planned_at: null, delivered_at: null, accepted_at: null, accepted_by: null,
  closed_at: null, reopened_at: null, reopened_count: 0,
  rev: 3, metadata: {}, created_at: '2026-11-12T09:00:00Z', updated_at: '2026-11-13T10:00:00Z',
  ...over,
})

const members: Member[] = [
  { user_id: ME, email: 'jacob@simstech.cloud', display_name: 'Jacob Sims', role: 'lead' },
  { user_id: 44, email: 'priya@simstech.cloud', display_name: 'Priya Nair', role: 'member' },
]

const outcome = (over: Partial<RequestOutcome> = {}): RequestOutcome => ({
  phase: 'in_progress', items: [], total: 0, done: 0, open: 0, blocked: 0, gated: 0, held: 0,
  deleted: 0, unverified: 0, rework: 0, linked: 1,
  outcome_brief_set: false, no_artifact: false, delivered_eligible: false,
  ...over,
})

const link = (over: Partial<ResolvedRequestLink> = {}): ResolvedRequestLink => ({
  id: 1, request_id: 7, link_kind: 'planned_as', target_kind: 'task', target_id: 3712,
  created_by: ME, created_at: '2026-11-13T09:00:00Z',
  target: { kind: 'task', id: 3712, title: 'AMP-V2-3712 — Berlin SSO group mapping', approved: false },
  ...over,
})

// ---- The server ----

interface Call { method: string; path: string; ifMatch: string | null }
let calls: Call[] = []
const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = [] })
afterEach(() => { server.resetHandlers() })
afterAll(() => server.close())

function route(method: 'post' | 'patch', pattern: string, answer: unknown, status = 200) {
  return http[method](pattern, async ({ request: r }) => {
    calls.push({ method: r.method, path: new URL(r.url).pathname, ifMatch: r.headers.get('if-match') })
    return HttpResponse.json(answer as Record<string, unknown>, { status })
  })
}

const answered = (over: Record<string, unknown> = {}) => ({
  request: { id: 7, public_ref: 'a1b2c3d4e5f6g7h8i9j0', state: 'triaging', rev: 4 }, changed: true, ...over,
})

// ---- Triage ----

describe('RequestTriageRail: claim and assignee are two fields', () => {
  it('shows Claim and Assignee under their own labels, with the claim’s age and the rule between them', () => {
    render(<RequestTriageRail request={request({ claim_user_id: null, claimed_at: null, owner_user_id: 44 })} meId={ME} members={members} canAssign />)
    const panel = screen.getByRole('region', { name: 'Triage' })
    // Two field labels, both present: "Claim" is also a button's name, so this
    // reads the label element itself.
    expect(within(panel).getByText('Claim', { selector: 'dt' })).toBeInTheDocument()
    expect(within(panel).getByText('Assignee', { selector: 'dt' })).toBeInTheDocument()
    expect(within(panel).getByText('Unclaimed')).toBeInTheDocument()
    // The caption is what stops the two from being read as one field.
    expect(within(panel).getByText(/Claim = who’s triaging now/)).toBeInTheDocument()
    expect(within(panel).getByRole('combobox', { name: 'Assign to a team member' })).toHaveValue('44')
  })

  it('weighs the claim’s age, because a claim never expires', () => {
    const now = Date.parse('2026-11-14T10:00:00Z')
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign now={now} />)
    // Claimed at 2026-11-13T08:00:00Z → 26h before `now`.
    expect(screen.getByText('· 26h ago')).toBeInTheDocument()
    expect(screen.getByText('(you)')).toBeInTheDocument()
  })

  it('offers Claim on an unclaimed request, and reports a replay as "nothing changed"', async () => {
    const user = userEvent.setup()
    let attempt = 0
    server.use(http.post('*/api/teams/4/requests/7/claim', ({ request: r }) => {
      calls.push({ method: r.method, path: new URL(r.url).pathname, ifMatch: r.headers.get('if-match') })
      // First click writes; the second is the same person clicking again, which
      // the server answers with changed: false (a retry, not a second claim).
      return HttpResponse.json(answered({ changed: attempt++ === 0 }))
    }))
    render(<RequestTriageRail request={request({ claim_user_id: null, claimed_at: null })} meId={ME} members={members} canAssign />)
    await user.click(screen.getByRole('button', { name: 'Claim' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toMatchObject({ method: 'POST', path: '/api/teams/4/requests/7/claim', ifMatch: '"3"' })
    await user.click(screen.getByRole('button', { name: 'Claim' }))
    expect(await screen.findByRole('status')).toHaveTextContent('You already hold this claim: nothing changed.')
  })

  it('offers Release only to the holder, and explains why someone else cannot take the claim', async () => {
    render(<RequestTriageRail request={request({ claim_user_id: 44, claimed_at: '2026-11-13T08:00:00Z' })} meId={ME} members={members} canAssign />)
    expect(screen.queryByRole('button', { name: 'Claim' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Release' })).toBeNull()
    expect(screen.getByText('Priya Nair', { selector: 'span' })).toBeInTheDocument()
    expect(screen.getByText(/A claim is released by its holder/)).toBeInTheDocument()
  })

  it('renders the lost-claim banner: who won, first-write-wins, and that nothing was sent', async () => {
    const user = userEvent.setup()
    const onChanged = () => { throw new Error('a refused write must not be reported as a change') }
    server.use(route('post', '*/api/teams/4/requests/7/claim', {
      error: 'request 7 is claimed by user 44: a claim is released by its holder or cleared by an assignee transfer, never taken',
      code: 'request.conflict', claim_user_id: 44, rev: 4,
    }, 409))
    render(<RequestTriageRail request={request({ claim_user_id: null, claimed_at: null })} meId={ME} members={members} canAssign onChanged={onChanged} onRefresh={() => {}} />)
    await user.click(screen.getByRole('button', { name: 'Claim' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Priya Nair claimed this request first.')
    expect(alert).toHaveTextContent('Claims are first-write-wins')
    expect(alert).toHaveTextContent('rev 3 → 4')
    expect(alert).toHaveTextContent('Nothing you typed was sent.')
    expect(within(alert).getByRole('button', { name: 'Refresh' })).toBeInTheDocument()
  })

  it('renders a stale-revision 409 as a reload, with no winner named', async () => {
    const user = userEvent.setup()
    server.use(route('post', '*/api/teams/4/requests/7/release', {
      error: 'request 7 is at rev 5, not rev 3', code: 'request.conflict', rev: 5,
    }, 409))
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign onRefresh={() => {}} />)
    await user.click(screen.getByRole('button', { name: 'Release' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('This request changed since your view loaded.')
    expect(alert).toHaveTextContent('rev 5')
    expect(alert).toHaveTextContent('Nothing was changed.')
    expect(alert).not.toHaveTextContent('claimed this request first')
  })

  it('states a plain failure in place rather than throwing it away', async () => {
    const user = userEvent.setup()
    server.use(route('post', '*/api/teams/4/requests/7/assign', { error: 'user 99 has no access to team 4’s inbox' }, 400))
    render(<RequestTriageRail request={request({ claim_user_id: null, owner_user_id: null })} meId={ME} members={members} canAssign />)
    await user.selectOptions(screen.getByRole('combobox', { name: 'Assign to a team member' }), '44')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('has no access to team 4')
    expect(alert).toHaveTextContent('Nothing was changed.')
  })

  it('disables the assignee select for someone who is not a team manager, and says why', () => {
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign={false} />)
    expect(screen.getByRole('combobox', { name: 'Assign to a team member' })).toBeDisabled()
    expect(screen.getByText('Only a team lead or an org owner/admin can assign a request.')).toBeInTheDocument()
  })

  it('offers no control that approves, dispatches or completes anything', () => {
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign />)
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /dispatch|start|complete|done/i })).toBeNull()
  })
})

describe('RequestTriageRail: the three triage acts, each refusing to act before it has what it needs', () => {
  const acts = () => ({
    ask: async () => ({ changed: true }),
    reject: async () => ({ changed: true }),
    duplicate: async () => ({ changed: true }),
  })

  it('asks the requester through the page, and disables Send until a question is typed', async () => {
    const user = userEvent.setup()
    const asked: string[] = []
    render(
      <RequestTriageRail request={request()} meId={ME} members={members} canAssign onTransition={{
        ...acts(), ask: async q => { asked.push(q); return { changed: true } },
      }} />,
    )
    await user.click(screen.getByRole('button', { name: 'Ask the requester' }))
    const send = screen.getByRole('button', { name: /Send question/ })
    expect(send).toBeDisabled()
    await user.type(screen.getByRole('textbox', { name: 'Question to the requester' }), 'Which group is the default?')
    expect(send).toBeEnabled()
    await user.click(send)
    expect(asked).toEqual(['Which group is the default?'])
    expect(await screen.findByRole('status')).toHaveTextContent('Needs information')
    // It says the answer is obliged, which is invariant I5's user-facing half.
    expect(screen.queryByText(/Asking obliges an answer/)).toBeNull() // the form closed
  })

  it('says asking obliges an answer while the question is being written', async () => {
    const user = userEvent.setup()
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign onTransition={acts()} />)
    await user.click(screen.getByRole('button', { name: 'Ask the requester' }))
    expect(screen.getByText(/Asking obliges an answer/)).toBeInTheDocument()
    expect(screen.getByText(/no SLA clock on the requester/)).toBeInTheDocument()
  })

  it('will not submit a rejection with no reason', async () => {
    const user = userEvent.setup()
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign onTransition={acts()} />)
    await user.click(screen.getByRole('button', { name: /Reject/ }))
    const confirm = screen.getByRole('button', { name: /Reject request/ })
    expect(confirm).toBeDisabled()
    expect(screen.getByText(/A rejection carries a reason and an actor/)).toBeInTheDocument()
  })

  it('will not fold into a duplicate without a request id', async () => {
    const user = userEvent.setup()
    const folded: number[] = []
    render(
      <RequestTriageRail request={request()} meId={ME} members={members} canAssign onTransition={{
        ...acts(), duplicate: async id => { folded.push(id); return { changed: true } },
      }} />,
    )
    await user.click(screen.getByRole('button', { name: /Mark duplicate/ }))
    const confirm = screen.getByRole('button', { name: /Mark duplicate/ })
    expect(confirm).toBeDisabled()
    await user.type(screen.getByRole('textbox', { name: 'The canonical request' }), '9')
    await user.click(confirm)
    expect(folded).toEqual([9])
  })

  it('offers none of the three acts when the page does not offer them', () => {
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign />)
    expect(screen.queryByRole('button', { name: 'Ask the requester' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Reject/ })).toBeNull()
  })

  it('follows the real transition graph: a rejected request can still be asked about (that edge reopens it), but not re-rejected', () => {
    render(<RequestTriageRail request={request({ state: 'rejected' })} meId={ME} members={members} canAssign onTransition={acts()} />)
    // Asking from rejected is the graph's reopen edge (rejected → needs_information).
    expect(screen.getByRole('button', { name: 'Ask the requester' })).toBeEnabled()
    expect(screen.getByRole('button', { name: /Reject/ })).toBeDisabled()
    expect(screen.getByRole('button', { name: /Mark duplicate/ })).toBeDisabled()
  })

  it('explains a delivered request instead of offering triage acts', () => {
    render(<RequestTriageRail request={request({ state: 'delivered' })} meId={ME} members={members} canAssign onTransition={acts()} />)
    expect(screen.queryByRole('button', { name: 'Ask the requester' })).toBeNull()
    expect(screen.getByText(/Acceptance and reopening after delivery are theirs alone/)).toBeInTheDocument()
  })

  it('says a close is a lead’s act rather than letting a plain member press it', () => {
    render(<RequestTriageRail request={request()} meId={ME} members={members} canAssign={false} onTransition={acts()} />)
    expect(screen.getByRole('button', { name: /Reject/ })).toBeDisabled()
    expect(screen.getByText(/Declining a request is a lead’s act/)).toBeInTheDocument()
  })

  it('does not offer a question on a request nobody has claimed yet', () => {
    render(<RequestTriageRail request={request({ state: 'new', claim_user_id: null })} meId={ME} members={members} canAssign onTransition={acts()} />)
    expect(screen.getByRole('button', { name: 'Ask the requester' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Ask the requester' })).toHaveAttribute('title', 'Claim the request first: a question leaves from Triaging.')
  })

  it('states a refused act in place and keeps the form open so the text is not lost', async () => {
    const user = userEvent.setup()
    render(
      <RequestTriageRail request={request()} meId={ME} members={members} canAssign onTransition={{
        ...acts(), ask: async () => { throw Object.assign(new Error('the request is not in a state to be asked'), { status: 409, rev: 8 }) },
      }} />,
    )
    await user.click(screen.getByRole('button', { name: 'Ask the requester' }))
    await user.type(screen.getByRole('textbox', { name: 'Question to the requester' }), 'Which group?')
    await user.click(screen.getByRole('button', { name: /Send question/ }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('This request changed since your view loaded.')
    expect(alert).toHaveTextContent('Nothing was changed.')
    // The typed question survives the refusal.
    expect(screen.getByRole('textbox', { name: 'Question to the requester' })).toHaveValue('Which group?')
  })
})

// ---- Planning ----

describe('RequestPlanningRail: the handoff warning is un-removable', () => {
  const warning = /Handoff never auto-approves/

  it('states the warning in every planning state', () => {
    const states: Partial<TeamRequest>[] = [
      {}, // unplanned, no route
      { state: 'triaging' },
      { state: 'needs_information' },
      { state: 'delivered' },
      { state: 'rejected' },
      { state: 'duplicate' },
    ]
    for (const over of states) {
      const { unmount } = render(
        <RequestPlanningRail request={request(over)} links={[]} outcome={outcome()} canPlan onHandoff={() => {}} />,
      )
      expect(screen.getByText(warning)).toBeInTheDocument()
      expect(screen.getByText(/awaiting a person’s approval/)).toBeInTheDocument()
      expect(screen.getByText(/Nothing here dispatches work/)).toBeInTheDocument()
      unmount()
    }
  })

  it('keeps the warning even when the viewer cannot plan (a closed request, a non-member)', () => {
    render(<RequestPlanningRail request={request({ state: 'rejected' })} links={[]} outcome={outcome()} canPlan={false} />)
    expect(screen.getByText(warning)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Create plan items/ })).toBeNull()
    expect(screen.getByText(/Reopen it before planning work/)).toBeInTheDocument()
  })

  it('shows the route and each plan item with its own approval fact, never inferred from state', () => {
    render(
      <RequestPlanningRail
        request={request()}
        outcome={outcome({ linked: 2 })}
        canPlan
        links={[
          link({ id: 1, link_kind: 'routes_to', target_kind: 'project', target_id: 3, target: { kind: 'project', id: 3, title: 'AMP v2' } }),
          link({ id: 2, target: { kind: 'task', id: 3712, title: 'AMP-V2-3712 — Berlin SSO group mapping', approved: false } }),
          link({ id: 3, target_kind: 'task', target_id: 3711, target: { kind: 'task', id: 3711, title: 'AMP-V2-3711 — Apply group mapping', approved: true } }),
        ]}
      />,
    )
    expect(screen.getByText(/routes to:/)).toHaveTextContent('AMP v2')
    expect(screen.getByText('awaiting approval')).toBeInTheDocument()
    expect(screen.getByText('approved', { selector: 'span[data-tone="ok"]' })).toBeInTheDocument()
    // The count is stated, not left for the reader to add up.
    expect(screen.getByText((_, el) => el?.tagName === 'SPAN' && /awaiting a person$/.test(el.textContent ?? '') && el.children.length === 1))
      .toBeInTheDocument()
    // No control anywhere on the rail approves a plan item.
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()
  })

  it('marks a deleted plan item as a tombstone instead of hiding the row', () => {
    render(
      <RequestPlanningRail
        request={request()}
        outcome={outcome()}
        canPlan
        links={[link({ target: { kind: 'task', id: 3712, title: 'AMP-V2-3712 — Berlin SSO group mapping', gone: true } })]}
      />,
    )
    expect(screen.getByText('deleted')).toBeInTheDocument()
    // The row keeps the name the plan had, so the tombstone is readable.
    expect(screen.getByText('Berlin SSO group mapping')).toBeInTheDocument()
    expect(screen.getByText('AMP-V2-3712')).toBeInTheDocument()
  })

  it('shows a link whose target cannot be resolved at all as no-longer-available, not as approved', () => {
    render(
      <RequestPlanningRail
        request={request()}
        outcome={outcome()}
        canPlan
        links={[link({ target: undefined })]}
      />,
    )
    expect(screen.getByText('This item is no longer available')).toBeInTheDocument()
    expect(screen.getByText('awaiting approval')).toBeInTheDocument()
  })

  it('says a handoff needs a routed project instead of offering a button that cannot work', () => {
    render(<RequestPlanningRail request={request()} links={[]} outcome={outcome()} canPlan />)
    expect(screen.queryByRole('button', { name: /Create plan items/ })).toBeNull()
    expect(screen.getByText(/Route the request to a project before planning items/)).toBeInTheDocument()
  })

  it('explains a refused attach: a second route is a deliberate re-route, not a silent retry', async () => {
    const user = userEvent.setup()
    const onAttach = async () => { throw Object.assign(new Error('request 7 is routed to project 3'), { status: 409 }) }
    render(
      <RequestPlanningRail
        request={request()}
        outcome={outcome()}
        canPlan
        onAttach={onAttach}
        links={[link({ id: 1, link_kind: 'routes_to', target_kind: 'project', target_id: 3 })]}
        onChangeProject={() => {}}
      />,
    )
    await user.type(screen.getByRole('textbox', { name: "The task's id" }), '3712')
    await user.click(screen.getByRole('button', { name: 'Attach' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('already routed to a project')
    expect(alert).toHaveTextContent('One project per request')
    expect(within(alert).getByRole('button', { name: /Change project/ })).toBeInTheDocument()
  })

  it('reports a replayed attach as "already attached" rather than a second link', async () => {
    const user = userEvent.setup()
    render(
      <RequestPlanningRail
        request={request()}
        outcome={outcome()}
        canPlan
        onAttach={async () => ({ changed: false })}
        links={[link({ id: 1, link_kind: 'routes_to', target_kind: 'project', target_id: 3 })]}
      />,
    )
    await user.type(screen.getByRole('textbox', { name: "The task's id" }), '3712')
    await user.click(screen.getByRole('button', { name: 'Attach' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Already attached: nothing changed.')
  })
})

// ---- Deliver ----

describe('RequestDeliverPanel: the evidence guard and its explanations', () => {
  it('disables Mark delivered until a brief and evidence exist, with the reason on screen', () => {
    render(
      <RequestDeliverPanel
        request={request({ state: 'in_progress' })}
        outcome={outcome({ blocked_reason: 'delivery needs the outcome brief (what is and is not included)' })}
        evidence={[]}
        canDeliver
        onDeliver={async () => ({ request: { id: 7, public_ref: 'x', state: 'delivered', rev: 5 }, changed: true })}
      />,
    )
    const button = screen.getByRole('button', { name: /Mark delivered/ })
    expect(button).toBeDisabled()
    // The reason is a sentence beside the button and its accessible description,
    // not only a title nobody opens.
    expect(screen.getByText('delivery needs the outcome brief (what is and is not included)')).toBeInTheDocument()
    expect(button).toHaveAttribute('aria-describedby', 'deliver-guard')
    expect(document.getElementById('deliver-guard')).toHaveTextContent('delivery needs the outcome brief')
  })

  it('stays disabled when the work is all done but the server has not made it eligible (I8)', () => {
    render(
      <RequestDeliverPanel
        request={request({ state: 'in_progress', outcome_brief: 'Includes: new sign-ins.' })}
        outcome={outcome({ total: 3, done: 3, items: [], phase: 'in_progress', outcome_brief_set: true, delivered_eligible: false })}
        evidence={[]}
        canDeliver
        onDeliver={async () => ({ request: { id: 7, public_ref: 'x', state: 'delivered', rev: 5 }, changed: true })}
      />,
    )
    // Somebody typed a brief, so the one thing left is the server's eligibility.
    expect(screen.getByRole('button', { name: /Mark delivered/ })).toBeDisabled()
    expect(screen.getByText(/Done isn’t delivered/)).toBeInTheDocument()
    expect(screen.getByText(/closing work never delivers a request/)).toBeInTheDocument()
  })

  it('enables the button only once the aggregate is eligible AND a brief is written', async () => {
    const user = userEvent.setup()
    let posted: unknown = null
    server.use(http.post('*/api/teams/4/requests/7/deliver', async ({ request: r }) => {
      posted = await r.json()
      calls.push({ method: r.method, path: new URL(r.url).pathname, ifMatch: r.headers.get('if-match') })
      return HttpResponse.json({ request: { id: 7, public_ref: 'x', state: 'delivered', rev: 5 }, changed: true })
    }))
    render(
      <RequestDeliverPanel
        request={request({ state: 'in_progress', outcome_brief: 'Includes: new sign-ins.' })}
        outcome={outcome({ total: 1, done: 1, outcome_brief_set: true, delivered_eligible: true })}
        evidence={[link({ link_kind: 'delivered_by', target_kind: 'task', target_id: 3711 })]}
        canDeliver
        onDeliver={async input => {
          const { requestApi } = await import('../../api/client')
          return requestApi.deliver(4, 7, { outcome_brief: input.outcome_brief, no_artifact: input.no_artifact, expected_rev: 3 })
        }}
      />,
    )
    const button = screen.getByRole('button', { name: /Mark delivered/ })
    expect(button).toBeDisabled()
    await user.type(screen.getByRole('textbox', { name: 'Outcome brief' }), 'Includes: new sign-ins. Not included: moving existing accounts.')
    expect(button).toBeEnabled()
    await user.click(button)
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toMatchObject({ method: 'POST', path: '/api/teams/4/requests/7/deliver', ifMatch: '"3"' })
    expect(posted).toMatchObject({
      outcome_brief: 'Includes: new sign-ins. Not included: moving existing accounts.',
      no_artifact: false,
      expected_rev: 3,
    })
  })

  it('tells a non-team viewer it cannot deliver, rather than showing a live button', () => {
    render(
      <RequestDeliverPanel
        request={request({ state: 'in_progress' })}
        outcome={outcome({ delivered_eligible: true, outcome_brief_set: true, total: 1 })}
        evidence={[]}
        canDeliver={false}
      />,
    )
    expect(screen.getByRole('button', { name: /Mark delivered/ })).toBeDisabled()
    expect(screen.getByText('Only the inbox team can deliver this request.')).toBeInTheDocument()
  })

  it('shows the delivery with each piece’s own state once delivered, and states that acceptance is the requester’s', () => {
    render(
      <RequestDeliverPanel
        request={request({ state: 'delivered', outcome_brief: 'Includes: new sign-ins.', delivered_at: '2026-11-14T08:00:00Z' })}
        outcome={outcome({
          phase: 'delivered', total: 2, done: 1, gated: 1, outcome_brief_set: true, delivered_eligible: true,
          items: [
            { kind: 'artifact', id: 41, state: 'done' },
            { kind: 'task', id: 3711, state: 'merge_gated', merge_state: 'needed' },
          ],
        })}
        evidence={[
          link({ id: 1, link_kind: 'delivered_by', target_kind: 'artifact', target_id: 41, target: { kind: 'artifact', id: 41, title: 'Artifact #41 — Berlin SSO mapping runbook' } }),
          link({ id: 2, link_kind: 'delivered_by', target_kind: 'task', target_id: 3711, target: { kind: 'task', id: 3711, title: 'AMP-V2-3711 — Apply group mapping' } }),
        ]}
        canDeliver
        requesterLabel="it-admin@acme.example"
      />,
    )
    expect(screen.getByText('Includes: new sign-ins.')).toBeInTheDocument()
    expect(screen.getByText('approved')).toBeInTheDocument()
    expect(screen.getByText('merge needed')).toBeInTheDocument()
    // The held/gated note is stated, not implied by the tag alone.
    expect(screen.getByText(/merge is not recorded/)).toBeInTheDocument()
    expect(screen.getByText(/Acceptance belongs to the requester alone/)).toHaveTextContent('Waiting on it-admin@acme.example.')
    // There is deliberately no Accept button: acceptance is the requester's own act (I1/D5).
    expect(screen.queryByRole('button', { name: /accept/i })).toBeNull()
  })

  it('renders the panel when the server sends items: null, the shape every undelivered request has', () => {
    // The real API marshals the aggregate's items as `null` (a nil Go slice:
    // request_outcome.go appends onto a nil Items), so this is the FIRST render
    // of every request that has no delivered_by link — not an edge case. A bare
    // `for...of outcome.items` here threw and blanked the whole detail page
    // (AMP #4282); the fixture defaults to `[]`, which is why it went unseen.
    render(
      <RequestDeliverPanel
        request={request({ state: 'in_progress' })}
        outcome={outcome({ phase: 'unlinked', items: null })}
        evidence={[]}
        canDeliver
        onDeliver={async () => ({ request: { id: 7, public_ref: 'x', state: 'delivered', rev: 5 }, changed: true })}
      />,
    )
    // The panel rendered rather than throwing: its own act and its empty-evidence
    // facts are both present.
    expect(screen.getByRole('button', { name: /Mark delivered/ })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Outcome brief' })).toBeInTheDocument()
    expect(screen.getByText(/No delivered work is linked yet/)).toBeInTheDocument()
  })

  it('explains a closed request instead of asking for a brief it can never use', () => {
    render(
      <RequestDeliverPanel
        request={request({ state: 'rejected' })}
        outcome={outcome()}
        evidence={[]}
        canDeliver
      />,
    )
    expect(screen.getByText('Nothing to deliver.')).toBeInTheDocument()
    expect(screen.getByText(/Reopen it first/)).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: 'Outcome brief' })).toBeNull()
  })

  it('never becomes deliverable from a state the lifecycle does not deliver out of', () => {
    const notInProgress: RequestStatus[] = ['new', 'triaging', 'needs_information', 'accepted']
    for (const state of notInProgress) {
      const { unmount } = render(
        <RequestDeliverPanel
          request={request({ state, outcome_brief: 'x' })}
          outcome={outcome({ delivered_eligible: true, outcome_brief_set: true, total: 1 })}
          evidence={[]}
          canDeliver
        />,
      )
      expect(screen.getByRole('button', { name: /Mark delivered/ })).toBeDisabled()
      unmount()
    }
  })
})
