/**
 * One team request, as its inbox triages it (AMP #3984; build slice J2's page
 * half; approved mockup artifact 37 v1, "Request detail").
 *
 * This page is the composition point, and it is deliberately thin: every panel
 * it draws was built and argued by its own ticket (#3979's discussion, history
 * and attachments; #3980's triage, planning and deliver rails), and every one of
 * them takes its data as props and performs no write on mount. What is left —
 * and what this file actually owns — is four things:
 *
 *  1. **The read, and which answer it is.** `GET /api/teams/:id/requests/:request`
 *     answers *two* shapes. A member gets `{view:'team', request, outcome}`; the
 *     request's own requester, who is not on the inbox, gets
 *     `{view:'requester', projection}` — a whitelist with no record, no claim,
 *     no assignee and no revision. The page branches on the server's own `view`
 *     word (`resolveDetailRead`) and renders a requester notice for the second
 *     arm rather than a triage screen built from a projection. That is the
 *     ticket's "no private-route/auth bypass" criterion at the point where it
 *     can actually be violated: a page that coerced the projection into a
 *     `TeamRequest`, or fell back to an empty one, would show internal fields to
 *     a customer.
 *  2. **The re-read, and the draft that survives it.** Every write answers the
 *     act's facts, never the record (no claim, no assignee), so the page re-reads
 *     after each one. The re-read never unmounts the body: `loading` is the first
 *     read and `refreshing` a background one, and no panel is keyed by `rev`. So
 *     a 409 → banner → Refresh leaves the rail's half-typed question and the
 *     deliver panel's half-typed brief exactly where they were.
 *  3. **Who may act, and what the page says instead.** `inboxStanding` resolves
 *     the caller's standing from the team's member rows and the org role the
 *     identity carries (the server's `RequireTeamManager` passes for an org
 *     owner/admin with no team row at all). A non-member gets no act and one
 *     sentence saying why.
 *  4. **The one lifecycle act nobody else owns.** The rails take claim, release,
 *     assign and the three closing acts; the planning rail says "reopen it first".
 *     Reopen is the page's, because it changes what the page is showing and its
 *     destination is fixed by the lifecycle (rejected/duplicate → triaging).
 *
 * # The record's three sub-reads, and what a failed one says
 *
 * Beside the record the page reads its discussion (`GET …/comments`), its
 * history (`GET …/events`) and its lineage (`GET …/links`), all three in the
 * same load path: the same `loading`/`refreshing` split, the same `withTimeout`,
 * the same sequence guard. The panels take the rows as props and never fetch —
 * opening a request must not mount anything write-capable.
 *
 * A failed sub-read is held **beside** its rows, never in place of them
 * (`SubRead` in `lib/requestDetail.ts`). That is the whole rule: a thread the
 * page never read must not render as an empty thread, because "No discussion
 * yet" is a claim about the record. So a panel with no rows and a failure
 * states the failure, and a panel with rows and a failed re-read keeps the rows
 * it has and says the re-read failed — the same choice the record's own soft
 * re-read makes, and the reason a half-typed comment survives both.
 *
 * Two things are reconstructed rather than left blank, because the aggregate
 * genuinely carries them: the active route row (`outcome.routed_project_id`,
 * titled by one project read — the raw rows can hold a superseded route) and the
 * delivery's own counts (`outcome.total`/`done`/…). The aggregate's counts are
 * the server's own, so the page never counts links it cannot see.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ArrowLeft, Loader2, RotateCw } from 'lucide-react'
import { api, requestApi, type HttpError } from '../api/client'
import type {
  Member, Project, RequestComment, RequestLifecycleAnswer, RequestOutcome, RequestReadAnswer,
  Team, TeamRequest,
} from '../types'
import { RequestAttachmentsPanel } from '../components/requests/RequestAttachmentsPanel'
import { RequestDeliverPanel } from '../components/requests/RequestDeliverPanel'
import { RequestDiscussion } from '../components/requests/RequestDiscussion'
import { RequestHistory } from '../components/requests/RequestHistory'
import { RequestPanelSection } from '../components/requests/RequestPanelSection'
import { RequestPlanningRail } from '../components/requests/RequestPlanningRail'
import { RequestTriageRail } from '../components/requests/RequestTriageRail'
import { PageHeader } from '../components/ui/PageHeader'
import { Markdown } from '../components/ui/Markdown'
import { Skeleton, SkeletonRegion, SkeletonText } from '../components/ui/Skeleton'
import { useDelayedFlag } from '../hooks/useDelayedFlag'
import { ago } from '../lib/approval'
import { lookupPerson, type Person } from '../lib/people'
import { projectLink } from '../lib/projectLink'
import { ticketPath } from '../lib/ticketRef'
import { withTimeout } from '../lib/timeout'
import {
  CLAIM_NEVER_EXPIRES, REQUEST_STATE_HINT, requestRef as formatRequestRef,
  requestRequester, requestSourceLabel, requestStateLabel, requestStateVar, teamPeople,
} from '../lib/requestState'
import {
  NO_SUB_READS, REQUESTER_VIEW_NOTICE, briefMutability, collectForwardPages, detailPanels, duplicateFacts,
  evidenceLinkRows, inboxStanding, namedLinkRows, notAMemberReason, planningLinkRows, rejectionFacts, reopenCopy,
  reopenOffer, resolveDetailRead, subReadFailureNotice, subReadTruncationNotice, teamThreadPage, withRouteTitle,
  type DetailRead, type InboxStanding, type SubReadName, type SubReads,
} from '../lib/requestDetail'
import type { ResolvedRequestLink } from '../lib/requestControls'

/**
 * What a read failure looks like, split the way the mockup draws it.
 *
 * The two are one status on the wire (a 404 covers "not yours", "another team"
 * and "no such request" alike, by design), so the page separates only what it can
 * genuinely distinguish: a refused read, and a read that did not answer.
 */
