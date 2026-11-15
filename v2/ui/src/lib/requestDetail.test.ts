// The detail page's decisions (AMP #3984, build slice J2's page half).
//
// Every decision the page makes that a reader could argue with is a pure
// function in `lib/requestDetail.ts`, so it is proved here rather than by
// clicking through a rendered page. The four that carry weight:
//
//  1. **Which arm a read answer is.** The detail route answers a team record or
//     a requester projection, and the projection has no record in it. A page that
//     coerced one into the other — or fell back to an empty record — is the auth
//     bypass the ticket names, so the resolution is a table over the shapes the
//     route can actually send, including a `view` this build does not know.
//  2. **Who may act.** `RequireTeamManager` passes for an org owner/admin with no
//     team row, so inferring standing from the member list alone would hide
//     Reject from the person the server would allow it for.
//  3. **Reopen.** The team's only reopen edges are rejected/duplicate →
//     triaging; delivery's reopen is the requester's alone (I1). The page must
//     offer the first, refuse the second with the reason, and never invent an
//     edge the transition graph does not have.
//  4. **The six approved detail states are reachable**, as a table, plus the
//     panels each state shows — the mockup's own rule that a Clarification card
//     does not appear on a request nobody asked about.
import { describe, expect, it } from 'vitest'
import { REQUEST_STATUSES } from '../types'
import type { Member, RequestOutcome, RequestReadAnswer, TeamRequest } from '../types'
import {
  DETAIL_STATE_REACHABLE, MOCKUP_DETAIL_STATES, NO_SUB_READS, REQUESTER_VIEW_NOTICE, briefMutability,
  collectForwardPages, detailLinkKey, detailPanels, detailState, duplicateFacts, evidenceLinkRows, inboxStanding,
  mockupStateName, namedLinkRows, notAMemberReason, planningLinkRows, rejectionFacts, reopenCopy, reopenOffer,
  resolveDetailRead, routedProjectLink, subReadFailureNotice, subReadTruncationNotice, teamThreadPage, withRouteTitle,
  DETAIL_STATES,
} from './requestDetail'
import type { ResolvedRequestLink } from './requestControls'

const ME = 12

const request = (over: Partial<TeamRequest> = {}): TeamRequest => ({
  id: 7, public_ref: 'a1b2c3d4e5f6g7h8i9j0', org_id: 2, team_id: 4,
  requester_user_id: 31, requester_contact: 'it-admin@acme.example',
  source_kind: 'web', source_external_id: '',
  subject: 'SSO group mapping for the Berlin office',
  input_brief: 'We need the group mapping set up.', outcome_brief: '',
  state: 'triaging', claim_user_id: ME, claimed_at: '2026-11-13T08:00:00Z', owner_user_id: ME,
  duplicate_of: null, reject_reason: '', needs_info_since: null,
  planned_at: null, delivered_at: null, accepted_at: null, accepted_by: null,
  closed_at: null, reopened_at: null, reopened_count: 0,
  rev: 7, metadata: {}, created_at: '2026-11-12T09:00:00Z', updated_at: '2026-11-13T10:00:00Z',
  ...over,
})

const outcome = (over: Partial<RequestOutcome> = {}): RequestOutcome => ({
  phase: 'in_progress', items: [], total: 0, done: 0, open: 0, blocked: 0, gated: 0, held: 0,
  deleted: 0, unverified: 0, rework: 0, linked: 0,
  outcome_brief_set: false, no_artifact: false, delivered_eligible: false,
  ...over,
})

const members: Member[] = [
  { user_id: ME, email: 'jacob@simstech.cloud', display_name: 'Jacob Sims', role: 'lead' },
  { user_id: 44, email: 'priya@simstech.cloud', display_name: 'Priya Nair', role: 'member' },
]

const link = (over: Partial<ResolvedRequestLink> = {}): ResolvedRequestLink => ({
  id: 1, request_id: 7, link_kind: 'planned_as', target_kind: 'task', target_id: 3712,
  created_by: ME, created_at: '2026-11-13T09:00:00Z',
  target: { kind: 'task', id: 3712, title: 'AMP-V2-3712 — Berlin SSO group mapping', approved: false },
  ...over,
})

