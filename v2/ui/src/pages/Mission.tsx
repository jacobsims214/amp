/**
 * Mission — the project for everyone, informational (KB decisions/human-work-ux.md §3).
 * Built for hundreds of tickets:
 *   - A "Waiting on you" strip that leads to My work, where every action lives.
 *   - Six readouts: in progress, ready, waiting, questions (N · M for you),
 *     merge needed (N · M yours), done.
 *   - Every epic is a panel; every ticket in it is one square, colored by
 *     state, in rows by story, outlined when it waits on you. Epics that need
 *     a person sort first; finished epics fold into a list at the bottom.
 *   - Beside them: Waiting on people (each person's counts by kind, linking
 *     into their My work), who is working what, and the latest activity.
 * Nothing here is an action: clicking a ticket opens it in the drawer, and
 * answering, merging, approving and doing happen in My work.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useFocusParam } from '../hooks/useFocusParam'
import { ChevronRight } from 'lucide-react'
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
import { countStates, displayState, QUIET_MS, silentFor, STATE_LABEL, STATE_VAR, type DisplayState } from '../lib/ticketState'
import { activityActor, lookupPerson } from '../lib/people'
import type { ActivityLog, AuditEntry, Epic, Story, Task, WaitCounts } from '../types'
import { isHuman, reasonLabel } from '../lib/human'
import { canApproveOnBehalf } from '../lib/approval'
import { projectLink } from '../lib/projectLink'
import { projectCounts, refreshWaitCounts, useWaitCounts } from '../lib/waitCounts'
import { countsSummary, kindSummary, needsMerge, panelOrder, personLink, ticketsWaitingOn, waitingOnPeople, type PersonWaiting } from '../lib/waitingOnPeople'
import { BandLoader } from '../components/brand/BandLoader'

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

/** One square per ticket; a ticket waiting on you is outlined in the link color. */
function Squares({ tasks, mine, onOpen }: { tasks: Task[]; mine: Set<number>; onOpen: (id: number) => void }) {
  return (
    <div className="flex flex-wrap gap-[3px]">
      {tasks.map(t => {
        const s = displayState(t)
        const hollow = s === 'wait' || s === 'ready'
        const forYou = mine.has(t.id)
        return (
          <button key={t.id} type="button" onClick={() => onOpen(t.id)} data-for-you={forYou || undefined}
            title={`#${t.id} ${t.name} · ${STATE_LABEL[s]}${t.agent_id ? ' · ' + t.agent_id : ''}${forYou ? ' · waits on you' : ''}`}
            aria-label={`Ticket ${t.id}: ${t.name}, ${STATE_LABEL[s]}${forYou ? ', waits on you' : ''}`}
            className="w-[11px] h-[11px] cursor-pointer hover:scale-125 transition-transform focus-visible:outline-2 focus-visible:outline-offset-1"
            style={{ borderRadius: 1, background: hollow ? 'transparent' : STATE_VAR[s],
              boxShadow: hollow ? `inset 0 0 0 ${s === 'wait' ? 1 : 1.5}px ${STATE_VAR[s]}` : undefined,
              outline: forYou ? '2px solid var(--link)' : undefined, outlineOffset: forYou ? 1 : undefined }} />
        )
      })}
    </div>
  )
}

function EpicPanel({ epic, stories, tasks, owners, mine, onOpen }: {
  epic: Epic; stories: Story[]; tasks: Task[]; owners: string; mine: Set<number>; onOpen: (id: number) => void
}) {
  const counts = countStates(tasks)
  const forYou = tasks.filter(t => mine.has(t.id)).length
  const byStory = (sid: number) => tasks.filter(t => t.story_id === sid).sort((a, b) => a.id - b.id)
  return (
    <section className="amp-panel flex flex-col" aria-label={`Epic ${epic.name}`}>
      <div className="flex gap-3 px-3.5 pt-3 pb-2.5">
        <span className="amp-bands-v" aria-hidden="true" />
        <div className="flex-1 min-w-0 grid gap-1">
          <span className="amp-label flex items-center gap-2">Epic <PriorityMark priority={epic.priority} />{owners && <span className="normal-case tracking-normal font-body font-normal text-[11px] truncate">{owners}</span>}</span>
          <h3 className="m-0 text-[14.5px] font-semibold leading-tight text-ink">{epic.name}</h3>
        </div>
        <div className="grid gap-1 justify-items-end">
          <div className="amp-num text-[20px] leading-none text-right text-ink whitespace-nowrap">{counts.done}<span className="text-[12px] text-ink-3">/{tasks.length}</span></div>
          {forYou > 0 && <span className="amp-num text-[11px] text-link whitespace-nowrap">{forYou} for you</span>}
        </div>
      </div>
      <div className="px-3.5 pb-2.5"><StateBar counts={counts} /></div>
      {stories.map(s => {
        const list = byStory(s.id)
        return (
          <div key={s.id} className="grid grid-cols-[112px_1fr] gap-2 items-center px-3.5 py-1.5 border-t border-line">
            <span className="text-[12px] text-ink-2 truncate" title={s.name}>{s.name}</span>
            {list.length ? <Squares tasks={list} mine={mine} onOpen={onOpen} />
              : <span className="font-label text-[10.5px] font-semibold uppercase tracking-[0.12em] text-danger-ink">No tickets</span>}
          </div>
        )
      })}
    </section>
  )
}

