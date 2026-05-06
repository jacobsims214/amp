import type { Priority } from '../types'

const CONFIG: Record<Priority, { label: string; color: string; bg: string }> = {
  '0': { label: 'Low',      color: 'text-[#3D5068]',  bg: '' },
  '1': { label: 'Normal',   color: 'text-[#3D5068]',  bg: '' },
  '2': { label: 'High',     color: 'text-[#FBBF24]',  bg: 'bg-[#F59E0B]/10 ring-1 ring-[#F59E0B]/20' },
  '3': { label: 'Critical', color: 'text-[#F87171]',  bg: 'bg-[#EF4444]/10 ring-1 ring-[#EF4444]/20' },
}

export function PriorityBadge({ priority }: { priority: Priority }) {
  const c = CONFIG[priority] ?? CONFIG['1']
  if (priority === '0' || priority === '1') return null
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-semibold ${c.color} ${c.bg}`}>
      {c.label}
    </span>
  )
}