// ---- 1. Which arm the read answered ----

describe('resolveDetailRead', () => {
  it('resolves the team arm to the record and its aggregate, both required', () => {
    const req = request()
    const out = outcome()
    expect(resolveDetailRead({ view: 'team', request: req, outcome: out })).toEqual({ kind: 'team', request: req, outcome: out })
  })

  it('resolves the requester arm to the projection and never to a record', () => {
    const read = resolveDetailRead({
      view: 'requester',
      projection: {
        public_ref: 'a1b2c3d4e5f6g7h8i9j0', state: 'triaging', state_word: 'Being reviewed',
        subject: 'SSO', input_brief: 'brief', created_at: '2026-11-12T09:00:00Z',
        milestones: [], comments: [], outputs: [],
      },
    })
    expect(read).toEqual({ kind: 'requester', publicRef: 'a1b2c3d4e5f6g7h8i9j0', state: 'triaging' })
    // The arm carries no record field at all: nothing a component could read a
    // claim, an assignee or a revision off.
    expect(read && 'request' in read).toBe(false)
  })

  it('branches on the server’s view word, not on which field happens to be present', () => {
    // A `requester` answer that somehow carried a record is still the projection:
    // the view word is the server's decision, and the page must not upgrade it.
    const smuggled = resolveDetailRead({
      view: 'requester',
      projection: { public_ref: 'x', state: 'new', state_word: 'Received', subject: 's', input_brief: '', created_at: '', milestones: [], comments: [], outputs: [] },
      request: request(),
    } as unknown as RequestReadAnswer)
    expect(smuggled?.kind).toBe('requester')
  })

  it('fails closed on a view it does not know, rather than rendering a triage screen', () => {
    const read = resolveDetailRead({ view: 'collaborator', request: request() } as unknown as RequestReadAnswer)
    expect(read).toEqual({ kind: 'other', view: 'collaborator' })
    expect(read && 'request' in read).toBe(false)
  })

  it('answers null for a missing answer or one missing half of the team pair', () => {
    expect(resolveDetailRead(null)).toBeNull()
    expect(resolveDetailRead(undefined)).toBeNull()
    expect(resolveDetailRead({ view: 'team', request: request() } as unknown as RequestReadAnswer)).toBeNull()
    expect(resolveDetailRead({ view: 'team', outcome: outcome() } as unknown as RequestReadAnswer)).toBeNull()
  })

  it('states on screen that a requester’s answer is not the triage page', () => {
    // The sentence is the page's whole answer to the second arm; it has to say
    // what is *not* shown, not just what is.
    expect(REQUESTER_VIEW_NOTICE).toContain('claim')
    expect(REQUESTER_VIEW_NOTICE).toContain('not shown here')
  })
})

// ---- 2. Who may act ----

describe('inboxStanding', () => {
  it('reads a plain member, a lead, and a stranger from the team’s rows', () => {
    expect(inboxStanding(44, members)).toEqual({ member: true, manager: false })
    expect(inboxStanding(ME, members)).toEqual({ member: true, manager: true })
    expect(inboxStanding(99, members)).toEqual({ member: false, manager: false })
    expect(inboxStanding(undefined, members)).toEqual({ member: false, manager: false })
  })

  it('admits an org owner or admin with no team row — the server’s RequireTeamManager does', () => {
    // `RequireTeamManager`: `if p.Super || role == owner || role == admin { return nil }`
    // before it ever reads a team row. A page inferring "manager" from `members`
    // alone would hide Reject from exactly that person.
    expect(inboxStanding(99, members, { orgRole: 'owner' })).toEqual({ member: true, manager: true })
    expect(inboxStanding(99, members, { orgRole: 'admin' })).toEqual({ member: true, manager: true })
    expect(inboxStanding(99, members, { orgRole: 'member' })).toEqual({ member: false, manager: false })
    expect(inboxStanding(99, members, { admin: true })).toEqual({ member: true, manager: true })
  })

  it('does not promote a plain member to manager from an org role it does not have', () => {
    expect(inboxStanding(44, members, { orgRole: 'member' })).toEqual({ member: true, manager: false })
  })

  it('explains what a non-member is missing instead of silently hiding the acts', () => {
    const reason = notAMemberReason()
    expect(reason).toContain('inbox team')
    expect(reason).toContain('requester')
  })
})

