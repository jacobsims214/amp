/**
 * The requester's "Your requests" screen (AMP #3986, build slice J5 of
 * `discoveries/team-inbox-approved-build-slices.md`): every request this person
 * has sent to a team's inbox, with where each one has got to.
 *
 * Built to the approved requester mockup (artifact 38 v1, the "Your requests"
 * screen and its three states: list, empty, loading). It composes #3981's
 * `RequesterChrome` — a customer frame with no room for project internals — and
 * owns no routing: the page takes its links as callbacks so #3988 can mount it
 * at whatever URL it wires.
 *
 * ## What this page reads, and what it therefore cannot show
 *
 * Every row comes from `RequesterRequestView`, the server's whitelist
 * projection, read from `GET /api/requests`. That type has no field for a
 * project, epic, story or ticket, a branch or merge state, a metadata key, a
 * user id, an agent identity, a claim, an assignee, a revision or an audit row
 * (see `architecture/requester-request-projection.md`) — so a row here shows the
 * reference, the subject, the server's state word and dates, and nothing else
 * can appear however the fixture is shaped. A test proves it by feeding the page
 * an object carrying those internal fields and asserting none renders.
 *
 * ## The four decisions the page makes, and why
 *
 * 1. **"Own" is the server's word, not a client filter.** `GET /api/requests`
 *    scopes to the caller's requester column in SQL and has no filter that could
 *    widen it (`architecture/request-read-service.md`), so this page cannot list
 *    somebody else's request even if a URL were wrong. It never filters, joins or
 *    de-duplicates rows itself.
 * 2. **The pills are server filters.** The own list accepts `state`, so a pill
 *    narrows the *query* — the rows under a pill and its count describe the same
 *    set while paging, which a client-side filter over one loaded page could not.
 *    Their counts are the server's `total` for each filter (four one-row reads;
 *    see `loadCounts`), never a count of the rows on screen.
 * 3. **Paging is the keyset cursor.** `next_before_id` is the id of the last row
 *    of the page just read; asking with it returns the rows below. Rows land at
 *    the top while somebody reads, and an offset would shift the page under them.
 * 4. **One empty state is not the other.** "You have never sent anything" is an
 *    invitation to send one; "this filter has nothing in it" is a filter to
 *    clear. They never share a sentence.
 *
 * ## What it deliberately does not do
 *
 * - **No team fetch.** `teamName` is a prop, not a read: a requester has no org
 *   standing, so `GET /api/teams/:id` answers them the surface's 404 — and a page
 *   that could fetch a team could render one.
 * - **No accept, answer or reopen.** Those are the status screen's (#3987), one
 *   request at a time. A list row's only act is opening the request, addressed by
 *   its public reference — the one address a requester holds.
 * - **No "Save draft", no attachment chips, no company column.** The own-list
 *   row carries none of the data behind them, and a chip with nothing behind it is
 *   a promise the product cannot keep.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ChevronRight, Inbox, X } from 'lucide-react'
import { requestApi } from '../api/client'
import type { OwnRequestPage, RequesterRequestView } from '../types'
import { RequesterChrome } from '../components/requests/RequesterChrome'
import { LoadError } from '../components/ui/LoadError'
import { Skeleton } from '../components/ui/Skeleton'
import { INDICATOR_DELAY_MS, useDelayedFlag } from '../hooks/useDelayedFlag'
import { requesterNavItemClass, type RequesterIdentity } from '../lib/requesterChrome'
import {
  REQUESTER_LIST_PILLS, REQUESTER_PAGE_SIZE, allPillCount, pillCountKeys, requesterEmpty, requesterMetaLine,
  requesterMetaTitle, requesterNoMatchesHeading, requesterPillFilter, requesterRowLine,
  requesterStateHint, requesterStateVar, requesterStateWord, requesterTotalLabel, type RequesterListPill,
} from '../lib/requesterRequests'
import { withTimeout } from '../lib/timeout'

export interface RequesterRequestsProps {
  /**
   * The desk's public display name ("Northwind support desk"). Passed in, not
   * fetched: a customer has no org standing, so the team route answers them 404,
   * and a page that cannot read a team cannot render one.
   */
  teamName: string
  /** The signed-in requester, as `/api/me` answers. */
  me: RequesterIdentity
  /** The requester's own two tabs, as the page's own links (#3988 owns routing). */
  nav?: { label: string; current: boolean; onClick?: () => void }[]
  /** An access notice under the frame's rule (a grant removed, say). */
  banner?: React.ReactNode
  onSignOut?: () => void
  /**
   * Open one request, by its public reference — the only address a requester
   * holds, and the one the status screen resolves. Without it the rows render as
   * plain text rather than as controls that do nothing.
   */
  onOpenRequest?: (publicRef: string) => void
  /** Start a new request. Without it, no such button. */
  onNewRequest?: () => void
}

