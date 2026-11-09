/**
 * Decide-by dates on a person's tickets (#146). The server stores an instant
 * (RFC 3339, UTC); the UI shows it relative ("in 2d", "3h overdue") and
 * absolute in the viewer's own time zone, with the zone named, so a date set
 * in one zone reads right in another.
 */

export interface DecideByView {
  /** "in 2d", "in 3h", "in 12m", "due now", "3h overdue", "2d overdue" */
  relative: string
  /** "Thu, Nov 12, 5:00 PM EST": the viewer's zone, named */
  absolute: string
  overdue: boolean
}

function span(ms: number): string {
  const mins = Math.floor(ms / 60000)
  if (mins < 60) return `${Math.max(mins, 1)}m`
  const hrs = Math.floor(mins / 60)
  if (hrs < 48) return `${hrs}h`
  return `${Math.floor(hrs / 24)}d`
}

/** How a decide-by date reads at now; null for none or an unreadable one. */
export function describeDecideBy(iso: string | null | undefined, now = Date.now(), timeZone?: string): DecideByView | null {
  if (!iso) return null
  const at = Date.parse(iso)
  if (Number.isNaN(at)) return null
  const diff = at - now
  const overdue = diff < 0
  const relative = Math.abs(diff) < 60000 ? (overdue ? 'due now' : 'in 1m')
    : overdue ? `${span(-diff)} overdue` : `in ${span(diff)}`
  const absolute = new Date(at).toLocaleString(undefined, {
    weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', timeZoneName: 'short', timeZone,
  })
  return { relative, absolute, overdue }
}

const pad = (n: number) => String(n).padStart(2, '0')

/** An instant as a datetime-local input's value, in the viewer's zone. */
export function toLocalInput(iso: string | null | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/**
 * A datetime-local input's value (the viewer's wall-clock time) as the
 * RFC 3339 UTC instant the server stores; null when it isn't a date.
 */
export function fromLocalInput(value: string): string | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(value.trim())
  if (!m) return null
  const [, y, mo, d, h, mi] = m.map(Number)
  const local = new Date(y, mo - 1, d, h, mi)
  if (Number.isNaN(local.getTime()) || local.getMonth() !== mo - 1) return null
  return local.toISOString().replace(/\.\d{3}Z$/, 'Z')
}
