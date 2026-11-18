/**
 * The requester's tracking screen (AMP #3987, build slice J6 of
 * `discoveries/team-inbox-approved-build-slices.md`): one request as the person
 * who sent it sees it, and the three things they can do about it.
 *
 * Built to the approved requester mockup (artifact 38 v1, the "Request status"
 * screen in its three states — in progress, needs your reply, delivered with
 * accept-or-reopen — plus the "No access" screen's three states). It composes
 * `RequesterChrome` (#3981), `RequesterAnswerBox` / `RequesterAcceptBar` /
 * `RequesterAccessStates` (this ticket), and the requester arms of
 * `RequestDiscussion` and `RequestHistory` (#3979). It owns no routing: the page
 * takes the reference and its links as props, so #3988 can mount it at whatever
 * URL it wires.
 *
 * ## What the page reads, and what it can never read
 *
 * One call: `GET /api/requests/{public_ref}` (`requestApi.ownDetail`), which the
 * server answers with the requester projection — unless the caller is also
 * somebody who works the inbox the request was sent to, in which case it answers
 * the **team** view (`RequestViewFor` ranks team standing first, by contract; see
 * `## The team arm` below). The type this page renders for a requester is
 * `RequesterRequestView`, which has no field for a project, an epic, a ticket, a
 * claim, an assignee, a revision, a metadata key, an agent or an audit row. So
 * "no internal data exposed" is a property of the data the page was given, not of
 * the page's self-restraint — and the test asserts it against the rendered text
 * anyway.
 *
 * ## The team arm
 *
 * `GET /api/requests/{ref}` loads only the caller's *own* request
 * (`WHERE public_ref = $1 AND requester_user_id = $2`), so a team answer here is
 * always the caller's own request. But when that caller is also a member, lead or
 * org admin of the inbox, the server answers `view: 'team'` with the record —
 * deliberately, because a triager must see the record they triage. That answer is
 * not the projection and must never be rendered as one: the projection's
 * milestones are re-gated server-side (`RequesterMilestoneDoneAllowed`), its
 * comments are the subset the team chose to share, its files are the ones it chose
 * to hand over, and its `state_word` is the server's word. None of that exists in
 * the record, so mapping the record into a projection would *invent* all four.
 *
 * So the page takes four fields of that answer (`team_id`, `id`, `public_ref`,
 * `subject` — the address of a screen the caller can already open) and offers the
 * inbox's own view, saying why. Everything else on the record — claim, assignee,
 * requester contact, revision, metadata — is not read, which is what keeps this
 * page unable to draw a triage screen. Any other `view` this build does not know
 * still fails closed: the guard is narrowed, not removed.
 *
 * ## The three acts, and why each is where it is
 *
 * - **answer** — only in `needs_information`, which is the only state an answer is
 *   legal out of. Its destination is read from the request's history by the
 *   server; the page never chooses a state.
 * - **accept** — only in `delivered`, and only as the requester. This is the one
 *   act no team member, agent, token or connector may perform (I1/D5); the server
 *   refuses a credential before it even resolves the reference, so the button's
 *   presence here is a UI convenience rather than the permission.
 * - **reopen** — in `delivered` and `requester_accepted` (D6). A reopen is a
 *   transition back to `triaging`, not a tenth state.
 *
 * ## The one thing the page will not do: guess why a read failed
 *
 * A 404 is the surface's single not-found — an unknown reference, another
 * person's reference and a request whose requester changed are one answer — so
 * the page renders `RequesterAccessStates`' not-found arm for it and says nothing
 * about which of the three it was. A 5xx or a dropped connection is *not* that
 * state: it renders a load failure with a retry, because telling someone their
 * request does not exist because a query timed out is a lie the server never told.
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { Download, FileText, Lock } from 'lucide-react'
import { requestApi } from '../api/client'
import { goToDownload } from '../lib/download'
import { withTimeout } from '../lib/timeout'
import { ago } from '../lib/approval'
import { requestRef } from '../lib/requestState'
import { requestFileSize } from '../lib/requestLabels'
import { requesterNavItemClass, type RequesterIdentity } from '../lib/requesterChrome'
import {
  REQUESTER_TRACK_NOTE,
  requesterDownloadPath,
  requesterOutputDownloadable,
  requesterOutputWord,
  requesterReadFailure,
  requesterTagTone,
  requesterTrack,
  requesterTrackVar,
  type RequesterTrack,
} from '../lib/requesterStatus'
import { RequesterChrome } from '../components/requests/RequesterChrome'
import { RequesterAnswerBox } from '../components/requests/RequesterAnswerBox'
import { RequesterAcceptBar } from '../components/requests/RequesterAcceptBar'
import { RequesterAccessStates, RequesterLoadFailure } from '../components/requests/RequesterAccessStates'
import { RequestDiscussion } from '../components/requests/RequestDiscussion'
import { RequestHistory } from '../components/requests/RequestHistory'
import { Markdown } from '../components/ui/Markdown'
import type { RequestReadAnswer, RequesterRequestView, TeamRequest } from '../types'

export interface RequesterStatusProps {
  /**
   * The reference the person holds — the token their submission answered with,
   * in the display form (`REQ-…`) or bare. It is the only address a requester
   * has: the projection carries neither a serial nor a team.
   */
  publicRef: string
  /** The signed-in requester, as `/api/me` answers. */
  me: RequesterIdentity
  /**
   * The desk's public display name, when the entry point knows it. Passed in
   * rather than read: a customer has no org standing, and the projection's own
   * `team_name` arrives with the request anyway — this prop is what an access
   * state (which has no request) can show.
   */
  teamName?: string
  /** The requester's own two tabs, as the page's own links (#3988 owns routing). */
  nav?: { label: string; current: boolean; onClick?: () => void }[]
  onSignOut?: () => void
  /** Back to the person's own list. Omitted, no such link renders. */
  onBackToRequests?: () => void
  /** Start a new request. Omitted, no such button renders. */
  onNewRequest?: () => void
  /**
   * The access state, when the entry point knows one. `not_found` is also
   * produced by this page's own read; `invite_pending` and `grant_revoked` can
   * only come from here, because the server answers one not-found for all three
   * and must never be turned into an oracle (see `lib/requesterStatus.ts`).
   */
  accessState?: 'invite_pending' | 'grant_revoked'
  /** Sign in (invite pending). */
  onSignIn?: () => void
  /** Ask the desk to restore access (revoked), or contact them (not found). */
  onContactDesk?: () => void
  /**
   * Open the request in its inbox's own view, when the caller is both its
   * requester and somebody who works that inbox. The page deliberately owns no
   * routing (#3988 does), so the destination travels as a prop like every other
   * link on it; omitted, the arm states why it is not the tracking page and
   * offers no button rather than a control that goes nowhere.
   */
  onOpenTeamView?: (at: { teamId: number; requestId: number }) => void
}

