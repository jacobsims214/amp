# Owner Now page: status at a glance

Status: Draft · Date: 2026-11-05 · Issue: #127 (depends on #126 / PR #149 and #125 / PR #148; tracking #141) · Paths are relative to `v2/`.

## Problem

The owner says: "I'm struggling to see what this system is doing, what it needs from me, and what
it completed / was it verified." Each of the three answers exists somewhere, but no single page
shows all of them:

- **What it needs from me.** Mission shows only counts (`ui/src/pages/Mission.tsx:118-137`,
  `WaitingStrip`). The one-line ask (`needsLine`, `ui/src/lib/humanActions.ts:85`) is rendered
  only in My work. The project landing opens My work when anything waits, and Mission otherwise
  (`ui/src/lib/landing.ts`), so the owner switches tabs to get both.
- **What it is doing.** Mission's Crew rows show the owner, the agent, and time since dispatch or
  "quiet Nm" (`Mission.tsx:371-388`). Mission already loads three days of project activity,
  including comment bodies (`GET /api/projects/:id/activity` returns `detail`,
  `repository/repo.go:912`; a comment's body is stored as `detail`, `actor/task_actor.go:918`). But
  `fromActivity` drops the text (`Mission.tsx:178`), and only the timestamps are kept
  (`Mission.tsx:238-240`).
- **What got done, and was it verified.** Done is one readout, `done/total`
  (`Mission.tsx:318-319`). PR #149 adds "N unverified" to that readout's subtitle, but there is no
  list of the tickets and no way to act on them.
- **What is stuck.** A blocked ticket is shown as Waiting or Questions (`displayState`,
  `lib/ticketState.ts:36`), with no reason and no name of the person it waits on. `WaitingOn` only
  counts tickets whose owner is the person (`service/waiting.go:250`). A question on an unowned
  ticket therefore waits on nobody, and no page lists it.

**Fixed when:** on one page, without opening a ticket, the owner can see:

- every item waiting on them, as a verb-first ask with its unblock count and the action (inline
  where possible);
- every running ticket's latest step, with silent ones flagged;
- what finished in the last 7 days, split into verified and unverified;
- what is stuck on someone else or on nobody, and who or what it waits on.

Signal: the owner can clear a morning's asks from this page alone. My work and this page never
disagree about what waits on the owner.

## Goals / Non-goals

- Goal: a project **Now** page with five lanes: **Needs you**, **Running now**, **Blocked**,
  **Done but unverified**, **Done & verified**. Every open or recently finished ticket the page
  covers appears in exactly one lane.
- Goal: one **ask** rule per wait kind, shared by My work and Now. In Phase 1 it lives in the
  client (`needsLine`). In Phase 2 it moves to the server, as `WaitItem.ask`.
- Goal: one **ordering** rule, by unblock impact and then age, in every lane.
- Goal: inline actions where My work already has them, and the drawer for everything else.
- Goal: Running now shows each ticket's **last step**. This folds in the issue's Mission Crew
  quick-fix, and both pages share the helper.
- Goal: a stated, honest fallback for the Done lanes in projects where receipts are Off (the
  default, #126 D8) or where #126 hasn't merged.
- Non-goal in v1: a cross-project Now page (Q2), video and screenshot thumbnails, verified counts
  in day reports, and a server-side aggregate endpoint. These are Phases 2–3 and the Follow-ups.
- Non-goal: changing what waits on whom. Lane membership reads `WaitingOn`
  (`service/waiting.go:201`) and does not redefine it.
- Non-goal: new verification behaviour. The Done lanes only display #126's status.

## Journeys

Ticket numbers are illustrative.

1. **Morning check (receipts Inform).** The owner opens the project and Now loads.
   - Needs you leads with "Answer: HubSpot or Pipedrive? · blocks 4 · 2h". The answer chips and
     the text box are inline. The owner sends an answer, and the row leaves on the next refresh.
   - Next is "Merge PR #44 · blocks 3 · 5h", with Review PR ↗ and Mark merged.
   - Running now: "amp-worker-backend · #52 Wire webhook · running 34m", with "Running go test
     ./... · 2m ago" under it. #57 shows "quiet 22m" in the flag color.
   - Blocked: "Waits on Sam · Merge PR #61 · blocks 2", and "Waits on nobody · Answer: which
     region? · Take it".
   - Done but unverified: "#48 Add retry · Self-checked 2/2", with an Accept button. The owner
     clicks Accept, and #48 moves to Done & verified as "Accepted".
2. **A project with receipts Off.** Done & verified reads "Nothing verified here. Receipts are off
   in this project", with a link to Settings. Done but unverified lists every agent ticket
   completed in the last 7 days as "No receipt". Nothing errors, and no ticket claims to be
   verified.
3. **A silent agent.** #57 was dispatched 40 minutes ago, and its last comment is 22 minutes old.
   It sorts to the top of Running now with "quiet 22m". Its step line still shows that last
   comment, so the owner can see where it stopped. Opening it shows the drawer, as on Mission
   today.

## Design

### Overview

```
                    existing endpoints (Phase 1, no API change)                Now lanes
GET /api/my-work … /projects/:id/my-work ─ items (approve/answer/verify*/merge/start/do) ─▶ Needs you
GET /api/projects/:id/waiting-on-people ─ everyone else's items ───────────────────────▶ Blocked
board tasks (useBoardData, SSE) ─ in_progress, blocked-with-question, unowned, done ───▶ Running / Blocked / Done
GET /api/projects/:id/activity?since=<3 days ago> ─ latest `comment` detail per task ───────────▶ Running (last step)
GET /api/projects/:id/receipts/summary* ─ status, held, has_media per task ────────────▶ Done split
                                                                     * = from PR #149 (#126)
Phase 2: WaitItem.ask + .unblocks, PersonWaitItem.ask, unowned items, latest comments (server)
Phase 3: summary poster_key, comment-derived rows, day-report verified counts
```

### Lanes: sources and membership

`ui/src/lib/now.ts` exports `assignLanes(input): Lanes`, a pure function over the page's data.
Lanes are checked in this order, and a ticket goes into the first lane that matches. The order is
fixed in code and independent of the display order (Q3).

| # | Lane | A ticket is here when | Row source today (main) | Added by #125 / #126 | New API (phase) |
|---|------|-----------------------|-------------------------|----------------------|-----------------|
| 1 | **Needs you** | It is in the viewer's `ProjectMyWork.items`, of any kind. Not `coming_up`. | `GET /api/projects/:id/my-work` (`service.ProjectMyWork`, `waiting.go:1157`), with `task` detail on each item | `verify` kind for held receipts (#149); `holding_up` on do/answer (#148) | `ask`, `unblocks` on every item (2) |
| 2 | **Blocked** | (a) An item in another person's `waiting-on-people` row, except a `do` whose ticket is `in_progress` (that one goes to Running). (b) An open ticket with no owner that is blocked with a question (`state=blocked`, a reason that isn't `scheduled:`), or completed with `merge_state` needed. | `GET /api/projects/:id/waiting-on-people` (`waiting_people.go:96`; kind, ticket ids, since) plus board tasks for (b) | others' `verify` items (#149; unowned held tickets go to project owners) | `unowned[]` in `PeopleWaiting`, with `ask` (2) |
| 3 | **Running now** | `state=in_progress` and not already placed: agent tickets, and people's own human tickets in progress | board tasks (`useBoardData`), activity for the last step | — | `last_step` from `Repo.LatestComments` (2) |
| 4 | **Done but unverified** | `state=completed`, `completed_at` in the window (Q8), not already placed, and the status is below "verified" (see Verification dependency below) | board tasks | `receipts/summary` status (#149) | comment-derived rows, `completed_at` (3) |
| 5 | **Done & verified** | `state=completed`, in the window, and the status is `independently_verified` or `human_accepted` | board tasks | `receipts/summary` (#149) | `poster_key` for thumbnails (3) |

Blocked lists **root blockers**, not every ticket that waits. If five tickets wait on Sam's merge
of PR #61, Blocked shows one row, "Waits on Sam · Merge PR #61 · blocks 5". The five waiting
tickets aren't listed. They appear only in a footer line, "N waiting on other tickets · M
scheduled · K ready to dispatch", which links to the Board with that filter. This keeps Blocked
short, and every row in it names the person or question that would clear it.

Tickets outside the lanes: `ready` tickets that aren't anyone's `do`, tickets waiting on
dependencies that are running normally, scheduled tickets, and completed tickets outside the
window. These are counted in the footer and never listed.

### The ask rule

There is one rule per kind. Phase 1 updates `needsLine` (`lib/humanActions.ts:85`), which My work
already uses, so both pages change together. Phase 2 ports the same table to Go as
`service.WaitAsk(it WaitItem, t *domain.Task) string`. From then on it is sent as `WaitItem.ask`
(and `PersonWaitItem.ask`, and the `ask` on MCP's `waiting_on_people`). The UI then renders
`it.ask ?? needsLine(it)`, so an older server still works. The text is always derived on read, so
editing a ticket changes it at once.

| Kind | Ask text | From | Change from today's `needsLine` |
|------|----------|------|---------------------------------|
| `approve` | "Approve N tickets in `<epic>`" | `count`, `epic_name` | was "Review and approve N tickets" |
| `answer` | "Answer: `<first sentence of the question>`", else "Answer: open the ticket for the question" | `question` (the block reason) | none |
| `verify` (#126) | "Check #N: `<hold reason>`" | `hold_reason` | was "Couldn't verify #N: … Accept or reject it on the ticket" |
| `merge` | "Merge PR #N" (or "the PR", or "branch `<b>`"); a rule alert: "Review PR #N on GitHub"; a closed PR: today's text | `pr_url`, `branch`, `alert`, `merge_state` | the "N tickets wait" suffix moves to the unblock count |
| `start` | "Launch its run, or return #N to ready" | `task_id` | none |
| `do`, judgment with a decision doc (#125) | "Decide: `<the doc's question>`" | the current doc version's `decision_ask.question` | new |
| `do`, other | "`<reason verb>`: `<first sentence of description>`", else "`<verb>`: `<first acceptance criterion>`", else "No instructions: open the ticket" | `human_reason`, `description`, `acceptance_criteria` (`checklistItems`, `lib/brief.ts:16`) | the first criterion replaces "see the acceptance criteria in the brief" |

Rows that aren't wait items use the same helpers:

- Blocked rows prefix the ask with "Waits on `<name>` ·" or "Waits on nobody ·".
- Done but unverified rows show the first criterion that isn't `VERIFIED` (or a failed check),
  when there is one.
- Running rows show the last step (below).

The ask is never more than 140 characters (`NEEDS_MAX`), and it is one line.

### Unblock count and ordering

**Unblocks** is how many open tickets a row would release. Each row shows it as "blocks N" and
omits it at 0.

- Phase 1 uses the server's `holding_up` when the item has it: merge (direct waiters,
  `TrueWaiters`), and do or answer (transitive, from #148). Otherwise it uses the open tickets in
  `chainOf(id, tasks).down` (`lib/dagModel.ts:17`). An approve item uses `count`.
- Phase 2 sends `unblocks` on every item from `Repo.OpenDownstream`, with merges keeping
  `TrueWaiters`, so all three views count the same way.

| Lane | Order |
|------|-------|
| Needs you | unblocks, most first, then kind (approve, answer, verify, merge, start, do), then oldest `since` first |
| Blocked | unblocks, most first, then oldest `since` first |
| Running now | quiet first, then unblocks, most first, then longest running first |
| Done but unverified | `unverified` before `self_checked`, then newest `completed_at` first |
| Done & verified | newest `completed_at` first |

My work keeps grouping by kind (`waiting.go:376`), since it is the page where a person works
through the list kind by kind. Now ranks by impact across kinds, because its job is to answer
"what should I do first". Both read the same items, so only the order differs. Each lane shows 8
rows, then "Show all N".

### Running now: the last step

`ui/src/lib/lastStep.ts` exports `lastSteps(activity): Map<taskId, {text, at}>`, built from the
latest `action === 'comment'` entry for each ticket:

- `text` is the comment's first non-empty line, with Markdown markers stripped (as in
  `firstSentence`), cut at 90 characters with "…".
- Mission's Crew uses the same helper, which does the issue's quick-fix. `fromActivity` keeps
  `detail` for that.

| Ticket | Line 1 | Line 2 |
|--------|--------|--------|
| agent | "`<agent_id>` · #N `<name>` · running 34m" (since `dispatched_at`) | "`<last step>` · 2m ago", or "no updates yet", or "no updates in 3 days" when it was dispatched more than 3 days ago (Phase 1 only reads 3 days of activity) |
| agent, quiet | the same, with "quiet 22m" in `--st-flag` in place of "running" | the same |
| a person's own | "`<name>` · #N `<name>` · started 13h ago" | the last step, else the first acceptance criterion |

**Quiet** means `silentFor([lastSeen, dispatched_at, updated_at]) > QUIET_MS` (15 minutes,
`lib/ticketState.ts:59-65`). This is the rule Mission uses today, and Q4 asks whether to keep it.
Human tickets are never quiet. Phase 2 adds `Repo.LatestComments(taskIDs)`, which removes the
3-day window.

### Verification dependency (the Done lanes)

The status comes from #126's `GET /api/projects/:id/receipts/summary`, read with PR #149's
`useReceiptSummary`. It reloads on `receipt.updated`.

| Situation | Done & verified | Done but unverified |
|-----------|-----------------|---------------------|
| #149 not merged (the summary route 404s) | Empty: "Verification isn't available on this AMP yet" | Every completed agent ticket in the window: "No receipt" |
| #149 merged, project **Off** (the default, #126 D8) | Empty: "Nothing verified here. Receipts are off in this project" (Settings link for owners) | Every completed agent ticket in the window: "No receipt". Phase 3 adds "Self-checked (from comment) 3/4" from the server-side comment parse, still unverified (#126 D28) |
| **Inform** or **Require** | `independently_verified` ("Verified"), `human_accepted` ("Accepted by `<name>`") | `self_checked` ("Self-checked 2/2"), `unverified`, and a Reject ("Rejected by `<name>`: `<note>`") |
| A **held** receipt (Require) | — | — (it is a `verify` item: Needs you for its owner, or Blocked for others) |
| A **human** ticket (no receipts, #126 D15) | see Q7 (recommended: here, as "Done by `<name>`") | — |

- Under the issue's rule, "verified" means `independently_verified` or `human_accepted`.
  `self_checked` counts as unverified (Q6).
- In #126 v1 nothing writes `independently_verified`, so until its verifier follow-up exists, Done
  & verified holds only Accepted tickets.
- PR #149 has no separate `failed` status. A failed verifier check derives `unverified`
  (`DeriveStatus` rule 3). A Reject only removes an earlier Accept, so the status falls back to
  the worker's checks (often `self_checked`) and the receipt shows "Rejected by `<name>`: `<note>`"
  (`domain.Rejection`). Either way the ticket is Done but unverified. If #149 adds `failed` before
  it merges, those tickets are shown in `--danger-ink`.
- Badges reuse #149's `VerificationBadge` and `STATUS_LABEL` / `STATUS_TONE`. A row with
  `has_media` shows the media icon in Phase 1. Thumbnails come in Phase 3.

### Inline actions vs the drawer

Every row opens the drawer on click (`?focus=`, as on Mission). Inline actions use
`HumanActions` (`components/human/HumanActions.tsx:75`), the component My work and the drawer
already share, so the buttons, guards and copy are the same everywhere.

| Row | Inline on Now | In the drawer or elsewhere |
|-----|---------------|---------------------------|
| Needs you · answer | Answer: chips, text box, Send answer | — |
| Needs you · merge | Review PR ↗, and Mark merged where `HumanActions` offers it (never on a rule alert or a closed PR) | Reopen and Not needed for a closed PR |
| Needs you · start | Return to ready / Keep waiting (`StartAction`) | — |
| Needs you · do (not judgment) | Start, Mark done… (note, link, hours) | — |
| Needs you · do, judgment | — | The drawer, for the decision card (#125). A decision deserves the full brief and doc. |
| Needs you · approve | "Review plan" link | My work's Approve view (`?approve=<epic>`). Approval happens only there (`HumanActions.tsx:22`). |
| Needs you · verify (held) | Accept | Reject, which needs a note, in the drawer's receipt |
| Running now | — | The drawer |
| Blocked | — | The drawer. An unowned row's "Take it" opens the drawer with Reassign focused (`openReassign`). |
| Done but unverified | Accept, with receipts on, for a person with `canWork` | Reject and the full receipt, in the drawer |
| Done & verified | — | The drawer, at the receipt |

After an inline action the page refetches. The row leaves when the server no longer lists it,
because the page never removes a row on its own. An undo toast is a follow-up.

### UI

- **Route and tab.** `/project/:id/now` and a **Now** tab first in `ProjectNav`
  (`components/ProjectNav.tsx:22`). Phase 1 doesn't change the landing (`lib/landing.ts`). Q1
  decides that.
- **Layout.** One column of five lane sections, top to bottom, in the display order (Q3). Each
  has a header with its label and count, rows as above, and an empty line ("Nothing needs you
  here.", "Nothing running.", "Nothing stuck.", "Nothing finished in the last 7 days."). The
  footer line comes last. Each row is one line plus an optional second, so it fits a laptop
  screen.
- **Live.** Tickets come from `useBoardData` (SSE). My work, waiting-on-people and activity
  refetch 600 ms after tickets change, as Mission does, and activity also every 60 s. The receipt
  summary reloads on `receipt.updated`.
- **No AMP user** (sign-in off, or signed out; `noMyWorkReason`). Needs you shows Mission's
  `NoWaitingStrip` text in place of rows. The other lanes still work. (#124 / PR #165 gives local
  dev a real user.)
- **Reading someone else's Now** is out of scope. Needs you is always the viewer's.

## Alternatives

- **Mission's top band, not a new page.** The page stays one URL, but Mission is described as
  "informational… nothing here is an action" (`Mission.tsx:12-13`), and Now needs inline actions.
  Kept as an option in Q1.
- **Five columns side by side.** These are cramped below 1600 px, and a long ask truncates.
  Rejected for stacked lanes.
- **A server aggregate (`GET /api/projects/:id/now`) first.** It puts lane membership in one
  place. But it holds Phase 1 behind a new endpoint, while every source it needs already exists.
  Deferred to the Follow-ups.
- **A planner-written `metadata.ask`.** It would give a judgment ticket a second source for its
  question, beside the decision doc (#125 D14 rejected `tasks.ask` for that reason). Q5.
- **Listing every blocked ticket.** This repeats the same blocker N times. Rejected for root
  blockers plus a footer count.

## Risks

- **Now and My work disagree.** Mitigation: Needs you is the my-work response itself, and the
  ask is one function. Phase 2 moves both to the server.
- **Two unblock counts in Phase 1** (the server's `holding_up` and the client graph).
  Mitigation: the server value wins whenever it is present, and Phase 2 sends `unblocks` for
  every item. A test pins that the two agree on a fixture without `waits_for_merge: false`.
- **The 3-day activity window** shows "no updates in 3 days" on long-running tickets.
  Mitigation: the wording is honest, and Phase 2 reads the latest comment without a window.
- **False "quiet" on long, silent builds.** Mitigation: it says "quiet", never "stalled", as
  Mission does today. Q4 covers the threshold.
- **Older unverified work leaves the window.** Mitigation: the lane footer says "M more completed
  before <date>", linking to the Board's done filter.
- **Load.** Five requests per view, in the same family as Mission's (board, waiting-on-people,
  activity, audit, counts). No new queries in Phase 1.
- **Merge order.** Phase 1 builds on PR #148 and PR #149, reusing `useReceiptSummary`,
  `VerificationBadge` and the `verify` kind. If it must start earlier, the fallbacks above apply,
  and those PRs' files aren't duplicated.
- **Rollback.** Phase 1 is UI only: one new tab and a changed `needsLine`. Phases 2–3 add
  optional JSON fields that old clients ignore.

## Decisions & Assumptions

| ID | Decision or assumption | Why | By |
|----|------------------------|-----|----|
| D1 | One surface answers "what is it doing, what does it need from me, what got done and was it verified", with the five lanes of #127 | Owner goal and issue #127 | human |
| D2 | The Done lanes read #126's derived status and add none of their own | #126 D21: status is derived on read, one source | human |
| D3 | Projects default to receipts Off, so the Off fallback is the common case and must be honest | #126 D8 | human |
| D4 | Human tickets have no receipts | #126 D15 | human |
| D5 | Comment-derived self-checks stay "unverified", labelled "from comment" | #126 D28 | human |
| D6 | No `tasks.ask` column. A judgment ticket's question comes from its decision doc | #125 D14 | human |
| D7 | Unblock counts are transitive for do and answer and direct waiters for merges, matching My work and `waiting_on_people` | #125 D24 | human |
| D8 | Related changes ship in one PR per phase | Owner process constraint | human |
| D9 | Lane membership is exclusive and checked in a fixed order (Needs you, Blocked, Running, Done) | A ticket shown twice reads as two things | agent |
| D10 | Blocked lists root blockers (wait items of others, and unowned questions and merges), and counts their waiting dependents | Short lane, and every row names who clears it | agent |
| D11 | Needs you sorts by unblocks across kinds. My work keeps its kind grouping | Different jobs: what first vs work through | agent |
| D12 | Inline actions are exactly `HumanActions` plus receipt Accept. Judgment, approve and Reject go to the drawer or the Approve view | One set of guards. Decisions and approvals need their full view | agent |
| D13 | Phase 1 is UI only, on existing endpoints. The server ask and unblocks come in Phase 2 | Smallest first phase that meets the acceptance | agent |
| D14 | The ask text is derived on read, never stored | Edits show at once, and there is no backfill | agent |
| D15 | Mission Crew's last-step line ships in Phase 1, sharing `lastSteps` with Now | The issue's quick-fix, with one helper | agent |
| D16 | Five stacked lanes, 8 rows each with "Show all" | Fits a laptop, and reads top to bottom (north star) | agent |

## Open questions for the owner

1. **Does Now replace Mission as the landing page?** Options: (a) Now becomes the project
   landing, replacing today's "My work if anything waits, else Mission" rule, and Mission stays a
   tab; (b) Now replaces Mission entirely; (c) Now is Mission's top band. **Recommendation: (a).**
   It answers both landing cases on one page. Mission's epic grid stays useful for planning, and
   (c) conflicts with Mission's "nothing here is an action".
2. **Project page or a top-level page across projects (Portfolio level)?** **Recommendation:
   per-project in v1, with a cross-project `/now` as a follow-up.** Every source endpoint is
   per-project today, and the top-level `/` is already the cross-project My work.
3. **Lane display order.** The issue lists Needs you, Running now, Done & verified, Done but
   unverified, Blocked. **Recommendation: Needs you, Running now, Blocked, Done but unverified,
   Done & verified**, so the lanes that ask for action come first and Blocked sits next to the
   work it holds up.
4. **What counts as "quiet" for Running now?** Today it is 15 minutes with no activity
   (`QUIET_MS`). **Recommendation: keep 15 minutes in v1.** A per-project threshold, like
   `not_started_flag_minutes`, can follow if long builds cause false flags.
5. **Is the ask planner-written (`metadata.ask`) or always derived?** **Recommendation: always
   derived.** Judgment tickets get their question from the #125 decision doc, other human tickets
   from the brief's first sentence or first criterion. A planner field would be a second source
   (D6).
6. **Does `self_checked` count as verified?** **Recommendation: no.** As the issue says, it stays
   in Done but unverified, labelled "Self-checked 2/2" and with Accept. Until #126's verifier
   exists, Done & verified then holds only tickets a person Accepted.
7. **Where do completed human tickets go?** They have no receipts (D4). **Recommendation: Done &
   verified, labelled "Done by `<name>`".** The person did the work themselves. The alternative is
   to leave them off the Done lanes entirely.
8. **How far back do the Done lanes go?** **Recommendation: 7 days**, with a "M more before
   <date>" link to the Board. "Since your last visit" is the alternative.
9. **Inline actions on Now.** Today only My work and the drawer act, and Mission is
   informational. **Recommendation: allow the inline actions in the table above**, through the
   same `HumanActions`, so "clear the morning from one page" is possible.

## Implementation plan

v1 is Phase 1. Phases 2 and 3 are follow-up PRs, each one PR (D8). Each phase ends with the
Verification commands passing.

### Phase 1: the Now page on existing endpoints (UI only, v1)

Built on main after PR #148 and PR #149 merge.

- `ui/src/lib/now.ts` (create):
  - `assignLanes` with the membership table, `unblocksOf` (server `holding_up`, else
    `chainOf().down` open tickets, else `count`), the per-lane sort, and the footer counts;
  - tests for exclusivity, each membership row, the sort ties, the unowned question and merge,
    another person's in-progress `do` going to Running, and held receipts going to Needs you or
    Blocked.
- `ui/src/lib/lastStep.ts` (create): `lastSteps(activity)`, with tests for the first non-empty
  line, Markdown stripping, the 90-character cut, the latest comment winning, non-comment actions
  ignored, and CRLF.
- `ui/src/lib/humanActions.ts` (modify): the `needsLine` changes from the ask table (approve with
  the epic, verify, merge without the suffix, judgment from the doc question, the first
  criterion). Update `lib/humanActions.test.ts`, `ProjectMyWork.test.tsx` and `MyWork.test.tsx` for the new copy.
- `ui/src/pages/Now.tsx` (create) and `Now.test.tsx`:
  - the five lanes, the footer, the empty and no-identity states, "Show all", and the inline
    actions;
  - receipts summary 404 and Off fallbacks, and the Accept flow moving a row from Done but
    unverified to Done & verified.
- `ui/src/App.tsx`, `components/ProjectNav.tsx`, `lib/projectLink.ts` (modify): the route and
  tab.
- `ui/src/pages/Mission.tsx` (modify): `fromActivity` keeps `detail`, and Crew rows get the
  last-step line and "running 34m". `Mission.test.tsx` covers 'Step 2: wiring API', "no updates
  yet", and the ellipsis (the issue's quick-fix acceptance).
- `kb/ui-tour.md` (modify): the Now page.

### Phase 2: the ask and unblocks on the server (follow-up)

- `amp-api/internal/service/ask.go` (create): `WaitAsk`, the Go port of the ask table, with a
  table test that runs against the same cases as `needsLine`'s tests (a shared JSON fixture in
  `testdata/`).
- `service/waiting.go` (modify):
  - `WaitItem.Ask` and `WaitItem.Unblocks`, filled in `waitingOn` for every kind: approve =
    `Count`, merge = `len(HoldingUp)`, others from `Repo.OpenDownstream`;
  - `PersonWaitItem.Ask`, `Unblocks` and `Name`;
  - `PeopleWaiting.Unowned`: items for open questions and merges on unowned tickets.
- `mcp/human.go` (modify): `ask` and `unblocks` on `waiting_on_people` items.
- `repository/mywork.go` (modify): `LatestComments(ctx, taskIDs) map[int]ActivityLog`, using
  `DISTINCT ON (task_id)`. It is exposed as `last_step` on a new `GET
  /api/projects/:id/running`, or folded into the Follow-ups' aggregate if that comes first.
- UI: `it.ask ?? needsLine(it)`, `it.unblocks ?? unblocksOf(...)`, and the unowned rows from the
  server.
- `cmd/server/waiting_on_test.go` (modify): ask per kind; unblocks equal to `holding_up` for
  merges and to `waiting_on_people` for the rest; unowned items listed for any member.

### Phase 3: receipt-backed Done details (follow-up, after #149)

- `service/receipts.go` (modify): `ReceiptSummaryRow` gains `completed_at` and `poster_key` (the
  stored `poster.gif`, else the first `screenshot-<n>.png`).
- Comment-derived rows for completed agent tickets with no receipt in the window, as `source:
  "comment"` with `self_check: {verified, total}`. They come from a Go `ParseSelfCheck` that
  matches #126's `parseSelfCheck` tests, and their status is always `unverified` (D5).
- `domain/dayreport.go` (modify): `DayReportStats` gains `verified` and `unverified` (agent
  completions only), `service/dayreports.go` fills them, and the report Markdown says "N verified,
  M unverified".
- UI: thumbnails in Done & verified (`<img src=/api/receipts/<poster_key>>`, lazy), the "from
  comment" subline, and the day report counts.

## Follow-ups (not in this change)

- A cross-project Now (`/now`) if Q2 says so, built on `MyWorkSummaryFor` and per-project
  summaries.
- `GET /api/projects/:id/now`: one server response with lane membership, if the client rule
  proves hard to keep in step.
- A per-project quiet threshold (Q4), an undo toast after inline actions, the critical-path band,
  and Flight-plan stage progress on running rows (north star).
- Skeleton loading in under 300 ms after the first visit (north star).

## Verification

Run after every phase, from `v2/`:

```sh
(cd ui && npx tsc -b && npm run lint && npx vitest run)
(cd amp-api && go vet ./... && go test ./... &&
  TEST_DATABASE_URL=postgres://amp:amp@localhost:5432/amp_test?sslmode=disable go test ./cmd/server/ -run 'Waiting|MyWork|Receipt|DayReport')
```

Phase 1 changes no Go code, but runs the Go suite anyway to catch API contract drift.
`git diff --exit-code plugins/` stays clean, because no plugin source changes.

Manual acceptance (`make dev`, receipts Inform on one project and Off on another):

- On the Inform project, set up the Journey 1 tickets. Now shows each in exactly one lane:
  - the question to the owner is first in Needs you with "blocks N", and answering it inline
    removes it;
  - a ticket another member owns, blocked with a question, shows in Blocked as "Waits on
    `<name>`";
  - an unowned one shows as "Waits on nobody · Take it".
- An agent ticket with a comment "Step 2: wiring API" shows that line in Running now and in
  Mission's Crew. One with no comments shows "no updates yet". One silent for 16 minutes shows
  "quiet 16m".
- `scripts/seed-sample-receipt.sh <task_id>` (from #126) completes a ticket as Self-checked. It
  appears in Done but unverified. Accept moves it to Done & verified as "Accepted".
- On the Off project, Done & verified reads "Receipts are off in this project", and every
  recently completed agent ticket is "No receipt".
- My work's cards show the same ask text as Now's Needs you rows, for every kind.
