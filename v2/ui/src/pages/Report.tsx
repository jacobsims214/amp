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
  created:    { dot: '#7E91A8', text: 'text-[#7E91A8]',  label: 'Created'    },
  dispatched: { dot: '#818CF8', text: 'text-[#818CF8]',  label: 'Dispatched' },
  completed:  { dot: '#10B981', text: 'text-[#10B981]',  label: 'Completed'  },
  blocked:    { dot: '#EF4444', text: 'text-[#F87171]',  label: 'Blocked'    },
  unblocked:  { dot: '#FBBF24', text: 'text-[#FBBF24]',  label: 'Unblocked'  },
  comment:    { dot: '#283A57', text: 'text-[#3D5068]',  label: 'Comment'    },
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
    <div className="flex flex-col h-screen overflow-hidden" style={{ background: '#08101F' }}>
      {/* Header */}
      <ProjectNav
        projectId={pid}
        currentView="report"
        rightSlot={
          <div className="flex items-center gap-1 p-1 rounded-lg"
            style={{ background: '#08101F', border: '1px solid #1E2C45' }}>
            {(['1d', '7d', '30d', 'all'] as Range[]).map(r => (
              <button key={r} onClick={() => setRange(r)}
                className="px-3 py-1 rounded-md text-xs font-semibold transition-all cursor-pointer"
                style={range === r
                  ? { background: '#172540', border: '1px solid #283A57', color: '#DDE6F0' }
                  : { color: '#3D5068' }}
                onMouseEnter={e => { if (range !== r) (e.currentTarget as HTMLElement).style.color = '#7E91A8' }}
                onMouseLeave={e => { if (range !== r) (e.currentTarget as HTMLElement).style.color = '#3D5068' }}>
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
              accent="#10B981" accentBg="rgba(16,185,129,0.1)" accentBorder="rgba(16,185,129,0.2)"
            />
            <StatCard
              icon={<Zap size={18}/>}
              label="Dispatched" value={stats.dispatched}
              accent="#818CF8" accentBg="rgba(99,102,241,0.1)" accentBorder="rgba(99,102,241,0.2)"
            />
            <StatCard
              icon={<Ban size={18}/>}
              label="Blocked events" value={stats.blocked}
              accent="#F87171" accentBg="rgba(239,68,68,0.1)" accentBorder="rgba(239,68,68,0.2)"
            />
            <StatCard
              icon={<TrendingUp size={18}/>}
              label="Avg cycle time"
              value={stats.avgCycleMs !== null ? formatMs(stats.avgCycleMs) : '—'}
              accent="#FBBF24" accentBg="rgba(245,158,11,0.1)" accentBorder="rgba(245,158,11,0.2)"
            />
          </div>

          {/* ── Completed tasks table ── */}
          {completedTasks.length > 0 && (
            <section>
              <SectionHeader icon={<CheckCircle2 size={14} className="text-[#10B981]"/>}
                label="Completed tasks" count={completedTasks.length} />
              <div className="rounded-xl overflow-hidden" style={{ border: '1px solid #1E2C45' }}>
                <table className="w-full text-xs">
                  <thead>
                    <tr style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45' }}>
                      {['#', 'Task', 'Agent', 'Completed', 'Cycle time'].map(h => (
                        <th key={h} className="text-left px-4 py-3 text-[#3D5068] font-bold uppercase tracking-widest text-[10px]">{h}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {completedTasks.map((task, i) => (
                      <tr key={task.id}
                        className="transition-colors"
                        style={{
                          borderBottom: i < completedTasks.length - 1 ? '1px solid #192238' : 'none',
                          background: i % 2 === 1 ? 'rgba(13,23,38,0.4)' : 'transparent',
                        }}
                        onMouseEnter={e => (e.currentTarget as HTMLElement).style.background = '#0D1726'}
                        onMouseLeave={e => (e.currentTarget as HTMLElement).style.background = i % 2 === 1 ? 'rgba(13,23,38,0.4)' : 'transparent'}
                      >
                        <td className="px-4 py-2.5 text-[#3D5068] font-mono tabular-nums">{task.id}</td>
                        <td className="px-4 py-2.5 text-[#DDE6F0] font-medium max-w-[260px]">
                          <span className="truncate block">{task.name}</span>
                        </td>
                        <td className="px-4 py-2.5 text-[#7E91A8] font-mono">{task.agent_id ?? '—'}</td>
                        <td className="px-4 py-2.5 text-[#10B981] tabular-nums">{formatDate(task.completed_at!)}</td>
                        <td className="px-4 py-2.5 text-[#FBBF24] font-medium tabular-nums">
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
              <SectionHeader icon={<Calendar size={14} className="text-[#7E91A8]"/>}
                label="Activity feed" count={filtered.length} />
              {/* Action filter pills */}
              <div className="flex items-center gap-1 p-1 rounded-lg flex-wrap"
                style={{ background: '#0D1726', border: '1px solid #1E2C45' }}>
                {ACTION_FILTERS.map(a => {
                  const conf = ACTION_CONFIG[a]
                  const isActive = actionFilter === a
                  return (
                    <button key={a} onClick={() => setActionFilter(a)}
                      className="flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-semibold capitalize transition-all cursor-pointer"
                      style={{
                        background: isActive ? '#172540' : 'transparent',
                        color: isActive ? (conf ? conf.dot : '#DDE6F0') : '#3D5068',
                        border: isActive ? '1px solid #283A57' : '1px solid transparent',
                      }}
                      onMouseEnter={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = '#7E91A8' }}
                      onMouseLeave={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = '#3D5068' }}>
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
                <Loader2 size={20} className="text-[#6366F1] animate-spin"/>
              </div>
            ) : filtered.length === 0 ? (
              <div className="text-center py-12 rounded-xl" style={{ border: '1px solid #192238' }}>
                <p className="text-sm text-[#3D5068]">No activity in this period</p>
              </div>
            ) : (
              <div className="rounded-xl overflow-hidden" style={{ border: '1px solid #1E2C45' }}>
                {filtered.map((entry, i) => {
                  const conf = ACTION_CONFIG[entry.action] ?? ACTION_CONFIG.comment
                  const task = taskById.get(entry.task_id)
                  return (
                    <div key={entry.id}
                      className="flex items-start gap-3 px-4 py-3 transition-colors"
                      style={{
                        borderBottom: i < filtered.length - 1 ? '1px solid #192238' : 'none',
                      }}
                      onMouseEnter={e => (e.currentTarget as HTMLElement).style.background = '#0D1726'}
                      onMouseLeave={e => (e.currentTarget as HTMLElement).style.background = 'transparent'}
                    >
                      <span className="w-2 h-2 rounded-full mt-1.5 flex-shrink-0"
                        style={{ background: conf.dot, boxShadow: `0 0 4px ${conf.dot}50` }}/>
                      <div className="flex-1 min-w-0">
                        <div className="flex items-center gap-2 flex-wrap">
                          <span className="text-xs font-semibold text-[#DDE6F0]">{entry.actor}</span>
                          <span className={`text-xs font-medium capitalize ${conf.text}`}>{entry.action.replace('_', ' ')}</span>
                          {task && (
                            <span className="text-xs text-[#7E91A8] truncate">
                              <span className="text-[#3D5068] font-mono">#{task.id}</span>
                              {' '}{task.name}
                            </span>
                          )}
                          {entry.from_state && entry.to_state && entry.action !== 'comment' && (
                            <span className="text-xs text-[#3D5068]">{entry.from_state} → {entry.to_state}</span>
                          )}
                        </div>
                        {entry.action === 'comment' && entry.detail && (
                          <p className="text-xs text-[#7E91A8] mt-1 line-clamp-2 leading-relaxed bg-[#08101F] rounded-lg px-2.5 py-1.5 border border-[#192238] mt-1.5">
                            {entry.detail}
                          </p>
                        )}
                      </div>
                      <span className="text-xs text-[#3D5068] flex-shrink-0 flex items-center gap-1 tabular-nums">
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
      style={{ background: '#0D1726', border: '1px solid #1E2C45' }}>
      <div className="flex items-center justify-between mb-3">
        <span className="text-xs font-semibold text-[#3D5068] uppercase tracking-widest">{label}</span>
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
    <h2 className="text-sm font-bold text-[#DDE6F0] mb-4 flex items-center gap-2">
      {icon}
      {label}
      <span className="text-xs font-normal text-[#3D5068] tabular-nums">({count})</span>
    </h2>
  )
}
