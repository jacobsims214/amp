/**
 * The team inbox queue's vocabulary: what each lifecycle state is called and
 * coloured, how a claim reads (holder + age), and how a lost claim race is
 * explained to the person who lost it.
 *
 * This is the one place those decisions live. `RequestRow` renders a row and
 * `TeamInbox` renders the queue; neither may re-decide a state's colour or a
 * claim's wording, because a queue whose pill and row disagree about what
 * "Triaging" looks like is the bug this file exists to prevent.
 *
 * The colours are the approved mockup's (artifact 37 v1, view 1) for the six
 * states it draws, unchanged:
 *
 *   new = ink · triaging = orange band · needs_info = red
 *   in progress = blue · delivered = green · duplicate = grey
 *
 * The three it does not draw are filled in on the same scale rather than left
 * uncoloured, so no state is a hole in the pill row: `accepted` (triaged and
 * queued for work) takes AMP's own scheduled violet, `requester_accepted` (the
 * mutually agreed end) takes teal, and `rejected` takes the darker danger ink so
 * it cannot be mistaken for `needs_information`'s red alarm — one needs someone
 * now, the other is over.
 */
import type { Member, RequestSourceKind, RequestStatus, TeamRequest } from '../types'
import { personName, type Person } from './people'
import { ago } from './approval'
import { requestConflict } from '../api/client'

/** What a state is called in the queue. Sentence case, no trailing punctuation. */
export const REQUEST_STATE_LABEL: Readonly<Record<RequestStatus, string>> = {
  new: 'New',
  triaging: 'Triaging',
  needs_information: 'Needs info',
  accepted: 'Accepted',
  in_progress: 'In progress',
  delivered: 'Delivered',
  requester_accepted: 'Accepted by requester',
  rejected: 'Rejected',
  duplicate: 'Duplicate',
}

/** The state's colour, as a CSS custom property reference (safe in an inline style). */
export const REQUEST_STATE_VAR: Readonly<Record<RequestStatus, string>> = {
  new: 'var(--st-ready)',
  triaging: 'var(--band-2)',
  needs_information: 'var(--st-flag)',
  accepted: 'var(--violet)',
  in_progress: 'var(--st-prog)',
  delivered: 'var(--success)',
  requester_accepted: 'var(--teal)',
  rejected: 'var(--danger-ink)',
  duplicate: 'var(--st-wait)',
}

/**
 * One sentence on what the state means for the desk, used as the tag's title.
 * These say what happens next, because "triaging" alone does not tell a person
 * whether anything is expected of them.
 */
export const REQUEST_STATE_HINT: Readonly<Record<RequestStatus, string>> = {
  new: 'Nobody has picked it up yet. Claim it to start triage.',
  triaging: 'Someone is looking at it now. The claim shows who, and for how long.',
  needs_information: 'The desk asked the requester a question; it stays here until they answer.',
  accepted: 'Triaged and accepted for work. It has not been planned into tickets yet.',
  in_progress: 'Plan items exist for it. Linked work does not move this state by itself.',
  delivered: 'The team says the outcome exists. Only the requester can accept it.',
  requester_accepted: 'The requester accepted the delivery. Closed.',
  rejected: 'The team declined it, with a reason the requester can read.',
  duplicate: 'Folded into a canonical request. Follow-up happens there.',
}

/** The state's colour as a CSS value. */
export function requestStateVar(state: RequestStatus): string {
  return REQUEST_STATE_VAR[state] ?? 'var(--st-wait)'
}

/** The state's word. */
export function requestStateLabel(state: RequestStatus): string {
  return REQUEST_STATE_LABEL[state] ?? state
}

/**
 * The display form of a request's reference: `REQ-<public_ref>`.
 *
 * The stored `public_ref` is an opaque token (D1) — 96 random bits in base64url,
 * never a per-team sequence — and `REQ-` is a rendering the server strips on the
 * way back in (`domain.NormalizeRequestPublicRef`). It is deliberately NOT the
 * serial: the serial is an internal row number and must never reach a requester.
 *
 * **The token's case is preserved exactly.** base64url is case-sensitive, and
 * the server resolves a reference with `WHERE public_ref = $1`, so uppercasing
 * it for display would render a reference nobody can paste back — a link that
 * looks right and answers not-found. Only the `REQ-` prefix is normalized (the
 * domain strips it case-insensitively).
 */