/** The page's one read, as one variable at a time. */
type PageState =
  | { kind: 'loading' }
  | { kind: 'ready'; view: RequesterRequestView }
  | { kind: 'team_standing'; teamId: number; requestId: number; publicRef: string; subject: string }
  | { kind: 'not_found' }
  | { kind: 'failed'; message: string }

/** The one sentence the page says when a read answers a shape it cannot render. */
const UNKNOWN_SHAPE = 'This request answered in a shape this page does not understand.'

/**
 * The most this page takes from a `team` answer: where the request is published,
 * and its own subject/reference. Nothing else on the record is read.
 *
 * It returns `null` for a team answer that does not name a team and a request —
 * which keeps the unknown-shape arm reachable for a payload that is only half a
 * team answer — and for every view this build has never heard of.
 */
function teamStanding(answer: RequestReadAnswer): Extract<PageState, { kind: 'team_standing' }> | null {
  if (!answer || answer.view !== 'team') return null
  const record = (answer as { request?: Partial<Pick<TeamRequest, 'team_id' | 'id' | 'public_ref' | 'subject'>> }).request
  const teamId = record?.team_id
  const requestId = record?.id
  if (typeof teamId !== 'number' || !Number.isInteger(teamId) || teamId <= 0) return null
  if (typeof requestId !== 'number' || !Number.isInteger(requestId) || requestId <= 0) return null
  return {
    kind: 'team_standing',
    teamId,
    requestId,
    publicRef: typeof record?.public_ref === 'string' ? record.public_ref : '',
    subject: typeof record?.subject === 'string' ? record.subject : '',
  }
}

