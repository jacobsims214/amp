/**
 * The project's My work tab, `/project/:id/my-work` (KB decisions/human-work-ux.md
 * §2, mockup artifact #23 "Project › My work"; the Approve card and view are
 * artifact #24 v2). The one place a person acts on this project: approve a
 * plan, answer a question, record a merge, start or finish their own ticket.
 *
 * - Readouts: Waiting on you (red above 0), Approve, Answer, Merge, Do, and
 *   Coming up (shown, never counted) or Cleared today.
 * - One list of items, approve → answer → merge → do, oldest first within each
 *   kind (the server's order). Approve items are ApproveCards that open the
 *   Approve view in place (`?approve=`); the rest carry HumanActions.
 * - Side panels: Coming up with what each waits on, Your agents here (links to
 *   the Board), Done today.
 * - Empty: "You're clear in <project>." Done: "You cleared N things today."
 * - `?focus=<ticket>` scrolls to and outlines the item that contains the
 *   ticket (an approval contains all its tickets).
 * - `?user=<id|email>`: a lead reading someone else's list (read-only).
 *
 * Data: GET /api/projects/:id/my-work (KB architecture/waiting-on-you.md).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { ChevronRight, X } from 'lucide-react'
import { api } from '../api/client'
import { ProjectNav } from '../components/ProjectNav'
import { BandLoader } from '../components/brand/BandLoader'
import { ApproveCard } from '../components/approve/ApproveCard'
import { ApproveView, type AlsoAwaiting } from '../components/approve/ApproveView'
import { HumanActions } from '../components/human/HumanActions'
import { useApproveParam } from '../hooks/useApproveParam'
import { useFocusParam } from '../hooks/useFocusParam'
import { useSSE } from '../hooks/useSSE'
import { personName } from '../lib/people'
import { ago } from '../lib/approval'
import { reasonLabel } from '../lib/human'
import { KIND_LABEL } from '../lib/humanActions'
import { projectLink } from '../lib/projectLink'
import { refreshWaitCounts } from '../lib/waitCounts'
import type { DepSummary, DoneItem, Project, ProjectMyWork as Data, ProjectWaitItem, WaitKind } from '../types'
import './projectMyWork.css'

const KINDS: WaitKind[] = ['approve', 'answer', 'merge', 'do']
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`
/** "3h" from "3h ago"; "just now" stays. */
const short = (iso?: string) => ago(iso).replace(/ ago$/, '')
const hm = (iso?: string) => (iso ? new Date(iso).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) : '')

const UNIT: Record<WaitKind, (n: number) => string> = {
  approve: n => (n === 1 ? 'plan' : 'plans'),
  answer: n => (n === 1 ? 'question' : 'questions'),
  merge: n => (n === 1 ? 'PR' : 'PRs'),
  do: n => (n === 1 ? 'your ticket' : 'your tickets'),
}

/** The list in display order (the server already sorts each kind oldest first). */
function itemsOf(d: Data): ProjectWaitItem[] {
  return KINDS.flatMap(k => d.items?.[k] ?? [])
}

/** Does the item contain the ticket? An approval contains every ticket it approves. */
const contains = (it: ProjectWaitItem, id: number) => it.task_id === id || (it.task_ids ?? []).includes(id)

/** "the plan approval", "the question on #2473", … for "Oldest: 3h, …". */
function oldestLabel(it: ProjectWaitItem): string {
  switch (it.kind) {
    case 'approve': return 'the plan approval'
    case 'answer': return `the question on #${it.task_id}`
    case 'merge': return `the merge of #${it.task_id}`
    default: return `#${it.task_id}`
  }
}

function depWord(d: DepSummary): string {
  if (d.state === 'completed') return d.satisfied ? 'done' : d.merge_state === 'abandoned' ? 'PR closed without merging' : 'done, merge needed'
  if (d.state === 'in_progress') return `${d.performer === 'human' ? 'a person' : 'agent'}, in progress`
  if (d.state === 'blocked') return d.block_reason?.startsWith('scheduled:') ? 'scheduled' : d.block_reason ? 'question' : 'waiting'
  return d.performer === 'human' ? 'a person, ready' : 'ready'
}

