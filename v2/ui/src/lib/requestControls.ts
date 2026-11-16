/**
 * The team request detail's right rail, as pure decisions (AMP #3980, build
 * slice J2 of discoveries/team-inbox-approved-build-slices.md; approved
 * mockups artifact 37 v1 "Request detail", right column).
 *
 * Three rails live on the rail: Triage (claim vs assignee), Planning (the one
 * project route, the plan items it serves, and the un-removable handoff
 * warning) and Deliver (the outcome brief plus the evidence guard). This file
 * is what all three read instead of each deciding the same thing twice:
 *
 *  1. **Claim and assignee are two fields, never one.** The short-circuit that
 *     turns "someone is looking at it" into "it is theirs" is the D4/I4 bug the
 *     contract exists to prevent, so the two are separate values here and the
 *     rail renders them under separate labels. A transfer clears the claim —
 *     the server does it in one statement (`SetAssignee`) — so this file never
 *     has to model a request that has both a new owner and a stale claim.
 *
 *  2. **Delivery eligibility is the server's answer, not a recomputation.**
 *     `outcome.delivered_eligible` is one definition
 *     (internal/service/request_outcome.go), built from the request's own
 *     state, its brief and its live evidence. A second gate in React is how
 *     "the tickets are all done, so it's delivered" sneaks back in (I8), so the
 *     Deliver panel disables its button on that field and explains itself with
 *     the server's own `blocked_reason`.
 *
 *  3. **Nothing here approves, dispatches or completes anything.** The rails
 *     attach relations, take a claim, transfer an assignee and — only through
 *     the page — make one lifecycle move the team asked for. A planned row's
 *     "awaiting approval" tag is read from the target's own approval fact
 *     (`approved_at` on a ticket, the approved version on an artifact), never
 *     inferred from the ticket's state: a completed ticket that nobody
 *     approved is still unapproved, and a closed ticket never delivers a
 *     request.
 *
 * The links are passed in by the page rather than fetched here: the shipped
 * read route answers the record and its delivery aggregate, not the link rows,
 * and a rail that fetched on mount could act before a person asked it to.
 */
import type {
  RequestLink, RequestLinkKind, RequestOutcome, RequestOutcomeItem, RequestPhase, RequestStatus,
  RequestTargetKind, TeamRequest,
} from '../types'

// ---- Claim and assignee ----

/**
 * What the Triage rail shows in its Claim row.
 *
 * `mine` is the caller's own claim (the only claim it may release), `other` is
 * someone else's (released by them, or cleared by a transfer a manager makes),
 * and `none` is an unclaimed request. Claims never expire (D4), so there is no
 * `stale` state: the age is shown instead, which is the whole point of
 * rendering `claimed_at`.
 */
export type ClaimState =
  | { kind: 'none' }
  | { kind: 'mine'; userId: number; at: string | null }
  | { kind: 'other'; userId: number; at: string | null }

/** The Claim row's state for a request and the caller. A claim always names a user, so no id means no claim. */
export function claimState(req: Pick<TeamRequest, 'claim_user_id' | 'claimed_at'>, meId: number | undefined): ClaimState {
  const id = req.claim_user_id
  if (id == null || id <= 0) return { kind: 'none' }
  if (meId != null && id === meId) return { kind: 'mine', userId: id, at: req.claimed_at }
  return { kind: 'other', userId: id, at: req.claimed_at }
}

/**
 * Whether the caller may take this request's claim.
 *
 * Mirrors the server's `ClaimRequest`: the request must be unclaimed, or
 * already the caller's (a retry, which changes nothing). A claim held by
 * anyone else is a 409 naming the holder — never a silent take-over — so the
 * button is offered and the conflict banner explains the loss, rather than the
 * button being hidden and the race becoming invisible.
 */
export function canClaim(req: Pick<TeamRequest, 'claim_user_id'>, meId: number | undefined): boolean {
  if (meId == null) return false
  return req.claim_user_id == null || req.claim_user_id === meId
}

