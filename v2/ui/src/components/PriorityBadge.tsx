import type { Priority } from '../types'

const CONFIG: Record<Priority, { label: string; color: string }> = {
  '0': { label: 'Low',      color: 'text-[#8b949e]' },
  '1': { label: 'Normal',   color: 'text-[#8b949e]' },
  '2': { label: 'High',     color: 'text-[#e3b341]' },
  '3': { label: 'Critical', color: 'text-[#f85149]' },
}

export function PriorityBadge({ priority }: { priority: Priority }) {
  const c = CONFIG[priority] ?? CONFIG['1']
  if (priority === '0' || priority === '1') return null // don't show normal/low, clutters board
  return (
    <span className={`text-xs font-medium ${c.color}`}>{c.label}</span>
  )
}