/** What a Coming up ticket waits on, in words. */
function waitsText(it: ProjectWaitItem): string {
  const parts: string[] = []
  const deps = it.waits_on ?? []
  if (deps.length) {
    const shown = deps.slice(0, 2).map(d => `#${d.id} ${d.name} (${depWord(d)})`)
    parts.push(`After ${shown.join(' and ')}${deps.length > 2 ? ` and ${deps.length - 2} more` : ''}`)
  }
  if (it.awaiting_approval) parts.push('Waits on plan approval')
  if (it.start_at) parts.push(`Scheduled for ${new Date(it.start_at).toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}`)
  return parts.join('. ') || 'Waiting to become ready'
}

function doneText(d: DoneItem): { title: string; sub: string } {
  switch (d.kind) {
    case 'approve':
      return { title: `Approved ${plural(d.count || d.task_ids.length, 'ticket')}: ${d.name}`, sub: d.epic_id ? `Epic ${d.epic_id}` : 'Plan approval' }
    case 'answer':
      return { title: `Answered #${d.task_id}: ${d.name}`, sub: 'Unblocked: the agent picks it up on its next run' }
    case 'merge': {
      if (d.merge_state === 'none') return { title: `Not needed: #${d.task_id} ${d.name}`, sub: 'What waited on it was released' }
      const by = d.merge_source === 'github' ? 'Recorded by GitHub' : d.merge_source === 'ci' ? 'Recorded by CI'
        : d.merge_source === 'ui' ? 'Marked merged in AMP' : d.merge_source ? `Recorded by ${d.merge_source}` : 'Recorded'
      return { title: `Merged #${d.task_id}: ${d.name}`, sub: by }
    }
    default:
      return { title: `Done #${d.task_id}: ${d.name}`, sub: 'Your ticket' }
  }
}

/** A kind's colour square with its label (or, `bare`, the label for screen readers only). */
function KindMark({ kind, label, bare }: { kind: WaitKind; label?: string; bare?: boolean }) {
  return (
    <span className={`pmw-kind k-${kind} amp-label`}><i aria-hidden="true" />
      {bare ? <span className="sr-only">{KIND_LABEL[kind]}</span> : label ?? KIND_LABEL[kind]}</span>
  )
}