/** Whether the caller may release the claim: its holder, and nobody else. */
export function canRelease(req: Pick<TeamRequest, 'claim_user_id'>, meId: number | undefined): boolean {
  return meId != null && req.claim_user_id === meId
}

// ---- The three triage acts, on the real transition graph ----

/**
 * Which triage acts the request's state allows, and the capability each needs.
 *
 * This is the server's graph written out (internal/service/request_transitions.go,
 * pinned by request_transitions_test.go's `expectedLifecycle`), not a guess from
 * the state's name. Three consequences a component would get wrong by intuition:
 *
 *  - **Asking is not available on `new`.** The graph's first question edge leaves
 *    `triaging`; a request nobody has picked up is claimed before it is queried.
 *  - **Reject and duplicate need the manager row**, not merely team membership
 *    (I3: the team's closes are a lead's act) — so a plain member sees them
 *    disabled with the reason, rather than a button that 403s.
 *  - **Both are available in almost every open state**, including `in_progress`,
 *    and asking is available from `rejected`/`duplicate` too — that edge reopens
 *    the request, which is how the team reconsiders a close.
 */
export interface TriageActs {
  ask: { available: boolean; reason?: string }
  reject: { available: boolean; reason?: string }
  duplicate: { available: boolean; reason?: string }
}

/** States a question may be asked from without reopening the request first. */
const ASK_STATES: readonly RequestStatus[] = ['triaging', 'accepted', 'in_progress']
/** States a question may be asked from by reopening it (the graph's reopen edges). */
const ASK_REOPEN_STATES: readonly RequestStatus[] = ['rejected', 'duplicate']
/** States the team may close as rejected. `delivered`/`requester_accepted` are the requester's to reopen. */
const CLOSE_STATES: readonly RequestStatus[] = ['new', 'triaging', 'needs_information', 'accepted', 'in_progress']

/**
 * The acts available on a request, for a caller who may (or may not) be a team
 * manager. `manager` is the server's `RequireTeamManager`: a team lead, or an
 * org owner/admin.
 */
export function triageActs(state: RequestStatus, manager: boolean): TriageActs {
  const askable = ASK_STATES.includes(state) || ASK_REOPEN_STATES.includes(state)
  const closable = CLOSE_STATES.includes(state)
  const deliveryState = state === 'delivered' || state === 'requester_accepted'
  return {
    ask: askable
      ? { available: true }
      : { available: false, reason: state === 'new'
          ? 'Claim the request first: a question leaves from Triaging.'
          : deliveryState
            ? 'Delivered: the team can no longer ask, only the requester can accept or reopen (I1).'
            : 'This request cannot be asked about in its current state.' },
    reject: closable
      ? (manager ? { available: true } : { available: false, reason: 'Declining a request is a lead’s act (I3): ask a team lead or an org owner to do it.' })
      : { available: false, reason: deliveryState
          ? 'Delivered: the requester decides whether it answers their request, not the team.'
          : 'This request is already closed.' },
    duplicate: closable
      ? (manager ? { available: true } : { available: false, reason: 'Folding a request into another is a lead’s act (I3): ask a team lead or an org owner to do it.' })
      : { available: false, reason: deliveryState
          ? 'Delivered: folding it into another request would hide a delivery the requester may accept.'
          : 'This request is already closed.' },
  }
}

// ---- The claim conflict (409) ----

/**
 * A lost claim race, as the banner tells it.
 *
 * The mockup's copy is a promise about the failure: name who won, say the
 * claim is first-write-wins, and say that nothing was sent. All three are
 * carried here so the component cannot render a conflict that leaves the
 * sentence out.
 */
export interface ClaimConflict {
  /** The user id the server named as the holder (`claim_user_id` on the 409). */
  holderUserId?: number
  /** The holder's display name, when the page could resolve it. */
  holderName?: string
  /** The revision the row is actually at, when the server sent one. */
  rev?: number
  /** The revision the page rendered when the action was taken. */
  fromRev: number
  /** The refusal's message, as the server wrote it. */
  message: string
}