// ---- 3. Reopen ----

describe('reopenOffer', () => {
  const member = { member: true, manager: false }
  const stranger = { member: false, manager: false }

  it('offers Reopen on the graph’s two team reopen edges, into Triaging', () => {
    for (const state of ['rejected', 'duplicate'] as const) {
      const offer = reopenOffer({ state }, member)
      expect(offer).toEqual({ offered: true, to: 'triaging', reason: '' })
      expect(reopenCopy(offer).label).toBe('Reopen')
    }
  })

  it('never offers it on an open request', () => {
    for (const state of ['new', 'triaging', 'needs_information', 'accepted', 'in_progress'] as const) {
      const offer = reopenOffer({ state }, member)
      expect(offer.offered).toBe(false)
      expect(offer.reason).toContain('nothing to reopen')
    }
  })

  it('refuses it on a delivery and says whose act it is (I1)', () => {
    for (const state of ['delivered', 'requester_accepted'] as const) {
      const offer = reopenOffer({ state }, member)
      expect(offer.offered).toBe(false)
      // The team cannot take a delivery back: acceptance and reopening after
      // delivery are the requester's alone.
      expect(offer.reason).toContain('requester')
      expect(offer.reason).toContain('I1')
    }
  })

  it('refuses it for a stranger on a closed request, with the reason', () => {
    const offer = reopenOffer({ state: 'rejected' }, stranger)
    expect(offer.offered).toBe(false)
    expect(offer.reason).toBe(notAMemberReason())
  })
})

// ---- 4. The approved states, and what each shows ----

describe('the approved detail states', () => {
  it('maps every lifecycle state onto one of the mockup’s six readings', () => {
    // Totality: a state the mockup does not draw must still land somewhere
    // deliberate rather than falling through to a default that was never chosen.
    for (const state of REQUEST_STATUSES) {
      const name = mockupStateName(detailState(state, null))
      expect(MOCKUP_DETAIL_STATES).toContain(name)
    }
  })

  it('maps the working middle to filled and acceptance to delivered', () => {
    expect(mockupStateName(detailState('new', null))).toBe('filled')
    expect(mockupStateName(detailState('triaging', null))).toBe('filled')
    expect(mockupStateName(detailState('accepted', null))).toBe('filled')
    expect(mockupStateName(detailState('in_progress', null))).toBe('filled')
    expect(mockupStateName(detailState('needs_information', null))).toBe('needsinfo')
    expect(mockupStateName(detailState('requester_accepted', null))).toBe('delivered')
  })

  it('layers a live conflict over the request’s own reading', () => {
    expect(detailState('triaging', { kind: 'claim' })).toEqual({ kind: 'conflict' })
    expect(detailState('triaging', { kind: 'stale' })).toEqual({ kind: 'stale' })
    expect(mockupStateName(detailState('triaging', { kind: 'claim' }))).toBe('conflict')
    // Without a failure the request's own state is what is shown.
    expect(detailState('triaging', null)).toEqual({ kind: 'state', state: 'triaging' })
  })

  it('reaches every approved mockup state from a real request state', () => {
    const reached = new Set([
      ...DETAIL_STATE_REACHABLE.map(r => r.mockup),
      // conflict is not a state: it is the 409 overlay, proved above.
      'conflict',
    ])
    for (const name of MOCKUP_DETAIL_STATES) expect(reached).toContain(name)
    // And each table entry really maps to the state it claims.
    for (const r of DETAIL_STATE_REACHABLE) {
      expect(mockupStateName(detailState(r.state, null))).toBe(r.mockup)
    }
  })

  it('keeps the nine lifecycle states as the server has them', () => {
    expect(DETAIL_STATES).toHaveLength(9)
    expect(new Set(DETAIL_STATES).size).toBe(9)
  })
})