type LoadFailure =
  | { kind: 'unavailable'; message: string }
  | { kind: 'not_found'; message: string }

/** The page's own refusal, in the two sentences a reader needs: what failed, and what it means. */
interface Failure { title: string; body: string }

interface DetailData {
  /** The resolved answer: a team record, a requester projection, or a view this build does not know. */
  read: DetailRead | null
  /** First load: nothing is on screen yet. */
  loading: boolean
  /** A background re-read while the body stays mounted. Drafts survive this. */
  refreshing: boolean
  failure: LoadFailure | null
}

const EMPTY: DetailData = { read: null, loading: true, refreshing: false, failure: null }

/** The attachments panel's own read, kept apart from the record's so one failing does not take the other down. */
interface AttachmentData {
  panel: import('../types').RequestAttachmentPanel | null
  loading: boolean
  error: string | null
}

export interface RequestDetailProps {
  /** The inbox the request lives in. Every team route is team-scoped. */
  teamId: number
  /**
   * Which request to open: its serial, or its `REQ-` public reference. The read
   * route accepts either (a requester only ever holds the reference).
   */
  requestRef: number | string
  /** The caller's user id. Decides "you" in claims, who may release, and whose requester arm this is. */
  meId?: number
  /** The caller's role in the team's org, when the identity carries one — the server's manager check reads it. */
  orgRole?: string
  /** Whether the identity is a global admin (the server admits one as a manager). */
  isAdmin?: boolean
  /** Leave the page (back to the queue). Omitted means no back link, which is right when mounted directly. */
  onBack?: () => void
  /** The clock for ages, so a test can pin it. */
  now?: number
}

/**
 * The one team request detail page.
 *
 * `teamId`/`requestRef` are props rather than route params: the routing ticket
 * (#3988) registers the route and reads them, and this ticket deliberately does
 * not touch `App.tsx` (the same arrangement #3978 used for the queue).
 */
