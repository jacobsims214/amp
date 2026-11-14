/**
 * The request detail's Triage rail (AMP #3980, build slice J2; approved mockup
 * artifact 37 v1, "Request detail" → Triage).
 *
 * Two fields, deliberately separate:
 *
 *  - **Claim** — "I am looking at this right now". One active claim per request,
 *    first-write-wins, manual, never expired (contract invariant I4, default
 *    D4). Its age is rendered because a claim that never expires is only honest
 *    if its staleness is visible.
 *  - **Assignee** — who owns the outcome to delivery. A team manager's transfer
 *    clears the claim in the same statement on the server, which is why the rail
 *    can never show a new owner beside a stale claim.
 *
 * Conflating them into one control is the mistake the contract exists to
 * prevent, so they are two rows with two labels and one shared caption.
 *
 * A lost claim race is a 409 that names the winner. The rail answers it with the
 * mockup's banner — who won, that claims are first-write-wins, and that nothing
 * was typed into the void — rather than retrying or silently taking the claim.
 * Nothing here approves, dispatches or completes anything: it takes a claim,
 * releases it, and sets an assignee.
 */
import { useState } from 'react'
import { Loader2 } from 'lucide-react'
import { requestApi, type HttpError } from '../../api/client'
import { MAX_REQUEST_REJECT_REASON_LEN } from '../../types'
import { lookupPerson, personName, type Person } from '../../lib/people'
import {
  canClaim, canRelease, claimConflictCopy, claimState, readClaimConflict, triageActs, type ClaimConflict,
} from '../../lib/requestControls'
import type { Member, RequestMutationAnswer, TeamRequest } from '../../types'
import { ClaimWho, CLAIM_VS_ASSIGNEE, DisabledHint, RailBanner, RailField } from './RequestRailBits'

export interface RequestTriageRailProps {
  /** The request as stored — the rail reads its claim, assignee and revision. */
  request: TeamRequest
  /** The signed-in caller: "mine" claims, the assignee's "(you)", and who may release. */
  meId?: number
  /** The inbox team's members: the only people an assignee may be set to (the server refuses anyone else). */
  members: Member[]
  /** Whether the caller may assign — a team lead or an org owner/admin, per the server's `RequireTeamManager`. */
  canAssign: boolean
  /**
   * Fired after a successful write. The write answers the act, not the record
   * (no claim, no assignee), so the page must re-read the request it renders.
   */
  onChanged?: (answer: RequestMutationAnswer) => void
  /**
   * The three triage acts the approved mockup puts beside the claim: ask the
   * requester a question, decline with a reason, fold into a canonical request.
   * Omitted when the page does not offer them; the buttons then do not render,
   * so the rail never shows a control that cannot work.
   */
  onTransition?: {
    ask: (question: string) => Promise<unknown>
    reject: (reason: string) => Promise<unknown>
    duplicate: (canonicalRequestId: number) => Promise<unknown>
  }
  /** Re-read the request after a conflict ("Refresh" on the banner). */
  onRefresh?: () => void
  /** Open the request's history ("Open history" on the banner); omitted when the page has no history yet. */
  onOpenHistory?: () => void
  /** The clock for the claim's age, so a test can pin it. */
  now?: number
}

type Busy = 'claim' | 'release' | 'assign' | 'ask' | 'reject' | 'duplicate' | null

/** Which triage act's inline form is open. One at a time, so two acts cannot race each other. */
type ActForm = 'ask' | 'reject' | 'duplicate' | null

/** One refusal the rail states in place: a claim conflict, a stale view, or a plain failure. */
type Failure =
  | { kind: 'claim'; conflict: ClaimConflict }
  | { kind: 'stale'; from: number; to?: number; message: string }
  | { kind: 'error'; message: string }