/** The five stepper words, in the mockup's order. The projection's keys are the same five. */
const STEP_WORD: Record<string, string> = {
  received: 'Received',
  reviewing: 'Being reviewed',
  in_progress: 'In progress',
  delivered: 'Delivered',
  accepted: 'You confirm',
}

export function RequesterStatus({
  publicRef, me, teamName, nav, onSignOut, onBackToRequests, onNewRequest,
  accessState, onSignIn, onContactDesk, onOpenTeamView,
}: RequesterStatusProps) {
  const [state, setState] = useState<PageState>({ kind: 'loading' })
  const [attempt, setAttempt] = useState(0)
  // Every load bumps this; a late answer from an older load is ignored, so a
  // retry during a slow read cannot be overwritten by the read it replaced.
  const loadSeq = useRef(0)

  // The read lives in the effect and touches state only from its own promise
  // callbacks: the loading state is the initial state and a retry's, so no render
  // cascades out of the effect body.
  useEffect(() => {
    const seq = ++loadSeq.current
    withTimeout(requestApi.ownDetail(publicRef), 'Loading your request')
      .then(answer => {
        if (seq !== loadSeq.current) return
        if (answer.view === 'requester') {
          setState({ kind: 'ready', view: answer.projection })
          return
        }
        // The team arm: the caller is this request's requester *and* somebody
        // who works its inbox, so the server answered the record it triages
        // (RequestViewFor ranks team standing first). Render the projection
        // from that record? No — the projection is server-built, and a client
        // map would have to invent its milestones, its shared thread and its
        // files. The page offers the view that answer really is, and says why.
        const team = teamStanding(answer)
        if (team) {
          setState(team)
          return
        }
        // Anything else — including a half-formed team answer — is a shape this
        // build cannot render, and it still fails closed.
        setState({ kind: 'failed', message: UNKNOWN_SHAPE })
      })
      .catch(e => {
        if (seq !== loadSeq.current) return
        if (requesterReadFailure(e) === 'not_found') {
          setState({ kind: 'not_found' })
          return
        }
        setState({
          kind: 'failed',
          message: e instanceof Error && e.message
            ? e.message
            : 'The request did not come back. Nothing about it changed.',
        })
      })
    return () => { if (seq === loadSeq.current) loadSeq.current += 1 }
  }, [publicRef, attempt])

  const retry = () => { setState({ kind: 'loading' }); setAttempt(n => n + 1) }

  // An access state the entry point knows wins over the read: there is nothing
  // to read when the person has no access to send at all.
  if (accessState) {
    return (
      <Frame teamName={teamName || 'The desk'} me={me} nav={nav} onSignOut={onSignOut}>
        <RequesterAccessStates
          state={accessState}
          teamName={teamName}
          email={me.email}
          onViewRequests={onBackToRequests}
          onSignIn={onSignIn}
          onContactDesk={onContactDesk}
        />
      </Frame>
    )
  }

  if (state.kind === 'loading') {
    return (
      <Frame teamName={teamName || 'Your request'} me={me} nav={nav} onSignOut={onSignOut}>
        <LoadingRequest />
      </Frame>
    )
  }

  if (state.kind === 'not_found') {
    return (
      <Frame teamName={teamName || 'The desk'} me={me} nav={nav} onSignOut={onSignOut}>
        <RequesterAccessStates
          state="not_found"
          teamName={teamName}
          email={me.email}
          onViewRequests={onBackToRequests}
          onContactDesk={onContactDesk}
        />
      </Frame>
    )
  }

  if (state.kind === 'team_standing') {
    return (
      <Frame teamName={teamName || 'The desk'} me={me} nav={nav} onSignOut={onSignOut}>
        <TeamStanding
          desk={(teamName ?? '').trim() || 'the desk'}
          reference={requestRef(state.publicRef || publicRef)}
          subject={state.subject}
          onOpenTeamView={onOpenTeamView
            ? () => onOpenTeamView({ teamId: state.teamId, requestId: state.requestId })
            : undefined}
          onBackToRequests={onBackToRequests}
        />
      </Frame>
    )
  }

  if (state.kind === 'failed') {
    return (
      <Frame teamName={teamName || 'Your request'} me={me} nav={nav} onSignOut={onSignOut}>
        <RequesterLoadFailure message={state.message} onRetry={retry} />
      </Frame>
    )
  }

  return (
    <Frame teamName={state.view.team_name || teamName || 'The desk'} me={me} nav={nav} onSignOut={onSignOut}>
      <RequestView
        view={state.view}
        publicRef={publicRef}
        onBackToRequests={onBackToRequests}
        onNewRequest={onNewRequest}
        onChanged={retry}
      />
    </Frame>
  )
}

