import { INDICATOR_DELAY_MS, useDelayedFlag } from '../../hooks/useDelayedFlag'

/**
 * The countdown loader: AMP's bands light in launch order while a page waits on
 * data whose shape it does not know (shaped waits use the Skeleton primitives).
 * It shows nothing until the wait has run `delay` ms, so a fast load never
 * flashes it; once up it stays at least 400ms. The caption is always visible:
 * under reduced motion the bands hold still, and the words carry the wait.
 */
export function BandLoader({ label, className = '', delay = INDICATOR_DELAY_MS }: { label?: string; className?: string; delay?: number }) {
  const text = label ?? 'Loading'
  const shown = useDelayedFlag(true, delay, 400)
  // Before the delay: an empty live region, so assistive tech still knows a wait began.
  if (delay > 0 && !shown) return <div role="status" aria-label={text} className={className} />
  return (
    <div role="status" aria-label={text} className={`amp-band-loader ${className}`}>
      <span className="amp-band-loader-seg" aria-hidden="true"><i /><i /><i /><i /></span>
      <span className="amp-band-loader-cap">{text}</span>
    </div>
  )
}
