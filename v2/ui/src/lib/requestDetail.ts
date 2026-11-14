/**
 * The team request detail page's decisions (AMP #3984, build slice J2's page
 * half; approved mockup artifact 37 v1, "Request detail").
 *
 * The page itself composes the reviewed panels and rails (#3979, #3980) and
 * owns the one thing none of them can: reading, re-reading and deciding what
 * the caller is allowed to do at all. Everything a reader could argue with is
 * here as a pure function, so it is a table test and not a component test:
 *
 *  - **which read answered** — the detail route answers two shapes, and one of
 *    them is a whitelist projection with no record in it;
 *  - **who the caller is to this inbox** (member, manager, neither) and
 *    therefore which acts the page offers;
 *  - **which lifecycle acts are the page's own** rather than the rails', which
 *    is exactly one: reopen;
 *  - **what the six detail states are**, so "every approved state is reachable"
 *    is a fact this file states and a test can enumerate rather than a claim
 *    about a screenshot.
 *
 * Nothing here decides authorization on the server's behalf. The functions in
 * this file answer "may this button be offered, and why not"; the server answers
 * the same question again on every write, and every refusal the page renders is
 * the server's own sentence.
 */
import type {
  Member, RequestLink, RequestOutcome, RequestReadAnswer, RequestStatus, TeamRequest,
} from '../types'
import { REQUEST_STATUSES } from '../types'
import type { ResolvedRequestLink } from './requestControls'

// ---- Which read answered, and what it is allowed to be ----

/**
 * The detail page's read, resolved to one arm.
 *
 * This exists because the route answers two different shapes and the
 * difference is a security boundary, not a display preference: a requester's
 * answer carries `projection` and **no record at all** — no org, no claim, no
 * assignee, no revision, no metadata. A page that coerced the projection into a
 * `TeamRequest` (or fell back to an empty one) would render an internal surface
 * from a whitelist, which is precisely the leak the two DTOs exist to prevent.
 */
export type DetailRead =
  | { kind: 'team'; request: TeamRequest; outcome: RequestOutcome }
  | { kind: 'requester'; publicRef: string; state: RequestStatus }
  | { kind: 'other'; view: string }

/**
 * Resolve the detail answer to the arm the page may render.
 *
 * `view` is the server's own word for what it decided, so this branches on it
 * rather than on which field happens to be present: an answer whose `view` says
 * `requester` is the projection **even if** a record field came along, and an
 * answer whose view says anything this build does not know is not rendered as a
 * team record (fail closed — a future third view must not inherit a triage
 * screen).
 */
export function resolveDetailRead(answer: RequestReadAnswer | null | undefined): DetailRead | null {
  if (!answer || typeof answer !== 'object') return null
  const view = (answer as { view?: unknown }).view
  if (view === 'team') {
    const team = answer as Extract<RequestReadAnswer, { view: 'team' }>
    if (!team.request || !team.outcome) return null
    return { kind: 'team', request: team.request, outcome: team.outcome }
  }
  if (view === 'requester') {
    const requesters = answer as Extract<RequestReadAnswer, { view: 'requester' }>
    const p = requesters.projection
    if (!p) return null
    return { kind: 'requester', publicRef: p.public_ref, state: p.state }
  }
  return { kind: 'other', view: typeof view === 'string' ? view : '' }
}

/** Why a requester's answer is not the triage page. Stated on screen, never silent. */
export const REQUESTER_VIEW_NOTICE =
  'This is your own request, seen as its requester. The team’s triage view — claim, assignee, plan and '
  + 'delivery — belongs to the inbox and is not shown here.'

// ---- Who the caller is to this inbox ----

/**
 * The caller's standing on one inbox, as the page can know it: the team's
 * member rows plus the org roles the identity carries.
 *
 * `manager` is the server's `RequireTeamManager` (a team lead, or an owner/admin
 * of the team's org) and `member` is `InboxRole`'s member row. Both are read from
 * data the page already has; a caller who is neither is a stranger to the inbox
 * and gets no act at all — the server would 404 the read before it got here, so
 * this arm exists for the local-dev identity and for a stale membership.
 */