export function requestRef(publicRef: string): string {
  const ref = publicRef.trim()
  if (!ref) return 'REQ'
  return /^req-/i.test(ref) ? `REQ-${ref.slice(4)}` : `REQ-${ref}`
}

/** How a request arrived, in the queue's words. */
export const REQUEST_SOURCE_LABEL: Readonly<Record<RequestSourceKind, string>> = {
  web: 'web',
  mcp: 'mcp',
  api: 'api',
  connector: 'connector',
}

/** The intake surface as the meta line shows it. */
export function requestSourceLabel(kind: RequestSourceKind): string {
  return REQUEST_SOURCE_LABEL[kind] ?? kind
}

/**
 * The requester as the row shows them: the contact snapshot taken at submit
 * (D2), which survives the requester's user being deleted. Never the user id —
 * the snapshot is what the team actually answers to.
 */
export function requestRequester(req: TeamRequest): string {
  const contact = req.requester_contact?.trim()
  if (contact) return contact
  // A connector submission has no contact of its own; name the vendor object.
  if (req.source_external_id) return `${requestSourceLabel(req.source_kind)}:${req.source_external_id}`
  return 'unknown requester'
}

// ---- Claims: who is looking, and for how long ----

/** A claim's age in the queue's short form: "26h", "3d", "just now". Empty when unclaimed. */
export function claimAge(claimedAt: string | null | undefined, now = Date.now()): string {
  const age = ago(claimedAt, now)
  return age.replace(/ ago$/, '')
}

/** The meta line's claim phrase: "claimed 26h ago". Empty when nobody holds the claim. */
export function claimLine(req: TeamRequest, now = Date.now()): string {
  if (req.claim_user_id == null) return ''
  const age = claimAge(req.claimed_at, now)
  return age ? `claimed ${age} ago` : 'claimed'
}

/** Who holds a request's triage claim, from the row's `claim_user_id`. */
export type ClaimHolder =
  | { kind: 'none' }
  | { kind: 'me'; person: Person }
  | { kind: 'other'; person: Person }

/** Resolve a user id to a person, falling back to a stable label for a non-member. */
export function personOrId(people: Map<number, Person>, id: number | null | undefined): Person | null {
  if (id == null) return null
  return people.get(id) ?? { id, name: `User #${id}`, email: '' }
}

/** Build the id → person lookup the queue renders claim holders and assignees from. */
export function teamPeople(members: Member[]): Map<number, Person> {
  return new Map(members.map(m => [m.user_id, { id: m.user_id, name: personName(m), email: m.email }]))
}

/** Who is triaging this request right now. A claim is never on someone else's behalf. */
export function claimHolder(req: TeamRequest, people: Map<number, Person>, meId?: number): ClaimHolder {
  const person = personOrId(people, req.claim_user_id)
  if (!person) return { kind: 'none' }
  return meId != null && req.claim_user_id === meId ? { kind: 'me', person } : { kind: 'other', person }
}

/**
 * Whether the row offers Claim as its primary action.
 *
 * A claim says "I am looking at it right now", so the only thing that makes a
 * request unclaimable is somebody already holding the claim — the server allows
 * a claim in any lifecycle state, because a claim records who is looking, not
 * where the request is. A claim held by someone else is a 409, never a steal,
 * and never expires (D4), so the UI offers no take-over.
 *
 * `meId` is deliberately not a parameter: whether the caller holds it decides
 * which *other* action a row shows (Release), not whether Claim appears. One
 * rule, so a row cannot offer Claim on a claim it already holds.
 */
export function isClaimable(req: TeamRequest): boolean {
  return req.claim_user_id == null
}

/** Whether the caller may hand back the claim they hold. */
export function canRelease(req: TeamRequest, meId?: number): boolean {
  return meId != null && req.claim_user_id === meId
}

/**
 * The claim in a few words, for an accessible name or a rail: "unclaimed",
 * "claimed by you", "claimed by Priya Nair". Never "someone" when the id is
 * known — a queue exists partly so a person can see who is on what.
 */
export function claimLabel(req: TeamRequest, people: Map<number, Person>, meId?: number): string {
  const holder = claimHolder(req, people, meId)
  switch (holder.kind) {
    case 'none':
      return 'unclaimed'
    case 'me':
      return 'claimed by you'
    case 'other':
      return `claimed by ${holder.person.name}`
  }
}

/**
 * The claim's meaning, stated once, next to wherever a claim is shown. The
 * mockup's footer copy: claims are manual, visible, and never expire.
 */
