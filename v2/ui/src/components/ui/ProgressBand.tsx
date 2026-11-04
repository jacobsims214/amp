import { useDelayedFlag } from '../../hooks/useDelayedFlag'

/**
 * Tier 2: a 2px band bar under a page header while data that is already on
 * screen refetches. The content stays mounted; this only says "updating".
 * It appears only once the refetch has run past the indicator delay.
 */
export function ProgressBand({ active }: { active: boolean }) {
  const show = useDelayedFlag(active, undefined, 400)
  // Always reserve the 2px, so the bar coming and going never moves the page.
  return (
    <div className="relative h-[2px] flex-shrink-0 overflow-hidden" aria-hidden="true">
      {show && <span className="amp-progress-band" data-testid="progress-band" />}
    </div>
  )
}
