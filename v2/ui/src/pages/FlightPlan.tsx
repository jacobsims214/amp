/**
 * Flight plan — review a whole plan, however large, on one screen.
 *
 * Columns are stages: stage 1 is what can start with nothing before it, and
 * every later stage waits on the one before (a ticket's stage is its
 * dependency depth). The number of stages is the plan's longest chain.
 * Rows are stories grouped by epic, and every ticket is one labeled chip in
 * its stage, so the shape of the plan shows without drawing any lines.
 * Clicking a ticket lights up everything it waits on and everything waiting
 * on it; the critical path is outlined. AMP's plan check sits beside it as a
 * GO / NO-GO poll.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { GitBranch, Loader2, RefreshCw } from 'lucide-react'
import { api } from '../api/client'
import { useBoardData } from '../hooks/useBoardData'
import { useAuth } from '../hooks/useAuth'
import { useProjectPeople } from '../hooks/useProjectPeople'
import { lookupPerson } from '../lib/people'
import { ProjectNav } from '../components/ProjectNav'
import { TaskDrawer } from '../components/TaskDrawer'
import { Panel, Readouts } from '../components/ui/PageHeader'
import { StateDot, StateTag } from '../components/ui/StateTag'
import { PriorityMark } from '../components/ui/PriorityMark'
import { displayState, STATE_LABEL, STATE_VAR, type DisplayState } from '../lib/ticketState'
import type { PlanIssue, PlanReport, Task } from '../types'
import { BandLoader } from '../components/brand/BandLoader'

/** Each ticket's stage (dependency depth) and the plan's critical path. */
function planShape(tasks: Task[]) {
  const byId = new Map(tasks.map(t => [t.id, t]))
  const depth = new Map<number, number>()
  const visiting = new Set<number>()
  const stageOf = (t: Task): number => {
    const known = depth.get(t.id)
    if (known != null) return known
    if (visiting.has(t.id)) return 0 // a cycle; AMP refuses them, but don't loop
    visiting.add(t.id)
    const deps = (t.dependency_ids ?? []).map(id => byId.get(id)).filter((d): d is Task => !!d)
    const d = deps.length ? 1 + Math.max(...deps.map(stageOf)) : 0
    visiting.delete(t.id)
    depth.set(t.id, d)
    return d
  }
  tasks.forEach(stageOf)
  const stages = tasks.length ? Math.max(...depth.values()) + 1 : 0
  const dependents = new Map<number, number[]>()
  for (const t of tasks) for (const d of t.dependency_ids ?? []) dependents.set(d, [...(dependents.get(d) ?? []), t.id])
  // Critical path: from a deepest ticket back through deps one stage earlier.
  const crit = new Set<number>()
  let cur = tasks.find(t => depth.get(t.id) === stages - 1)
  while (cur) {
    crit.add(cur.id)
    const want = (depth.get(cur.id) ?? 0) - 1
    cur = (cur.dependency_ids ?? []).map(id => byId.get(id)).find(d => d && depth.get(d.id) === want)
  }
  return { depth, stages, dependents, crit, byId }
}

function chain(id: number, byId: Map<number, Task>, dependents: Map<number, number[]>) {
  const up = new Set<number>(), down = new Set<number>()
  const walkUp = (x: number) => (byId.get(x)?.dependency_ids ?? []).forEach(d => { if (!up.has(d)) { up.add(d); walkUp(d) } })
  const walkDown = (x: number) => (dependents.get(x) ?? []).forEach(d => { if (!down.has(d)) { down.add(d); walkDown(d) } })
  walkUp(id); walkDown(id)
  return { up, down }
}

const ISSUE_TITLE: Record<string, string> = {
  isolated: 'Every ticket wired in',
  wide_start: 'Start width',
  empty_stories: 'Every story has tickets',
  no_context: 'Context in the KB',
  coupled_gate: "People's work kept apart",
  metadata_drift: 'Epics carry the same metadata',
}