/** What the page knows. One variable per fact, so two of them cannot disagree. */
interface ListState {
  page: OwnRequestPage | null
  loading: boolean
  loadingMore: boolean
  error: string | null
}

const EMPTY: ListState = { page: null, loading: true, loadingMore: false, error: null }

export function RequesterRequests({
  teamName, me, nav, banner, onSignOut, onOpenRequest, onNewRequest,
}: RequesterRequestsProps) {
  const [state, setState] = useState<ListState>(EMPTY)
  const [pill, setPill] = useState<RequesterListPill>('all')
  const [counts, setCounts] = useState<Partial<Record<RequesterListPill, number>>>({})
  const [attempt, setAttempt] = useState(0)

  // Every load bumps this. A late answer from an older load (a pill the person
  // has already left) is dropped rather than painted over the newer result.
  const loadSeq = useRef(0)

  /**
   * The one place the page reads requests. `beforeId` pages back; without it the
   * load starts at the first page.
   */
  const load = useCallback((opts: { beforeId?: number; append?: boolean } = {}) => {
    const seq = ++loadSeq.current
    const filter = {
      ...requesterPillFilter(pill),
      limit: REQUESTER_PAGE_SIZE,
      ...(opts.beforeId ? { before_id: opts.beforeId } : {}),
    }
    setState(s => ({ ...s, loading: !opts.append, loadingMore: !!opts.append, error: null }))
    withTimeout(requestApi.own(filter), 'Loading your requests')
      .then(answer => {
        if (seq !== loadSeq.current) return
        setState(s => ({
          page: opts.append && s.page
            // Appending keeps the first page's rows: the total describes the
            // filter, and every page of one filter answers the same total.
            ? { ...answer, items: [...s.page.items, ...answer.items] }
            : answer,
          loading: false,
          loadingMore: false,
          error: null,
        }))
        // The active pill's count is this answer's total — the server's own
        // number for exactly this filter, so the pill and the rows agree.
        setCounts(c => ({ ...c, [pill]: answer.total }))
      })
      .catch(e => {
        if (seq !== loadSeq.current) return
        setState(s => ({
          ...s,
          loading: false,
          loadingMore: false,
          error: e instanceof Error ? e.message : 'Your requests did not come back.',
        }))
      })
  }, [pill])

  /**
   * The four counts the mockup shows beside the pills other than `All`, as the
   * server's own `total` for each filter.
   *
   * The own-list route has no per-state totals field (the *queue* does), so each
   * count is a one-row read whose page is thrown away — the same shape as the
   * team queue's `summary` route, which is `ListRequests` with the page
   * discarded. Four small reads on mount, and best-effort: a count that fails
   * leaves its pill without a number rather than showing a fabricated `0`, and
   * the list itself never waits for them.
   */
  const loadCounts = useCallback(() => {
    for (const key of pillCountKeys()) {
      const filter = requesterPillFilter(key)
      requestApi.own({ ...filter, limit: 1 })
        .then(answer => setCounts(c => ({ ...c, [key]: answer.total })))
        .catch(() => { /* the pill simply shows no count */ })
    }
  }, [])

  useEffect(() => { loadCounts() }, [loadCounts])

  // The first load, and every change of the server-side filter. Deferred by a
  // tick (the TeamInbox convention) so the effect never sets state synchronously
  // — and so a pill pressed twice in one frame costs one read, not two.
  useEffect(() => {
    const t = setTimeout(() => load(), 0)
    return () => clearTimeout(t)
  }, [load, attempt])

  const retry = () => { setState(EMPTY); setAttempt(n => n + 1); loadCounts() }

  const rows = state.page?.items ?? []
  const total = state.page?.total ?? 0
  const empty = state.page ? requesterEmpty(pill, state.page.total) : 'none-yet'
  const showSkeleton = useDelayedFlag(state.loading, INDICATOR_DELAY_MS)

  const navNode = useMemo(() => (nav && nav.length > 0
    ? nav.map(item => (
      <button
        key={item.label}
        type="button"
        onClick={item.onClick}
        aria-current={item.current ? 'page' : undefined}
        className={requesterNavItemClass(item.current)}
      >
        {item.label}
      </button>
    ))
    : undefined), [nav])

  const all = allPillCount(counts)
  const countFor = (key: RequesterListPill): number | undefined => (key === 'all' ? all : counts[key])

  return (
    <RequesterChrome teamName={teamName} me={me} nav={navNode} banner={banner} onSignOut={onSignOut}>
      <div className="mx-auto w-full">
        {/* The heading block: what this list is, and the way to add to it. */}
        <div className="flex flex-wrap items-baseline gap-x-3.5 gap-y-2">
          <h1 className="m-0 text-[24px] font-semibold tracking-[-0.01em] text-ink">Your requests</h1>
          <span className="text-[12.5px] text-ink-3">to {teamName}</span>
          <span className="flex-1" />
          {onNewRequest && (
            <button
              type="button"
              onClick={onNewRequest}
              className="amp-btn amp-btn-primary w-full sm:w-auto"
              style={{ minHeight: 44 }}
            >
              New request
            </button>
          )}
        </div>
        <p className="mb-5 mt-2 max-w-[62ch] text-[13.5px] leading-relaxed text-ink-2">
          Everything you've sent from {me.email}. You see only your own requests — never anyone else's, and never the
          desk's internal planning.
        </p>

        {/* Filters. A server filter, not a view of the loaded page. On the very
            first load the pills are still placeholders (the mockup draws them as
            skeletons), so the real strip waits for an answer; on a later filter
            change they stay, because they are the control the person is using. */}
        {(!state.loading || state.page != null) && (
          <div
            role="group"
            aria-label="Filter requests"
            className="mb-3.5 flex max-w-full items-center gap-1.5 overflow-x-auto"
          >
            {REQUESTER_LIST_PILLS.map(p => {
              const active = pill === p.key
              const count = countFor(p.key)
              return (
                <button
                  key={p.key}
                  type="button"
                  onClick={() => setPill(p.key)}
                  aria-pressed={active}
                  title={p.title}
                  className={`inline-flex flex-shrink-0 cursor-pointer items-center gap-1.5 whitespace-nowrap border px-2.5 py-1.5 font-label text-[10.5px] font-semibold uppercase leading-none tracking-[0.12em] transition-colors ${
                    active ? 'border-ink bg-ink text-on-ink' : 'border-line bg-surface text-ink-2 hover:text-ink'
                  }`}
                  style={{ borderRadius: 999, minHeight: 32 }}
                >
                  {p.label}
                  {count != null && (
                    <span className="amp-num text-[10.5px] text-ink-3">{count}</span>
                  )}
                </button>
              )
            })}
          </div>
        )}

        {state.error && (
          <div className="mb-4">
            <LoadError
              message={`Your requests didn't load — ${state.error}`}
              onRetry={retry}
            />
          </div>
        )}

        {/* Loading: the mockup's own shape — pill placeholders and four rows — and
            only once the wait is long enough to be worth showing. */}
        {state.loading && showSkeleton && (
          <div role="status" aria-busy="true" aria-label="Loading your requests">
            <div className="mb-3.5 flex gap-1.5" aria-hidden="true">
              {[64, 130, 104, 92, 88].map((w, i) => (
                <span key={i} className="amp-skel block" style={{ width: w, height: 32, borderRadius: 999 }} />
              ))}
            </div>
            {[0, 1, 2, 3].map(i => (
              <div
                key={i}
                aria-hidden="true"
                className="mb-2 border border-line bg-surface px-3.5 py-3"
                style={{ borderRadius: 'var(--radius)' }}
              >
                <Skeleton w={i % 2 ? '42%' : '58%'} h={12} />
                <div className="mt-2.5"><Skeleton w="70%" h={9} /></div>
                <div className="mt-2"><Skeleton w={168} h={8} /></div>
              </div>
            ))}
            <p className="m-0 mt-3.5 text-center font-data text-[11.5px] text-ink-3">Loading your requests…</p>
          </div>
        )}

        {/* The list. One target per row: the request itself, by its reference. */}
        {!state.loading && rows.length > 0 && (
          <div role="list" aria-label="Your requests" className="flex flex-col gap-2">
            {rows.map(view => (
              <RequestCard key={view.public_ref} view={view} onOpen={onOpenRequest} />
            ))}
          </div>
        )}

        {/* Empty. "Nothing yet" and "nothing matches" are different news. */}
        {!state.loading && !state.error && state.page && rows.length === 0 && (
          empty === 'none-yet' ? (
            <div
              className="px-5 py-12 text-center"
              style={{ border: '1px dashed var(--line-str)', borderRadius: 'var(--radius)', background: 'var(--surface)' }}
            >
              <Inbox size={40} strokeWidth={1.5} className="mx-auto text-line-str" aria-hidden="true" />
              <h2 className="mb-1.5 mt-3.5 text-[17px] font-semibold text-ink">Nothing here yet</h2>
              <p className="mx-auto mb-4 mt-0 max-w-[44ch] text-[13px] leading-relaxed text-ink-2">
                Requests you send to {teamName} show up here, with their reference and where they've got to. Send one
                and it appears the moment it's received.
              </p>
              {onNewRequest && (
                <button type="button" onClick={onNewRequest} className="amp-btn amp-btn-primary" style={{ minHeight: 44 }}>
                  Send your first request
                </button>
              )}
            </div>
          ) : (
            <div
              className="flex flex-col items-start gap-2.5 px-5 py-7"
              style={{ border: '1px solid var(--line)', borderRadius: 'var(--radius)', background: 'var(--surface)' }}
            >
              <span className="amp-label">No matches</span>
              <h2 className="m-0 max-w-[46ch] text-[17px] font-semibold text-ink">
                {requesterNoMatchesHeading(pill)}
              </h2>
              <p className="m-0 max-w-[62ch] text-[13.5px] leading-relaxed text-ink-2">
                {pill === 'all'
                  ? 'The list came back without rows. Reload to try again — nothing has been changed by this.'
                  : 'Your requests are not gone — this filter excludes all of them. Clear it to see everything you\'ve sent.'}
              </p>
              {pill === 'all' ? (
                <button type="button" className="amp-btn amp-btn-sm amp-btn-primary mt-1" onClick={retry}>
                  <X size={11} aria-hidden="true" />Reload
                </button>
              ) : (
                <button type="button" className="amp-btn amp-btn-sm amp-btn-primary mt-1" onClick={() => setPill('all')}>
                  <X size={11} aria-hidden="true" />Clear the filter
                </button>
              )}
            </div>
          )
        )}

        {/* The footer: the server's total, and the keyset cursor. */}
        {!state.loading && !state.error && state.page && (
          <div className="mt-3 flex flex-wrap items-center gap-x-3 gap-y-2 text-[12px] text-ink-3">
            <span aria-live="polite">{requesterTotalLabel(total, pill)}</span>
            {rows.length > 0 && state.page.count < total && (
              <span className="text-ink-2">
                showing {rows.length} of {total}
              </span>
            )}
            {state.page.next_before_id != null && (
              <button
                type="button"
                onClick={() => load({ beforeId: state.page!.next_before_id, append: true })}
                disabled={state.loadingMore}
                className="amp-btn amp-btn-sm ml-auto"
                style={{ minHeight: 36 }}
              >
                {state.loadingMore ? 'Loading…' : 'Older requests'}
                {state.loadingMore ? null : <ChevronRight size={12} aria-hidden="true" />}
              </button>
            )}
          </div>
        )}
      </div>
    </RequesterChrome>
  )
}