export interface InboxStanding {
  member: boolean
  manager: boolean
}

/**
 * Resolve the caller's standing on the inbox.
 *
 * `orgRole` is the caller's role in the team's org when the identity carries one
 * (`Me.orgs`), because the server's manager check passes for an org owner/admin
 * with no team row at all — inferring "manager" from `members` alone would hide
 * Reject from exactly the person the server would allow it for. There is
 * deliberately no `teamId` parameter: the members list *is* the scoped answer for
 * one team, so a second way to name the team could only disagree with it.
 */
export function inboxStanding(
  meId: number | undefined,
  members: readonly Member[],
  opts: { orgRole?: string; admin?: boolean } = {},
): InboxStanding {
  const mine = meId != null ? members.find(m => m.user_id === meId) : undefined
  const orgManager = opts.admin === true || opts.orgRole === 'owner' || opts.orgRole === 'admin'
  const member = !!mine || orgManager
  return { member, manager: orgManager || mine?.role === 'lead' }
}

/** The one sentence that explains a missing act without pretending it is available. */
export function notAMemberReason(): string {
  return 'Only the inbox team can triage a request: you can read it as its requester, and the team’s claim, '
    + 'assign and delivery acts belong to its members.'
}

// ---- Reopen: the one lifecycle act the page owns ----

/** The states the team may reopen from, with the destination each edge has. */
const TEAM_REOPEN_EDGES: readonly { from: RequestStatus; to: RequestStatus }[] = [
  // `request_transitions_test.go`'s expectedLifecycle: the team's only reopen
  // edges are rejected → triaging and duplicate → triaging. Delivery's reopen is
  // the requester's (I1), so it is deliberately absent here.
  { from: 'rejected', to: 'triaging' },
  { from: 'duplicate', to: 'triaging' },
]

export interface ReopenOffer {
  offered: boolean
  /** The destination, when offered. */
  to?: RequestStatus
  /** Why not, when not offered — the sentence the page shows instead of the button. */
  reason: string
}

/**
 * Whether the page offers Reopen, and where the reopen goes.
 *
 * The rails deliberately do not own this: `RequestTriageRail` takes claim,
 * release, assign and the three closing acts, and `RequestPlanningRail` says
 * "reopen it before planning work". Reopen is the page's because it is the one
 * act that changes what the *page* is showing — the whole detail moves from a
 * closed reading back to an open one — and because its destination is fixed by
 * the lifecycle rather than chosen by the caller.
 */
export function reopenOffer(req: Pick<TeamRequest, 'state'>, standing: InboxStanding): ReopenOffer {
  const edge = TEAM_REOPEN_EDGES.find(e => e.from === req.state)
  if (!edge) {
    return {
      offered: false,
      reason: req.state === 'delivered' || req.state === 'requester_accepted'
        ? 'Reopening a delivered request is the requester’s act alone (I1): the team cannot take it back.'
        : 'This request is open: there is nothing to reopen.',
    }
  }
  if (!standing.member) return { offered: false, reason: notAMemberReason() }
  return { offered: true, to: edge.to, reason: '' }
}

/** What the reopen button says, and what it will do — the same words in the button and its hint. */
export function reopenCopy(offer: ReopenOffer): { label: string; hint: string } {
  return {
    label: offer.to === 'triaging' ? 'Reopen' : 'Reopen',
    hint: offer.to === 'triaging'
      ? 'Reopen moves it back to Triaging and records who reopened it. Asking the requester something reopens it too, '
        + 'straight into Needs information.'
      : '',
  }
}

// ---- The six detail states, named ----

/**
 * The detail states approved mockup 37 v1 draws: filled, needsinfo, conflict,
 * rejected, duplicate, delivered. Two more are real and the mockup does not
 * draw them — `requester_accepted` (a delivered request the requester accepted)
 * and `accepted`/`in_progress` (the working middle) — so the vocabulary is the
 * nine lifecycle states plus the two things that are not states at all: a lost
 * claim race and a stale view.
 */
