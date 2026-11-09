import { useEffect } from 'react'
import { Search } from 'lucide-react'
import type { Find } from '../../hooks/useFind'

/**
 * The find box on My work (global and per project). `/` jumps to it unless you
 * are already typing somewhere; Escape clears it. State lives in `?q=`.
 */
export function FindBox({ find, label, placeholder }: { find: Find; label: string; placeholder: string }) {
  const { ref, raw, setRaw } = find
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return
      const t = e.target as HTMLElement | null
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return
      if (!ref.current) return
      e.preventDefault()
      ref.current.focus()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [ref])
  return (
    <div role="search" className="relative max-w-[420px]">
      <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-ink-3 pointer-events-none" aria-hidden="true" />
      <input ref={ref} type="search" value={raw} onChange={e => setRaw(e.target.value || null)}
        onKeyDown={e => { if (e.key === 'Escape' && raw) { e.preventDefault(); setRaw(null) } }}
        aria-label={label} placeholder={placeholder}
        className="w-full h-9 pl-8 pr-3 text-[13px] bg-surface text-ink border border-line placeholder:text-ink-3" />
    </div>
  )
}

/** "No items match 'x'" with a button that clears the box and returns to it. */
export function NoMatches({ find, what = 'items', note, className = 'amp-panel px-7 py-9 grid gap-2' }: { find: Find; what?: string; note?: string; className?: string }) {
  return (
    <div className={className} role="status">
      <span className="amp-label">No matches</span>
      <p className="m-0 text-ink-2 text-[13.5px]">No {what} match &ldquo;{find.raw.trim()}&rdquo;. Search covers ticket numbers and titles{note ? `, ${note}` : ''}.</p>
      <div><button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={() => { find.setRaw(null); find.ref.current?.focus() }}>Clear</button></div>
    </div>
  )
}
