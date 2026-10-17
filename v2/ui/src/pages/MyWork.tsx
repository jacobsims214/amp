/**
 * My work — where you land. In order: what needs a person (agents' questions in
 * projects you can see), what you own, what your agents are doing, and how
 * your days have gone.
 */
import { useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { BarChart } from '../components/ui/BarChart'
import { PageHeader, Panel, Readouts } from '../components/ui/PageHeader'
import { StateTag } from '../components/ui/StateTag'
import { PriorityMark } from '../components/ui/PriorityMark'
import { Avatar } from '../components/ui/Avatar'
import { displayState, type DisplayState } from '../lib/ticketState'
import type { DayReport, MyWork as MyWorkData, VelocityDay, WorkTask } from '../types'

const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone
const NEXT_UP: DisplayState[] = ['prog', 'flag', 'ready', 'wait', 'sched', 'done']
const ymd = (d: Date) => d.toLocaleDateString('en-CA', { timeZone: TZ })

function ago(iso?: string | null): string {
  if (!iso) return ''
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 3_600_000) return `${Math.max(1, Math.round(ms / 60_000))}m`
  if (ms < 172_800_000) return `${Math.round(ms / 3_600_000)}h`
  return `${Math.round(ms / 86_400_000)}d`
}

function greeting(name: string) {
  const h = new Date().getHours()
  const part = h < 5 ? 'Working late' : h < 12 ? 'Good morning' : h < 18 ? 'Good afternoon' : 'Good evening'
  return `${part}, ${name.split(/\s|@/)[0]}`
}

/** Opens a ticket on its project's Mission page. */
function ticketLink(t: WorkTask) {
  return `/project/${t.project_id}?ticket=${t.id}`
}

function Velocity({ days }: { days: VelocityDay[] }) {
  const short = (d: string) => new Date(d + 'T12:00').toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  return <BarChart ariaLabel="Tickets you completed per day" bars={days.map((d, i) => {
    const today = i === days.length - 1
    return { key: d.day, value: d.completed, title: `${d.day}: ${d.completed} completed`, highlight: today,
      label: today ? 'today' : i === 0 ? short(d.day) : '' }
  })} />
}

