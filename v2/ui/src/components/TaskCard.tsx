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

// Left-border accent color per column state
const STATE_ACCENT: Record<string, string> = {
  scheduled:   'before:bg-[#F59E0B]',
  backlog:     'before:bg-[#283A57]',
  in_progress: 'before:bg-[#6366F1]',
  blocked:     'before:bg-[#EF4444]',
  completed:   'before:bg-[#10B981]',
}

export function TaskCard({ task, onClick, dimmed = false, isScheduled = false }: Props) {
  const activeAgent = task.agent_id
  const plannedAgent = !activeAgent && task.assigned_to ? task.assigned_to : null
  const scheduledStartAt = isScheduled ? getScheduledStartAt(task) : null

  // Determine logical state for accent bar
  const logicalState = isScheduled ? 'scheduled'
    : task.state === 'in_progress' ? 'in_progress'
    : task.state === 'blocked' ? 'blocked'
    : task.state === 'completed' ? 'completed'
    : 'backlog'

  const accentClass = STATE_ACCENT[logicalState] ?? STATE_ACCENT.backlog

  return (
    <div
      onClick={onClick}
      className={`
        group relative cursor-pointer rounded-xl border transition-all duration-200
        before:absolute before:left-0 before:top-2 before:bottom-2 before:w-0.5 before:rounded-full
        ${accentClass}
        ${isScheduled
          ? 'bg-[#120E00] border-[#F59E0B]/20 hover:border-[#F59E0B]/40'
          : 'bg-[#111E30] border-[#1E2C45] hover:border-[#6366F1]/35'
        }
        hover:bg-[#172540] hover:shadow-lg hover:shadow-black/30 hover:-translate-y-px
        active:scale-[0.99] active:translate-y-0
        ${dimmed ? 'opacity-35' : ''}
      `}
      style={{ padding: '10px 12px 10px 16px' }}
    >
      <div className="flex flex-col gap-2">
        {/* Planned assignee badge */}
        {plannedAgent && (
          <div
            className="flex items-center gap-1.5 px-2 py-0.5 rounded-md border border-dashed border-[#6366F1]/35 bg-[#6366F1]/8 w-fit"
            title={`Planned for: ${task.assigned_to}`}
          >
            <UserCheck size={10} className="text-[#818CF8] flex-shrink-0" />
            <span className="text-xs text-[#818CF8] font-medium">{shortAgent(plannedAgent)}</span>
          </div>
        )}

        {/* Name */}
        <p className="text-sm font-medium text-[#DDE6F0] leading-snug line-clamp-2 group-hover:text-white transition-colors">
          {task.name}
        </p>

        {/* Scheduled start */}
        {scheduledStartAt && (
          <div className="flex items-center gap-1.5 text-xs text-[#FBBF24]">
            <Calendar size={11} className="flex-shrink-0" />
            <span>Starts {formatScheduledStart(scheduledStartAt)}</span>
          </div>
        )}

        {/* Footer row */}
        <div className="flex items-center justify-between gap-2 mt-0.5">
          <div className="flex items-center gap-2">
            <span className="text-xs text-[#3D5068] font-mono tabular-nums">#{task.id}</span>
            <PriorityBadge priority={task.priority} />
          </div>
          <div className="flex items-center gap-2">
            {task.blocked_by_ids && task.blocked_by_ids.length > 0 && (
              <div className="flex items-center gap-1 text-[#F87171]" title={`Blocked by #${task.blocked_by_ids.join(', #')}`}>
                <AlertCircle size={11} />
                <span className="text-xs">{task.blocked_by_ids.length}</span>
              </div>
            )}
            {task.dependency_ids?.length > 0 && !(task.blocked_by_ids?.length) && (
              <div className="flex items-center gap-1 text-[#3D5068]" title="Has dependencies">
                <GitBranch size={11} />
              </div>
            )}
            {activeAgent && (
              <div className="flex items-center gap-1 text-[#10B981]" title={`Working: ${activeAgent}`}>
                <User size={11} />
                <span className="text-xs truncate max-w-[64px]">{shortAgent(activeAgent)}</span>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
