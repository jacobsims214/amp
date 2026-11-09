/**
 * HumanActions: every action a person takes on their work, in one component
 * (v2/kb/working/human-work.md, mockup artifact #23). The project's My
 * work cards and the TaskDrawer both use it, so a ticket opened from the
 * Board offers the same buttons as My work.
 *
 * - Answer: the question as a quote, suggested-answer chips, a text box, Send answer.
 * - Merge: one action for one PR (approved artifact #27 v2, AMP #3011's
 *   contract). Review PR ↗ always; Mark merged only where it does something —
 *   never on a closed PR, never while the plan's folded step is still gated,
 *   and never on a rule alert (AMP #3158: the approval happens on GitHub) —
 *   and demoted to a quiet manual record when nothing waits on the merge.
 *   The classifier's arm decides what the card says: the detected default
 *   branch with its source, a rule card naming the exact requirement with the
 *   GitHub review link as its action and no Mark merged, or the honest unknown
 *   warn chip. A PR closed without merging offers Reopen ticket and Not needed.
 *   Merges are recorded once: a repeat says who recorded it first ("already
 *   merged, recorded by github at …"), and so does the linked step when one is
 *   folded in.
 * - Do: Start, and Mark done… with an inline note, link and hours. A ticket
 *   with a decision doc is decided on its doc, in its drawer (Record
 *   decision): Do offers Start and "Decide in the ticket", never Mark done,
 *   which the server refuses for it.
 *
 * Approval is not here: it happens only in My work's Approve view
 * (ApproveCard / ApproveView). The drawer links there instead.
 */
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowRight, Check, ExternalLink, GitMerge, Loader2, Play, RotateCcw, Send } from 'lucide-react'
import { api } from '../../api/client'
import { suggestAnswers } from '../../lib/answers'
import { safeUrl } from '../../lib/safeUrl'
import { actionKindOf, type ActionKind } from '../../lib/humanActions'
import {
  markMergedRank, mergeAlertView, mergeCardState, MERGE_MARK_GUARD, MERGE_RULE_GUARD, mergePrerequisites, mergeRoute,
  mergeTargetChip, mergeWaiters, mergeWhyText,
} from '../../lib/merge'
import type { MergeAlertKind, MergeRecord, MergeStepFold, MergeTargetSource, Task } from '../../types'
import { Markdown } from '../ui/Markdown'

export interface HumanActionsProps {
  task: Task
  /** which action; derived from the ticket when omitted */
  kind?: ActionKind | null
  /** the person may act (the owner, or someone the ticket's rules allow); otherwise read-only text */
  canAct: boolean
  /** merge: the PR and branch from the latest run (My work items carry them) */
  prUrl?: string
  branch?: string
  /** merge: the branch the merge would land on as a run reported it, and where that came from (AMP #3011) */
  baseBranch?: string
  targetSource?: MergeTargetSource
  /** merge: true waiters, the waits_for_merge: false opt-outs, and the folded plan step */
  holdingUp?: number[]
  notWaiting?: number[]
  trackedBy?: MergeStepFold
  /**
   * merge: the merge-alert classifier's decision and the facts behind it (AMP
   * #3158, artifact #27 v2). Omitted by a pre-classifier server and by the
   * TaskDrawer, which reads a ticket and no WaitItem: the card then keeps its
   * explicitly unverified behaviour rather than guessing an arm.
   */
  alert?: MergeAlertKind
  alertReason?: string
  rule?: string
  reviewUrl?: string
  defaultBranch?: string
  /** start: how long it has been dispatched, from the server's not_started_minutes */
  minutes?: number
  /** who asked (answer); defaults to the ticket's agent */
  askedBy?: string
  /** do: the owner's name, for "Only <name> starts and finishes it" */
  ownerName?: string
  /** do, a ticket with a decision doc: where it is decided (its drawer) */
  decideHref?: string
  /** called with the updated ticket (or null when only a merge record came back) */
  onChanged?: (t: Task | null, note?: string) => void
}

export function HumanActions(props: HumanActionsProps) {
  const kind = props.kind === undefined ? actionKindOf(props.task) : props.kind
  if (kind === 'answer') return <AnswerAction {...props} />
  if (kind === 'merge') return <MergeAction {...props} />
  if (kind === 'start') return <StartAction {...props} />
  if (kind === 'do') return <DoAction {...props} />
  return null
}

