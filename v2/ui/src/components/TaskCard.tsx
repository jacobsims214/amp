import { User, GitBranch, AlertCircle, UserCheck, Calendar } from 'lucide-react'
import type { Task } from '../types'
import { PriorityBadge } from './PriorityBadge'

interface Props {
  task: Task
  onClick: () => void
  dimmed?: boolean
  isScheduled?: boolean
}

// Shorten agent names for compact display on cards
function shortAgent(s: string) {
  return s.replace('amp-worker-', '').replace('amp-worker', 'worker').slice(0, 18)
}

// Extract start_at datetime from block_reason
function getScheduledStartAt(task: Task): string | null {
  if (!task.block_reason?.startsWith('scheduled:')) return null
  return task.block_reason.replace('scheduled:', '')
}

// Format scheduled start datetime
function formatScheduledStart(isoString: string): string {
  try {
    const date = new Date(isoString)
    return date.toLocaleString('en-US', {
      month: 'short',
      day: 'numeric',
      hour: 'numeric',
      minute: '2-digit',
      hour12: true,
    })
  } catch {
    return 'Invalid date'
  }
}

export function TaskCard({ task, onClick, dimmed = false, isScheduled = false }: Props) {
  // What to show in the assignee slot:
  // - agent_id (in_progress/completed): who is/was actually working it — solid icon
  // - assigned_to (backlog/blocked): who the manager planned to assign — dashed outline icon
  const activeAgent = task.agent_id
  const plannedAgent = !activeAgent && task.assigned_to ? task.assigned_to : null
  const scheduledStartAt = isScheduled ? getScheduledStartAt(task) : null

  return (
    <div
      onClick={onClick}
      className={`
        group relative cursor-pointer rounded-lg border p-3 transition-all duration-150
        ${isScheduled ? 'bg-[#2a2416] border-[#d29922]/30' : 'bg-[#1c2333] border-[#30363d]'}
        hover:border-[#58a6ff]/40 hover:bg-[#1c2333]/80 hover:shadow-lg hover:shadow-black/20
        ${dimmed ? 'opacity-40' : ''}
      `}
    >
      {/* Priority accent bar */}
      {(task.priority === '2' || task.priority === '3') && (
        <div className={`absolute left-0 top-0 bottom-0 w-0.5 rounded-l-lg ${
          task.priority === '3' ? 'bg-[#f85149]' : 'bg-[#e3b341]'
        }`} />
      )}

      <div className="flex flex-col gap-2">
        {/* Assignee badge — shown at the top when planned but not yet active */}
        {plannedAgent && (
          <div
            className="flex items-center gap-1.5 px-1.5 py-0.5 rounded border border-dashed border-[#388bfd]/40 bg-[#388bfd]/5 w-fit"
            title={`Planned for: ${task.assigned_to}`}
          >
            <UserCheck size={10} className="text-[#388bfd] flex-shrink-0" />
            <span className="text-xs text-[#388bfd] font-medium">{shortAgent(plannedAgent)}</span>
          </div>
        )}

        {/* Name */}
        <p className="text-sm font-medium text-[#e6edf3] leading-snug line-clamp-2 group-hover:text-white">
          {task.name}
        </p>

        {/* Scheduled start datetime — shown for scheduled tasks */}
        {scheduledStartAt && (
          <div className="flex items-center gap-1.5 text-xs text-[#d29922]">
            <Calendar size={12} className="flex-shrink-0" />
            <span>Starts {formatScheduledStart(scheduledStartAt)}</span>
          </div>
        )}

        {/* Footer row */}
        <div className="flex items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <span className="text-xs text-[#484f58] font-mono">#{task.id}</span>
            <PriorityBadge priority={task.priority} />
          </div>
          <div className="flex items-center gap-2">
            {task.blocked_by_ids && task.blocked_by_ids.length > 0 && (
              <div className="flex items-center gap-1 text-[#f85149]" title={`Blocked by #${task.blocked_by_ids.join(', #')}`}>
                <AlertCircle size={11} />
                <span className="text-xs">{task.blocked_by_ids.length}</span>
              </div>
            )}
            {task.dependency_ids?.length > 0 && !(task.blocked_by_ids?.length) && (
              <div className="flex items-center gap-1 text-[#8b949e]" title="Has dependencies">
                <GitBranch size={11} />
              </div>
            )}
            {activeAgent && (
              <div className="flex items-center gap-1 text-[#3fb950]" title={`Working: ${activeAgent}`}>
                <User size={11} />
                <span className="text-xs truncate max-w-[60px]">{shortAgent(activeAgent)}</span>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
