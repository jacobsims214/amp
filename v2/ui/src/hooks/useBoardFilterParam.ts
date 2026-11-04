import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { parseBoardFilter, serializeBoardFilter, type FilterState } from '../lib/boardFilter'
import { usePatchSearchParams } from './useSearchParamState'

/** How long typing pauses before the search text goes into the URL. */
export const SEARCH_WRITE_MS = 250

/**
 * The Board filter, kept in the URL (`status`, `q`, `epic`, `owner`; see
 * lib/boardFilter.ts and v2/docs/url-state.md).
 *
 * - Pills and selects push a history entry, so Back undoes them.
 * - Search text shows as typed and is written to `?q=` with replace once
 *   typing pauses, so typing never adds history entries.
 * - Other params (`focus`, …) are kept.
 */
export function useBoardFilterParam(): [FilterState, (f: FilterState) => void] {
  const [params] = useSearchParams()
  const patch = usePatchSearchParams()
  const fromUrl = useMemo(() => parseBoardFilter(params), [params])

  // Typed search text not yet in the URL; null once the URL has caught up.
  const [draft, setDraft] = useState<string | null>(null)
  if (draft !== null && draft === fromUrl.query) setDraft(null)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])

  const filter = useMemo(() => (draft === null ? fromUrl : { ...fromUrl, query: draft }), [fromUrl, draft])

  const setFilter = useCallback((f: FilterState) => {
    clearTimeout(timer.current)
    setDraft(f.query)
    const typing = f.state === filter.state && f.epicId === filter.epicId && f.owner === filter.owner
    if (typing && f.query !== '') {
      timer.current = setTimeout(() => patch({ q: f.query }), SEARCH_WRITE_MS)
      return
    }
    // Clearing the search replaces; changing a pill, select or Clear pushes.
    patch(serializeBoardFilter(f), { push: !typing })
  }, [filter, patch])

  return [filter, setFilter]
}
