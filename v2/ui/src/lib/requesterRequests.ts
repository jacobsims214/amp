/**
 * The requester's own-requests list, decided here rather than inline in the
 * page (AMP #3986, build slice J5 of
 * `discoveries/team-inbox-approved-build-slices.md`; approved requester mockup
 * artifact 38 v1, the "Your requests" screen; approved product contract
 * artifact 36 v2 §4).
 *
 * `pages/RequesterRequests.tsx` renders the list; this file decides the four
 * things a page must not decide in JSX, each with its own test:
 *
 *  1. **What a row says about its state.** One sentence per lifecycle state,
 *     in the requester's own words, and the server's `state_word` is always the
 *     tag's label — a row never invents a state, softens one, or shows a state
 *     the server did not name.
 *  2. **How a row is coloured.** The mockup's four tones (needs-reply = red,
 *     delivered = green, in progress = blue, closed = grey) extended to the two
 *     states the mockup does not draw, so no state is an unstyled hole.
 *  3. **What the filter pills mean.** The mockup draws five pills; its four
 *     sample rows leave `new` and `triaging` unreachable from any of them, so
 *     the mapping here is total: every lifecycle state is in exactly one pill,
 *     which is what lets `All` be the union of the others rather than a sixth
 *     unrelated answer.
 *  4. **The reference.** A requester's only address for a request is the opaque
 *     `public_ref` the server minted at submission (D1). It is case-sensitive
 *     (base64url; the server resolves `WHERE public_ref = $1`), so it is never
 *     folded, upper-cased or truncated for display.
 *
 * The type it reads is `RequesterRequestView` — the server's whitelist — and
 * never the team's `TeamRequest`. There is no field here for a project, a
 * claim, an assignee, a branch or a metadata key, so a row built from this
 * module cannot render one even by accident.
 */
import type { RequestStatus, RequesterRequestView } from '../types'
import { REQUEST_STATUSES } from '../types'
import { ago } from './approval'
import { requestRef } from './requestState'

// ---- The state tag: one word, one tone ----

/** The mockup's five tag tones (artifact 38 v1: `.tag-flag/.tag-ok/.tag-prog/.tag-done/.tag-wait`). */
export type RequesterTone = 'flag' | 'ok' | 'prog' | 'done' | 'wait'

/**
 * Each lifecycle state's tone, on the mockup's scale.
 *
 * The mockup draws four of the nine states. The other five are placed on the
 * same scale rather than left uncoloured:
 *
 *   new            wait  — received, nothing is happening yet
 *   triaging       prog  — the desk is reviewing it: work in flight, nothing needed from you
 *   needs_info     flag  — the only state that needs the requester; the mockup's red alarm
 *   accepted       prog  — queued for work
 *   in_progress    prog  — the mockup's blue
 *   delivered      ok    — the mockup's green: an outcome exists, their call
 *   requester_accepted done — over, by their own decision (the mockup's grey row)
 *   rejected       done  — closed by the desk; the server's word for it is "Closed"
 *   duplicate      done  — closed into another request
 */
export const REQUESTER_STATE_TONE: Readonly<Record<RequestStatus, RequesterTone>> = {
  new: 'wait',
  triaging: 'prog',
  needs_information: 'flag',
  accepted: 'prog',
  in_progress: 'prog',
  delivered: 'ok',
  requester_accepted: 'done',
  rejected: 'done',
  duplicate: 'done',
}

/** The left rule's colour: the mockup's accent/green/blue/grey, as CSS custom properties. */
export const REQUESTER_TONE_VAR: Readonly<Record<RequesterTone, string>> = {
  flag: 'var(--accent)',
  ok: 'var(--success)',
  prog: 'var(--st-prog)',
  done: 'var(--line-str)',
  wait: 'var(--st-wait)',
}

/** The tone for a state. An unrecognised state is neutral, never a guess at a warning. */
export function requesterTone(state: RequestStatus): RequesterTone {
  return REQUESTER_STATE_TONE[state] ?? 'wait'
}

/** The left rule's colour for a state. */
export function requesterStateVar(state: RequestStatus): string {
  return REQUESTER_TONE_VAR[requesterTone(state)]
}

/**
 * The tag's label: **the server's word**, never a table of the UI's own.
 *
 * `state_word` is server-side for a reason (contract §4): nine lifecycle states
 * need one consistent word, and the approved mockup specifies it. An empty word
 * is a bug made visible rather than a state invented, so it reads as such
 * instead of falling back to the raw enum.
 */
export function requesterStateWord(view: Pick<RequesterRequestView, 'state' | 'state_word'>): string {
  const word = (view.state_word ?? '').trim()
  return word || 'Status unavailable'
}

