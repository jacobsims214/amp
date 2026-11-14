// The right rail's pure decisions (AMP #3980, build slice J2).
//
// The ticket's three criteria, one describe each:
//
//  1. **Approved controls and disabled explanations match.** Every control the
//     approved mockup (artifact 37 v1) disables has a sentence saying why, and
//     that sentence comes from here — never from a component's own guess.
//  2. **Claim conflict and link retry are handled.** A 409 that names a holder
//     becomes the banner that names who won; a 409 without one is a stale view;
//     a replay of an attach is success, not a second relation.
//  3. **No automatic approvals or delivery on task completion.** A request whose
//     every linked ticket is done is reported as such and is explicitly not
//     delivered (I8); the delivery gate is the server's `delivered_eligible`,
//     and nothing about ticket state can make it true.
//
// It tests the decisions, not the markup: the components' own render tests are
// in components/requests/RequestControls.test.tsx.
import { describe, expect, it } from 'vitest'
import {
  alreadyLinked, canClaim, canRelease, claimConflictCopy, claimState, deliverBlockReason, deliverGuard,
  deliverGuardSentence, evidenceRows, groupLinks, HANDOFF_WARNING, isPlanned, linkKey, notDeliveredYet,
  outcomeBrief, outcomeSummary, phaseWord, plannedApproval, readAttachFailure, readClaimConflict, triageActs,
  type ResolvedRequestLink,
} from './requestControls'
import type { RequestOutcome, RequestOutcomeItem, RequestStatus, TeamRequest } from '../types'

const req = (over: Partial<TeamRequest> = {}): TeamRequest => ({
  id: 7, public_ref: 'a1b2c3d4e5f6g7h8i9j0', org_id: 2, team_id: 4,
  requester_user_id: 31, requester_contact: 'sam@outside.example',
  source_kind: 'web', source_external_id: '',
  subject: 'SSO group mapping for the Berlin office',
  input_brief: 'We need the group mapping set up.',
  outcome_brief: '', state: 'triaging',
  claim_user_id: null, claimed_at: null, owner_user_id: null,
  duplicate_of: null, reject_reason: '', needs_info_since: null,
  planned_at: null, delivered_at: null, accepted_at: null, accepted_by: null,
  closed_at: null, reopened_at: null, reopened_count: 0,
  rev: 3, metadata: {}, created_at: '2026-11-12T09:00:00Z', updated_at: '2026-11-13T10:00:00Z',
  ...over,
})

const outcome = (over: Partial<RequestOutcome> = {}): RequestOutcome => ({
  phase: 'not_started', items: [], total: 0, done: 0, open: 0, blocked: 0, gated: 0, held: 0,
  deleted: 0, unverified: 0, rework: 0, linked: 0,
  outcome_brief_set: false, no_artifact: false, delivered_eligible: false,
  ...over,
})

const link = (over: Partial<ResolvedRequestLink> = {}): ResolvedRequestLink => ({
  id: 1, request_id: 7, link_kind: 'planned_as', target_kind: 'task', target_id: 3712,
  created_by: 12, created_at: '2026-11-13T09:00:00Z',
  target: { kind: 'task', id: 3712, title: 'AMP-V2-3712 — Berlin SSO group mapping', approved: false },
  ...over,
})