/**
 * The arm for a caller who is both this request's requester and a person who
 * works the inbox it was sent to.
 *
 * It is not an access state and it is not an error: the request is the caller's
 * own, it loaded, and the answer is simply the team's view. So it says that
 * plainly, names where the request can be read in full, and offers that screen.
 * Only the four fields the page kept from the answer are on it — no claim, no
 * assignee, no contact, no revision, no metadata — because the point of routing
 * is that the inbox's own screen owns the record, not that this one renders a
 * narrower copy of it.
 */
function TeamStanding({ desk, reference, subject, onOpenTeamView, onBackToRequests }: {
  desk: string
  reference: string
  subject: string
  onOpenTeamView?: () => void
  onBackToRequests?: () => void
}) {
  return (
    <div
      className="mx-auto mt-8 max-w-[600px] border bg-surface px-6 py-7"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
      data-testid="requester-team-standing"
    >
      <span className="amp-label">Your request, in the inbox</span>
      <h1 className="m-0 mt-1.5 break-words text-[21px] font-semibold tracking-[-0.01em] text-ink">
        {subject || 'Your request'}
      </h1>
      <p className="m-0 mt-1 font-mono text-[11px] text-ink-3">
        {reference} · {desk}
      </p>
      <p className="m-0 mt-3 text-[13px] leading-relaxed text-ink-2">
        You sent this request, and you also have access to {desk}'s inbox — so it opens in the inbox's own view,
        the one the team works from, rather than on this tracking page. This page can only show what a requester
        outside the team sees, and that would leave out what you are entitled to.
      </p>
      <div className="mt-5 flex flex-col gap-2 sm:flex-row sm:flex-wrap">
        {onOpenTeamView && (
          <button type="button" onClick={onOpenTeamView} className="amp-btn amp-btn-primary w-full sm:w-auto" style={{ minHeight: 44 }}>
            Open it in the inbox's view
          </button>
        )}
        {onBackToRequests && (
          <button type="button" onClick={onBackToRequests} className="amp-btn w-full sm:w-auto" style={{ minHeight: 44 }}>
            ← Your requests
          </button>
        )}
      </div>
    </div>
  )
}

/** The requester frame, with the page's nav built from the props it was given. */
function Frame({ teamName, me, nav, onSignOut, children }: {
  teamName: string
  me: RequesterIdentity
  nav?: { label: string; current: boolean; onClick?: () => void }[]
  onSignOut?: () => void
  children: React.ReactNode
}) {
  const navNode = useMemo(() => (nav && nav.length > 0
    ? nav.map(item => (
      <button
        key={item.label}
        type="button"
        onClick={item.onClick}
        aria-current={item.current ? 'page' : undefined}
        className={requesterNavItemClass(item.current)}
      >
        {item.label}
      </button>
    ))
    : undefined), [nav])
  return (
    <RequesterChrome teamName={teamName} me={me} nav={navNode} onSignOut={onSignOut}>
      {children}
    </RequesterChrome>
  )
}

/** The read in flight: the mockup's skeleton shapes, announced once as busy. */
function LoadingRequest() {
  return (
    <div className="mx-auto w-full" style={{ maxWidth: 760 }} role="status" aria-busy="true" aria-label="Loading your request">
      <div className="amp-skel mb-3 h-[26px] w-[70%]" />
      <div className="amp-skel mb-5 h-[13px] w-[42%]" />
      <div className="amp-skel mb-4 h-[56px]" />
      <div className="amp-skel mb-3 h-[92px]" />
      <div className="amp-skel h-[120px]" />
      <p className="mt-3.5 text-center font-mono text-[11.5px] text-ink-3">Loading your request…</p>
    </div>
  )
}

