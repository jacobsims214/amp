// Content-shaped placeholders for a page that knows its layout but not its data
// yet. The style lives in index.css (.amp-skel): square edges, a low-contrast
// sweep, and a static fill under prefers-reduced-motion. Gate them with
// useDelayedFlag so a fast load shows nothing at all.

type Size = number | string
const px = (v: Size | undefined) => (typeof v === 'number' ? `${v}px` : v)

/** One placeholder block. */
export function Skeleton({ w, h = 10, className = '' }: { w?: Size; h?: Size; className?: string }) {
  return <span aria-hidden="true" className={`amp-skel block ${className}`} style={{ width: px(w), height: px(h) }} />
}

/** A few lines of text; the last one runs short, like a real paragraph. */
export function SkeletonText({ lines = 2, className = '' }: { lines?: number; className?: string }) {
  return (
    <span aria-hidden="true" className={`grid gap-1.5 ${className}`}>
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton key={i} h={9} w={i === lines - 1 && lines > 1 ? '62%' : '100%'} />
      ))}
    </span>
  )
}

/** Shaped like a TaskCard: bordered, a state rule down the left, an id and a title. */
export function SkeletonCard({ className = '' }: { className?: string }) {
  return (
    <span aria-hidden="true" className={`amp-skel-card grid gap-2 bg-surface border border-line ${className}`}>
      <Skeleton w={34} h={8} />
      <SkeletonText lines={2} />
    </span>
  )
}

/** A readout strip (amp-panel-top) with `cells` label-and-number placeholders. */
export function SkeletonReadouts({ cells }: { cells: number }) {
  return (
    <div aria-hidden="true" className="amp-panel amp-panel-top grid grid-flow-col auto-cols-fr">
      {Array.from({ length: cells }, (_, i) => (
        <div key={i} className={`px-4 py-3 grid gap-2 border-line ${i ? 'border-l' : ''}`}>
          <Skeleton w={64} h={8} />
          <Skeleton w={28} h={22} />
        </div>
      ))}
    </div>
  )
}

/** A section: its rule-and-label header, then a panel of `rows` list rows. */
export function SkeletonSection({ rows = 3 }: { rows?: number }) {
  return (
    <div aria-hidden="true" className="grid gap-2">
      <div className="flex items-center gap-3 mb-1">
        <Skeleton w={140} h={8} />
        <span className="flex-1 h-px bg-line" />
      </div>
      <div className="amp-panel">
        {Array.from({ length: rows }, (_, i) => (
          <div key={i} className="px-4 py-3 grid gap-1.5 border-t border-line-sub first:border-t-0">
            <Skeleton w="55%" h={10} />
            <Skeleton w="32%" h={8} />
          </div>
        ))}
      </div>
    </div>
  )
}

/**
 * The live region around a page's skeletons: screen readers hear the label
 * once, sighted users see the shapes.
 */
export function SkeletonRegion({ label, className = '', children }: { label: string; className?: string; children: React.ReactNode }) {
  return (
    <div role="status" aria-busy="true" aria-label={label} className={className}>
      <span className="sr-only">{label}</span>
      {children}
    </div>
  )
}
