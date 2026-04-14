import type { TaskState } from '../types'

const CONFIG: Record<TaskState, { label: string; dot: string; text: string; bg: string }> = {
  scheduled:   { label: 'Scheduled',   dot: 'bg-[#d29922]', text: 'text-[#d29922]', bg: 'bg-[#d29922]/10' },
  backlog:     { label: 'Backlog',     dot: 'bg-[#8b949e]', text: 'text-[#8b949e]', bg: 'bg-[#8b949e]/10' },
  in_progress: { label: 'In Progress', dot: 'bg-[#58a6ff]', text: 'text-[#58a6ff]', bg: 'bg-[#58a6ff]/10' },
  completed:   { label: 'Done',        dot: 'bg-[#3fb950]', text: 'text-[#3fb950]', bg: 'bg-[#3fb950]/10' },
  blocked:     { label: 'Blocked',     dot: 'bg-[#f85149]', text: 'text-[#f85149]', bg: 'bg-[#f85149]/10' },
}

export function StatusBadge({ state }: { state: TaskState }) {
  const c = CONFIG[state] ?? CONFIG.backlog
  return (
    <span className={`inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-xs font-medium ${c.text} ${c.bg}`}>
      <span className={`w-1.5 h-1.5 rounded-full ${c.dot}`} />
      {c.label}
    </span>
  )
}

export function statusConfig(state: TaskState) {
  return CONFIG[state] ?? CONFIG.backlog
}