export type DetailState =
  | { kind: 'state'; state: RequestStatus }
  /** A 409 that named a claim holder: somebody else has the claim now. */
  | { kind: 'conflict' }
  /** A 409 that carried only a revision: this view is stale, nobody is named. */
  | { kind: 'stale' }

/**
 * Which of the approved detail states a request is in, plus whether a live
 * failure overlays it.
 *
 * A conflict is layered over the request's own state rather than replacing it:
 * the mockup shows the request's whole body under the banner, so a page that
 * rendered only a banner would answer "what is this request" with silence.
 */
export function detailState(
  state: RequestStatus,
  failure: { kind: 'claim' | 'stale' } | null,
): DetailState {
  if (failure?.kind === 'claim') return { kind: 'conflict' }
  if (failure?.kind === 'stale') return { kind: 'stale' }
  return { kind: 'state', state }
}

/**
 * The mockup state name a detail state corresponds to, so a test (or a
 * screenshot pass) can assert "the approved states are reachable" against the
 * mockup's own vocabulary instead of inventing a second one.
 *
 * `accepted` and `in_progress` share the mockup's `filled` reading (the working
 * middle, with Triage/Planning/Deliver live); `requester_accepted` reads as
 * `delivered` with acceptance recorded.
 */
export function mockupStateName(s: DetailState): string {
  switch (s.kind) {
    case 'conflict': return 'conflict'
    case 'stale': return 'stale'
  }
  switch (s.state) {
    case 'new':
    case 'triaging':
    case 'accepted':
    case 'in_progress': return 'filled'
    case 'needs_information': return 'needsinfo'
    case 'rejected': return 'rejected'
    case 'duplicate': return 'duplicate'
    case 'delivered':
    case 'requester_accepted': return 'delivered'
    default: return 'filled'
  }
}

/** Every approved mockup detail state, in the order the mockup's switcher lists them. */
export const MOCKUP_DETAIL_STATES: readonly string[] = [
  'filled', 'needsinfo', 'conflict', 'rejected', 'duplicate', 'delivered',
]

/**
 * One representative request state per mockup state, so the page's tests can
 * prove each approved state is reachable by rendering it — the ticket's first
 * criterion, as a table rather than as six assertions that could drift.
 */
export const DETAIL_STATE_REACHABLE: readonly { mockup: string; state: RequestStatus }[] = [
  { mockup: 'filled', state: 'triaging' },
  { mockup: 'needsinfo', state: 'needs_information' },
  { mockup: 'rejected', state: 'rejected' },
  { mockup: 'duplicate', state: 'duplicate' },
  { mockup: 'delivered', state: 'delivered' },
]

// ---- Which panels a reading shows ----

/**
 * Which halves of the detail body a state shows.
 *
 * The mockup is explicit about this and it is not decoration: a Clarification
 * card on a request nobody asked about, or a Rejected card on one that is open,
 * would state a fact the record does not. Each card is therefore tied to the
 * state that has the fact.
 */
export interface DetailPanels {
  /** The immutable request block: always, in every state. */
  request: true
  /** The internal discussion thread. */
  discussion: boolean
  /** The clarification card: only while a question is open. */
  clarification: boolean
  /** The rejection, with its reason and actor. */
  rejection: boolean
  /** The duplicate pointer. */
  duplicate: boolean
  /** The delivery: brief and evidence. */
  delivery: boolean
  /** The triage rail's closing acts (ask / reject / duplicate). */
  triageActs: boolean
  /** The planning rail. */
  planning: boolean
  /** The deliver panel. */
  deliverPanel: boolean
  /** The append-only history. */
  history: boolean
}

export function detailPanels(state: RequestStatus): DetailPanels {
  const delivered = state === 'delivered' || state === 'requester_accepted'
  const closed = state === 'rejected' || state === 'duplicate'
  return {
    request: true,
    // The thread is the record of the exchange, so it stays readable in every
    // state including the closed ones: a rejection's discussion is why it was
    // rejected.
    discussion: true,
    clarification: state === 'needs_information',
    rejection: state === 'rejected',
    duplicate: state === 'duplicate',
    delivery: delivered,
    // A delivered request's triage acts are gone (the requester decides now),
    // and a closed one's are gone too: reopen is the page's, and asking from a
    // close is the rail's own reopen edge.
    triageActs: !delivered,
    planning: !closed,
    deliverPanel: !closed,
    history: true,
  }
}