/**
 * Read a thrown write failure as a claim conflict, or null when it is not one.
 *
 * A 409 on this surface is one status for several causes (a stale revision, a
 * lost claim, a refused re-route), so this returns a conflict only when the
 * error carries the fact that makes it a claim loss: the holder's id. A stale
 * revision is a different banner ("reload, your view is old") with no winner to
 * name, and the page renders it as such.
 */
export function readClaimConflict(err: unknown, fromRev: number, holderName?: string): ClaimConflict | null {
  const e = err as { status?: number; rev?: number; claimUserId?: number; message?: string } | null
  if (!e || e.status !== 409 || typeof e.claimUserId !== 'number' || e.claimUserId <= 0) return null
  return {
    holderUserId: e.claimUserId,
    holderName,
    rev: e.rev,
    fromRev,
    message: e.message ?? '',
  }
}

/** The conflict banner's sentences: who won, what the rule is, and that nothing was sent. */
export function claimConflictCopy(c: ClaimConflict): { title: string; detail: string } {
  const who = c.holderName?.trim() || `User #${c.holderUserId ?? 0}`
  const moved = c.rev != null && c.rev !== c.fromRev ? ` (rev ${c.fromRev} → ${c.rev})` : ''
  return {
    title: `${who} claimed this request first.`,
    detail: `Your view was from before that${moved}. Claims are first-write-wins — refresh to see their claim, ` +
      'or ask them to hand it over. Nothing you typed was sent.',
  }
}

// ---- Planning: the route, the plan items, the approval fact ----

/**
 * What a link's target is, as the page resolved it from the target's own row.
 *
 * The rails never invent a title, and never decide approval: `approved` is the
 * target's own fact (a ticket's `approved_at >= planned_at`, an artifact's
 * approved version), and `gone` is the tombstone a deleted target leaves
 * behind (055 has no FK on `target_id` on purpose).
 */
export interface RequestLinkTarget {
  kind: RequestTargetKind
  id: number
  /** Its display name — "AMP-V2-3712 — Berlin SSO group mapping", "Artifact #41". */
  title: string
  /** Its project, when it has one (a receipt may not). */
  projectId?: number
  /** For planned work: whether a person approved this item's current plan. Absent means "not known here". */
  approved?: boolean
  /** For delivery evidence: whether the work is on the main line (a merged ticket, an approved artifact). */
  merged?: boolean
  /** The target no longer resolves: the link outlived it, which is a tombstone, not silence. */
  gone?: boolean
}

/** One link with its resolved target. */
export interface ResolvedRequestLink extends RequestLink {
  target?: RequestLinkTarget
}

/** The planning rail's contents: the one route, the plan items, and the related requests. */
export interface RequestPlanning {
  /** The project the request is routed to (`routes_to`). At most one is active (D9). */
  route?: ResolvedRequestLink
  /** `planned_as` rows: the epics, stories and tickets serving the request. */
  planned: ResolvedRequestLink[]
  /** `relates_to` rows: related requests. Never planning, never delivery. */
  related: ResolvedRequestLink[]
}

/** Group a request's links by what they mean to the rail. Order is preserved (attach order). */
export function groupLinks(links: readonly ResolvedRequestLink[]): RequestPlanning & { delivered: ResolvedRequestLink[]; duplicates: ResolvedRequestLink[] } {
  const out = { planned: [], related: [], delivered: [], duplicates: [] } as RequestPlanning & {
    delivered: ResolvedRequestLink[]; duplicates: ResolvedRequestLink[]
  }
  for (const l of links) {
    switch (l.link_kind) {
      case 'routes_to':
        // The first active route wins, matching RequestOutcome.RoutedProjectID: a
        // re-route supersedes rather than appends a second active route.
        if (!out.route) out.route = l
        break
      case 'planned_as': out.planned.push(l); break
      case 'relates_to': out.related.push(l); break
      case 'delivered_by': out.delivered.push(l); break
      case 'duplicate_of': out.duplicates.push(l); break
    }
  }
  return out
}

