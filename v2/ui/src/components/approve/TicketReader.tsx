/**
 * The ticket reader (artifact #24 v2): the selected ticket in full, what
 * including it does, what changed since it was approved (a diff against the
 * snapshot stored at approval), and what it runs after and unlocks. A sticky
 * side panel on desktop; ApproveView puts it in a full-screen sheet on phone.
 */
import type { CSSProperties, ReactNode } from 'react'
import { ChevronDown, ChevronUp, ExternalLink, X, ArrowLeft } from 'lucide-react'
import { Link } from 'react-router-dom'
import { canTick, includeEffect, outsideState, type PlanModel, type SelectionEffect } from '../../lib/approvePlan'
import { changedLines } from '../../lib/textDiff'
import { projectLink } from '../../lib/projectLink'
import { Markdown } from '../ui/Markdown'
import type { PlanTicket } from '../../types'

export interface ReaderProps {
  projectId: number
  model: PlanModel
  id: number
  ticks: Set<number>
  effect: SelectionEffect
  canOnBehalf: boolean
  showCritical: boolean
  onSelect: (id: number) => void
  onStep: (dir: 1 | -1) => void
  onTick: (ids: number[], on: boolean) => void
  formatTime: (iso?: string) => string
  /** phone: the reader is a full-screen sheet with a back button */
  sheet?: boolean
  onClose?: () => void
}

const PRIORITY = ['Low', 'Normal', 'High', 'Critical']

function stateWords(t: PlanTicket): string {
  switch (t.state) {
    case 'in_progress': return 'in progress'
    case 'completed': return 'done'
    case 'blocked': return t.block_reason && !t.block_reason.startsWith('scheduled:') ? 'question' : 'waiting'
    case 'scheduled': return 'scheduled'
    default: return 'ready'
  }
}

