/**
 * The Approve view (design #2494, artifact #24 v2; KB
 * decisions/human-work-ux.md §2 "Approve view"). Full width inside the
 * project's My work tab at `?approve=<epic id>[,<epic id>…]&focus=<ticket>`.
 * This is the only place a plan is approved.
 *
 * Top to bottom: the header, what approving does (four readouts and plan
 * check warnings), the plan in run order (PlanOutline) beside the ticket
 * reader (TicketReader), and the sticky approval bar. The click sends the
 * existing POST /api/projects/:id/approve-plan {task_ids, as_of, on_behalf};
 * anything planned after as_of comes back skipped ("changed while you were
 * reading").
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowLeft, Loader2, ShieldCheck, TriangleAlert } from 'lucide-react'
import { api } from '../../api/client'
import { buildModel, canTick, defaultTicks, selectionEffect, heldIds, awaitingForUser, type PlanModel } from '../../lib/approvePlan'
import { projectLink } from '../../lib/projectLink'
import type { ApproveSet } from '../../hooks/useApproveParam'
import type { ApprovalPlan, ApproveResult } from '../../types'
import { PlanOutline } from './PlanOutline'
import { TicketReader } from './TicketReader'
import './approve.css'

export interface AlsoAwaiting {
  epicId: number
  name: string
  count: number
}

export interface ApproveViewProps {
  projectId: number
  projectName: string
  set: ApproveSet
  /** the reader's ticket (?focus=) */
  focusId: number | null
  onFocus: (id: number | null, opts?: { replace?: boolean }) => void
  /** "← My work" (and after an approval, Back to My work) */
  onBack: () => void
  /** open another set ("Review both plans together", "Review the 2 changes") */
  onOpen?: (set: ApproveSet, focus?: number | null) => void
  /** other epics awaiting the person, for "Also awaiting … review both together" */
  alsoAwaiting?: AlsoAwaiting[]
  /** a lead reading someone else's list: whose (passed to the API as ?user=) */
  user?: string
  /** read-only: nothing can be approved here (someone else's list without on-behalf rights) */
  readOnly?: boolean
  /** called after a successful approval, so the page refreshes its counts */
  onApproved?: (result: ApproveResult) => void
  /** for tests: a fixed plan instead of fetching */
  initialPlan?: ApprovalPlan
}

type Outcome = { result: ApproveResult; ticked: number[]; startsNow: number[]; yourSteps: number[]; at: string; asOf?: string; model: PlanModel; plan: ApprovalPlan }

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`
const ids = (xs: number[]) => xs.map(x => `#${x}`).join(', ')
const hm = (iso?: string) => (iso ? new Date(iso).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) : '')
const hms = (iso?: string) => (iso ? new Date(iso).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '')
/** "12:41" today, "Oct 26 19:05" otherwise. */
function when(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return d.toDateString() === new Date().toDateString() ? hm(iso) : d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}
/** approve-plan's reason for a ticket skipped because it changed after as_of. */
const isStale = (reason: string) => /as_of|planned after|changed/i.test(reason)

function useNarrow(query = '(max-width: 980px)') {
  const get = () => typeof window !== 'undefined' && typeof window.matchMedia === 'function' && window.matchMedia(query).matches
  const [narrow, setNarrow] = useState(get)
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const m = window.matchMedia(query)
    const on = () => setNarrow(m.matches)
    m.addEventListener?.('change', on)
    return () => m.removeEventListener?.('change', on)
  }, [query])
  return narrow
}

