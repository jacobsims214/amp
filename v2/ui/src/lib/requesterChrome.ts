/**
 * The small pieces of the requester frame that are not JSX (ticket AMP #3981).
 *
 * They live in `lib/` rather than beside the component for two reasons: a
 * component file that also exports helpers breaks React Fast Refresh, and both
 * are pure functions worth testing on their own.
 */

/** Who is signed in, as the requester chrome says it: the two facts a header needs. */
export interface RequesterIdentity {
  /** Their name, when AMP knows one. */
  name?: string
  /** Their sign-in address — the identity a request is visible from. */
  email: string
}

/** "MR" from "Marta Reyes"; the email's first letter when there is no name. */
export function initials(me: RequesterIdentity): string {
  const parts = (me.name ?? '').trim().split(/\s+/).filter(Boolean)
  if (parts.length === 0) return (me.email.trim()[0] ?? '?').toUpperCase()
  const first = parts[0][0] ?? ''
  const last = parts.length > 1 ? parts[parts.length - 1][0] ?? '' : ''
  return `${first}${last}`.toUpperCase()
}

/**
 * How the identity chip names the person: their name, else their address.
 *
 * The address is the fallback rather than the default because it is the fact
 * that matters here — a request is visible only from the address that sent it —
 * so a person with no name still sees which sign-in they are.
 */
export function identityLabel(me: RequesterIdentity): string {
  const name = (me.name ?? '').trim()
  return name || me.email
}

/**
 * One of the frame's two tabs. The page passes its own links in, because this
 * ticket owns no routing; the class lives here so the two tabs cannot drift.
 */
export function requesterNavItemClass(current: boolean): string {
  return [
    'inline-block border-b-2 px-3 pb-[9px] pt-[10px] font-label text-[11.5px] font-semibold uppercase tracking-[0.14em]',
    current ? 'border-ink text-ink' : 'border-transparent text-ink-2 hover:text-ink',
  ].join(' ')
}
