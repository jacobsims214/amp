// The queue's vocabulary (AMP #3978, build slice J1).
//
// Three things are proved here, all of them pure decisions a component would
// otherwise have to re-derive:
//
//  1. **Every lifecycle state has a word and a colour.** `totals.states` always
//     carries all nine keys, so a pill row renders nine; a state missing from
//     these tables would render as an unlabelled, colourless button — a hole in
//     the queue rather than a state nobody used.
//  2. **A claim's age reads, and never says "ago" twice.** The age is the
//     anti-expiry signal (D4), so its exact wording is load-bearing: "claimed
//     26h ago" is the mockup's sentence.
//  3. **A lost claim race names the winner and says nothing was sent.** The 409
//     carries `claimUserId` and sometimes `rev`; the banner is built from those
//     facts and must not invent a duration the response does not carry.
import { describe, expect, it } from 'vitest'
import { REQUEST_STATUSES, type RequestStatus } from '../types'
import type { HttpError } from '../api/client'
import { request } from '../test/requestFixture'
import {
  canRelease, claimAge, claimConflictNotice, claimHolder, claimLabel, claimLine,
  isClaimable, matchesOwnerFilter, REQUEST_STATE_HINT, REQUEST_STATE_LABEL, REQUEST_STATE_VAR,
  requestRef, requestRequester, requestSourceLabel, requestStateLabel, requestStateVar,
  teamPeople,
} from './requestState'
import type { Person } from './people'

const people = new Map<number, Person>([
  [5, { id: 5, name: 'Priya Nair', email: 'priya@simstech.cloud' }],
  [9, { id: 9, name: 'Marco Testa', email: 'marco@simstech.cloud' }],
])

/** The now used by every age assertion, so the test does not depend on the wall clock. */
const NOW = Date.parse('2026-11-14T10:00:00Z')

describe('every lifecycle state is renderable', () => {
  it('names and colours all nine states, with no repeats', () => {
    for (const state of REQUEST_STATUSES) {
      expect(REQUEST_STATE_LABEL[state], `label for ${state}`).toBeTruthy()
      expect(REQUEST_STATE_VAR[state], `colour for ${state}`).toBeTruthy()
      expect(REQUEST_STATE_HINT[state], `hint for ${state}`).toBeTruthy()
    }
    // A repeated colour would make two states indistinguishable in a pill row.
    const colours = REQUEST_STATUSES.map(s => REQUEST_STATE_VAR[s])
    expect(new Set(colours).size).toBe(colours.length)
  })

  it('uses the mockup\'s six colours unchanged', () => {
    // Approved artifact 37 v1: new=ink, triaging=orange band, needs-info=red,
    // in-progress=blue, delivered=green, duplicate=grey.
    expect(REQUEST_STATE_VAR.new).toBe('var(--st-ready)')
    expect(REQUEST_STATE_VAR.triaging).toBe('var(--band-2)')
    expect(REQUEST_STATE_VAR.needs_information).toBe('var(--st-flag)')
    expect(REQUEST_STATE_VAR.in_progress).toBe('var(--st-prog)')
    expect(REQUEST_STATE_VAR.delivered).toBe('var(--success)')
    expect(REQUEST_STATE_VAR.duplicate).toBe('var(--st-wait)')
  })

  it('falls back rather than rendering undefined for a state the domain does not name', () => {
    const unknown = 'shipped' as RequestStatus
    expect(requestStateLabel(unknown)).toBe('shipped')
    expect(requestStateVar(unknown)).toBe('var(--st-wait)')
  })

  it('says the two accepted states apart', () => {
    // "Accepted" (triaged, waiting to be planned) and "accepted by requester"
    // (the end) are different facts; one word for both would lose the lifecycle.
    expect(requestStateLabel('accepted')).not.toBe(requestStateLabel('requester_accepted'))
  })
})

describe('the reference and the intake line', () => {
  it('renders a public ref as REQ-, never as the serial, and preserves the token\'s case', () => {
    // D1: the stored token is the only public identity. The serial is internal.
    // Case matters: base64url is case-sensitive and the server resolves a
    // reference with `WHERE public_ref = $1`, so a case-folded display would be
    // a reference nobody can paste back.
    expect(requestRef('8f2q1x7k4m2p9t5w')).toBe('REQ-8f2q1x7k4m2p9t5w')
    expect(requestRef('Ab-12_cd9efgh76_')).toBe('REQ-Ab-12_cd9efgh76_')
    expect(requestRef('REQ-8F2Q1X7K4M2P9T5W')).toBe('REQ-8F2Q1X7K4M2P9T5W')
    expect(requestRef('req-abc123')).toBe('REQ-abc123')
    expect(requestRef('  ')).toBe('REQ')
  })

  it('shows the contact snapshot, and a connector\'s external id when there is no contact', () => {
    expect(requestRequester(request())).toBe('it-admin@acme.example')
    expect(requestRequester(request({ requester_contact: '', source_kind: 'connector', source_external_id: 'acme-sso-14' })))
      .toBe('connector:acme-sso-14')
    expect(requestRequester(request({ requester_contact: '', source_kind: 'api' }))).toBe('unknown requester')
  })

  it('names all four intake surfaces', () => {
    expect(requestSourceLabel('web')).toBe('web')
    expect(requestSourceLabel('mcp')).toBe('mcp')
    expect(requestSourceLabel('api')).toBe('api')
    expect(requestSourceLabel('connector')).toBe('connector')
  })
})