describe('claim and assignee are two fields, never one', () => {
  it('reports unclaimed, mine and someone-else’s as different things', () => {
    expect(claimState(req(), 12)).toEqual({ kind: 'none' })
    expect(claimState(req({ claim_user_id: 12, claimed_at: '2026-11-13T10:00:00Z' }), 12))
      .toEqual({ kind: 'mine', userId: 12, at: '2026-11-13T10:00:00Z' })
    expect(claimState(req({ claim_user_id: 44, claimed_at: '2026-11-12T10:00:00Z' }), 12))
      .toEqual({ kind: 'other', userId: 44, at: '2026-11-12T10:00:00Z' })
  })

  it('offers the claim only when nobody else holds it (never a silent take-over)', () => {
    expect(canClaim(req(), 12)).toBe(true)
    expect(canClaim(req({ claim_user_id: 12 }), 12)).toBe(true)
    expect(canClaim(req({ claim_user_id: 44 }), 12)).toBe(false)
    expect(canClaim(req(), undefined)).toBe(false)
  })

  it('lets only the holder release it', () => {
    expect(canRelease(req({ claim_user_id: 12 }), 12)).toBe(true)
    expect(canRelease(req({ claim_user_id: 44 }), 12)).toBe(false)
    expect(canRelease(req(), 12)).toBe(false)
  })

  it('keeps an owner who is also the claimant as two separate facts on the record', () => {
    // The server clears the claim on transfer; this is the shape the rail shows
    // while a claim is live — both fields set, both rendered under their own label.
    const r = req({ claim_user_id: 12, claimed_at: '2026-11-13T10:00:00Z', owner_user_id: 44 })
    expect(claimState(r, 12).kind).toBe('mine')
    expect(r.owner_user_id).toBe(44)
  })
})

describe('the triage acts follow the server’s transition graph, not the state’s name', () => {
  // Pinned to internal/service/request_transitions_test.go's expectedLifecycle.
  it('allows a question from triaging, accepted and in_progress, and from rejected/duplicate by reopening', () => {
    for (const s of ['triaging', 'accepted', 'in_progress', 'rejected', 'duplicate'] as RequestStatus[]) {
      expect(triageActs(s, true).ask.available).toBe(true)
    }
  })

  it('does not allow a question on a request nobody has claimed yet', () => {
    const acts = triageActs('new', true)
    expect(acts.ask.available).toBe(false)
    expect(acts.ask.reason).toContain('Claim the request first')
  })

  it('makes reject and duplicate a manager’s acts, in every open state', () => {
    for (const s of ['new', 'triaging', 'needs_information', 'accepted', 'in_progress'] as RequestStatus[]) {
      expect(triageActs(s, true).reject.available).toBe(true)
      expect(triageActs(s, true).duplicate.available).toBe(true)
      expect(triageActs(s, false).reject.available).toBe(false)
      expect(triageActs(s, false).duplicate.available).toBe(false)
      expect(triageActs(s, false).reject.reason).toContain('lead’s act')
    }
  })

  it('offers no close once the request is delivered or closed', () => {
    for (const s of ['delivered', 'requester_accepted', 'rejected', 'duplicate'] as RequestStatus[]) {
      expect(triageActs(s, true).reject.available).toBe(false)
      expect(triageActs(s, true).duplicate.available).toBe(false)
    }
  })

  it('offers nothing at all on a delivered request, and says why', () => {
    const acts = triageActs('delivered', true)
    expect(acts.ask.available).toBe(false)
    expect(acts.ask.reason).toContain('only the requester can accept or reopen')
    expect(acts.reject.reason).toContain('the requester decides')
  })
})

describe('a lost claim race is a banner that names the winner and promises nothing was sent', () => {
  it('reads a 409 that names a holder as a claim conflict', () => {
    const err = Object.assign(new Error('request 7 is claimed by user 44'), {
      status: 409, code: 'request.conflict', claimUserId: 44, rev: 4,
    })
    const c = readClaimConflict(err, 3)
    expect(c).not.toBeNull()
    expect(c!.holderUserId).toBe(44)
    expect(c!.rev).toBe(4)
    expect(c!.fromRev).toBe(3)
  })

  it('does not read a bare stale-revision 409 as a claim conflict (there is no winner to name)', () => {
    const err = Object.assign(new Error('request 7 is at rev 5, not rev 3'), {
      status: 409, code: 'request.conflict', rev: 5,
    })
    expect(readClaimConflict(err, 3)).toBeNull()
  })

  it('ignores anything that is not a 409', () => {
    expect(readClaimConflict(Object.assign(new Error('nope'), { status: 500, claimUserId: 44 }), 3)).toBeNull()
    expect(readClaimConflict(null, 3)).toBeNull()
  })

  it('says who won, that claims are first-write-wins, and that nothing was sent', () => {
    const copy = claimConflictCopy({ holderUserId: 44, holderName: 'Priya Nair', rev: 4, fromRev: 3, message: '' })
    expect(copy.title).toBe('Priya Nair claimed this request first.')
    expect(copy.detail).toContain('first-write-wins')
    expect(copy.detail).toContain('rev 3 → 4')
    expect(copy.detail).toContain('Nothing you typed was sent.')
  })

  it('names a user id when the holder is not resolvable, rather than an empty name', () => {
    const copy = claimConflictCopy({ holderUserId: 44, rev: 3, fromRev: 3, message: '' })
    expect(copy.title).toBe('User #44 claimed this request first.')
    expect(copy.detail).not.toContain('rev 3 → 3')
  })
})