/** The request itself: header, stepper, the arm its state selects, then what was asked and the thread. */
function RequestView({ view, publicRef, onBackToRequests, onNewRequest, onChanged }: {
  view: RequesterRequestView
  publicRef: string
  onBackToRequests?: () => void
  onNewRequest?: () => void
  onChanged: () => void
}) {
  const track = requesterTrack(view.state)
  const reference = requestRef(view.public_ref || publicRef)
  const desk = view.team_name?.trim() || 'the desk'

  return (
    <div className="mx-auto w-full" style={{ maxWidth: 760 }}>
      {onBackToRequests && (
        <p className="m-0 mb-2.5">
          <button type="button" onClick={onBackToRequests} className="text-[12.5px] text-ink-2 underline underline-offset-2 hover:text-ink">
            ← Your requests
          </button>
        </p>
      )}

      {/* ---- Header: the subject, the reference and the one status word ---- */}
      {/* Below `sm` the tag sits *under* the subject rather than beside it: a
          tag that shares the row squeezes a 60-character subject to three short
          lines on a phone (found in the 390 px pass, not by a unit test). The
          DOM order stays title-then-tag, so the visual order never contradicts
          what a screen reader reads. */}
      <div className="mb-4 flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-start sm:gap-3">
        <div className="min-w-0 flex-1">
          <h1 className="m-0 text-[21px] font-semibold tracking-[-0.01em] text-ink" data-testid="status-subject">
            {view.subject || 'Your request'}
          </h1>
          <div className="mt-1 font-mono text-[11px] text-ink-3" data-testid="status-meta">
            {reference} · opened {view.created_at ? ago(view.created_at) : 'just now'} · {desk}
          </div>
        </div>
        <TrackTag track={track} word={view.state_word} />
      </div>

      <Stepper milestones={view.milestones} track={track} />

      {/* ---- The arm the state selects ---- */}
      {track === 'needs_reply' && view.question && (
        <div className="mb-3.5">
          <RequesterAnswerBox
            publicRef={view.public_ref || publicRef}
            question={view.question}
            teamName={desk}
            onAnswered={() => onChanged()}
          />
        </div>
      )}

      {track === 'needs_reply' && !view.question && (
        // The state says a reply is wanted and no question was shared. The
        // projection cannot invent one (a question is a comment the team wrote),
        // so the page says what is true instead of showing an empty box.
        <div className="mb-3.5 border bg-surface px-4 py-3.5" style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)', borderLeft: '5px solid var(--accent)' }}>
          <h3 className="m-0 mb-1 font-label text-[12px] font-semibold uppercase tracking-[0.16em]" style={{ color: 'var(--danger-ink)' }}>
            The desk needs a bit more from you
          </h3>
          <p className="m-0 text-[13px] leading-relaxed text-ink-2">
            Their question has not reached this page yet. Reload it — if it still is not here, {desk} has it on their
            side and will be in touch.
          </p>
        </div>
      )}

      {(track === 'delivered' || track === 'accepted') && (
        <div className="mb-3.5"><DeliveredBlock view={view} /></div>
      )}

      {(track === 'delivered' || track === 'accepted') && (
        <div className="mb-3.5">
          <RequesterAcceptBar
            publicRef={view.public_ref || publicRef}
            state={track === 'accepted' ? 'requester_accepted' : 'delivered'}
            onChanged={() => onChanged()}
          />
        </div>
      )}

      {track === 'progress' && (
        <div className="mb-3.5 border bg-surface-2 px-4 py-3.5" style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}>
          <h3 className="m-0 mb-1 font-label text-[12px] font-semibold uppercase tracking-[0.16em] text-ink-2">Where it stands</h3>
          <p className="m-0 text-[13px] leading-relaxed text-ink-2" data-testid="track-note">{REQUESTER_TRACK_NOTE[track]}</p>
        </div>
      )}

      {track === 'closed' && (
        <div className="mb-3.5 border bg-surface-2 px-4 py-3.5" style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}>
          <h3 className="m-0 mb-1 font-label text-[12px] font-semibold uppercase tracking-[0.16em] text-ink-2">Where it stands</h3>
          <p className="m-0 text-[13px] leading-relaxed text-ink-2" data-testid="track-note">{REQUESTER_TRACK_NOTE[track]}</p>
          {view.reject_reason && (
            <p className="m-0 mt-2 whitespace-pre-wrap break-words text-[13px] leading-relaxed text-ink" data-testid="reject-reason">
              <b className="font-semibold">Reason given:</b> {view.reject_reason}
            </p>
          )}
        </div>
      )}

      {/* ---- What you asked for ---- */}
      <section className="mb-3.5 border bg-surface px-4 py-3.5" style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}>
        <h2 className="m-0 mb-2 font-label text-[12px] font-semibold uppercase tracking-[0.16em] text-ink-2">What you asked for</h2>
        {view.input_brief
          ? <div className="text-ink" data-testid="status-brief"><Markdown size="compact">{view.input_brief}</Markdown></div>
          : <p className="m-0 text-[12.5px] text-ink-3">No description was sent with this request.</p>}
      </section>

      {/* ---- Files the desk shared, and the person's own uploads ---- */}
      {view.outputs.length > 0 && (
        <section className="mb-3.5 border bg-surface px-4 py-3.5" style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}>
          <h2 className="m-0 mb-2 font-label text-[12px] font-semibold uppercase tracking-[0.16em] text-ink-2">Files</h2>
          <div className="flex flex-col gap-1.5" role="list" aria-label="Files on this request">
            {view.outputs.map((file, i) => (
              <FileRow key={`${file.source}-${file.name}-${i}`} file={file} />
            ))}
          </div>
        </section>
      )}

      {/* ---- The shared thread, then the milestones that happened ---- */}
      <div className="mb-3.5">
        <RequestDiscussion mode="requester" comments={view.comments} />
      </div>
      <div className="mb-3.5">
        <RequestHistory mode="requester" milestones={view.milestones} />
      </div>

      {onNewRequest && (
        <div className="flex flex-wrap gap-2 pt-1">
          <button type="button" onClick={onNewRequest} className="amp-btn amp-btn-primary w-full sm:w-auto" style={{ minHeight: 44 }}>
            Send another request
          </button>
        </div>
      )}

      <p className="m-0 mt-4 flex items-start gap-2 text-[11.5px] leading-relaxed text-ink-3">
        <Lock size={13} className="mt-0.5 flex-shrink-0" aria-hidden="true" />
        <span>
          This page shows only your request and what {desk} has shared with you. Their internal planning, other
          customers, and account settings are never part of it.
        </span>
      </p>
    </div>
  )
}

