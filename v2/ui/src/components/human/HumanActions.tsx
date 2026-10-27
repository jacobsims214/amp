/**
 * HumanActions: every action a person takes on their work, in one component
 * (KB decisions/human-work-ux.md §2, mockup artifact #23). The project's My
 * work cards and the TaskDrawer both use it, so a ticket opened from the
 * Board offers the same buttons as My work.
 *
 * - Answer: the question as a quote, suggested-answer chips, a text box, Send answer.
 * - Merge: Review PR ↗ and Mark merged; a PR closed without merging offers
 *   Reopen ticket and Not needed. Merges are recorded once: a repeat says
 *   who recorded it first ("already merged, recorded by github at …").
 * - Do: Start, and Mark done… with an inline note, link and hours.
 *
 * Approval is not here: it happens only in My work's Approve view
 * (ApproveCard / ApproveView). The drawer links there instead.
 */
import { useState } from 'react'
import { Check, ExternalLink, GitMerge, Loader2, Play, RotateCcw, Send } from 'lucide-react'
import { api } from '../../api/client'
import { suggestAnswers } from '../../lib/answers'
import { safeUrl } from '../../lib/safeUrl'
import { actionKindOf, type ActionKind } from '../../lib/humanActions'
import type { MergeRecord, Task } from '../../types'

export interface HumanActionsProps {
  task: Task
  /** which action; derived from the ticket when omitted */
  kind?: ActionKind | null
  /** the person may act (the owner, or someone the ticket's rules allow); otherwise read-only text */
  canAct: boolean
  /** merge: the PR and branch from the latest run (My work items carry them) */
  prUrl?: string
  branch?: string
  /** who asked (answer); defaults to the ticket's agent */
  askedBy?: string
  /** called with the updated ticket (or null when only a merge record came back) */
  onChanged?: (t: Task | null, note?: string) => void
}

export function HumanActions(props: HumanActionsProps) {
  const kind = props.kind === undefined ? actionKindOf(props.task) : props.kind
  if (kind === 'answer') return <AnswerAction {...props} />
  if (kind === 'merge') return <MergeAction {...props} />
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
      <blockquote className="m-0 pl-3 text-[14px] leading-relaxed text-ink" style={{ borderLeft: '2px solid var(--line-str)' }}>{task.block_reason}</blockquote>
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

function prLabel(url?: string): string {
  const m = url?.match(/\/pull\/(\d+)/)
  return m ? `PR #${m[1]}` : 'PR'
}

function MergeAction({ task, canAct, prUrl, branch, onChanged }: HumanActionsProps) {
  const [record, setRecord] = useState<MergeRecord | null>(null)
  const { busy, error, run } = useRun(onChanged)
  const abandoned = task.merge_state === 'abandoned'
  const href = safeUrl(prUrl ?? '')
  const merge = (fn: () => Promise<MergeRecord>, name: string) => run(name, async () => {
    const rec = await fn()
    setRecord(rec)
    return rec.task ?? null
  }, () => undefined)
  return (
    <div className="grid gap-2.5" data-action="merge">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px] text-ink-2 amp-num">
        {prUrl && <span>{prLabel(prUrl)}</span>}
        {branch && <span>{branch}</span>}
        <span>{abandoned ? 'PR closed without merging. What depends on it still waits.' : 'Done · Merge needed. What depends on it waits for the merge.'}</span>
      </div>
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
          <a className="amp-btn amp-btn-sm" href={href} target="_blank" rel="noreferrer noopener">
            <ExternalLink size={12} aria-hidden="true" />Review {prLabel(prUrl)}</a>
        )}
        {canAct && !record && (abandoned ? <>
          <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy}
            onClick={() => run('reopen', () => api.reopenTask(task.id))}>
            {busy === 'reopen' ? <Spin /> : <RotateCcw size={12} aria-hidden="true" />}Reopen ticket</button>
          <button type="button" className="amp-btn amp-btn-primary amp-btn-sm ml-auto" disabled={!!busy}
            onClick={() => merge(() => api.mergeNotNeeded(task.id), 'not-needed')}>
            {busy === 'not-needed' && <Spin />}Not needed</button>
        </> : <>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" disabled={!!busy} title="The work won't be merged: release what waits on it"
            onClick={() => merge(() => api.mergeNotNeeded(task.id), 'not-needed')}>
            {busy === 'not-needed' && <Spin />}Not needed</button>
          <button type="button" className="amp-btn amp-btn-primary amp-btn-sm ml-auto" disabled={!!busy}
            onClick={() => merge(() => api.markMerged(task.id), 'merged')}>
            {busy === 'merged' ? <Spin /> : <GitMerge size={12} aria-hidden="true" />}Mark merged</button>
        </>)}
      </div>
    </div>
  )
}

// ---- Do ----

function DoAction({ task, canAct, onChanged }: HumanActionsProps) {
  const [finishing, setFinishing] = useState(false)
  const [note, setNote] = useState('')
  const [link, setLink] = useState('')
  const [hours, setHours] = useState('')
  const { busy, error, run } = useRun(onChanged)
  const hoursNum = hours.trim() === '' ? undefined : Number(hours)
  const hoursBad = hoursNum !== undefined && (Number.isNaN(hoursNum) || hoursNum < 0 || hoursNum > 1000)
  if (task.state === 'completed') return null
  if (task.state === 'blocked') {
    return <span className="text-[12.5px] text-ink-2" data-action="do">Waiting on #{(task.blocked_by_ids?.length ? task.blocked_by_ids : task.dependency_ids).join(', #')} before it can start.</span>
  }
  if (!canAct) return <span className="text-[12.5px] text-ink-2" data-action="do">Only its owner starts and finishes it.</span>
  if (finishing) {
    return (
      <form className="grid gap-2" data-action="do" onSubmit={e => {
        e.preventDefault()
        if (hoursBad) return
        run('done', () => api.completeHumanTask(task.id, { note: note.trim() || undefined, pr_url: link.trim() || undefined, hours_worked: hoursNum }))
          .then(ok => { if (ok) setFinishing(false) })
      }}>
        <label htmlFor={`done-note-${task.id}`} className="amp-label">What happened (optional)</label>
        <textarea id={`done-note-${task.id}`} className="amp-input min-h-[56px] resize-y" value={note} onChange={e => setNote(e.target.value)}
          placeholder="Anything the next person should know" />
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