// ---- The links the shipped read does not carry ----

/**
 * What the detail read does **not** answer, and therefore what a page cannot
 * render from the shipped API.
 *
 * `GET /api/teams/{id}/requests/{request}` answers the record and the delivery
 * aggregate. It does not answer the request's discussion rows, its
 * `request_events` history, or its `request_link` rows — and nothing else does
 * either: the only request reads registered in `internal/api/rest.go` are the
 * queue, the summary, the detail and the attachments panel. This is the same
 * gap #3980 recorded for the rails, now seen from the page.
 *
 * So the page takes these as an optional seam (`reads`). With the shipped server
 * the seam is empty and every affected panel says so in one sentence. What the
 * page must never do is the other two options: invent an empty thread (which
 * reads as "nobody has said anything" — a statement about the record), or let a
 * panel fetch (opening a request would then perform a write-capable mount).
 */
export interface DetailReads {
  comments?: import('../types').RequestComment[]
  events?: import('../types').RequestEvent[]
  links?: ResolvedRequestLink[]
}

/** Which of the three unserved reads are present. */
export function servedReads(reads: DetailReads): { comments: boolean; events: boolean; links: boolean } {
  return {
    comments: Array.isArray(reads.comments),
    events: Array.isArray(reads.events),
    links: Array.isArray(reads.links),
  }
}

/** The one sentence a panel says instead of a fake empty list. */
export function unservedReadSentence(what: 'discussion' | 'history' | 'links'): string {
  const noun = what === 'discussion' ? 'discussion' : what === 'history' ? 'history' : 'linked work'
  return `The request read that reaches this page answers the record and the delivery summary, not its ${noun}. `
    + 'Nothing is missing from the request: this build has no route that returns the rows, so they are not shown '
    + 'rather than shown as empty.'
}

/**
 * The route row, built from the one link-ish fact the shipped read *does*
 * carry: the aggregate's `routed_project_id`.
 *
 * Without this the planning rail would say "Not routed yet" for a request that
 * has a route — the aggregate reports `routed_project_id` and counts the route
 * in `linked`, so a page ignoring it would contradict the server's own answer.
 * The row is a `routes_to` link with its id; the title is resolved separately
 * (see `resolveRoutedProject`) because it costs a read of its own.
 *
 * A `delivered_by`/`planned_as` row can **not** be reconstructed this way: the
 * aggregate carries counts and per-item kinds/ids only for evidence, and no
 * title, so a synthesised `planned_as` row would be a guess. Only the route is
 * synthesised, because it is the one relation with exactly one target.
 */
export function routedProjectLink(req: Pick<TeamRequest, 'id'>, outcome: RequestOutcome): ResolvedRequestLink | null {
  const projectId = outcome.routed_project_id
  if (!projectId) return null
  const link: ResolvedRequestLink = {
    id: 0,
    request_id: req.id,
    link_kind: 'routes_to',
    target_kind: 'project',
    target_id: projectId,
    created_by: null,
    created_at: '',
    target: { kind: 'project', id: projectId, title: `Project #${projectId}`, projectId },
  }
  return link
}

/**
 * The planning rail's link list: whatever the seam supplied, plus the route the
 * aggregate reported when the seam has none.
 *
 * The order matters only in that a second `routes_to` would be a lie:
 * `groupLinks` takes the first, and the aggregate's route is the active one, so
 * it goes first when the seam did not name a route itself.
 */
export function planningLinks(reads: DetailReads, req: Pick<TeamRequest, 'id'>, outcome: RequestOutcome): ResolvedRequestLink[] {
  const supplied = reads.links ?? []
  const hasRoute = supplied.some(l => l.link_kind === 'routes_to')
  const route = hasRoute ? null : routedProjectLink(req, outcome)
  return route ? [route, ...supplied] : supplied
}

