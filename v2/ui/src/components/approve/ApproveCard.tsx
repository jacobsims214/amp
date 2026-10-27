/**
 * The Approve item in the project's My work list (artifact #24 v2, "Approve
 * card"): one per epic awaiting the person. A summary that opens the full
 * Approve view; nothing is approved from the card itself.
 *
 * - the epic, its ticket count, how long it has waited;
 * - four small readouts: starts right away, steps for you, new or changed
 *   since your last approval, plan check;
 * - one line per story with a bar over the steps it spans;
 * - "Review and approve N", and "Review both plans together" when more than
 *   one epic awaits the person.
 */
import { useMemo, type CSSProperties } from 'react'
import { buildModel, selectionEffect, defaultTicks, awaitingForUser } from '../../lib/approvePlan'
import { ago } from '../../lib/approval'
import type { ApproveSet } from '../../hooks/useApproveParam'
import type { ProjectWaitItem } from '../../types'
import './approve.css'

export interface ApproveCardProps {
  item: ProjectWaitItem
  /** every approve item in the list, for "Review both plans together" */
  allItems?: ProjectWaitItem[]
  onReview: (set: ApproveSet, focus?: number | null) => void
  /** a lead reading someone else's list: the card says whose it is, and the button reads "Review" */
  readOnly?: boolean
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

export function ApproveCard({ item, allItems = [], onReview, readOnly }: ApproveCardProps) {
  const plan = item.plan
  const model = useMemo(() => (plan ? buildModel(plan) : null), [plan])
  const effect = useMemo(() => (plan && model ? selectionEffect(model, defaultTicks(plan), false) : null), [plan, model])
  const count = item.count ?? item.task_ids.length
  const epicId = item.epic_id!
  const others = allItems.filter(i => i.kind === 'approve' && i.epic_id !== epicId)
  const together = others.length > 0 ? allItems.reduce((n, i) => n + (i.count ?? i.task_ids.length), 0) : 0
  const forUser = plan ? awaitingForUser(plan) : item.task_ids
  const first = forUser[0] ?? item.task_ids[0]
  const human = plan?.tickets.filter(t => t.awaiting && t.performer === 'human' && t.yours) ?? []
  const changed = plan ? plan.new_ids.length + plan.changed_ids.length : 0
  const stories = model?.epics.flatMap(e => e.stories) ?? []

  return (
    <article className="apv-card grid gap-3" aria-label={`Approve plan: ${item.epic_name || `epic ${epicId}`}`} data-kind="approve">
      <div className="grid gap-1">
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <span className="apv-kicker amp-label"><i aria-hidden="true" />Approve plan</span>
          <span className="text-[15px] font-semibold text-ink">{item.epic_name || item.name}</span>
        </div>
        <div className="flex flex-wrap gap-x-4 gap-y-1 text-[12.5px] text-ink-2">
          <span><b className="text-ink">{plural(count, 'ticket')}</b> await {readOnly ? 'approval' : 'your approval'}</span>
          <span>Epic <span className="amp-num">{epicId}</span>{stories.length ? ` · ${plural(stories.length, 'story', 'stories')}` : ''}</span>
          {item.since && <span className="amp-num">{ago(item.since)}</span>}
          <span>Agents can't start them until {readOnly ? 'they are approved' : 'you approve'}</span>
        </div>
      </div>
      {plan && model && effect && (
        <div className="grid grid-cols-2 md:grid-cols-4 gap-x-6 gap-y-2 px-3 py-2.5 bg-surface-2 border border-line-sub text-[12.5px]">
          <Readout label="Starts right away" value={effect.startsNow.length} text={effect.startsNow.slice(0, 4).map(i => `#${i}`).join(' ') + (effect.startsNow.length > 4 ? ' …' : '')} />
          <Readout label="Steps for you" value={human.length} violet text={human[0] ? `#${human[0].id} ${human[0].name}${human[0].waits_on.length ? `, after #${human[0].waits_on.join(', #')}` : ''}` : 'none'} />
          <Readout label="Since your last approval" value={changed} text={plan.caller_last_approved_at ? `${plan.new_ids.length} new, ${plan.changed_ids.length} changed` : 'all new'} />
          <Readout label="Plan check" value={plan.issues.length ? plural(plan.issues.length, 'warning') : 'OK'} ok={!plan.issues.length}
            text={`${plural(model.steps, 'step')}, longest chain ${model.critical.size}`} />
        </div>
      )}
      {model && stories.length > 0 && (
        <div className="grid gap-1" style={{ '--steps': Math.max(1, model.steps) } as CSSProperties}>
          {stories.map(s => (
            <div key={s.story.id} className="grid grid-cols-[minmax(0,1fr)_80px_auto_80px] gap-3 items-center text-[12.5px] pl-2.5"
              style={{ borderLeft: `3px solid ${s.color}`, '--sc': s.color } as CSSProperties}>
              <span className="truncate text-ink">{s.story.name}</span>
              <span className="amp-num text-ink-2 text-right">{plural(s.ids.length, 'ticket')}</span>
              <span className="apv-trk span" aria-hidden="true">
                {Array.from({ length: model.steps }, (_, i) => <i key={i} className={i + 1 >= s.firstStep && i + 1 <= s.lastStep ? 'in' : ''} />)}
              </span>
              <span className="amp-num text-ink-3">{s.firstStep === s.lastStep ? `step ${s.firstStep}` : `steps ${s.firstStep}–${s.lastStep}`}</span>
            </div>
          ))}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <button type="button" className="amp-btn apv-approve" onClick={() => onReview([epicId], first)}>
          {readOnly ? 'Review' : `Review and approve ${count}`}</button>
        {others.length > 0 && (
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => onReview('all', first)}>
            Review {others.length === 1 ? 'both plans' : `all ${others.length + 1} plans`} together ({together})</button>
        )}
        <span className="text-[12px] text-ink-3">Read every ticket, see the order, approve all or some.</span>
      </div>
    </article>
  )
}

function Readout({ label, value, text, violet, ok }: { label: string; value: number | string; text: string; violet?: boolean; ok?: boolean }) {
  return (
    <div className="grid gap-1 min-w-0">
      <span className="amp-label flex items-center gap-1.5">{violet && <i className="inline-block w-[8px] h-[8px]" style={{ background: 'var(--violet)' }} aria-hidden="true" />}{label}</span>
      <span className="flex gap-2 items-baseline min-w-0">
        <span className={`amp-num text-[15px] ${ok ? 'text-success-ink' : 'text-ink'}`}>{value}</span>
        <span className="truncate text-ink-2">{text}</span>
      </span>
    </div>
  )
}
