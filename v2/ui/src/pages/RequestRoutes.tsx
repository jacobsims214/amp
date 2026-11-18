/**
 * The route-level wrappers for the request surfaces (AMP #3988; build slice map
 * `discoveries/team-inbox-approved-build-slices.md`, the J1/J2/J4/J5/J6 entries;
 * approved mockups artifact 37 v1 team side and 38 v1 requester side).
 *
 * Every page this ticket mounts was built by its own ticket and **owns no
 * routing** — `TeamInbox`, `RequestDetail`, `RequesterSubmit`,
 * `RequesterRequests` and `RequesterStatus` all take their identity, their
 * reference and their links as props, because the ticket that built each one
 * deliberately kept `react-router` out of it (see each page's header). This file
 * is where that promise is kept: it is the only place in the app that reads a
 * route param or a query string for these screens, and the only place that
 * decides where each of their links goes.
 *
 * ## The one decision that matters here: which frame
 *
 * `App.tsx` puts the team routes **inside** `<Route element={<AppShell />}>` and
 * the requester routes **outside** it, at the top level. That is not a layout
 * preference — it is the product's access rule made structural. `AppShell` is
 * the team's frame: the project tree, org admin, day reports, trace, agents and
 * tokens, every internal name in the product. A requester is a customer, may
 * belong to no org at all, and must never see any of it.
 *
 * Mounting the requester screens outside the shell means the guarantee does not
 * depend on this file remembering to hide anything: there is no sidebar in the
 * tree they render in. `components/requests/RequesterChrome` supplies their own
 * frame. A test asserts the absence directly (no `Main` nav landmark, no project
 * tree, on each requester route) so a later refactor cannot quietly move them in.
 *
 * ## Role: the one rule this file enforces, and the ones it refuses to guess
 *
 * A **customer** identity is sent from the team routes to their own list. That
 * is a fact the server states (`/api/me` answers `kind: "customer"` for an
 * account provisioned by a requester invitation) rather than a guess about
 * membership, and it is the difference between a customer seeing a refusal
 * sentence and a customer seeing the sidebar.
 *
 * Everything else is the **server's** answer on every call, and this file
 * deliberately does not re-derive it. In particular it does not try to decide
 * whether a signed-in person is *staff*: the three ways the server admits
 * somebody to an inbox (a team row, a lead row, an org owner/admin — plus a
 * global admin) are not all answerable from `/api/me` in every deployment, and a
 * client-side guess would produce a false "not yours" for a perfectly entitled
 * person. So an under-entitled caller gets the page and the page renders the
 * API's own refusal — one place decides, and it is the one that matters.
 */