export const CLAIM_NEVER_EXPIRES = 'claims are manual and never expire — the age shows next to the claim, so a stale claim is visible instead of silently dropped'

/**
 * A lost claim race, explained to the person who lost it.
 *
 * The 409 carries `claimUserId` — who won — and, on a stale revision, `rev`.
 * The banner names the winner (resolved through the team's member list, falling
 * back to the id) and says plainly that nothing was sent, which is the fact a
 * person needs before retrying. It never claims to know *when* the other person
 * claimed: the response does not carry that, and inventing a duration would be
 * the banner telling a story the server did not.
 */
export interface ClaimConflictNotice {
  /** The winner's display name, when the 409 named them. */
  holder: string
  /** The revision the row is actually at, when the server sent it. */
  rev?: number
  title: string
  body: string
}

/** Read a failed claim as the banner it should become; null for any other failure. */
export function claimConflictNotice(
  err: unknown,
  people: Map<number, Person>,
  shownRev?: number,
): ClaimConflictNotice | null {
  const conflict = requestConflict(err)
  if (!conflict) return null
  const winner = personOrId(people, conflict.claimUserId)
  const holder = winner?.name ?? 'Someone else'
  const revision =
    conflict.rev != null && shownRev != null && conflict.rev !== shownRev
      ? ` Your view was from before that (rev ${shownRev} → ${conflict.rev}).`
      : conflict.rev != null
        ? ` The request is now at rev ${conflict.rev}.`
        : ''
  return {
    holder,
    rev: conflict.rev,
    title: conflict.claimUserId != null
      ? `${holder} claimed this request first.`
      : 'This request changed before your claim landed.',
    body: `${revision} Claims are first-write-wins — refresh to see their claim, or ask them to hand it over. Nothing was sent.`,
  }
}

// ---- The owner select ----

/**
 * Whose rows the queue shows. The mockup's select, and the anti-fan-out rule
 * made visible: a request is yours only because you claimed it or own it.
 *
 * It is a **view of the loaded page**, not a server filter: the queue's API has
 * no claim/assignee term (only `submitter`, `source_kind`, dates, `attached`,
 * `q` and the `mine` id list), so the page says so rather than letting the rows
 * and the pill counts silently describe two different queues.
 */
export type RequestOwnerFilter = 'everyone' | 'unclaimed' | 'claimed' | 'assigned'

/** The select's options, in the mockup's order. */
export const REQUEST_OWNER_OPTIONS: readonly { value: RequestOwnerFilter; label: string }[] = [
  { value: 'everyone', label: 'Everyone' },
  { value: 'unclaimed', label: 'Unclaimed' },
  { value: 'claimed', label: 'Claimed by me' },
  { value: 'assigned', label: 'Assigned to me' },
]

/** Whether one row belongs in the selected owner view. */
export function matchesOwnerFilter(req: TeamRequest, filter: RequestOwnerFilter, meId?: number): boolean {
  switch (filter) {
    case 'unclaimed':
      return req.claim_user_id == null && req.owner_user_id == null
    case 'claimed':
      return meId != null && req.claim_user_id === meId
    case 'assigned':
      return meId != null && req.owner_user_id === meId
    case 'everyone':
      return true
  }
}

/**
 * The queue's pills: All plus every lifecycle state, in lifecycle order.
 *
 * The approved mockup draws six of the nine; the row here is complete so no
 * state is reachable only through "All". The counts come from the server's
 * `totals.states`, which always carries all nine keys — never from the page.
 */
export const REQUEST_QUEUE_PILLS: readonly { state: RequestStatus | null; label: string }[] = [
  { state: null, label: 'All' },
  { state: 'new', label: REQUEST_STATE_LABEL.new },
  { state: 'triaging', label: REQUEST_STATE_LABEL.triaging },
  { state: 'needs_information', label: REQUEST_STATE_LABEL.needs_information },
  { state: 'accepted', label: REQUEST_STATE_LABEL.accepted },
  { state: 'in_progress', label: REQUEST_STATE_LABEL.in_progress },
  { state: 'delivered', label: REQUEST_STATE_LABEL.delivered },
  { state: 'requester_accepted', label: REQUEST_STATE_LABEL.requester_accepted },
  { state: 'rejected', label: REQUEST_STATE_LABEL.rejected },
  { state: 'duplicate', label: REQUEST_STATE_LABEL.duplicate },
]
