/**
 * My work's two team-inbox panels (AMP #3983, build slice J3; approved mockup
 * artifact 37 v1, view 3).
 *
 * The approved contract's rule that shapes everything here is that a request is
 * **not** a ticket: one shared queue per team, and being on a team gives you
 * nothing in your own list. So the two panels answer two different questions,
 * from two different server aggregates, and neither derives a number from the
 * rows it happens to have:
 *
 *  - **Team inboxes** answers "what is in this team's queue" from
 *    `totals.states` / `totals.total`. It renders **no rows at all** — a shared
 *    queue summarised as a personal list is exactly the fan-out the contract
 *    forbids.
 *  - **Claimed by me · team requests** answers "what have I taken on" from
 *    `mine`. The rows are built from `mine.ids`, which the server defines as
 *    the deduped union of `claim_user_id = me` and `owner_user_id = me` — so a
 *    request that is both claimed and assigned by me appears **once**, and a
 *    request I have merely been given access to appears **never**.
 *
 * The one thing the server cannot answer is a *page* of my owned requests:
 * `mine` carries ids (see `architecture/request-reads-rest-routes.md`), so a row
 * can only be named from a row the same read also sent. `unlistedCount` makes
 * that limitation visible instead of silently dropping a request — the same
 * choice J1 made for its owner select.
 */
import type { Me, RequestOwnership, RequestStateTotals, RequestStatus, TeamRequest } from '../types'
import { REQUEST_STATUSES } from '../types'
import { claimAge, REQUEST_STATE_LABEL } from './requestState'

/** A team as the panels need it: an id to read and a name to show. */
export interface InboxTeam {
  id: number
  name: string
}

/**
 * One team's answer. `totals` and `mine` are the server's aggregates; `rows` is
 * only ever used to *name* an id in `mine.ids`, never to count anything.
 */
export interface TeamInbox {
  team: InboxTeam
  /** The whole queue's per-state counts. Absent when the read failed or an older server omitted it. */
  totals?: RequestStateTotals
  /** The caller's claimed/assigned slice. Absent when the read failed, or the server did not compute one. */
  mine?: RequestOwnership
  /** id → row, from the same read that answered `mine`. */
  rows: Map<number, TeamRequest>
  /** Why this team's inbox could not be read. The other teams still render. */
  error?: string
}

/** One request in "Claimed by me", with how it became mine. */
export interface ClaimedRequest {
  request: TeamRequest
  team: InboxTeam
  /** `claimed` when the caller holds the triage claim; `assigned` when they only own the outcome. */
  via: 'claimed' | 'assigned'
}

/** How many of the caller's owned requests one team's read asks for. The total is cursor-free, so this only bounds the rows. */
export const CLAIMED_ROWS = 10

/**
 * The teams whose inboxes the panels cover: the caller's own memberships, in a
 * stable order.
 *
 * Only memberships, deliberately. An org owner/admin can read every team's
 * queue in their org and a global admin every queue, but `/api/me` lists the
 * teams you are *on*, and a panel that enumerated the rest would be telling you
 * about queues you have not joined. A membership read that 404s (a team deleted
 * under the session) is skipped with the rest of the panel still working — the
 * hook records that as a failed team rather than inventing a count.
 */
export function inboxTeams(me: Me | null): InboxTeam[] {
  return (me?.teams ?? [])
    .map(t => ({ id: t.id, name: t.name }))
    .sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id)
}

/** "1 request" / "3 requests" — the panels' only plural helper. */
export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}

/**
 * The per-state counts a team block shows, in lifecycle order.
 *
 * A state with nothing in it is left out rather than drawn as a zero: the block
 * is a "what is in there" line, and the states that carry the queue are the
 * news. The whole queue is still readable, because the counts that are shown
 * sum to `totals.total` — and `queueTotal` states it.
 */
export function queueStates(totals?: RequestStateTotals): { state: RequestStatus; count: number }[] {
  if (!totals) return []
  return REQUEST_STATUSES
    .map(state => ({ state, count: totals.states?.[state] ?? 0 }))
    .filter(row => row.count > 0)
}