function Chip({ t, state, crit, lit, sel, dim, onClick }: {
  t: Task; state: DisplayState; crit: boolean; lit: boolean; sel: boolean; dim: boolean; onClick: () => void
}) {
  const filled = state === 'done' || state === 'flag'
  return (
    <button type="button" onClick={onClick} title={`#${t.id} ${t.name} · ${STATE_LABEL[state]}`}
      aria-label={`Ticket ${t.id}: ${t.name}, ${STATE_LABEL[state]}`} aria-pressed={sel}
      className="relative inline-grid place-items-center min-w-[40px] h-[19px] px-[3px] m-[1px] amp-num text-[10px] cursor-pointer transition-opacity"
      style={{
        borderRadius: 'var(--radius)', border: `1px solid ${STATE_VAR[state]}`,
        color: filled ? '#fff' : state === 'ready' ? 'var(--ink)' : STATE_VAR[state],
        background: filled ? STATE_VAR[state] : `color-mix(in srgb, ${STATE_VAR[state]} 9%, var(--surface))`,
        outline: crit ? '1.5px solid var(--accent)' : undefined, outlineOffset: 1,
        boxShadow: sel ? '0 0 0 3px var(--accent)' : undefined,
        opacity: dim && !lit && !sel ? 0.16 : 1,
      }}>
      {t.id}
      {t.priority === '3' && <i className="absolute -top-px -right-px w-0 h-0 border-t-[6px] border-l-[6px] border-l-transparent"
        style={{ borderTopColor: state === 'flag' ? 'var(--ink)' : 'var(--st-flag)' }} aria-hidden="true" />}
    </button>
  )
}

