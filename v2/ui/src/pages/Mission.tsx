/**
 * Mission — a project while agents work it. Built for hundreds of tickets:
 *   - Six readouts: in progress, ready, waiting, flagged (an alarm), scheduled, done.
 *   - Every epic is a panel; every ticket in it is one square, colored by
 *     state, in rows by story. Epics that need a person sort first; finished
 *     epics fold into a list at the bottom.
 *   - Beside them: agent flags you can answer in place, who is working what,
 *     and the latest activity.
 * Clicking any ticket opens it in the drawer.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useParams, useSearchParams } from 'react-router-dom'
import { Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { useBoardData } from '../hooks/useBoardData'
import { useAuth } from '../hooks/useAuth'
import { useProjectPeople } from '../hooks/useProjectPeople'
import { ProjectNav } from '../components/ProjectNav'
import { TaskDrawer } from '../components/TaskDrawer'
import { Panel, Readouts } from '../components/ui/PageHeader'
import { StateBar, StateDot } from '../components/ui/StateTag'
import { Avatar } from '../components/ui/Avatar'
import { PriorityMark } from '../components/ui/PriorityMark'
import { countStates, displayState, STATE_LABEL, STATE_VAR, type DisplayState } from '../lib/ticketState'
import { lookupPerson } from '../lib/people'
import type { ActivityLog, AuditEntry, Epic, Story, Task } from '../types'

function ago(iso?: string | null): string {
  if (!iso) return ''
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 60_000) return 'now'
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m`
  if (ms < 172_800_000) return `${Math.round(ms / 3_600_000)}h`
  return `${Math.round(ms / 86_400_000)}d`
}

function clock(iso: string): string {
  const d = new Date(iso)
  const day = d.toDateString() === new Date().toDateString() ? '' : d.toLocaleDateString(undefined, { weekday: 'short' }) + ' '
  return day + d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })
}

/** One square per ticket. */
function Squares({ tasks, onOpen }: { tasks: Task[]; onOpen: (id: number) => void }) {
  return (
    <div className="flex flex-wrap gap-[3px]">
      {tasks.map(t => {
        const s = displayState(t)
        const hollow = s === 'wait' || s === 'ready'
        return (
          <button key={t.id} type="button" onClick={() => onOpen(t.id)}
            title={`#${t.id} ${t.name} · ${STATE_LABEL[s]}${t.agent_id ? ' · ' + t.agent_id : ''}`}
            aria-label={`Ticket ${t.id}: ${t.name}, ${STATE_LABEL[s]}`}
            className="w-[11px] h-[11px] cursor-pointer hover:scale-125 transition-transform focus-visible:outline-2 focus-visible:outline-offset-1"
            style={{ borderRadius: 1, background: hollow ? 'transparent' : STATE_VAR[s],
              boxShadow: hollow ? `inset 0 0 0 ${s === 'wait' ? 1 : 1.5}px ${STATE_VAR[s]}` : undefined }} />
        )
      })}
    </div>
  )
}

function EpicPanel({ epic, stories, tasks, owners, onOpen }: {
  epic: Epic; stories: Story[]; tasks: Task[]; owners: string; onOpen: (id: number) => void
}) {
  const counts = countStates(tasks)
  const byStory = (sid: number) => tasks.filter(t => t.story_id === sid).sort((a, b) => a.id - b.id)
  return (
    <section className="amp-panel flex flex-col" aria-label={`Epic ${epic.name}`}>
      <div className="flex gap-3 px-3.5 pt-3 pb-2.5">
        <span className="amp-bands-v" aria-hidden="true" />
        <div className="flex-1 min-w-0 grid gap-1">
          <span className="amp-label flex items-center gap-2">Epic <PriorityMark priority={epic.priority} />{owners && <span className="normal-case tracking-normal font-body font-normal text-[11px] truncate">{owners}</span>}</span>
          <h3 className="m-0 text-[14.5px] font-semibold leading-tight text-ink">{epic.name}</h3>
        </div>
        <div className="amp-num text-[20px] leading-none text-right text-ink whitespace-nowrap">{counts.done}<span className="text-[12px] text-ink-3">/{tasks.length}</span></div>
      </div>
      <div className="px-3.5 pb-2.5"><StateBar counts={counts} /></div>
      {stories.map(s => {
        const list = byStory(s.id)
        return (
          <div key={s.id} className="grid grid-cols-[112px_1fr] gap-2 items-center px-3.5 py-1.5 border-t border-line">
            <span className="text-[12px] text-ink-2 truncate" title={s.name}>{s.name}</span>
            {list.length ? <Squares tasks={list} onOpen={onOpen} />
              : <span className="font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] text-danger-ink">No tickets</span>}
          </div>
        )
      })}
    </section>
  )
}