describe('a claim and its age', () => {
  it('reads the age without a second "ago"', () => {
    expect(claimAge('2026-11-13T08:00:00Z', NOW)).toBe('26h')
    expect(claimAge('2026-11-11T10:00:00Z', NOW)).toBe('3d')
    expect(claimAge('2026-11-14T09:59:40Z', NOW)).toBe('just now')
    expect(claimAge(null, NOW)).toBe('')
  })

  it('says "claimed 26h ago" and nothing when unclaimed', () => {
    expect(claimLine(request({ claim_user_id: 5, claimed_at: '2026-11-13T08:00:00Z' }), NOW)).toBe('claimed 26h ago')
    expect(claimLine(request(), NOW)).toBe('')
  })

  it('resolves the holder as you or as a named teammate, never as a bare id', () => {
    const held = request({ claim_user_id: 5, claimed_at: '2026-11-13T08:00:00Z' })
    expect(claimLabel(held, people, 5)).toBe('claimed by you')
    expect(claimLabel(held, people, 9)).toBe('claimed by Priya Nair')
    expect(claimLabel(held, people)).toBe('claimed by Priya Nair')
    expect(claimLabel(request(), people, 5)).toBe('unclaimed')
    expect(claimHolder(held, people, 5).kind).toBe('me')
    expect(claimHolder(held, people, 9).kind).toBe('other')
    expect(claimHolder(request(), people, 5).kind).toBe('none')
  })

  it('falls back to a stable label for a claim held by someone off the member list', () => {
    const held = request({ claim_user_id: 404 })
    expect(claimLabel(held, people, 5)).toBe('claimed by User #404')
  })

  it('offers Claim only on an unclaimed row, and Release only to the holder', () => {
    expect(isClaimable(request())).toBe(true)
    const held = request({ claim_user_id: 5, claimed_at: '2026-11-13T08:00:00Z' })
    expect(isClaimable(held)).toBe(false)
    expect(canRelease(held, 5)).toBe(true)
    expect(canRelease(held, 9)).toBe(false)
    expect(canRelease(request(), 5)).toBe(false)
    // No meId means nobody is identified: a claim is never offered for release.
    expect(canRelease(held)).toBe(false)
  })

  it('builds the person lookup from a team\'s members', () => {
    const map = teamPeople([
      { user_id: 5, email: 'priya@simstech.cloud', display_name: 'Priya Nair', role: 'lead' },
      { user_id: 9, email: 'marco@simstech.cloud', display_name: '', role: 'member' },
    ])
    expect(map.get(5)?.name).toBe('Priya Nair')
    // An empty display name falls back to the email's local part, the house rule.
    expect(map.get(9)?.name).toBe('marco')
  })
})

describe('a lost claim race', () => {
  const notFound = new Error('nope') as HttpError

  it('names the winner and says nothing was sent', () => {
    const err = Object.assign(new Error('Conflict'), { status: 409, code: 'request.conflict', claimUserId: 5, rev: 4 }) as HttpError
    const notice = claimConflictNotice(err, people, 3)
    expect(notice).not.toBeNull()
    expect(notice!.title).toBe('Priya Nair claimed this request first.')
    expect(notice!.body).toContain('rev 3 → 4')
    expect(notice!.body).toContain('first-write-wins')
    expect(notice!.body).toContain('Nothing was sent.')
    // It must not invent when the other person claimed: the 409 does not carry it.
    expect(notice!.body).not.toMatch(/seconds ago|minutes ago|just now/)
  })

  it('says "Someone else" when the 409 names no holder', () => {
    const err = Object.assign(new Error('Conflict'), { status: 409, code: 'request.conflict', rev: 8 }) as HttpError
    const notice = claimConflictNotice(err, people, 7)
    expect(notice!.title).toBe('This request changed before your claim landed.')
    expect(notice!.body).toContain('rev 7 → 8')
  })

  it('is null for a failure that is not a 409 — those are the queue\'s error line, not a banner', () => {
    expect(claimConflictNotice(notFound, people, 3)).toBeNull()
    expect(claimConflictNotice(new Error('offline'), people, 3)).toBeNull()
  })
})

describe('the owner view', () => {
  const mine = request({ id: 1, claim_user_id: 5, claimed_at: '2026-11-14T09:00:00Z' })
  const assigned = request({ id: 2, owner_user_id: 5 })
  const theirs = request({ id: 3, claim_user_id: 9 })
  const open = request({ id: 4 })

  it('splits claimed from assigned — they are two different fields', () => {
    expect(matchesOwnerFilter(mine, 'claimed', 5)).toBe(true)
    expect(matchesOwnerFilter(mine, 'assigned', 5)).toBe(false)
    expect(matchesOwnerFilter(assigned, 'assigned', 5)).toBe(true)
    expect(matchesOwnerFilter(assigned, 'claimed', 5)).toBe(false)
  })

  it('counts a row with no claim and no assignee as unclaimed', () => {
    expect(matchesOwnerFilter(open, 'unclaimed', 5)).toBe(true)
    expect(matchesOwnerFilter(mine, 'unclaimed', 5)).toBe(false)
    expect(matchesOwnerFilter(theirs, 'unclaimed', 5)).toBe(false)
    expect(matchesOwnerFilter(assigned, 'unclaimed', 5)).toBe(false)
  })

  it('never shows another person\'s claim as yours, and shows everything under Everyone', () => {
    expect(matchesOwnerFilter(theirs, 'claimed', 5)).toBe(false)
    expect(matchesOwnerFilter(theirs, 'assigned', 5)).toBe(false)
    for (const r of [mine, assigned, theirs, open]) expect(matchesOwnerFilter(r, 'everyone', 5)).toBe(true)
  })
})