export function RequestDetail(props: RequestDetailProps) {
  const { teamId, requestRef, meId, orgRole, isAdmin, onBack, now } = props

  const [data, setData] = useState<DetailData>(EMPTY)
  const [sub, setSub] = useState<SubReads>(NO_SUB_READS)
  const [team, setTeam] = useState<Team | null>(null)
  const [members, setMembers] = useState<Member[]>([])
  const [routeTitle, setRouteTitle] = useState<{ id: number; name: string } | null>(null)
  const [attachments, setAttachments] = useState<AttachmentData>({ panel: null, loading: true, error: null })
  const [attempt, setAttempt] = useState(0)
  const [notice, setNotice] = useState<string | null>(null)
  const [failure, setFailure] = useState<Failure | null>(null)
  const [reopening, setReopening] = useState(false)

  // Every read bumps this; a late answer from an older read is dropped, so a
  // Refresh during a slow re-read cannot overwrite the newer result.
  const loadSeq = useRef(0)

  const request = data.read?.kind === 'team' ? data.read.request : null
  const outcome = data.read?.kind === 'team' ? data.read.outcome : null
  const requestId = request?.id ?? null

  // ---- The record's read, and the three sub-reads beside it ----
  //
  // `soft`: a re-read after a write. The body keeps the previous answer on
  // screen while it runs, which is what makes a draft survive — the panels never
  // unmount, so a composer's text, a triage question and a deliver brief are all
  // still there when the answer lands. A failure leaves the last good record
  // visible with the refusal beside it rather than replacing it with an error
  // page.
  //
  // The discussion, history and lineage reads are part of this same load path —
  // same sequence guard, same `withTimeout`, same per-read failure handling —
  // and they run *after* the record answers, because which arm it answered
  // decides whether they may run at all: a requester's page must not ask for its
  // own history or lineage (the server refuses both with the surface's one 404,
  // by design — the projection's milestones are its summary of the same facts),
  // and its `comments` answer is a different row type this screen does not draw.
  const loadSubReads = useCallback((seq: number) => {
    const fail = (name: SubReadName, err: unknown) => {
      const e = err as HttpError | undefined
      if (seq !== loadSeq.current) return
      setSub(s => ({
        ...s,
        [name]: {
          // The rows are kept: a failed *re*-read leaves the last successful
          // answer on screen, and a panel with no rows says so instead of
          // rendering an empty list.
          rows: s[name].rows,
          failure: e?.message?.trim() || 'The read did not answer.',
          truncated: s[name].truncated,
        },
      }))
    }
    const succeed = <T,>(name: SubReadName, rows: T, truncated = false) => {
      if (seq !== loadSeq.current) return
      setSub(s => ({ ...s, [name]: { rows, failure: null, truncated } }))
    }

    // Each read is independent: one failing must not take the other two down,
    // nor the record they sit beside.

    // The thread and the history are forward-cursored, so both follow the cursor
    // up to the same bound — the newest rows are at the *end* of an append-only
    // read, and showing only the server's first page would silently drop them.
    withTimeout(
      collectForwardPages(async after => {
        const answer = await requestApi.listComments(teamId, requestRef, after == null ? undefined : { after_id: after })
        const page = teamThreadPage(answer)
        // Not the team arm: a shape this page does not render. Thrown so it is
        // stated as the failure it is, rather than drawn as an empty thread.
        if (!page) throw new Error('The discussion answered a shape this page does not render.')
        return { rows: page.rows, nextAfterId: page.nextAfterId }
      }),
      'Reading the discussion',
    )
      .then(({ rows, truncated }) => succeed('discussion', rows, truncated))
      .catch((err: unknown) => fail('discussion', err))

    withTimeout(
      collectForwardPages(async after => {
        const answer = await requestApi.listEvents(teamId, requestRef, after == null ? undefined : { after_id: after })
        return { rows: answer.events ?? [], nextAfterId: answer.next_after_id ?? null }
      }),
      'Reading the history',
    )
      .then(({ rows, truncated }) => succeed('history', rows, truncated))
      .catch((err: unknown) => fail('history', err))

    // The lineage has no cursor: it is bounded by what planning and delivery
    // attached, so one answer is the whole list.
    withTimeout(requestApi.listLinks(teamId, requestRef), 'Reading the linked work')
      .then(rows => succeed('links', rows))
      .catch((err: unknown) => fail('links', err))
  }, [teamId, requestRef])

  const load = useCallback((opts: { soft?: boolean } = {}) => {
    const seq = ++loadSeq.current
    setData(d => ({ ...d, loading: !opts.soft && d.read == null, refreshing: opts.soft === true }))
    setFailure(null)
    // A first read starts the three panels from nothing. A soft re-read keeps
    // what is on screen (and any failure already stated) until the answers land.
    if (!opts.soft) setSub(NO_SUB_READS)
    withTimeout(requestApi.detail(teamId, requestRef), 'Opening the request')
      .then((answer: RequestReadAnswer) => {
        if (seq !== loadSeq.current) return
        const read = resolveDetailRead(answer)
        setData({ read, loading: false, refreshing: false, failure: null })
        // Only the triage arm has these three reads. A requester's answer is a
        // whitelist with no record in it, and its thread/history/lineage are
        // either a different shape or refused outright.
        if (read?.kind === 'team') loadSubReads(seq)
      })
      .catch((err: unknown) => {
        if (seq !== loadSeq.current) return
        const e = err as HttpError | undefined
        // One status for "this request is not yours" and "there is no such
        // request", on purpose (B4): the page must not tell the two apart,
        // because telling them apart is how a stranger enumerates requests.
        const kind: LoadFailure['kind'] = e?.status === 404 ? 'not_found' : 'unavailable'
        setData(d => ({
          ...d,
          loading: false,
          refreshing: false,
          failure: { kind, message: e?.message?.trim() || 'The request did not load.' },
        }))
      })
  }, [teamId, requestRef, loadSubReads])

  useEffect(() => {
    // Deferred past the effect body: the load sets state as it starts, and a
    // synchronous setState in an effect body cascades a render for no reason.
    // (The queue page does the same.)
    const t = setTimeout(() => load(), 0)
    return () => clearTimeout(t)
  }, [load, attempt])

  // ---- The team read: names for claim holders and assignees, and the inbox's own name ----
  //
  // Best-effort in one direction only: a failed team read must not take the
  // request down, and an absent name falls back to "User #id" rather than a
  // blank.
  useEffect(() => {
    let live = true
    withTimeout(api.getTeam(teamId), 'Loading the team')
      .then(r => { if (live) { setTeam(r.team); setMembers(r.members) } })
      .catch(() => { /* the request's own failure is the one worth showing */ })
    return () => { live = false }
  }, [teamId])

  const people = useMemo(() => teamPeople(members), [members])
  const standing = useMemo(
    () => inboxStanding(meId, members, { orgRole, admin: isAdmin }),
    [meId, members, orgRole, isAdmin],
  )

  // ---- The routed project's name ----
  //
  // The aggregate names the route by id and nothing else, so the rail would show
  // "Project #12 — Project #12". One read of the project fixes the name; a
  // failed read leaves the id-bearing title, because the route is a fact either
  // way and only its name is missing. The answer is held *with* its id, so a
  // re-route to another project cannot show the previous project's name.
  const routedProjectId = outcome?.routed_project_id ?? 0
  useEffect(() => {
    if (!routedProjectId) return
    let live = true
    api.getProject(routedProjectId)
      .then((p: Project) => { if (live) setRouteTitle({ id: routedProjectId, name: p.name }) })
      .catch(() => { /* the id stays visible; the name is best-effort */ })
    return () => { live = false }
  }, [routedProjectId])

  // ---- The attachments panel's read ----
  //
  // Its own route (`GET …/attachments`) and its own failure line: a broken
  // attachment read must not hide the request, and vice versa. Only a team
  // answer loads it — a requester's arm has no team-scoped attachment panel in
  // this build (J6 owns the requester's outputs list).
  const loadAttachments = useCallback(() => {
    if (requestId == null) return
    setAttachments(a => ({ panel: a.panel, loading: a.panel == null, error: null }))
    withTimeout(requestApi.attachments(teamId, requestId), 'Loading the attachments')
      .then(panel => setAttachments({ panel, loading: false, error: null }))
      .catch((err: unknown) => {
        const e = err as HttpError | undefined
        setAttachments(a => ({
          panel: a.panel,
          loading: false,
          error: e?.message?.trim() || 'The files did not load.',
        }))
      })
  }, [teamId, requestId])

  useEffect(() => {
    if (requestId == null) return
    // Deferred for the same reason the record's read is: the load sets state as
    // it starts, and doing that synchronously in an effect body cascades a render.
    const t = setTimeout(() => loadAttachments(), 0)
    return () => clearTimeout(t)
  }, [loadAttachments, requestId])

  // ---- Writes ----
  //
  // Every one answers "the act's facts, never the record", so each re-reads. The
  // re-read is soft, which is what keeps a draft in the composer, the triage
  // question or the deliver brief alive across the act that caused it.

  const afterWrite = useCallback((answer: { changed?: boolean }, changedMsg: string | null) => {
    setNotice(changedMsg)
    if (answer.changed === false) return
    load({ soft: true })
  }, [load])

  const reopen = useCallback(() => {
    if (!request) return
    setReopening(true)
    setFailure(null)
    setNotice(null)
    requestApi.reopen(teamId, request.id, { expected_rev: request.rev })
      .then((answer: RequestLifecycleAnswer) => afterWrite(answer, 'Reopened: the request is back in Triaging.'))
      .catch((err: unknown) => setFailure(failureOf(err, request.rev, people)))
      .finally(() => setReopening(false))
  }, [teamId, request, afterWrite, people])

  // ---- Render ----

  const showSkeleton = useDelayedFlag(data.loading)
  const panels = request ? detailPanels(request.state) : null
  // The rail's rows: the read's own lineage with the aggregate's active route
  // in front of it (see `planningLinkRows`), titled by the one project read.
  // Each row is named by id where the server resolved no target, so a live
  // ticket is never drawn as "no longer available" (see `nameTargetById`).
  const linkRows = namedLinkRows(sub.links.rows ?? [])
  const titledLinks = withRouteTitle(
    request && outcome ? planningLinkRows(linkRows, request, outcome) : [],
    routeTitle && routeTitle.id === routedProjectId ? routeTitle.name : null,
  )
  const evidence = evidenceLinkRows(linkRows)

  const backLink = onBack ? (
    <button type="button" className="amp-btn amp-btn-sm amp-btn-ghost" onClick={onBack}>
      <ArrowLeft size={11} aria-hidden="true" />Requests
    </button>
  ) : null

  // The requester's arm: a customer reading their own request. It is not the
  // triage page, and it says so instead of rendering a half-empty one.
  if (data.read?.kind === 'requester') {
    return (
      <div className="flex flex-col h-full min-h-0" style={{ background: 'var(--bg)' }}>
        <PageHeader
          crumb={team ? `Teams / ${team.name}` : 'Requests'}
          title={formatRequestRef(data.read.publicRef)}
          meta="Your own request, as you sent it."
          actions={backLink}
        />
        <div className="flex-1 min-h-0 overflow-y-auto px-5 py-5">
          <div className="amp-panel px-6 py-6 flex flex-col items-start gap-3 max-w-[72ch]">
            <span className="amp-label">Not the team’s view</span>
            <h2 className="m-0 text-[17px] font-semibold text-ink leading-snug">{REQUESTER_VIEW_NOTICE}</h2>
            <p className="m-0 text-[13px] text-ink-2 leading-relaxed">
              This request is still <b className="text-ink">{requestStateLabel(data.read.state)}</b>. Its tracking page —
              the milestones, the shared thread and any files the team sent back — is its own screen, not this one.
            </p>
          </div>
        </div>
      </div>
    )
  }

  if (data.read?.kind === 'other') {
    return (
      <div className="flex flex-col h-full min-h-0" style={{ background: 'var(--bg)' }}>
        <PageHeader crumb="Requests" title="This request can’t be shown here" actions={backLink} />
        <div className="flex-1 min-h-0 overflow-y-auto px-5 py-5">
          <div className="amp-panel px-6 py-6 flex flex-col items-start gap-3 max-w-[72ch]">
            <span className="amp-label" style={{ color: 'var(--danger-ink)' }}>Not this screen</span>
            <p className="m-0 text-[13px] text-ink-2 leading-relaxed">
              The server answered this request as “{data.read.view || 'an unknown view'}”, which this screen does not
              render. It is not being drawn as a team record on purpose: a view this build does not know must not
              inherit the triage screen.
            </p>
            <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" onClick={() => setAttempt(n => n + 1)}>
              <RotateCw size={11} aria-hidden="true" />Try again
            </button>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full min-h-0" style={{ background: 'var(--bg)' }}>
      <PageHeader
        crumb={team ? `Teams / ${team.name} / Requests` : 'Requests'}
        title={request ? request.subject : 'Request'}
        meta={request ? (
          <span className="flex items-center gap-2.5 flex-wrap text-[12px]">
            <span className="amp-num text-ink-3">{formatRequestRef(request.public_ref)}</span>
            <span className="text-ink-3" aria-hidden="true">·</span>
            <span className="amp-num text-ink-3">rev {request.rev}</span>
          </span>
        ) : undefined}
        actions={
          <>
            {backLink}
            {request && <StateTag state={request.state} />}
          </>
        }
      />

      <div className="flex-1 min-h-0 overflow-y-auto px-5 py-5">
        <div className="flex flex-col gap-4 max-w-[1400px]">

          {/* First load: content-shaped, after the delay. Never a bare spinner. */}
          {data.loading && showSkeleton && (
            <SkeletonRegion label="Loading the request">
              <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1.7fr)_minmax(0,1fr)] gap-4 items-start">
                <div className="amp-panel px-4 py-4 grid gap-2.5">
                  <Skeleton w="55%" h={14} />
                  <SkeletonText lines={3} />
                </div>
                <div className="amp-panel px-4 py-4 grid gap-2.5">
                  <Skeleton w="40%" h={12} />
                  <SkeletonText lines={2} />
                </div>
              </div>
            </SkeletonRegion>
          )}

          {/* The read failed. Two failures, two sentences — and the 404 one must
              not distinguish "not yours" from "does not exist". */}
          {data.failure && !data.read && (
            <div className="amp-panel px-7 py-7 flex flex-col items-start gap-2.5 max-w-[68ch]">
              <span className="amp-label" style={{ color: 'var(--danger-ink)' }}>
                {data.failure.kind === 'not_found' ? 'Couldn’t open this request' : 'Couldn’t load the request'}
              </span>
              <h2 className="m-0 text-[16px] font-semibold text-ink">
                {data.failure.kind === 'not_found' ? 'This request didn’t open.' : 'The request didn’t come back.'}
              </h2>
              <p className="m-0 text-[13px] text-ink-2 leading-relaxed">
                {data.failure.kind === 'not_found'
                  ? 'If you followed a link from an email, it may be for a team you’re not on — requests outside your '
                    + 'teams answer not-found on purpose, never permission-denied, so a link cannot be used to discover '
                    + 'what exists. Nothing was changed.'
                  : 'Nothing was changed. This is usually the connection rather than the data — try again, and if it '
                    + 'keeps happening tell your team lead the message below.'}
              </p>
              <p className="m-0 font-mono text-[11px] text-ink-3 break-all">{data.failure.message}</p>
              <button type="button" className="amp-btn amp-btn-sm amp-btn-primary mt-1"
                onClick={() => { setData(EMPTY); setAttempt(n => n + 1) }}>
                <RotateCw size={11} aria-hidden="true" />Retry
              </button>
            </div>
          )}

          {/* The body. `request` and `outcome` are both present exactly when the
              read answered the team arm. */}
          {request && outcome && panels && (
            <>
              {/* A refusal the page itself made (reopen), stated where the act was. */}
              {failure && (
                <div role="alert" className="flex items-start gap-3 px-4 py-3 border text-[13px]"
                  style={{ borderColor: 'var(--danger)', background: 'color-mix(in srgb, var(--danger) 7%, var(--surface))' }}>
                  <div className="min-w-0">
                    <span className="font-semibold text-ink block">{failure.title}</span>
                    <span className="text-ink-2">{failure.body}</span>
                  </div>
                </div>
              )}

              {/* A background re-read: the body stays, one quiet line says it is
                  being re-read. Deliberately not a skeleton over the panels —
                  that is what would throw a half-typed draft away. */}
              {data.refreshing && (
                <p className="m-0 flex items-center gap-2 text-[12px] text-ink-3" role="status">
                  <Loader2 size={11} className="animate-spin" aria-hidden="true" />Re-reading the request…
                </p>
              )}
              {notice && <p className="m-0 text-[12px] text-ink-2" role="status">{notice}</p>}

              <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1.7fr)_minmax(0,1fr)] gap-4 items-start min-w-0">
                {/* ---- Left: the request itself, its cards, its files and thread ---- */}
                <div className="flex flex-col gap-4 min-w-0">
                  <RequestBlock request={request} teamName={team?.name ?? null} people={people} />

                  {panels.clarification && (
                    <ClarificationCard
                      request={request}
                      question={(sub.discussion.rows ?? []).filter(c => c.kind === 'question').slice(-1)[0]}
                      readFailed={sub.discussion.failure != null}
                    />
                  )}

                  {panels.rejection && <RejectionCard request={request} />}

                  {panels.duplicate && <DuplicateCard request={request} />}

                  {panels.delivery && <DeliveryCard request={request} outcome={outcome} evidence={evidence} />}

                  <RequestAttachmentsPanel
                    teamId={teamId}
                    /* The read's own `request_id` guards a stale panel: opening
                       another request must never list this one's files. */
                    panel={attachments.panel?.request_id === requestId ? attachments.panel : null}
                    error={attachments.error}
                    loading={attachments.loading}
                    onRetry={loadAttachments}
                    onChanged={loadAttachments}
                    viewerUserId={meId}
                    canManage={standing.member}
                    isTeamLead={standing.manager}
                  />

                  {/* The internal thread. Its rows or its failure, never an empty
                      thread standing in for a read that did not answer. */}
                  {sub.discussion.rows ? (
                    <>
                      {sub.discussion.failure && (
                        <SubReadFailure what="discussion" overRowsAlreadyShown />
                      )}
                      <RequestDiscussion
                        mode="team"
                        comments={sub.discussion.rows}
                        viewerUserId={meId}
                        tenant={standing.member ? { teamId, requestId: request.id } : undefined}
                        canComment={standing.member}
                        onPosted={() => load({ soft: true })}
                      />
                      {sub.discussion.truncated && (
                        <p className="m-0 px-1 text-[11.5px] text-ink-3 leading-snug" data-testid="subread-truncated-discussion">
                          {subReadTruncationNotice('discussion')}
                        </p>
                      )}
                    </>
                  ) : sub.discussion.failure ? (
                    <RequestPanelSection label="Discussion" note="Internal comments stay internal">
                      <SubReadFailure what="discussion" overRowsAlreadyShown={false} />
                    </RequestPanelSection>
                  ) : null}
                </div>

                {/* ---- Right: triage, reopen, planning, deliver, history ---- */}
                <div className="flex flex-col gap-4 min-w-0">
                  <RequestTriageRail
                    request={request}
                    meId={meId}
                    members={members}
                    canAssign={standing.manager}
                    onChanged={answer => afterWrite(answer, null)}
                    onTransition={panels.triageActs && standing.member ? {
                      ask: (question: string) =>
                        requestApi.needsInfo(teamId, request.id, question, request.rev)
                          .then((a: RequestLifecycleAnswer) => { load({ soft: true }); return a }),
                      reject: (reason: string) =>
                        requestApi.reject(teamId, request.id, reason, request.rev)
                          .then((a: RequestLifecycleAnswer) => { load({ soft: true }); return a }),
                      duplicate: (canonicalRequestId: number) =>
                        requestApi.duplicate(teamId, request.id, canonicalRequestId, request.rev)
                          .then((a: RequestLifecycleAnswer) => { load({ soft: true }); return a }),
                    } : undefined}
                    onRefresh={() => load({ soft: true })}
                    now={now}
                  />

                  {/* Reopen: the page's own act, offered exactly where the graph
                      has a team edge and the caller may take it. */}
                  <ReopenBlock request={request} standing={standing} busy={reopening} onReopen={reopen} />

                  {panels.planning && (
                    <RequestPlanningRail
                      request={request}
                      links={titledLinks}
                      outcome={outcome}
                      canPlan={standing.member}
                      hrefFor={planningHref}
                      now={now}
                    />
                  )}
                  {/* The lineage read's failure, stated where its rows would be.
                      Without it the rail's "not routed" and its counts would be
                      claims about a list the page never received — and the
                      aggregate's route is still drawn, because that fact does
                      not depend on this read. */}
                  {panels.planning && sub.links.failure && (
                    <SubReadFailure what="links" overRowsAlreadyShown={sub.links.rows != null} />
                  )}

                  {panels.deliverPanel && (
                    <RequestDeliverPanel
                      request={request}
                      outcome={outcome}
                      evidence={evidence}
                      canDeliver={standing.member}
                      onDeliver={standing.member
                        ? (input: { outcome_brief: string; no_artifact: boolean }) =>
                            requestApi.deliver(teamId, request.id, { ...input, expected_rev: request.rev })
                              .then((a: RequestLifecycleAnswer) => { load({ soft: true }); return a })
                        : undefined}
                      hrefFor={planningHref}
                      requesterLabel={requestRequester(request)}
                    />
                  )}

                  {panels.history && sub.history.rows && (
                    <>
                      {sub.history.failure && <SubReadFailure what="history" overRowsAlreadyShown />}
                      <RequestHistory mode="team" events={sub.history.rows} />
                      {sub.history.truncated && (
                        <p className="m-0 -mt-2 px-1 text-[11.5px] text-ink-3 leading-snug" data-testid="subread-truncated-history">
                          {subReadTruncationNotice('history')}
                        </p>
                      )}
                    </>
                  )}
                  {panels.history && !sub.history.rows && sub.history.failure && (
                    <RequestPanelSection label="History" note="Append-only: one row per act">
                      <SubReadFailure what="history" overRowsAlreadyShown={false} />
                    </RequestPanelSection>
                  )}
                </div>
              </div>

              <p className="m-0 text-[11.5px] text-ink-3 leading-snug">{CLAIM_NEVER_EXPIRES}.</p>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

// ---- Sub-views ----
//
// Kept in this file because none of them is reusable: each states one fact about
// *this* page's record, and moving them out would invite a second caller to draw
// a rejection card over an open request.

/**
 * One sub-read's failure, stated in its own panel.
 *
 * The page already has a voice for a failed read ("Couldn't load the request",
 * the sentence about what was and was not changed, the quiet mono line with the
 * server's own message) and this says the same three things about one panel: a
 * failed read is never drawn as an empty list, because "No discussion yet" is a
 * claim about the record this page has no basis for. `role="alert"` because it
 * is news the reader did not ask for and the rest of the panel will not say it.
 */
function SubReadFailure({ what, overRowsAlreadyShown }: { what: SubReadName; overRowsAlreadyShown: boolean }) {
  return (
    <p role="alert" className="m-0 text-[12.5px] leading-snug text-danger-ink border border-danger px-2.5 py-2"
      style={{ borderRadius: 'var(--radius)', background: 'color-mix(in srgb, var(--danger) 6%, var(--surface))' }}
      data-testid={`subread-failure-${what}`}>
      {subReadFailureNotice(what, overRowsAlreadyShown)}
    </p>
  )
}

/**
 * Read a write's refusal as this page can state it.
 *
 * The page's own act is Reopen, and a reopen 409 is never a claim race: nobody
 * is competing for a claim when a closed request is reopened. So this does not
 * route through `claimConflictNotice` — that helper turns *any* 409 (including
 * a bare stale revision) into "…claimed this request first", which over a reopen
 * would name a winner who does not exist. The distinction it *does* make is the
 * one the server's body makes: a 409 carrying `claim_user_id` names a holder, and
 * one that carries only `rev` means this view is stale.
 */
function failureOf(err: unknown, fromRev: number, people: Map<number, Person>): Failure {
  const e = err as HttpError | undefined
  const message = e?.message?.trim() || 'The request could not be updated.'
  if (e?.status === 409) {
    // A holder named on a reopen would be a fact the server did not send; when
    // it is sent (a lost claim race, which the rail owns), it is still honest
    // here because the id is real.
    const holder = e.claimUserId != null ? lookupPerson(people, e.claimUserId)?.name : null
    const at = e.rev != null ? ` It is at rev ${e.rev} now, and your view was from rev ${fromRev}.` : ''
    return {
      title: holder ? `${holder} is holding this request.` : 'This request changed since your view loaded.',
      body: `${message}${at} Nothing was changed — re-read it and act again.`,
    }
  }
  return { title: 'That didn’t work.', body: `${message} Nothing was changed.` }
}

/** The state tag beside the title, with the queue's own colour and hint. */
function StateTag({ state }: { state: TeamRequest['state'] }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-[11.5px] font-medium whitespace-nowrap"
      title={REQUEST_STATE_HINT[state]} data-testid="detail-state-tag">
      <span className="w-2 h-2 flex-shrink-0" style={{ background: requestStateVar(state), borderRadius: 1 }} aria-hidden="true" />
      {requestStateLabel(state)}
    </span>
  )
}

