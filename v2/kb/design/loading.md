---
title: Loading — the tiered contract (AMP's web UI)
tags: [design, ui, ux, loading, skeleton, performance, accessibility, amp-ui]
---

# Loading: three tiers

How AMP's own web UI waits. One long wait, at cold boot. Everything after that is soft: the
page's frame is there at once, its content fills in, and data already on screen stays on screen
while it updates. Use this for any screen you add to AMP, and as a worked example of the
"Loading" pattern in `design/ux-patterns.md` for products you design.

The code: `ui/src/components/brand/LaunchSequence.tsx` and `ui/src/lib/launch.ts` (tier 0),
`ui/src/components/ui/Skeleton.tsx`, `ui/src/components/brand/BandLoader.tsx` and
`ui/src/hooks/useDelayedFlag.ts` (tier 1), `ui/src/components/ui/ProgressBand.tsx` (tier 2),
`ui/src/lib/boardStore.ts` and `ui/src/lib/projectCache.ts` (shared data).

## The tiers

| Tier | When | What shows | Timing | Component |
|---|---|---|---|---|
| 0 | Cold boot and sign-in only: the app opened at `/` in a new tab | The launch: full screen, branded, the only long wait. Never on a route change, a refresh or a deep link | Short: 600 ms floor, ends as soon as `/api/me` answers, 2 s at most. Full: 5 s, only right after the sign-in redirect; it waits for `/api/me` up to 4 s more (9 s at most) | `LaunchSequence` |
| 1 | A route's first load: nothing of its data is on screen yet | The layout shell at once (sidebar, the page header with its tabs and actions). The content area draws skeletons in the shape of the content | Nothing for the first 250 ms (a fast load shows nothing at all); once shown, at least 400 ms | `Skeleton`, `SkeletonText`, `SkeletonCard`, `SkeletonReadouts`, `SkeletonSection` inside a `SkeletonRegion`, gated by `useDelayedFlag(flag, 250, 400)` |
| 1, shape unknown | A wait whose result has no known layout: a redirect hop (`/t/<ref>`, `/r/<id>`, `/project/:id` on a cold start), the Graph canvas, a remote session connecting | The band loader, with a caption, inside the page's area | Waits 250 ms (its `delay`), then at least 400 ms | `BandLoader` |
| 2 | A refetch, a mutation's refresh, a live (SSE) resync, a revisit with cached data | The stale content stays mounted and usable. A 2px band under the header and a spinning Refresh icon, on pages that have a Refresh. No layout change | Nothing for the first 250 ms; once shown, at least 400 ms | `ProgressBand`, `RefreshCw` with `animate-spin`, `aria-busy` on Refresh |
| Action | A button's own action (Save, Approve, Dispatch) | The button's `Loader2` spinner, the button disabled | At once | `Loader2` in the button |

Rules that hold across tiers:

- **One loading screen per click.** A click never chains two different loaders. `/project/:id`
  hops to its tab on the first render when the "waiting on you" counts are already in; the tab
  then shows its own tier 1.
- **No layout jump.** A skeleton occupies the columns and sections the content will. The
  progress band always reserves its 2px. The sidebar's Project group is drawn from the URL at
  once, with skeleton bars for the name and code until they're known.
- **Never the word "Loading…" as a title.** Use a title-width `Skeleton` bar (ProjectNav's title
  skeleton is in #168 (quick wins)).
- **The band loader is not a page loader.** It's for waits whose shape is unknown (above), and
  tier 0 is its branded big brother. A page that knows its layout uses skeletons.

## Reduced motion

Under `prefers-reduced-motion: reduce`:

- Tier 0 plays only the logo fading in (700 ms).
- Skeletons are a static fill, no sweep.
- The band loader holds a static lit band strip; its caption carries the wait.
- The progress band stays put (no run).

## Accessibility

- One live region per waiting area: `SkeletonRegion` is `role="status"`, `aria-busy="true"`,
  with an `aria-label` and a screen-reader-only label ("Loading the board"). The skeleton shapes
  inside it are `aria-hidden`. Sighted users see the shapes; screen readers hear the label once.
- The band loader is `role="status"` with its caption as the label; before its delay it's an
  empty status region, so assistive tech still knows a wait began.
- Tier 2 marks the Refresh button `aria-busy` while it runs; the content keeps its roles.
- The launch is `aria-hidden` and skippable with any key or click.

## Errors

- Inline, where the content is, never a blank page: `LoadError` with the message and a Retry.
- A refetch that fails keeps the stale data on screen and says so beside it (Board: "Couldn't
  refresh the board: …. Showing what was loaded.", with Retry).
- Every page load has an end: `withTimeout` (12 s) turns a load that never answers into an
  error with Retry.
- A link to something that doesn't exist or isn't visible says so (`/t/999999`: "Ticket 999999
  was not found, or you can't see it"), never an empty board.

## Shared data (why most revisits show no loader)

- **One store per project** (`lib/boardStore.ts`): Mission, Board, Flight plan, Graph and
  Report read the project's epics, stories and tickets from it. A view renders cached data on
  its first frame. It re-reads in the background (tier 2) when the cache is older than 15 s;
  Refresh and mutations always re-read. Concurrent reads share one request set, and only the
  newest may land.
- **Live events patch the store**, not a component: a change seen on the Board is there when
  Mission opens. The store listens while any view of the project is mounted, and 30 s after.
  A change that arrives while a load is in flight is replayed on that load's answer, so a slow
  load never undoes it. The server's re-read signals (a `task.updated` that isn't a ticket: a
  ticket deleted, tickets restored from Trash, a handover) remove the ticket or re-read the
  project, and pages outside the board views that change it (Trash) call `invalidateBoard`.
  A load answered with 403 or 404 drops the cached project instead of showing it.
- **The ticket drawer** reads the same store, so opening a ticket on a board view costs no
  request; opening another ticket in the same drawer re-reads the project once.
- **Three requests per board load**, whatever the epic count: epics, tickets and
  `GET /api/projects/:id/stories`, in parallel (an older API without the stories route falls
  back to one request per epic).
- **Projects** (`lib/projectCache.ts`): one store by id, primed by the sidebar's project list,
  so the sidebar's Project group and the drawer's ticket ref render without their own request.
- **Waiting-on-you counts** (`lib/waitCounts.ts`): the sidebar, project tabs, My work and
  `/project/:id` read one shared store.

## Decisions (#132)

- **Tier 0, short:** ends as soon as `/api/me` answers, with a 600 ms floor so it reads as a
  beat, not a flash; it waits for the answer up to 2 s in all. It used to run a fixed 2 s.
- **Tier 0, full (5 s):** only right after the sign-in redirect (or `?launch=full` for demos).
  Like the short one it waits for `/api/me` at its API line, up to 4 s more, so 9 s at most.
  The "first visit in 12 hours" rule is gone: a new tab at `/` gets the short launch. It still
  plays at most once per tab, and only at `/` (refreshes and deep links skip it).
- **Tier 2 indicator:** the 2px band under the header is the global tier-2 indicator, with the
  spinning Refresh icon where a page has Refresh. A background revalidation on a page without
  Refresh shows nothing: the data on screen is already current through live events.
- **Thresholds:** one delay, 250 ms, for tier 1 and tier 2 (`INDICATOR_DELAY_MS`), and a 400 ms
  minimum once shown. A second constant (300 ms for tier 2) bought nothing a person can see.
- **Cache freshness:** a view mount re-reads project data older than 15 s; live events keep it
  current in between; Refresh always re-reads.
- **No new dependency:** the shared store extends the in-house `useSyncExternalStore` pattern
  (`waitCounts.ts`) instead of adopting `@tanstack/react-query`.

`LAUNCH_MS` in `ui/src/lib/launch.ts` holds these timings; `lib/launch.test.ts` pins them.

## Every page-level loading site

Target tier, and where it is today. "Follow-up" means it still uses the older loader and should
move to its target the next time the page is touched.

| Page (`ui/src/pages`) | Waits on | Target | Today |
|---|---|---|---|
| `Board.tsx` | the project's board | 1 skeleton (ProjectNav + one column per state, 3 cards each); 2 on refetch | done |
| `Mission.tsx` | the project's board | 1 skeleton (6 readouts + 5 epic rows); 2 on revisit | done |
| `FlightPlan.tsx` | the project's board | 1 skeleton (5 readouts + epic bars with story rows); 2 on revisit | done |
| `DAG.tsx` | the project's board, laid out | 1, shape unknown: band loader inside the canvas, header and toolbar drawn | done |
| `MyWork.tsx` | your waiting items | 1 skeleton (section headers, 3 rows each) | done |
| `ProjectMyWork.tsx` | the project's waiting items | 1 skeleton (section headers, 3 rows each) | done |
| `ProjectLanding.tsx` | the waiting counts (cold start only) | none when counts are cached; else 1, shape unknown | in #168 (quick wins) (hops from cached counts) |
| `TicketRedirect.tsx` (`/t/`, `/r/`) | the ticket or run | 1, shape unknown | done |
| `Report.tsx` | the project report | 1 skeleton (readouts + charts) | follow-up: band loader under the header (its range in the URL: quick-wins PR) |
| `Portfolio.tsx` | the org portfolio | 1 skeleton (readouts + table rows) | follow-up: band loader under the header |
| `KB.tsx` | KB info, the doc list, a doc | 1 skeleton (tree rows; doc title and paragraphs) | follow-up: band loader and spinner |
| `ProjectSettings.tsx` | the project | 1 skeleton (form rows) | follow-up: band loader |
| `Projects.tsx` | the project list | 1 skeleton (project cards) | follow-up: band loader |
| `DayReports.tsx` | people, your draft, saved reports | 1 skeleton (people list, report sections) | follow-up: spinners |
| `Sessions.tsx` | remote sessions | 1 skeleton (session rows); no "Loading…" title | follow-up: band loader, "Loading…" panel title |
| `Session.tsx` | the remote session (standalone page) | 1, shape unknown | done |
| `Artifacts.tsx`, `ArtifactView.tsx` | artifacts, a version | 1 skeleton (list rows; document) | follow-up: spinners |
| `Users.tsx`, `Orgs.tsx`, `AdminAudit.tsx`, `Access.tsx` | admin lists | 1 skeleton (table rows) | follow-up: band loaders and spinners |
| `Trace.tsx` | a trace query; a story's timeline | 1 for the results; the timeline's inline spinner is fine (opened on demand) | results: the button spinner (action) |
| any page | a button's action | Action: `Loader2` in the button | done |
