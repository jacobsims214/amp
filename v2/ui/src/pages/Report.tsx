import { useState, useEffect, useMemo } from 'react'
import { useParams, Link } from 'react-router-dom'
import { ArrowLeft, Loader2, CheckCircle2, Clock, Zap, Ban, TrendingUp, Calendar } from 'lucide-react'
import { api } from '../api/client'
import { useBoardData } from '../hooks/useBoardData'
import type { ActivityLog } from '../types'

// ---- Date range presets ----
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
  return new Date(iso).toLocaleDateString('en-US', {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
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

const ACTION_COLORS: Record<string, { dot: string; text: string }> = {
  created:    { dot: 'bg-[#8b949e]',  text: 'text-[#8b949e]' },
  dispatched: { dot: 'bg-[#58a6ff]',  text: 'text-[#58a6ff]' },
  completed:  { dot: 'bg-[#3fb950]',  text: 'text-[#3fb950]' },
  blocked:    { dot: 'bg-[#f85149]',  text: 'text-[#f85149]' },
  unblocked:  { dot: 'bg-[#e3b341]',  text: 'text-[#e3b341]' },
  comment:    { dot: 'bg-[#484f58]',  text: 'text-[#484f58]' },
}

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

  // ---- Stats ----
  const stats = useMemo(() => {
    const completed = activity.filter(a => a.action === 'completed')
    const dispatched = activity.filter(a => a.action === 'dispatched')
    const blocked = activity.filter(a => a.action === 'blocked')
    const comments = activity.filter(a => a.action === 'comment')

    // Cycle time: created → completed for tasks that completed in this window
    const cycleTimes: number[] = []
    for (const c of completed) {
      const task = taskById.get(c.task_id)
      if (task?.created_at && task?.completed_at) {
        cycleTimes.push(new Date(task.completed_at).getTime() - new Date(task.created_at).getTime())
      }
    }
    const avgCycleMs = cycleTimes.length > 0
      ? cycleTimes.reduce((a, b) => a + b, 0) / cycleTimes.length
      : null

    return { completed: completed.length, dispatched: dispatched.length, blocked: blocked.length, comments: comments.length, avgCycleMs }
  }, [activity, taskById])

  // ---- Filtered feed ----
  const filtered = useMemo(() => {
    if (actionFilter === 'all') return activity
    return activity.filter(a => a.action === actionFilter)
  }, [activity, actionFilter])

  // ---- Completed tasks with full lifecycle ----
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

  function formatMs(ms: number): string {
    const mins = Math.floor(ms / 60000)
    const hrs = Math.floor(mins / 60)
    const days = Math.floor(hrs / 24)
    if (days > 0) return `${days}d ${hrs % 24}h avg`
    if (hrs > 0) return `${hrs}h ${mins % 60}m avg`
    return `${mins}m avg`
  }

  return (
    <div className="flex flex-col h-screen bg-[#0d1117] overflow-hidden">
      {/* Header */}
      <header className="flex items-center gap-3 px-5 py-3 border-b border-[#21262d] bg-[#161b22] flex-shrink-0">
        <Link to={`/project/${pid}`} className="flex items-center gap-1.5 text-[#8b949e] hover:text-[#e6edf3] transition-colors">
          <ArrowLeft size={14} />
        </Link>
        <div className="w-px h-4 bg-[#30363d]" />
        <span className="text-sm font-semibold text-[#e6edf3]">Report</span>
        <div className="ml-auto flex items-center gap-1 bg-[#0d1117] border border-[#30363d] rounded-md p-1">
          {(['1d', '7d', '30d', 'all'] as Range[]).map(r => (
            <button
              key={r}
              onClick={() => setRange(r)}
              className={`px-3 py-1 rounded text-xs font-medium transition-colors ${
                range === r
                  ? 'bg-[#21262d] text-[#e6edf3]'
                  : 'text-[#8b949e] hover:text-[#e6edf3]'
              }`}
            >
              {r === 'all' ? 'All time' : r === '1d' ? 'Today' : r === '7d' ? 'This week' : 'This month'}
            </button>
          ))}
        </div>
      </header>

      <div className="flex-1 overflow-y-auto">
        <div className="max-w-5xl mx-auto px-5 py-6 space-y-6">

          {/* ---- Stat cards ---- */}
          <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
            <StatCard icon={<CheckCircle2 size={16} className="text-[#3fb950]" />} label="Completed" value={stats.completed} color="text-[#3fb950]" />
            <StatCard icon={<Zap size={16} className="text-[#58a6ff]" />} label="Dispatched" value={stats.dispatched} color="text-[#58a6ff]" />
            <StatCard icon={<Ban size={16} className="text-[#f85149]" />} label="Blocked events" value={stats.blocked} color="text-[#f85149]" />
            <StatCard
              icon={<TrendingUp size={16} className="text-[#e3b341]" />}
              label="Avg cycle time"
              value={stats.avgCycleMs !== null ? formatMs(stats.avgCycleMs) : '—'}
              color="text-[#e3b341]"
            />
          </div>

          {/* ---- Completed tasks table ---- */}
          {completedTasks.length > 0 && (
            <div>
              <h2 className="text-sm font-semibold text-[#e6edf3] mb-3 flex items-center gap-2">
                <CheckCircle2 size={14} className="text-[#3fb950]" />
                Completed tasks
                <span className="text-xs text-[#484f58] font-normal">({completedTasks.length})</span>
              </h2>
              <div className="rounded-lg border border-[#21262d] overflow-hidden">
                <table className="w-full text-xs">
                  <thead>
                    <tr className="border-b border-[#21262d] bg-[#161b22]">
                      <th className="text-left px-4 py-2.5 text-[#8b949e] font-medium">#</th>
                      <th className="text-left px-4 py-2.5 text-[#8b949e] font-medium">Task</th>
                      <th className="text-left px-4 py-2.5 text-[#8b949e] font-medium">Agent</th>
                      <th className="text-left px-4 py-2.5 text-[#8b949e] font-medium">Created</th>
                      <th className="text-left px-4 py-2.5 text-[#8b949e] font-medium">Completed</th>
                      <th className="text-left px-4 py-2.5 text-[#8b949e] font-medium">Cycle time</th>
                    </tr>
                  </thead>
                  <tbody>
                    {completedTasks.map((task, i) => (
                      <tr
                        key={task.id}
                        className={`border-b border-[#21262d] last:border-0 hover:bg-[#161b22] transition-colors ${i % 2 === 0 ? '' : 'bg-[#0d1117]/50'}`}
                      >
                        <td className="px-4 py-2.5 text-[#484f58] font-mono">{task.id}</td>
                        <td className="px-4 py-2.5 text-[#e6edf3] max-w-[240px]">
                          <span className="truncate block">{task.name}</span>
                        </td>
                        <td className="px-4 py-2.5 text-[#8b949e] font-mono">{task.agent_id ?? '—'}</td>
                        <td className="px-4 py-2.5 text-[#8b949e]">{formatDate(task.created_at)}</td>
                        <td className="px-4 py-2.5 text-[#3fb950]">{formatDate(task.completed_at!)}</td>
                        <td className="px-4 py-2.5 text-[#e3b341]">
                          {task.dispatched_at
                            ? duration(task.dispatched_at, task.completed_at!)
                            : duration(task.created_at, task.completed_at!)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          {/* ---- Activity feed ---- */}
          <div>
            <div className="flex items-center justify-between mb-3">
              <h2 className="text-sm font-semibold text-[#e6edf3] flex items-center gap-2">
                <Calendar size={14} className="text-[#8b949e]" />
                Activity feed
                <span className="text-xs text-[#484f58] font-normal">({filtered.length})</span>
              </h2>
              <div className="flex items-center gap-1 bg-[#0d1117] border border-[#30363d] rounded-md p-1">
                {['all', 'created', 'dispatched', 'completed', 'blocked', 'comment'].map(a => (
                  <button
                    key={a}
                    onClick={() => setActionFilter(a)}
                    className={`px-2.5 py-1 rounded text-xs font-medium capitalize transition-colors ${
                      actionFilter === a
                        ? 'bg-[#21262d] text-[#e6edf3]'
                        : 'text-[#8b949e] hover:text-[#e6edf3]'
                    }`}
                  >
                    {a}
                  </button>
                ))}
              </div>
            </div>

            {loading ? (
              <div className="flex justify-center py-8">
                <Loader2 size={20} className="text-[#58a6ff] animate-spin" />
              </div>
            ) : filtered.length === 0 ? (
              <div className="text-center py-8 text-sm text-[#8b949e]">No activity in this period</div>
            ) : (
              <div className="space-y-px">
                {filtered.map(entry => {
                  const c = ACTION_COLORS[entry.action] ?? ACTION_COLORS.comment
                  const task = taskById.get(entry.task_id)
                  return (
                    <div
                      key={entry.id}
                      className="flex items-start gap-3 px-4 py-2.5 rounded-md hover:bg-[#161b22] transition-colors"
                    >
                      <span className={`w-2 h-2 rounded-full mt-1.5 flex-shrink-0 ${c.dot}`} />
                      <div className="flex-1 min-w-0">
                        <div className="flex items-center gap-2 flex-wrap">
                          <span className="text-xs font-medium text-[#e6edf3]">{entry.actor}</span>
                          <span className={`text-xs capitalize ${c.text}`}>{entry.action.replace('_', ' ')}</span>
                          {task && (
                            <span className="text-xs text-[#8b949e] truncate">
                              <span className="text-[#484f58] font-mono">#{task.id}</span>
                              {' '}
                              {task.name}
                            </span>
                          )}
                          {entry.from_state && entry.to_state && entry.action !== 'comment' && (
                            <span className="text-xs text-[#484f58]">
                              {entry.from_state} → {entry.to_state}
                            </span>
                          )}
                        </div>
                        {entry.action === 'comment' && entry.detail && (
                          <p className="text-xs text-[#8b949e] mt-1 line-clamp-2 leading-relaxed">
                            {entry.detail}
                          </p>
                        )}
                      </div>
                      <span className="text-xs text-[#484f58] flex-shrink-0 flex items-center gap-1">
                        <Clock size={10} />
                        {relativeTime(entry.created_at)}
                      </span>
                    </div>
                  )
                })}
              </div>
            )}
          </div>

        </div>
      </div>
    </div>
  )
}

function StatCard({ icon, label, value, color }: {
  icon: React.ReactNode
  label: string
  value: number | string
  color: string
}) {
  return (
    <div className="bg-[#161b22] border border-[#21262d] rounded-lg p-4">
      <div className="flex items-center gap-2 mb-2">
        {icon}
        <span className="text-xs text-[#8b949e]">{label}</span>
      </div>
      <div className={`text-2xl font-bold ${color}`}>{value}</div>
    </div>
  )
}
