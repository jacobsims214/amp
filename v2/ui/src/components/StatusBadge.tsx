import type { TaskState } from '../types'

const CONFIG: Record<TaskState, { label: string; dot: string; text: string; bg: string; ring: string }> = {
  scheduled:   { label: 'Scheduled',   dot: 'bg-[#F59E0B]', text: 'text-[#FBBF24]', bg: 'bg-[#F59E0B]/10', ring: 'ring-[#F59E0B]/20' },
  backlog:     { label: 'Backlog',     dot: 'bg-[#7E91A8]', text: 'text-[#7E91A8]', bg: 'bg-[#7E91A8]/10', ring: 'ring-[#7E91A8]/20' },
  in_progress: { label: 'In Progress', dot: 'bg-[#818CF8]', text: 'text-[#818CF8]', bg: 'bg-[#6366F1]/15', ring: 'ring-[#6366F1]/25' },
  completed:   { label: 'Done',        dot: 'bg-[#10B981]', text: 'text-[#10B981]', bg: 'bg-[#10B981]/10', ring: 'ring-[#10B981]/20' },
  blocked:     { label: 'Blocked',     dot: 'bg-[#EF4444]', text: 'text-[#EF4444]', bg: 'bg-[#EF4444]/10', ring: 'ring-[#EF4444]/20' },
}

export function StatusBadge({ state }: { state: TaskState }) {
  const c = CONFIG[state] ?? CONFIG.backlog
  return (
    <span className={`inline-flex items-center gap-1.5 px-2.5 py-0.5 rounded-full text-xs font-medium ring-1 ${c.text} ${c.bg} ${c.ring}`}>
      <span className={`w-1.5 h-1.5 rounded-full flex-shrink-0 ${c.dot}`} />
      {c.label}
    </span>
  )
}

export function statusConfig(state: TaskState) {
  return CONFIG[state] ?? CONFIG.backlog
}
