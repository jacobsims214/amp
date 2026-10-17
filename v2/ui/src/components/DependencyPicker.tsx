import { useMemo, useState } from 'react'
import { Plus, X } from 'lucide-react'
import type { Task } from '../types'

const STATE_COLOR: Record<string, string> = {
  backlog: 'var(--ink-2)', in_progress: 'var(--link)', blocked: 'var(--danger-ink)', completed: 'var(--success)',
}
const STATE_LABEL: Record<string, string> = {
  backlog: 'ready', in_progress: 'running', blocked: 'blocked', completed: 'done',
}

interface Props {
  /** tickets it can depend on (the project's) */
  tasks: Task[]
  /** the ticket being edited; left out of the choices */
  selfId?: number
  value: number[]
  onChange: (ids: number[]) => void
  disabled?: boolean
  onOpen?: (task: Task) => void
}

// Choose which tickets a ticket waits on: chips for the current ones, and a
// search box (name or #id) to add more.
export function DependencyPicker({ tasks, selfId, value, onChange, disabled, onOpen }: Props) {
  const [query, setQuery] = useState('')
  const byId = useMemo(() => new Map(tasks.map(t => [t.id, t])), [tasks])
  const matches = useMemo(() => {
    const q = query.trim().toLowerCase().replace(/^#/, '')
    if (!q) return []
    return tasks
      .filter(t => t.id !== selfId && !value.includes(t.id))
      .filter(t => String(t.id) === q || t.name.toLowerCase().includes(q))
      .slice(0, 8)
  }, [query, tasks, selfId, value])

  return (
    <div className="space-y-2">
      {value.length === 0 && (
        <p className="text-xs text-ink-2">None — it can start as soon as it's dispatched.</p>
      )}
      <div className="flex flex-wrap gap-1.5">
        {value.map(id => {
          const t = byId.get(id)
          return (
            <span key={id} className="flex items-center gap-1.5 pl-2 pr-1 py-0.5 rounded-lg text-xs"
              style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}>
              <button type="button" onClick={() => t && onOpen?.(t)} disabled={!t || !onOpen}
                className="flex items-center gap-1.5 min-w-0 cursor-pointer disabled:cursor-default">
                <span className="font-mono text-ink-2">#{id}</span>
                <span className="text-ink truncate max-w-[180px]">{t?.name ?? 'unknown ticket'}</span>
                {t && <span style={{ color: STATE_COLOR[t.state] ?? 'var(--ink-2)' }}>{STATE_LABEL[t.state] ?? t.state}</span>}
              </button>
              {!disabled && (
                <button type="button" onClick={() => onChange(value.filter(v => v !== id))}
                  className="p-0.5 text-ink-2 hover:text-danger-ink cursor-pointer" title="Stop depending on this ticket">
                  <X size={11} />
                </button>
              )}
            </span>
          )
        })}
      </div>
      {!disabled && (
        <div className="relative">
          <div className="flex items-center gap-1.5">
            <Plus size={12} className="text-ink-3" />
            <input value={query} onChange={e => setQuery(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter' && matches[0]) { e.preventDefault(); onChange([...value, matches[0].id]); setQuery('') } }}
              placeholder="Add a ticket it waits on (name or #id)…"
              className="flex-1 bg-bg border border-line rounded-lg px-2.5 py-1.5 text-xs text-ink placeholder-ink-3 focus:outline-none focus:border-primary" />
          </div>
          {matches.length > 0 && (
            <div className="absolute z-30 left-5 right-0 mt-1 rounded-xl overflow-hidden shadow-2xl" style={{ background: 'var(--surface)', border: '1px solid var(--line-str)' }}>
              {matches.map(t => (
                <button type="button" key={t.id} onClick={() => { onChange([...value, t.id]); setQuery('') }}
                  className="w-full text-left px-3 py-2 text-xs hover:bg-surface-3 flex items-center gap-2 cursor-pointer">
                  <span className="font-mono text-ink-2">#{t.id}</span>
                  <span className="text-ink truncate flex-1">{t.name}</span>
                  <span style={{ color: STATE_COLOR[t.state] ?? 'var(--ink-2)' }}>{STATE_LABEL[t.state] ?? t.state}</span>
                </button>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