export function FlightPlan() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)
  const { epics, stories, tasks, loading, error, refresh, applyTask } = useBoardData(pid)
  const { members, myAccess, openReassign, people } = useProjectPeople(pid)
  const { me } = useAuth()
  const [focus, setFocus] = useState<number | null>(null)
  const [openId, setOpenId] = useState<number | null>(null)
  const [hideDone, setHideDone] = useState(false)
  const [report, setReport] = useState<PlanReport | null>(null)
  // Bumped on each check, so the GO stamps replay like a fresh poll.
  const [poll, setPoll] = useState(0)
  const [checking, setChecking] = useState(true)
  const [checkError, setCheckError] = useState<string | null>(null)

  const check = useCallback(() => api.planCheck(pid).then(r => { setReport(r); setPoll(n => n + 1); setCheckError(null) })
    .catch(e => setCheckError(e instanceof Error ? e.message : 'Plan check failed'))
    .finally(() => setChecking(false)), [pid])
  useEffect(() => { check() }, [check])
  const runCheck = () => { setChecking(true); check() }
  const [approving, setApproving] = useState(false)
  const [approveError, setApproveError] = useState<string | null>(null)
  const approve = async () => {
    setApproving(true); setApproveError(null)
    try { await api.approvePlan(pid); await check() }
    catch (e) { setApproveError(e instanceof Error ? e.message : 'Could not approve the plan') }
    finally { setApproving(false) }
  }

  const shape = useMemo(() => planShape(tasks), [tasks])
  const lit = useMemo(() => {
    if (focus == null) return null
    const { up, down } = chain(focus, shape.byId, shape.dependents)
    return { up, down, all: new Set([...up, ...down]) }
  }, [focus, shape])
  const focused = focus != null ? shape.byId.get(focus) ?? null : null
  const opened = openId != null ? tasks.find(t => t.id === openId) ?? null : null

  const rows = useMemo(() => epics.slice().sort((a, b) => a.id - b.id).map(e => ({
    epic: e,
    stories: stories.filter(s => s.epic_id === e.id).sort((a, b) => a.id - b.id).map(s => ({
      story: s,
      tasks: tasks.filter(t => t.story_id === s.id && (!hideDone || t.state !== 'completed')),
      total: tasks.filter(t => t.story_id === s.id).length,
    })),
  })), [epics, stories, tasks, hideDone])

  const stageCounts = useMemo(() => {
    const c = Array.from({ length: shape.stages }, () => 0)
    for (const t of tasks) c[shape.depth.get(t.id) ?? 0]++
    return c
  }, [tasks, shape])

  const issueByKind = new Map((report?.issues ?? []).map(i => [i.kind, i]))
  const pollOrder = ['isolated', 'wide_start', 'empty_stories', 'no_context', 'coupled_gate', 'metadata_drift']
  const pollRow = (kind: string, okDetail: string) => {
    const issue: PlanIssue | undefined = issueByKind.get(kind)
    const stamp = pollOrder.indexOf(kind) * 90
    return (
      <div key={kind} className="grid grid-cols-[1fr_auto] gap-x-3 gap-y-1 px-4 py-2.5 border-t border-line first:border-t-0">
        <span className="text-[13px] text-ink">{ISSUE_TITLE[kind]}</span>
        <Verdict key={`${poll}-${kind}`} ok={!issue} warn={issue?.severity === 'warn'} stampDelay={stamp} />
        <span className="col-span-2 text-[11.5px] text-ink-2 leading-snug">{issue ? issue.message : okDetail}</span>
        {issue?.task_ids && issue.task_ids.length > 0 && (
          <span className="col-span-2 flex flex-wrap gap-1">
            {issue.task_ids.slice(0, 18).map(id => (
              <button key={id} type="button" onClick={() => setFocus(id)} className="amp-num text-[10.5px] px-1 border border-line hover:border-ink cursor-pointer">#{id}</button>
            ))}
            {issue.task_ids.length > 18 && <span className="amp-num text-[10.5px] text-ink-3">+{issue.task_ids.length - 18}</span>}
          </span>
        )}
        {issue?.fix && <span className="col-span-2 text-[11.5px] text-ink-3">{issue.fix}</span>}
      </div>
    )
  }

  if (loading && !tasks.length) {
    return <div className="flex items-center justify-center h-full"><BandLoader label="Loading the flight plan" /></div>
  }

  const openCount = tasks.filter(t => t.state !== 'completed').length
  return (
    <div className="flex flex-col min-h-full">
      <ProjectNav projectId={pid} currentView="plan" rightSlot={<>
        <label className="inline-flex items-center gap-2 text-[12.5px] text-ink-2 cursor-pointer select-none">
          <input type="checkbox" checked={hideDone} onChange={e => setHideDone(e.target.checked)} className="accent-[var(--ink)]" />Hide done
        </label>
        <Link to={`/project/${pid}/dag`} className="amp-btn"><GitBranch size={12} />Graph</Link>
      </>} />
      <div className="p-7 pt-5 grid gap-4">
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        <Readouts items={[
          { label: 'Tickets', value: tasks.length, sub: `${openCount} open` },
          { label: 'Epics · stories', value: <>{epics.length}<span className="text-[14px] text-ink-3"> · {stories.length}</span></>, sub: 'in this plan' },
          { label: 'Dependencies', value: report?.dependencies ?? '—', sub: 'links between tickets' },
          { label: 'Longest chain', value: shape.stages, sub: 'stages to finish everything' },
          { label: 'Ready now', value: report?.ready_now.length ?? '—', sub: 'can be dispatched', color: 'var(--ink)' },
        ]} />

        <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-[minmax(0,1fr)_340px]">
          <section className="amp-panel min-w-0 overflow-x-auto" aria-label="Plan by stage">
            {tasks.length === 0 ? (
              <p className="m-0 px-5 py-8 text-center text-[13.5px] text-ink-2">No tickets yet. When your planner agent files a plan, it shows up here stage by stage.</p>
            ) : (
              <table className="w-full border-collapse" style={{ minWidth: 160 + shape.stages * 70, tableLayout: 'fixed' }}>
                <colgroup><col style={{ width: 160 }} />{stageCounts.map((_, i) => <col key={i} />)}</colgroup>
                <thead>
                  <tr className="bg-surface-2">
                    <th className="amp-label text-left px-4 py-2.5 border-b border-line">Story</th>
                    {stageCounts.map((n, i) => (
                      <th key={i} className="amp-label text-center px-1 py-2.5 border-b border-line">Stage {i + 1}
                        <span className="block amp-num normal-case tracking-normal text-[10px] mt-1">{n}</span></th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {rows.map(r => (
                    <FragmentRows key={r.epic.id} epicName={r.epic.name} epicPriority={r.epic.priority} cols={shape.stages}
                      count={r.stories.reduce((a, s) => a + s.total, 0)}>
                      {r.stories.map(s => (
                        <tr key={s.story.id}>
                          <td className="px-4 py-1.5 border-b border-line align-top">
                            <span className="block text-[12.5px] text-ink truncate" title={s.story.name}>{s.story.name}</span>
                            <span className="block amp-num text-[10px] text-ink-3">{s.total ? `${s.total} ticket${s.total === 1 ? '' : 's'}` : <span className="text-danger-ink">no tickets</span>}</span>
                          </td>
                          {stageCounts.map((_, i) => (
                            <td key={i} className="px-0.5 py-1 border-b border-l border-dashed border-line text-center align-top">
                              {s.tasks.filter(t => shape.depth.get(t.id) === i).map(t => (
                                <Chip key={t.id} t={t} state={displayState(t)} crit={shape.crit.has(t.id)}
                                  lit={!!lit?.all.has(t.id)} sel={focus === t.id} dim={focus != null}
                                  onClick={() => setFocus(f => f === t.id ? null : t.id)} />
                              ))}
                            </td>
                          ))}
                        </tr>
                      ))}
                    </FragmentRows>
                  ))}
                </tbody>
              </table>
            )}
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-2.5 border-t border-line text-[11.5px] text-ink-2">
              {(['done', 'prog', 'ready', 'wait', 'flag', 'sched'] as DisplayState[]).map(s => (
                <span key={s} className="inline-flex items-center gap-1.5"><StateDot state={s} size={10} />{STATE_LABEL[s]}</span>
              ))}
              <span className="inline-flex items-center gap-1.5"><i className="inline-block w-[14px] h-[10px]" style={{ outline: '1.5px solid var(--accent)' }} />Critical path</span>
              <span className="ml-auto text-ink-3">Click a ticket to trace what it waits on and what it unblocks</span>
            </div>
          </section>

          <div className="grid grid-cols-1 gap-4 min-w-0">
            {/* Below xl the side column sits under the plan, so a selection docks to the corner instead. */}
            <Panel className={focused ? 'max-xl:fixed max-xl:bottom-4 max-xl:right-4 max-xl:z-30 max-xl:w-[min(380px,calc(100vw-32px))] max-xl:shadow-[0_8px_24px_rgba(0,0,0,0.16)]' : 'max-xl:hidden'} label="Selected" title={focused ? `#${focused.id} · stage ${(shape.depth.get(focused.id) ?? 0) + 1} of ${shape.stages}` : 'Nothing selected'}>
              {focused ? (
                <div className="grid gap-3 px-4 py-3">
                  <span className="text-[14px] font-semibold text-ink leading-snug">{focused.name}</span>
                  <dl className="m-0 grid grid-cols-[96px_1fr] gap-x-3 gap-y-2 text-[12.5px] items-baseline">
                    <dt className="amp-label">State</dt><dd className="m-0"><StateTag state={displayState(focused)} /></dd>
                    <dt className="amp-label">Priority</dt><dd className="m-0"><PriorityMark priority={focused.priority} showLabel /></dd>
                    <dt className="amp-label">Waits on</dt><dd className="m-0 amp-num">{(focused.dependency_ids ?? []).length
                      ? focused.dependency_ids.map(id => <button key={id} type="button" onClick={() => setFocus(id)} className="mr-1.5 underline underline-offset-2 cursor-pointer">#{id}</button>)
                      : 'nothing'} <span className="text-ink-3">({lit?.up.size ?? 0} upstream)</span></dd>
                    <dt className="amp-label">Unblocks</dt><dd className="m-0 amp-num">{(shape.dependents.get(focused.id) ?? []).length
                      ? (shape.dependents.get(focused.id) ?? []).map(id => <button key={id} type="button" onClick={() => setFocus(id)} className="mr-1.5 underline underline-offset-2 cursor-pointer">#{id}</button>)
                      : 'nothing'} <span className="text-ink-3">({lit?.down.size ?? 0} downstream)</span></dd>
                    <dt className="amp-label">Profile</dt><dd className="m-0 amp-num">{focused.assigned_to || '—'}</dd>
                    <dt className="amp-label">Agent</dt><dd className="m-0 amp-num">{focused.agent_id || 'not dispatched'}</dd>
                  </dl>
                  <div className="flex gap-2">
                    <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" onClick={() => setOpenId(focused.id)}>Open ticket</button>
                    <button type="button" className="amp-btn amp-btn-sm" onClick={() => setFocus(null)}>Clear</button>
                  </div>
                </div>
              ) : <p className="m-0 px-4 py-3 text-[13px] text-ink-2">Click a ticket to see where it sits in the plan.</p>}
            </Panel>
            <Panel label="Plan check" title="GO / NO-GO" right={
              <button type="button" onClick={runCheck} disabled={checking} className="amp-btn amp-btn-ghost amp-btn-sm" title="Run the plan check again">
                {checking ? <Loader2 size={11} className="animate-spin" /> : <RefreshCw size={11} />}Check</button>}>
              {checkError && <p className="m-0 px-4 py-3 text-[12.5px] text-danger-ink">{checkError}</p>}
              {report && <>
                {pollRow('isolated', 'Every open ticket depends on something or has something depending on it.')}
                {pollRow('wide_start', `${report.ready_now.length} of ${report.open_tasks} open tickets can start now.`)}
                {pollRow('empty_stories', 'Every story has at least one ticket.')}
                {pollRow('no_context', 'The project KB explains what this is and why the plan looks like this.')}
                {pollRow('coupled_gate', "No single ticket makes one person's next work wait on another's.")}
                {pollRow('metadata_drift', 'Every epic has the metadata keys the others use, so reports roll it up.')}
                <div className="grid grid-cols-[1fr_auto] gap-x-3 gap-y-1 px-4 py-2.5 border-t border-line">
                  <span className="text-[13px] text-ink">No stuck tickets</span><Verdict key={`${poll}-stuck`} ok stampDelay={pollOrder.length * 90} />
                  <span className="col-span-2 text-[11.5px] text-ink-2">{report.repaired_task_ids?.length
                    ? `Repaired ${report.repaired_task_ids.length} blocked on finished work: #${report.repaired_task_ids.join(', #')}`
                    : 'Nothing is blocked on finished work.'}</span>
                </div>
                <p className="m-0 px-4 py-2.5 border-t border-line text-[11.5px] text-ink-3">{report.summary}</p>
                <Approval key={poll} report={report} onApprove={approve} approving={approving} error={approveError} onFocus={setFocus}
                  approver={report.plan_approved_by != null ? lookupPerson(people, report.plan_approved_by)?.name : undefined} />
              </>}
            </Panel>

          </div>
        </div>
      </div>

      <TaskDrawer task={opened} onClose={() => setOpenId(null)} onRefresh={refresh}
        members={members} meId={me?.user_id} myAccess={myAccess} openReassign={openReassign} onTaskUpdated={applyTask} />
    </div>
  )
}

/** Plan approval: what's new since the last approval, and the button. */
function Approval({ report, onApprove, approving, error, onFocus, approver }: {
  report: PlanReport; onApprove: () => void; approving: boolean; error: string | null; onFocus: (id: number) => void; approver?: string
}) {
  const waiting = report.awaiting_approval ?? []
  const when = report.plan_approved_at ? new Date(report.plan_approved_at).toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : null
  return (
    <div className="grid gap-2.5 px-4 py-3.5 border-t-2 border-ink">
      <div className="grid grid-cols-[1fr_auto] gap-x-3 gap-y-1">
        <span className="text-[13px] font-semibold text-ink">Plan approval</span>
        <Verdict ok={waiting.length === 0} stampDelay={700} />
        <span className="col-span-2 text-[11.5px] text-ink-2 leading-snug">
          {waiting.length === 0
            ? <>Approved{when ? <> {when}</> : null}{approver ? <> by {approver}</> : null}. Nothing new has been planned since.</>
            : <>{waiting.length} open ticket{waiting.length === 1 ? '' : 's'} planned {when ? <>since the last approval ({when}{approver ? `, ${approver}` : ''})</> : 'and never approved'}.{' '}
                {report.require_plan_approval ? 'Agents can\'t start them until the plan is approved.' : 'This project doesn\'t require approval, so agents can already start them.'}</>}
        </span>
        {waiting.length > 0 && (
          <span className="col-span-2 flex flex-wrap gap-1">
            {waiting.slice(0, 24).map(id => <button key={id} type="button" onClick={() => onFocus(id)} className="amp-num text-[10.5px] px-1 border border-line hover:border-ink cursor-pointer">#{id}</button>)}
            {waiting.length > 24 && <span className="amp-num text-[10.5px] text-ink-3">+{waiting.length - 24}</span>}
          </span>
        )}
      </div>
      {error && <span className="text-[12px] text-danger-ink">{error}</span>}
      {waiting.length > 0 && (
        <div className="flex gap-2">
          <button type="button" className="amp-btn amp-btn-accent" disabled={approving} onClick={onApprove}>
            {approving && <Loader2 size={12} className="animate-spin" />}Approve plan</button>
        </div>
      )}
    </div>
  )
}

/** GO / WARN / NO-GO. A GO with stampDelay stamps in after that many ms; the others stay still. */
function Verdict({ ok, warn, stampDelay }: { ok: boolean; warn?: boolean; stampDelay?: number }) {
  const style = ok ? { border: '1px solid var(--success)', color: 'var(--success-ink)' }
    : warn ? { border: '1px solid var(--warning)', color: 'var(--warning-ink)' }
      : { background: 'var(--st-flag)', color: '#fff' }
  return (
    <span className={`font-label text-[10.5px] font-bold tracking-[0.16em] px-1.5 py-1 self-start leading-none${ok && stampDelay != null ? ' amp-go-stamp' : ''}`}
      style={ok && stampDelay != null ? { ...style, animationDelay: `${stampDelay}ms`, ['--amp-stamp-delay' as string]: `${stampDelay}ms` } : style}>
      {ok ? 'GO' : warn ? 'WARN' : 'NO-GO'}
    </span>
  )
}

function FragmentRows({ epicName, epicPriority, count, cols, children }: {
  epicName: string; epicPriority: string; count: number; cols: number; children: React.ReactNode
}) {
  return (
    <>
      <tr className="bg-surface-2">
        <td colSpan={cols + 1} className="px-4 py-2 border-b border-line">
          <span className="inline-flex items-center gap-3">
            <span className="amp-bands-v h-[14px]" aria-hidden="true" />
            <span className="font-label text-[11.5px] font-semibold uppercase tracking-[0.14em] text-ink">{epicName}</span>
            <PriorityMark priority={epicPriority} />
            <span className="amp-num text-[10.5px] text-ink-3">{count} tickets</span>
          </span>
        </td>
      </tr>
      {children}
    </>
  )
}