export function MyWork() {
  const { me } = useAuth()
  const navigate = useNavigate()
  const [work, setWork] = useState<MyWorkData | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [velocity, setVelocity] = useState<VelocityDay[]>([])
  const [draft, setDraft] = useState<DayReport | null>(null)

  useEffect(() => {
    api.myWork().then(setWork).catch(e => setError(e instanceof Error ? e.message : 'Failed to load'))
    const to = new Date(), from = new Date(Date.now() - 13 * 86_400_000)
    api.velocity({ from: ymd(from), to: ymd(to), tz: TZ }).then(setVelocity).catch(() => {})
    api.dayReportDraft(ymd(to), TZ).then(setDraft).catch(() => {})
  }, [])

  // What needs me first: running, questions, ready, then waiting and scheduled.
  const owned = [...(work?.owned ?? [])].sort((a, b) =>
    NEXT_UP.indexOf(displayState(a)) - NEXT_UP.indexOf(displayState(b)) || Number(b.priority) - Number(a.priority) || a.id - b.id)
  const flags = work?.flags ?? []
  const agents = work?.agents ?? []
  const working = agents.filter(a => a.working.length > 0).length
  const inProgress = owned.filter(t => t.state === 'in_progress').length
  const projectsCount = new Set(owned.map(t => t.project_id)).size
  // Agents in my harness work under my token, so my in-progress tickets are
  // theirs; agent accounts (their own identity) are counted on top.
  const flying = owned.filter(t => t.state === 'in_progress')
  const accountTickets = agents.reduce((n, a) => n + a.working.length, 0)

  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Overview" title={me ? greeting(me.name || me.email) : 'My work'}
        meta={new Date().toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' })}
        actions={<><Link to="/projects" className="amp-btn">All projects</Link><Link to="/reports" className="amp-btn amp-btn-primary">Day report</Link></>} />
      <div className="p-7 pt-5 grid gap-4">
        {error && <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">{error}</div>}
        {!work && !error && <div className="flex justify-center py-16"><Loader2 size={20} className="animate-spin text-ink-3" /></div>}
        {work && <>
          <Readouts items={[
            { label: 'My open tickets', value: owned.length, sub: `${inProgress} in progress · ${projectsCount} project${projectsCount === 1 ? '' : 's'}` },
            { label: 'Agents working', value: flying.length + accountTickets, sub: flying.length + accountTickets ? `on my tickets${agents.length ? ` · ${working}/${agents.length} agent accounts` : ''}` : 'nothing running', color: 'var(--st-prog)' },
            { label: 'Questions', value: flags.length, sub: flags.length ? 'agents waiting on an answer' : 'nothing needs a person', alarm: flags.length > 0 },
            { label: 'Done in 24h', value: work.done_24h + agents.reduce((a, x) => a + x.done_24h, 0), sub: 'by me and my agents' },
          ]} />

          <div className="grid gap-4 items-start grid-cols-1 xl:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
            <div className="grid grid-cols-1 gap-4 min-w-0">
              <Panel label="Questions" title={flags.length ? 'Agents waiting on an answer' : 'No questions'} right={<span>blocked with a reason</span>}>
                {flags.length === 0
                  ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">When an agent stops on a question in any project you can see, it shows up here.</p>
                  : flags.slice(0, 8).map(t => (
                    <div key={t.id} className="grid grid-cols-[22px_1fr_auto] gap-x-3 gap-y-1 px-4 py-3 border-t border-line first:border-t-0">
                      <span className="row-span-2 w-[22px] h-[20px] bg-st-flag text-white grid place-items-center text-[11px] font-bold pt-1"
                        style={{ clipPath: 'polygon(50% 0, 100% 100%, 0 100%)' }} aria-hidden="true">!</span>
                      <p className="m-0 text-[13px] leading-snug text-ink">{t.block_reason}</p>
                      <Link to={ticketLink(t)} className="amp-btn amp-btn-accent amp-btn-sm row-span-2 self-start">Answer</Link>
                      <span className="amp-num text-[11px] text-ink-3">#{t.id} · {t.project_code} · {t.agent_id || 'agent'} · waiting {ago(t.updated_at)}</span>
                    </div>
                  ))}
                {flags.length > 8 && <p className="m-0 px-4 py-2.5 border-t border-line text-[12px] text-ink-3">+{flags.length - 8} more</p>}
              </Panel>

              <Panel label="Mine" title="Tickets I own" right={<span>{owned.length} open</span>}>
                {owned.length === 0
                  ? <p className="m-0 px-4 py-4 text-[13px] text-ink-2">You don't own any open tickets. Claim one from a project's board, or dispatch one to your agent.</p>
                  : (
                    <div className="overflow-x-auto">
                    <table className="w-full border-collapse text-[13px]">
                      <thead><tr className="bg-surface-2">
                        {['', 'ID', 'Ticket', 'State', 'Agent', ...(projectsCount > 1 ? ['Project'] : [])].map(h => <th key={h} className="amp-label text-left px-3 py-2.5 border-b border-line whitespace-nowrap">{h}</th>)}
                      </tr></thead>
                      <tbody>
                        {owned.slice(0, 12).map(t => (
                          <tr key={t.id} className="hover:bg-surface-2 cursor-pointer" onClick={() => navigate(ticketLink(t))}>
                            <td className="px-3 h-[38px] border-b border-line"><PriorityMark priority={t.priority} /></td>
                            <td className="px-3 border-b border-line amp-num text-ink-3">#{t.id}</td>
                            <td className="px-3 border-b border-line max-w-[320px] truncate"><Link to={ticketLink(t)} className="text-ink hover:underline" onClick={e => e.stopPropagation()}>{t.name}</Link></td>
                            <td className="px-3 border-b border-line"><StateTag state={displayState(t)} /></td>
                            <td className="px-3 border-b border-line whitespace-nowrap">{t.agent_id
                              ? <span className="inline-flex items-center gap-2"><Avatar name={t.agent_id} kind="agent" size={20} />{t.agent_id}</span>
                              : <span className="amp-num text-[12px] text-ink-3 block max-w-[160px] truncate">{t.assigned_to || '—'}</span>}</td>
                            {projectsCount > 1 && <td className="px-3 border-b border-line amp-num whitespace-nowrap max-w-[140px] truncate" title={t.project_code}>{t.project_code}</td>}
                          </tr>
                        ))}
                      </tbody>
                    </table>
                    </div>
                  )}
                {owned.length > 12 && <p className="m-0 px-4 py-2.5 text-[12px] text-ink-3">+{owned.length - 12} more</p>}
              </Panel>
            </div>

            <div className="grid grid-cols-1 gap-4 min-w-0">
              <Panel label="My agents" title="On station" right={<Link to="/access" className="underline underline-offset-2 hover:text-ink">manage</Link>}>
                {flying.map(t => (
                  <Link key={t.id} to={ticketLink(t)} className="grid grid-cols-[auto_1fr_auto] gap-x-2.5 gap-y-0.5 items-center px-4 py-2.5 border-t border-line first:border-t-0 hover:bg-surface-2">
                    <Avatar name={t.agent_id || 'agent'} kind="agent" />
                    <span className="text-[12.5px] font-semibold text-ink truncate">{t.agent_id || 'agent'}</span>
                    <span className="amp-num text-[11px] text-ink-3">{ago(t.dispatched_at)}</span>
                    <span className="col-start-2 col-span-2 text-[11.5px] text-ink-2 truncate">#{t.id} {t.name}{projectsCount > 1 ? ` · ${t.project_code}` : ''}</span>
                  </Link>
                ))}
                {flying.length === 0 && agents.length === 0 &&
                  <p className="m-0 px-4 py-4 text-[13px] text-ink-2">Nothing running. When your harness dispatches one of your tickets, its agent shows up here.</p>}
                {agents.map(a => {
                    const name = a.user.display_name || a.user.email
                    const t = a.working[0]
                    return (
                      <div key={a.user.id} className="grid grid-cols-[auto_1fr_auto] gap-x-2.5 gap-y-0.5 items-center px-4 py-2.5 border-t border-line first:border-t-0">
                        <Avatar name={name} kind="agent" />
                        <span className="text-[12.5px] font-semibold text-ink truncate">{name}</span>
                        <span className="amp-num text-[11px] text-ink-3">{a.done_24h} done · 24h</span>
                        <span className="col-start-2 col-span-2 text-[11.5px] text-ink-2 truncate">
                          {t ? <Link to={ticketLink(t)} className="hover:underline">#{t.id} {t.name} · {t.project_code} · {ago(t.dispatched_at)}</Link>
                            : a.user.disabled_at ? 'deactivated' : 'idle'}
                          {a.working.length > 1 && ` (+${a.working.length - 1})`}
                        </span>
                      </div>
                    )
                  })}
              </Panel>

              <Panel label="Velocity" title="Completed per day" right={<span>last 14 days</span>}>
                <div className="px-4 py-3">{velocity.length ? <Velocity days={velocity} /> : <p className="m-0 text-[13px] text-ink-2">No completions yet.</p>}</div>
              </Panel>

              <Panel label="Day report" title={draft?.saved ? 'Today, saved' : 'Today, draft'} right={<Link to="/reports" className="amp-btn amp-btn-sm">{draft?.saved ? 'View' : 'Write'}</Link>}>
                {draft ? (
                  <div className="px-4 py-3 grid gap-2 text-[13px]">
                    <div className="flex flex-wrap gap-x-5 gap-y-1">
                      {([['completed', 'done'], ['started', 'started'], ['in_progress', 'in progress'], ['blocked', 'blocked']] as const).map(([k, l]) => (
                        <span key={k}><b className="amp-num text-[16px] font-normal text-ink">{draft.data.stats[k]}</b> <span className="text-ink-2">{l}</span></span>
                      ))}
                    </div>
                    {draft.summary ? <p className="m-0 text-ink-2 leading-relaxed">{draft.summary}</p>
                      : <p className="m-0 text-ink-3">Built from today's activity. Add a summary and save it to share your day.</p>}
                  </div>
                ) : <p className="m-0 px-4 py-4 text-[13px] text-ink-2">No activity yet today.</p>}
              </Panel>
            </div>
          </div>
        </>}
      </div>
    </div>
  )
}