/** One sentence naming what the state means for the requester, shown as the tag's title. */
export const REQUESTER_STATE_HINT: Readonly<Record<RequestStatus, string>> = {
  new: 'Received. The desk has not reviewed it yet.',
  triaging: 'The desk is reviewing it. Nothing is needed from you.',
  needs_information: 'The desk asked you a question. Your reply moves it forward.',
  accepted: 'The desk accepted it and has not started the work yet. Nothing is needed from you.',
  in_progress: 'Being worked on. Nothing is needed from you right now.',
  delivered: 'An outcome exists. Only you can accept it — or say what is missing.',
  requester_accepted: 'You accepted the outcome. This request is finished.',
  rejected: 'The desk closed this request without delivering it.',
  duplicate: 'Closed — folded into another request of yours.',
}

/** The tag's title for a state. */
export function requesterStateHint(state: RequestStatus): string {
  return REQUESTER_STATE_HINT[state] ?? 'The desk has this request.'
}

// ---- A row's two lines ----

/**
 * A short date for the row's meta line: "Nov 8". The exact moment stays in the
 * row's `title`, so one hover answers "when exactly" without the list reading
 * like a log file.
 */
export function requesterDate(iso: string | null | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

/**
 * The row's second line: what this request's state means, in requester terms.
 *
 * Every sentence is about the requester's own request and the desk's next step.
 * None names a ticket, a run, an agent or a branch — and none promises a
 * deadline, because AMP stores no SLA (contract: needs_info_since is the only
 * clock).
 *
 * The relative phrase falls back to a timeless sentence when the server did not
 * send the timestamp it needs, rather than printing "ago" with nothing in front
 * of it.
 */
export function requesterRowLine(view: RequesterRequestView, now = Date.now()): string {
  switch (view.state) {
    case 'new':
      return 'Received. The desk will review it — usually within one business day.'
    case 'triaging':
      return 'The desk is reviewing it. Nothing is needed from you right now.'
    case 'needs_information': {
      const since = ago(view.needs_reply_since, now)
      return since
        ? `The desk asked you a question ${since} — your reply moves this forward.`
        : 'The desk asked you a question — your reply moves this forward.'
    }
    case 'accepted':
    case 'in_progress':
      return 'Being worked on. Nothing needed from you right now.'
    case 'delivered': {
      const when = ago(view.delivered_at, now)
      return when
        ? `Delivered ${when} — check the outcome and accept it, or say what's missing.`
        : 'Delivered — check the outcome and accept it, or say what\'s missing.'
    }
    case 'requester_accepted': {
      const when = requesterDate(view.accepted_at)
      return when ? `You accepted the outcome on ${when}.` : 'You accepted the outcome.'
    }
    case 'rejected':
      return 'Closed — the desk declined this request.'
    case 'duplicate':
      return 'Closed — folded into another request of yours.'
  }
}

/**
 * The meta line: `REQ-8F3K-2XQ7 · opened Nov 8`, and ` · closed Oct 21` once it
 * is closed. The reference keeps its case: it is what a person pastes into a
 * mail to the desk, so a display transform would produce a reference that no
 * longer resolves.
 */
export function requesterMetaLine(view: RequesterRequestView): string {
  const parts = [requestRef(view.public_ref)]
  const opened = requesterDate(view.created_at)
  if (opened) parts.push(`opened ${opened}`)
  const closed = requesterDate(view.closed_at)
  if (closed) parts.push(`closed ${closed}`)
  return parts.join(' · ')
}

/** The exact moment behind the meta line, for the row's `title`. */
export function requesterMetaTitle(view: RequesterRequestView): string {
  const at = (iso: string | undefined) => {
    if (!iso) return ''
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
  }
  const opened = at(view.created_at)
  const closed = at(view.closed_at)
  return [
    requestRef(view.public_ref),
    opened ? `opened ${opened}` : '',
    closed ? `closed ${closed}` : '',
  ].filter(Boolean).join(' · ')
}

// ---- The filter pills ----

/** The five pills the approved mockup draws, as stable keys. */
export type RequesterListPill = 'all' | 'needs_reply' | 'in_progress' | 'delivered' | 'closed'

export interface RequesterPill {
  key: RequesterListPill
  /** The mockup's label, verbatim. */
  label: string
  /** The lifecycle states this pill asks the server for. Empty means every state. */
  states: RequestStatus[]
  /** One sentence on what the pill holds, for the pill's title. */
  title: string
}

/**
 * The pills, and the states each one is.
 *
 * The mockup's five labels, verbatim. Its four sample rows cover needs-reply,
 * delivered, in progress and accepted, which leaves `new` and `triaging`
 * unreachable from any pill it draws — so "In progress" carries them here
 * ("with the desk, nothing needed from you"), which is the same claim the row's
 * own line makes. The mapping is total and exclusive (a test asserts it), so
 * `All` is exactly the union of the other four and no request is only findable
 * by remembering to clear the filter.
 *
 * "Closed" is three states because the server gives `requester_accepted`,
 * `rejected` and `duplicate` one word ("Closed", `domain.RequesterStateWord`):
 * one pill, three endings, and the row's own tag says which.
 */
export const REQUESTER_LIST_PILLS: readonly RequesterPill[] = [
  {
    key: 'all',
    label: 'All',
    states: [],
    title: 'Every request you have sent to this desk.',
  },
  {
    key: 'needs_reply',
    label: 'Needs your reply',
    states: ['needs_information'],
    title: 'The desk asked you a question. Your reply moves these forward.',
  },
  {
    key: 'in_progress',
    label: 'In progress',
    states: ['new', 'triaging', 'accepted', 'in_progress'],
    title: 'With the desk — received, being reviewed or being worked on. Nothing is needed from you.',
  },
  {
    key: 'delivered',
    label: 'Delivered',
    states: ['delivered'],
    title: 'The desk delivered an outcome. Only you can accept it.',
  },
  {
    key: 'closed',
    label: 'Closed',
    states: ['requester_accepted', 'rejected', 'duplicate'],
    title: 'Finished — accepted by you, declined, or folded into another request.',
  },
]

/** The pill with this key. `all` for anything unknown, so a stale key can never hide every row. */
export function requesterPill(key: RequesterListPill): RequesterPill {
  return REQUESTER_LIST_PILLS.find(p => p.key === key) ?? REQUESTER_LIST_PILLS[0]
}

/**
 * The query the server takes for a pill: repeated `state` values, which the own
 * list's parser reads (or nothing for `All`, which is "every state").
 *
 * This is a *server* filter, not a view of the loaded page: the own-list route
 * accepts `state`, so the pill and the rows under it describe the same set even
 * while paging.
 */
export function requesterPillFilter(key: RequesterListPill): { state?: RequestStatus[] } {
  const states = requesterPill(key).states
  return states.length > 0 ? { state: states } : {}
}

/** Whether a request belongs in a pill — for tests and for the footer's own accounting. */
export function matchesPill(state: RequestStatus, key: RequesterListPill): boolean {
  const states = requesterPill(key).states
  return states.length === 0 || states.includes(state)
}

/** The pills that carry their own count — every pill but `All`. */
export function pillCountKeys(): RequesterListPill[] {
  return REQUESTER_LIST_PILLS.filter(p => p.key !== 'all').map(p => p.key)
}

/**
 * The `All` pill's count, from the four counts the page already asked for.
 *
 * `All` is every state, and the four other pills partition the nine states, so
 * their sum *is* the total — no fifth read is needed. It is undefined until all
 * four have answered, because a partial sum would be a number that silently
 * shrinks while the other reads land.
 */
export function allPillCount(counts: Partial<Record<RequesterListPill, number>>): number | undefined {
  const keys = pillCountKeys()
  if (keys.some(k => counts[k] == null)) return undefined
  return keys.reduce((sum, k) => sum + (counts[k] ?? 0), 0)
}

/** Every state appears in exactly one non-`All` pill. Used by the mapping's test. */
export function pillStatesCovered(): RequestStatus[] {
  return REQUESTER_LIST_PILLS.filter(p => p.key !== 'all').flatMap(p => p.states)
}

/** The states the mockup's pills leave out, if the mapping ever stops being total. */
export function pillStatesMissing(): RequestStatus[] {
  const covered = new Set(pillStatesCovered())
  return REQUEST_STATUSES.filter(s => !covered.has(s))
}

// ---- The list's own words ----

/**
 * How the list describes its size: the server's cursor-free `total`, never a
 * count of the rows on screen (a page is a page, and "1 request" for somebody
 * with forty would be wrong in the one place a person checks).
 */
export function requesterTotalLabel(total: number, pill: RequesterListPill): string {
  const n = Number.isFinite(total) ? total : 0
  const one = n === 1
  const noun = one ? 'request' : 'requests'
  if (pill === 'all') return `${n} ${noun}`
  return `${n} ${noun} in ${requesterPill(pill).label.toLowerCase()}`
}

/**
 * Which empty state a list is in. The two are not the same news and must not
 * share a sentence: "you have never sent anything" is an invitation, while "this
 * filter has nothing in it" is a filter to clear.
 */
export type RequesterEmpty = 'none-yet' | 'no-matches'

/** The empty state for this answer. */
export function requesterEmpty(pill: RequesterListPill, total: number): RequesterEmpty {
  return pill === 'all' && total === 0 ? 'none-yet' : 'no-matches'
}

/**
 * The heading for the "this filter has nothing in it" state.
 *
 * It names the pill, because that is what the person has to undo — except for
 * `All`, where naming the pill would produce "None of your requests are all
 * right now". That case is a page whose rows are empty while the server's total
 * is not (only reachable if rows were deleted between the count and the page),
 * and it says what is true instead of what a template would say.
 */
export function requesterNoMatchesHeading(pill: RequesterListPill): string {
  return pill === 'all'
    ? 'None of your requests came back.'
    : `None of your requests are ${requesterPill(pill).label.toLowerCase()} right now.`
}

/** How many rows a page asks the server for. */
export const REQUESTER_PAGE_SIZE = 20