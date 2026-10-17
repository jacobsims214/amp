import { useState, useEffect, useMemo } from 'react'
import { useParams } from 'react-router-dom'
import { Loader2, CheckCircle2, Clock, Zap, Ban, TrendingUp, Calendar } from 'lucide-react'
import { api } from '../api/client'
import { useBoardData } from '../hooks/useBoardData'
import { ProjectNav } from '../components/ProjectNav'
import type { ActivityLog } from '../types'

type Range = '1d' | '7d' | '30d' | 'all'

function rangeToSince(r: Range): string {
  if (r === 'all') return ''
  const d = new Date()
  d.setDate(d.getDate() - (r === '1d' ? 1 : r === '7d' ? 7 : 30))
  return d.toISOString()
}

function relativeTime(iso: string) {
  const diff = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(diff / 60000)
  const hrs = Math.floor(mins / 60)
  const days = Math.floor(hrs / 24)
  if (days > 0) return `${days}d ago`
  if (hrs > 0) return `${hrs}h ago`
  if (mins > 0) return `${mins}m ago`
  return 'just now'
}

function formatDate(iso: string) {
  return new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

function duration(start: string, end: string): string {
  const ms = new Date(end).getTime() - new Date(start).getTime()
  const mins = Math.floor(ms / 60000)
  const hrs = Math.floor(mins / 60)
  const days = Math.floor(hrs / 24)
  if (days > 0) return `${days}d ${hrs % 24}h`
  if (hrs > 0) return `${hrs}h ${mins % 60}m`
  return `${mins}m`
}

function formatMs(ms: number): string {
  const mins = Math.floor(ms / 60000)
  const hrs = Math.floor(mins / 60)
  const days = Math.floor(hrs / 24)
  if (days > 0) return `${days}d ${hrs % 24}h`
  if (hrs > 0) return `${hrs}h ${mins % 60}m`
  return `${mins}m`
}

const ACTION_CONFIG: Record<string, { dot: string; text: string; label: string }> = {
  created:    { dot: 'var(--ink-2)', text: 'text-ink-2',  label: 'Created'    },
  dispatched: { dot: 'var(--link)', text: 'text-link',  label: 'Dispatched' },
  completed:  { dot: 'var(--success)', text: 'text-success',  label: 'Completed'  },
  blocked:    { dot: 'var(--danger)', text: 'text-danger-ink',  label: 'Blocked'    },
  unblocked:  { dot: 'var(--warning-ink)', text: 'text-warning-ink',  label: 'Unblocked'  },
  comment:    { dot: 'var(--line-str)', text: 'text-ink-3',  label: 'Comment'    },
}

const RANGE_LABELS: Record<Range, string> = {
  '1d': 'Today', '7d': 'This week', '30d': 'This month', 'all': 'All time',
}

const ACTION_FILTERS = ['all', 'created', 'dispatched', 'completed', 'blocked', 'comment'] as const

export function Report() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)

  const [range, setRange] = useState<Range>('7d')
  const [activity, setActivity] = useState<ActivityLog[]>([])
  const [loading, setLoading] = useState(true)
  const [actionFilter, setActionFilter] = useState<string>('all')

  const { tasks } = useBoardData(pid)
  const taskById = useMemo(() => new Map(tasks.map(t => [t.id, t])), [tasks])

  useEffect(() => {
    setLoading(true)
    api.getProjectActivity(pid, rangeToSince(range))
      .then(setActivity)
      .finally(() => setLoading(false))
  }, [pid, range])

  const stats = useMemo(() => {
    const completed  = activity.filter(a => a.action === 'completed')
    const dispatched = activity.filter(a => a.action === 'dispatched')
    const blocked    = activity.filter(a => a.action === 'blocked')
    const cycleTimes: number[] = []
    for (const c of completed) {
      const task = taskById.get(c.task_id)
      if (task?.created_at && task?.completed_at)
        cycleTimes.push(new Date(task.completed_at).getTime() - new Date(task.created_at).getTime())
    }
    const avgCycleMs = cycleTimes.length > 0
      ? cycleTimes.reduce((a, b) => a + b, 0) / cycleTimes.length : null
    return { completed: completed.length, dispatched: dispatched.length, blocked: blocked.length, avgCycleMs }
  }, [activity, taskById])

  const filtered = useMemo(() =>
    actionFilter === 'all' ? activity : activity.filter(a => a.action === actionFilter)
  , [activity, actionFilter])

  const completedTasks = useMemo(() => {
    return tasks
      .filter(t => t.state === 'completed' && t.completed_at)
      .filter(t => {
        if (range === 'all') return true
        const since = rangeToSince(range)
        return since ? new Date(t.completed_at!).getTime() >= new Date(since).getTime() : true
      })
      .sort((a, b) => new Date(b.completed_at!).getTime() - new Date(a.completed_at!).getTime())
  }, [tasks, range])

  return (
    <div className="flex flex-col h-full overflow-hidden" style={{ background: 'var(--bg)' }}>
      {/* Header */}
      <ProjectNav
        projectId={pid}
        currentView="report"
        rightSlot={
          <div className="flex items-center gap-1 p-1 rounded-lg"
            style={{ background: 'var(--bg)', border: '1px solid var(--line)' }}>
            {(['1d', '7d', '30d', 'all'] as Range[]).map(r => (
              <button key={r} onClick={() => setRange(r)}
                className="px-3 py-1 rounded-md text-xs font-semibold transition-all cursor-pointer"
                style={range === r
                  ? { background: 'var(--surface-3)', border: '1px solid var(--line-str)', color: 'var(--ink)' }
                  : { color: 'var(--ink-3)' }}
                onMouseEnter={e => { if (range !== r) (e.currentTarget as HTMLElement).style.color = 'var(--ink-2)' }}
                onMouseLeave={e => { if (range !== r) (e.currentTarget as HTMLElement).style.color = 'var(--ink-3)' }}>
                {RANGE_LABELS[r]}
              </button>
            ))}
          </div>
        }
      />

      <div className="flex-1 overflow-y-auto">
        <div className="max-w-5xl mx-auto px-5 py-7 space-y-8">

          {/* ── Stat cards ── */}
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-3">
            <StatCard
              icon={<CheckCircle2 size={18}/>}
              label="Completed" value={stats.completed}
              accent="var(--success)" accentBg="color-mix(in srgb, var(--success) 10%, transparent)" accentBorder="color-mix(in srgb, var(--success) 20%, transparent)"
            />
            <StatCard
              icon={<Zap size={18}/>}
              label="Dispatched" value={stats.dispatched}
              accent="var(--link)" accentBg="color-mix(in srgb, var(--link) 10%, transparent)" accentBorder="color-mix(in srgb, var(--link) 20%, transparent)"
            />
            <StatCard
              icon={<Ban size={18}/>}
              label="Blocked events" value={stats.blocked}
              accent="var(--danger-ink)" accentBg="color-mix(in srgb, var(--danger) 10%, transparent)" accentBorder="color-mix(in srgb, var(--danger) 20%, transparent)"
            />
            <StatCard
              icon={<TrendingUp size={18}/>}
              label="Avg cycle time"
              value={stats.avgCycleMs !== null ? formatMs(stats.avgCycleMs) : '—'}
              accent="var(--warning-ink)" accentBg="color-mix(in srgb, var(--warning) 10%, transparent)" accentBorder="color-mix(in srgb, var(--warning) 20%, transparent)"
            />
          </div>

          {/* ── Completed tasks table ── */}
          {completedTasks.length > 0 && (
            <section>
              <SectionHeader icon={<CheckCircle2 size={14} className="text-success"/>}
                label="Completed tasks" count={completedTasks.length} />
              <div className="rounded-xl overflow-hidden" style={{ border: '1px solid var(--line)' }}>
                <table className="w-full text-xs">
                  <thead>
                    <tr style={{ background: 'var(--surface)', borderBottom: '1px solid var(--line)' }}>
                      {['#', 'Task', 'Agent', 'Completed', 'Cycle time'].map(h => (
                        <th key={h} className="text-left px-4 py-3 text-ink-3 font-bold uppercase tracking-widest text-[10px]">{h}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {completedTasks.map((task, i) => (
                      <tr key={task.id}
                        className="transition-colors"
                        style={{
                          borderBottom: i < completedTasks.length - 1 ? '1px solid var(--line-sub)' : 'none',
                          background: i % 2 === 1 ? 'color-mix(in srgb, var(--ink) 20%, transparent)' : 'transparent',
                        }}
                        onMouseEnter={e => (e.currentTarget as HTMLElement).style.background = 'var(--surface)'}
                        onMouseLeave={e => (e.currentTarget as HTMLElement).style.background = i % 2 === 1 ? 'color-mix(in srgb, var(--ink) 20%, transparent)' : 'transparent'}
                      >
                        <td className="px-4 py-2.5 text-ink-3 font-mono tabular-nums">{task.id}</td>
                        <td className="px-4 py-2.5 text-ink font-medium max-w-[260px]">
                          <span className="truncate block">{task.name}</span>
                        </td>
                        <td className="px-4 py-2.5 text-ink-2 font-mono">{task.agent_id ?? '—'}</td>
                        <td className="px-4 py-2.5 text-success tabular-nums">{formatDate(task.completed_at!)}</td>
                        <td className="px-4 py-2.5 text-warning-ink font-medium tabular-nums">
                          {task.dispatched_at ? duration(task.dispatched_at, task.completed_at!) : duration(task.created_at, task.completed_at!)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </section>
          )}

          {/* ── Activity feed ── */}
          <section>
            <div className="flex items-center justify-between mb-4 flex-wrap gap-3">
              <SectionHeader icon={<Calendar size={14} className="text-ink-2"/>}
                label="Activity feed" count={filtered.length} />
              {/* Action filter pills */}
              <div className="flex items-center gap-1 p-1 rounded-lg flex-wrap"
                style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}>
                {ACTION_FILTERS.map(a => {
                  const conf = ACTION_CONFIG[a]
                  const isActive = actionFilter === a
                  return (
                    <button key={a} onClick={() => setActionFilter(a)}
                      className="flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-semibold capitalize transition-all cursor-pointer"
                      style={{
                        background: isActive ? 'var(--surface-3)' : 'transparent',
                        color: isActive ? (conf ? conf.dot : 'var(--ink)') : 'var(--ink-3)',
                        border: isActive ? '1px solid var(--line-str)' : '1px solid transparent',
                      }}
                      onMouseEnter={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = 'var(--ink-2)' }}
                      onMouseLeave={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = 'var(--ink-3)' }}>
                      {a !== 'all' && conf && (
                        <span className="w-1.5 h-1.5 rounded-full flex-shrink-0" style={{ background: conf.dot }}/>
                      )}
                      {a === 'all' ? 'All' : conf?.label ?? a}
                    </button>
                  )
                })}
              </div>
            </div>

            {loading ? (
              <div className="flex justify-center py-10">
                <Loader2 size={20} className="text-primary animate-spin"/>
              </div>
            ) : filtered.length === 0 ? (
              <div className="text-center py-12 rounded-xl" style={{ border: '1px solid var(--line-sub)' }}>
                <p className="text-sm text-ink-3">No activity in this period</p>
              </div>
            ) : (
              <div className="rounded-xl overflow-hidden" style={{ border: '1px solid var(--line)' }}>
                {filtered.map((entry, i) => {
                  const conf = ACTION_CONFIG[entry.action] ?? ACTION_CONFIG.comment
                  const task = taskById.get(entry.task_id)
                  return (
                    <div key={entry.id}
                      className="flex items-start gap-3 px-4 py-3 transition-colors"
                      style={{
                        borderBottom: i < filtered.length - 1 ? '1px solid var(--line-sub)' : 'none',
                      }}
                      onMouseEnter={e => (e.currentTarget as HTMLElement).style.background = 'var(--surface)'}
                      onMouseLeave={e => (e.currentTarget as HTMLElement).style.background = 'transparent'}
                    >
                      <span className="w-2 h-2 rounded-full mt-1.5 flex-shrink-0"
                        style={{ background: conf.dot, boxShadow: `0 0 4px ${conf.dot}50` }}/>
                      <div className="flex-1 min-w-0">
                        <div className="flex items-center gap-2 flex-wrap">
                          <span className="text-xs font-semibold text-ink">{entry.actor}</span>
                          <span className={`text-xs font-medium capitalize ${conf.text}`}>{entry.action.replace('_', ' ')}</span>
                          {task && (
                            <span className="text-xs text-ink-2 truncate">
                              <span className="text-ink-3 font-mono">#{task.id}</span>
                              {' '}{task.name}
                            </span>
                          )}
                          {entry.from_state && entry.to_state && entry.action !== 'comment' && (
                            <span className="text-xs text-ink-3">{entry.from_state} → {entry.to_state}</span>
                          )}
                        </div>
                        {entry.action === 'comment' && entry.detail && (
                          <p className="text-xs text-ink-2 mt-1 line-clamp-2 leading-relaxed bg-bg rounded-lg px-2.5 py-1.5 border border-line-sub mt-1.5">
                            {entry.detail}
                          </p>
                        )}
                      </div>
                      <span className="text-xs text-ink-3 flex-shrink-0 flex items-center gap-1 tabular-nums">
                        <Clock size={9}/>
                        {relativeTime(entry.created_at)}
                      </span>
                    </div>
                  )
                })}
              </div>
            )}
          </section>

        </div>
      </div>
    </div>
  )
}

/* ── Stat card ────────────────────────────────────────────────────────────── */
function StatCard({ icon, label, value, accent, accentBg, accentBorder }: {
  icon: React.ReactNode; label: string; value: number | string
  accent: string; accentBg: string; accentBorder: string
}) {
  return (
    <div className="rounded-2xl p-5 transition-all"
      style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}>
      <div className="flex items-center justify-between mb-3">
        <span className="text-xs font-semibold text-ink-3 uppercase tracking-widest">{label}</span>
        <span className="flex-shrink-0 p-2 rounded-xl" style={{ background: accentBg, border: `1px solid ${accentBorder}`, color: accent }}>
          {icon}
        </span>
      </div>
      <div className="text-3xl font-bold tabular-nums" style={{ color: accent, fontVariantNumeric: 'tabular-nums' }}>
        {value}
      </div>
    </div>
  )
}

/* ── Section header ───────────────────────────────────────────────────────── */
function SectionHeader({ icon, label, count }: { icon: React.ReactNode; label: string; count: number }) {
  return (
    <h2 className="text-sm font-bold text-ink mb-4 flex items-center gap-2">
      {icon}
      {label}
      <span className="text-xs font-normal text-ink-3 tabular-nums">({count})</span>
    </h2>
  )
}
