/**
 * The team inbox queue (`/teams/:teamId/requests` in the routing ticket's
 * shape; this page takes the team id as a prop so it can be mounted without a
 * router edit — the ticket deliberately does not touch App.tsx).
 *
 * Mockup artifact 37 v1, view 1: the shared queue. The approved product
 * contract's rule that shapes everything here is that a request is **not** a
 * ticket — one shared queue per team, and claiming a request creates nothing for
 * anybody. So:
 *
 *  - The pills' counts are the **server's** `totals.states`, never a count of
 *    the rows on screen: the server computes the page and the counts from one
 *    predicate, so a pill cannot describe a different queue than the list under
 *    it.
 *  - Paging is the keyset cursor (`next_before_id`), not offsets: rows arrive at
 *    the top while a person reads, and an offset would shift the page under them.
 *  - Claim is the row's only primary action, first-write-wins, and a lost race
 *    is the 409 banner naming the winner — never a silent steal, and never an
 *    automatic expiry (D4).
 *
 * Three deliberate departures from the mockup, each because the surface behind
 * it does not exist yet or the API has no term for it. They are stated here and
 * in the completion note rather than left for a reader to notice:
 *
 *  1. **The page actions are not rendered.** The mockup's "New request" (J4),
 *     "Submit access" and "Invite a requester" (#3975's grant routes) have no
 *     route or page in this slice, and a dead button is worse than its absence.
 *  2. **The owner select is a view of the loaded page.** The queue API has no
 *     claim/assignee filter term — only `submitter`, `source_kind`, dates,
 *     `attached`, `q` and the `mine` id list — so filtering server-side would
 *     mean inventing a term. The select therefore filters what is loaded, and
 *     the footer says so, because a filter that silently claims to be a queue
 *     filter would let a person conclude "nobody has claimed anything" from one
 *     page.
 *  3. **No attachment or company chips.** `TeamRequest` carries no attachment
 *     count and no company field, so the mockup's "2 attachments" /
 *     "1 restricted attachment" / "Northwind" chips have nothing behind them.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Search, X } from 'lucide-react'
import { api, requestApi } from '../api/client'
import type { RequestQueue, RequestStatus, Team, TeamRequest } from '../types'
import { RequestRow } from '../components/requests/RequestRow'
import { LoadError } from '../components/ui/LoadError'
import { PageHeader } from '../components/ui/PageHeader'
import { Skeleton } from '../components/ui/Skeleton'
import { useDelayedFlag } from '../hooks/useDelayedFlag'
import { withTimeout } from '../lib/timeout'
import {
  CLAIM_NEVER_EXPIRES, REQUEST_OWNER_OPTIONS, REQUEST_QUEUE_PILLS, claimConflictNotice,
  matchesOwnerFilter, requestStateLabel, requestStateVar, teamPeople, type ClaimConflictNotice,
  type RequestOwnerFilter,
} from '../lib/requestState'
import type { Person } from '../lib/people'

interface Props {
  teamId: number
  /** The caller's user id. Decides "You", "Claimed by me" and Release. */
  meId?: number
  /** The team's members, for claim holders' and assignees' names. Defaults to one fetch of the team. */
  people?: Map<number, Person>
  /** Open a request. The routing ticket (J2) supplies the real navigation; this slice takes a callback. */
  onOpenRequest?: (req: TeamRequest) => void
}

/** The page's own state, one variable at a time. */
interface QueueState {
  queue: RequestQueue | null
  loading: boolean
  loadingMore: boolean
  error: string | null
}

const EMPTY: QueueState = { queue: null, loading: true, loadingMore: false, error: null }

/** How many rows a page asks for. The server's default is 50; this is the queue's own page size. */
const PAGE_SIZE = 25