/**
 * Whether the request is planned at all — the "is anything attached yet?" question.
 *
 * A route alone counts: routing a request to a project is planning it, even
 * before a ticket exists. A tombstoned link still counts, because "we planned
 * this and the plan changed" is not "we never planned it" (the aggregate's own
 * rule, which is why `Linked` counts the link rather than the live target).
 */
export function isPlanned(planning: Pick<RequestPlanning, 'route' | 'planned'>): boolean {
  return !!planning.route || planning.planned.length > 0
}

/**
 * The planning rail's approval summary: how many planned items a person still
 * has to approve, and how many of those are gone.
 *
 * This is the number the rail states next to its rows so "awaiting approval" is
 * never a per-row decoration a reader has to add up. An unknown approval
 * (`approved` absent, because the target could not be resolved) is counted as
 * not approved: the rail must never claim more approval than it can prove.
 */
export function plannedApproval(planning: Pick<RequestPlanning, 'planned'>): { total: number; approved: number; awaiting: number; gone: number } {
  let approved = 0, gone = 0
  for (const l of planning.planned) {
    if (l.target?.gone) { gone++; continue }
    if (l.target?.approved === true) approved++
  }
  return { total: planning.planned.length, approved, awaiting: planning.planned.length - approved - gone, gone }
}

/**
 * The one sentence the handoff warning makes, in the approved mockup's words.
 *
 * It is exported as data rather than typed into the component so a test can
 * assert it is rendered on every planning state — "Handoff never auto-approves"
 * is the invariant this rail exists to keep visible (contract §3, slice J2's
 * third criterion), and a component that dropped it would still compile.
 */
export const HANDOFF_WARNING = {
  title: 'Handoff never auto-approves.',
  detail: 'Plan items created from this request land as tickets awaiting a person\u2019s approval — the request ' +
    'text reaches agents as quoted, untrusted data. Nothing here dispatches work.',
} as const

// ---- Delivery: the evidence guard ----

/**
 * What the Deliver panel may and may not do right now.
 *
 * `ready` is the server's `delivered_eligible`: the request reached a delivery
 * state, its brief is set and it has live evidence (or the explicit no-artifact
 * record). Everything else is a refusal the panel states before the button is
 * pressed, in the contract's words:
 *
 *  - `state` — the request is not in progress yet (the one delivery edge).
 *  - `brief` — no outcome brief: a delivery without one is an assertion, not a
 *    statement of what was and was not included (I2).
 *  - `evidence` — neither a live delivered link nor the no-artifact record.
 *  - `already` — it is delivered; the panel switches to showing the delivery.
 *  - `closed` — rejected or duplicate: there is nothing to deliver.
 *  - `act` — everything a delivery needs is present and the server still does not
 *    consider it eligible, so the only thing missing is the act itself. This is
 *    not `state`: telling a team to move a request that is already in progress
 *    would be the panel misreading the row it is rendering.
 */
export type DeliverBlock = 'none' | 'state' | 'brief' | 'evidence' | 'act' | 'already' | 'closed'

export interface DeliverGuard {
  /** The button is enabled exactly when this is true. */
  ready: boolean
  /** Which guard is holding it back (`none` when ready). */
  block: DeliverBlock
  /** What to say: the server's own `blocked_reason` when it sent one, else this file's sentence for `block`. */
  reason: string
  /** Whether a delivery already happened (so the panel shows it instead of offering the act). */
  delivered: boolean
}

/** States a request is delivered in. Both mean "the desk stated the outcome exists". */
const DELIVERED_STATES: readonly RequestStatus[] = ['delivered', 'requester_accepted']

/** The stage of the lifecycle at which the delivery edge leaves: in_progress and nothing else. */
const DELIVERABLE_STATE: RequestStatus = 'in_progress'

