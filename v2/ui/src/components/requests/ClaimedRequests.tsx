/**
 * "Claimed by me · team requests" — the personal half of My work's request
 * panels (AMP #3983, build slice J3; approved mockup artifact 37 v1, view 3).
 *
 * This is the only place a team request becomes a row in somebody's own list,
 * and what makes it safe is *where the rows come from*: `mine.ids`, the
 * server's deduped union of "I hold the triage claim" and "I own the outcome"
 * (`architecture/request-queue-summaries.md`). So:
 *
 *  - **A request appears at most once.** The dedupe is the server's, not a
 *    `Set` this component happens to add — a request that is both claimed and
 *    assigned by the viewer is one row, and `claimedRequests` keeps a guard
 *    anyway so a server that ever repeated an id still could not double it.
 *  - **Being on the team puts nothing here.** The panel never reads a member
 *    list and never loops over people: there is no code path that could fan one
 *    request out to every member. A request I merely have access to is absent.
 *  - **The count is the server's `mine.total`**, cursor-free, and is not the
 *    number of rows: `mine` carries ids, so a row can only be *named* from a
 *    row the same read sent. When the total exceeds the named rows the panel
 *    says how many it could not list rather than silently dropping them.
 *
 * The right-hand phrase on a row ("claimed 26h", "assigned") is the anti-expiry
 * signal D4 asks for: a claim never expires, so a stale one stays visible
 * instead of being swept away by a clock.
 */
import { ChevronRight } from 'lucide-react'
import { Link } from 'react-router-dom'
import { requestRef, requestStateLabel, requestStateVar } from '../../lib/requestState'
import {
  claimedRequestLink, claimedRequests, claimedTotal, mineLine, plural, unlistedCount,
  type ClaimedRequest, type TeamInbox,
} from '../../lib/teamInboxes'

interface Props {
  /** One entry per team the viewer belongs to, from `useTeamInboxes`. */
  inboxes: TeamInbox[]
  /** True until every team has answered. The panel shows shapes, never zeros. */
  loading?: boolean
}

/** One row: state edge, reference, subject, and how it became yours. */
function ClaimedRow({ row, showTeam }: { row: ClaimedRequest; showTeam: boolean }) {
  const { request, team } = row
  const via = mineLine(row)
  const label = `${requestRef(request.public_ref)}: ${request.subject}, ${requestStateLabel(request.state)}, ${via}${showTeam ? `, in ${team.name}` : ''}`

  return (
    <li className="border-t border-line-sub first:border-t-0">
      <Link
        to={claimedRequestLink(row)}
        aria-label={label}
        className="grid grid-cols-[3px_minmax(0,1fr)_auto] items-center gap-x-3 px-4 py-2.5 hover:bg-surface-2 group"
      >
        <span className="self-stretch" style={{ background: requestStateVar(request.state) }} aria-hidden="true" />
        <span className="min-w-0 grid gap-0.5">
          <span className="flex items-baseline gap-2 min-w-0">
            <span className="amp-num text-[11px] text-ink-3 flex-shrink-0">{requestRef(request.public_ref)}</span>
            <span className="text-[13px] text-ink truncate group-hover:underline">{request.subject}</span>
          </span>
          <span className="text-[11.5px] text-ink-3 truncate">
            {requestStateLabel(request.state).toLowerCase()} · {via}
            {showTeam ? ` · ${team.name}` : ''}
          </span>
        </span>
        <ChevronRight size={15} className="text-ink-3" aria-hidden="true" />
      </Link>
    </li>
  )
}

export function ClaimedRequests({ inboxes, loading = false }: Props) {
  const rows = claimedRequests(inboxes)
  const total = claimedTotal(inboxes)
  const unlisted = unlistedCount(inboxes)
  const showTeam = new Set(rows.map(r => r.team.id)).size > 1

  if (loading && inboxes.length === 0) {
    return (
      <section className="amp-panel min-w-0" aria-label="Claimed by me, team requests" role="status" aria-busy="true">
        <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
          <h2 className="amp-label m-0">Claimed by me · team requests</h2>
        </div>
        <span className="sr-only">Loading your claimed requests</span>
        <div aria-hidden="true" className="px-4 py-3 grid gap-2">
          <span className="amp-skel block h-3 w-48" />
          <span className="amp-skel block h-2.5 w-64" />
        </div>
      </section>
    )
  }

  return (
    <section className="amp-panel min-w-0" aria-label="Claimed by me, team requests">
      <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1 px-4 py-3 border-b border-line">
        <h2 className="amp-label m-0">Claimed by me · team requests</h2>
        {/* The server's count, never the row count: `mine.total` is cursor-free.
            Nothing claimed prints no number at all — an empty list with "0"
            beside it reads as a scoreboard, and the line below says why. */}
        {total > 0 && <span className="ml-auto text-[11px] text-ink-3 amp-num">{plural(total, 'request')}</span>}
      </div>

      {rows.length === 0 ? (
        <p className="m-0 px-4 py-3.5 text-[13px] text-ink-2 leading-snug">
          {total > 0
            ? `${plural(total, 'request')} in your name, but this read didn't send ${total === 1 ? 'its row' : 'their rows'}. Open the team inbox to see ${total === 1 ? 'it' : 'them'}.`
            : 'Nothing claimed or assigned to you. Claim a request in a team inbox and it shows here.'}
        </p>
      ) : (
        <ul className="list-none m-0 p-0">
          {rows.map(row => <ClaimedRow key={row.request.id} row={row} showTeam={showTeam} />)}
        </ul>
      )}

      {/* The total can exceed the named rows because `mine` carries ids; say so
          rather than letting a counted request go missing. */}
      {unlisted > 0 && rows.length > 0 && (
        <p className="m-0 px-4 py-2 border-t border-line-sub text-[11.5px] leading-snug text-ink-3">
          +{plural(unlisted, 'more request')} in your name that this list didn't name. Open the team inbox for {unlisted === 1 ? 'it' : 'all of them'}.
        </p>
      )}

      <p className="m-0 px-4 py-2.5 border-t border-line-sub text-[11.5px] leading-snug text-ink-3">
        Claims never expire — the age beside one shows how long you've held it.
        A request you're only on the team for is not here.
      </p>
    </section>
  )
}
