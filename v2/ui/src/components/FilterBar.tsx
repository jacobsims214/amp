import { Search, X, SlidersHorizontal } from 'lucide-react'

export interface FilterState {
  query: string
  state: '' | 'backlog' | 'in_progress' | 'completed' | 'blocked'
  epicId: number | null
}

interface Props {
  filter: FilterState
  onChange: (f: FilterState) => void
  epicOptions: { id: number; name: string }[]
  counts: { backlog: number; in_progress: number; completed: number; blocked: number; total: number }
}

const STATE_OPTIONS = [
  { value: '', label: 'All states' },
  { value: 'backlog', label: 'Backlog', dot: 'bg-[#8b949e]' },
  { value: 'in_progress', label: 'In Progress', dot: 'bg-[#58a6ff]' },
  { value: 'completed', label: 'Done', dot: 'bg-[#3fb950]' },
  { value: 'blocked', label: 'Blocked', dot: 'bg-[#f85149]' },
] as const

export function FilterBar({ filter, onChange, epicOptions, counts }: Props) {
  const hasActiveFilter = filter.query || filter.state || filter.epicId !== null

  return (
    <div className="flex items-center gap-3 flex-wrap">
      {/* Search */}
      <div className="relative flex-1 min-w-[220px]">
        <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-[#8b949e]" />
        <input
          type="text"
          placeholder="Search tasks, epics, stories…"
          value={filter.query}
          onChange={e => onChange({ ...filter, query: e.target.value })}
          className="w-full bg-[#0d1117] border border-[#30363d] rounded-md pl-9 pr-3 py-2 text-sm text-[#e6edf3] placeholder-[#484f58] focus:outline-none focus:border-[#58a6ff]/60 focus:ring-1 focus:ring-[#58a6ff]/30 transition-colors"
        />
        {filter.query && (
          <button
            onClick={() => onChange({ ...filter, query: '' })}
            className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[#8b949e] hover:text-[#e6edf3]"
          >
            <X size={12} />
          </button>
        )}
      </div>

      {/* State filter */}
      <div className="flex items-center gap-1 bg-[#0d1117] border border-[#30363d] rounded-md p-1">
        {STATE_OPTIONS.map(opt => (
          <button
            key={opt.value}
            onClick={() => onChange({ ...filter, state: opt.value as FilterState['state'] })}
            className={`flex items-center gap-1.5 px-3 py-1 rounded text-xs font-medium transition-colors ${
              filter.state === opt.value
                ? 'bg-[#21262d] text-[#e6edf3]'
                : 'text-[#8b949e] hover:text-[#e6edf3]'
            }`}
          >
            {'dot' in opt && <span className={`w-1.5 h-1.5 rounded-full ${opt.dot}`} />}
            {opt.label}
            {opt.value && (
              <span className="text-[#484f58] text-xs">
                {counts[opt.value as keyof typeof counts]}
              </span>
            )}
          </button>
        ))}
      </div>

      {/* Epic filter */}
      {epicOptions.length > 0 && (
        <div className="relative">
          <SlidersHorizontal size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-[#8b949e]" />
          <select
            value={filter.epicId ?? ''}
            onChange={e => onChange({ ...filter, epicId: e.target.value ? Number(e.target.value) : null })}
            className="bg-[#0d1117] border border-[#30363d] rounded-md pl-8 pr-3 py-2 text-sm text-[#e6edf3] focus:outline-none focus:border-[#58a6ff]/60 appearance-none cursor-pointer"
          >
            <option value="">All epics</option>
            {epicOptions.map(e => (
              <option key={e.id} value={e.id}>{e.name}</option>
            ))}
          </select>
        </div>
      )}

      {/* Clear */}
      {hasActiveFilter && (
        <button
          onClick={() => onChange({ query: '', state: '', epicId: null })}
          className="flex items-center gap-1.5 px-3 py-2 text-xs text-[#8b949e] hover:text-[#e6edf3] border border-[#30363d] rounded-md transition-colors"
        >
          <X size={12} />
          Clear
        </button>
      )}

      {/* Total */}
      <span className="text-xs text-[#484f58] ml-auto">{counts.total} tasks</span>
    </div>
  )
}
