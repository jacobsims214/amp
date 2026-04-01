import { useEffect, useState } from 'react'
import { X, Clock, User, UserCheck, GitBranch, CheckCircle2, AlertCircle, Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { StatusBadge } from './StatusBadge'
import type { Task, Comment, ActivityLog } from '../types'

interface Props {
  task: Task | null
  onClose: () => void
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

const ACTION_ICONS: Record<string, React.ReactNode> = {
  created:    <span className="w-2 h-2 rounded-full bg-[#8b949e] mt-1.5 flex-shrink-0" />,
  dispatched: <span className="w-2 h-2 rounded-full bg-[#58a6ff] mt-1.5 flex-shrink-0" />,
  completed:  <span className="w-2 h-2 rounded-full bg-[#3fb950] mt-1.5 flex-shrink-0" />,
  blocked:    <span className="w-2 h-2 rounded-full bg-[#f85149] mt-1.5 flex-shrink-0" />,
  unblocked:  <span className="w-2 h-2 rounded-full bg-[#e3b341] mt-1.5 flex-shrink-0" />,
  comment:    <span className="w-2 h-2 rounded-full bg-[#8b949e]/50 mt-1.5 flex-shrink-0 border border-[#8b949e]" />,
}

export function TaskDrawer({ task, onClose }: Props) {
  const [history, setHistory] = useState<ActivityLog[]>([])
  const [comments, setComments] = useState<Comment[]>([])
  const [tab, setTab] = useState<'history' | 'details'>('history')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (!task) return
    setLoading(true)
    Promise.all([api.getHistory(task.id), api.getComments(task.id)])
      .then(([h, c]) => { setHistory(h); setComments(c) })
      .finally(() => setLoading(false))
  }, [task?.id])

  if (!task) return null

  return (
    <>
      {/* Backdrop */}
      <div className="fixed inset-0 bg-black/50 z-40" onClick={onClose} />

      {/* Drawer */}
      <div className="fixed right-0 top-0 h-full w-[520px] bg-[#161b22] border-l border-[#30363d] z-50 flex flex-col shadow-2xl">
        {/* Header */}
        <div className="flex items-start justify-between gap-3 p-5 border-b border-[#30363d]">
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2 mb-1.5">
              <span className="text-xs text-[#8b949e] font-mono">#{task.id}</span>
              <StatusBadge state={task.state} />
              {task.priority === '2' && <span className="text-xs text-[#e3b341] font-medium">High</span>}
              {task.priority === '3' && <span className="text-xs text-[#f85149] font-medium">Critical</span>}
            </div>
            <h2 className="text-base font-semibold text-[#e6edf3] leading-snug">{task.name}</h2>
          </div>
          <button onClick={onClose} className="p-1.5 hover:bg-[#21262d] rounded-md transition-colors flex-shrink-0">
            <X size={16} className="text-[#8b949e]" />
          </button>
        </div>

        {/* Meta strip */}
        <div className="flex items-center gap-4 px-5 py-2.5 border-b border-[#21262d] bg-[#0d1117]/30 flex-wrap">
          {/* Planned assignee — shown before dispatch */}
          {task.assigned_to && !task.agent_id && (
            <div className="flex items-center gap-1.5 text-xs text-[#388bfd]" title="Planned assignee — set at planning time">
              <UserCheck size={11} />
              <span className="font-medium">{task.assigned_to}</span>
              <span className="text-[#484f58] font-normal">(planned)</span>
            </div>
          )}
          {/* Active agent — shown once dispatched */}
          {task.agent_id && (
            <div className="flex items-center gap-1.5 text-xs text-[#3fb950]" title="Currently working this task">
              <User size={11} />
              <span className="font-medium">{task.agent_id}</span>
              {task.assigned_to && task.assigned_to !== task.agent_id && (
                <span className="text-[#484f58] font-normal">(planned: {task.assigned_to})</span>
              )}
            </div>
          )}
          {task.dispatched_at && (
            <div className="flex items-center gap-1.5 text-xs text-[#8b949e]">
              <Clock size={11} />
              <span>Started {relativeTime(task.dispatched_at)}</span>
            </div>
          )}
          {task.dependency_ids?.length > 0 && (
            <div className="flex items-center gap-1.5 text-xs text-[#8b949e]">
              <GitBranch size={11} />
              <span>{task.dependency_ids.length} dep{task.dependency_ids.length > 1 ? 's' : ''}</span>
            </div>
          )}
          {task.blocked_by_ids?.length ? (
            <div className="flex items-center gap-1.5 text-xs text-[#f85149]">
              <AlertCircle size={11} />
              <span>Blocked by #{task.blocked_by_ids.join(', #')}</span>
            </div>
          ) : null}
          {task.state === 'completed' && task.completed_at && (
            <div className="flex items-center gap-1.5 text-xs text-[#3fb950]">
              <CheckCircle2 size={11} />
              <span>Done {relativeTime(task.completed_at)}</span>
            </div>
          )}
        </div>

        {/* Tabs */}
        <div className="flex border-b border-[#30363d]">
          {(['history', 'details'] as const).map(t => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`px-5 py-2.5 text-xs font-medium capitalize transition-colors border-b-2 -mb-px ${
                tab === t
                  ? 'border-[#58a6ff] text-[#58a6ff]'
                  : 'border-transparent text-[#8b949e] hover:text-[#e6edf3]'
              }`}
            >
              {t}
            </button>
          ))}
        </div>

        {/* Content */}
        <div className="flex-1 overflow-y-auto p-5">
          {loading ? (
            <div className="flex justify-center py-8">
              <Loader2 size={20} className="text-[#8b949e] animate-spin" />
            </div>
          ) : tab === 'details' ? (
            <DetailsTab task={task} />
          ) : (
            <HistoryTab history={history} comments={comments} />
          )}
        </div>
      </div>
    </>
  )
}