/**
 * "Waiting on you": the viewer's counts in this project (the same numbers as
 * the My work tab's badge), leading to My work. Quiet when nothing waits.
 */
function WaitingStrip({ projectId, counts }: { projectId: number; counts: WaitCounts }) {
  if (counts.waiting > 0) {
    return (
      <Link to={projectLink(projectId, 'my-work')} aria-label={`Waiting on you: ${counts.waiting}. ${countsSummary(counts)}. Open My work`}
        className="amp-panel flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-3 hover:bg-surface-2 no-underline" style={{ borderColor: 'var(--ink)' }}>
        <span className="inline-flex items-center gap-2 amp-label" style={{ color: 'var(--ink)' }}><i className="w-2 h-2 bg-accent" aria-hidden="true" />Waiting on you</span>
        <span className="amp-num text-[22px] leading-none text-ink">{counts.waiting}</span>
        <span className="text-[13px] text-ink-2">{countsSummary(counts)}</span>
        <span className="ml-auto amp-label inline-flex items-center gap-1" style={{ color: 'var(--link)' }}>Open My work<ChevronRight size={14} aria-hidden="true" /></span>
      </Link>
    )
  }
  return (
    <Link to={projectLink(projectId, 'my-work')} className="amp-panel flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-3 hover:bg-surface-2 no-underline">
      <span className="amp-label" style={{ color: 'var(--success-ink)' }}>Nothing waits on you here</span>
      {counts.coming_up > 0 && <span className="text-[13px] text-ink-2">{counts.coming_up} coming up</span>}
      <span className="ml-auto amp-label inline-flex items-center gap-1" style={{ color: 'var(--link)' }}>My work<ChevronRight size={14} aria-hidden="true" /></span>
    </Link>
  )
}

/** One person in Waiting on people: their counts by kind, linking into their My work. */
function PersonRow({ p, name, mine, href }: { p: PersonWaiting; name: string; mine: boolean; href: string }) {
  const summary = kindSummary(p)
  return (
    <Link to={href} className="grid grid-cols-[auto_1fr_auto] gap-x-2.5 gap-y-0.5 items-center px-4 py-2.5 border-t border-line first:border-t-0 hover:bg-surface-2 no-underline"
      aria-label={`${mine ? 'You' : name}: ${p.counts.waiting} waiting${summary ? `. ${summary}` : ''}`}>
      <Avatar name={name} kind="human" />
      <span className="text-[12.5px] text-ink truncate"><b className="font-semibold">{mine ? 'You' : name}</b>: <span className="amp-num">{p.counts.waiting}</span></span>
      <ChevronRight size={15} className="text-ink-3" aria-hidden="true" />
      {summary && <span className="col-start-2 col-span-2 text-[11.5px] text-ink-2 truncate">{summary}</span>}
    </Link>
  )
}

const ACTION_COLOR: Record<string, string> = {
  completed: 'var(--st-done)', dispatched: 'var(--st-prog)', blocked: 'var(--st-flag)', unblocked: 'var(--ink-2)', claimed: 'var(--st-prog)',
}