export function ApproveView(props: ApproveViewProps) {
  const { projectId, set, user, initialPlan } = props
  const [plan, setPlan] = useState<ApprovalPlan | null>(initialPlan ?? null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [reload, setReload] = useState(0)
  const key = `${projectId}:${set === 'all' ? 'all' : set.join(',')}:${user ?? ''}:${reload}`
  useEffect(() => {
    if (initialPlan && reload === 0) return
    let live = true
    api.approvalPlan(projectId, set, { user })
      .then(p => { if (live) setPlan(p) })
      .catch(e => { if (live) setLoadError(e instanceof Error ? e.message : 'Could not load the plan') })
    return () => { live = false }
  }, [key]) // eslint-disable-line react-hooks/exhaustive-deps

  if (loadError) {
    return (
      <div className="apv">
        <BackLink onBack={props.onBack} />
        <div className="apv-result bad" role="alert"><h2>Couldn't load the plan</h2><p>{loadError}</p>
          <div><button type="button" className="amp-btn amp-btn-sm" onClick={() => { setLoadError(null); setReload(r => r + 1) }}>Try again</button></div></div>
      </div>
    )
  }
  if (!plan) {
    return <div className="apv"><BackLink onBack={props.onBack} /><p className="flex items-center gap-2 text-ink-2 text-[13px]"><Loader2 size={14} className="animate-spin" />Loading the plan…</p></div>
  }
  return <Loaded {...props} plan={plan} key={key + (plan.as_of ?? '')} onReload={() => setReload(r => r + 1)} />
}

function BackLink({ onBack }: { onBack: () => void }) {
  return <div><button type="button" className="amp-btn amp-btn-ghost amp-btn-sm apv-back" onClick={onBack}><ArrowLeft size={13} aria-hidden="true" />My work</button></div>
}

function Loaded(props: ApproveViewProps & { plan: ApprovalPlan; onReload: () => void }) {
  const { plan, projectId, focusId, onFocus } = props
  const model = useMemo(() => buildModel(plan), [plan])
  const canOnBehalf = plan.can_approve_on_behalf && !props.readOnly
  const [ticks, setTicks] = useState<Set<number>>(() => props.readOnly ? new Set() : defaultTicks(plan))
  const [collapsed, setCollapsed] = useState<Set<number>>(() => new Set(model.epics.flatMap(e => e.stories).filter(s => s.allApproved).map(s => s.story.id)))
  const [filter, setFilter] = useState('')
  const [showCritical, setShowCritical] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [outcome, setOutcome] = useState<Outcome | null>(null)
  const [sheetOpen, setSheetOpen] = useState(false)
  const narrow = useNarrow()
  const effect = useMemo(() => selectionEffect(model, ticks, canOnBehalf), [model, ticks, canOnBehalf])

  const firstAwaiting = model.order.find(id => model.byId.get(id)!.awaiting && model.byId.get(id)!.yours) ?? model.order[0]
  const selected = focusId != null && model.byId.has(focusId) ? focusId : (narrow ? null : firstAwaiting ?? null)

  const select = useCallback((id: number, opts?: { replace?: boolean }) => {
    onFocus(id, opts)
    if (narrow) setSheetOpen(true)
    // Opening a ticket inside a collapsed story expands it.
    const s = model.storyOf.get(id)
    if (s) setCollapsed(c => { if (!c.has(s.story.id)) return c; const n = new Set(c); n.delete(s.story.id); return n })
  }, [onFocus, narrow, model])
  const step = useCallback((dir: 1 | -1) => {
    const i = selected == null ? -1 : model.order.indexOf(selected)
    const next = model.order[Math.min(model.order.length - 1, Math.max(0, i + dir))]
    if (next != null) select(next, { replace: true })
  }, [model, selected, select])
  const tick = useCallback((list: number[], on: boolean) => {
    setTicks(prev => {
      const n = new Set(prev)
      for (const id of list) {
        const t = model.byId.get(id)
        if (!t || !canTick(t, canOnBehalf)) continue
        if (on) n.add(id); else n.delete(id)
      }
      return n
    })
  }, [model, canOnBehalf])

  // Keys: J/K next and previous in outline order, X ticks, Esc closes the sheet.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null
      if (e.metaKey || e.ctrlKey || e.altKey || el?.closest?.('input, textarea, select, [contenteditable="true"]')) return
      const k = e.key.toLowerCase()
      if (k === 'j') { e.preventDefault(); step(1) }
      else if (k === 'k') { e.preventDefault(); step(-1) }
      else if (k === 'x' && selected != null) { e.preventDefault(); tick([selected], !ticks.has(selected)) }
      else if (k === 'escape' && sheetOpen) setSheetOpen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [step, tick, selected, ticks, sheetOpen])

  const tickable = plan.tickets.filter(t => canTick(t, canOnBehalf)).map(t => t.id)
  const forUser = awaitingForUser(plan)
  const epics = model.epics.map(e => e.epic)
  const multi = epics.length > 1

  const approve = async () => {
    const list = model.order.filter(id => ticks.has(id))
    if (!list.length) return
    setBusy(true); setError(null)
    try {
      const onBehalf = list.some(id => !model.byId.get(id)!.yours)
      const result = await api.approvePlan(projectId, { taskIds: list, asOf: plan.as_of, onBehalf })
      setOutcome({ result, ticked: list, startsNow: effect.startsNow.filter(id => result.approved.includes(id)),
        yourSteps: effect.yourSteps.filter(id => result.approved.includes(id)), at: new Date().toISOString(), asOf: plan.as_of, model, plan })
      props.onApproved?.(result)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Approval failed')
    } finally {
      setBusy(false)
    }
  }

  if (outcome) return <Result {...props} outcome={outcome} onReview={id => { props.onFocus(id); props.onReload() }} />
  if (forUser.length === 0 && plan.awaiting_ids.length === 0) {
    return <NothingToApprove {...props} />
  }

  const awaitingCount = plan.awaiting_ids.length
  const allYours = plan.awaiting_ids.every(id => model.byId.get(id)?.yours)
  const lastApproved = plan.caller_last_approved_at
  const storiesN = model.epics.reduce((n, e) => n + e.stories.length, 0)
  const someApproved = plan.tickets.length > awaitingCount
  const doneStories = plan.done_stories ?? []
  const checks = plan.issues ?? []
  const humanReadout = plan.tickets.filter(t => t.awaiting && t.performer === 'human' && t.yours)

  const leaveOut = heldIds(effect)
  const startsLine = [
    `${effect.startsNow.length} start right away`,
    effect.yourSteps.length ? `${plural(effect.yourSteps.length, 'step')} for you (${ids(effect.yourSteps)})` : null,
    effect.waitEarlier.length ? `${effect.waitEarlier.length} wait on earlier tickets` : null,
  ].filter(Boolean).join(' · ')
  const onBehalfN = [...ticks].filter(id => !model.byId.get(id)?.yours).length

  return (
    <div className="apv">
      <BackLink onBack={props.onBack} />
      <header>
        <div className="apv-kicker amp-label"><i aria-hidden="true" />Approve plan · {multi ? `${epics.length} epics: ${epics.map(e => e.id).join(' + ')}` : <>Epic <span className="amp-num">{epics[0]?.id}</span></>}</div>
        <h1>{multi ? `Everything awaiting your approval in ${props.projectName}` : epics[0]?.name}</h1>
        <div className="apv-sub">
          <span>{someApproved && lastApproved
            ? <><b className="text-ink">{awaitingCount} of {plan.tickets_total}</b> tickets await you; you approved the other {plan.tickets_total - awaitingCount} at {when(lastApproved)}</>
            : <><b className="text-ink">{plural(awaitingCount, 'ticket')}</b> in {multi ? `${epics.length} epics, ` : ''}{plural(storiesN, 'story', 'stories')} await your approval</>}</span>
          {plan.as_of && <span>Planned up to {when(plan.as_of)}</span>}
          <span>{allYours ? 'All of them are yours' : `${plan.awaiting_ids.filter(id => !model.byId.get(id)?.yours).length} are other people's`}</span>
          {doneStories.map(s => <span key={s.id} className="text-ink-3">Story {s.id} ({s.name}, {plural(s.task_ids.length, 'ticket')}) is done.</span>)}
        </div>
        {props.alsoAwaiting && props.alsoAwaiting.length > 0 && props.onOpen && (
          <p className="m-0 mt-2 text-[12.5px] text-right">
            {props.alsoAwaiting.map(a => (
              <button key={a.epicId} type="button" className="apv-link" onClick={() => props.onOpen!([...(props.set === "all" ? [] : props.set), a.epicId])}>
                Also awaiting: {a.name} ({a.count}) · review both together ›</button>
            ))}
          </p>
        )}
        {plan.more && <p className="m-0 mt-2 text-[12.5px] text-warning-ink">Showing {plan.tickets.length} of {plan.tickets_total} tickets. Approve these first; the rest load after.</p>}
      </header>

      <section className="apv-impact" aria-label="What approving does">
        <div className="apv-ro">
          <span className="amp-label">Starts right away</span>
          <span className="v">{effect.startsNow.length}</span>
          <span className="s">{effect.startsNow.length ? <>Agents can pick these up as soon as you approve: <Ids list={effect.startsNow} onSelect={select} /></> : 'Nothing starts at once: everything waits on earlier work.'}</span>
        </div>
        <div className="apv-ro">
          <span className="amp-label flex items-center gap-1.5"><i className="inline-block w-[9px] h-[9px]" style={{ background: 'var(--violet)' }} aria-hidden="true" />Steps for you</span>
          <span className="v">{humanReadout.length}</span>
          <span className="s">{humanReadout.length ? humanReadout.map(t => (
            <span key={t.id} className="block"><button type="button" className="apv-link" onClick={() => select(t.id)}>#{t.id}</button> {t.name}{t.waits_on.length ? <> · after <Ids list={t.waits_on} onSelect={select} /></> : ''}</span>
          )) : 'None of these tickets is yours to do.'}</span>
        </div>
        <div className={`apv-ro${plan.new_ids.length + plan.changed_ids.length ? ' chg' : ''}`}>
          <span className="amp-label">Since your last approval</span>
          <span className="v">{plan.new_ids.length + plan.changed_ids.length}</span>
          <span className="s">{!lastApproved ? <>All new. You haven't approved any of {multi ? 'these epics' : 'this epic'} yet.</> : <>
            {plan.new_ids.length > 0 && <span className="block">{plan.new_ids.length} new: <Ids list={plan.new_ids} onSelect={select} /></span>}
            {plan.changed_ids.length > 0 && <span className="block">{plan.changed_ids.length} changed: <Ids list={plan.changed_ids} onSelect={select} /></span>}
            {plan.new_ids.length + plan.changed_ids.length === 0 && <>Nothing new since {when(lastApproved)}.</>}
          </>}</span>
        </div>
        <div className={`apv-ro ${checks.length ? 'warn' : 'ok'}`}>
          <span className="amp-label">Plan check</span>
          <span className="v">{checks.length ? plural(checks.length, 'warning') : 'OK'}</span>
          <span className="s">{plural(model.steps, 'step')}; the longest chain is {plural(model.critical.size, 'ticket')}.{checks.length ? '' : ' Every ticket is connected.'}</span>
        </div>
      </section>

      {checks.length > 0 && (
        <div className="apv-checks" aria-label="Plan check warnings">
          {checks.map((c, i) => (
            <div key={i} className="apv-check"><TriangleAlert size={13} className="text-warning-ink flex-none" aria-hidden="true" />
              <span>{c.message}{c.task_ids?.length ? <> <Ids list={c.task_ids} onSelect={id => model.byId.has(id) && select(id)} /></> : null}{c.fix ? <span className="text-ink-3"> {c.fix}</span> : null}</span></div>
          ))}
        </div>
      )}

      <div className="apv-grid">
        <section className="apv-panel" aria-label="The plan">
          <div className="apv-panel-h">
            <h2>The plan, in run order</h2>
            {plan.tickets.length > 25 && (
              <input className="amp-input apv-find" type="search" placeholder="Find a ticket (title or #)" aria-label="Find a ticket" value={filter} onChange={e => setFilter(e.target.value)} />
            )}
            <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => setCollapsed(new Set())}>Expand all</button>
            <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => setCollapsed(new Set(model.epics.flatMap(e => e.stories.map(s => s.story.id))))}>Collapse all</button>
          </div>
          <div className="apv-legend">
            <span className="opt"><i className="apv-sw" />Step: when a ticket can start. Same step = runs in parallel</span>
            <span className="opt"><label className="inline-flex items-center gap-1.5 cursor-pointer"><input type="checkbox" checked={showCritical} onChange={e => setShowCritical(e.target.checked)} /><i className="apv-sw c" />Critical path</label></span>
            <span className="opt"><i className="apv-sw h" />Your step</span>
            <span><i className="apv-sw out" />Waits on another story or outside</span>
            {effect.leftOut.size > 0 && <span><i className="apv-sw o" />Left out</span>}
            {effect.held.size > 0 && <span><i className="apv-sw held" />Held</span>}
          </div>
          <PlanOutline model={model} ticks={ticks} effect={effect} canOnBehalf={canOnBehalf} selected={selected} collapsed={collapsed} filter={filter}
            showCritical={showCritical} showNew={!!lastApproved} multiEpic={multi} onSelect={select} onTick={tick} formatTime={when}
            onToggleStory={sid => setCollapsed(c => { const n = new Set(c); if (n.has(sid)) n.delete(sid); else n.add(sid); return n })} />
        </section>
        {!narrow && selected != null && (
          <TicketReader projectId={projectId} model={model} id={selected} ticks={ticks} effect={effect} canOnBehalf={canOnBehalf} showCritical={showCritical} showNew={!!lastApproved}
            onSelect={select} onStep={step} onTick={tick} formatTime={when} />
        )}
      </div>
      {narrow && sheetOpen && selected != null && (
        <TicketReader sheet projectId={projectId} model={model} id={selected} ticks={ticks} effect={effect} canOnBehalf={canOnBehalf} showCritical={showCritical} showNew={!!lastApproved}
          onSelect={select} onStep={step} onTick={tick} formatTime={when} onClose={() => setSheetOpen(false)} />
      )}

      {!props.readOnly && (
        <div className="apv-bar" role="region" aria-label="Approval">
          <div className="sum">
            <b>Approve {ticks.size} of {tickable.length} tickets</b>
            <span>{startsLine}</span>
            {effect.leftOut.size > 0 && (
              <span className="warnline">Leaving out {effect.leftOut.size} ({ids([...effect.leftOut].sort((a, b) => a - b))}).
                {leaveOut.length > 0 && <> {plural(leaveOut.length, 'ticket')} you approve will wait on them: {ids(leaveOut)}.</>}</span>
            )}
            {plan.as_of && <span className="asof">Approves these tickets as shown here, planned up to {hms(plan.as_of)}. A ticket edited after that keeps waiting for you.</span>}
            {error && <span role="alert" className="text-danger-ink">{error}</span>}
          </div>
          <div className="btns">
            {leaveOut.length > 0 && <button type="button" className="amp-btn amp-btn-sm" onClick={() => tick(leaveOut, false)}>Leave {leaveOut.length === 1 ? 'it' : `those ${leaveOut.length}`} out too</button>}
            <span className="selbtns flex gap-0.5">
              <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => setTicks(new Set(plan.tickets.filter(t => t.awaiting && t.yours).map(t => t.id)))}>Select all</button>
              <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => setTicks(new Set())}>None</button>
            </span>
            <button type="button" className="amp-btn apv-approve" disabled={busy || ticks.size === 0} onClick={approve}>
              {busy && <Loader2 size={12} className="animate-spin" aria-hidden="true" />}
              {ticks.size === 0 ? 'Select tickets to approve' : `Approve ${plural(ticks.size, 'ticket')}${onBehalfN ? ` (${onBehalfN} on behalf)` : ''}`}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