/** Shared busy/error handling for an action's buttons. */
function useRun(onChanged?: HumanActionsProps['onChanged']) {
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const run = async (name: string, fn: () => Promise<Task | null>, note?: (t: Task | null) => string | undefined) => {
    setBusy(name); setError(null)
    try {
      const t = await fn()
      onChanged?.(t, note?.(t))
      return true
    } catch (e) {
      setError(e instanceof Error ? e.message : 'That did not work')
      return false
    } finally {
      setBusy(null)
    }
  }
  return { busy, error, run }
}

const Spin = () => <Loader2 size={11} className="animate-spin" aria-hidden="true" />

// ---- Answer ----

function AnswerAction({ task, canAct, askedBy, onChanged }: HumanActionsProps) {
  const [answer, setAnswer] = useState('')
  const { busy, error, run } = useRun(onChanged)
  const chips = suggestAnswers(task.block_reason)
  const id = `answer-${task.id}`
  const send = () => run('send', () => api.answerFlag(task.id, answer.trim()))
    .then(ok => { if (ok) setAnswer('') })
  return (
    <div className="grid gap-2.5" data-action="answer">
      <span className="amp-label" style={{ color: 'var(--danger-ink)' }}>{askedBy || task.agent_id || task.assigned_to || 'An agent'} asked</span>
      {/* Agents write their questions as markdown (lists of options, code): shown as such. */}
      <blockquote className="m-0 pl-3 text-ink" style={{ borderLeft: '2px solid var(--line-str)' }}>
        <Markdown size="compact">{task.block_reason ?? ''}</Markdown>
      </blockquote>
      {!canAct ? (
        <span className="text-[12px] text-ink-2">Only people who can work this ticket can answer it.</span>
      ) : <>
        {chips.length > 0 && (
          <div className="flex flex-wrap gap-1.5" role="group" aria-label="Suggested answers">
            {chips.map(c => (
              <button key={c} type="button" className="text-[12.5px] px-2.5 py-1.5 border border-line bg-surface text-ink-2 hover:text-ink hover:border-ink cursor-pointer"
                style={{ borderRadius: 'var(--radius)' }} aria-pressed={answer === c} onClick={() => setAnswer(c)}>{c}</button>
            ))}
          </div>
        )}
        <label htmlFor={id} className="amp-label">Your answer</label>
        <textarea id={id} className="amp-input min-h-[64px] resize-y" value={answer} onChange={e => setAnswer(e.target.value)}
          placeholder="The agent picks this up on its next run" />
        {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
        <div className="flex gap-2 flex-wrap">
          <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" disabled={!!busy || !answer.trim()} onClick={send}>
            {busy === 'send' ? <Spin /> : <Send size={12} aria-hidden="true" />}Send answer</button>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={!!busy} title="Unblock without a comment"
            onClick={() => run('unblock', () => api.answerFlag(task.id, ''))}>
            {busy === 'unblock' && <Spin />}Unblock only</button>
        </div>
      </>}
    </div>
  )
}

// ---- Merge ----

function MergeAction({ task, canAct, prUrl, branch, baseBranch, targetSource, holdingUp, notWaiting, trackedBy, alert, alertReason, rule, reviewUrl, defaultBranch, onChanged }: HumanActionsProps) {
  const [record, setRecord] = useState<MergeRecord | null>(null)
  const { busy, error, run } = useRun(onChanged)
  const abandoned = task.merge_state === 'abandoned'
  // Where the click lands: primary when a merge is the next thing owed,
  // secondary when nothing waits, absent when a step still gates it — and
  // absent on a rule alert, where the owed act is the review on GitHub and the
  // merge is recorded by the webhook afterwards (artifact #27 state 3).
  const state = mergeCardState({
    merge_state: task.merge_state, holding_up: holdingUp, tracked_by: trackedBy, target_source: targetSource,
  })
  const rank = markMergedRank(state, alert)
  const prereqs = mergePrerequisites(trackedBy)
  // One component owns the whole merge concern — where it goes and how AMP
  // knows, why it's here, who waits, what the plan still gates, and the one
  // action — so My work's card and the TaskDrawer can never disagree about the
  // same PR (approved artifact #27 v2's card anatomy, top to bottom).
  const route = mergeRoute({ pr_url: prUrl, branch, base_branch: baseBranch, target_source: targetSource })
  // The classifier's verdict, or null on a server that made none. Its arm
  // decides the why-line, the rule box, the chip and which action is offered;
  // the wording of "why" is the server's, so policy and card can't drift.
  const view = mergeAlertView({
    alert, alert_reason: alertReason, rule, review_url: reviewUrl, default_branch: defaultBranch,
  })
  const ruleAlert = view?.kind === 'rule' ? view : null
  // `internal` (agent organization) and `none` (already merged) ask this person
  // nothing at all. The server drops those items before they reach a list, so
  // this only guards a server that sends one anyway — and it must not grow an
  // action: offering Not needed for agent organization would invite a person to
  // dismiss work that was never theirs.
  const silent = view?.kind === 'silent'
  const href = safeUrl((ruleAlert?.reviewUrl ?? undefined) ?? prUrl ?? '')
  const legacyChip = !view && targetSource ? mergeTargetChip({ target_source: targetSource }) : null
  const waiters = holdingUp === undefined && targetSource === undefined ? null : mergeWaiters({
    holding_up: holdingUp, not_waiting: notWaiting, target_source: targetSource,
  })
  const pr = route.pr === 'PR' ? 'the PR' : route.pr
  const merge = (fn: () => Promise<MergeRecord>, name: string) => run(name, async () => {
    const rec = await fn()
    setRecord(rec)
    return rec.task ?? null
  }, () => undefined)
  // A folded plan step: its own completion is the same act as recording the
  // merge, so the record says which happened rather than leaving it ambiguous.
  const step = record?.linked_step
  return (
    <div className="grid gap-2.5" data-action="merge" data-merge-state={state}>
      {/* "PR #88 · feat/x → main": the separators are text, not just the CSS
          gap, so the route reads the same to a screen reader. */}
      {(prUrl || branch) && (
        <div className="pmw-route" data-testid="merge-route">
          {prUrl && <span className="pr">{route.pr}</span>}
          {route.head && <>{' · '}<span>{route.head}</span></>}
          {route.known
            ? <>{' → '}<span className="target">{route.target}</span></>
            : <>{' · '}<span className="target unknown">target not verified</span></>}
        </div>
      )}
      {(view || targetSource || prUrl) && (
        <p className="pmw-why" data-testid="merge-why">
          {/* Artifact #27 v2 labels state 3 and 9 "Why you're asked", but state
              6 (internal) asks nothing — its own label is "Why you're not
              asked", and reusing the alert label there would misdescribe an
              informational row as a chore. */}
          <span className="amp-label">{silent ? "Why you're not asked" : "Why you're asked"}</span>
          <span>{view ? view.reason : mergeWhyText({ base_branch: baseBranch, target_source: targetSource })}
            {view?.chip && <>{' '}<span className={`pmw-chip${view.chip.warn ? ' warn' : ''}`}
              data-testid="merge-chip"
              title={view.chip.warn
                ? 'AMP could not verify this: read the reason above, and link the project to its repository to resolve it'
                : 'Read from the repository by AMP, not inferred from a label or a hint'}>
              {view.chip.text}</span></>}
            {legacyChip && <>{' '}<span className={`pmw-chip${legacyChip.warn ? ' warn' : ''}`}
              data-testid="merge-chip"
              title={legacyChip.warn ? 'AMP holds no repository metadata: link the project to its repository to resolve this' : 'Read from the run that put the work up'}>
              {legacyChip.text}</span></>}
          </span>
        </p>
      )}
      {/* State 3's rule box: the exact requirement, in the repository's own
          terms (which ruleset demands what, and why it is this person). It is
          present only for a rule alert — a stray rule string on another arm is
          a policy bug, and the card shows nothing rather than invent a rule. */}
      {ruleAlert && ruleAlert.rule && (
        <div className="pmw-rule-box" data-testid="merge-rule">
          <span className="amp-label">The exact requirement</span>
          <p>{ruleAlert.rule}</p>
        </div>
      )}
      {/* When a plan step is folded in, that step already says what the merge is
          waiting for, and a closed PR has its own line below — a "nothing waits"
          line next to either would contradict it. A rule card's action is the
          review, not a recording here, so it does not offer the "recording it
          here" framing either. A silent arm (internal / already merged) asks
          nothing at all, so it makes no claim about waiters: state 6's whole
          point is that agent organization is not a person's chore. Real waiters
          are still worth naming whenever the card is a real alert. */}
      {waiters && !silent && ((!trackedBy && !abandoned && !ruleAlert) || (holdingUp?.length ?? 0) > 0) && (
        <p className="pmw-waiters" data-testid="merge-waiters"><b>{waiters.lead}</b>{waiters.detail}</p>
      )}
      {trackedBy && (
        <div className={trackedBy.ready ? 'pmw-step-ready' : 'pmw-step-wait'} data-testid="merge-step">
          <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
            <span className="amp-label">Plan step</span>
            <span className="text-[12.5px] text-ink-2">
              Tracked by <span className="amp-num text-link">#{trackedBy.task_id}</span> {trackedBy.name}
              {trackedBy.state === 'in_progress' ? ' — in progress' : trackedBy.state === 'blocked' ? ' — waiting on its owner' : ''}
            </span>
          </div>
          {!trackedBy.ready && prereqs.length > 0 && (
            <ul className="pmw-prereqs" aria-label="Merge prerequisites">
              {prereqs.map(id => (
                <li key={id}><span className="box" aria-hidden="true" />
                  <span className="rid">#{id}</span> <span>still open</span></li>
              ))}
            </ul>
          )}
          {trackedBy.ready
            ? <span className="text-[12.5px] text-ink-2">All conditions met: <b className="text-ink">Mark merged</b> records the merge and completes <span className="amp-num">#{trackedBy.task_id}</span> in one step.</span>
            : prereqs.length === 0 && trackedBy.reason && <span className="text-[12.5px] text-ink-2">{trackedBy.reason}.</span>}
        </div>
      )}
      {abandoned && (
        <p className="m-0 text-[12.5px] text-ink-2">PR closed without merging. What depends on it still waits.</p>
      )}
      {step && (
        <p role="status" className="m-0 text-[12.5px] text-ink-2">
          {step.already_done ? <>Its merge step <span className="text-ink">#{step.task_id}</span> was already done.</>
            : step.completed ? <>Its merge step <span className="text-ink">#{step.task_id}</span> was completed by this record: nothing left to mark done.</>
              : <>Its merge step <span className="text-ink">#{step.task_id}</span> stays open: {step.message}.</>}
        </p>
      )}
      {record && (
        <p role="status" className="m-0 text-[12.5px] text-ink-2">
          {record.already_recorded ? <>Already recorded: <span className="text-ink">{record.message}</span>.</>
            : record.merge_state === 'merged' ? 'Merged. What waited on it can start.'
              : record.merge_state === 'none' ? 'Marked not needed. What waited on it can start.' : record.message}
        </p>
      )}
      {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
      <div className="flex gap-2 flex-wrap items-center">
        {href && (
          <a className={`amp-btn amp-btn-sm${ruleAlert ? ' amp-btn-primary' : ''}`} href={href} target="_blank" rel="noreferrer noopener"
            data-testid="merge-review">
            <ExternalLink size={12} aria-hidden="true" />{ruleAlert ? `Review and approve ${pr}` : `Review ${pr}`}</a>
        )}
        {canAct && !record && !silent && (abandoned ? <>
          <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy}
            onClick={() => run('reopen', () => api.reopenTask(task.id))}>
            {busy === 'reopen' ? <Spin /> : <RotateCcw size={12} aria-hidden="true" />}Reopen ticket</button>
          <button type="button" className="amp-btn amp-btn-primary amp-btn-sm ml-auto" disabled={!!busy}
            onClick={() => merge(() => api.mergeNotNeeded(task.id), 'not-needed')}>
            {busy === 'not-needed' && <Spin />}Not needed</button>
        </> : ruleAlert ? null : <>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={!!busy} title="The work won't be merged: release what waits on it"
            onClick={() => merge(() => api.mergeNotNeeded(task.id), 'not-needed')}>
            {busy === 'not-needed' && <Spin />}Not needed</button>
          {/* One action per PR: Mark merged is absent, not disabled, when a
              gated plan step hasn't met its prerequisites, and on a rule alert
              (its action is the review on GitHub). */}
          {rank && (
            <button type="button" disabled={!!busy}
              className={`amp-btn amp-btn-sm ml-auto${rank === 'primary' ? ' amp-btn-primary' : ''}`}
              title={rank === 'secondary' ? 'Nothing waits on this merge: GitHub or CI records it by itself. Use this only after you merged it on GitHub.' : undefined}
              onClick={() => merge(() => api.markMerged(task.id), 'merged')}>
              {busy === 'merged' ? <Spin /> : <GitMerge size={12} aria-hidden="true" />}Mark merged</button>
          )}
        </>)}
      </div>
      {canAct && !record && rank === 'primary' && (
        <p className="m-0 text-[11.5px] leading-[1.45] text-ink-3 max-w-[52ch]">{MERGE_MARK_GUARD}</p>
      )}
      {canAct && !record && ruleAlert && (
        <p className="m-0 text-[11.5px] leading-[1.45] text-ink-3 max-w-[52ch]" data-testid="merge-rule-guard">{MERGE_RULE_GUARD}</p>
      )}
      {canAct && !record && state === 'gated-waiting' && trackedBy && (
        <p className="m-0 text-[11.5px] leading-[1.45] text-ink-3 max-w-[52ch]">
          No Mark merged while its merge step <b className="text-ink-2">#{trackedBy.task_id}</b>
          {prereqs.length > 0 ? <> still waits on {prereqs.map(id => `#${id}`).join(', ')}</> : ` isn't ready (${trackedBy.reason || 'not yet'})`}.
          {' '}Recording it early can't overturn the plan.
        </p>
      )}

    </div>
  )
}

// ---- Start (a dispatched ticket no worker started) ----

/**
 * A ticket of yours is dispatched to an agent and no worker has started it
 * (AMP #2801): the run was never launched, so nothing is happening on it.
 * Returning it to ready clears the dispatch and puts it back where a manager
 * (or you) dispatches it again; the watchdog does the same by itself at the
 * project's not_started_return_minutes. "Keep waiting" changes nothing: the
 * watchdog will still return it at the timeout.
 */
function StartAction({ task, canAct, minutes, onChanged }: HumanActionsProps) {
  const { busy, error, run } = useRun(onChanged)
  return (
    <div className="grid gap-2.5" data-action="start">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px] text-ink-2">
        <span>
          Dispatched{task.agent_id ? <> to <b className="text-ink">{task.agent_id}</b></> : null}
          {minutes != null ? ` ${minutes} min ago` : ''}; no worker has started it.
        </span>
      </div>
      {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
      {!canAct
        ? <span className="text-[12px] text-ink-2">Only its owner can return it to ready.</span>
        : <div className="flex gap-2 flex-wrap items-center">
          <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" disabled={!!busy}
            title="Clear the dispatch and put it back where it can be dispatched again"
            onClick={() => run('return', () => api.returnTask(task.id, 'no worker started it'))}>
            {busy === 'return' ? <Spin /> : <RotateCcw size={12} aria-hidden="true" />}Return to ready</button>
          <span className="text-[12px] text-ink-3">Keep waiting, and AMP returns it by itself if no worker starts it.</span>
        </div>}
    </div>
  )
}

// ---- Do ----

function DoAction({ task, canAct, ownerName, decideHref, onChanged }: HumanActionsProps) {
  const [finishing, setFinishing] = useState(false)
  const [note, setNote] = useState('')
  const [link, setLink] = useState('')
  const [hours, setHours] = useState('')
  const { busy, error, run } = useRun(onChanged)
  const hoursNum = hours.trim() === '' ? undefined : Number(hours)
  const hoursBad = hoursNum !== undefined && (Number.isNaN(hoursNum) || hoursNum < 0 || hoursNum > 1000)
  // A judgment or approval ticket is a decision others wait on: the note is the decision.
  const decision = task.human_reason === 'judgment' || task.human_reason === 'approval'
  const [noteMissing, setNoteMissing] = useState(false)
  if (task.state === 'completed') return null
  if (task.state === 'blocked') {
    return <span className="text-[12.5px] text-ink-2" data-action="do">Waiting on #{(task.blocked_by_ids?.length ? task.blocked_by_ids : task.dependency_ids).join(', #')} before it can start.</span>
  }
  if (!canAct) return <span className="text-[12.5px] text-ink-2" data-action="do">Only {ownerName || 'its owner'} starts and finishes it.</span>
  if (task.has_decision_doc) {
    // Its decision is recorded on its doc; Mark done would be refused.
    return (
      <div className="flex gap-2 flex-wrap items-center" data-action="do">
        {task.state === 'backlog' && (
          <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy} onClick={() => run('start', () => api.startHumanTask(task.id))}>
            {busy === 'start' ? <Spin /> : <Play size={12} aria-hidden="true" />}Start</button>
        )}
        {decideHref
          ? <Link to={decideHref} className="amp-btn amp-btn-primary amp-btn-sm ml-auto">Decide in the ticket<ArrowRight size={12} aria-hidden="true" /></Link>
          : <span className="text-[12.5px] text-ink-2 ml-auto">Record the decision on its doc, in the ticket.</span>}
        {error && <span role="alert" className="text-[12px] text-danger-ink w-full">{error}</span>}
      </div>
    )
  }
  if (finishing) {
    return (
      <form className="grid gap-2" data-action="do" onSubmit={e => {
        e.preventDefault()
        if (hoursBad) return
        if (decision && !note.trim()) { setNoteMissing(true); return }
        run('done', () => api.completeHumanTask(task.id, { note: note.trim() || undefined, pr_url: link.trim() || undefined, hours_worked: hoursNum }))
          .then(ok => { if (ok) setFinishing(false) })
      }}>
        <label htmlFor={`done-note-${task.id}`} className="amp-label">{decision ? 'Your decision' : 'What happened (optional)'}</label>
        <textarea id={`done-note-${task.id}`} className="amp-input min-h-[56px] resize-y" value={note}
          onChange={e => { setNote(e.target.value); setNoteMissing(false) }}
          aria-required={decision} aria-invalid={noteMissing}
          placeholder={decision ? 'What you decided. The tickets waiting on this receive it' : 'Anything the next person should know'} />
        {noteMissing && <span role="alert" className="text-[12px] text-danger-ink">Write what you decided</span>}
        <div className="grid grid-cols-[1fr_110px] gap-2">
          <div className="grid gap-1">
            <label htmlFor={`done-link-${task.id}`} className="amp-label">Link (optional)</label>
            <input id={`done-link-${task.id}`} className="amp-input" value={link} onChange={e => setLink(e.target.value)} placeholder="PR, doc, or ticket URL" />
          </div>
          <div className="grid gap-1">
            <label htmlFor={`done-hours-${task.id}`} className="amp-label">Hours spent</label>
            <input id={`done-hours-${task.id}`} className="amp-input" inputMode="decimal" value={hours} onChange={e => setHours(e.target.value)} placeholder="1.5" />
          </div>
        </div>
        <span className="text-[11.5px] text-ink-3">Hours spent is the work itself, not the waiting. Reports compare the two.</span>
        {hoursBad && <span role="alert" className="text-[12px] text-danger-ink">Hours must be a number from 0 to 1000.</span>}
        {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
        <div className="flex gap-2">
          <button type="submit" className="amp-btn amp-btn-primary amp-btn-sm" disabled={!!busy || hoursBad}>
            {busy === 'done' ? <Spin /> : <Check size={12} aria-hidden="true" />}Mark done</button>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={!!busy} onClick={() => setFinishing(false)}>Cancel</button>
        </div>
      </form>
    )
  }
  return (
    <div className="flex gap-2 flex-wrap items-center" data-action="do">
      {task.state === 'backlog' && (
        <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy} onClick={() => run('start', () => api.startHumanTask(task.id))}>
          {busy === 'start' ? <Spin /> : <Play size={12} aria-hidden="true" />}Start</button>
      )}
      <button type="button" className="amp-btn amp-btn-primary amp-btn-sm ml-auto" disabled={!!busy} onClick={() => setFinishing(true)}>
        <Check size={12} aria-hidden="true" />Mark done…</button>
      {error && <span role="alert" className="text-[12px] text-danger-ink w-full">{error}</span>}
    </div>
  )
}