describe('planning: the route, the plan items and their own approval facts', () => {
  it('groups the four link kinds without inventing any', () => {
    const g = groupLinks([
      link({ id: 1, link_kind: 'routes_to', target_kind: 'project', target_id: 3, target: { kind: 'project', id: 3, title: 'AMP v2' } }),
      link({ id: 2 }),
      link({ id: 3, link_kind: 'relates_to', target_kind: 'request', target_id: 9 }),
      link({ id: 4, link_kind: 'delivered_by', target_kind: 'artifact', target_id: 41 }),
    ])
    expect(g.route?.target_id).toBe(3)
    expect(g.planned.map(l => l.id)).toEqual([2])
    expect(g.related.map(l => l.id)).toEqual([3])
    expect(g.delivered.map(l => l.id)).toEqual([4])
  })

  it('takes the first active route (a re-route supersedes rather than adding a second)', () => {
    const g = groupLinks([
      link({ id: 1, link_kind: 'routes_to', target_kind: 'project', target_id: 3 }),
      link({ id: 2, link_kind: 'routes_to', target_kind: 'project', target_id: 5 }),
    ])
    expect(g.route?.target_id).toBe(3)
  })

  it('counts awaiting approval from each target’s own fact, and counts an unknown as not approved', () => {
    const counts = plannedApproval(groupLinks([
      link({ id: 1, target: { kind: 'task', id: 1, title: 'AMP-V2-1 — a', approved: true } }),
      link({ id: 2, target: { kind: 'task', id: 2, title: 'AMP-V2-2 — b', approved: false } }),
      link({ id: 3, target: { kind: 'task', id: 3, title: 'AMP-V2-3 — c' } }),
      link({ id: 4, target: { kind: 'task', id: 4, title: 'AMP-V2-4 — d', gone: true } }),
    ]))
    expect(counts).toEqual({ total: 4, approved: 1, awaiting: 2, gone: 1 })
  })

  it('is planned when routed even with no plan items, and not planned when neither exists at all', () => {
    const routed = groupLinks([link({ link_kind: 'routes_to', target_kind: 'project', target_id: 3 })])
    expect(isPlanned(routed)).toBe(true)
    expect(isPlanned(groupLinks([]))).toBe(false)
    expect(isPlanned(groupLinks([link()]))).toBe(true)
  })

  it('states the handoff warning in the approved words, un-removable by any state', () => {
    expect(HANDOFF_WARNING.title).toBe('Handoff never auto-approves.')
    expect(HANDOFF_WARNING.detail).toContain('awaiting a person')
    expect(HANDOFF_WARNING.detail).toContain('quoted, untrusted data')
    expect(HANDOFF_WARNING.detail).toContain('Nothing here dispatches work.')
  })
})