/**
 * The immutable request block: the brief, who sent it and how, and the one
 * sentence about whether it can still be edited.
 *
 * "Immutable" is a fact about triage, not decoration: `UpdateRequest` refuses a
 * submitter's edit once the desk has picked the request up, so an untriaged
 * request says it can still change and a triaged one says why it cannot.
 */
function RequestBlock({ request, teamName, people }: {
  request: TeamRequest
  teamName: string | null
  people: Map<number, Person>
}) {
  const mutability = briefMutability(request)
  const claimer = request.claim_user_id != null ? lookupPerson(people, request.claim_user_id) : null
  return (
    <RequestPanelSection
      label="Request"
      note={
        <span className="flex items-center gap-x-2 gap-y-1 flex-wrap">
          <span className="amp-num text-[11px] text-ink-3">submitted {request.created_at.slice(0, 10)}</span>
          <span className="text-ink-3" aria-hidden="true">·</span>
          <span className="text-ink-3">source {requestSourceLabel(request.source_kind)}</span>
          <span className="text-ink-3" aria-hidden="true">·</span>
          <span className="text-ink-3" title={mutability.note}>{mutability.immutable ? 'immutable' : 'editable'}</span>
        </span>
      }
    >
      <div className="text-ink" data-testid="request-brief">
        <Markdown size="compact">{request.input_brief || '_No brief was written._'}</Markdown>
      </div>

      <dl className="grid grid-cols-[92px_minmax(0,1fr)] sm:grid-cols-[120px_minmax(0,1fr)] gap-x-3 gap-y-1.5 m-0 text-[12.5px]">
        <dt className="text-ink-3">Requester</dt>
        <dd className="m-0 text-ink break-all min-w-0">
          <span className="amp-num">{requestRequester(request)}</span>
          {request.requester_user_id != null && (
            <span className="text-ink-3"> (invited customer{teamName ? ` — ${teamName}` : ''})</span>
          )}
        </dd>

        <dt className="text-ink-3">Source</dt>
        <dd className="m-0 text-ink">
          {requestSourceLabel(request.source_kind)}
          {request.source_external_id && (
            <span className="text-ink-3"> · external id <span className="amp-num">{request.source_external_id}</span></span>
          )}
        </dd>

        {claimer && (
          <>
            <dt className="text-ink-3">Claimed by</dt>
            <dd className="m-0 text-ink">
              {claimer.name}
              {request.claimed_at && <span className="text-ink-3"> · {ago(request.claimed_at)}</span>}
            </dd>
          </>
        )}

        <dt className="text-ink-3">Reopened</dt>
        <dd className="m-0 text-ink">
          {request.reopened_count === 0 ? 'never' : `${request.reopened_count} time${request.reopened_count === 1 ? '' : 's'}`}
        </dd>
      </dl>

      <p className="m-0 text-[11.5px] text-ink-3 leading-snug">{mutability.note}</p>
    </RequestPanelSection>
  )
}