/** The evidence rows the deliver panel shows: the `delivered_by` links, when the seam served any. */
export function evidenceLinks(reads: DetailReads): ResolvedRequestLink[] {
  return (reads.links ?? []).filter(l => l.link_kind === 'delivered_by')
}

/**
 * Fill a route's resolved title in once its project read answers.
 *
 * Kept separate from `routedProjectLink` so the page can render the rail
 * immediately (with the route present and its id) instead of holding the whole
 * detail behind a second read. A project read that fails leaves the id-bearing
 * title in place: the route is a fact either way, and only the name is missing.
 */
export function withRouteTitle(links: readonly ResolvedRequestLink[], title: string | null): ResolvedRequestLink[] {
  if (!title) return [...links]
  return links.map(l =>
    l.link_kind === 'routes_to' && l.target ? { ...l, target: { ...l.target, title } } : l,
  )
}

/**
 * Whether **every** link row the mockup needs came from the seam.
 *
 * The planning rail can be honest from a partial list only if it says which half
 * is missing; the page therefore tells the rail when the seam served no links at
 * all (the shipped case), and the rail's own "not routed" copy is then never
 * reached with a route present. This is the flag for that.
 */
export function linksServed(reads: DetailReads): boolean {
  return Array.isArray(reads.links)
}

// ---- The request block's own facts ----

/**
 * How the request block describes its own immutability.
 *
 * The mockup's header says "submitted … · source web · immutable", and
 * "immutable" is true of exactly one thing: once the desk has picked the
 * request up, its subject and brief stop being editable (`UpdateRequest` refuses
 * after triage begins). So the word is tied to that fact rather than printed on
 * every state — a `new`, unclaimed request is still the submitter's to edit.
 */
export function briefMutability(req: Pick<TeamRequest, 'state' | 'claim_user_id' | 'owner_user_id'>): {
  immutable: boolean
  note: string
} {
  const untriaged = req.state === 'new' && req.claim_user_id == null && req.owner_user_id == null
  return untriaged
    ? { immutable: false, note: 'Untriaged: the person who submitted it can still edit the subject and the brief.' }
    : { immutable: true, note: 'Immutable once triage starts: changes are said in the thread instead.' }
}

/**
 * The rejection's actor and reason, from the fields the record carries.
 *
 * There is deliberately no actor name here: `TeamRequest` stores the reason, not
 * who wrote it — the actor lives in the `request_events` row, which this build's
 * read does not serve. So the card states the reason verbatim (which is what the
 * requester is shown) and says where the actor is, instead of guessing a name.
 */
export function rejectionFacts(req: Pick<TeamRequest, 'reject_reason' | 'closed_at'>): {
  reason: string
  when: string | null
  actorKnown: false
} {
  return {
    reason: req.reject_reason.trim(),
    when: req.closed_at && !req.closed_at.startsWith('0001-01-01') ? req.closed_at : null,
    actorKnown: false,
  }
}

/**
 * The duplicate pointer, from `duplicate_of`.
 *
 * The canonical is a request serial, and the page cannot name its reference
 * without a read it does not have (`duplicate_of` is an id, and the reference is
 * per row). So the card shows the serial it can prove and does not invent a
 * `REQ-` token — a fabricated reference would be a link that answers not-found.
 */
export function duplicateFacts(req: Pick<TeamRequest, 'duplicate_of' | 'closed_at'>): {
  canonicalId: number | null
  when: string | null
} {
  return {
    canonicalId: req.duplicate_of != null && req.duplicate_of > 0 ? req.duplicate_of : null,
    when: req.closed_at && !req.closed_at.startsWith('0001-01-01') ? req.closed_at : null,
  }
}

/** The nine lifecycle states, re-exported so the page's tests enumerate the same set the server has. */
export const DETAIL_STATES: readonly RequestStatus[] = REQUEST_STATUSES

/**
 * A link row's stable key — the same identity the rails use, so a seam-supplied
 * row and a synthesised route cannot collide in React's reconciliation.
 */
export function detailLinkKey(link: Pick<RequestLink, 'link_kind' | 'target_kind' | 'target_id'>): string {
  return `${link.link_kind}:${link.target_kind}:${link.target_id}`
}