/** The whole queue's size for one team, or 0 when the read did not answer. */
export function queueTotal(totals?: RequestStateTotals): number {
  return totals?.total ?? 0
}

/**
 * The rows of "Claimed by me · team requests".
 *
 * Built from `mine.ids` and nothing else. That list is the server's deduped
 * union (`architecture/request-queue-summaries.md`), which is what makes
 * "a request appears at most once" structural rather than a `Set` this file
 * happened to add; the guard stays anyway, so a server that ever answered the
 * same id twice still cannot put it in the panel twice.
 *
 * An id with no row in the same read is **not** invented: it is counted by
 * `unlistedCount` and the panel says so. `team` rides on the row because the
 * link destination is the team's inbox, and with more than one team the panel
 * shows it.
 */
export function claimedRequests(inboxes: TeamInbox[]): ClaimedRequest[] {
  const seen = new Set<number>()
  const out: ClaimedRequest[] = []
  for (const inbox of inboxes) {
    const meId = inbox.mine?.user_id
    for (const id of inbox.mine?.ids ?? []) {
      if (seen.has(id)) continue
      const request = inbox.rows.get(id)
      if (!request) continue
      seen.add(id)
      out.push({ request, team: inbox.team, via: request.claim_user_id != null && request.claim_user_id === meId ? 'claimed' : 'assigned' })
    }
  }
  return out
}

/** How many of the caller's owned requests the server counts, across every team. */
export function claimedTotal(inboxes: TeamInbox[]): number {
  return inboxes.reduce((n, inbox) => n + (inbox.mine?.total ?? 0), 0)
}

/**
 * Owned requests the server counts but this read could not name — the rows it
 * did not send. Never negative: a stale total that is smaller than the rows we
 * did name is not a reason to print "-1 more".
 */
export function unlistedCount(inboxes: TeamInbox[]): number {
  return Math.max(0, claimedTotal(inboxes) - claimedRequests(inboxes).length)
}

/**
 * The row's right-hand phrase: how it became yours, and how long that has been
 * true. "claimed 26h" is the anti-expiry signal the mockup draws (D4: a claim
 * never expires, so a stale one is visible rather than silently dropped).
 */
export function mineLine(row: ClaimedRequest, now = Date.now()): string {
  if (row.via === 'assigned') return 'assigned'
  const age = claimAge(row.request.claimed_at, now)
  return age ? `claimed ${age}` : 'claimed'
}

/**
 * A state's word when it follows a count: "4 new", "2 need info".
 *
 * Not just `REQUEST_STATE_LABEL` lowercased: the label is a tag on its own
 * ("Needs info"), and "2 needs info" is not a sentence. The mockup's own
 * `team-counts` draws "2 need info", so the count phrase is its own table —
 * one place, so a block and the queue's pill cannot drift apart in wording.
 */
export function stateCountLabel(state: RequestStatus): string {
  return state === 'needs_information' ? 'need info' : REQUEST_STATE_LABEL[state].toLowerCase()
}

/** The team blocks whose read failed, so the panel can say so beside the ones that answered. */
export function inboxFailures(inboxes: TeamInbox[]): TeamInbox[] {
  return inboxes.filter(inbox => !!inbox.error)
}

/**
 * Where "Open inbox" and a claimed row go: the team's queue, the entry point
 * approved mockup 37 v1 view 3 draws.
 *
 * One helper, deliberately, because **no request route exists yet**. J1 built
 * the queue as a page that takes `teamId` as a prop and #3988 registers the
 * routes; until it does, `/teams/:id/requests` is the path that ticket will
 * own. A row pointing at `/teams/:id/requests/:serial` today would be a dead
 * link to a route nobody registered, so the claimed row opens the queue and
 * #3988 retargets this one function to the detail path. Every link in both
 * panels is built here so that is a one-line change, not a hunt.
 */
export function teamInboxLink(teamId: number): string {
  return `/teams/${teamId}/requests`
}

/** A claimed row's destination: its team's queue (see `teamInboxLink`). */
export function claimedRequestLink(row: ClaimedRequest): string {
  return teamInboxLink(row.team.id)
}