describe('detailPanels', () => {
  it('states the request block, the thread and the history in every state', () => {
    for (const state of REQUEST_STATUSES) {
      const p = detailPanels(state)
      expect(p.request).toBe(true)
      expect(p.discussion).toBe(true)
      expect(p.history).toBe(true)
    }
  })

  it('ties each card to the state that has the fact', () => {
    expect(detailPanels('needs_information').clarification).toBe(true)
    expect(detailPanels('triaging').clarification).toBe(false)
    expect(detailPanels('rejected').rejection).toBe(true)
    expect(detailPanels('duplicate').rejection).toBe(false)
    expect(detailPanels('duplicate').duplicate).toBe(true)
    expect(detailPanels('delivered').delivery).toBe(true)
    expect(detailPanels('requester_accepted').delivery).toBe(true)
    expect(detailPanels('in_progress').delivery).toBe(false)
  })

  it('offers no triage act on a delivery, and no planning or delivery on a close', () => {
    // A delivered request is the requester's to accept now; a closed one is
    // reopened before planning means anything.
    expect(detailPanels('delivered').triageActs).toBe(false)
    expect(detailPanels('requester_accepted').triageActs).toBe(false)
    expect(detailPanels('triaging').triageActs).toBe(true)
    for (const state of ['rejected', 'duplicate'] as const) {
      expect(detailPanels(state).planning).toBe(false)
      expect(detailPanels(state).deliverPanel).toBe(false)
      // The replacing act is the page's Reopen, not a panel's.
      expect(detailPanels(state).triageActs).toBe(true)
    }
  })
})

// ---- The record's three sub-reads ----

