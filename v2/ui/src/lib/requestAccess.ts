/**
 * Who may send requests to one inbox, as pure decisions and the exact sentences
 * (AMP #3982; approved contract artifact 36 v2 §2 and §5; build slice map
 * `discoveries/team-inbox-approved-build-slices.md`).
 *
 * This file exists for one reason: **submit access and membership are two
 * different things, and the interface must never blur them.** A grant lets a
 * person send a request to one team's inbox; it reaches no project, confers no
 * role and creates no `team_members` row (the server writes only
 * `team_submit_grants`, see `architecture/request-grants-rest-routes.md`). A
 * requester invitation provisions a customer identity and grants no role at all.
 * The copy lives here, in one place, so the panel's headings, its notes and its
 * success lines cannot drift into "add to the team" — the one wrong idea a
 * screen like this can plant.
 *
 * The ordering decisions are here too, because they are decisions rather than
 * markup: `grantRows` puts grants in force ahead of revoked ones (a manager
 * asking "who can submit" should not have to scan past history to find out), and
 * `submitScopedTokens` answers only the credentials bound to *this* inbox.
 *
 * Nothing here decides permission: whether a caller may grant, invite, revoke or
 * mint is `authz.RequireTeamManager`'s answer on every call, and a UI that
 * re-derived it would only be inventing a second rule. `canManageSubmitAccess`
 * is the one exception, and it is deliberately an *optimistic hint* — see its
 * own note.
 */
import type { APIToken, Me, Team, TeamSubmitGrantee } from '../types'

/** The rule the panel states before any control: one sentence, no hedging. */
export const SUBMIT_ACCESS_IS_NOT_MEMBERSHIP =
  'Submit access is a revocable grant on this inbox, never team membership: a requester reaches no project, ' +
  'reads no ticket and holds no role.'

/** What a requester invitation is, and the one thing it is not. */
export const REQUESTER_INVITE_RULE =
  'A requester invitation creates a customer identity at their first sign-in and grants no org or team role.'

/** What a submit-only credential may reach — and the length of the list is the point. */
export const SUBMIT_CREDENTIAL_RULE =
  'A submit-only credential can send requests to this inbox and do nothing else: no reads, no lists, ' +
  'no access administration, no approvals.'

/** Why a viewer who cannot manage the inbox is seeing an explanation instead of controls. */
export const MANAGERS_ONLY =
  'Only this team’s leads and its org’s owners and admins can change who may submit.'

/** A grant's state as the panel groups it. `revoked` keeps its row: to a manager, "who was taken away" is the same question. */
export type GrantState = 'active' | 'revoked'

export function grantState(grant: TeamSubmitGrantee): GrantState {
  return grant.revoked_at ? 'revoked' : 'active'
}

/**
 * The listing's order: in force first, then revoked, each keeping the server's
 * order inside its group.
 *
 * The server already answers this way (`ListTeamSubmitGrants`), and the panel
 * still groups rather than trusting that: a screen whose "who can submit"
 * section silently interleaves revoked rows would answer the wrong question
 * first, and the grouping is the panel's promise, not the transport's.
 */
export function grantRows(grants: TeamSubmitGrantee[] | null): TeamSubmitGrantee[] {
  const all = grants ?? []
  return [...all.filter(g => grantState(g) === 'active'), ...all.filter(g => grantState(g) === 'revoked')]
}

export function activeGrantCount(grants: TeamSubmitGrantee[] | null): number {
  return (grants ?? []).filter(g => grantState(g) === 'active').length
}

/** The person a grant names: their display name, else their email, else the id (never blank). */
export function granteeName(grant: TeamSubmitGrantee): string {
  return grant.display_name?.trim() || grant.email?.trim() || `User #${grant.user_id}`
}

/**
 * Whether a token is a submit-only credential bound to this inbox.
 *
 * Both facts are required. A full-scope token has no `scope_team_id`, and a
 * credential bound to another team is that team's business — showing either in
 * this inbox's list would claim an attenuation the token does not have.
 */
export function isSubmitScopedToken(token: APIToken, teamId: number): boolean {
  return token.scope === 'submit' && token.scope_team_id === teamId
}