export function ProjectMyWork() {
  const { projectId } = useParams()
  const pid = Number(projectId)
  const [params] = useSearchParams()
  const user = params.get('user') || undefined
  const approveParam = useApproveParam()
  const [focusId, setFocus] = useFocusParam()
  // Tagged with what it was loaded for, so another project or person never shows stale items.
  const key = `${pid}:${user ?? ''}`
  const [loaded, setLoaded] = useState<{ key: string; data: Data } | null>(null)
  const data = loaded?.key === key ? loaded.data : null
  const [project, setProject] = useState<Project | null>(null)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(() => {
    return api.projectMyWork(pid, { user })
      .then(d => { setLoaded({ key: `${pid}:${user ?? ''}`, data: d }); setError(null) })
      .catch(e => setError(e instanceof Error ? e.message : 'Could not load My work'))
  }, [pid, user])

  useEffect(() => { load() }, [load])
  useEffect(() => { api.getProject(pid).then(setProject).catch(() => null) }, [pid])

  // Something changed (an action here, an agent elsewhere): reload, and refresh the badges.
  const changed = useCallback(() => { load(); refreshWaitCounts() }, [load])
  const sseTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useSSE(pid, () => {
    if (sseTimer.current) clearTimeout(sseTimer.current)
    sseTimer.current = setTimeout(changed, 1200)
  })
  useEffect(() => () => { if (sseTimer.current) clearTimeout(sseTimer.current) }, [])

  const name = project?.name ?? 'this project'
  const items = useMemo(() => (data ? itemsOf(data) : []), [data])
  const approveItems = items.filter(i => i.kind === 'approve')
  const readOnly = !!data?.read_only
  // Whose list a lead is reading, for the labels screen readers hear.
  const readUser = readOnly ? data?.user_id : undefined
  const [person, setPerson] = useState<{ key: string; name: string } | null>(null)
  useEffect(() => {
    if (readUser == null) return
    const k = `${pid}:${readUser}`
    api.listProjectMembers(pid)
      .then(r => {
        const m = (r.members ?? []).find(x => x.user_id === readUser)
        setPerson({ key: k, name: m ? personName(m) : `User #${readUser}` })
      })
      .catch(() => setPerson({ key: k, name: `User #${readUser}` }))
  }, [pid, readUser])
  const who = readUser == null ? 'you' : person?.key === `${pid}:${readUser}` ? person.name : 'them'

  // The Approve view replaces the list, full width (artifact #24 v2).
  if (approveParam.approve) {
    const set = approveParam.approve
    const also: AlsoAwaiting[] = set === 'all' ? [] : approveItems
      .filter(i => i.epic_id && !set.includes(i.epic_id))
      .map(i => ({ epicId: i.epic_id!, name: i.epic_name || i.name, count: i.count ?? i.task_ids.length }))
    return (
      <div className="flex flex-col min-h-full">
        <ProjectNav projectId={pid} currentView="my-work" project={project} />
        <div className="px-7 pt-5 pb-10 max-md:px-4">
          <ApproveView projectId={pid} projectName={name} set={set} user={user}
            focusId={approveParam.focusId} onFocus={approveParam.focus}
            onBack={() => approveParam.close()} onOpen={approveParam.open} alsoAwaiting={also}
            onApproved={() => changed()} />
        </div>
      </div>
    )
  }

  return (
    <div className="flex flex-col min-h-full">
      <ProjectNav projectId={pid} currentView="my-work" project={project}
        meta={readOnly ? "Someone else's list: what waits on them here. The actions are theirs." : "What in this project waits on you, and what's coming. Do it here."}
        rightSlot={<span className="max-md:hidden"><Link to={projectLink(pid, 'board')} className="amp-btn amp-btn-ghost amp-btn-sm">Everyone's work on the Board</Link></span>} />
      <div className="pmw px-7 pt-5 pb-12 max-md:px-4 max-w-[1240px] grid gap-[22px]">
        {error && (
          <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink flex items-center gap-3" role="alert">
            Couldn't load My work: {error}
            <button type="button" className="amp-btn amp-btn-sm ml-auto" onClick={() => load()}>Try again</button>
          </div>
        )}
        {!data && !error && <div className="py-16 flex justify-center"><BandLoader label="Loading My work" /></div>}
        {data && <Body data={data} items={items} name={name} pid={pid} focusId={focusId} setFocus={setFocus}
          onReview={approveParam.open} onChanged={changed} readOnly={readOnly} who={who} />}
      </div>
    </div>
  )
}

interface BodyProps {
  data: Data
  items: ProjectWaitItem[]
  name: string
  pid: number
  focusId: number | null
  setFocus: (id: number | null) => void
  onReview: ReturnType<typeof useApproveParam>['open']
  onChanged: () => void
  readOnly: boolean
  /** whose list, for screen readers: "you", or the person a lead is reading */
  who: string
}

function Body({ data, items, name, pid, focusId, setFocus, onReview, onChanged, readOnly, who }: BodyProps) {
  const counts = data.counts
  const done = data.done_today ?? []
  const coming = data.coming_up ?? []
  const state: 'busy' | 'empty' | 'done' = counts.waiting > 0 || items.length > 0 ? 'busy' : done.length > 0 ? 'done' : 'empty'

  // ?focus=: the item (or Coming up row) that contains the ticket.
  const focusItem = focusId != null ? items.find(i => contains(i, focusId)) ?? null : null
  const focusComing = focusId != null && !focusItem ? coming.find(i => contains(i, focusId)) ?? null : null
  const focusKey = focusItem?.key ?? focusComing?.key ?? null
  useEffect(() => {
    if (!focusKey) return
    const el = document.querySelector(`[data-item-key="${CSS.escape(focusKey)}"]`)
    el?.scrollIntoView?.({ block: 'center', behavior: 'smooth' })
  }, [focusKey])

  const oldest = items.reduce<ProjectWaitItem | null>((a, b) => (!a || (b.since && b.since < a.since) ? b : a), null)
  const approveTickets = (data.items?.approve ?? []).reduce((n, i) => n + (i.count ?? i.task_ids.length), 0)

  return (
    <>
      <div className="amp-panel amp-panel-top pmw-ro" role="group" aria-label={`Waiting on ${who} in ${name}`}>
        <div className={`ro ${counts.waiting > 0 ? 'alarm' : 'calm'}`}>
          <span className="amp-label">Waiting on {readOnly ? 'them' : 'you'}</span>
          <span className="v">{counts.waiting}</span>
          <span className="s">{counts.waiting > 0 && oldest ? `Oldest: ${short(oldest.since)}, ${oldestLabel(oldest)}` : 'All clear'}</span>
        </div>
        {KINDS.map(k => (
          <div key={k} className={`ro ${counts[k] ? '' : 'zero'}`}>
            <KindMark kind={k} />
            <span className="v">{counts[k]}</span>
            {counts[k] > 0 && <span className="s">{k === 'approve' ? `${UNIT.approve(counts[k])} · ${plural(approveTickets, 'ticket')}` : UNIT[k](counts[k])}</span>}
          </div>
        ))}
        {state === 'done'
          ? <div className="ro"><span className="amp-label">Cleared today</span><span className="v">{done.length}</span></div>
          : <div className="ro muted"><span className="amp-label">Coming up</span><span className="v">{counts.coming_up}</span>{counts.coming_up > 0 && <span className="s">not counted yet</span>}</div>}
      </div>

      {focusId != null && !focusItem && !focusComing && (
        <div className="amp-panel px-4 py-3 text-[13px] text-ink-2 flex flex-wrap items-center gap-x-3 gap-y-1" role="status">
          <span>#{focusId} isn't waiting on {readOnly ? 'them' : 'you'} here{state !== 'busy' ? '' : ' any more'}.</span>
          <Link to={projectLink(pid, 'board', focusId)} className="text-link underline underline-offset-2">Open #{focusId} on the Board</Link>
          <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm ml-auto" onClick={() => setFocus(null)}>Dismiss</button>
        </div>
      )}

      <div className="pmw-layout">
        <section aria-label={state === 'busy' ? `Waiting on ${who}` : state === 'done' ? 'Cleared today' : `Nothing waits on ${who}`} className="grid gap-2 min-w-0">
          {state === 'busy' && <>
            <div className="flex items-center gap-3 mb-1">
              <span className="amp-label whitespace-nowrap">Waiting on {readOnly ? 'them' : 'you'} · oldest first within each kind</span>
              <span className="flex-1 h-px bg-line" />
            </div>
            {items.map((it, i) => (
              <ItemCard key={it.key} it={it} pid={pid} all={items} readOnly={readOnly}
                focused={focusItem?.key === it.key} onDismiss={() => setFocus(null)}
                onReview={onReview} onChanged={onChanged}
                note={it.kind === 'merge' && items[i + 1]?.kind !== 'merge' ? <MergeNote /> : null} />
            ))}
          </>}
          {state === 'empty' && <ClearPanel name={name} readOnly={readOnly} />}
          {state === 'done' && <DoneSummary done={done} name={name} agentsDone={data.agents?.done_today ?? 0} readOnly={readOnly} />}
        </section>

        <aside aria-label="Context" className="pmw-side min-w-0">
          <ComingUp items={coming} pid={pid} focusKey={focusComing?.key ?? null} />
          <Agents pid={pid} a={data.agents} />
          {state === 'busy' && done.length > 0 && (
            <section className="amp-panel" aria-label="Done today">
              <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
                <span className="amp-label">Done today</span><span className="ml-auto amp-num text-[11px] text-ink-3">{done.length}</span>
              </div>
              <DoneList done={done} />
            </section>
          )}
        </aside>
      </div>
    </>
  )
}

function ItemCard({ it, pid, all, readOnly, focused, onDismiss, onReview, onChanged, note }: {
  it: ProjectWaitItem; pid: number; all: ProjectWaitItem[]; readOnly: boolean; focused: boolean
  onDismiss: () => void; onReview: BodyProps['onReview']; onChanged: () => void; note: React.ReactNode
}) {
  const kind = it.kind as WaitKind
  const chip = focused && (
    <span className="pmw-focus-chip">Opened from a link
      <button type="button" aria-label="Dismiss the highlight" onClick={onDismiss}><X size={11} aria-hidden="true" /></button>
    </span>
  )
  const frame = `pmw-item k-${kind}${focused ? ' focus' : ''}`
  if (kind === 'approve') {
    return (
      <div className={frame} data-item-key={it.key} data-focused={focused || undefined}>
        {chip}
        <ApproveCard item={it} allItems={all} onReview={onReview} readOnly={readOnly} />
      </div>
    )
  }
  const t = it.task
  const meta: string[] = []
  if (kind === 'answer') meta.push(`Waiting ${short(it.since)} for an answer`)
  if (kind === 'merge') {
    meta.push(it.merge_state === 'abandoned' ? `PR closed ${ago(it.since)}` : `Done ${ago(it.completed_at ?? it.since)}${t?.agent_id ? ` by ${t.agent_id}` : ''}`)
    const n = it.holding_up?.length ?? 0
    meta.push(n ? `${plural(n, 'ticket')} wait${n === 1 ? 's' : ''} on the merge` : 'Nothing waits on it')
  }
  if (kind === 'do') {
    meta.push(`${it.state === 'in_progress' ? 'Started' : 'Ready'} ${ago(it.since)}`)
    meta.push(reasonLabel(it.human_reason ?? t?.human_reason))
  }
  return (
    <>
      <article className={frame} data-item-key={it.key} data-focused={focused || undefined} aria-label={`${KIND_LABEL[kind]}: #${it.task_id} ${it.name}`}>
        {chip}
        <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1">
          <KindMark kind={kind} />
          <Link to={projectLink(pid, 'board', it.task_id)} className="it-title hover:underline">{it.name}</Link>
          <span className="it-id">#{it.task_id}</span>
          {kind === 'do' && <span className="amp-label border border-line px-1.5 py-1 leading-none">{it.state === 'in_progress' ? 'In progress' : 'Ready'}</span>}
        </div>
        <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-[12.5px] text-ink-2">{meta.map(m => <span key={m}>{m}</span>)}</div>
        <div className="mt-3">
          {t ? <HumanActions task={t} kind={kind} canAct={!readOnly} prUrl={it.pr_url} branch={it.branch} onChanged={() => onChanged()} />
            : <span className="text-[12.5px] text-ink-2">{it.question ?? ''}</span>}
        </div>
      </article>
      {note}
    </>
  )
}

function MergeNote() {
  return (
    <p className="m-0 mb-1 flex gap-2.5 items-start bg-surface-2 border border-line px-3 py-2.5 text-[12.5px] leading-[1.45] text-ink-2">
      <span className="pmw-kind k-merge pt-1"><i aria-hidden="true" /></span>
      <span>A merge is recorded once, by whichever comes first: GitHub's webhook (when this project has one), CI, or <b className="text-ink">Mark merged</b> here.</span>
    </p>
  )
}

function ClearPanel({ name, readOnly }: { name: string; readOnly: boolean }) {
  return (
    <div className="amp-panel pmw-clear">
      <span className="amp-label">Nothing waits on {readOnly ? 'them' : 'you'}</span>
      <h2>{readOnly ? `They're clear in ${name}.` : `You're clear in ${name}.`}</h2>
      <p>When this project needs {readOnly ? 'them' : 'you'}, it shows up here, with the button to do it:</p>
      <div className="flex flex-wrap gap-x-3.5 gap-y-2 mt-1.5">
        <KindMark kind="approve" label="Approve a plan" /><KindMark kind="answer" label="Answer a question" />
        <KindMark kind="merge" label="Merge a PR" /><KindMark kind="do" label="Do a ticket only you can do" />
      </div>
      <p className="text-[12.5px] text-ink-3">The same count shows on this tab, in the sidebar, and in My work across projects.</p>
    </div>
  )
}

function DoneSummary({ done, name, agentsDone, readOnly }: { done: DoneItem[]; name: string; agentsDone: number; readOnly: boolean }) {
  return (
    <>
      <div className="amp-panel pmw-clear">
        <span className="amp-label" style={{ color: 'var(--success-ink)' }}>All clear</span>
        <h2>{readOnly ? 'They' : 'You'} cleared {plural(done.length, 'thing')} in {name} today.</h2>
        <p>{agentsDone > 0 ? `Your agents finished ${plural(agentsDone, 'ticket')} here today. ` : ''}Anything new that needs {readOnly ? 'them' : 'you'} shows up here.</p>
      </div>
      <div className="flex items-center gap-3 mt-3.5 mb-1">
        <span className="amp-label whitespace-nowrap">Cleared today</span><span className="flex-1 h-px bg-line" />
      </div>
      <div className="amp-panel"><DoneList done={done} /></div>
    </>
  )
}

function DoneList({ done }: { done: DoneItem[] }) {
  return (
    <ul className="pmw-list">
      {done.map((d, i) => {
        const { title, sub } = doneText(d)
        return (
          <li key={`${d.kind}:${d.task_id ?? d.epic_id}:${i}`}>
            <div className="pmw-row">
              <KindMark kind={d.kind} bare />
              <span className="t">{title}</span>
              <span className="amp-num text-[11.5px] text-ink-3">{hm(d.at)}</span>
              <span className="s">{sub}</span>
            </div>
          </li>
        )
      })}
    </ul>
  )
}

function ComingUp({ items, pid, focusKey }: { items: ProjectWaitItem[]; pid: number; focusKey: string | null }) {
  return (
    <section className="amp-panel" aria-label="Coming up">
      <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
        <span className="amp-label">Coming up</span>
        <span className="ml-auto amp-num text-[11px] text-ink-3">{items.length ? `${items.length} · not counted yet` : '0'}</span>
      </div>
      {items.length === 0
        ? <p className="m-0 px-4 py-3.5 text-[13px] text-ink-2">None of your tickets are waiting on other work.</p>
        : (
          <ul className="pmw-list">
            {items.map(it => (
              <li key={it.key}>
                <Link to={projectLink(pid, 'board', it.task_id)} className={`pmw-row${focusKey === it.key ? ' focus' : ''}`} data-item-key={it.key}>
                  <span className="pmw-dot hollow" aria-hidden="true" />
                  <span className="t">{it.name}</span>
                  <span className="amp-num text-[12px] text-ink-3">#{it.task_id}</span>
                  <span className="s">{waitsText(it)}</span>
                </Link>
              </li>
            ))}
          </ul>
        )}
    </section>
  )
}

function Agents({ pid, a }: { pid: number; a?: Data['agents'] }) {
  const board = projectLink(pid, 'board')
  const none = !a || a.in_progress + a.ready + a.waiting + a.done_today === 0
  return (
    <section className="amp-panel" aria-label="Your agents here">
      <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
        <span className="amp-label">Your agents here</span><span className="ml-auto amp-num text-[11px] text-ink-3">informational</span>
      </div>
      {none ? <p className="m-0 px-4 py-3.5 text-[13px] text-ink-2">No agent tickets of yours in this project.</p> : (
        <ul className="pmw-list">
          <li>
            <Link to={board} className="pmw-row">
              <span className="pmw-dot" style={{ background: 'var(--st-prog)' }} aria-hidden="true" />
              <span className="t">{a!.in_progress} in progress</span>
              <ChevronRight size={14} className="text-ink-3 self-center" aria-hidden="true" />
              <span className="s">{a!.done_today ? `${a!.done_today} done today` : 'None done yet today'}</span>
            </Link>
          </li>
          <li>
            <Link to={board} className="pmw-row">
              <span className="pmw-dot" style={{ background: 'var(--st-ready)', opacity: 0.3 }} aria-hidden="true" />
              <span className="t">{a!.ready} ready, {a!.waiting} waiting</span>
              <ChevronRight size={14} className="text-ink-3 self-center" aria-hidden="true" />
              <span className="s">On the Board, filtered to Mine</span>
            </Link>
          </li>
        </ul>
      )}
    </section>
  )
}