/** The status tag: the projection's own word, in the arm's colour. */
function TrackTag({ track, word }: { track: RequesterTrack; word: string }) {
  const tone = requesterTagTone(track)
  const color = requesterTrackVar(track)
  const style = tone === 'flag' || tone === 'ok' || tone === 'prog'
    ? { color, borderColor: `color-mix(in srgb, ${color} 42%, var(--surface))`, background: `color-mix(in srgb, ${color} 7%, var(--surface))` }
    : { color: 'var(--ink)', borderColor: 'var(--line-str)', background: 'var(--surface-2)' }
  return (
    <span
      className="inline-flex flex-shrink-0 items-center gap-1.5 self-start border px-2 py-1 font-label text-[10px] font-semibold uppercase tracking-[0.14em]"
      style={{ borderRadius: 2, ...style }}
      data-testid="status-word"
      data-track={track}
    >
      <i className="block h-1.5 w-1.5" style={{ background: tone === 'done' ? 'var(--ink)' : color, borderRadius: 999 }} aria-hidden="true" />
      {word}
    </span>
  )
}

/**
 * The five-step progress bar.
 *
 * Every step's `done` is the *server's* answer (`milestones[].done`, gated by
 * `RequesterMilestoneDoneAllowed`), so a request whose linked work is all finished
 * but which the desk has not delivered shows "Delivered" as pending. The `now`
 * marker is the last done step, which is why the bar cannot point past what the
 * lifecycle says.
 */
function Stepper({ milestones, track }: { milestones: RequesterRequestView['milestones']; track: RequesterTrack }) {
  const lastDone = milestones.reduce((last, m, i) => (m.done ? i : last), -1)
  const nowColor = requesterTrackVar(track)
  return (
    <div
      className="mb-4 flex overflow-hidden border bg-surface"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
      role="list"
      aria-label="Where this request has got to"
      data-testid="status-stepper"
    >
      {milestones.map((m, i) => {
        const isNow = i === lastDone
        return (
          <div
            key={m.key}
            role="listitem"
            className="flex-1 border-r border-line-sub px-1 py-2 text-center last:border-r-0 sm:px-2"
            style={isNow ? { background: 'var(--surface-2)', boxShadow: `inset 0 3px 0 ${nowColor}` } : undefined}
            data-step={m.key}
            data-done={m.done ? 'true' : 'false'}
            data-now={isNow ? 'true' : 'false'}
          >
            <i
              className="mx-auto mb-1 block h-1.5 w-1.5 sm:h-2 sm:w-2"
              style={{ background: isNow ? nowColor : m.done ? 'var(--ink)' : 'var(--line-str)', borderRadius: 999 }}
              aria-hidden="true"
            />
            <span
              className="block font-label text-[8px] font-semibold uppercase leading-tight tracking-[0.04em] sm:text-[10px] sm:tracking-[0.12em]"
              style={{ color: isNow ? 'var(--ink)' : m.done ? 'var(--ink-2)' : 'var(--ink-3)' }}
            >
              {STEP_WORD[m.key] ?? m.key}
            </span>
            <span className="sr-only">{m.done ? 'done' : 'not yet'}</span>
          </div>
        )
      })}
    </div>
  )
}

