import { Bot, GitBranch, AlertCircle, Calendar } from 'lucide-react'
import type { Task } from '../types'
import type { Person } from '../lib/people'
import { PriorityBadge } from './PriorityBadge'
import { OwnerAvatar, UnassignedPill } from './OwnerAvatar'

interface Props {
  task: Task
  onClick: () => void
  dimmed?: boolean
  isScheduled?: boolean
  /** the ticket's human owner (resolved from owner_user_id); null = unassigned */
  owner?: Person | null
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
  scheduled:   'before:bg-warning',
  backlog:     'before:bg-line-str',
  in_progress: 'before:bg-primary',
  blocked:     'before:bg-danger',
  completed:   'before:bg-success',
}

export function TaskCard({ task, onClick, dimmed = false, isScheduled = false, owner = null }: Props) {
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
          ? 'bg-surface-2 border-warning/20 hover:border-warning/40'
          : 'bg-surface-2 border-line hover:border-primary/35'
        }
        hover:bg-surface-3 hover:shadow-lg hover:shadow-black/30 hover:-translate-y-px
        active:scale-[0.99] active:translate-y-0
        ${dimmed ? 'opacity-35' : ''}
      `}
      style={{ padding: '10px 12px 10px 16px' }}
    >
      <div className="flex flex-col gap-2">
        {/* Planned agent badge (agent profile, not the human owner) */}
        {plannedAgent && (
          <div
            className="flex items-center gap-1.5 px-2 py-0.5 rounded-md border border-dashed border-primary/35 bg-primary/8 w-fit max-w-full"
            title={`Planned agent: ${task.assigned_to}`}
          >
            <Bot size={10} className="text-link flex-shrink-0" />
            <span className="text-xs text-link font-medium truncate">{shortAgent(plannedAgent)}</span>
          </div>
        )}

        {/* Active agent badge */}
        {activeAgent && (
          <div
            className="flex items-center gap-1.5 px-2 py-0.5 rounded-md border border-success/30 bg-success/8 w-fit max-w-full"
            title={`Agent working: ${activeAgent}`}
          >
            <Bot size={10} className="text-success flex-shrink-0" />
            <span className="text-xs text-success font-medium truncate">{shortAgent(activeAgent)}</span>
          </div>
        )}

        {/* Name */}
        <p className="text-sm font-medium text-ink leading-snug line-clamp-2 group-hover:text-white transition-colors">
          {task.name}
        </p>

        {/* Scheduled start */}
        {scheduledStartAt && (
          <div className="flex items-center gap-1.5 text-xs text-warning-ink">
            <Calendar size={11} className="flex-shrink-0" />
            <span>Starts {formatScheduledStart(scheduledStartAt)}</span>
          </div>
        )}

        {/* Footer row */}
        <div className="flex items-center justify-between gap-2 mt-0.5">
          <div className="flex items-center gap-2">
            <span className="text-xs text-ink-3 font-mono tabular-nums">#{task.id}</span>
            <PriorityBadge priority={task.priority} />
          </div>
          <div className="flex items-center gap-2">
            {task.blocked_by_ids && task.blocked_by_ids.length > 0 && (
              <div className="flex items-center gap-1 text-danger-ink" title={`Blocked by #${task.blocked_by_ids.join(', #')}`}>
                <AlertCircle size={11} />
                <span className="text-xs">{task.blocked_by_ids.length}</span>
              </div>
            )}
            {task.dependency_ids?.length > 0 && !(task.blocked_by_ids?.length) && (
              <div className="flex items-center gap-1 text-ink-3" title="Has dependencies">
                <GitBranch size={11} />
              </div>
            )}
            {owner ? <OwnerAvatar person={owner} title="Owner" /> : <UnassignedPill />}
          </div>
        </div>
      </div>
    </div>
  )
}
