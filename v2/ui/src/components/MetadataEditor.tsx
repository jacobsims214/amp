import { useState } from 'react'
import { Plus, X } from 'lucide-react'
import { fromRows, toRows, type MetadataRow, type MetadataType } from '../lib/metadata'
import type { Metadata, MetadataValue } from '../types'

interface Props {
  /** the item's own metadata, as loaded */
  initial?: Metadata
  /** what it inherits from its project / epic / story, shown read-only */
  inherited?: Metadata
  readOnly?: boolean
  /** called with the edited metadata, or an error to show instead */
  onChange: (metadata: Metadata, error: string | null) => void
}

const cellCls =
  'bg-bg border border-line rounded-lg px-2 py-1.5 text-xs text-ink placeholder-ink-3 ' +
  'focus:outline-none focus:border-primary disabled:opacity-60'

const show = (v: MetadataValue) => (typeof v === 'boolean' ? (v ? 'yes' : 'no') : String(v))

// Free-form key/value fields for integrations (e.g. jira_board, initiative,
// capitalizable). Inherited fields are listed below the item's own; adding
// the same field here overrides it.
export function MetadataEditor({ initial, inherited, readOnly, onChange }: Props) {
  const [rows, setRows] = useState<MetadataRow[]>(() => toRows(initial))
  const [error, setError] = useState<string | null>(null)

  const update = (next: MetadataRow[]) => {
    setRows(next)
    const { metadata, error } = fromRows(next)
    setError(error)
    onChange(metadata, error)
  }
  const setRow = (i: number, patch: Partial<MetadataRow>) =>
    update(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)))

  const own = new Set(rows.map(r => r.key.trim()))
  const inheritedOnly = Object.entries(inherited ?? {}).filter(([k]) => !own.has(k)).sort(([a], [b]) => a.localeCompare(b))

  return (
    <div className="space-y-2">
      {rows.length === 0 && readOnly && <p className="text-xs text-ink-3">No fields.</p>}
      {rows.map((r, i) => (
        <div key={i} className="flex items-center gap-1.5">
          <input value={r.key} disabled={readOnly} onChange={e => setRow(i, { key: e.target.value })}
            placeholder="field (e.g. jira_board)" className={`${cellCls} w-36 font-mono`} />
          <select value={r.type} disabled={readOnly}
            onChange={e => {
              const type = e.target.value as MetadataType
              setRow(i, { type, value: type === 'boolean' ? (r.value === 'true' ? 'true' : 'false') : r.value })
            }}
            className={`${cellCls} w-[84px] cursor-pointer`} title="Type">
            <option value="text">Text</option>
            <option value="number">Number</option>
            <option value="boolean">Yes / no</option>
          </select>
          {r.type === 'boolean' ? (
            <select value={r.value} disabled={readOnly} onChange={e => setRow(i, { value: e.target.value })} className={`${cellCls} flex-1 cursor-pointer`}>
              <option value="true">Yes</option>
              <option value="false">No</option>
            </select>
          ) : (
            <input value={r.value} disabled={readOnly} onChange={e => setRow(i, { value: e.target.value })}
              placeholder="value" inputMode={r.type === 'number' ? 'decimal' : undefined} className={`${cellCls} flex-1 min-w-0`} />
          )}
          {!readOnly && (
            <button type="button" onClick={() => update(rows.filter((_, j) => j !== i))}
              className="p-1 text-ink-2 hover:text-danger-ink cursor-pointer" title="Remove field">
              <X size={13} />
            </button>
          )}
        </div>
      ))}
      {!readOnly && (
        <button type="button" onClick={() => update([...rows, { key: '', type: 'text', value: '' }])}
          className="flex items-center gap-1 text-xs text-link hover:text-ink cursor-pointer">
          <Plus size={12} />Add field
        </button>
      )}
      {inheritedOnly.length > 0 && (
        <div className="pt-1">
          <p className="text-[10px] font-semibold uppercase tracking-wide text-ink-3 mb-1">Inherited</p>
          <div className="flex flex-wrap gap-1.5">
            {inheritedOnly.map(([k, v]) => (
              <span key={k} className="text-[11px] px-2 py-0.5 rounded-md text-ink-2" style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}
                title="Inherited; add the same field above to override it here">
                <span className="font-mono">{k}</span>: {show(v)}
              </span>
            ))}
          </div>
        </div>
      )}
      {error && <p className="text-xs text-danger-ink">{error}</p>}
    </div>
  )
}