function Ids({ list, onSelect }: { list: number[]; onSelect: (id: number) => void }) {
  return <span className="apv-ids">{list.map(id => <button key={id} type="button" className="apv-link amp-num" onClick={() => onSelect(id)}>#{id}</button>)}</span>
}

function NothingToApprove(props: ApproveViewProps & { plan: ApprovalPlan }) {
  return (
    <div className="apv">
      <BackLink onBack={props.onBack} />
      <div className="apv-result good">
        <div className="apv-kicker amp-label"><i aria-hidden="true" />Approve plan</div>
        <h2>Nothing awaits your approval in {props.projectName}</h2>
        <p>{props.plan.caller_last_approved_at ? <>Your last approval here was at {when(props.plan.caller_last_approved_at)}.</> : 'Every open ticket in this plan is approved.'}</p>
        <div className="flex gap-2"><button type="button" className="amp-btn amp-btn-primary amp-btn-sm" onClick={props.onBack}>Back to My work</button></div>
      </div>
    </div>
  )
}

function Result(props: ApproveViewProps & { outcome: Outcome; onReview: (focus: number) => void }) {
  const { result, model, plan, ticked, startsNow, yourSteps } = props.outcome
  const stale = Object.entries(result.skipped).filter(([, r]) => isStale(r)).map(([id]) => Number(id))
  const other = Object.entries(result.skipped).filter(([, r]) => !isStale(r))
  const approved = result.approved
  const epicName = model.epics.length === 1 ? model.epics[0].epic.name : `${model.epics.length} epics`
  const lastStep = Math.max(1, ...approved.map(id => model.step.get(id) ?? 1))
  const follow = approved.length - startsNow.length
  const name = (id: number) => model.byId.get(id)?.name ?? ''
  const rows = (
    <>
      {startsNow.length > 0 && <div><span className="amp-label">Starting now</span><span><Ids list={startsNow} onSelect={id => props.onFocus(id)} />: your manager agent can dispatch {startsNow.length === 1 ? 'it' : 'these'} now.</span></div>}
      {follow > 0 && <div><span className="amp-label">Then</span><span>{follow} follow over {plural(Math.max(1, lastStep - 1), 'more step')} as their dependencies finish.{ticked.length === plan.awaiting_ids.length && !stale.length ? ' Nothing else needs your approval.' : ''}</span></div>}
      {yourSteps.map(id => <div key={id}><span className="amp-label" style={{ color: 'var(--violet)' }}>Your step</span><span>#{id} {name(id)}. It's under Coming up and moves to Do when it's ready.</span></div>)}
      <div><span className="amp-label">Recorded</span><span>Approved by you at {hm(props.outcome.at)}{props.outcome.asOf ? `, as of ${hms(props.outcome.asOf)}` : ''}. In each ticket's history and the audit log.</span></div>
    </>
  )
  if (stale.length > 0 || other.length > 0) {
    return (
      <div className="apv">
        <BackLink onBack={props.onBack} />
        <div className="apv-result bad" role="status">
          <div className="apv-kicker amp-label"><i aria-hidden="true" />Approve plan · partly done</div>
          <h2>{approved.length} approved.{stale.length ? ` ${stale.length} changed while you were reading, so ${stale.length === 1 ? 'it still waits' : 'they still wait'} for you.` : ` ${other.length} weren't approved.`}</h2>
          {stale.length > 0 && <p>AMP approves only what you saw. {stale.length === 1 ? 'This ticket was' : 'These tickets were'} planned or edited after {hms(props.outcome.asOf)}, when the plan you read was made.</p>}
          <div className="rows">
            {stale.map(id => <div key={id}><span className="amp-label">Changed</span><span>#{id} {name(id)}</span></div>)}
            {other.map(([id, why]) => <div key={id}><span className="amp-label">Not approved</span><span>#{id} {name(Number(id))}: {why}</span></div>)}
            {approved.length > 0 && <div><span className="amp-label">Approved</span><span>{approved.length === 1 ? 'The other one.' : `The other ${approved.length}.`} {startsNow.length > 0 && <><Ids list={startsNow} onSelect={id => props.onFocus(id)} /> can start now.</>}</span></div>}
          </div>
          <div className="flex gap-2 flex-wrap">
            {stale.length > 0 && (
              <button type="button" className="amp-btn apv-approve amp-btn-sm" onClick={() => props.onReview(stale[0])}>Review the {plural(stale.length, 'change')}</button>
            )}
            <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={props.onBack}>Back to My work</button>
          </div>
        </div>
      </div>
    )
  }
  return (
    <div className="apv">
      <BackLink onBack={props.onBack} />
      <div className="apv-result good" role="status">
        <div className="apv-kicker amp-label"><i aria-hidden="true" />Approve plan · done</div>
        <h2><ShieldCheck size={18} className="inline -mt-1 mr-1 text-success-ink" aria-hidden="true" />You approved {plural(approved.length, 'ticket')} in {epicName}</h2>
        <div className="rows">{rows}</div>
        <div className="flex gap-2 flex-wrap">
          <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" onClick={props.onBack}>Back to My work</button>
          <Link to={projectLink(props.projectId, 'board')} className="amp-btn amp-btn-ghost amp-btn-sm">Watch it on the Board</Link>
        </div>
      </div>
    </div>
  )
}