/**
 * The Deliver panel's guard, from the request and the server's aggregate.
 *
 * The order matters: an already-delivered request is not "blocked", and a
 * rejected one is not "needs a brief" — telling the team to write a brief for a
 * request they declined would be the panel misunderstanding the lifecycle.
 */
export function deliverGuard(req: Pick<TeamRequest, 'state' | 'outcome_brief'>, outcome: RequestOutcome): DeliverGuard {
  const delivered = DELIVERED_STATES.includes(req.state)
  if (delivered) {
    const eligible = outcome.delivered_eligible
    return {
      ready: false, block: 'already', delivered: true,
      reason: eligible
        ? 'Delivered. Acceptance belongs to the requester alone — no one on the team can accept it for them.'
        : (outcome.blocked_reason?.trim() || 'Delivered.'),
    }
  }
  if (req.state === 'rejected' || req.state === 'duplicate') {
    return {
      ready: false, block: 'closed', delivered: false,
      reason: req.state === 'rejected'
        ? 'This request was rejected: there is nothing to deliver. Reopen it first if that was wrong.'
        : 'This request is a duplicate: deliver the canonical request instead.',
    }
  }
  // The server's own reason first: it is the one definition of what a delivery
  // needs, and a sentence composed here could drift from it.
  const serverReason = outcome.blocked_reason?.trim() ?? ''
  if (outcome.delivered_eligible && req.state === DELIVERABLE_STATE) {
    return { ready: true, block: 'none', reason: serverReason, delivered: false }
  }
  const briefSet = outcome.outcome_brief_set || req.outcome_brief.trim() !== ''
  const hasEvidence = outcome.no_artifact || outcome.total > 0
  const block: DeliverBlock = req.state !== DELIVERABLE_STATE
    ? 'state'
    : !briefSet
      ? 'brief'
      : !hasEvidence
        ? 'evidence'
        : 'act'
  return { ready: false, block, delivered: false, reason: serverReason || deliverBlockReason(block) }
}

/** The sentence a guard states when the server sent no `blocked_reason` of its own. */
export function deliverBlockReason(block: DeliverBlock): string {
  switch (block) {
    case 'state': return 'Delivery leaves from In progress: move the request there before marking it delivered.'
    case 'brief': return 'Delivery needs an outcome brief: what is and is not included (I2).'
    case 'evidence': return 'Delivery needs evidence: link the work that delivered it (a task, an artifact or a receipt), ' +
      'or state explicitly that there is nothing to link.'
    case 'act': return 'Everything a delivery needs is in place: the team has not delivered it yet.'
    case 'already': return 'This request is already delivered.'
    case 'closed': return 'This request is closed: there is nothing to deliver.'
    default: return ''
  }
}

/** The request's delivery note, trimmed, or null when the team has not written one. */
export function outcomeBrief(req: Pick<TeamRequest, 'outcome_brief'>): string | null {
  const b = req.outcome_brief.trim()
  return b === '' ? null : b
}

// ---- Delivery evidence rows ----

/** One evidence row's tag: what it says about that piece of work, in the mockup's three tones. */
export type EvidenceTone = 'ok' | 'rev' | 'chg' | 'gone'

export interface EvidenceRow {
  link: ResolvedRequestLink
  /** The state word for the row ("merged", "approved", "in review", "pending verify", "deleted", …). */
  word: string
  tone: EvidenceTone
  /** Why the row cannot count as delivery evidence right now; absent when it can. */
  note?: string
}

/**
 * The delivery's evidence rows, from the aggregate's items joined to the links.
 *
 * The aggregate is the source for each piece's state (`RequestOutcomeItem`), and
 * the link is the source for what it is called; a link with no item is a
 * tombstone the aggregate reports as deleted. Both halves are needed, so a row
 * can never show "done" for a piece the aggregate counts as gating the
 * delivery.
 *
 * `outcome.items` is `null` — not `[]` — whenever the request has no
 * `delivered_by` links, which is every request before its first delivery: the
 * server builds the list with `append` on a nil slice, and Go marshals a nil
 * slice as `null`. Iterating it bare threw `TypeError: ... is not iterable` and
 * unmounted the whole detail page (AMP #4282), so the guard is load-bearing.
 */
