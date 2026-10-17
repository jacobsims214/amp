// Priority 0–3 as four rising bars: how many are filled is the priority;
// critical is all red.
const LABEL = ['Low', 'Normal', 'High', 'Critical']

export function PriorityMark({ priority, showLabel = false }: { priority: string | number; showLabel?: boolean }) {
  const p = Math.max(0, Math.min(3, Number(priority) || 0))
  return (
    <span className="inline-flex items-center gap-1.5" title={`Priority ${p}: ${LABEL[p]}`}>
      <span className="inline-flex items-end gap-[2px] h-[13px]" aria-hidden="true">
        {[4, 7, 10, 13].map((h, i) => (
          <i key={i} className="block w-[3px]" style={{ height: h,
            background: p === 3 ? 'var(--st-flag)' : i <= p ? 'var(--ink)' : 'var(--line)' }} />
        ))}
      </span>
      {showLabel && <span className="font-label text-[10.5px] font-semibold uppercase tracking-[0.1em] text-ink-2">{LABEL[p]}</span>}
      {!showLabel && <span className="sr-only">Priority {LABEL[p]}</span>}
    </span>
  )
}
