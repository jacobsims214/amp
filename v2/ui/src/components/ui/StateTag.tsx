import { STATE_HINT, STATE_LABEL, STATE_VAR, type DisplayState } from '../../lib/ticketState'

// A ticket state as a small square-cornered tag. Flagged is filled red so it
// reads as an alarm; waiting and ready are outlined (nothing is happening).

export function StateDot({ state, size = 8 }: { state: DisplayState; size?: number }) {
  const hollow = state === 'wait' || state === 'ready'
  return (
    <i
      className="inline-block flex-shrink-0"
      style={{ width: size, height: size, borderRadius: 1, background: hollow ? 'transparent' : STATE_VAR[state],
        boxShadow: hollow ? `inset 0 0 0 1.5px ${STATE_VAR[state]}` : undefined }}
      aria-hidden="true"
    />
  )
}

export function StateTag({ state, label, title }: { state: DisplayState; label?: string; title?: string }) {
  const flag = state === 'flag'
  return (
    <span
      title={title ?? STATE_HINT[state]}
      className="inline-flex items-center gap-1.5 whitespace-nowrap border px-1.5 py-[4px] font-label text-[10.5px] font-semibold uppercase leading-none tracking-[0.1em]"
      style={{
        borderRadius: 'var(--radius)',
        background: flag ? 'var(--st-flag)' : 'var(--surface)',
        borderColor: flag ? 'var(--st-flag)' : 'var(--line)',
        color: flag ? '#fff' : 'var(--ink)',
      }}
    >
      {flag ? <i className="inline-block w-2 h-2 bg-white" style={{ borderRadius: 1 }} aria-hidden="true" /> : <StateDot state={state} />}
      {label ?? STATE_LABEL[state]}
    </span>
  )
}

/** A thin bar split by state, e.g. an epic's progress. */
export function StateBar({ counts, height = 6, order = ['done', 'prog', 'flag', 'sched', 'ready', 'wait'] }: {
  counts: Record<DisplayState, number>; height?: number; order?: DisplayState[]
}) {
  const total = order.reduce((a, s) => a + counts[s], 0)
  if (!total) return <div style={{ height }} className="bg-surface-3" />
  return (
    <div className="flex gap-[2px]" style={{ height }} role="img"
      aria-label={order.filter(s => counts[s]).map(s => `${counts[s]} ${STATE_LABEL[s].toLowerCase()}`).join(', ')}>
      {order.filter(s => counts[s]).map(s => (
        <i key={s} className="block h-full" title={`${counts[s]} ${STATE_LABEL[s].toLowerCase()}`}
          style={{ flex: counts[s], background: s === 'wait'
            ? `repeating-linear-gradient(90deg, var(--st-wait) 0 2px, transparent 2px 4px)`
            : s === 'ready' ? 'color-mix(in srgb, var(--ink) 25%, var(--surface))' : STATE_VAR[s] }} />
      ))}
    </div>
  )
}