export function evidenceRows(links: readonly ResolvedRequestLink[], outcome: RequestOutcome): EvidenceRow[] {
  const items = new Map<string, RequestOutcomeItem>()
  for (const it of outcome.items ?? []) items.set(`${it.kind}:${it.id}`, it)
  return links.map(link => {
    const item = isEvidenceKind(link.target_kind) ? items.get(`${link.target_kind}:${link.target_id}`) : undefined
    const gone = link.target?.gone === true || item?.state === 'deleted'
    if (gone) {
      return {
        link, word: 'deleted', tone: 'gone' as const,
        note: item?.deleted_at
          ? `The target was deleted ${item.deleted_at.slice(0, 10)}. A deleted target is not delivery evidence: re-plan the piece, or state there is nothing to link.`
          : 'The target no longer exists, so it is not delivery evidence.',
      }
    }
    switch (item?.state) {
      case 'done':
        return { link, word: link.target_kind === 'artifact' ? 'approved' : 'merged', tone: 'ok' as const }
      case 'merge_gated':
        return {
          link, word: 'merge needed', tone: 'chg' as const,
          note: 'The work is done but its merge is not recorded, so it is not on the main line yet.',
        }
      case 'held':
        return {
          link, word: 'pending verify', tone: 'chg' as const,
          note: item.verification_status ? `The receipt is ${item.verification_status}: held for a person, not available as evidence.` : undefined,
        }
      case 'blocked':
        return {
          link, word: 'blocked', tone: 'chg' as const,
          note: 'A person has to answer a question on the linked work before it moves.',
        }
      case 'unverified':
        return {
          link, word: 'in review', tone: 'rev' as const,
          note: 'The outcome exists, but the current version is not approved.',
        }
      case 'open':
        return { link, word: 'open', tone: 'rev' as const }
      default:
        // No aggregate item: the link names something the aggregate did not
        // report on, so the only honest word is that it is not verified here.
        return {
          link, word: 'unverified', tone: 'chg' as const,
          note: 'This piece was not part of the delivery aggregate, so it cannot be counted as evidence.',
        }
    }
  })
}

/** The three target kinds that can evidence a delivery at all (D10) — the aggregate reports items only for these. */
export function isEvidenceKind(kind: RequestTargetKind): kind is 'task' | 'artifact' | 'receipt' {
  return kind === 'task' || kind === 'artifact' || kind === 'receipt'
}

// ---- Counts, phases and the rail's summary line ----

/** The one word for the planning state: "routed", "planned", "needs a person", … */
export function phaseWord(phase: RequestPhase): string {
  switch (phase) {
    case 'unlinked': return 'Nothing attached yet'
    case 'not_started': return 'Planned, not started'
    case 'in_progress': return 'Work in progress'
    case 'blocked': return 'Needs a person'
    case 'delivered': return 'Delivered'
    case 'closed': return 'Closed'
  }
}

/** The honest one-line progress summary: how many pieces are done, gated, held or gone. */
export function outcomeSummary(out: RequestOutcome): string {
  if (out.total === 0) {
    return out.no_artifact
      ? 'Delivered with no artifact: the team stated there is nothing to link.'
      : 'No delivered work is linked yet.'
  }
  const parts = [`${out.done} of ${out.total} done`]
  if (out.gated) parts.push(`${out.gated} waiting on a merge`)
  if (out.held) parts.push(`${out.held} pending verify`)
  if (out.blocked) parts.push(`${out.blocked} needing a person`)
  if (out.deleted) parts.push(`${out.deleted} deleted`)
  if (out.rework) parts.push(`${out.rework} abandoned run${out.rework === 1 ? '' : 's'}`)
  return parts.join(' · ')
}