export function TicketReader(p: ReaderProps) {
  const { model, id } = p
  const t = model.byId.get(id)
  if (!t) return null
  const pos = model.order.indexOf(id) + 1
  const story = model.storyOf.get(id)
  const effect = includeEffect(t, p.ticks, p.effect)
  const held = p.effect.held.has(id)
  const tickable = canTick(t, p.canOnBehalf)
  const human = t.performer === 'human'
  const deps = [...t.dependency_ids].sort((a, b) => a - b)
  const unlocks = [...(model.dependents.get(id) ?? [])].sort((a, b) => a - b)
  const titleId = `apv-reader-title-${id}`

  const depRow = (d: number, why: string) => {
    const inSet = model.byId.get(d)
    if (inSet) {
      const s = model.storyOf.get(d)
      const bits = [inSet.awaiting ? (p.ticks.has(d) ? 'in this approval' : 'left out') : `approved, ${stateWords(inSet)}`]
      if (p.showCritical && model.critical.has(d)) bits.push('critical path')
      return (
        <li key={`${why}-${d}`} style={{ '--sc': s?.color } as CSSProperties}>
          <i aria-hidden="true" /><span className="amp-num text-[12px] text-ink-2">#{d}</span>
          <button type="button" className="lnk" onClick={() => p.onSelect(d)}>{inSet.name}</button>
          <span className="st">{bits.join(' · ')}</span>
        </li>
      )
    }
    const o = model.outside.get(d)
    return (
      <li key={`${why}-${d}`} style={{ '--sc': 'var(--line-str)' } as CSSProperties}>
        <i aria-hidden="true" /><span className="amp-num text-[12px] text-ink-2">#{d}</span>
        <Link className="lnk" to={projectLink(o?.project_id ?? p.projectId, 'board', d)}>{o?.name ?? `Ticket ${d}`}</Link>
        <span className="st">Outside this approval{o ? `: ${o.project_code ? `${o.project_code} · ` : ''}epic ${o.epic_id}${o.epic_name ? ` (${o.epic_name})` : ''} · ${outsideState(o)}` : ''}</span>
      </li>
    )
  }

  return (
    <section className={p.sheet ? 'apv-sheet' : 'apv-panel apv-reader'} aria-labelledby={titleId} role={p.sheet ? 'dialog' : undefined} aria-modal={p.sheet ? true : undefined}>
      <div className="apv-reader-h">
        {p.sheet && <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={p.onClose}><ArrowLeft size={13} aria-hidden="true" />Plan</button>}
        <span className="amp-num text-[12px] text-ink-2">#{id}</span>
        <span className="amp-label">{pos} of {model.order.length} · step {model.step.get(id)}</span>
        <span className="ml-auto flex gap-1">
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" aria-label="Previous ticket (K)" onClick={() => p.onStep(-1)}><ChevronUp size={14} /></button>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" aria-label="Next ticket (J)" onClick={() => p.onStep(1)}><ChevronDown size={14} /></button>
          {p.sheet && <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" aria-label="Close" onClick={p.onClose}><X size={14} /></button>}
        </span>
      </div>
      <div className="apv-reader-b">
        <h3 id={titleId}>{t.name}</h3>
        <div className="apv-tags">
          {human ? <span className={`apv-tag${t.yours ? ' you' : ''}`}>Person · {t.yours ? 'you do this' : t.owner_name || 'a person'}</span>
            : <span className="apv-tag">Agent · {t.assigned_to?.replace(/^amp:/, '') || 'unassigned'}</span>}
          <span className="apv-tag">Owner · {t.yours ? 'you' : t.owner_name || 'nobody'}</span>
          <span className="apv-tag">Priority {t.priority}{PRIORITY[Number(t.priority)] ? ` · ${PRIORITY[Number(t.priority)]}` : ''}</span>
          {story && <span className="apv-tag" style={{ '--sc': story.color } as CSSProperties}><i aria-hidden="true" />Story {story.story.id}</span>}
          {p.showCritical && model.critical.has(id) && <span className="apv-tag crit">Critical path</span>}
          {t.change === 'new' && <span className="apv-badge">New</span>}
          {t.change === 'changed' && <span className="apv-badge chg">Changed</span>}
        </div>

        <label className={`apv-incl${held ? ' held' : ''}`}>
          {t.awaiting
            ? <input type="checkbox" checked={p.ticks.has(id)} disabled={!tickable} onChange={e => p.onTick([id], e.target.checked)} />
            : null}
          <span>
            {t.awaiting ? 'Include in this approval' : `Approved ${p.formatTime(t.approved_at)}`}
            <small>{t.awaiting && !tickable ? `Awaits ${t.owner_name || 'its owner'}: only they (or a lead, on their behalf) approve it.` : effect}</small>
          </span>
        </label>

        {t.change === 'changed' && <WhatChanged t={t} formatTime={p.formatTime} onSelect={p.onSelect} model={model} />}

        <Section title={`Runs after`} count={deps.length}>
          {deps.length ? <ul className="apv-deps">{deps.map(d => depRow(d, 'after'))}</ul>
            : <p className="m-0 text-[12.5px] text-ink-3">Nothing. It can start first.</p>}
        </Section>
        <Section title="Unlocks" count={unlocks.length}>
          {unlocks.length ? <ul className="apv-deps">{unlocks.map(d => depRow(d, 'unlocks'))}</ul>
            : <p className="m-0 text-[12.5px] text-ink-3">Nothing in this plan waits on it.</p>}
        </Section>
        <Section title="Description">
          {t.description ? <Markdown size="compact">{t.description}</Markdown> : <p className="m-0 text-[12.5px] text-ink-3">No description.</p>}
        </Section>
        <Section title="Acceptance criteria">
          {t.acceptance_criteria ? <Markdown size="compact">{t.acceptance_criteria}</Markdown> : <p className="m-0 text-[12.5px] text-ink-3">None given.</p>}
        </Section>
        <Link to={projectLink(p.projectId, 'board', id)} className="amp-btn amp-btn-ghost amp-btn-sm justify-self-start">
          Open on the Board<ExternalLink size={12} aria-hidden="true" /></Link>
      </div>
    </section>
  )
}

function Section({ title, count, children }: { title: string; count?: number; children: ReactNode }) {
  return (
    <div className="grid gap-2">
      <span className="amp-label">{title}{count !== undefined ? <span className="amp-num ml-2">{count}</span> : null}</span>
      {children}
    </div>
  )
}

/** What changed since it was approved: name, dependencies, then line diffs of the description and acceptance criteria. */
function WhatChanged({ t, model, formatTime, onSelect }: { t: PlanTicket; model: PlanModel; formatTime: (iso?: string) => string; onSelect: (id: number) => void }) {
  const cs = t.changed_since
  const prev = cs?.previous
  const when = formatTime(cs?.last_approved_at ?? t.approved_at)
  if (!prev) {
    return (
      <Section title={`What changed since you approved it${when ? ` at ${when}` : ''}`}>
        <p className="m-0 text-[12.5px] text-ink-2">Edited at {formatTime(cs?.at ?? t.planned_at)}{cs?.by ? ` by ${cs.by}` : ''}. AMP has no copy of the text as it was approved (it was approved before AMP kept one), so there's no diff.</p>
      </Section>
    )
  }
  const added = t.dependency_ids.filter(d => !prev.dependency_ids.includes(d))
  const removed = prev.dependency_ids.filter(d => !t.dependency_ids.includes(d))
  const desc = changedLines(prev.description, t.description)
  const ac = changedLines(prev.acceptance_criteria, t.acceptance_criteria)
  const name = (d: number) => model.byId.get(d)?.name ?? model.outside.get(d)?.name ?? ''
  const nothing = prev.name === t.name && !added.length && !removed.length && !desc.length && !ac.length
  const unchanged = [prev.name === t.name && 'title', !desc.length && 'description', !ac.length && 'acceptance criteria', !added.length && !removed.length && 'dependencies'].filter(Boolean)
  return (
    <Section title={`What changed since you approved it${when ? ` at ${when}` : ''}`}>
      {nothing ? <p className="m-0 text-[12.5px] text-ink-2">Re-planned at {formatTime(cs?.at)}, but its title, text and dependencies are as you approved them.</p> : (
        <div className="apv-diff">
          {prev.name !== t.name && <>
            <div className="del"><span className="lab">Title</span>{prev.name}</div>
            <div className="add"><span className="lab">Title</span>{t.name}</div>
          </>}
          {added.map(d => <div key={`a${d}`} className="add"><span className="lab">Dependency</span>
            <button type="button" className="apv-link" onClick={() => onSelect(d)}>+ #{d}</button> {name(d)}</div>)}
          {removed.map(d => <div key={`r${d}`} className="del"><span className="lab">Dependency</span>− #{d} {name(d)}</div>)}
          {desc.map((l, i) => <div key={`d${i}`} className={l.kind}><span className="lab">{l.kind === 'add' ? 'Added' : 'Removed'}</span>{l.text}</div>)}
          {ac.map((l, i) => <div key={`c${i}`} className={l.kind}><span className="lab">{l.kind === 'add' ? 'Criteria added' : 'Criteria removed'}</span>{l.text}</div>)}
        </div>
      )}
      {!nothing && unchanged.length > 0 && <p className="m-0 text-[11.5px] text-ink-3">{cap(listWords(unchanged as string[]))} unchanged. Edited at {formatTime(cs?.at)}{cs?.by ? ` by ${cs.by}` : ''}.</p>}
    </Section>
  )
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)
function listWords(xs: string[]): string {
  return xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs[xs.length - 1]}`
}