describe('the sub-reads', () => {
  it('starts every panel unread, which is neither empty nor failed', () => {
    // The distinction the whole mechanism is for: a panel with no rows and no
    // failure has not been read, so it must say nothing about the record.
    for (const name of ['discussion', 'history', 'links'] as const) {
      expect(NO_SUB_READS[name]).toEqual({ rows: null, failure: null })
    }
  })

  it('reads only the team arm of a discussion answer, and keeps its cursor', () => {
    const rows = [{ id: 1, request_id: 7, kind: 'comment' as const, body: 'hi', author: 'a', author_user_id: null, created_at: '' }]
    expect(teamThreadPage({ view: 'team', request_id: 7, count: 1, comments: rows })).toEqual({ rows, nextAfterId: null })
    expect(teamThreadPage({ view: 'team', request_id: 7, count: 1, next_after_id: 41, comments: rows }))
      .toEqual({ rows, nextAfterId: 41 })
    // An empty team thread is a real answer: it is the record saying nobody has
    // spoken, which the page may state.
    expect(teamThreadPage({ view: 'team', request_id: 7, count: 0, comments: [] })).toEqual({ rows: [], nextAfterId: null })
  })

  it('refuses a requester’s thread rather than reading it as an empty one', () => {
    // A requester's answer carries `shared_comments` and omits `comments`. A
    // page that read `comments ?? []` would draw "No discussion yet" over a
    // thread it never received.
    expect(teamThreadPage({ view: 'requester', count: 2, shared_comments: [] })).toBeNull()
    expect(teamThreadPage(null)).toBeNull()
    expect(teamThreadPage(undefined)).toBeNull()
  })

  it('states a failure as a failure, and never as an empty list', () => {
    for (const what of ['discussion', 'history', 'links'] as const) {
      const never = subReadFailureNotice(what, false)
      // Three facts: which read, that it is not empty, and what to do.
      expect(never).toContain('did not load')
      expect(never).toContain('not an empty')
      expect(never).toContain('retry')
      expect(never).toContain(what === 'links' ? 'linked work' : what)
    }
  })

  it('says a failed re-read left the rows on screen alone', () => {
    // Over rows already shown, the rows are real — they are the last successful
    // read's — so calling the list "empty" would contradict what is visible.
    const over = subReadFailureNotice('discussion', true)
    expect(over).toContain('Re-reading the discussion failed')
    expect(over).toContain('last successful read')
    expect(over).not.toContain('not an empty')
  })

  it('says a bounded read stopped short instead of presenting it as the whole list', () => {
    for (const what of ['discussion', 'history', 'links'] as const) {
      const sentence = subReadTruncationNotice(what)
      expect(sentence).toContain('More')
      expect(sentence).toContain('not all of it')
    }
  })

  it('follows a forward cursor to the end, in append order', async () => {
    const pages = [
      { rows: ['a', 'b'], nextAfterId: 2 },
      { rows: ['c'], nextAfterId: 3 },
      { rows: ['d'], nextAfterId: null },
    ]
    const seen: (number | null)[] = []
    const got = await collectForwardPages(async after => {
      seen.push(after)
      return pages[seen.length - 1]
    })
    expect(got).toEqual({ rows: ['a', 'b', 'c', 'd'], truncated: false })
    // The first page asks for no cursor, each later one for the server's own.
    expect(seen).toEqual([null, 2, 3])
  })

  it('stops at the page bound and says it did', async () => {
    let calls = 0
    const got = await collectForwardPages(async () => {
      calls += 1
      // Every page claims another exists, so only the bound can stop it.
      return { rows: [calls], nextAfterId: calls * 10 }
    }, 3)
    expect(got).toEqual({ rows: [1, 2, 3], truncated: true })
    expect(calls).toBe(3)
  })

  it('propagates a failed page rather than returning a partial list as the whole one', async () => {
    await expect(collectForwardPages<string>(async () => { throw new Error('the read failed') }))
      .rejects.toThrow('the read failed')
  })

  it('reconstructs the route row from the one link-ish fact the aggregate carries', () => {
    const route = routedProjectLink(request(), outcome({ routed_project_id: 12, linked: 1 }))
    expect(route).not.toBeNull()
    expect(route?.link_kind).toBe('routes_to')
    expect(route?.target_kind).toBe('project')
    expect(route?.target_id).toBe(12)
    expect(route?.target?.title).toBe('Project #12')

    expect(routedProjectLink(request(), outcome({ routed_project_id: 0 }))).toBeNull()
  })

  it('puts the aggregate’s active route first and never doubles it', () => {
    const out = outcome({ routed_project_id: 12 })
    // With no rows read, the aggregate's route is still the rail's only row: a
    // request the aggregate counted as routed must not read as unrouted.
    const alone = planningLinkRows([], request(), out)
    expect(alone).toHaveLength(1)
    expect(alone[0].link_kind).toBe('routes_to')

    // A read that carries the route itself: one route reaches the rail, and it
    // is the aggregate's — the active one, not whichever row came first.
    const stale = link({ id: 9, link_kind: 'routes_to', target_kind: 'project', target_id: 3 })
    const both = planningLinkRows([stale], request(), out)
    expect(both).toHaveLength(1)
    expect(both[0].target_id).toBe(12)
    expect(both[0].id).toBe(0)
  })

  it('keeps the read’s planned rows, in attach order, after the route', () => {
    const out = outcome({ routed_project_id: 12 })
    const rows = planningLinkRows([link({ id: 2 }), link({ id: 3, target_kind: 'epic', target_id: 401 })], request(), out)
    expect(rows.map(r => r.link_kind)).toEqual(['routes_to', 'planned_as', 'planned_as'])
    expect(rows.slice(1).map(r => r.id)).toEqual([2, 3])
  })

  it('leaves the read’s own route in place when the aggregate names none', () => {
    // Not routed now, but the lineage remembers being routed: dropping the row
    // would rewrite the record.
    const historic = link({ link_kind: 'routes_to', target_kind: 'project', target_id: 3 })
    const rows = planningLinkRows([historic], request(), outcome({ routed_project_id: 0 }))
    expect(rows).toHaveLength(1)
    expect(rows[0].id).toBe(historic.id)
  })

  it('takes only delivered_by rows as delivery evidence', () => {
    const rows = [link({ link_kind: 'planned_as' }), link({ link_kind: 'delivered_by' }), link({ link_kind: 'relates_to' })]
    const evidence = evidenceLinkRows(rows)
    expect(evidence).toHaveLength(1)
    expect(evidence[0].link_kind).toBe('delivered_by')
    expect(evidenceLinkRows([])).toEqual([])
  })

  it('names a row nothing resolved by its id, and does not bury it as deleted', () => {
    // The lineage read answers the stored row: no `target`, and so no approval
    // fact and no tombstone. The rails read a missing target as "no longer
    // available", which is a claim about the target's record.
    const bare = link({ link_kind: 'planned_as', target_kind: 'task', target_id: 3712, target: undefined })
    const named = namedLinkRows([bare])
    expect(named).toHaveLength(1)
    expect(named[0].target).toEqual({ kind: 'task', id: 3712, title: '#3712 — unnamed here' })
    expect(named[0].target?.gone).toBeUndefined()
    expect(named[0].target?.approved).toBeUndefined()
    // The relation itself is untouched.
    expect(named[0].link_kind).toBe('planned_as')
    expect(named[0].target_kind).toBe('task')
  })

  it('never overwrites a target the read did resolve', () => {
    const resolved = link({ target: { kind: 'task', id: 3712, title: 'AMP-V2-3712 — Berlin SSO group mapping', approved: true } })
    const gone = link({ id: 2, target: { kind: 'task', id: 99, title: 'AMP-V2-99 — Removed', gone: true } })
    const named = namedLinkRows([resolved, gone])
    expect(named[0]).toBe(resolved)
    expect(named[1]).toBe(gone)
  })

  it('fills the route’s name in once the project read answers, and leaves the id when it does not', () => {
    const route = routedProjectLink(request(), outcome({ routed_project_id: 12 }))!
    const planned = link()
    const titled = withRouteTitle([route, planned], 'AMP v2')
    expect(titled[0].target?.title).toBe('AMP v2')
    // A planned row's title is the target's own, never the project's.
    expect(titled[1].target?.title).toBe('AMP-V2-3712 — Berlin SSO group mapping')

    // A failed project read: the route is still a fact, so the row keeps its id.
    expect(withRouteTitle([route], null)[0].target?.title).toBe('Project #12')
  })

  it('keys a row by its relation identity, so a synthesised route cannot collide', () => {
    expect(detailLinkKey({ link_kind: 'routes_to', target_kind: 'project', target_id: 12 })).toBe('routes_to:project:12')
  })
})