/** The inbox's live submit-only credentials, in the server's order. Revoked ones are dropped, as the token list drops them. */
export function submitScopedTokens(tokens: APIToken[] | null, teamId: number): APIToken[] {
  return (tokens ?? []).filter(t => !t.revoked_at && isSubmitScopedToken(t, teamId))
}

/**
 * A moment on a grant or credential row, as people say dates.
 *
 * Deliberately different from `lib/approval`'s `ago`: that one is for a list of
 * things that already happened, so it never has to show a future moment.
 * A credential's expiry always is one, and `ago` renders it as a full locale
 * timestamp ("2/11/2027, 12:24:27 PM") that reads like an error message. So
 * this answers "under an hour ago" in minutes, "under two days" in hours, and
 * anything else as a date — past or future alike.
 *
 * `now` is a parameter for the same reason `credentialExpired` takes one: a
 * render that reads the clock is not idempotent, and a row's label changing
 * between two renders of the same data is a bug, not a live clock.
 */
export function dayLabel(at: string | null | undefined, now: number): string {
  if (!at) return 'never'
  const ms = Date.parse(at)
  if (Number.isNaN(ms)) return 'never'
  const mins = Math.floor((now - ms) / 60_000)
  if (mins >= 0 && mins < 60) return mins < 1 ? 'just now' : `${mins}m ago`
  if (mins >= 60 && mins < 48 * 60) return `${Math.floor(mins / 60)}h ago`
  return new Date(ms).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
}

/**
 * Whether a credential has passed its expiry.
 *
 * `now` is a parameter rather than a `Date.now()` inside the component: a
 * render that reads the clock is not idempotent (react-hooks' purity rule), and
 * a list of credentials is a thing a test wants to see at a pinned moment. The
 * server still refuses an expired token on every request — this only labels it.
 */
export function credentialExpired(token: APIToken, now: number): boolean {
  return !!token.expires_at && Date.parse(token.expires_at) < now
}

/**
 * The credential's prefix, as the only thing left of it once the secret is
 * dismissed: proof that the panel reads a token back as `amp_ab12…` and never
 * as the secret itself (the API has no route that returns one).
 */
export function tokenPrefix(token: APIToken): string {
  return token.prefix ? `${token.prefix}…` : `#${token.id}`
}

/**
 * Whether the signed-in viewer may administer this inbox's submit access.
 *
 * This is the UI's *hint*, and it mirrors `authz.RequireTeamManager` exactly:
 * a global admin, an owner or admin of the team's org, or a lead of the team.
 * It is deliberately wide rather than narrow — the manager gate is the server's
 * on every write, so a false positive here costs one refused call and a
 * readable message, while a false negative would hide a control from somebody
 * who is entitled to it. A local-dev identity is a global admin (auth is off).
 *
 * It is a function here rather than five lines inside the component because the
 * panel's read-only state quotes it: "only this team's leads and its org's
 * owners and admins can change who may submit" has to be the truth about the
 * rule it is refusing on behalf of.
 */
export function canManageSubmitAccess(me: Me | null | undefined, team: Team | null | undefined, isAdmin = false): boolean {
  if (!team) return false
  if (isAdmin || me?.local_dev || (me?.roles ?? []).includes('admin')) return true
  const org = (me?.orgs ?? []).find(o => o.id === team.org_id)
  if (org?.my_role === 'owner' || org?.my_role === 'admin') return true
  return (me?.teams ?? []).some(t => t.id === team.id && t.role === 'lead')
}

/**
 * The sentence a viewer who may not manage the inbox sees instead of controls.
 *
 * Why the viewer specifically is refused is not stated: a lead learns they are
 * not a lead of *this* team, and an org member learns they are not an admin of
 * *this* org, from the same line, without the panel having to guess which fact
 * applies. What the line does say is who can, which is the part they need.
 */
export function cannotManageReason(me: Me | null | undefined): string {
  if (!me) return `${MANAGERS_ONLY} Sign in to see who can send requests to this inbox.`
  return `${MANAGERS_ONLY} Ask a team lead to make you a lead of this team, or an org owner to make you an admin of its org.`
}

/** The default credential lifetime, in days: the same 90 days the access page offers. */
export const SUBMIT_TOKEN_DAYS = 90