function FlagCard({ task, blocks, onOpen, onAnswered }: { task: Task; blocks: number; onOpen: () => void; onAnswered: (t: Task) => void }) {
  const [answering, setAnswering] = useState(false)
  const [answer, setAnswer] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async () => {
    setBusy(true); setError(null)
    try { onAnswered(await api.answerFlag(task.id, answer)) }
    catch (e) { setError(e instanceof Error ? e.message : 'Could not answer') }
    finally { setBusy(false) }
  }
  return (
    <div className="grid grid-cols-[22px_1fr] gap-x-3 gap-y-1 px-4 py-3 border-t border-line first:border-t-0">
      <span className="row-span-3 w-[22px] h-[20px] bg-st-flag text-white grid place-items-center text-[11px] font-bold pt-1"
        style={{ clipPath: 'polygon(50% 0, 100% 100%, 0 100%)' }} aria-hidden="true">!</span>
      <p className="m-0 text-[13px] leading-snug text-ink">{task.block_reason}</p>
      <span className="amp-num text-[11px] text-ink-3">
        <button type="button" onClick={onOpen} className="underline underline-offset-2 hover:text-ink cursor-pointer">#{task.id}</button>
        {' '}· {task.agent_id || task.assigned_to || 'unassigned'} · {ago(task.updated_at)} · blocks {blocks}
      </span>
      {answering ? (
        <div className="grid gap-2 pt-1">
          <label htmlFor={`answer-${task.id}`} className="sr-only">Answer for ticket {task.id}</label>
          <textarea id={`answer-${task.id}`} className="amp-input min-h-[64px] resize-y" autoFocus value={answer}
            onChange={e => setAnswer(e.target.value)} placeholder="Your answer. It's posted on the ticket and the ticket goes back to ready." />
          {error && <span className="text-[12px] text-danger-ink">{error}</span>}
          <div className="flex gap-2">
            <button type="button" className="amp-btn amp-btn-accent amp-btn-sm" disabled={busy || !answer.trim()} onClick={submit}>
              {busy && <Loader2 size={11} className="animate-spin" />}Answer and unblock</button>
            <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => setAnswering(false)}>Cancel</button>
          </div>
        </div>
      ) : (
        <div className="flex gap-2 pt-1">
          <button type="button" className="amp-btn amp-btn-accent amp-btn-sm" onClick={() => setAnswering(true)}>Answer</button>
          <button type="button" className="amp-btn amp-btn-sm" onClick={onOpen}>Open</button>
        </div>
      )}
    </div>
  )
}

const ACTION_COLOR: Record<string, string> = {
  completed: 'var(--st-done)', dispatched: 'var(--st-prog)', blocked: 'var(--st-flag)', unblocked: 'var(--ink-2)', claimed: 'var(--st-prog)',
}

/** One line in the activity feed: a ticket event or a plan-level change. */
interface FeedItem { key: string; at: string; actor: string; action: string; taskId?: number; summary?: string; target?: string; count?: number }
const fromActivity = (a: ActivityLog): FeedItem => ({ key: `a${a.id}`, at: a.created_at, actor: a.actor, action: a.action, taskId: a.task_id })
const fromAudit = (e: AuditEntry): FeedItem => ({ key: `u${e.id}`, at: e.at, actor: e.actor_email.split('@')[0], action: e.action, summary: e.summary, target: e.target_type })

/** Folds a run of the same plan-level change ("deleted ticket" ×87) into one line. */
function collapseRuns(out: FeedItem[], it: FeedItem): FeedItem[] {
  const prev = out[out.length - 1]
  if (it.taskId == null && prev && prev.taskId == null && prev.actor === it.actor && prev.action === it.action) {
    const n = (prev.count ?? 1) + 1
    const verb = it.action.split('.').pop() ?? it.action
    const noun = it.target === 'task' ? 'tickets' : it.target === 'story' ? 'stories' : `${it.target ?? 'item'}s`
    out[out.length - 1] = { ...prev, count: n, summary: verb.endsWith('e') ? `${verb}d ${n} ${noun}` : `${it.action} ×${n}` }
    return out
  }
  out.push(it)
  return out
}