describe('link retry: a replay is success, a second route is a deliberate re-route', () => {
  it('recognises a relation that is already attached, by the server’s own unique key', () => {
    const links = [link({ link_kind: 'planned_as', target_kind: 'task', target_id: 3712 })]
    expect(alreadyLinked(links, { link_kind: 'planned_as', target_kind: 'task', target_id: 3712 })).toBe(true)
    expect(alreadyLinked(links, { link_kind: 'planned_as', target_kind: 'task', target_id: 3713 })).toBe(false)
    expect(alreadyLinked(links, { link_kind: 'delivered_by', target_kind: 'task', target_id: 3712 })).toBe(false)
    expect(linkKey({ link_kind: 'planned_as', target_kind: 'task', target_id: 3712 })).toBe('planned_as:task:3712')
  })

  it('keeps the idempotency in the server, not in the client: the client-side check only says "already"', () => {
    // The rail never writes a link itself; `alreadyLinked` exists so the button
    // can say "already attached" rather than offering the same relation twice.
    // The convergence on a replay is the repository's ON CONFLICT, which a
    // component test cannot prove — so this asserts the shape, not the write.
    expect(alreadyLinked([link()], { link_kind: 'planned_as', target_kind: 'task', target_id: 3712 })).toBe(true)
  })

  it('reads the two attach failures the rail talks about, and nothing else', () => {
    expect(readAttachFailure(Object.assign(new Error('request 7 is routed to project 3'), { status: 409 })).kind).toBe('rerouted')
    const failed = readAttachFailure(Object.assign(new Error('task 99 not found'), { status: 404 }))
    expect(failed).toEqual({ kind: 'failed', message: 'task 99 not found' })
    expect(readAttachFailure(undefined)).toEqual({ kind: 'failed', message: 'The link could not be written.' })
  })
})

describe('delivery: the guard is the server’s, and its explanations are sentences', () => {
  it('is ready only when the server says eligible and the request is in progress', () => {
    const g = deliverGuard(req({ state: 'in_progress' }), outcome({ delivered_eligible: true, outcome_brief_set: true, total: 1 }))
    expect(g).toMatchObject({ ready: true, block: 'none', delivered: false })
  })

  it('does not become ready from a state the lifecycle does not deliver out of, even if the server said eligible', () => {
    // Belt and braces: the delivery edge leaves `in_progress` and nothing else,
    // so a stale or miscomputed aggregate cannot make the button live on a
    // request that is still being triaged.
    const g = deliverGuard(req({ state: 'triaging' }), outcome({ delivered_eligible: true, outcome_brief_set: true, total: 1 }))
    expect(g.ready).toBe(false)
    expect(g.block).toBe('state')
  })

  it('names the missing brief and the missing evidence separately, in the contract’s words', () => {
    const noBrief = deliverGuard(req({ state: 'in_progress' }), outcome({ total: 1 }))
    expect(noBrief.block).toBe('brief')
    expect(noBrief.reason).toBe(deliverBlockReason('brief'))

    const noEvidence = deliverGuard(req({ state: 'in_progress' }), outcome({ outcome_brief_set: true }))
    expect(noEvidence.block).toBe('evidence')
    expect(noEvidence.reason).toBe(deliverBlockReason('evidence'))
    expect(deliverBlockReason('evidence')).toContain('nothing to link')
  })

  it('prefers the server’s own blocked_reason over a sentence composed here', () => {
    const g = deliverGuard(req({ state: 'in_progress' }), outcome({
      outcome_brief_set: true,
      blocked_reason: 'delivery needs the outcome brief (what is and is not included) and a delivered link',
    }))
    expect(g.reason).toBe('delivery needs the outcome brief (what is and is not included) and a delivered link')
  })

  it('is not "blocked" once delivered, and not "needs a brief" once closed', () => {
    const delivered = deliverGuard(req({ state: 'delivered' }), outcome({ delivered_eligible: true, outcome_brief_set: true, total: 1, phase: 'delivered' }))
    expect(delivered).toMatchObject({ ready: false, block: 'already', delivered: true })
    expect(delivered.reason).toContain('requester alone')

    const rejected = deliverGuard(req({ state: 'rejected' }), outcome())
    expect(rejected.block).toBe('closed')
    expect(rejected.reason).toContain('Reopen it first')
  })

  it('says what is left to do in each case, so a disabled button is explained on screen', () => {
    const ready = deliverGuard(req({ state: 'in_progress' }), outcome({ delivered_eligible: true }))
    expect(deliverGuardSentence(ready, false, true)).toContain('Write the outcome brief')
    expect(deliverGuardSentence(ready, true, true)).toContain('Everything a delivery needs is in place')
    expect(deliverGuardSentence(ready, true, false)).toContain('Only the inbox team')

    // A brief typed into the box but not yet stored: the guard still says the
    // brief is missing, so the sentence has to say what the typing bought.
    const briefTyped = deliverGuard(req({ state: 'in_progress' }), outcome())
    expect(deliverGuardSentence(briefTyped, true, true)).toContain('still needs evidence')

    const needsEvidence = deliverGuard(req({ state: 'in_progress' }), outcome({ outcome_brief_set: true }))
    expect(deliverGuardSentence(needsEvidence, true, true)).toContain('Delivery needs evidence')
  })

  it('does not tell an in-progress request with everything in place to move to In progress', () => {
    // The one case where every input is present and the server still says no:
    // the act itself is what is missing, so the sentence says that rather than
    // pointing at a state the request is already in.
    const g = deliverGuard(req({ state: 'in_progress', outcome_brief: 'Includes: new sign-ins.' }), outcome({
      total: 2, done: 2, outcome_brief_set: true, delivered_eligible: false,
    }))
    expect(g.block).toBe('act')
    expect(g.reason).not.toMatch(/move the request there/i)
    expect(g.reason).toContain('has not delivered it yet')
  })

  it('reads the outcome brief only when the team wrote one', () => {
    expect(outcomeBrief(req())).toBeNull()
    expect(outcomeBrief(req({ outcome_brief: '   ' }))).toBeNull()
    expect(outcomeBrief(req({ outcome_brief: ' Includes: new sign-ins.' }))).toBe('Includes: new sign-ins.')
  })
})

