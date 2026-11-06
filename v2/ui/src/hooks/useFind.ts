import { useRef } from 'react'
import { useSearchParam } from './useSearchParam'

/**
 * The My work find box's state: `?q=` in the URL, so a filtered view can be
 * linked and survives reload. `q` is the normalized query rows are matched
 * against (trimmed, lower-case); `raw` is what the box shows.
 */
export function useFind() {
  const [raw, setRaw] = useSearchParam('q')
  const ref = useRef<HTMLInputElement>(null)
  return { raw: raw ?? '', q: (raw ?? '').trim().toLowerCase(), setRaw, ref }
}
export type Find = ReturnType<typeof useFind>

/** Does an item match the query: any of its ticket refs (`#123` or `123`) or its title? */
export function ticketMatches(q: string, ids: (number | undefined | null)[], name?: string | null): boolean {
  const refs = ids.filter(Boolean).map(id => `#${id}`)
  return `${refs.join(' ')} ${name ?? ''}`.toLowerCase().includes(q)
}