export function Mission() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)
  const { epics, stories, tasks, loading, error, refresh, applyTask } = useBoardData(pid)
  const { members, myAccess, openReassign, people } = useProjectPeople(pid)
  const { me } = useAuth()
  // ?ticket=ID opens a ticket on arrival (links from My work and elsewhere).
  const [params, setParams] = useSearchParams()
  const [selectedId, setSelected] = useState<number | null>(() => Number(params.get('ticket')) || null)
  const setSelectedId = (id: number | null) => {
    setSelected(id)
    if (params.has('ticket')) { params.delete('ticket'); setParams(params, { replace: true }) }
  }
  const [showDone, setShowDone] = useState(false)
  const [activity, setActivity] = useState<FeedItem[]>([])
  const selected = selectedId == null ? null : tasks.find(t => t.id === selectedId) ?? null

  // Latest activity; refreshed shortly after tickets change.
  const loadActivity = useCallback(() => {
    const since = new Date(Date.now() - 3 * 86_400_000).toISOString()
    // Ticket activity plus plan-level changes (deletes, approvals, settings) from
    // the audit log, which only leads can read — others just get ticket activity.
    Promise.all([
      api.getProjectActivity(pid, since),
      api.audit({ projectId: pid, limit: 200 }).catch(() => [] as AuditEntry[]),
    ]).then(([a, audit]) => setActivity(
      [...a.map(fromActivity), ...audit.filter(e => e.at >= since).map(fromAudit)]
        .sort((x, y) => y.at.localeCompare(x.at)).reduce(collapseRuns, []).slice(0, 14)))
      .catch(() => {})
  }, [pid])
  useEffect(() => {
    const t = setTimeout(loadActivity, 600)
    return () => clearTimeout(t)
  }, [tasks, loadActivity])

  const counts = useMemo(() => countStates(tasks), [tasks])
  const dependents = useMemo(() => {
    const m = new Map<number, number>()
    for (const t of tasks) for (const d of t.dependency_ids ?? []) m.set(d, (m.get(d) ?? 0) + 1)
    return m
  }, [tasks])
  const flagged = useMemo(() => tasks.filter(t => displayState(t) === 'flag')
    .sort((a, b) => (dependents.get(b.id) ?? 0) - (dependents.get(a.id) ?? 0)), [tasks, dependents])
  const flying = useMemo(() => tasks.filter(t => t.state === 'in_progress')
    .sort((a, b) => (a.dispatched_at ?? '').localeCompare(b.dispatched_at ?? '')), [tasks])
  const nextStart = useMemo(() => tasks.filter(t => displayState(t) === 'sched' && t.start_at)
    .map(t => t.start_at!).sort()[0], [tasks])

  // Epics: needing a person first, then active, then the rest; finished last.
  const epicRows = useMemo(() => {
    const rank = (c: Record<DisplayState, number>, n: number) =>
      c.flag ? 0 : c.prog ? 1 : c.ready ? 2 : c.done < n ? 3 : 4
    return epics.map(e => {
      const list = tasks.filter(t => t.epic_id === e.id)
      const c = countStates(list)
      return { epic: e, list, c, rank: rank(c, list.length), stories: stories.filter(s => s.epic_id === e.id).sort((a, b) => a.id - b.id) }
    }).sort((a, b) => a.rank - b.rank || a.epic.id - b.epic.id)
  }, [epics, stories, tasks])
  const open = epicRows.filter(r => r.rank < 4 || r.list.length === 0)
  const finished = epicRows.filter(r => r.rank === 4 && r.list.length > 0)
  const ownersOf = (ids?: number[]) => (ids ?? []).map(id => lookupPerson(people, id)?.name).filter(Boolean).join(', ')

  if (loading && !tasks.length) {
    return <div className="flex items-center justify-center h-full"><Loader2 size={20} className="animate-spin text-ink-3" /></div>
  }

  const agents = new Set(flying.map(t => t.agent_id).filter(Boolean)).size
  return (
    <div className="flex flex-col min-h-full">
      <ProjectNav projectId={pid} currentView="mission" />
      <div className="p-7 pt-5 grid gap-4">
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        <Readouts items={[
          { label: 'In progress', value: counts.prog, sub: `${agents} agent${agents === 1 ? '' : 's'} working`, color: 'var(--st-prog)' },
          { label: 'Ready', value: counts.ready, sub: 'can dispatch now' },
          { label: 'Waiting', value: counts.wait, sub: 'on dependencies', color: 'var(--ink-3)' },
          { label: 'Questions', value: counts.flag, sub: counts.flag ? 'agents waiting on an answer' : 'nothing needs you', alarm: counts.flag > 0 },
          { label: 'Scheduled', value: counts.sched, sub: nextStart ? `next ${new Date(nextStart).toLocaleString(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit' })}` : 'none', color: 'var(--st-sched)' },
          { label: 'Done', value: <>{counts.done}<span className="text-[13px] text-ink-3">/{tasks.length}</span></>,
            sub: tasks.length ? `${Math.round(counts.done / tasks.length * 100)}% of the plan` : 'no tickets yet' },
        ]} />

        <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-[minmax(0,1fr)_340px]">
          <div className="grid grid-cols-1 gap-3 min-w-0">
            {open.length === 0 && finished.length === 0 && (
              <div className="amp-panel px-5 py-8 text-center text-[13.5px] text-ink-2">
                No epics yet. Ask your planner agent to plan this project, or add an epic from the Board view.
              </div>
            )}
            <div className="grid gap-3 grid-cols-[repeat(auto-fill,minmax(310px,1fr))] items-start">
              {open.map(r => <EpicPanel key={r.epic.id} epic={r.epic} stories={r.stories} tasks={r.list} owners={ownersOf(r.epic.owner_ids)} onOpen={setSelectedId} />)}
            </div>
            {finished.length > 0 && (
              <section className="amp-panel">
                <button type="button" onClick={() => setShowDone(v => !v)} aria-expanded={showDone}
                  className="w-full flex items-center gap-3 px-4 py-3 cursor-pointer text-left">
                  <span className="amp-label">Finished</span>
                  <span className="text-[13.5px] font-semibold text-ink">{finished.length} epic{finished.length === 1 ? '' : 's'} done</span>
                  <span className="ml-auto amp-num text-[11px] text-ink-3">{showDone ? 'hide' : 'show'}</span>
                </button>
                {showDone && (
                  <div className="grid gap-3 p-3 pt-0 grid-cols-[repeat(auto-fill,minmax(310px,1fr))] items-start">
                    {finished.map(r => <EpicPanel key={r.epic.id} epic={r.epic} stories={r.stories} tasks={r.list} owners={ownersOf(r.epic.owner_ids)} onOpen={setSelectedId} />)}
                  </div>
                )}
              </section>
            )}
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-[11.5px] text-ink-2 px-1">
              {(['done', 'prog', 'ready', 'wait', 'flag', 'sched'] as DisplayState[]).map(s => (
                <span key={s} className="inline-flex items-center gap-1.5"><StateDot state={s} size={10} />{STATE_LABEL[s]}</span>
              ))}
              <span className="ml-auto text-ink-3">One square per ticket · click to open</span>
            </div>
          </div>

          <div className="grid grid-cols-1 gap-4 min-w-0">
            <Panel label="Questions" title={flagged.length ? `${flagged.length} waiting on an answer` : 'None'}
              right={<span>blocked with a reason</span>}>
              {flagged.length === 0
                ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">When an agent stops on a question, it shows up here. Answer it here, or have your orchestrator answer it with amp_answer_question.</p>
                : flagged.map(t => <FlagCard key={t.id} task={t} blocks={dependents.get(t.id) ?? 0} onOpen={() => setSelectedId(t.id)} onAnswered={applyTask} />)}
            </Panel>

            <Panel label="Crew" title="Who's working what">
              {flying.length === 0
                ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Nothing in progress.</p>
                : flying.map(t => (
                  <button key={t.id} type="button" onClick={() => setSelectedId(t.id)}
                    className="w-full grid grid-cols-[auto_1fr_auto] gap-x-2.5 gap-y-0.5 items-center px-4 py-2.5 border-t border-line first:border-t-0 text-left hover:bg-surface-2 cursor-pointer">
                    <Avatar name={t.agent_id || '?'} kind="agent" />
                    <span className="text-[12.5px] font-semibold text-ink truncate">{t.agent_id || 'agent'}</span>
                    <span className="amp-num text-[11px] text-ink-3">{ago(t.dispatched_at)}</span>
                    <span className="col-start-2 col-span-2 text-[11.5px] text-ink-2 truncate">#{t.id} {t.name}</span>
                  </button>
                ))}
            </Panel>

            <Panel label="Activity" title="Latest" right={<span className="inline-flex items-center gap-1.5"><i className="w-1.5 h-1.5 rounded-full bg-success" />live</span>}>
              {activity.length === 0
                ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">No activity in the last three days.</p>
                : activity.map(a => (
                  <div key={a.key} className="grid grid-cols-[52px_1fr] gap-2.5 px-4 py-1.5 border-t border-line first:border-t-0 text-[12.5px]">
                    <span className="amp-num text-[11px] text-ink-3 pt-px">{clock(a.at)}</span>
                    <span className="min-w-0">
                      <b className="font-semibold text-ink">{a.actor}</b>{' '}
                      {a.taskId != null
                        ? <><span className="font-semibold" style={{ color: ACTION_COLOR[a.action] ?? 'var(--ink-2)' }}>{a.action.replace('_', ' ')}</span>{' '}
                          <button type="button" className="amp-num underline underline-offset-2 text-ink-2 hover:text-ink cursor-pointer" onClick={() => setSelectedId(a.taskId!)}>#{a.taskId}</button></>
                        : <span className="text-ink-2">{a.summary}</span>}
                    </span>
                  </div>
                ))}
            </Panel>
          </div>
        </div>
      </div>

      <TaskDrawer task={selected} onClose={() => setSelectedId(null)} onRefresh={refresh}
        members={members} meId={me?.user_id} myAccess={myAccess} openReassign={openReassign} onTaskUpdated={applyTask} />
    </div>
  )
}