/**
 * The sentence that keeps a finished-looking request from reading as delivered
 * (I8). Rendered whenever every piece is done and the request still is not
 * delivered, which is exactly the state the contract says must not be mistaken
 * for an outcome.
 */
export function notDeliveredYet(out: RequestOutcome, state: RequestStatus): string | null {
  if (out.total === 0 || out.phase === 'delivered' || out.phase === 'closed') return null
  if (out.done !== out.total) return null
  return DELIVERED_STATES.includes(state)
    ? null
    : 'Every linked piece of work is done, and the request is still not delivered: closing work never delivers a ' +
      'request. The team states the outcome, with what is and is not included.'
}

// ---- Attach actions' input helpers ----

/** The target kinds a planning rail may attach, from the server's own matrix. */
export const PLANNING_TARGETS: readonly RequestTargetKind[] = ['epic', 'story', 'task']

/** The relation a planning attach writes for a target kind. */
export function planningLinkKind(kind: RequestTargetKind): RequestLinkKind {
  return kind === 'project' ? 'routes_to' : 'planned_as'
}

/** A link's identity: the server's unique key, so a retry can be recognised client-side before it is sent. */
export function linkKey(link: Pick<RequestLink, 'link_kind' | 'target_kind' | 'target_id'>): string {
  return `${link.link_kind}:${link.target_kind}:${link.target_id}`
}

/**
 * Whether a link is already present — the client-side half of the attach's
 * idempotency.
 *
 * The server converges a replay on its own (`ON CONFLICT DO NOTHING`, and the
 * service answers `changed: false`), so this is not what makes attaching safe.
 * It is what makes the button say so: an attach the team already made is shown
 * as attached rather than offered again, and a retry after a lost response
 * reports "already linked" instead of pretending it just created the relation.
 */
export function alreadyLinked(links: readonly Pick<RequestLink, 'link_kind' | 'target_kind' | 'target_id'>[], want: Pick<RequestLink, 'link_kind' | 'target_kind' | 'target_id'>): boolean {
  const key = linkKey(want)
  return links.some(l => linkKey(l) === key)
}

/**
 * Read a thrown attach failure as something the rail can say.
 *
 * Three outcomes, and they are the three the approved mockup talks about: the
 * link was written before (a replay, which is success), the request is routed
 * somewhere else (D9's 409, which offers a deliberate re-route), or it failed
 * for another reason and says so verbatim. Nothing here retries on its own: an
 * attach is a person's act, and a UI that retried it silently would be guessing
 * at their intent.
 */
export type AttachOutcome =
  | { kind: 'linked'; changed: boolean }
  | { kind: 'rerouted'; message: string }
  | { kind: 'failed'; message: string }

export function readAttachFailure(err: unknown): AttachOutcome {
  const e = err as { status?: number; code?: string; message?: string } | null
  const message = e?.message ?? 'The link could not be written.'
  if (e?.status === 409) return { kind: 'rerouted', message }
  return { kind: 'failed', message }
}

/**
 * The sentence under the Deliver button: what is missing, in the guard's own
 * words, and — when everything else is in place — that the act itself is all
 * that is left.
 *
 * It is stated rather than hovered because a disabled control that explains
 * itself only in a `title` explains itself to nobody using a keyboard or a
 * screen reader.
 */
export function deliverGuardSentence(guard: DeliverGuard, briefTyped: boolean, canDeliver: boolean): string {
  if (!canDeliver) return 'Only the inbox team can deliver this request.'
  if (guard.ready && !briefTyped) return 'Write the outcome brief above; the delivery needs it.'
  if (guard.ready) return 'Everything a delivery needs is in place: record it when the outcome is true.'
  if (guard.block === 'brief' && briefTyped) {
    return 'The brief is ready; the delivery still needs evidence linked below, or the explicit no-artifact record.'
  }
  return guard.reason
}