export function TeamInbox({ teamId, meId, people: peopleProp, onOpenRequest }: Props) {
  const [state, setState] = useState<QueueState>(EMPTY)
  const [team, setTeam] = useState<Team | null>(null)
  const [members, setMembers] = useState<Map<number, Person>>(new Map())
  const [selected, setSelected] = useState<RequestStatus | null>(null)
  const [owner, setOwner] = useState<RequestOwnerFilter>('everyone')
  const [query, setQuery] = useState('')
  const [attempt, setAttempt] = useState(0)
  const [claiming, setClaiming] = useState<number | null>(null)
  const [conflict, setConflict] = useState<ClaimConflictNotice | null>(null)

  // Every load bumps this; a late answer from an older load is ignored, so a
  // filter change during a slow request cannot overwrite the newer result.
  const loadSeq = useRef(0)

  const people = peopleProp ?? members

  // The team's own name for the header, and its members for names. Both are
  // best-effort and loaded once: a missing team name must not take the queue
  // down with it, and names fall back to "User #id".
  useEffect(() => {
    let live = true
    withTimeout(api.getTeam(teamId), 'Loading the team')
      .then(r => { if (live) { setTeam(r.team); setMembers(teamPeople(r.members)) } })
      .catch(() => { /* the queue's own error is the one worth showing */ })
    return () => { live = false }
  }, [teamId])

  const load = useCallback((opts: { beforeId?: number; append?: boolean } = {}) => {
    const seq = ++loadSeq.current
    const filter = {
      ...(selected ? { state: [selected] } : {}),
      ...(query.trim() ? { q: query.trim() } : {}),
      limit: PAGE_SIZE,
      ...(opts.beforeId ? { before_id: opts.beforeId } : {}),
    }
    setState(s => ({ ...s, loading: !opts.append, loadingMore: !!opts.append, error: null }))
    withTimeout(requestApi.queue(teamId, filter), 'Loading the inbox')
      .then(answer => {
        if (seq !== loadSeq.current) return
        setState(s => ({
          queue: opts.append && s.queue
            // Appending keeps the first page's totals: they describe the filter,
            // and every page of one filter answers the same counts.
            ? { ...answer, items: [...s.queue.items, ...answer.items] }
            : answer,
          loading: false,
          loadingMore: false,
          error: null,
        }))
      })
      .catch(e => {
        if (seq !== loadSeq.current) return
        setState(s => ({
          ...s,
          loading: false,
          loadingMore: false,
          error: e instanceof Error ? e.message : 'The request list did not come back.',
        }))
      })
  }, [teamId, selected, query])

  // The first load, and every change of a server-side filter. The query is
  // debounced so typing does not spend a request per keystroke.
  useEffect(() => {
    const t = setTimeout(() => load(), query ? 250 : 0)
    return () => clearTimeout(t)
  }, [load, attempt, query])

  const retry = () => { setState(EMPTY); setAttempt(n => n + 1) }

  const claim = useCallback((req: TeamRequest) => {
    setConflict(null)
    setClaiming(req.id)
    requestApi.claim(teamId, req.id, { expected_rev: req.rev })
      .then(() => {
        // A claim's write answer carries no claim field, so the row is re-read —
        // the queue's own read, so counts and rows stay one answer.
        load()
      })
      .catch(e => {
        // A lost race is the 409 banner, not a toast: it names the winner and
        // says nothing was sent. Anything else is the queue's error line.
        //
        // Only a conflict re-reads the queue. A conflict means the row is stale
        // (someone else got there first), so the rows must be refreshed; any
        // other failure wrote nothing, and re-reading would clear the error
        // line before it could be read — the message would flash and vanish.
        const notice = claimConflictNotice(e, people, req.rev)
        if (notice) {
          setConflict(notice)
          load()
        } else {
          setState(s => ({ ...s, error: e instanceof Error ? e.message : 'The claim did not go through.' }))
        }
      })
      .finally(() => setClaiming(null))
  }, [teamId, load, people])

  const release = useCallback((req: TeamRequest) => {
    setClaiming(req.id)
    requestApi.release(teamId, req.id, { expected_rev: req.rev })
      .then(() => load())
      .catch(e => setState(s => ({ ...s, error: e instanceof Error ? e.message : 'The release did not go through.' })))
      .finally(() => setClaiming(null))
  }, [teamId, load])

  const rows = useMemo(
    () => (state.queue?.items ?? []).filter(r => matchesOwnerFilter(r, owner, meId)),
    [state.queue, owner, meId],
  )

  const totals = state.queue?.totals
  const filteredByOwner = owner !== 'everyone'
  const hasFilters = selected != null || query.trim() !== '' || filteredByOwner
  const showSkeleton = useDelayedFlag(state.loading)

  const clearFilters = () => { setSelected(null); setQuery(''); setOwner('everyone') }

  return (
    <div className="flex flex-col h-full min-h-0" style={{ background: 'var(--bg)' }}>
      <PageHeader
        crumb={team ? `Teams / ${team.name}` : 'Teams'}
        title="Requests"
        meta="What customers and API clients asked this team for. One shared queue — claiming a request never creates a ticket for anyone."
      />

      <div className="flex-1 min-h-0 overflow-y-auto px-5 py-5 flex flex-col gap-4">
        {/* The 409 claim conflict: the winner, the revision, and that nothing was sent. */}
        {conflict && (
          <div role="alert" className="flex items-start gap-3 px-4 py-3 border text-[13px]"
            style={{ borderColor: 'var(--danger)', background: 'color-mix(in srgb, var(--danger) 7%, var(--surface))' }}>
            <div className="min-w-0">
              <span className="font-semibold text-ink block">{conflict.title}</span>
              <span className="text-ink-2">{conflict.body}</span>
              <div className="mt-2 flex gap-2">
                <button type="button" className="amp-btn amp-btn-sm amp-btn-primary" onClick={() => { setConflict(null); load() }}>
                  Refresh
                </button>
                <button type="button" className="amp-btn amp-btn-sm" onClick={() => setConflict(null)}>
                  Dismiss
                </button>
              </div>
            </div>
            <button type="button" onClick={() => setConflict(null)} className="ml-auto text-ink-3 hover:text-ink cursor-pointer" aria-label="Dismiss the conflict notice">
              <X size={14} aria-hidden="true" />
            </button>
          </div>
        )}

        {state.error && <LoadError message={state.error} onRetry={retry} />}

        {/* Filters: search, the state pills with server counts, and the owner view. */}
        <div className="flex items-center gap-2.5 flex-wrap">
          <div className="relative flex-1 min-w-[220px]">
            <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-ink-3 pointer-events-none" />
            <input
              type="text"
              value={query}
              onChange={e => setQuery(e.target.value)}
              placeholder="Search requests…"
              aria-label="Search requests"
              className="w-full bg-surface border border-line pl-9 pr-8 py-2 text-sm text-ink placeholder-ink-3 focus:outline-none focus:border-ink"
              style={{ borderRadius: 'var(--radius)' }}
            />
            {query && (
              <button type="button" onClick={() => setQuery('')} aria-label="Clear the search"
                className="absolute right-2.5 top-1/2 -translate-y-1/2 text-ink-3 hover:text-ink cursor-pointer">
                <X size={12} aria-hidden="true" />
              </button>
            )}
          </div>

          {/* The pill strip scrolls rather than wrapping or being truncated: nine
              lifecycle states plus All are wider than a phone, and the mockup's
              390 px rule is a horizontal scroller, not a second row of pills. */}
          <div role="group" aria-label="Filter by state"
            className="flex items-center gap-0.5 p-1 overflow-x-auto max-w-full shrink-0 bg-surface border border-line"
            style={{ borderRadius: 'var(--radius)' }}>
            {REQUEST_QUEUE_PILLS.map(pill => {
              const active = selected === pill.state
              const count = pill.state == null ? totals?.total : totals?.states[pill.state]
              return (
                <button
                  key={pill.state ?? 'all'}
                  type="button"
                  onClick={() => setSelected(pill.state)}
                  aria-pressed={active}
                  className={`flex items-center gap-1.5 px-2.5 py-1 text-xs font-medium whitespace-nowrap cursor-pointer transition-colors ${
                    active ? 'bg-ink text-on-ink' : 'text-ink-2 hover:text-ink hover:bg-surface-2'
                  }`}
                  style={{ borderRadius: 'var(--radius)' }}
                >
                  {pill.state && (
                    <span className="w-2 h-2 flex-shrink-0"
                      style={{ background: requestStateVar(pill.state), borderRadius: 1, opacity: active ? 1 : 0.6 }}
                      aria-hidden="true" />
                  )}
                  {pill.label}
                  {count != null && (
                    <span className={`tabular-nums ${active ? 'text-ink-2' : 'text-ink-3'}`}>{count}</span>
                  )}
                </button>
              )
            })}
          </div>

          <select
            value={owner}
            onChange={e => setOwner(e.target.value as RequestOwnerFilter)}
            aria-label="Whose requests to show"
            title="A view of the requests loaded so far — the queue API has no claim or assignee filter"
            className="bg-surface border border-line px-3 py-2 text-sm text-ink focus:outline-none focus:border-ink appearance-none cursor-pointer shrink-0"
            style={{ borderRadius: 'var(--radius)' }}
          >
            {REQUEST_OWNER_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>

          {hasFilters && (
            <button type="button" onClick={clearFilters}
              className="flex items-center gap-1.5 px-3 py-2 text-xs font-medium text-ink-2 hover:text-ink border border-line hover:border-line-str cursor-pointer"
              style={{ borderRadius: 'var(--radius)' }}>
              <X size={11} aria-hidden="true" />Clear
            </button>
          )}
        </div>

        {/* Loading: skeletons shaped like rows, only after the delay. */}
        {state.loading && showSkeleton && (
          <div className="flex flex-col gap-2" aria-label="Loading requests" aria-busy="true">
            {[0, 1, 2, 3].map(i => (
              <div key={i} className="flex items-center gap-2.5 bg-surface border border-line px-3 py-3">
                <Skeleton w={3} h={34} />
                <Skeleton w={64} h={11} />
                <Skeleton w={i % 2 ? '35%' : '48%'} h={13} />
                <Skeleton w={90} h={11} className="ml-auto" />
              </div>
            ))}
          </div>
        )}

        {/* The queue itself. */}
        {!state.loading && state.queue && rows.length > 0 && (
          <div role="list" aria-label="Requests" className="flex flex-col gap-2">
            {rows.map(req => (
              <RequestRow
                key={req.id}
                request={req}
                meId={meId}
                people={people}
                claiming={claiming === req.id}
                onClaim={claim}
                onRelease={release}
                onOpen={req => onOpenRequest?.(req)}
              />
            ))}
          </div>
        )}

        {/* Empty: two different empties, and they must not be confused. */}
        {!state.loading && state.queue && rows.length === 0 && (
          <div className="amp-panel px-7 py-9 flex flex-col items-start gap-2.5">
            {hasFilters ? (
              <>
                <span className="amp-label">No matches</span>
                <h2 className="m-0 text-[18px] font-semibold text-ink max-w-[46ch]">
                  Nothing in this inbox matches what you filtered for.
                </h2>
                <p className="m-0 text-[13.5px] text-ink-2 max-w-[62ch] leading-relaxed">
                  {filteredByOwner
                    ? 'The owner view applies to the requests loaded so far, not to the whole queue. Clear it to see everything again.'
                    : 'The queue is not empty — your filters exclude everything in it. Clear them to see it again.'}
                </p>
                <button type="button" className="amp-btn amp-btn-sm amp-btn-primary mt-1" onClick={clearFilters}>
                  Clear filters
                </button>
              </>
            ) : (
              <>
                <span className="amp-label">Team inbox</span>
                <h2 className="m-0 text-[20px] font-semibold text-ink max-w-[34ch]">
                  No requests yet. When someone asks {team?.name ?? 'this team'} for something, it lands here.
                </h2>
                <p className="m-0 text-[13.5px] text-ink-2 max-w-[62ch] leading-relaxed">
                  Requests come from invited customers on the web, or from MCP and API clients with a submit
                  grant on this team. Nothing here can approve a plan or start work — triage decides what
                  becomes planned tickets, and a person still approves each one.
                </p>
              </>
            )}
          </div>
        )}

        {/* Paging and the claim rule, in one quiet line under the list. */}
        {state.queue && (
          <div className="flex items-center gap-3 flex-wrap text-[12px] text-ink-3">
            {totals && (
              <span>
                {totals.total} request{totals.total === 1 ? '' : 's'}
                {selected && ` in ${requestStateLabel(selected)}`}
              </span>
            )}
            {filteredByOwner && (
              <span className="text-ink-2">
                showing {rows.length} of {state.queue.items.length} loaded — the owner view filters what is loaded, not the queue
              </span>
            )}
            <span className="hidden sm:inline">{CLAIM_NEVER_EXPIRES}</span>
            {state.queue.next_before_id != null && (
              <button
                type="button"
                onClick={() => load({ beforeId: state.queue!.next_before_id, append: true })}
                disabled={state.loadingMore}
                className="amp-btn amp-btn-sm ml-auto"
              >
                {state.loadingMore ? 'Loading…' : 'Older requests ›'}
              </button>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