export function RequestTriageRail(props: RequestTriageRailProps) {
  const { request, meId, members, canAssign, onTransition, onChanged, onRefresh, onOpenHistory, now } = props
  const [busy, setBusy] = useState<Busy>(null)
  const [failure, setFailure] = useState<Failure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [act, setAct] = useState<ActForm>(null)
  const [actText, setActText] = useState('')

  // Which acts this state allows, and which of those need a lead. The graph is
  // lib/requestControls' `triageActs` (the server's own table), so the rail
  // never offers an act the service would refuse for the state's sake.
  const acts = triageActs(request.state, canAssign)
  const anyAct = acts.ask.available || acts.reject.available || acts.duplicate.available
  const closedCopy = request.state === 'delivered' || request.state === 'requester_accepted'
    ? 'Delivered: waiting on the requester. Acceptance and reopening after delivery are theirs alone (I1).'
    : request.state === 'rejected'
      ? 'Declined. The team reopens it from the request’s lifecycle; the requester cannot.'
      : request.state === 'duplicate'
        ? 'Folded into a canonical request: triage happens there, and reopening this one detaches it.'
        : ''

  const people = new Map<number, Person>(members.map(m => [m.user_id, { id: m.user_id, name: personName(m), email: m.email }]))
  const claim = claimState(request, meId)
  const assignee = lookupPerson(people, request.owner_user_id)
  // The name behind a conflict's holder id: the banner names a person, not a number.
  const holderPerson = (id: number | undefined) => (id != null ? lookupPerson(people, id) : null)

  const run = async (what: Exclude<Busy, null>, fn: () => Promise<RequestMutationAnswer>) => {
    setBusy(what)
    setFailure(null)
    setNotice(null)
    try {
      const answer = await fn()
      // The answer carries `changed: false` for a replay (the row already looked
      // like this), which is success — a second Claim click by the same person
      // is not an error.
      setNotice(what === 'claim'
        ? (answer.changed ? null : 'You already hold this claim: nothing changed.')
        : what === 'release'
          ? (answer.changed ? null : 'The claim was already released.')
          : (answer.changed ? null : 'The request already had that assignee.'))
      onChanged?.(answer)
    } catch (err) {
      setFailure(failureOf(err, request.rev, holderPerson))
    } finally {
      setBusy(null)
    }
  }

  const claimNow = () => run('claim', () => requestApi.claim(request.team_id, request.id, { expected_rev: request.rev }))
  const releaseNow = () => run('release', () => requestApi.release(request.team_id, request.id, { expected_rev: request.rev }))
  const assignTo = (assigneeRef: string) =>
    run('assign', () => requestApi.assign(request.team_id, request.id, { assignee: assigneeRef, expected_rev: request.rev }))

  /**
   * Run one triage act through the page's own call and report its outcome.
   *
   * The rail does not build a transition body of its own: which route an act
   * takes, whether it needs a question or a reason, and what the server refuses
   * are all the page's (and the service's) business. This only collects the one
   * field the act needs and states what came back.
   */
  const runAct = async (which: Exclude<ActForm, null>, call: () => Promise<unknown>, done: string) => {
    setBusy(which)
    setFailure(null)
    setNotice(null)
    try {
      await call()
      setAct(null)
      setActText('')
      setNotice(done)
    } catch (err) {
      setFailure(failureOf(err, request.rev, holderPerson))
    } finally {
      setBusy(null)
    }
  }

  const submitAct = async () => {
    if (!onTransition || !act) return
    const text = actText.trim()
    if (text === '') return
    if (act === 'ask') {
      await runAct('ask', () => onTransition.ask(text), 'The question is with the requester: the request is now Needs information.')
    } else if (act === 'reject') {
      await runAct('reject', () => onTransition.reject(text), 'Declined, with your reason. The requester sees it verbatim.')
    } else {
      const id = Number.parseInt(text, 10)
      if (!Number.isFinite(id) || id <= 0) return
      await runAct('duplicate', () => onTransition.duplicate(id), 'Folded into the canonical request; this one stays readable.')
    }
  }

  const actCopy = act === 'ask'
    ? {
        label: 'Question to the requester',
        placeholder: 'What you need to know — the requester reads this verbatim',
        confirm: 'Send question',
        hint: 'Asking obliges an answer: the request stays in Needs information until the requester answers (I5). ' +
          'There is no SLA clock on the requester in v1.',
      }
    : act === 'reject'
      ? {
          label: 'Why it is declined',
          placeholder: 'The reason, in the requester’s words — they read it verbatim',
          confirm: 'Reject request',
          hint: 'A rejection carries a reason and an actor. The team can reopen it later; the requester cannot.',
        }
      : {
          label: 'The canonical request',
          placeholder: 'Request id it duplicates',
          confirm: 'Mark duplicate',
          hint: 'A canonical is never a duplicate. Reopening this one detaches it (D7), and its follow-up ' +
            'moves to the canonical request.',
        }

  return (
    <section className="amp-panel" aria-label="Triage">
      <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
        <span className="amp-label">Triage</span>
      </div>
      <div className="px-4 py-3 flex flex-col gap-3">
        {failure?.kind === 'claim' && (
          <RailBanner
            tone="err"
            title={claimConflictCopy(failure.conflict).title}
            actions={
              <>
                {onRefresh && (
                  <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" onClick={() => { setFailure(null); onRefresh() }}>
                    Refresh
                  </button>
                )}
                {onOpenHistory && (
                  <button type="button" className="amp-btn amp-btn-sm" onClick={onOpenHistory}>Open history</button>
                )}
              </>
            }
          >
            {claimConflictCopy(failure.conflict).detail}
          </RailBanner>
        )}
        {failure?.kind === 'stale' && (
          <RailBanner
            tone="err"
            title="This request changed since your view loaded."
            actions={onRefresh && (
              <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" onClick={() => { setFailure(null); onRefresh() }}>
                Refresh
              </button>
            )}
          >
            {failure.to != null && failure.to !== failure.from
              ? `It is at rev ${failure.to} now, and your view was from rev ${failure.from}. `
              : ''}
            Nothing was changed. Reload to see the current claim and assignee, then act again.
          </RailBanner>
        )}
        {failure?.kind === 'error' && (
          <RailBanner tone="err" title="That didn’t work.">{failure.message} Nothing was changed.</RailBanner>
        )}
        {notice && <p className="m-0 text-[12px] text-ink-3" role="status">{notice}</p>}

        <dl className="grid grid-cols-[76px_minmax(0,1fr)] gap-x-3 gap-y-3 items-start m-0">
          <RailField
            label="Claim"
            hint={claim.kind === 'other'
              ? 'A claim is released by its holder, or cleared when a lead transfers the assignee — never taken.'
              : undefined}
          >
            {claim.kind === 'none' ? (
              <div className="flex items-center gap-2 flex-wrap">
                <span className="font-label text-[10.5px] font-semibold uppercase tracking-[0.14em] text-ink-3 border border-dashed px-2 py-1"
                  style={{ borderColor: 'var(--line-str)' }}>
                  Unclaimed
                </span>
                <button
                  type="button"
                  className="amp-btn amp-btn-sm amp-btn-primary"
                  disabled={busy !== null || !canClaim(request, meId)}
                  title={canClaim(request, meId) ? undefined : 'Sign in as a member of this team to claim the request.'}
                  onClick={claimNow}
                >
                  {busy === 'claim' && <Loader2 size={11} className="animate-spin" aria-hidden="true" />}
                  Claim
                </button>
              </div>
            ) : (
              <div className="flex items-center gap-2 flex-wrap">
                <ClaimWho person={holderPerson(claim.userId)} at={claim.at} now={now} />
                {claim.kind === 'mine' && <span className="text-[11.5px] text-ink-3">(you)</span>}
                {canRelease(request, meId) && (
                  <button
                    type="button"
                    className="amp-btn amp-btn-sm amp-btn-ghost"
                    disabled={busy !== null}
                    onClick={releaseNow}
                  >
                    {busy === 'release' && <Loader2 size={11} className="animate-spin" aria-hidden="true" />}
                    Release
                  </button>
                )}
              </div>
            )}
          </RailField>

          <RailField
            label="Assignee"
            hint={canAssign
              ? 'Whoever owns the outcome. A transfer clears the claim.'
              : 'Only a team lead or an org owner/admin can assign a request.'}
          >
            <div className="flex items-center gap-2 flex-wrap">
              <select
                className="amp-input !w-auto !text-[12.5px] !py-1"
                aria-label="Assign to a team member"
                disabled={busy !== null || !canAssign}
                value={request.owner_user_id != null ? String(request.owner_user_id) : 'none'}
                onChange={e => assignTo(e.target.value)}
              >
                <option value="none">Unassigned</option>
                {members.map(m => (
                  <option key={m.user_id} value={String(m.user_id)}>
                    {personName(m)}{m.user_id === meId ? ' (you)' : ''}
                  </option>
                ))}
                {assignee && !people.has(assignee.id) && (
                  // An owner who has since left the team: shown, never silently dropped.
                  <option value={String(assignee.id)}>{assignee.name} (no longer on the team)</option>
                )}
              </select>
              {busy === 'assign' && <Loader2 size={12} className="animate-spin text-ink-3" aria-hidden="true" />}
            </div>
          </RailField>
        </dl>

        <DisabledHint id="claim-vs-assignee">{CLAIM_VS_ASSIGNEE}</DisabledHint>

        {onTransition && anyAct && (
          <div className="border-t border-line-sub pt-3 flex flex-col gap-2">
            {act ? (
              <div className="flex flex-col gap-1.5">
                <label className="amp-label" htmlFor="triage-act">{actCopy.label}</label>
                {act === 'ask' || act === 'reject' ? (
                  <textarea
                    id="triage-act"
                    className="amp-input resize-y min-h-[62px] !text-[13px]"
                    rows={3}
                    maxLength={act === 'reject' ? MAX_REQUEST_REJECT_REASON_LEN : undefined}
                    value={actText}
                    disabled={busy !== null}
                    onChange={e => setActText(e.target.value)}
                    placeholder={actCopy.placeholder}
                  />
                ) : (
                  <input
                    id="triage-act"
                    className="amp-input !text-[13px]"
                    inputMode="numeric"
                    value={actText}
                    disabled={busy !== null}
                    onChange={e => setActText(e.target.value)}
                    placeholder={actCopy.placeholder}
                  />
                )}
                <p className="m-0 text-[11.5px] text-ink-3 leading-[1.45]">{actCopy.hint}</p>
                <div className="flex gap-2">
                  <button
                    type="button"
                    className={`amp-btn amp-btn-sm ${act === 'reject' ? 'amp-btn-danger' : 'amp-btn-primary'}`}
                    disabled={busy !== null || actText.trim() === ''}
                    onClick={submitAct}
                  >
                    {busy === act && <Loader2 size={11} className="animate-spin" aria-hidden="true" />}
                    {actCopy.confirm}
                  </button>
                  <button
                    type="button"
                    className="amp-btn amp-btn-sm amp-btn-ghost"
                    disabled={busy !== null}
                    onClick={() => { setAct(null); setActText('') }}
                  >
                    Cancel
                  </button>
                </div>
              </div>
            ) : (
              <div className="flex gap-2 flex-wrap">
                <button
                  type="button"
                  className="amp-btn amp-btn-sm"
                  disabled={!acts.ask.available}
                  title={acts.ask.reason}
                  onClick={() => { setAct('ask'); setActText('') }}
                >
                  Ask the requester
                </button>
                <button
                  type="button"
                  className="amp-btn amp-btn-sm amp-btn-danger"
                  disabled={!acts.reject.available}
                  title={acts.reject.reason}
                  onClick={() => { setAct('reject'); setActText('') }}
                >
                  Reject…
                </button>
                <button
                  type="button"
                  className="amp-btn amp-btn-sm amp-btn-ghost"
                  disabled={!acts.duplicate.available}
                  title={acts.duplicate.reason}
                  onClick={() => { setAct('duplicate'); setActText('') }}
                >
                  Mark duplicate…
                </button>
              </div>
            )}
            {/* The reason is stated on screen, not only in the title: a lead-only
                act on a plain member's screen has to say whose act it is. */}
            {!acts.reject.available && acts.reject.reason && (
              <p className="m-0 text-[11.5px] text-ink-3 leading-[1.45]">{acts.reject.reason}</p>
            )}
          </div>
        )}
        {onTransition && !anyAct && closedCopy && (
          <DisabledHint id="triage-closed">{closedCopy}</DisabledHint>
        )}
      </div>
    </section>
  )
}

/**
 * Read a write's refusal as the rail can state it.
 *
 * A 409 that names a holder is a lost claim race; a 409 that carries only a
 * revision is a stale view; anything else is the server's own sentence. All
 * three keep the promise the mockup's banner makes — nothing was changed, and
 * the reader is told what to do next.
 */
function failureOf(err: unknown, fromRev: number, holder: (id: number | undefined) => Person | null): Failure {
  const conflict = readClaimConflict(err, fromRev, undefined)
  if (conflict) {
    const name = holder(conflict.holderUserId)?.name
    return { kind: 'claim', conflict: { ...conflict, holderName: name } }
  }
  const e = err as HttpError | undefined
  const message = e?.message?.trim() || 'The request could not be updated.'
  if (e?.status === 409) return { kind: 'stale', from: fromRev, to: e.rev, message }
  return { kind: 'error', message }
}
