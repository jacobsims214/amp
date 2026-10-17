import { Search, X, SlidersHorizontal, UserRound } from 'lucide-react'
import type { OwnerFilter } from '../lib/people'
import { DEFAULT_FILTER, type FilterState } from '../lib/boardFilter'

export type { FilterState }

interface Props {
  filter: FilterState
  onChange: (f: FilterState) => void
  epicOptions: { id: number; name: string }[]
  /** project members selectable in the owner filter */
  memberOptions: { id: number; name: string }[]
  counts: { backlog: number; in_progress: number; completed: number; blocked: number; total: number }
}

const STATE_OPTIONS = [
  { value: '', label: 'All' },
  { value: 'backlog',     label: 'Backlog',      dot: 'bg-ink-2' },
  { value: 'in_progress', label: 'In Progress',  dot: 'bg-link' },
  { value: 'completed',   label: 'Done',         dot: 'bg-success' },
  { value: 'blocked',     label: 'Blocked',      dot: 'bg-danger' },
] as const

function parseOwner(v: string): OwnerFilter {
  return v === 'mine' || v === 'unassigned' || v === 'everyone' ? v : Number(v)
}

export function FilterBar({ filter, onChange, epicOptions, memberOptions, counts }: Props) {
  const hasActiveFilter = filter.query || filter.state || filter.epicId !== null || filter.owner !== DEFAULT_FILTER.owner

  return (
    <div className="flex items-center gap-2.5 flex-wrap">
      {/* Search */}
      <div className="relative flex-1 min-w-[200px]">
        <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-ink-3 pointer-events-none" />
        <input
          type="text"
          placeholder="Search tasks, epics, stories…"
          value={filter.query}
          onChange={e => onChange({ ...filter, query: e.target.value })}
          className="w-full bg-surface border border-line rounded-lg pl-9 pr-8 py-2 text-sm text-ink placeholder-ink-3 focus:outline-none focus:border-primary focus:ring-2 focus:ring-primary/20 transition-all"
        />
        {filter.query && (
          <button
            onClick={() => onChange({ ...filter, query: '' })}
            className="absolute right-2.5 top-1/2 -translate-y-1/2 text-ink-3 hover:text-ink transition-colors cursor-pointer"
          >
            <X size={12} />
          </button>
        )}
      </div>

      {/* State filter pills */}
      <div className="flex items-center gap-1 p-1 rounded-lg" style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}>
        {STATE_OPTIONS.map(opt => {
          const isActive = filter.state === opt.value
          return (
            <button
              key={opt.value}
              onClick={() => onChange({ ...filter, state: opt.value as FilterState['state'] })}
              className={`flex items-center gap-1.5 px-3 py-1 rounded-md text-xs font-medium transition-all cursor-pointer ${
                isActive
                  ? 'bg-surface-3 text-ink shadow-sm'
                  : 'text-ink-2 hover:text-ink hover:bg-surface-2'
              }`}
            >
              {'dot' in opt && (
                <span className={`w-1.5 h-1.5 rounded-full flex-shrink-0 ${opt.dot} ${isActive ? 'opacity-100' : 'opacity-60'}`} />
              )}
              {opt.label}
              {opt.value && (
                <span className={`tabular-nums ${isActive ? 'text-ink-2' : 'text-ink-3'}`}>
                  {counts[opt.value as keyof typeof counts]}
                </span>
              )}
            </button>
          )
        })}
      </div>

      {/* Owner filter */}
      <div className="relative">
        <UserRound size={12} className="absolute left-3 top-1/2 -translate-y-1/2 text-ink-3 pointer-events-none" />
        <select
          value={String(filter.owner)}
          onChange={e => onChange({ ...filter, owner: parseOwner(e.target.value) })}
          title="Whose tickets to show"
          className="bg-surface border border-line rounded-lg pl-8 pr-3 py-2 text-sm text-ink focus:outline-none focus:border-primary focus:ring-2 focus:ring-primary/20 appearance-none cursor-pointer transition-all"
        >
          <option value="mine">Mine</option>
          <option value="unassigned">Unassigned</option>
          <option value="everyone">Everyone</option>
          {memberOptions.length > 0 && (
            <optgroup label="Members">
              {memberOptions.map(m => (
                <option key={m.id} value={m.id}>{m.name}</option>
              ))}
            </optgroup>
          )}
        </select>
      </div>

      {/* Epic filter */}
      {epicOptions.length > 0 && (
        <div className="relative">
          <SlidersHorizontal size={12} className="absolute left-3 top-1/2 -translate-y-1/2 text-ink-3 pointer-events-none" />
          <select
            value={filter.epicId ?? ''}
            onChange={e => onChange({ ...filter, epicId: e.target.value ? Number(e.target.value) : null })}
            className="bg-surface border border-line rounded-lg pl-8 pr-3 py-2 text-sm text-ink focus:outline-none focus:border-primary focus:ring-2 focus:ring-primary/20 appearance-none cursor-pointer transition-all"
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
          onClick={() => onChange(DEFAULT_FILTER)}
          className="flex items-center gap-1.5 px-3 py-2 text-xs font-medium text-ink-2 hover:text-ink border border-line hover:border-line-str rounded-lg transition-all cursor-pointer"
        >
          <X size={11} />
          Clear
        </button>
      )}

      {/* Total */}
      <span className="text-xs text-ink-3 ml-auto tabular-nums">
        {counts.total} task{counts.total !== 1 ? 's' : ''}
      </span>
    </div>
  )
}
