/**
 * One request in the team inbox queue (mockup artifact 37 v1, view 1, the row
 * pattern).
 *
 * It follows `TaskCard`: a white square-cornered row with a state-coloured rule
 * down its left edge, the mono reference first, the subject as the loudest
 * thing, and a meta line of small facts. What it does NOT copy is the card's
 * "the whole card is one button": a request row carries its own action (Claim),
 * and nesting a button inside a button is invalid and unreachable by keyboard.
 * So the subject is the row's one link, and the action sits beside it as a
 * sibling.
 *
 * Claim is the only primary (filled) action, and only on an unclaimed row —
 * mockup 37's rule. Everything else a triager does to a request lives on its
 * detail page (J2), which this row links to but does not pre-empt.
 */
import { Clock } from 'lucide-react'
import type { RequestStatus, TeamRequest } from '../../types'
import type { Person } from '../../lib/people'
import { Avatar } from '../ui/Avatar'
import {
  canRelease, claimHolder, claimLabel, claimLine, isClaimable, requestRef, requestRequester,
  requestSourceLabel, requestStateLabel, requestStateVar, REQUEST_STATE_HINT,
} from '../../lib/requestState'

interface Props {
  request: TeamRequest
  /** The caller's user id: decides "You" and which secondary action a claimed row shows. */
  meId?: number
  /** The team's members, for resolving a claim holder's name. */
  people: Map<number, Person>
  /** The claim request in flight for this row, so the button says so and cannot be double-fired. */
  claiming?: boolean
  /** Claim, from the row. Resolves when the server answered; a lost race is the page's banner. */
  onClaim: (req: TeamRequest) => void
  /** Hand the claim back. Only rendered when the caller holds it. */
  onRelease?: (req: TeamRequest) => void
  /** Open the request. The routing ticket wires this to a real path; today it is the page's callback. */
  onOpen: (req: TeamRequest) => void
}

/** A state dot, matching the pill row's dots so a row and its pill agree. */
function StateDot({ state }: { state: RequestStatus }) {
  return (
    <i
      className="inline-block w-2 h-2 flex-shrink-0"
      style={{ background: requestStateVar(state), borderRadius: 1 }}
      aria-hidden="true"
    />
  )
}

export function RequestRow({ request, meId, people, claiming = false, onClaim, onRelease, onOpen }: Props) {
  const state = request.state
  const holder = claimHolder(request, people, meId)
  const claim = claimLine(request)
  const claimable = isClaimable(request)
  const releasable = canRelease(request, meId)

  // The one-line accessible name: everything a sighted person reads off the row,
  // in the order they read it. A screen-reader user gets the same picture.
  const label = [
    `${requestRef(request.public_ref)}: ${request.subject}`,
    requestStateLabel(state),
    claimLabel(request, people, meId),
  ].join(', ')

  return (
    <div
      role="listitem"
      aria-label={label}
      className="grid items-stretch bg-surface border border-line hover:border-line-str transition-colors grid-cols-[3px_minmax(0,1fr)] sm:grid-cols-[3px_minmax(0,1fr)_auto]"
      style={{ borderRadius: 'var(--radius)' }}
    >
      {/* The state edge: the row's colour, from the same table the pills use. */}
      <span className="row-span-2 sm:row-span-1" style={{ background: requestStateVar(state) }} aria-hidden="true" />

      <div className="px-3 py-2.5 flex flex-col gap-1.5 min-w-0">
        <div className="flex items-baseline gap-2.5 min-w-0">
          <span className="amp-num text-[11px] text-ink-3 flex-shrink-0">{requestRef(request.public_ref)}</span>
          <button
            type="button"
            onClick={() => onOpen(request)}
            className="text-[13.5px] font-semibold text-ink text-left hover:underline decoration-1 underline-offset-2 cursor-pointer min-w-0 sm:truncate"
            title={request.subject}
          >
            {request.subject}
          </button>
        </div>

        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-[12px] text-ink-2 min-w-0">
          <span className="min-w-0 truncate">
            <span className="text-ink-3">from</span> {requestRequester(request)}
          </span>
          <span>
            <span className="text-ink-3">via</span> {requestSourceLabel(request.source_kind)}
          </span>
          {/* The claim's age, beside the claim itself (D4): a stale claim is
              visible here rather than silently dropped by a clock. */}
          {claim && (
            <span className="inline-flex items-center gap-1 text-ink-2">
              <Clock size={11} aria-hidden="true" className="flex-shrink-0" />
              {claim}
            </span>
          )}
          {state === 'needs_information' && request.needs_info_since && (
            <span className="text-danger-ink">waiting on requester</span>
          )}
          {request.reopened_count > 0 && (
            <span className="text-ink-3">reopened {request.reopened_count}×</span>
          )}
        </div>
      </div>

      {/* At phone width this column drops below the content and runs along it,
          so the subject keeps the full width instead of being squeezed into a
          narrow strip. From sm up it is the row's right-hand rail, the mockup's
          layout. */}
      <div className="col-start-2 px-3 pb-2.5 flex flex-row items-center justify-between gap-2.5 flex-wrap sm:col-start-3 sm:pr-3 sm:py-2.5 sm:flex-col sm:items-end sm:justify-center sm:gap-1.5">
        <span
          className="inline-flex items-center gap-1.5 text-[11.5px] font-medium whitespace-nowrap"
          title={REQUEST_STATE_HINT[state]}
        >
          <StateDot state={state} />
          {requestStateLabel(state)}
        </span>

        {/* Claim is the only primary action, and only where it is the right one. */}
        {claimable ? (
          <button
            type="button"
            onClick={() => onClaim(request)}
            disabled={claiming}
            aria-busy={claiming || undefined}
            className="amp-btn amp-btn-sm amp-btn-primary"
          >
            {claiming ? 'Claiming…' : 'Claim'}
          </button>
        ) : releasable ? (
          <button
            type="button"
            onClick={() => onRelease?.(request)}
            className="amp-btn amp-btn-sm amp-btn-ghost"
            title="Give the claim back — claims never expire on their own"
          >
            Release
          </button>
        ) : (
          // Someone else's claim: the holder, shown as the mockup's claim tag
          // (avatar + name). It is deliberately not a control — a claim is never
          // taken over, so there is no action to offer here.
          <span
            className="inline-flex items-center gap-1.5 text-[11.5px] text-ink-2 whitespace-nowrap"
            title={`${holder.kind === 'other' ? holder.person.name : 'Someone'} is triaging this. A claim is never taken over; a lead transfers the assignee instead.`}
          >
            <Avatar name={holder.kind === 'other' ? holder.person.name : 'Unknown'} size={18} />
            <span className="max-w-[9rem] truncate">{holder.kind === 'other' ? holder.person.name : 'Claimed'}</span>
          </span>
        )}
      </div>
    </div>
  )
}
