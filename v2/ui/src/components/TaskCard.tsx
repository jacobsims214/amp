import { Calendar, GitBranch } from 'lucide-react'
import type { Task } from '../types'
import type { Person } from '../lib/people'
import { displayState, STATE_LABEL, STATE_VAR } from '../lib/ticketState'
import { PriorityMark } from './ui/PriorityMark'
import { Avatar } from './ui/Avatar'
import { OwnerAvatar, UnassignedPill } from './OwnerAvatar'

interface Props {
  task: Task
  onClick: () => void
  dimmed?: boolean
  isScheduled?: boolean
  /** the ticket's human owner (resolved from owner_user_id); null = unassigned */
  owner?: Person | null
}

function formatStart(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
}

/** A ticket on the board: white, square, a state-colored left edge, its ID
 *  and priority, the agent on it (or the planned profile), and its owner.
 *  A question shows the question itself. */
export function TaskCard({ task, onClick, dimmed = false, owner = null }: Props) {
  const state = displayState(task)
  const startAt = state === 'sched' ? (task.start_at ?? task.block_reason?.replace('scheduled:', '')) : null
  const waitingOn = task.blocked_by_ids?.length ?? 0

  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={`Ticket ${task.id}: ${task.name}, ${STATE_LABEL[state]}`}
      className={`w-full text-left bg-surface border border-line hover:border-ink cursor-pointer transition-colors grid gap-1.5 ${dimmed ? 'opacity-35' : ''}`}
      style={{ padding: '8px 10px 9px 12px', boxShadow: `inset 3px 0 0 ${STATE_VAR[state]}`, borderRadius: 'var(--radius)' }}
    >
      <span className="flex items-center gap-2">
        <span className="amp-num text-[11px] text-ink-3">#{task.id}</span>
        <PriorityMark priority={task.priority} />
        {state === 'flag' && (
          <span className="ml-auto font-label text-[10px] font-semibold uppercase tracking-[0.1em] px-1.5 py-[3px] bg-st-flag text-white">Question</span>
        )}
      </span>

      <span className="text-[13px] font-medium text-ink leading-snug line-clamp-2">{task.name}</span>

      {state === 'flag' && task.block_reason && (
        <span className="text-[12px] leading-snug text-danger-ink line-clamp-3">{task.block_reason}</span>
      )}
      {startAt && (
        <span className="flex items-center gap-1.5 text-[11.5px] text-violet"><Calendar size={11} className="flex-shrink-0" />Starts {formatStart(startAt)}</span>
      )}

      <span className="flex items-center gap-2 min-w-0 pt-0.5">
        {task.agent_id ? (
          <span className="inline-flex items-center gap-1.5 min-w-0" title={`Agent working: ${task.agent_id}`}>
            <Avatar name={task.agent_id} kind="agent" size={18} />
            <span className="amp-num text-[11px] text-ink-2 truncate">{task.agent_id}</span>
          </span>
        ) : task.assigned_to ? (
          <span className="amp-num text-[11px] text-ink-3 truncate" title={`Planned agent profile: ${task.assigned_to}`}>{task.assigned_to}</span>
        ) : <span />}
        <span className="ml-auto flex items-center gap-2 flex-shrink-0">
          {waitingOn > 0 && (
            <span className="inline-flex items-center gap-1 amp-num text-[11px] text-ink-3" title={`Waiting on #${task.blocked_by_ids!.join(', #')}`}>
              <GitBranch size={11} />{waitingOn}
            </span>
          )}
          {owner ? <OwnerAvatar person={owner} title="Owner" /> : <UnassignedPill />}
        </span>
      </span>
    </button>
  )
}
