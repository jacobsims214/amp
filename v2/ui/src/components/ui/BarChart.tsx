export interface Bar {
  key: string
  value: number
  /** shown under the bar; leave empty to skip */
  label?: string
  /** hover text */
  title: string
  highlight?: boolean
}

/**
 * Daily-count bars. HTML rather than a scaled SVG, so axis text stays at
 * text size however wide the panel is.
 */
export function BarChart({ bars, ariaLabel, height = 112 }: { bars: Bar[]; ariaLabel: string; height?: number }) {
  const max = Math.max(1, ...bars.map(b => b.value))
  const ticks = max > 1 ? [max, Math.round(max / 2), 0] : [1, 0]
  return (
    <div role="img" aria-label={ariaLabel} className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2 amp-num text-[10.5px] text-ink-3">
      <div className="relative" style={{ height }}>
        {ticks.map(t => (
          <span key={t} className="absolute right-0 -translate-y-1/2 leading-none" style={{ top: `${(1 - t / max) * 100}%` }}>{t}</span>
        ))}
      </div>
      <div className="relative flex items-end gap-1" style={{ height }}>
        {ticks.map(t => (
          <i key={t} className="absolute inset-x-0 border-t border-line" style={{ top: `${(1 - t / max) * 100}%` }} aria-hidden="true" />
        ))}
        {bars.map(b => (
          <div key={b.key} title={b.title} className="relative flex-1 min-w-0"
            style={{ height: `${b.value / max * 100}%`, background: b.highlight ? 'var(--accent)' : 'var(--ink)' }} />
        ))}
      </div>
      <span />
      <div className="flex gap-1 mt-1.5">
        {bars.map(b => (
          <span key={b.key} className="flex-1 min-w-0 text-center whitespace-nowrap overflow-visible">{b.label ?? ''}</span>
        ))}
      </div>
    </div>
  )
}
