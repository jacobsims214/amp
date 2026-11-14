/**
 * Where the request surfaces live, and what a URL has to carry to reach them
 * (AMP #3988; build slice map `discoveries/team-inbox-approved-build-slices.md`
 * J1/J2/J4/J5/J6; approved mockups artifact 37 v1 team side, 38 v1 requester
 * side).
 *
 * Every path in the product that names a request is built here, and nothing
 * anywhere else spells one. That is not tidiness: two facts about these URLs are
 * easy to get wrong and impossible to notice until a person holds a dead link.
 *
 * ## 1. The requester routes are outside the app shell, and the paths say so
 *
 * `/teams/:teamId/requests` is the team's queue and `/requests` is a person's own
 * list. They differ by one segment and could not be more different: the first is
 * staff-only and renders inside `AppShell` (the project tree, org admin, every
 * internal name); the second is a customer's, outside the shell entirely
 * (`components/requests/RequesterChrome`). Keeping the builders together means
 * the difference stays visible in one file instead of being a coincidence of two
 * `<Route>` lines in `App.tsx`.
 *
 * ## 2. A requester knows their desk, and the server cannot tell them
 *
 * The approver mockup's frame shows one team fact: the desk's public display
 * name. But a requester has no org standing, so `GET /api/teams/:id` answers
 * them the surface's 404 — and `RequesterRequestView`, the whitelist they *can*
 * read, carries `team_name` but never a team id. So `/requests/new` cannot look
 * its desk up; the **entry link the desk sent** has to carry both, which is why
 * the two `?team=` / `?desk=` parameters exist and why they are threaded through
 * every in-app link once they are known.
 *
 * The id is needed to submit (`POST /api/teams/:id/requests`); the name is needed
 * to say who you are writing to. When the name is missing the pages use the same
 * fallback the status screen already uses ("the desk"), because "Tell the desk
 * what you need" is grammatical and honest, while "Tell  what you need" is a
 * broken sentence on the first screen a customer ever sees.
 *
 * The id is **required** in practice: with no desk there is nowhere to send, so
 * `requesterNewRequestPath` and the "New request" affordance are only offered
 * when an entry link supplied one. A bare `/requests` deep link still lists
 * what the person sent, which is the honest answer to a URL that names no desk.
 */
import type { Me } from '../types'

/** The two query keys `#3988` threads from an entry link through the requester's own screens. */
export const DESK_TEAM_PARAM = 'team'
export const DESK_NAME_PARAM = 'desk'

/**
 * Which inbox a requester is writing to, as far as their own URLs know it.
 *
 * Both are optional because a URL is not a promise: a person can paste
 * `/requests` with nothing on it, and a page must still answer something true.
 */
export interface RequesterDesk {
  /** The inbox to submit to. Absent means "no desk named", so no form can be offered. */
  teamId?: number
  /** The desk's public display name. Absent falls back to "the desk" in copy. */
  teamName?: string
}

/** The sentence-less fallback for a frame that has to name a desk it was not told about. */
export const UNKNOWN_DESK = 'the desk'

/**
 * oauth2-proxy clears the session and returns to the signed-out home page. The
 * same href `components/shell/IdentityMenu` uses, so the two sign-outs cannot
 * disagree.
 */
export const SIGN_OUT_URL = '/oauth2/sign_out?rd=%2F'

/**
 * Whether this identity is a customer — an account that exists to send requests
 * and read its own, with no org or team standing (contract §2, D2).
 *
 * Compared as a **string** on purpose. `types/index.ts`'s `UserKind` still reads
 * `'human' | 'agent'`, and widening it is a client-type change this ticket does
 * not own; but `/api/me` has answered `kind: "customer"` since the customer
 * identity landed, and a customer who opens a team URL must not get the staff
 * frame in the meantime. So this reads the value the API actually sends rather
 * than the value the type admits — when `UserKind` gains the member, this
 * function needs no change at all.
 *
 * A customer is the one role fact a URL decision may use here: it is stated
 * outright by `/api/me`, unlike inbox membership, which is the server's answer
 * on every call and which this module deliberately does not re-derive.
 */
