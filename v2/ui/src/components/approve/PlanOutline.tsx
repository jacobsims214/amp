/**
 * The plan, in run order (artifact #24 v2): epic > story > ticket, with an
 * aligned step track. Same column = runs in parallel; critical path filled
 * dark; your steps violet. An "after #x" chip appears only where a
 * dependency leaves the story. Selecting a ticket marks the rows that run
 * before it (blue) and after it (dashed). No lines are drawn.
 */
import { useEffect, useRef, type CSSProperties } from 'react'
import { ChevronDown, ChevronRight, Check } from 'lucide-react'
import { afterChips, canTick, chipIds, outsideState, type EpicSection, type PlanModel, type SelectionEffect, type StorySection } from '../../lib/approvePlan'
import type { PlanTicket } from '../../types'

export interface OutlineProps {
  model: PlanModel
  ticks: Set<number>
  effect: SelectionEffect
  canOnBehalf: boolean
  selected: number | null
  collapsed: Set<number>
  /** find box text: rows that don't match are hidden */
  filter: string
  showCritical: boolean
  /** mark New tickets: only once the person has approved something here (otherwise everything is new) */
  showNew: boolean
  multiEpic: boolean
  onSelect: (id: number) => void
  onTick: (ids: number[], on: boolean) => void
  onToggleStory: (storyId: number) => void
  formatTime: (iso?: string) => string
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

function Track({ steps, at, cls }: { steps: number; at: number; cls: string }) {
  return (
    <span className="apv-trk" aria-hidden="true">
      {Array.from({ length: steps }, (_, i) => <i key={i} className={i + 1 === at ? `on ${cls}` : ''} />)}
    </span>
  )
}

function SpanTrack({ steps, from, to }: { steps: number; from: number; to: number }) {
  return (
    <span className="apv-trk span" aria-hidden="true">
      {Array.from({ length: steps }, (_, i) => <i key={i} className={i + 1 >= from && i + 1 <= to ? 'in' : ''} />)}
    </span>
  )
}

/** A checkbox that can show "some ticked". */
function TriBox({ state, disabled, label, onChange }: { state: 'all' | 'some' | 'none'; disabled: boolean; label: string; onChange: (on: boolean) => void }) {
  const ref = useRef<HTMLInputElement>(null)
  useEffect(() => { if (ref.current) ref.current.indeterminate = state === 'some' }, [state])
  return <input ref={ref} type="checkbox" checked={state === 'all'} disabled={disabled} aria-label={label}
    aria-checked={state === 'some' ? 'mixed' : state === 'all'} onChange={e => onChange(e.target.checked)} />
}

function boxState(ids: number[], ticks: Set<number>): 'all' | 'some' | 'none' {
  const n = ids.filter(id => ticks.has(id)).length
  return n === 0 ? 'none' : n === ids.length ? 'all' : 'some'
}

function matches(t: PlanTicket, q: string): boolean {
  if (!q) return true
  const s = q.trim().toLowerCase().replace(/^#/, '')
  return String(t.id).startsWith(s) || t.name.toLowerCase().includes(s)
}

export function PlanOutline(p: OutlineProps) {
  const { model, multiEpic } = p
  const sel = p.selected != null ? model.byId.get(p.selected) : undefined
  const ups = new Set(sel?.dependency_ids ?? [])
  const downs = new Set(p.selected != null ? model.dependents.get(p.selected) ?? [] : [])
  const narrow = model.steps > 8
  return (
    <div className={`apv-ol${narrow ? ' narrow' : ''}`} style={{ '--steps': Math.max(1, model.steps) } as CSSProperties} role="tree" aria-label="The plan, in run order">
      <div className="apv-cols" aria-hidden="true">
        <span />
        <span className="amp-label">{multiEpic ? 'Epic › ' : ''}Story › ticket, in run order</span>
        <span className="apv-trk">{Array.from({ length: model.steps }, (_, i) => <b key={i}>{i + 1}</b>)}</span>
      </div>
      {model.epics.map(e => <EpicBlock key={e.epic.id} {...p} section={e} ups={ups} downs={downs} />)}
    </div>
  )
}

function EpicBlock(p: OutlineProps & { section: EpicSection; ups: Set<number>; downs: Set<number> }) {
  const { section: e, model } = p
  const tickable = e.stories.flatMap(s => s.ids).filter(id => canTick(model.byId.get(id)!, p.canOnBehalf))
  return (
    <div role="group" aria-label={`Epic ${e.epic.id}: ${e.epic.name}`}>
      {p.multiEpic && (
        <div className="apv-epic">
          <TriBox state={boxState(tickable, p.ticks)} disabled={tickable.length === 0} label={`Approve all of epic ${e.epic.id}`}
            onChange={on => p.onTick(tickable, on)} />
          <span className="t">
            <span className="apv-kicker amp-label"><i />Epic {e.epic.id}</span>
            <span>{e.epic.name}</span>
            <span className="text-[12px] font-normal text-ink-3">{plural(e.stories.length, 'story', 'stories')} · {plural(e.ticketCount, 'ticket')}</span>
          </span>
          <span />
        </div>
      )}
      {e.stories.map(s => <StoryBlock key={s.story.id} {...p} story={s} />)}
    </div>
  )
}

function StoryBlock(p: OutlineProps & { story: StorySection; ups: Set<number>; downs: Set<number> }) {
  const { story: s, model, ticks, effect } = p
  const tickable = s.ids.filter(id => canTick(model.byId.get(id)!, p.canOnBehalf))
  const open = !p.collapsed.has(s.story.id)
  const visible = s.ids.filter(id => matches(model.byId.get(id)!, p.filter))
  if (p.filter && visible.length === 0) return null
  const leftOut = s.ids.filter(id => effect.leftOut.has(id)).length
  const held = s.ids.filter(id => effect.held.has(id)).length
  const awaiting = s.ids.filter(id => model.byId.get(id)!.awaiting).length
  const steps = s.firstStep === s.lastStep ? `step ${s.firstStep}` : `steps ${s.firstStep}–${s.lastStep}`
  const showRows = open || !!p.filter
  return (
    <div role="group" aria-label={`Story ${s.story.id}: ${s.story.name}`} style={{ '--sc': s.color } as CSSProperties}>
      <div className="apv-story">
        {s.allApproved
          ? <Check size={14} className="text-ink-3" aria-label="All approved" />
          : <TriBox state={boxState(tickable, ticks)} disabled={tickable.length === 0} label={`Approve all of story ${s.story.id}`} onChange={on => p.onTick(tickable, on)} />}
        <span className="t">
          <button type="button" className="sn" aria-expanded={showRows} onClick={() => p.onToggleStory(s.story.id)}>
            {showRows ? <ChevronDown size={13} aria-hidden="true" /> : <ChevronRight size={13} aria-hidden="true" />}{s.story.name}
          </button>
          <span className="sm">
            Story {s.story.id} · {s.allApproved ? `all ${s.ids.length} approved` : awaiting < s.ids.length ? `${awaiting} to review` : plural(s.ids.length, 'ticket')}
            {leftOut > 0 && <span className="o"> · {leftOut} left out</span>}
            {held > 0 && <span className="hd"> · {held} held</span>}
            {' · '}{steps}
          </span>
        </span>
        <SpanTrack steps={model.steps} from={s.firstStep} to={s.lastStep} />
      </div>
      {showRows && visible.map(id => <Row key={id} {...p} id={id} />)}
    </div>
  )
}

function Row(p: OutlineProps & { story: StorySection; id: number; ups: Set<number>; downs: Set<number> }) {
  const { model, id, ticks, effect } = p
  const t = model.byId.get(id)!
  const step = model.step.get(id)!
  const isSel = p.selected === id
  const leftOut = effect.leftOut.has(id) || (t.awaiting && !ticks.has(id) && !canTick(t, p.canOnBehalf))
  const held = effect.held.get(id)
  const chips = afterChips(model, id, effect.leftOut)
  const human = t.performer === 'human'
  const crit = p.showCritical && model.critical.has(id)
  const cls = ['apv-row', isSel && 'sel', p.ups.has(id) && 'up', p.downs.has(id) && 'dn', leftOut && 'out', held && 'held', !t.awaiting && 'ctx'].filter(Boolean).join(' ')
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => { if (isSel) ref.current?.scrollIntoView?.({ block: 'nearest' }) }, [isSel])

  let note: { text: string; cls: string } | null = null
  if (!t.awaiting) {
    note = { text: `${t.state === 'in_progress' ? 'In progress · ' : ''}approved ${p.formatTime(t.approved_at)}`.trim(), cls: t.state === 'in_progress' ? 'prog' : '' }
  } else if (leftOut) {
    note = { text: t.yours || p.canOnBehalf ? 'Left out: stays awaiting' : `Awaits ${t.owner_name || 'its owner'}`, cls: '' }
  } else if (held) {
    note = { text: `Held: waits on ${held.map(x => `#${x}`).join(', ')}`, cls: 'hd' }
  } else if (t.starts_on_approval) {
    note = { text: 'Starts right away', cls: 'go' }
  }
  const role = human ? null : (t.assigned_to || '').replace(/^amp[:-]/, '').replace(/^amp-worker-/, '').replace(/^amp-/, '') || 'agent'
  return (
    <div ref={ref} className={cls} role="treeitem" aria-selected={isSel} data-ticket={id}>
      {t.awaiting
        ? <input type="checkbox" checked={ticks.has(id)} disabled={!canTick(t, p.canOnBehalf)} aria-label={`Approve #${id}`}
            onChange={e => p.onTick([id], e.target.checked)} />
        : <Check size={14} className="done-mark" aria-label="Approved" />}
      <span className="t">
        <span className="nid">#{id}</span>
        <button type="button" className="tt" onClick={() => p.onSelect(id)}>{t.name}</button>
        {t.change === 'new' && p.showNew && <span className="apv-badge">New</span>}
        {t.change === 'changed' && <span className="apv-badge chg">Changed</span>}
        <span className="meta">
          <span className="stepn">Step {step}</span>
          {human ? <span className={`apv-who${t.yours ? ' you' : ''}`}>{t.yours ? 'You do this' : `${t.owner_name || 'A person'} does this`}</span>
            : <span className="apv-who">{role}</span>}
          {p.ups.has(id) && <span className="apv-rel">Runs before #{p.selected}</span>}
          {p.downs.has(id) && <span className="apv-rel dn">Runs after #{p.selected}</span>}
          {chips.map((c, i) => c.story ? (
            <button key={i} type="button" className={`apv-xchip${c.held ? ' held' : ''}`} style={{ '--sc': c.story.color } as CSSProperties}
              title={`After ${c.ids.map(x => `#${x}`).join(', ')} (story ${c.story.story.id})`} onClick={() => p.onSelect(c.ids[0])}>
              <i />after {chipIds(c.ids)}</button>
          ) : (
            <span key={i} className="apv-xchip ext" title={c.outside ? `${c.outside.name}, epic ${c.outside.epic_id}${c.outside.project_code ? `, ${c.outside.project_code}` : ''}` : undefined}>
              <i />after #{c.ids[0]} · outside, {outsideState(c.outside)}</span>
          ))}
          {note && <span className={`nt ${note.cls}`}>{note.text}</span>}
        </span>
      </span>
      <Track steps={model.steps} at={step} cls={human ? 'h' : crit ? 'c' : ''} />
    </div>
  )
}