/** The desk's delivery note and the files it names. Only `delivered`/`accepted` reach here. */
function DeliveredBlock({ view }: { view: RequesterRequestView }) {
  return (
    <div
      className="border bg-surface px-4 py-3.5"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)', borderLeft: '5px solid var(--success)' }}
    >
      <h3 className="m-0 mb-2 font-label text-[12px] font-semibold uppercase tracking-[0.16em]" style={{ color: 'var(--success-ink)' }}>
        What the desk delivered
      </h3>
      {view.outcome_brief
        ? <div className="text-ink" data-testid="outcome-brief"><Markdown size="compact">{view.outcome_brief}</Markdown></div>
        : (
          <p className="m-0 text-[13px] leading-relaxed text-ink-2" data-testid="outcome-brief-missing">
            The desk marked this delivered without a note. The files below are what was shared — if they are not what you
            asked for, reopen it.
          </p>
        )}
      {view.delivered_at && (
        <p className="m-0 mt-2 font-mono text-[10.5px] text-ink-3">Delivered {ago(view.delivered_at)}</p>
      )}
    </div>
  )
}

/** One file: its name, its size, the one of three public words, and a download when one exists. */
function FileRow({ file }: { file: RequesterRequestView['outputs'][number] }) {
  const ready = requesterOutputDownloadable(file.download_ref)
  const path = ready ? requesterDownloadPath(file.download_ref as string) : ''
  return (
    <div
      // Below `sm` the row is three lines: the name on its own, then size and
      // verdict, then the action. Sharing one line squeezed a 34-character name
      // to "octo…" on a phone (found in the 390 px pass, not by a unit test) —
      // the same defect #3981 hit in the upload picker.
      className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5 border px-2.5 py-2 text-[12.5px]"
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line)' }}
      role="listitem"
      data-testid="output-row"
      data-status={file.status}
    >
      <FileText size={14} className="flex-shrink-0 text-ink-3" aria-hidden="true" />
      {/* The name gets a whole line of its own below `sm` and shares the row from
          there up. `truncate` alone was not enough: beside a tag and a Download
          button a 34-character name collapsed to "octo…" at 390 px. */}
      <span className="w-full min-w-0 truncate font-medium text-ink sm:w-auto sm:flex-1" title={file.name}>{file.name}</span>
      <span className="font-mono text-[10.5px] text-ink-3">{requestFileSize(file.size_bytes)}</span>
      <span
        className="border px-1.5 py-[3px] font-label text-[9.5px] font-semibold uppercase leading-none tracking-[0.1em]"
        style={{
          borderRadius: 2,
          borderColor: file.status === 'ready' ? 'color-mix(in srgb, var(--success) 40%, var(--surface))' : 'var(--line)',
          color: file.status === 'ready' ? 'var(--success-ink)' : 'var(--ink-3)',
        }}
      >
        {requesterOutputWord(file.status)}
      </span>
      {ready && (
        <button
          type="button"
          onClick={() => goToDownload(path)}
          className="amp-btn amp-btn-sm flex-shrink-0"
          style={{ minHeight: 32 }}
          aria-label={`Download ${file.name}`}
        >
          <Download size={11} aria-hidden="true" />Download
        </button>
      )}
      {file.source === 'you' && (
        <span className="font-mono text-[10px] uppercase tracking-[0.08em] text-ink-3">yours</span>
      )}
    </div>
  )
}

/** Re-exported so #3988 can render the load-failure state on a wrapper page if it needs to. */
export { RequesterLoadFailure }