/**
 * The open question, and the promise that it obliges an answer (I5).
 *
 * The question's own text is a discussion row, so the card takes the thread's
 * rows as a prop (the page owns every read) and shows the last `question` in
 * them. It never invents one: with no rows it states the two facts the record
 * does carry — that a question is open (the state) and since when
 * (`needs_info_since`) — and says which one it cannot show.
 *
 * A thread that *failed* is stated as a failed read rather than as "not
 * carried": the question may well be there, this page just did not receive the
 * answer.
 */
function ClarificationCard({ request, question, readFailed }: {
  request: TeamRequest
  question: RequestComment | undefined
  readFailed: boolean
}) {
  const since = request.needs_info_since && !request.needs_info_since.startsWith('0001-01-01')
    ? request.needs_info_since
    : null
  return (
    <RequestPanelSection
      label="Clarification"
      note={<span style={{ color: 'var(--danger-ink)' }}>needs the requester’s answer</span>}
    >
      <blockquote className="m-0 pl-3 py-1.5 border-l-[3px] text-[13px] text-ink-2"
        style={{ borderColor: 'var(--st-flag)', background: 'var(--surface-2)' }}>
        <span className="amp-label block mb-1.5" style={{ color: 'var(--danger-ink)' }}>
          Question to requester{since ? ` · sent ${ago(since)}` : ''}
        </span>
        {question ? (
          <Markdown size="compact">{question.body}</Markdown>
        ) : readFailed ? (
          <span data-testid="clarification-read-failed">
            The question is one row of the discussion, and the discussion did not load — so it is not shown rather
            than shown as absent.
          </span>
        ) : (
          <span data-testid="clarification-unrecorded">
            The question is recorded in the discussion, which this read did not return.
          </span>
        )}
      </blockquote>
      <p className="m-0 text-[12.5px] text-ink-2 leading-snug">
        The request stays in <b className="text-ink">Needs information</b> until the requester answers; asking obliges
        an answer (contract I5). There is no SLA clock on the requester in v1.
      </p>
      <p className="m-0 text-[12px] text-ink-3 leading-snug">
        The requester sees the question and a plain-language status — never this page.
      </p>
    </RequestPanelSection>
  )
}

