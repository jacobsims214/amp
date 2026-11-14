/**
 * The one read behind My work's two team-inbox panels (AMP #3983, slice J3).
 *
 * **One call per team, not two.** The queue route answers the whole-queue pill
 * totals *and* the caller's own slice in the same response
 * (`GET /api/teams/:id/requests?mine=true`), and the server computes both from
 * one filter, so the two panels cannot disagree. `/requests/summary` (the
 * counts-without-a-page route) was built for panels like these, but the claimed
 * panel also has to *name* rows, and the ids in `mine` can only be named from a
 * row the same read sent. Reading the queue once gives both; reading the
 * summary *and* the queue would be two round trips per team for numbers that
 * could then drift apart.
 *
 * Two things this deliberately does not do:
 *
 *  - **It does not fan out per member.** A team's queue is read once, by the
 *    viewer, for the viewer's own slice. There is no loop over the member list,
 *    so "every member owns every request" is not expressible here.
 *  - **It does not read a queue it has no business reading.** The teams come
 *    from the caller's own memberships (`me.teams`), never from an org's team
 *    list: an org admin can read every queue in the org, but My work is about
 *    the teams you are on.
 *
 * A team whose read fails does not take the others down: its error is recorded
 * on its own entry and the panels render the rest. Counts are only ever what the
 * server answered, so a failed read shows no number at all rather than a zero.
 *
 * **The answers are keyed, not merely stored.** Every read carries the identity
 * and team set it was issued for, and the hook hands back an answer only when
 * that key still matches what is on screen. That is what makes "the previous
 * user's claims are never shown after a sign-in change" true without an effect
 * that clears state: a stale answer simply is not the current key, so the
 * panels see an empty, loading state until the right read lands. It also means
 * a re-render with the same identity and teams re-issues nothing.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { requestApi } from '../api/client'
import type { Me } from '../types'
import { CLAIMED_ROWS, inboxTeams, type InboxTeam, type TeamInbox } from '../lib/teamInboxes'
import { withTimeout } from '../lib/timeout'

export interface TeamInboxes {
  /** One entry per team, in `inboxTeams` order. Empty before the first answer. */
  inboxes: TeamInbox[]
  /** True until every team has answered (or failed) for the current identity and team set. */
  loading: boolean
  /** Re-read every team. */
  reload: () => void
}

/** One team's read: its whole-queue totals and the viewer's own slice, in one answer. */
async function readTeam(team: InboxTeam): Promise<TeamInbox> {
  try {
    const answer = await withTimeout(
      requestApi.queue(team.id, { mine: true, mine_limit: CLAIMED_ROWS }),
      `Loading ${team.name}`,
    )
    // Rows are kept to *name* an id in `mine.ids` — never to count anything. The
    // page is newest-first, so a recent claim or assignment resolves here; an
    // older one may not, and `unlistedCount` says so rather than dropping it.
    return { team, totals: answer.totals, mine: answer.mine, rows: new Map(answer.items.map(r => [r.id, r])) }
  } catch (e) {
    return { team, rows: new Map(), error: e instanceof Error ? e.message : 'Failed to load' }
  }
}

/** One read's answers, with the key (identity + team set) they belong to. */
interface Answer {
  key: string
  inboxes: TeamInbox[]
}

/**
 * Read every team's inbox summary for this viewer.
 *
 * The reads re-run when the identity or the team set changes, and only then: a
 * re-render with the same teams must not re-issue them.
 */
export function useTeamInboxes(me: Me | null, loading: boolean): TeamInboxes {
  const [answer, setAnswer] = useState<Answer | null>(null)
  const [attempt, setAttempt] = useState(0)

  // `me` is the auth context's own state object, so its identity is stable
  // between renders; the memo is what keeps the effect from re-reading.
  const teams = useMemo(() => (loading ? [] : inboxTeams(me)), [me, loading])

  // The identity and team set a read would be for. `me.user_id` is in the key
  // because the slice is the caller's: the same teams under a different sign-in
  // are a different answer, and showing the previous one would be the worst
  // kind of stale number.
  const key = useMemo(
    () => (teams.length === 0 ? '' : `${me?.user_id ?? ''}|${teams.map(t => t.id).join(',')}`),
    [me, teams],
  )

  useEffect(() => {
    if (!key) return
    let live = true
    Promise.all(teams.map(readTeam)).then(inboxes => {
      if (live) setAnswer({ key, inboxes })
    })
    return () => { live = false }
  }, [key, teams, attempt])

  const reload = useCallback(() => setAttempt(n => n + 1), [])

  // An answer that is not for the current key is not shown at all: the panels
  // get an empty list and a loading flag rather than a stale count.
  const fresh = answer !== null && answer.key === key
  return {
    inboxes: fresh ? answer.inboxes : [],
    loading: !!key && !fresh,
    reload,
  }
}
