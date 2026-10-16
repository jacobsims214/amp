import type { Metadata, MetadataPatch, MetadataValue } from '../types'

// Metadata helpers shared by the editor and the forms that save it.

export type MetadataType = 'text' | 'number' | 'boolean'

export interface MetadataRow {
  key: string
  type: MetadataType
  /** the value as typed; booleans are 'true' / 'false' */
  value: string
}

export function typeOf(v: MetadataValue): MetadataType {
  return typeof v === 'boolean' ? 'boolean' : typeof v === 'number' ? 'number' : 'text'
}

export function toRows(m: Metadata | undefined): MetadataRow[] {
  return Object.entries(m ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, v]) => ({ key, type: typeOf(v), value: String(v) }))
}

/** The metadata the rows describe, plus a problem to show if any. */
export function fromRows(rows: MetadataRow[]): { metadata: Metadata; error: string | null } {
  const metadata: Metadata = {}
  for (const r of rows) {
    const key = r.key.trim()
    if (!key) {
      if (r.value.trim()) return { metadata, error: 'Every field needs a name.' }
      continue
    }
    if (key.length > 64) return { metadata, error: `"${key.slice(0, 20)}…" is longer than 64 characters.` }
    if (key in metadata) return { metadata, error: `"${key}" is listed twice.` }
    if (r.type === 'number') {
      const n = Number(r.value)
      if (r.value.trim() === '' || Number.isNaN(n)) return { metadata, error: `"${key}" needs a number.` }
      metadata[key] = n
    } else if (r.type === 'boolean') {
      metadata[key] = r.value === 'true'
    } else {
      metadata[key] = r.value
    }
  }
  return { metadata, error: null }
}

/** The change that turns `before` into `after`: new or changed keys, and null for removed ones. */
export function metadataPatch(before: Metadata | undefined, after: Metadata): MetadataPatch | undefined {
  const patch: MetadataPatch = {}
  for (const [k, v] of Object.entries(after)) if (before?.[k] !== v) patch[k] = v
  for (const k of Object.keys(before ?? {})) if (!(k in after)) patch[k] = null
  return Object.keys(patch).length ? patch : undefined
}