describe('evidence rows carry each piece’s own state, never an assumed “done”', () => {
  const items = (...rows: RequestOutcomeItem[]) => outcome({ items: rows, total: rows.length })

  it('marks a completed ticket whose merge is not recorded as merge needed, not merged', () => {
    const rows = evidenceRows([link({ link_kind: 'delivered_by', target_kind: 'task', target_id: 21 })],
      items({ kind: 'task', id: 21, state: 'merge_gated', merge_state: 'needed' }))
    expect(rows[0].word).toBe('merge needed')
    expect(rows[0].tone).toBe('chg')
    expect(rows[0].note).toContain('merge is not recorded')
  })

  it('marks a deleted target as gone, with the tombstone date', () => {
    const rows = evidenceRows([link({ link_kind: 'delivered_by', target_kind: 'task', target_id: 21 })],
      items({ kind: 'task', id: 21, state: 'deleted', deleted_at: '2026-11-14T10:00:00Z' }))
    expect(rows[0].word).toBe('deleted')
    expect(rows[0].tone).toBe('gone')
    expect(rows[0].note).toContain('2026-11-14')
  })

  it('marks a held receipt as pending verify, with the receipt’s own status', () => {
    const rows = evidenceRows([link({ link_kind: 'delivered_by', target_kind: 'receipt', target_id: 3 })],
      items({ kind: 'receipt', id: 3, state: 'held', verification_status: 'self_checked' }))
    expect(rows[0].word).toBe('pending verify')
    expect(rows[0].note).toContain('self_checked')
  })

  it('calls a link with no aggregate item unverified rather than done', () => {
    const rows = evidenceRows([link({ link_kind: 'delivered_by', target_kind: 'task', target_id: 77 })], items())
    expect(rows[0].word).toBe('unverified')
    expect(rows[0].tone).toBe('chg')
    expect(rows[0].note).toContain('not part of the delivery aggregate')
  })

  it('reads an approved artifact as approved and a done ticket as merged', () => {
    const rows = evidenceRows(
      [link({ id: 1, link_kind: 'delivered_by', target_kind: 'artifact', target_id: 41 }),
        link({ id: 2, link_kind: 'delivered_by', target_kind: 'task', target_id: 21 })],
      items({ kind: 'artifact', id: 41, state: 'done' }, { kind: 'task', id: 21, state: 'done' }),
    )
    expect(rows.map(r => r.word)).toEqual(['approved', 'merged'])
    expect(rows.every(r => r.tone === 'ok')).toBe(true)
  })
})