/** The rejection: the reason verbatim (the requester reads it), and where the actor is. */
function RejectionCard({ request }: { request: TeamRequest }) {
  const facts = rejectionFacts(request)
  return (
    <RequestPanelSection
      label="Rejected"
      note={facts.when ? <span className="amp-num text-[11px] text-ink-3">{facts.when.slice(0, 16).replace('T', ' ')}</span> : undefined}
    >
      <blockquote className="m-0 pl-3 py-1.5 border-l-[3px] text-[13px] text-ink-2"
        style={{ borderColor: 'var(--st-flag)', background: 'var(--surface-2)' }}>
        {facts.reason || 'No reason was recorded with this rejection.'}
      </blockquote>
      <p className="m-0 text-[12.5px] text-ink-2 leading-snug">
        A rejection carries a reason and an actor — the requester sees the reason, verbatim. The person who declined is
        in the request’s history, which this build’s read does not carry.
      </p>
    </RequestPanelSection>
  )
}

/** The duplicate pointer: the serial it folds into, without inventing a reference. */
function DuplicateCard({ request }: { request: TeamRequest }) {
  const facts = duplicateFacts(request)
  return (
    <RequestPanelSection label="Duplicate">
      <p className="m-0 text-[13px] text-ink">
        Folded into {facts.canonicalId != null
          ? <>the canonical request <span className="amp-num">#{facts.canonicalId}</span></>
          : 'a canonical request this read does not name'}.
        All follow-up happens on the canonical request; this one stays readable for the requester.
      </p>
      <p className="m-0 text-[11.5px] text-ink-3 leading-snug">
        This read carries the canonical’s serial, not its reference, so no link is offered rather than a link that
        would answer not-found.
      </p>
      <p className="m-0 text-[12.5px] text-ink-2 leading-snug">
        A canonical is never a duplicate. Reopening this one detaches it (contract D7).
      </p>
    </RequestPanelSection>
  )
}

