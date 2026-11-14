/**
 * "Team inboxes" — the shared-queue half of My work's request panels (AMP
 * #3983, build slice J3; approved mockup artifact 37 v1, view 3).
 *
 * The panel exists to answer one question — *what is sitting in the queues I
 * belong to* — and the whole point of the approved contract is that the answer
 * is **not** a list of your tickets. So:
 *
 *  - **It renders no request rows.** A shared queue summarised as a personal
 *    list is exactly the per-member fan-out the contract forbids: one request
 *    asked once would appear once per team member, and My work would read as
 *    though every member owned it. A team block is a name, its per-state
 *    counts, and a link into the queue where the rows actually live.
 *  - **Every number is the server's `totals`** (`RequestStateTotals`), computed
 *    from the same predicate as the queue's pills. Nothing here counts the rows
 *    it happens to hold — it holds none.
 *  - **A team that could not be read shows no number at all**, not a zero: "0
 *    new" and "we could not ask" are different answers, and the panel says
 *    which one it has.
 *
 * The header states the rule in words ("shared queues, not your tickets") and
 * the footer repeats it, because a count with no rows beside it is a number a
 * reader will otherwise assume is theirs.
 */
import { AlertTriangle, ChevronRight } from 'lucide-react'
import { Link } from 'react-router-dom'
import type { RequestStatus } from '../../types'
import { REQUEST_STATE_HINT, requestStateVar } from '../../lib/requestState'
import { plural, queueStates, queueTotal, stateCountLabel, teamInboxLink, type TeamInbox } from '../../lib/teamInboxes'

interface Props {
  /** One entry per team the viewer belongs to, from `useTeamInboxes`. */
  inboxes: TeamInbox[]
  /** True until every team has answered. The panel shows shapes, never zeros. */
  loading?: boolean
}

/** A state dot, from the same table the queue's pills and rows use. */
function StateDot({ state }: { state: RequestStatus }) {
  return (
    <i
      className="inline-block w-2 h-2 flex-shrink-0"
      style={{ background: requestStateVar(state), borderRadius: 1 }}
      aria-hidden="true"
    />
  )
}

/**
 * One team: its name, "Open inbox", and its per-state counts.
 *
 * The counts are `queueStates`' — the states that carry the queue, in lifecycle
 * order, with empty states left out (a block is a "what is in there" line, not a
 * scoreboard). The block always states the queue's whole size, so leaving the
 * zeros out never hides how much is there.
 */
function TeamBlock({ inbox }: { inbox: TeamInbox }) {
  const states = queueStates(inbox.totals)
  const total = queueTotal(inbox.totals)
  const unreadable = !!inbox.error

  return (
    <li className="px-4 py-3 grid gap-2 border-t border-line-sub first:border-t-0" data-team-id={inbox.team.id}>
      <div className="flex items-baseline gap-2 min-w-0">
        <h3 className="m-0 text-[13.5px] font-semibold text-ink truncate">{inbox.team.name}</h3>
        <Link
          to={teamInboxLink(inbox.team.id)}
          className="ml-auto flex-shrink-0 inline-flex items-center gap-0.5 text-[12.5px] hover:underline"
          style={{ color: 'var(--link)' }}
          aria-label={`Open inbox for ${inbox.team.name}`}
        >
          Open inbox
          <ChevronRight size={13} aria-hidden="true" />
        </Link>
      </div>

      {unreadable ? (
        // No number at all: a failed read must not render as an empty queue.
        <p className="m-0 flex items-start gap-1.5 text-[12px] leading-snug text-ink-3">
          <AlertTriangle size={12} aria-hidden="true" className="flex-shrink-0 mt-[2px]" />
          <span>Couldn't read this queue just now, so no counts are shown. {inbox.error}</span>
        </p>
      ) : inbox.totals ? (
        <>
          {states.length > 0 ? (
            // The mockup's `team-counts`: a run of labelled numbers, not a list.
            // They are spans on purpose — a queue's counts are a sentence about
            // the team, and a `ul` here would put eight list items inside a
            // panel whose only list is its teams.
            <div className="flex flex-wrap gap-x-3.5 gap-y-1 text-[12px] text-ink-2">
              {states.map(row => (
                <span key={row.state} className="inline-flex items-center gap-1.5" title={REQUEST_STATE_HINT[row.state]}>
                  <StateDot state={row.state} />
                  <b className="amp-num font-medium text-ink" style={{ letterSpacing: 0 }}>{row.count}</b>
                  <span>{stateCountLabel(row.state)}</span>
                </span>
              ))}
            </div>
          ) : (
            <p className="m-0 text-[12.5px] text-ink-2">No requests in this queue right now.</p>
          )}
          <p className="m-0 text-[11.5px] text-ink-3">
            {plural(total, 'request')} in the shared queue{total > 0 ? ' — anyone on the team can take one.' : '.'}
          </p>
        </>
      ) : (
        // An older server that answered without `totals`: say nothing rather than zero.
        <p className="m-0 text-[12px] text-ink-3">This queue answered without counts.</p>
      )}
    </li>
  )
}

export function TeamInboxSummary({ inboxes, loading = false }: Props) {
  // Nothing to show while the first answer is in flight, and no team at all is
  // not an empty queue: it is a person who is on no team, and the panel says so
  // instead of drawing an empty list.
  if (loading && inboxes.length === 0) {
    return (
      <section className="amp-panel min-w-0" aria-label="Team inboxes" role="status" aria-busy="true">
        <div className="flex items-center gap-2.5 px-4 py-3 border-b border-line">
          <h2 className="amp-label m-0">Team inboxes</h2>
          <span className="ml-auto text-[11px] text-ink-3">shared queues, not your tickets</span>
        </div>
        <span className="sr-only">Loading your team queues</span>
        <div aria-hidden="true" className="px-4 py-3 grid gap-2">
          <span className="amp-skel block h-3 w-40" />
          <span className="amp-skel block h-2.5 w-56" />
        </div>
      </section>
    )
  }

  return (
    <section className="amp-panel min-w-0" aria-label="Team inboxes">
      <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1 px-4 py-3 border-b border-line">
        <h2 className="amp-label m-0">Team inboxes</h2>
        <span className="ml-auto text-[11px] text-ink-3">shared queues, not your tickets</span>
      </div>

      {inboxes.length === 0 ? (
        <p className="m-0 px-4 py-3.5 text-[13px] text-ink-2 leading-snug">
          You're not on a team with a request inbox yet. A team lead adds you, and its queue appears here.
        </p>
      ) : (
        <>
          <ul className="list-none m-0 p-0">
            {inboxes.map(inbox => <TeamBlock key={inbox.team.id} inbox={inbox} />)}
          </ul>
          <p className="m-0 px-4 py-2.5 border-t border-line-sub text-[11.5px] leading-snug text-ink-3">
            Team counts are the whole queue. Nothing here files a personal ticket for every member —
            a request becomes yours only when you claim or are assigned it.
          </p>
        </>
      )}
    </section>
  )
}