describe('no automatic approvals or delivery: a done ticket flips nothing', () => {
  it('says every piece is done and the request is still not delivered (I8)', () => {
    const allDone = outcome({ phase: 'in_progress', total: 3, done: 3, items: [], delivered_eligible: false })
    const line = notDeliveredYet(allDone, 'in_progress')
    expect(line).toContain('still not delivered')
    expect(line).toContain('closing work never delivers a request')
  })

  it('says nothing when some work is still open, or when the request is already delivered', () => {
    expect(notDeliveredYet(outcome({ total: 3, done: 2 }), 'in_progress')).toBeNull()
    expect(notDeliveredYet(outcome({ phase: 'delivered', total: 3, done: 3 }), 'delivered')).toBeNull()
    expect(notDeliveredYet(outcome({ phase: 'closed', total: 3, done: 3 }), 'rejected')).toBeNull()
    expect(notDeliveredYet(outcome(), 'in_progress')).toBeNull()
  })

  it('never reports a request as deliverable from completed work alone', () => {
    // The one input that could say "delivered" is delivered_eligible. A fully
    // done set of work with the flag false is not eligible, whatever the counts say.
    const g = deliverGuard(req({ state: 'in_progress', outcome_brief: 'x' }), outcome({
      total: 5, done: 5, phase: 'in_progress', outcome_brief_set: true, delivered_eligible: false,
      blocked_reason: 'the team has not delivered it yet: everything a delivery needs is in place',
    }))
    expect(g.ready).toBe(false)
  })

  it('has no input at all for approval: an approval is a target’s fact, not something the rail computes', () => {
    // plannedApproval reads `approved` and nothing else — not the ticket's state,
    // not its completion, not its merge. A completed ticket whose target fact is
    // absent is counted as awaiting, which is the conservative direction.
    const counts = plannedApproval(groupLinks([
      link({ id: 1, target: { kind: 'task', id: 1, title: 'AMP-V2-1 — done but unapproved' } }),
    ]))
    expect(counts).toEqual({ total: 1, approved: 0, awaiting: 1, gone: 0 })
  })
})

describe('the rail’s words', () => {
  it('names each phase in plain language, with no ticket vocabulary', () => {
    const phases = ['unlinked', 'not_started', 'in_progress', 'blocked', 'delivered', 'closed'] as const
    const words = phases.map(phaseWord)
    expect(words).toEqual(['Nothing attached yet', 'Planned, not started', 'Work in progress', 'Needs a person', 'Delivered', 'Closed'])
    for (const w of words) expect(w).not.toMatch(/task|ticket|merge|branch/i)
  })

  it('summarises an outcome honestly, counting what is gated, held and deleted', () => {
    expect(outcomeSummary(outcome())).toBe('No delivered work is linked yet.')
    expect(outcomeSummary(outcome({ no_artifact: true }))).toContain('nothing to link')
    expect(outcomeSummary(outcome({ total: 4, done: 1, gated: 1, held: 1, deleted: 1, rework: 3 })))
      .toBe('1 of 4 done · 1 waiting on a merge · 1 pending verify · 1 deleted · 3 abandoned runs')
  })

  it('never renders a raw lifecycle value in an explanation', () => {
    const states: RequestStatus[] = ['new', 'triaging', 'needs_information', 'accepted', 'in_progress', 'delivered', 'requester_accepted', 'rejected', 'duplicate']
    // The words "rejected" and "duplicate" are the requester's own (their
    // projection carries `reject_reason`), so those two states' sentences may say
    // them; the snake_case and internal values must never appear.
    const internal = ['needs_information', 'requester_accepted', 'in_progress', 'triaging']
    for (const s of states) {
      const g = deliverGuard(req({ state: s }), outcome({ outcome_brief_set: true, total: 1 }))
      for (const word of internal) expect(g.reason).not.toContain(word)
    }
  })
})