export function isCustomerIdentity(me: { kind?: string } | null | undefined): boolean {
  return String(me?.kind ?? '') === 'customer'
}

// ---- Team side (inside AppShell) ----

/**
 * A team's shared queue. The approved queue screen's own entry point (slice J1),
 * and the destination of My work's "Open inbox".
 *
 * Not project-scoped on purpose: an inbox is a team's, and a request routes to a
 * project later, if at all.
 */
export function teamInboxPath(teamId: number): string {
  return `/teams/${teamId}/requests`
}

/**
 * One request as its inbox sees it (slice J2).
 *
 * Addressed by the **serial or the public reference** — the team read accepts
 * either. Every link this app builds passes the serial, because the row it was
 * built from carries one and a serial is what a team *write* needs (a reference
 * is refused on a team write path; see `api/client.ts`).
 */
export function teamRequestPath(teamId: number, ref: number | string): string {
  return `${teamInboxPath(teamId)}/${encodeURIComponent(String(ref))}`
}

/**
 * Which org the caller's role in a team's org comes from, for the detail page's
 * manager checks.
 *
 * `me.teams` carries the team's `org_id`, which is the only link between a team
 * and the org role the server's `RequireTeamManager` also accepts. A team the
 * caller is not on answers `undefined` — and that is the right answer for an org
 * admin browsing an inbox they were never added to only if `me.orgs` names them,
 * which it does; the miss is for a genuine stranger.
 */
export function teamOrgRole(me: Me | null, teamId: number): string | undefined {
  const team = me?.teams?.find(t => t.id === teamId)
  if (!team) return undefined
  return me?.orgs?.find(o => o.id === team.org_id)?.my_role || undefined
}

// ---- Requester side (outside AppShell) ----

/** The query string that carries a desk through the requester's own links. Empty when nothing is known. */
export function deskSearch(desk: RequesterDesk = {}): string {
  const q = new URLSearchParams()
  if (desk.teamId != null && Number.isFinite(desk.teamId)) q.set(DESK_TEAM_PARAM, String(desk.teamId))
  const name = desk.teamName?.trim()
  if (name) q.set(DESK_NAME_PARAM, name)
  const s = q.toString()
  return s ? `?${s}` : ''
}

/**
 * Read a desk off a URL. A `team` that is not a positive integer is treated as
 * absent rather than as `NaN`: a hand-edited URL must not turn into a submit to
 * `/api/teams/NaN/requests`.
 */
export function readDesk(params: URLSearchParams): RequesterDesk {
  const rawTeam = params.get(DESK_TEAM_PARAM)
  const teamId = rawTeam != null && /^\d+$/.test(rawTeam.trim()) ? Number(rawTeam.trim()) : undefined
  const teamName = params.get(DESK_NAME_PARAM)?.trim() || undefined
  return { ...(teamId && teamId > 0 ? { teamId } : {}), ...(teamName ? { teamName } : {}) }
}

/** The person's own requests (slice J5). */
export function requesterListPath(desk: RequesterDesk = {}): string {
  return `/requests${deskSearch(desk)}`
}

/** The submit form (slice J4). */
export function requesterNewRequestPath(desk: RequesterDesk = {}): string {
  return `/requests/new${deskSearch(desk)}`
}

/**
 * One request, as the person who sent it sees it (slice J6).
 *
 * The reference is the only address a requester holds, and it is **case
 * sensitive** (base64url; the server resolves `WHERE public_ref = $1`), so it
 * travels exactly as it arrived — no upper-casing, no `REQ-` prefix added here.
 * The display form is `requestRef()`'s job, at render time only.
 */
export function requesterStatusPath(publicRef: string, desk: RequesterDesk = {}): string {
  return `/request/${encodeURIComponent(publicRef.trim())}${deskSearch(desk)}`
}