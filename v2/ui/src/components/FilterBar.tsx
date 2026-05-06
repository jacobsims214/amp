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
  { value: '', label: 'All' },
  { value: 'backlog',     label: 'Backlog',      dot: 'bg-[#7E91A8]' },
  { value: 'in_progress', label: 'In Progress',  dot: 'bg-[#818CF8]' },
  { value: 'completed',   label: 'Done',         dot: 'bg-[#10B981]' },
  { value: 'blocked',     label: 'Blocked',      dot: 'bg-[#EF4444]' },
] as const

export function FilterBar({ filter, onChange, epicOptions, counts }: Props) {
  const hasActiveFilter = filter.query || filter.state || filter.epicId !== null

  return (
    <div className="flex items-center gap-2.5 flex-wrap">
      {/* Search */}
      <div className="relative flex-1 min-w-[200px]">
        <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-[#3D5068] pointer-events-none" />
        <input
          type="text"
          placeholder="Search tasks, epics, stories…"
          value={filter.query}
          onChange={e => onChange({ ...filter, query: e.target.value })}
          className="w-full bg-[#0D1726] border border-[#1E2C45] rounded-lg pl-9 pr-8 py-2 text-sm text-[#DDE6F0] placeholder-[#3D5068] focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 transition-all"
        />
        {filter.query && (
          <button
            onClick={() => onChange({ ...filter, query: '' })}
            className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[#3D5068] hover:text-[#DDE6F0] transition-colors cursor-pointer"
          >
            <X size={12} />
          </button>
        )}
      </div>

      {/* State filter pills */}
      <div className="flex items-center gap-1 p-1 rounded-lg" style={{ background: '#0D1726', border: '1px solid #1E2C45' }}>
        {STATE_OPTIONS.map(opt => {
          const isActive = filter.state === opt.value
          return (
            <button
              key={opt.value}
              onClick={() => onChange({ ...filter, state: opt.value as FilterState['state'] })}
              className={`flex items-center gap-1.5 px-3 py-1 rounded-md text-xs font-medium transition-all cursor-pointer ${
                isActive
                  ? 'bg-[#172540] text-[#DDE6F0] shadow-sm'
                  : 'text-[#7E91A8] hover:text-[#DDE6F0] hover:bg-[#111E30]'
              }`}
            >
              {'dot' in opt && (
                <span className={`w-1.5 h-1.5 rounded-full flex-shrink-0 ${opt.dot} ${isActive ? 'opacity-100' : 'opacity-60'}`} />
              )}
              {opt.label}
              {opt.value && (
                <span className={`tabular-nums ${isActive ? 'text-[#7E91A8]' : 'text-[#3D5068]'}`}>
                  {counts[opt.value as keyof typeof counts]}
                </span>
              )}
            </button>
          )
        })}
      </div>

      {/* Epic filter */}
      {epicOptions.length > 0 && (
        <div className="relative">
          <SlidersHorizontal size={12} className="absolute left-3 top-1/2 -translate-y-1/2 text-[#3D5068] pointer-events-none" />
          <select
            value={filter.epicId ?? ''}
            onChange={e => onChange({ ...filter, epicId: e.target.value ? Number(e.target.value) : null })}
            className="bg-[#0D1726] border border-[#1E2C45] rounded-lg pl-8 pr-3 py-2 text-sm text-[#DDE6F0] focus:outline-none focus:border-[#6366F1] focus:ring-2 focus:ring-[#6366F1]/20 appearance-none cursor-pointer transition-all"
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
          className="flex items-center gap-1.5 px-3 py-2 text-xs font-medium text-[#7E91A8] hover:text-[#DDE6F0] border border-[#1E2C45] hover:border-[#283A57] rounded-lg transition-all cursor-pointer"
        >
          <X size={11} />
          Clear
        </button>
      )}

      {/* Total */}
      <span className="text-xs text-[#3D5068] ml-auto tabular-nums">
        {counts.total} task{counts.total !== 1 ? 's' : ''}
      </span>
    </div>
  )
}