/**
 * The delivery: the team's brief, and what the aggregate says its evidence
 * amounts to.
 *
 * The counts are the aggregate's own, not a page-side count of rows it can see.
 * The names come from the lineage read (`delivered_by` rows), and the aggregate
 * stays the source for each piece's state, so a row can never show "merged" for
 * a piece the aggregate counts as gating the delivery.
 */
function DeliveryCard({ request, outcome, evidence }: {
  request: TeamRequest
  outcome: RequestOutcome
  evidence: readonly ResolvedRequestLink[]
}) {
  const accepted = request.state === 'requester_accepted'
  const brief = request.outcome_brief.trim()
  return (
    <RequestPanelSection
      label="Delivery"
      note={
        <span style={{ color: accepted ? 'var(--ink-2)' : 'var(--success-ink)' }}>
          {accepted ? 'accepted by the requester' : 'delivered · awaiting requester acceptance'}
        </span>
      }
    >
      {brief ? (
        <div className="text-ink" data-testid="outcome-brief">
          <Markdown size="compact">{brief}</Markdown>
        </div>
      ) : (
        <p className="m-0 text-[12.5px] text-ink-2">No outcome brief was recorded with this delivery.</p>
      )}

      <div className="flex items-center gap-x-2 gap-y-1 flex-wrap text-[12px] text-ink-2">
        <span><span className="amp-num text-ink">{outcome.total}</span> delivered item{outcome.total === 1 ? '' : 's'}</span>
        {outcome.done > 0 && <><span className="text-ink-3" aria-hidden="true">·</span><span><span className="amp-num text-ink">{outcome.done}</span> done</span></>}
        {outcome.held > 0 && <><span className="text-ink-3" aria-hidden="true">·</span><span><span className="amp-num text-ink">{outcome.held}</span> pending verify</span></>}
        {outcome.deleted > 0 && <><span className="text-ink-3" aria-hidden="true">·</span><span><span className="amp-num text-ink">{outcome.deleted}</span> deleted</span></>}
        {outcome.no_artifact && <><span className="text-ink-3" aria-hidden="true">·</span><span>stated as nothing to link</span></>}
        {evidence.length > 0 && (
          <><span className="text-ink-3" aria-hidden="true">·</span><span>{evidence.length} named in this read</span></>
        )}
      </div>

      <p className="m-0 text-[12.5px] text-ink-2 leading-snug">
        Acceptance belongs to the requester alone (contract I1) — no one on the team, and no agent, can accept on their
        behalf.
        {accepted && request.accepted_at && <> Accepted {request.accepted_at.slice(0, 10)}.</>}
      </p>
    </RequestPanelSection>
  )
}

