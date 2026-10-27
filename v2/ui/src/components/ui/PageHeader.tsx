import { NavLink } from 'react-router-dom'

// The top of every page: where you are (crumb), the title, page actions on
// the right, and optional view tabs underneath.

export interface PageTab {
  to: string; label: string; end?: boolean; count?: number
  /** the count needs the viewer (white on the accent); otherwise it's quiet */
  hot?: boolean
  /** what the count is, for the link's accessible name ("My work, 6 waiting on you") */
  countLabel?: string
}

export function PageHeader({ crumb, title, meta, actions, tabs, children }: {
  crumb?: React.ReactNode
  title: React.ReactNode
  meta?: React.ReactNode
  actions?: React.ReactNode
  tabs?: PageTab[]
  children?: React.ReactNode
}) {
  return (
    <header className="bg-surface border-b border-line flex-shrink-0">
      <div className="flex items-end gap-4 px-7 pt-5 pb-4 flex-wrap">
        <div className="min-w-0">
          {crumb && <div className="amp-label mb-2">{crumb}</div>}
          <h1 className="amp-heading text-[24px] leading-[1.05] text-ink m-0 flex items-center gap-3 flex-wrap">{title}</h1>
          {meta && <div className="mt-2 text-[13px] text-ink-2">{meta}</div>}
        </div>
        {actions && <div className="ml-auto flex items-center gap-2 flex-wrap">{actions}</div>}
      </div>
      {children}
      {tabs && (
        <nav className="flex px-7 gap-0 -mb-px overflow-x-auto" aria-label="Views">
          {tabs.map(t => (
            <NavLink key={t.to} to={t.to} end={t.end} aria-label={t.count != null && t.countLabel ? `${t.label}, ${t.count} ${t.countLabel}` : undefined}
              className={({ isActive }) => `font-label text-[12px] font-semibold uppercase tracking-[0.16em] px-3.5 pt-3 pb-2.5 border-b-2 whitespace-nowrap ${
                isActive ? 'text-ink border-accent' : 'text-ink-3 border-transparent hover:text-ink-2'}`}>
              {t.label}{t.count != null && (
                <span className={`amp-num ml-1.5 tracking-normal ${t.hot ? 'text-white bg-accent px-[5px] py-px' : 'text-ink-3'}`}>{t.count}</span>
              )}
            </NavLink>
          ))}
        </nav>
      )}
    </header>
  )
}

/** A section label with a rule to its right. */
export function SectionLabel({ children, right }: { children: React.ReactNode; right?: React.ReactNode }) {
  return (
    <div className="flex items-center gap-3">
      <span className="amp-label whitespace-nowrap">{children}</span>
      <span className="flex-1 h-px bg-line" />
      {right}
    </div>
  )
}

/** A white panel; `top` adds the black rule along its top edge. */
export function Panel({ title, label, right, top, className = '', children }: {
  title?: React.ReactNode; label?: React.ReactNode; right?: React.ReactNode; top?: boolean; className?: string; children: React.ReactNode
}) {
  return (
    <section className={`amp-panel ${top ? 'amp-panel-top' : ''} ${className}`}>
      {(title || label || right) && (
        <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
          {label && <span className="amp-label">{label}</span>}
          {title && <h2 className="m-0 text-[14.5px] font-semibold text-ink">{title}</h2>}
          {right && <div className="ml-auto flex items-center gap-2 text-[11px] amp-num text-ink-3">{right}</div>}
        </div>
      )}
      {children}
    </section>
  )
}

/** A row of large numbers (readouts) under a black top rule. */
export function Readouts({ items }: { items: { label: string; value: React.ReactNode; sub?: React.ReactNode; color?: string; alarm?: boolean }[] }) {
  return (
    <div className="amp-panel amp-panel-top grid" style={{ gridTemplateColumns: `repeat(${items.length}, minmax(0, 1fr))` }}>
      {items.map((it, i) => (
        <div key={it.label} className={`px-4 py-3 grid gap-1.5 ${i ? 'border-l border-line' : ''}`}
          style={it.alarm ? { background: 'var(--st-flag)' } : undefined}>
          <span className="amp-label" style={it.alarm ? { color: '#fff' } : undefined}>{it.label}</span>
          <span className="amp-num text-[26px] leading-none" style={{ color: it.alarm ? '#fff' : it.color ?? 'var(--ink)' }}>{it.value}</span>
          {it.sub && <span className="text-[11.5px]" style={{ color: it.alarm ? '#fff' : 'var(--ink-2)' }}>{it.sub}</span>}
        </div>
      ))}
    </div>
  )
}
