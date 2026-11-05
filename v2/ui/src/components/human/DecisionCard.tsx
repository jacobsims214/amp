/**
 * DecisionCard: a judgment ticket's decision doc in its drawer
 * (docs/specs/human-decision-docs.md §5). It shows the question and the
 * options of the doc's current version (OptionPicker), the doc's prose
 * (context, trade-offs, why the recommendation), and "Decided means": the
 * ticket's acceptance criteria, read-only.
 *
 * Its action is Mark done: it sends "<option label> — <decision text>" (just
 * the text for Something else) through the same completion as any person's
 * ticket, so the tickets waiting on it receive the decision. Edit doc,
 * provenance and Record decision come later, in this same card.
 */
import { useState } from 'react'
import { Check, Loader2, Play, Sparkles } from 'lucide-react'
import { api } from '../../api/client'
import { checklistItems } from '../../lib/brief'
import { decisionNote, type OptionChoice } from '../../lib/decisions'
import type { DecisionDoc, Task } from '../../types'
import { Markdown } from '../ui/Markdown'
import { OptionPicker } from './OptionPicker'

export interface DecisionCardProps {
  task: Task
  doc: DecisionDoc
  /** the person may decide (its owner, or anyone on an unassigned ticket) */
  canAct: boolean
  ownerName?: string
  onChanged?: (t: Task) => void
}

export function DecisionCard({ task, doc, canAct, ownerName, onChanged }: DecisionCardProps) {
  const [choice, setChoice] = useState<OptionChoice>(null)
  const [text, setText] = useState('')
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const ask = doc.ask
  const criteria = task.acceptance_criteria?.trim() ?? ''
  const checklist = checklistItems(criteria)
  const blocked = task.state === 'blocked'
  const open = task.state !== 'completed'
  const actionable = canAct && open && !blocked
  const textId = `decision-text-${task.id}`

  const run = async (name: string, fn: () => Promise<Task>) => {
    setBusy(name); setError(null)
    try {
      onChanged?.(await fn())
    } catch (e) {
      setError(e instanceof Error ? e.message : 'That did not work')
    } finally {
      setBusy(null)
    }
  }
  const acceptRecommendation = () => {
    if (!ask) return
    setChoice(ask.recommended)
    setText(ask.options[ask.recommended]?.label ?? '')
    setError(null)
  }
  const markDone = () => {
    if (ask && choice === null) { setError('Pick an option, or Something else'); return }
    if (!text.trim()) { setError('Write what you decided'); return }
    run('done', () => api.completeHumanTask(task.id, { note: decisionNote(doc, choice, text) }))
  }

  return (
    <div className="grid gap-3" data-testid="decision-card">
      <span className="text-[12px] text-ink-3" data-testid="decision-meta">
        Decision doc · drafted by {doc.author_kind === 'agent' ? 'an agent' : 'a person'} · v{doc.current_version}
        {' · '}<a href={`/artifacts/${doc.artifact_id}`} className="underline underline-offset-2 hover:text-ink">open the doc</a>
      </span>

      {ask ? <>
        <div className="grid gap-1">
          <span className="amp-label">Question</span>
          <p className="m-0 text-[14px] leading-relaxed text-ink" data-testid="decision-question">{ask.question}</p>
        </div>
        <div className="grid gap-1.5">
          <span className="amp-label">Options</span>
          <OptionPicker ask={ask} value={choice} onChange={v => { setChoice(v); setError(null) }} disabled={!actionable || !!busy} name={`decision-${task.id}`} />
        </div>
      </> : (
        <p className="m-0 text-[13px] text-warning-ink" role="note">This doc's version has no options; write the decision below.</p>
      )}

      {doc.text.trim() && (
        <div className="max-h-[32vh] overflow-y-auto pr-1 border-l-2 pl-3" style={{ borderColor: 'var(--line-str)' }} data-testid="decision-doc">
          <Markdown size="compact">{doc.text}</Markdown>
        </div>
      )}

      {criteria && (
        <div>
          <div className="amp-label mb-1.5">Decided means</div>
          {checklist ? (
            <ul className="m-0 pl-4 grid gap-1 text-[13px] text-ink" aria-label="Decided means">
              {checklist.map((item, i) => <li key={i} data-testid="decided-means-item"><Markdown size="compact">{item.text}</Markdown></li>)}
            </ul>
          ) : <Markdown size="compact">{criteria}</Markdown>}
        </div>
      )}

      {!open ? null : blocked ? (
        <span className="text-[12.5px] text-ink-2">Waiting on #{(task.blocked_by_ids?.length ? task.blocked_by_ids : task.dependency_ids).join(', #')} before it can start.</span>
      ) : !canAct ? (
        <span className="text-[12.5px] text-ink-2">Only {ownerName || 'its owner'} decides this.</span>
      ) : (
        <form className="grid gap-2" onSubmit={e => { e.preventDefault(); markDone() }}>
          <label htmlFor={textId} className="amp-label">Your decision</label>
          <textarea id={textId} className="amp-input min-h-[56px] resize-y" value={text} aria-required
            onChange={e => { setText(e.target.value); setError(null) }}
            placeholder="What you decided. The tickets waiting on this receive it" />
          {error && <span role="alert" className="text-[12px] text-danger-ink">{error}</span>}
          <div className="flex gap-2 flex-wrap items-center">
            {task.state === 'backlog' && (
              <button type="button" className="amp-btn amp-btn-sm" disabled={!!busy} onClick={() => run('start', () => api.startHumanTask(task.id))}>
                {busy === 'start' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Play size={12} aria-hidden="true" />}Start</button>
            )}
            {ask && (
              <button type="button" className="amp-btn amp-btn-sm ml-auto" disabled={!!busy} onClick={acceptRecommendation}>
                <Sparkles size={12} aria-hidden="true" />Accept recommendation</button>
            )}
            <button type="submit" className={`amp-btn amp-btn-primary amp-btn-sm${ask ? '' : ' ml-auto'}`} disabled={!!busy}>
              {busy === 'done' ? <Loader2 size={11} className="animate-spin" aria-hidden="true" /> : <Check size={12} aria-hidden="true" />}Mark done</button>
          </div>
        </form>
      )}
    </div>
  )
}