/**
 * Reopen, or the sentence that says why it is not offered.
 *
 * It lives outside the triage rail because it is not a triage act: it changes
 * what the whole page is showing (a closed reading back to an open one), and its
 * destination is fixed by the lifecycle rather than chosen. The two states it
 * applies to are the graph's only team reopen edges; delivery's reopen is the
 * requester's alone (I1) and the page says so.
 */
function ReopenBlock({ request, standing, busy, onReopen }: {
  request: TeamRequest
  standing: InboxStanding
  busy: boolean
  onReopen: () => void
}) {
  // Only where the lifecycle has a team reopen edge (rejected/duplicate) or a
  // delivered request (where it must say the act is not the team's). Any other
  // state has nothing to reopen, and the block is not drawn at all.
  const offered = reopenOffer(request, standing)
  const closed = request.state === 'rejected' || request.state === 'duplicate'
  const delivered = request.state === 'delivered' || request.state === 'requester_accepted'
  if (!closed && !delivered) return null
  const copy = reopenCopy(offered)
  return (
    <section className="amp-panel" aria-label="Reopen">
      <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
        <span className="amp-label">Reopen</span>
      </div>
      <div className="px-4 py-3 flex flex-col gap-2">
        {offered.offered ? (
          <>
            <button type="button" className="amp-btn amp-btn-sm self-start" disabled={busy} onClick={onReopen}>
              {busy ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : null}
              {copy.label}
            </button>
            <p className="m-0 text-[11.5px] text-ink-3 leading-snug">{copy.hint}</p>
          </>
        ) : (
          <p className="m-0 text-[12px] text-ink-3 leading-snug" data-testid="reopen-refusal">
            {offered.reason || (standing.member ? '' : notAMemberReason())}
          </p>
        )}
      </div>
    </section>
  )
}

/** Where a planning row's target lives, so a linked item opens instead of only naming itself. */
function planningHref(target: { kind: string; id: number; projectId?: number }): string | undefined {
  switch (target.kind) {
    case 'task':
      return ticketPath(null, target.id)
    case 'artifact':
      return `/artifacts/${target.id}`
    case 'project':
      return projectLink(target.projectId ?? target.id)
    default:
      return undefined
  }
}
