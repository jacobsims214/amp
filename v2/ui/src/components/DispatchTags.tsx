/**
 * The two dispatch-integrity tags (AMP #2801, epic 180, story 444):
 * amber "Dispatched · not started" for an in_progress ticket the server says no
 * worker has started, and "Returned" on a ticket the watchdog sent back.
 *
 * Amber is the warning token, not the red question flag: nothing is wrong with
 * the ticket, its worker just isn't running. Once the watchdog has flagged it
 * the badge is filled, so a scan of the board finds it.
 */
import { RotateCcw } from 'lucide-react'
import type { Task } from '../types'
import { notStartedBadge, notStartedHint, returnedHint, returnedLine } from '../lib/dispatchState'

interface NotStartedProps {
  task: Task
  /** the server's minutes for this ticket (a not_started list row), for "· 12 min" */
  minutes?: number
  /** the project's not_started_flag_minutes: past it the badge is filled; 0 = the flag is off */
  flagMinutes?: number
  /** the board card has less room than the drawer */
  compact?: boolean
}

/** Amber "Dispatched · not started"; nothing when the ticket isn't one. */
export function NotStartedTag({ task, minutes, flagMinutes, compact = false }: NotStartedProps) {
  const badge = notStartedBadge(task, { minutes, flagMinutes })
  if (!badge) return null
  const long = badge.minutes != null
  return (
    <span data-testid="not-started" data-flagged={badge.flagged || undefined}
      title={notStartedHint(task)}
      className={`inline-flex items-center gap-1 whitespace-nowrap border px-1.5 py-[3px] font-label font-semibold uppercase leading-none tracking-[0.1em] ${compact ? 'text-[10px]' : 'text-[10.5px]'}`}
      style={{
        borderRadius: 'var(--radius)',
        borderColor: 'var(--warning)',
        background: badge.flagged ? 'var(--warning)' : 'transparent',
        color: badge.flagged ? '#fff' : 'var(--warning-ink)',
      }}>
      <i className="inline-block w-1.5 h-1.5" aria-hidden="true"
        style={{ borderRadius: 1, background: badge.flagged ? '#fff' : 'var(--warning)' }} />
      {badge.label}{long && !compact ? ` · ${badge.minutes} min` : ''}
    </span>
  )
}

/** "Returned" on a ticket the watchdog sent back to ready, with its reason on hover. */
export function ReturnedTag({ task, compact = false }: { task: Task; compact?: boolean }) {
  const line = returnedLine(task)
  if (!line) return null
  return (
    <span data-testid="returned"
      title={returnedHint(task.returned_reason)}
      className={`inline-flex items-center gap-1 whitespace-nowrap border px-1.5 py-[3px] font-label font-semibold uppercase leading-none tracking-[0.1em] ${compact ? 'text-[10px]' : 'text-[10.5px]'}`}
      style={{ borderRadius: 'var(--radius)', borderColor: 'var(--line-str)', color: 'var(--ink-2)' }}>
      <RotateCcw size={compact ? 10 : 11} aria-hidden="true" />Returned
    </span>
  )
}

/** The drawer's own line: "Returned: <reason>", in the server's words. */
export function ReturnedLine({ task }: { task: Task }) {
  const line = returnedLine(task)
  if (!line) return null
  return <span data-testid="returned-line" title={returnedHint(task.returned_reason)}>{line}</span>
}