/** One line in the activity feed: a ticket event or a plan-level change. */
interface FeedItem { key: string; at: string; actor: string; actorUserId?: number; action: string; taskId?: number; summary?: string; target?: string; count?: number }
const fromActivity = (a: ActivityLog): FeedItem => ({ key: `a${a.id}`, at: a.created_at, actor: a.actor, actorUserId: a.actor_user_id, action: a.action, taskId: a.task_id })
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
  const waitCounts = useWaitCounts()
  const yours = projectCounts(waitCounts, pid)
  // Whether the project requires plan approval: approve items only count when it does.
  const [requireApproval, setRequireApproval] = useState(false)
  useEffect(() => {
    let live = true
    api.getProject(pid).then(p => { if (live) setRequireApproval(!!p.require_plan_approval) }).catch(() => {})
    return () => { live = false }
  }, [pid])
  // ?focus=ID (or the old ?ticket=) opens a ticket; it stays in the URL until closed.
  const [selectedId, setSelectedId] = useFocusParam()
  const [showDone, setShowDone] = useState(false)
  const [activity, setActivity] = useState<FeedItem[]>([])
  // Last sign of life per ticket (any activity), and a clock that moves on
  // its own, so a dispatched ticket that has gone silent shows as quiet.
  const [lastSeen, setLastSeen] = useState<Map<number, number>>(new Map())
  const [now, setNow] = useState(Date.now)
  const selected = selectedId == null ? null : tasks.find(t => t.id === selectedId) ?? null

  // Latest activity; refreshed shortly after tickets change.
  const loadActivity = useCallback(() => {
    const since = new Date(Date.now() - 3 * 86_400_000).toISOString()
    // Ticket activity plus plan-level changes (deletes, approvals, settings) from
    // the audit log, which only leads can read — others just get ticket activity.
    Promise.all([
      api.getProjectActivity(pid, since),
      api.audit({ projectId: pid, limit: 200 }).catch(() => [] as AuditEntry[]),
    ]).then(([a, audit]) => {
      const seen = new Map<number, number>()
      for (const e of a) seen.set(e.task_id, Math.max(seen.get(e.task_id) ?? 0, Date.parse(e.created_at)))
      setLastSeen(seen)
      setNow(Date.now())
      setActivity([...a.map(fromActivity), ...audit.filter(e => e.at >= since).map(fromAudit)]
        .sort((x, y) => y.at.localeCompare(x.at)).reduce(collapseRuns, []).slice(0, 14))
    })
      .catch(() => {})
  }, [pid])
  useEffect(() => {
    const t = setTimeout(loadActivity, 600)
    return () => clearTimeout(t)
  }, [tasks, loadActivity])
  // Agents' comments don't change tickets, so poll for them once a minute.
  useEffect(() => {
    const t = setInterval(loadActivity, 60_000)
    return () => clearInterval(t)
  }, [loadActivity])
  const quietFor = (t: Task) => silentFor([lastSeen.get(t.id), t.dispatched_at, t.updated_at], now)

  const counts = useMemo(() => countStates(tasks), [tasks])
  // Keep the strip's numbers in step when tickets change under us (an answer, a merge).
  useEffect(() => {
    if (!tasks.length) return
    const t = setTimeout(() => { refreshWaitCounts() }, 1200)
    return () => clearTimeout(t)
  }, [tasks])
  const mergeNeeded = useMemo(() => tasks.filter(needsMerge).length, [tasks])
  const waiting = useMemo(() => waitingOnPeople(tasks, { requireApproval }), [tasks, requireApproval])
  const mine = useMemo(() => ticketsWaitingOn(me?.user_id != null ? waiting.get(me.user_id) : undefined), [waiting, me?.user_id])
  const waitingPeople = useMemo(() => panelOrder(waiting, me?.user_id), [waiting, me?.user_id])
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
    return <div className="flex items-center justify-center h-full"><BandLoader label="Loading mission" /></div>
  }

  // Agents are profiles in each person's harness, so count the people they work for.
  // People doing their own (human) tickets aren't agents, and are never quiet.
  const agentsFlying = flying.filter(t => !isHuman(t))
  const peopleWorking = flying.length - agentsFlying.length
  const quiet = agentsFlying.filter(t => quietFor(t) > QUIET_MS).length
  const drivers = new Set(agentsFlying.map(t => t.owner_user_id).filter(id => id != null)).size
  const runningSub = [
    agentsFlying.length ? `agents for ${drivers} ${drivers === 1 ? 'person' : 'people'}` : '',
    peopleWorking ? `${peopleWorking} by people` : '',
  ].filter(Boolean).join(' · ') || 'nothing running'
  return (
    <div className="flex flex-col min-h-full">
      <ProjectNav projectId={pid} currentView="mission" />
      <div className="p-7 pt-5 grid gap-4">
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {yours && <WaitingStrip projectId={pid} counts={yours} />}
        <Readouts wrap items={[
          { label: 'In progress', value: counts.prog, sub: quiet ? `${quiet} quiet · may have stalled` : runningSub, alarm: quiet > 0, color: 'var(--st-prog)' },
          { label: 'Ready', value: counts.ready, sub: 'can dispatch now' },
          { label: 'Waiting', value: counts.wait + counts.sched, color: 'var(--ink-3)',
            sub: counts.sched ? `${counts.sched} scheduled${nextStart ? `, next ${new Date(nextStart).toLocaleString(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit' })}` : ''}` : 'on other tickets' },
          { label: 'Questions', value: counts.flag, color: counts.flag ? 'var(--st-flag)' : undefined,
            sub: yours ? `${yours.answer} for you` : counts.flag ? 'waiting on an answer' : 'none' },
          { label: 'Merge needed', value: mergeNeeded, color: mergeNeeded ? 'var(--teal)' : undefined,
            sub: yours ? `${yours.merge} yours` : mergeNeeded ? 'done, not merged' : 'none' },
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
              {open.map(r => <EpicPanel key={r.epic.id} epic={r.epic} stories={r.stories} tasks={r.list} owners={ownersOf(r.epic.owner_ids)} mine={mine} onOpen={setSelectedId} />)}
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
                    {finished.map(r => <EpicPanel key={r.epic.id} epic={r.epic} stories={r.stories} tasks={r.list} owners={ownersOf(r.epic.owner_ids)} mine={mine} onOpen={setSelectedId} />)}
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
            <Panel label="Waiting on people" right={<span>informational</span>}>
              {waitingPeople.length === 0
                ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Nothing waits on anyone here.</p>
                : waitingPeople.map(p => (
                  <PersonRow key={p.userId} p={p} mine={p.userId === me?.user_id}
                    name={lookupPerson(people, p.userId)?.name ?? `User #${p.userId}`}
                    href={personLink(pid, p, { meId: me?.user_id, canReadOthers: canApproveOnBehalf(myAccess) })} />
                ))}
              <p className="m-0 px-4 pt-2 pb-3 border-t border-line text-[12px] text-ink-3 leading-snug">
                {canApproveOnBehalf(myAccess)
                  ? 'Each person acts in their own My work. Rows open theirs with the ticket focused, read-only unless you can act for them.'
                  : 'Each person acts in their own My work. Your row opens yours; others open their ticket on the Board.'}
              </p>
            </Panel>

            <Panel label="Crew" title="Who's working what">
              {flying.length === 0
                ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Nothing in progress.</p>
                : flying.map(t => (
                  <button key={t.id} type="button" onClick={() => setSelectedId(t.id)}
                    className="w-full grid grid-cols-[auto_1fr_auto] gap-x-2.5 gap-y-0.5 items-center px-4 py-2.5 border-t border-line first:border-t-0 text-left hover:bg-surface-2 cursor-pointer">
                    {isHuman(t)
                      ? <Avatar name={lookupPerson(people, t.owner_user_id)?.name ?? '?'} kind="human" />
                      : <Avatar name={t.agent_id || '?'} kind="agent" />}
                    <span className="text-[12.5px] text-ink truncate"><b className="font-semibold">{lookupPerson(people, t.owner_user_id)?.name ?? 'Unowned'}</b>
                      <span className="amp-num text-[11px] text-ink-3"> · {isHuman(t) ? `themselves · ${reasonLabel(t.human_reason).toLowerCase()}` : t.agent_id || 'agent'}</span></span>
                    {!isHuman(t) && quietFor(t) > QUIET_MS
                      ? <span className="amp-num text-[11px] text-st-flag" title="No activity on this ticket for a while: the agent may have stalled.">quiet {Math.round(quietFor(t) / 60_000)}m</span>
                      : <span className="amp-num text-[11px] text-ink-3">{ago(t.dispatched_at)}</span>}
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
                      <b className="font-semibold text-ink">{activityActor({ actor: a.actor, actor_user_id: a.actorUserId }, people)}</b>{' '}
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