// ---- The request block's own facts ----

describe('briefMutability', () => {
  it('says an untriaged request can still be edited', () => {
    const m = briefMutability({ state: 'new', claim_user_id: null, owner_user_id: null })
    expect(m.immutable).toBe(false)
    expect(m.note).toContain('still edit')
  })

  it('says a triaged one is immutable, which is what UpdateRequest enforces', () => {
    for (const over of [
      { state: 'new' as const, claim_user_id: ME, owner_user_id: null },
      { state: 'new' as const, claim_user_id: null, owner_user_id: ME },
      { state: 'triaging' as const, claim_user_id: null, owner_user_id: null },
      { state: 'in_progress' as const, claim_user_id: null, owner_user_id: null },
    ]) {
      const m = briefMutability(over)
      expect(m.immutable).toBe(true)
      expect(m.note).toContain('thread instead')
    }
  })
})

describe('rejectionFacts and duplicateFacts', () => {
  it('reads the reason verbatim and admits the actor is not in this read', () => {
    const facts = rejectionFacts(request({ reject_reason: ' Out of scope: no hardware procurement. ', closed_at: '2026-11-10T11:02:00Z' }))
    expect(facts.reason).toBe('Out of scope: no hardware procurement.')
    expect(facts.when).toBe('2026-11-10T11:02:00Z')
    // The actor lives in request_events, which the read does not serve. Saying
    // so is the honest answer; guessing a name would not be.
    expect(facts.actorKnown).toBe(false)
  })

  it('treats a zero closed_at as no date', () => {
    expect(rejectionFacts(request({ closed_at: '0001-01-01T00:00:00Z' })).when).toBeNull()
  })

  it('names the canonical by the serial it can prove, and never invents a reference', () => {
    expect(duplicateFacts(request({ duplicate_of: 71 }))).toEqual({ canonicalId: 71, when: null })
    expect(duplicateFacts(request({ duplicate_of: null }))).toEqual({ canonicalId: null, when: null })
    expect(duplicateFacts(request({ duplicate_of: 0 }))).toEqual({ canonicalId: null, when: null })
  })
})