function DetailsTab({ task }: { task: Task }) {
  return (
    <div className="space-y-5">
      {task.description && (
        <div>
          <div className="text-xs font-medium text-[#8b949e] uppercase tracking-wide mb-2">Description</div>
          <div className="text-sm text-[#e6edf3] whitespace-pre-wrap leading-relaxed bg-[#0d1117] rounded-lg p-3 border border-[#21262d]">
            {task.description}
          </div>
        </div>
      )}
      {task.acceptance_criteria && (
        <div>
          <div className="text-xs font-medium text-[#8b949e] uppercase tracking-wide mb-2">Acceptance Criteria</div>
          <div className="text-sm text-[#e6edf3] whitespace-pre-wrap leading-relaxed bg-[#0d1117] rounded-lg p-3 border border-[#21262d]">
            {task.acceptance_criteria}
          </div>
        </div>
      )}
      <div className="grid grid-cols-2 gap-3">
        <MetaItem label="Task ID" value={`#${task.id}`} />
        <MetaItem label="State" value={task.state} />
        <MetaItem label="Created" value={relativeTime(task.created_at)} />
        <MetaItem label="Updated" value={relativeTime(task.updated_at)} />
        {task.dependency_ids?.length > 0 && (
          <MetaItem label="Depends on" value={task.dependency_ids.map(id => `#${id}`).join(', ')} />
        )}
      </div>
    </div>
  )
}

function HistoryTab({ history, comments }: { history: ActivityLog[]; comments: Comment[] }) {
  // Merge history + comments into a unified timeline (history already contains comments as entries)
  if (history.length === 0 && comments.length === 0) {
    return <p className="text-sm text-[#8b949e] text-center py-8">No activity yet</p>
  }

  return (
    <div className="space-y-0">
      {history.map((entry, i) => (
        <div key={entry.id} className="flex gap-3">
          <div className="flex flex-col items-center">
            <div className="mt-1">{ACTION_ICONS[entry.action] ?? ACTION_ICONS.comment}</div>
            {i < history.length - 1 && <div className="w-px flex-1 bg-[#21262d] mt-1" />}
          </div>
          <div className="pb-5 flex-1 min-w-0">
            <div className="flex items-center gap-2 mb-1">
              <span className="text-xs font-medium text-[#e6edf3]">{entry.actor}</span>
              <span className="text-xs text-[#484f58]">·</span>
              <span className="text-xs text-[#8b949e] capitalize">{entry.action.replace('_', ' ')}</span>
              {entry.from_state && entry.to_state && (
                <span className="text-xs text-[#8b949e]">
                  {entry.from_state} → {entry.to_state}
                </span>
              )}
              <span className="text-xs text-[#484f58] ml-auto flex-shrink-0">{relativeTime(entry.created_at)}</span>
            </div>
            {entry.detail && (
              <div className={`text-xs rounded-md p-3 leading-relaxed whitespace-pre-wrap ${
                entry.action === 'comment'
                  ? 'bg-[#0d1117] border border-[#21262d] text-[#e6edf3]'
                  : 'text-[#8b949e]'
              }`}>
                {entry.detail}
              </div>
            )}
          </div>
        </div>
      ))}
    </div>
  )
}

function MetaItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-[#0d1117] rounded-md p-3 border border-[#21262d]">
      <div className="text-xs text-[#8b949e] mb-0.5">{label}</div>
      <div className="text-sm text-[#e6edf3] font-medium truncate">{value}</div>
    </div>
  )
}
