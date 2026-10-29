/**
 * Dispatch integrity in the UI (AMP #2801, epic 180, story 444; KB
 * decisions/dispatch-integrity.md): a dispatched ticket isn't started until
 * its worker claims it.
 *
 * Which tickets are dispatched-but-unstarted, which the watchdog has flagged,
 * and the `not_started` / `returned` lists are the server's answers, not ours:
 * it derives `phase` on every read (`in_progress` with no `started_at` =
 * "not_started") and sends every "N min" with those lists. Nothing here
 * re-derives them from `started_at`. The only arithmetic is the age of a
 * timestamp the server sent, which every "5m ago" in this UI already does.
 */
import type { Task, WaitItem } from '../types'

/**
 * True when the server says this in_progress ticket has no worker yet. False
 * when it says the ticket is started, and false when it says nothing at all
 * (`phase` absent: an older server), so nothing is invented from `started_at`.
 */
export function isNotStarted(t: Pick<Task, 'state' | 'phase'>): boolean {
  return t.state === 'in_progress' && t.phase === 'not_started'
}

/** Whether the ticket was returned to ready and not dispatched again since. */
export function wasReturned(t: Pick<Task, 'returned_at'>): boolean {
  return !!t.returned_at
}

/**
 * The badge for a dispatched, unstarted ticket: its label and whether the
 * watchdog has flagged it (which only makes the same badge louder).
 *
 * `minutes` is the server's number: `not_started_minutes` on the ticket, or
 * the minutes on a My work `start` item. `flagMinutes` is the project's
 * not_started_flag_minutes (0 = the flag is off: the badge still shows,
 * quiet). With no minutes from the server the badge still shows; it just
 * can't say how long, and it is never filled on a guess.
 */
export function notStartedBadge(
  t: Pick<Task, 'state' | 'phase' | 'not_started_minutes'>,
  opts: { minutes?: number; flagMinutes?: number } = {},
): { label: string; flagged: boolean; minutes?: number } | null {
  if (!isNotStarted(t)) return null
  const minutes = opts.minutes ?? t.not_started_minutes
  const flagged = !!opts.flagMinutes && opts.flagMinutes > 0 && minutes != null && minutes >= opts.flagMinutes
  return { label: 'Dispatched · not started', flagged, minutes }
}

/** "since 12 min", from a number the server sent. Empty when it sent none. */
export function sinceMinutes(minutes?: number): string {
  return minutes == null ? '' : `since ${minutes} min`
}

/**
 * "Returned: <reason>" while a ticket the watchdog (or amp_return_task) sent
 * back still carries its return. The reason is the server's own words; the
 * next dispatch clears it, so a re-dispatched ticket shows nothing here.
 */
export function returnedLine(t: Pick<Task, 'returned_at' | 'returned_reason'>): string | null {
  if (!t.returned_at) return null
  const reason = t.returned_reason?.trim()
  return reason ? `Returned: ${reason}` : 'Returned to ready'
}

/** The tooltip on the badge: who it went to, and what happens next. */
export function notStartedHint(t: Pick<Task, 'agent_id' | 'assigned_to'>): string {
  const agent = t.agent_id || t.assigned_to
  return `Dispatched${agent ? ` to ${agent}` : ''}; no worker has started it yet. ` +
    'Its worker starts it with a claim; if none does, AMP returns it to ready by itself.'
}

/** What the drawer's returned line means, in full. */
export function returnedHint(reason?: string): string {
  const r = reason?.trim()
  return (r ? `Returned to ready: ${r}. ` : 'Returned to ready because no worker started it. ') +
    'Dispatch it again to run it, or leave it ready.'
}

/**
 * What a "start" item in My work says: the agent, and how long it has waited.
 * Both come from the server (`agent_id`, `not_started_minutes`).
 */
export function startItemText(it: Pick<WaitItem, 'agent_id' | 'not_started_minutes'>): string {
  const who = it.agent_id ? ` to ${it.agent_id}` : ''
  const howLong = it.not_started_minutes != null ? ` ${it.not_started_minutes} min ago` : ''
  return `Dispatched${who}${howLong}; no worker has started it.`
}