import { useCallback, useMemo } from 'react'
import { Navigate, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { BandLoader } from '../components/brand/BandLoader'
import { TeamInbox } from './TeamInbox'
import { RequestDetail } from './RequestDetail'
import { RequesterSubmit } from './RequesterSubmit'
import { RequesterRequests } from './RequesterRequests'
import { RequesterStatus } from './RequesterStatus'
import type { RequesterIdentity } from '../lib/requesterChrome'
import type { TeamRequest } from '../types'
import {
  DESK_NAME_PARAM, DESK_TEAM_PARAM, isCustomerIdentity, readDesk, requesterListPath, requesterNewRequestPath,
  requesterStatusPath, SIGN_OUT_URL, teamInboxPath, teamOrgRole, teamRequestPath, UNKNOWN_DESK, type RequesterDesk,
} from '../lib/requestRoutes'

/**
 * The identity a requester page gets: what their own frame says, and nothing
 * more. `RequesterChrome` cannot leak a field it was never handed, so the map
 * from `Me` to `RequesterIdentity` is deliberately lossy — no roles, no user id,
 * no orgs, no teams, no capabilities.
 */
function requesterIdentity(me: { name?: string; email?: string }): RequesterIdentity {
  return { name: me.name, email: me.email ?? '' }
}

/** A whole-screen wait for the identity a page cannot render without. */
function Waiting({ label }: { label: string }) {
  return (
    <div className="flex min-h-[60vh] items-center justify-center">
      <BandLoader label={label} />
    </div>
  )
}

// ---- Team side (inside AppShell) ----

/**
 * The shared queue (`/teams/:teamId/requests`), slice J1, with a row opening its
 * request.
 *
 * The destination carries the **serial**, not the public reference: the queue's
 * rows have one, a team *write* needs one (a reference is refused on a team write
 * path — `api/client.ts` states why), and a detail page that can act is the point.
 */
export function TeamInboxRoute() {
  const { teamId: raw } = useParams()
  const teamId = Number(raw)
  const { me, loading } = useAuth()
  const navigate = useNavigate()
  const open = useCallback((req: TeamRequest) => { navigate(teamRequestPath(req.team_id, req.id)) }, [navigate])

  if (loading) return <Waiting label="Loading the inbox" />
  if (isCustomerIdentity(me)) return <Navigate to={requesterListPath()} replace />
  if (!Number.isFinite(teamId) || teamId <= 0) return <Navigate to="/" replace />
  return <TeamInbox teamId={teamId} meId={me?.user_id} onOpenRequest={open} />
}

/** One request in its inbox (`/teams/:teamId/requests/:ref`), slice J2. */
export function TeamRequestRoute() {
  const { teamId: rawTeam, ref } = useParams()
  const teamId = Number(rawTeam)
  const { me, loading, isAdmin } = useAuth()
  const navigate = useNavigate()
  const back = useCallback(() => { navigate(teamInboxPath(teamId)) }, [navigate, teamId])
  // The org role is the team's org's, and it is one of the two principals the
  // server's `RequireTeamManager` accepts — so the page's act gating needs it.
  const orgRole = useMemo(() => teamOrgRole(me, teamId), [me, teamId])

  if (loading) return <Waiting label="Opening the request" />
  if (isCustomerIdentity(me)) return <Navigate to={requesterListPath()} replace />
  if (!Number.isFinite(teamId) || teamId <= 0) return <Navigate to="/" replace />
  if (!ref) return <Navigate to={teamInboxPath(teamId)} replace />
  return (
    <RequestDetail
      teamId={teamId}
      // A team read accepts a serial *or* a public reference, so the segment
      // travels as it arrived: a pasted `REQ-` reference must still resolve.
      requestRef={ref}
      meId={me?.user_id}
      orgRole={orgRole}
      isAdmin={isAdmin}
      onBack={back}
    />
  )
}

// ---- Requester side (outside AppShell) ----

/** The desk a requester URL names. Both parts are optional: a URL is not a promise. */
function useDesk(): RequesterDesk {
  const [params] = useSearchParams()
  const team = params.get(DESK_TEAM_PARAM)
  const name = params.get(DESK_NAME_PARAM)
  return useMemo(() => readDesk(new URLSearchParams({
    ...(team != null ? { [DESK_TEAM_PARAM]: team } : {}),
    ...(name != null ? { [DESK_NAME_PARAM]: name } : {}),
  })), [team, name])
}

/**
 * The requester's tabs, as the pages' own nav slot.
 *
 * Both are built when a desk is known, so the frame's shape is stable; `current`
 * is decided by the **route**, never by the URL, because `/requests/new` and
 * `/requests` are different destinations and a query string must not be able to
 * claim otherwise.
 *
 * With no desk, only "Your requests" is offered. A "New request" tab there would
 * be a control that navigates to a form which redirects straight back here — the
 * one kind of dead end a person cannot see coming, because it looks like the way
 * in. The list is what a URL that names no desk can actually answer.
 */
function requesterNav(desk: RequesterDesk, current: 'new' | 'list', navigate: (to: string) => void) {
  const list = { label: 'Your requests', current: current === 'list', onClick: () => navigate(requesterListPath(desk)) }
  if (desk.teamId == null) return [list]
  return [
    { label: 'New request', current: current === 'new', onClick: () => navigate(requesterNewRequestPath(desk)) },
    list,
  ]
}

/**
 * The shared context of the three requester screens: identity, desk, tabs and
 * sign-out, in one place so the three cannot disagree about any of them.
 */
function useRequesterContext(current: 'new' | 'list') {
  const { me, loading } = useAuth()
  const desk = useDesk()
  const navigate = useNavigate()
  const nav = useMemo(() => requesterNav(desk, current, navigate), [desk, current, navigate])
  const signOut = useCallback(() => { window.location.assign(SIGN_OUT_URL) }, [])
  return { me, loading, desk, nav, signOut }
}

/**
 * The submit form (`/requests/new`), slice J4.
 *
 * A URL that names no `?team=` names no desk, and a form that cannot send must
 * not be drawn as though it could — so it goes to the person's own list, the one
 * requester screen that answers something true without a desk.
 */
export function RequesterNewRequestRoute() {
  const { me, loading, desk, nav, signOut } = useRequesterContext('new')
  const navigate = useNavigate()
  if (loading) return <Waiting label="Loading your requests" />
  if (desk.teamId == null) return <Navigate to={requesterListPath(desk)} replace />
  if (!me) return <Waiting label="Signing you in" />
  return (
    <RequesterSubmit
      teamId={desk.teamId}
      teamName={desk.teamName ?? UNKNOWN_DESK}
      me={requesterIdentity(me)}
      nav={nav}
      onSignOut={signOut}
      onOpenRequest={ref => navigate(requesterStatusPath(ref, desk))}
      onViewRequests={() => navigate(requesterListPath(desk))}
    />
  )
}

/** The person's own requests (`/requests`), slice J5. */
export function RequesterRequestsRoute() {
  const { me, loading, desk, nav, signOut } = useRequesterContext('list')
  const navigate = useNavigate()
  if (loading) return <Waiting label="Loading your requests" />
  if (!me) return <Waiting label="Signing you in" />
  return (
    <RequesterRequests
      teamName={desk.teamName ?? UNKNOWN_DESK}
      me={requesterIdentity(me)}
      nav={nav}
      onSignOut={signOut}
      onOpenRequest={ref => navigate(requesterStatusPath(ref, desk))}
      // No desk, no form: the button is omitted rather than offered and refused.
      onNewRequest={desk.teamId != null ? () => navigate(requesterNewRequestPath(desk)) : undefined}
    />
  )
}

/**
 * One request as its requester tracks it (`/request/:publicRef`), slice J6.
 *
 * No access state is passed. `invite_pending` and `grant_revoked` are states
 * only the *entry link* can know (the server answers one 404 for all three and
 * must never be turned into an oracle — `lib/requesterStatus.ts`), and no link in
 * this build carries one. Deriving them from a 404 here would rebuild exactly the
 * oracle the server refuses to be, so the page reads the request and renders its
 * own not-found arm, which is byte-identical for all three causes.
 */
export function RequesterStatusRoute() {
  const { publicRef } = useParams()
  const { me, loading, desk, nav, signOut } = useRequesterContext('list')
  const navigate = useNavigate()
  if (loading) return <Waiting label="Opening your request" />
  if (!publicRef) return <Navigate to={requesterListPath(desk)} replace />
  if (!me) return <Waiting label="Signing you in" />
  return (
    <RequesterStatus
      publicRef={publicRef}
      me={requesterIdentity(me)}
      teamName={desk.teamName}
      nav={nav}
      onSignOut={signOut}
      onBackToRequests={() => navigate(requesterListPath(desk))}
      onNewRequest={desk.teamId != null ? () => navigate(requesterNewRequestPath(desk)) : undefined}
      // A caller who sent this request *and* works its inbox gets the team
      // answer, not the projection (the server ranks team standing first).
      // The page states that and offers the inbox's own screen; the path is
      // built here because this file is the only one that knows routes.
      onOpenTeamView={({ teamId, requestId }) => navigate(teamRequestPath(teamId, requestId))}
    />
  )
}