/**
 * One request in the list.
 *
 * The row is a list item wrapping one target: the request itself, by its
 * reference (the mockup's `.reqrow` with `role="link"`). The target is a real
 * `<button>` and the list item is the wrapper around it, deliberately — a button
 * carrying `role="listitem"` would lose the button role, so a screen-reader user
 * would hear "list item" for the one control on the row and never learn it can
 * be activated. The row is one target and not a card with an inner link, so there
 * is nothing to nest and nothing to reach twice.
 *
 * When the page gives no `onOpenRequest` — a test, or a mount before routing
 * exists — the row is plain content rather than a control that does nothing.
 *
 * What it shows is the whole requester-visible row: the subject, one sentence on
 * where the request has got to, the reference and its dates, and the server's own
 * state word as the tag. There is deliberately no field for anything else a
 * request record holds.
 */
function RequestCard({ view, onOpen }: { view: RequesterRequestView; onOpen?: (publicRef: string) => void }) {
  const content = (
    <>
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="text-[13.5px] font-semibold text-ink">{view.subject}</span>
        <span className="text-[12.5px] leading-snug text-ink-2">{requesterRowLine(view)}</span>
        <span className="amp-num mt-0.5 text-[11.5px] text-ink-3" title={requesterMetaTitle(view)}>
          {requesterMetaLine(view)}
        </span>
      </span>
      <StateTag view={view} />
    </>
  )

  const box: React.CSSProperties = {
    background: 'var(--surface)',
    border: '1px solid var(--line)',
    borderLeft: `5px solid ${requesterStateVar(view.state)}`,
    borderRadius: 'var(--radius)',
    // The mockup dims a finished request; it stays readable, and it stays a
    // target — an accepted request is still the place the outcome lives.
    opacity: view.state === 'requester_accepted' ? 0.8 : 1,
  }
  const layout = 'flex w-full flex-col items-start gap-2 px-3.5 py-3 text-left sm:flex-row sm:items-center sm:gap-3'
  const label = `${view.subject}, ${requesterStateWord(view)}`

  return (
    <div role="listitem">
      {onOpen ? (
        <button
          type="button"
          onClick={() => onOpen(view.public_ref)}
          aria-label={label}
          className={`${layout} cursor-pointer hover:bg-surface-2`}
          style={box}
        >
          {content}
        </button>
      ) : (
        <div aria-label={label} className={layout} style={box}>
          {content}
        </div>
      )}
    </div>
  )
}

/** The server's state word as the mockup's tag, with the row's own tone. */
function StateTag({ view }: { view: RequesterRequestView }) {
  const tone = requesterStateVar(view.state)
  return (
    <span
      className="inline-flex flex-shrink-0 items-center gap-1.5 whitespace-nowrap border px-1.5 py-[4px] font-label text-[10px] font-semibold uppercase leading-none tracking-[0.14em]"
      title={requesterStateHint(view.state)}
      style={{
        borderRadius: 2,
        color: tone,
        borderColor: `color-mix(in srgb, ${tone} 40%, var(--surface))`,
        background: `color-mix(in srgb, ${tone} 7%, var(--surface))`,
      }}
    >
      <i className="block h-1.5 w-1.5 flex-shrink-0" style={{ background: tone, borderRadius: 999 }} aria-hidden="true" />
      {requesterStateWord(view)}
    </span>
  )
}