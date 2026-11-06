# Project inbox and GitHub Issues sync

Status: Draft · Date: 2026-11-05 · Issue: #161 (related: #3154 project GitHub credential, #2479
inbound GitHub webhook, tracking #141)

## Problem

AMP has nowhere to put work that has arrived but is not planned. The owner parks it as empty
epics: *"I'll tell the manager agent, just make an epic, put nothing in it, say it's parked, we'll
come back to it. That's kind of how I right now that's my hacky inbox. Like the projects need an
Inbox, connections to GitHub issues, all that sort of stuff like that shit's missing here."*
(Jacob, call 2026-11-05, 53:46).

What the code has today:

- No intake object. The plan is epics → stories → tickets (`v2/amp-api/internal/actor/project_actor.go`,
  package comment). Nothing exists above or beside an epic. A grep for `inbox` in `amp-api/internal`
  and `ui/src` finds only the lucide `Inbox` icon that the **My work** tab uses
  (`ui/src/components/shell/AppShell.tsx:20`).
- "Parked" is a naming convention and not a state. `EpicState` has `backlog`, `in_progress`,
  `completed` and `blocked` (`amp-api/internal/domain/models.go:28-32`). A parked epic is an empty
  `backlog` epic, so Mission and the Flight plan count it as planned work, and nothing can tell it
  apart from a real epic that has no stories yet.
- GitHub reaches a project in two ways, and neither one reads issues. The inbound webhook
  (`internal/api/github_hook.go`, `internal/service/github_hook.go:340`) answers 204 to anything
  that is not `pull_request`. The facts reader (`internal/githubfacts/client.go:77-88`) reads only
  repository, pull request, rules, protection and review-decision data, using the project
  credential (`internal/service/github_credentials.go`, #3154).
- Projects have no repository setting. The facts cache learns the repository slug from PR URLs on
  runs (`internal/service/github_facts.go`). Project metadata is free-form key/value data
  (`internal/domain/metadata.go`), so `repo` is only a convention.

**How we will know it is fixed**
- A person or an agent adds "Customer asks for CSV export" to project 7's inbox. It shows in the
  project's Inbox with source `manual` or `mcp`. Mission and the Flight plan do not count it as
  planned work.
- The manager agent calls `amp_inbox_list(project_id=7)`, then
  `amp_inbox_triage(item_id, action="plan_epic")`. A backlog epic is created with the item's title
  and body. The item becomes `planned` and links to the epic, and the audit log has
  `inbox.create` and `inbox.triage`.
- A parked epic with no stories becomes an inbox item with one call. The epic goes to the trash
  and the item keeps its name, description and metadata.
- (v2) With GitHub Issues connected, opening issue `acme/web#88` adds one inbox item with source
  `github`, the issue's link and `source_ref` set to `acme/web#88`. A redelivery or a re-import
  does not add a second item.

## Goals / Non-goals

- Goal: each project has an **inbox**, a list of unplanned work items, each with a title, body,
  source, link and triage state.
- Goal: people (UI) and agents (MCP) can add items. The manager agent can list and triage them,
  and triage turns an item into an epic or a story by the same path the plan uses today.
- Goal: parked epics can move into the inbox without losing text or metadata.
- Goal (v2): import GitHub Issues per project, and keep them in sync, using the project's existing
  GitHub credential. The owner's constraint applies: inbound events are handled by a Go actor,
  outbound calls go through the webhook actor, and both are audit-logged.
- Non-goal: becoming a ticket tracker, or two-way sync of state, assignees or comments with
  GitHub, Jira or Linear. Jd (12:07): *"tie into Jira, GitHub issues and Linear ... I don't think
  you should take that on because it's a solved boring problem."* The inbox *imports* and *links
  back*. It does not mirror.
- Non-goal in this spec: Jira and Linear. `source` is an open string so that they can be added
  later, but nothing here builds them.
- Non-goal: auto-triage. The agent proposes and triages only when it is asked to. Nothing becomes
  planned work on a timer.
- Non-goal: inbox items as tickets. They have no actor, no state machine, no dispatch and no
  dependencies.

## Journeys

**J1. A person captures work (UI).**
Given project 7, when Jacob opens **Inbox**, types "Customer asks for CSV export", adds a body
and saves, then the item appears at the top with state `new`, source `manual` and "added by
Jacob · just now". The Inbox tab badge shows the count of `new` items.

**J2. An agent captures work (MCP).**
Given a worker that finds an out-of-scope bug while on ticket #212, when it calls
`amp_inbox_add(project_id=7, title="Race in session lock", body="…", from_task_id=212)`, then the
item is created with source `mcp` and `metadata.from_task_id=212`. The worker's ticket is not
changed. The tool description tells workers to use this instead of creating epics or tickets for
work outside their ticket.

**J3. The manager triages (main path).**
Given three `new` items, when the owner says "triage the inbox" and the manager calls
`amp_inbox_list(project_id=7, state="new")`, then it gets each item with its title, body, source,
link, age and who added it. It then calls one of these for each item:
- `amp_inbox_triage(item_id, action="plan_epic", priority="2")`: creates an epic. The item becomes
  `planned` with `epic_id`.
- `amp_inbox_triage(item_id, action="plan_story", epic_id=31)`: creates a story in epic 31. The
  item becomes `planned` with `story_id`.
- `amp_inbox_triage(item_id, action="later", note="after launch")`: the item becomes `later`.
- `amp_inbox_triage(item_id, action="decline", note="duplicate of #88")`: the item becomes
  `declined`.
Each call writes one `inbox.triage` audit entry and emits `inbox.updated`.

**J4. Parked epics move into the inbox.**
Given epic 40 "Matrix federation (parked)" with no stories, when the manager calls
`amp_inbox_add(project_id=7, from_epic_id=40)`, then an item is created with the epic's name,
description and metadata, source `parked_epic` and `source_ref="epic:40"`, and epic 40 goes to
the trash (`DeleteEpic`, `service/plan.go:1141`). The trash can restore it. An epic that has
stories is refused: "epic 40 has 2 stories: only an empty epic can move to the inbox".

**J5. (v2) A GitHub issue arrives.**
Given project 7 is connected to `acme/web`, with label filter `amp`, when someone opens issue #88
with that label, then GitHub's `issues` delivery is checked as `pull_request` deliveries are
today, the project actor receives it, and one item is created with source `github`, the issue's
URL and `source_ref="acme/web#88"`. A later `edited` delivery updates the title and body while
the item is still `new`. A `closed` delivery moves a `new` item to `declined` with the note
"closed on GitHub", and `reopened` brings it back to `new`. Planned items are never changed by
GitHub. A delivery for a repository that is not connected, or for an issue without the label, is
ignored. Changes the hook misses are picked up by the 6-hour reconcile (§6).

**J6. (v2) Link back.**
Given item `acme/web#88` is triaged to epic 52, when link-back is on, then the webhook
dispatcher posts one comment on the issue: "Planned in AMP: <epic link>". The audit entry
`inbox.github.outbound` records the repository, the issue and the fact that a comment left the
system.

## Design

### Overview

```
 person (Inbox tab) ──POST /api/projects/:id/inbox──▶ Service.CreateInboxItem ─▶ inbox_items
 agent ──amp_inbox_add / _list / _triage──────────────▶ Service (authz, audit, inbox.updated)
                       triage plan_epic / plan_story ─▶ Registry.Send(MsgCreateEpic / MsgCreateStory)
 (v2) GitHub issues delivery ─▶ HandleGitHubDelivery (signature, claim) ─▶ ProjectActor MsgIntakeIssue
 (v2) GitHub import / reconcile ─▶ githubfacts OpIssues (project credential #3154) ─▶ ProjectActor MsgIntakeIssue
 (v2) link-back ─▶ webhook_deliveries row (kind github_issue_comment, migration 055) ─▶ GitHub API, audited
```

Every GitHub-originated write, pushed or pulled, goes through `MsgIntakeIssue` on the project
actor, so GitHub writes are serialized against each other. UI and MCP triage run in the service
and do not go through the actor. Writes from the two sides are kept safe by guarded SQL: every
state change is an `UPDATE … WHERE id = $1 AND state IN (<allowed from-states>)`, and a write that
matches no row loses the race and reports it. The actor does not stand in for those guards.

### 1. Data model (`053_project_inbox.sql`, Phase 1)

```sql
CREATE TABLE IF NOT EXISTS inbox_items (
    id          SERIAL PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title       TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 300),
    body        TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL,            -- manual | mcp | parked_epic | github (open set)
    source_ref  TEXT NOT NULL DEFAULT '', -- 'acme/web#88', 'epic:40'; '' for manual/mcp
    url         TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'new'
                CHECK (state IN ('new','planning','planned','later','declined')),
    claimed_at  TIMESTAMPTZ,              -- set when triage moves the item to 'planning'
    prev_state  TEXT CHECK (prev_state IN ('new','later')), -- state before 'planning'; NULL otherwise
    claim_action   TEXT CHECK (claim_action IN ('plan_epic','plan_story')), -- the planning call in flight
    claim_epic_id  INTEGER REFERENCES epics(id) ON DELETE SET NULL,          -- target epic for plan_story
    triage_note TEXT NOT NULL DEFAULT '',
    epic_id     INTEGER REFERENCES epics(id) ON DELETE SET NULL,
    story_id    INTEGER REFERENCES stories(id) ON DELETE SET NULL,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    triaged_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    triaged_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_inbox_items_source
    ON inbox_items(project_id, source, source_ref) WHERE source_ref <> '';
CREATE INDEX IF NOT EXISTS idx_inbox_items_open ON inbox_items(project_id, state, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_inbox_items_creator ON inbox_items(project_id, created_by, created_at);
```

- The unique partial index makes imports and redeliveries idempotent (J5). Manual items have no
  `source_ref`, so they never collide.
- `metadata` uses `domain.Metadata` and its limits (`domain/metadata.go`). When an item is
  planned it is copied onto the new epic or story, so `amp_find_by_metadata` keeps working for
  things like a Jira key.
- The migration is additive and idempotent, like 039 and 045. Main ends at
  `045_github_credential_secrets.sql`, but open PRs already hold 046 (#148), 047 (#149), 048
  (#171), 049 (#170), 050 (#172), 051 (#174) and 052 (#146 decide-by). This spec claims **053** (Phase 1), **054**
  (Phase 2) and **055** (Phase 3). The final numbers are assigned at merge time: whichever PR
  merges later renumbers its files to follow main.
- `domain/inbox.go`: `InboxItem`, `InboxState`, `InboxSource*` constants, and
  `InboxItem.Validate()`.

### 2. Service and permissions (`service/inbox.go`, Phase 1)

| Operation | Who | Notes |
|---|---|---|
| `CreateInboxItem` | project member (`authz.Member`) | source comes from the channel: `p.InUI()` → `manual`, otherwise `mcp`. Callers cannot set `github` or `parked_epic` |
| `ListInbox` | project member | filters by state (default `new`) and source, ordered newest first, limit 200 |
| `UpdateInboxItem` | creator or project owner, `new` and `later` items only | title, body, metadata |
| `TriageInboxItem` | project member, as for `amp_create_epic` | `plan_story` also needs `RequireEditStory`-level rights on the epic (`service/service.go:630`) |
| `InboxFromEpic` | `RequireEditEpic` on the epic | refuses an epic that has stories; then calls `DeleteEpic` |

- Planning uses the same path as the design-system flow, `s.Registry.Send(proj.ID,
  &actor.MsgCreateEpic{…})` followed by `actor.Await` (`service/designsystem.go:228`), with
  `MsgCreateStory` for stories. The epic's description is the item's body plus "From inbox: <url
  or source>". Creating the epic and updating the item are not one transaction, so `plan_epic`
  and `plan_story` **claim the item before calling the actor**:
  1. `UPDATE inbox_items SET state='planning', claimed_at=NOW(), prev_state=CASE WHEN
     state='planning' THEN prev_state ELSE state END, claim_action=$2, claim_epic_id=$3 WHERE
     id=$1 AND (state IN ('new','later') OR (state='planning' AND claimed_at < NOW() - interval
     '5 minutes')) RETURNING …`. No row means another call holds the claim, and the caller gets
     409 "inbox item is already being triaged". Two concurrent calls (UI and agent, or an agent
     retrying) cannot both pass this step.
  2. On **every** claim, fresh or stale, the holder looks for an earlier result before sending:
     epics in the project for `plan_epic`, or stories in the target epic for `plan_story`, whose
     `metadata.inbox_item_id` equals the item. If one exists it skips to step 4 with that id. The
     lookup runs on fresh claims too because an earlier call may have created the epic after its
     caller stopped waiting (step 3).
  3. Send `MsgCreateEpic` or `MsgCreateStory` with `metadata.inbox_item_id` set, and wait with
     `actor.Await` under an explicit deadline: `context.WithTimeout(ctx, 2*time.Minute)`.
     `Await` (`actor/await.go:20`) has no timeout of its own and waits only on `ctx`; the
     cross-replica path can take up to `routeTimeout` (30s) plus `forwardTimeout` (60s)
     (`actor/remote.go:50-53`), so 2 minutes covers both and stays under the 5-minute stale
     window. A live holder is therefore never reclaimed.
  4. `UPDATE … SET state='planned', epic_id|story_id=…, prev_state=NULL, claim_action=NULL,
     claim_epic_id=NULL, triaged_by, triaged_at WHERE id=$1 AND state='planning'`.
  Outcomes of step 3:
  - **Reply with no error**: step 4.
  - **Reply with an actor error that is not `ErrNoReply`** (validation, epic not found): the actor
    did not create anything, so the item goes back with `UPDATE … SET state=prev_state,
    prev_state=NULL, claim_action=NULL, claim_epic_id=NULL, claimed_at=NULL WHERE id=$1 AND
    state='planning'`, and the caller gets the error.
  - **`ErrNoReply`** (the deadline passed or the client went away): the epic or story may still be
    created, as `Await`'s error text says. The item is **left in `planning`** and never reverted.
    The caller gets 504 "planning did not finish in time; the item stays in Planning and can be
    retried". The next claim, after the stale window, finds any late result in step 2 and creates
    nothing new.
  A failure between 3 and 4 (a crash, or a failed step-4 UPDATE) also leaves the item in
  `planning` with an epic or story that names it, and is handled the same way.
- **Stuck items stay visible.** `ListInbox` returns `planning` items under their own filter
  (`state=planning`) and in `counts.planning`, with `stale: true` when `claimed_at` is older than
  5 minutes. The UI has a **Planning** filter that shows them with "stuck since <time>" when stale
  and a **Retry** action. Retry calls `TriageInboxItem` with `action="retry"`, which is allowed only
  on a stale `planning` item and runs steps 1–4 with the stored `claim_action` and
  `claim_epic_id`. The `new` badge also counts stale `planning` items, so a stuck item is not
  silently dropped from the queue. No sweeper is needed: recovery is always through step 2, which
  is the only path that can tell whether the epic exists.
- State changes: `new → planning | later | declined`, `later → new | planning | declined`,
  `planning → planned`, `planning → prev_state` (only on a definite actor error, never on
  `ErrNoReply`), and `declined → new` (reopen). `planned` is final. To undo it, delete the epic,
  which sets `epic_id` to NULL; the item stays `planned` with a note. Every transition is a
  guarded `UPDATE … WHERE state IN (…)`, which is what keeps UI/MCP triage and GitHub intake from
  overwriting each other (Overview).
- **Reopen.** `TriageInboxItem` with `action="reopen"` moves `later → new` or `declined → new`
  and clears `triage_note`. Who: the same as the other triage actions (project member, Q2). It is
  refused with 409 "the issue is closed on GitHub: reopen it there" when `metadata.github_closed`
  is true, because the next reconcile would decline the item again. GitHub's `reopened` remains the
  path for those items (§6).
- **Add limit.** `CreateInboxItem` refuses the call with 429 when `SELECT count(*) FROM
  inbox_items WHERE project_id=$1 AND created_by=$2 AND created_at > NOW() - interval '1 hour'`
  is 50 or more (index `idx_inbox_items_creator`). The count lives in Postgres, so it is the same
  on every replica. Agents are user accounts (`authz.Principal.UserID`), so the cap is per
  person or agent. It is not applied to imports, GitHub intake or `InboxFromEpic`, or to the
  local-dev user (`UserID` 0, so `created_by` is NULL). The check and the insert are not atomic, so
  concurrent calls can go a few over 50. That is acceptable for a flood guard.
- Every write calls `s.audit` (`service/audit.go:41`) with `inbox.create`, `inbox.update`,
  `inbox.triage` or `inbox.from_epic`, target type `inbox_item`. The audit trigger and outbox
  (migration 033) already forward these entries to SIEM webhooks with no further work.
- Every write publishes the new `EventInboxUpdated` (`"inbox.updated"`), which is added to
  `AllEventTypes` (`domain/models.go:596`). The UI uses it to refresh, and project webhooks can
  subscribe to it.

### 3. REST (`api/rest.go`, Phase 1)

```
GET    /api/projects/:id/inbox?state=new&source=github   → {items: InboxItem[], counts: {new, later, …}}
POST   /api/projects/:id/inbox                          {title, body, metadata}
PATCH  /api/inbox/:id                                   {title?, body?, metadata?}
POST   /api/inbox/:id/triage                            {action, epic_id?, priority?, note?}
       action: plan_epic | plan_story | later | decline | reopen | retry (§2)
POST   /api/epics/:id/to-inbox                          (J4)
```

### 4. MCP tools (`mcp/server.go`, `mcp/catalog.go`, Phase 1)

| Tool | Catalog entry |
|---|---|
| `amp_inbox_add` | "Add to inbox": capture unplanned work, a found bug outside your ticket, a parked idea. Takes `title` + `body`, or `from_epic_id` |
| `amp_inbox_list` | "List inbox", `readOnly`: incoming work not yet planned, triage queue |
| `amp_inbox_triage` | "Triage inbox item": plan an inbox item as an epic or story, defer it, decline it, reopen a deferred or declined item, or retry a stuck one. `action` is `plan_epic`, `plan_story`, `later`, `decline`, `reopen` or `retry` (§2) |

- `amp_get_plan` gains `inbox: {new: N}` so that a manager that reads the plan sees the queue
  without an extra call.
- `mcp/instructions.go`: one rule for workers ("work outside your ticket goes to
  `amp_inbox_add`, not to new epics or tickets"), and one for managers ("when asked to park
  something, add it to the inbox").
- `amp_inbox_triage` with `plan_epic` or `plan_story` is not affected by plan approval: it creates
  a backlog grouping with no tickets, which is what `amp_create_epic` does today.

### 5. UI (Phase 1)

- A new project tab, **Inbox**, placed after Mission. It is added to `ProjectView` in
  `ui/src/lib/projectLink.ts:11` and `components/ProjectNav.tsx:9`, to `PROJECT_VIEWS`
  (`AppShell.tsx:18`), and as a route in `App.tsx`. Its icon is lucide `ListPlus`, because `Inbox` is
  already My work's icon (see Q1). The badge shows the count of `new` items.
- `ui/src/pages/ProjectInbox.tsx`: a quick-add row (title, then optional body), state filters
  (New · Later · Planning · Planned · Declined) and a source chip on each row (GitHub rows link
  out). `new` and `later` rows have actions **Plan as epic**, **Add to epic…**, **Later** and
  **Decline**; `later` and `declined` rows have **Reopen**; stale `planning` rows show "stuck
  since <time>" and **Retry** (§2). The triage note is optional.
- Mission (`ui/src/pages/Mission.tsx`) stays informational, as its header comment requires, and
  gets one readout: "Inbox N new", which links to the tab.
- `api/client.ts`, `types/index.ts`: `InboxItem`, `listInbox`, `addInboxItem`, `triageInboxItem`,
  and `inbox.updated` in the `SSEEvent` union.

### 6. GitHub Issues (v2, Phases 2–3)

**Connection.** A new table `project_inbox_sources` has these columns: `project_id`, `kind`
(`github`), `repo` (owner/name slug, checked like `githubFactsSlug`), `label_filter` (optional),
`enabled`, `import_claimed_at` (the poll lease), `sync_cursor` (the `updated_at` of the last issue
fully processed; the watermark), `last_success_at`, `import_complete` (false until a run reaches
the end of its listing), `last_error`, `link_back`, `hook_events` (the event list from the last
`ping`, see below) and `last_issues_delivery_at`. It holds one row per project and repository. It is
configured in Project settings → GitHub, next to the existing **GitHub token** and **GitHub merge
webhook** sections (`ProjectSettings.tsx:270-279`), and only project owners can change it, as for
the credential.

**Import (pull).** A new read, `OpIssues`, is added to `githubfacts` (`GET
/repos/{o}/{r}/issues?state=…&labels=…&since=…&sort=updated&direction=asc`, skipping entries
that have `pull_request` set). It
uses the client's existing token source, retries, rate-limit handling and redaction
(`githubfacts/client.go:811-1038`). The service does the HTTP read and then sends one
`MsgIntakeIssue{…, Action: "sync"}` per issue to the project actor, which upserts by
`(project_id, 'github', 'owner/repo#n')` under the same rules as pushed events. The import does
not write to the repository itself, so an import running alongside an `edited` or `closed`
delivery is serialized with it. It reads at most 500 issues per run and reports when it stops
early. Because the listing is sorted by `updated` ascending, a run that stops early has processed
a prefix, and the next run resumes after it (below).

**Reconcile (decision D13: webhook plus a scheduled poll).** Deliveries can be missed (the hook is
not subscribed to `issues`, or AMP is down past GitHub's retries), so webhooks alone would drift
with no repair. The import runs on **Import now**, when the source is first enabled, and from a
ticker. The lease and the watermark are separate columns, so a failed or capped run never skips
issues:
- **Lease.** Every replica runs a 5-minute ticker. A source is claimed with `UPDATE
  project_inbox_sources SET import_claimed_at=NOW() WHERE id=$1 AND enabled AND
  (import_claimed_at IS NULL OR import_claimed_at < NOW() - interval '15 minutes') AND (NOT
  import_complete OR last_success_at IS NULL OR last_success_at < NOW() - interval '6 hours')
  RETURNING …`, so one replica runs each poll, and a source whose first import failed
  (`last_success_at IS NULL`) is retried on the next tick. **Import now** uses the same claim
  without the due condition. A run releases the lease (`import_claimed_at=NULL`) when it ends,
  whether it succeeded or failed; a crashed run's lease expires after 15 minutes.
- **Watermark.** The query is `state=open` until the first import completes, then `state=all`, and
  `since=<sync_cursor − 10m>` when `sync_cursor` is set. After each page is fully processed (every
  `MsgIntakeIssue` answered), the run advances `sync_cursor` to that page's last `updated_at`. A run
  that fails keeps the cursor of its last completed page, and the next tick resumes there.
- **Cap.** A run that stops at 500 issues sets `import_complete=false` and leaves the lease
  released, so the next tick (5 minutes later) continues from `sync_cursor`. A first import of a
  repository with more than 500 matching open issues therefore finishes over several ticks rather
  than dropping the rest. Only a run that reaches the end of the listing sets
  `import_complete=true` and `last_success_at=NOW()`. On the first completion it also switches
  later runs to `state=all`.
The scheduled run sees issues closed or edited while the hook was down, so a missed `closed` still
declines its `new` item. Drift is bounded at 6 hours plus the time to work through the backlog.

**Inbound events (push).** `handleGitHubEvent` (`service/github_hook.go:340`) accepts `issues`
events. `HandleGitHubDelivery` (`:244`) decodes the body today only when `event ==
"pull_request"`, and passes that payload's `ev.Action` to `ClaimGitHubDelivery`. It gains a
second decode into a new `ghIssuesEvent` (action, issue number/title/body/URL/labels/state,
`repository.full_name`) when `event == "issues"`, before the claim, so that the claim records the
issue action. A malformed `issues` body gets 400, as a malformed `pull_request` does. Signature
checking and the delivery log are unchanged. After the claim the service filters:
- The repository in the payload has to match an **enabled** `project_inbox_sources` row for the
  project in the URL. Otherwise the result is 204 "ignored: repo not connected to the inbox". One
  project hook secret can be installed on several repositories, and only connected ones count.
- If the source has a `label_filter`, `opened` and `reopened` for an issue without that label get
  204 "ignored: label filter". `edited`, `closed`, `deleted` and `transferred` are still delivered
  for an issue that already has an item, whatever its labels are now.

Then it sends `MsgIntakeIssue{Repo, Number, Action, Title, Body, URL, Labels}` to the project actor
through `s.Registry.Send`. Across replicas this message is routed to the project's owning replica
(`actor/remote.go`) and has to be registered there (`remote.go:394`). The actor applies these
rules, and every change goes through a guarded UPDATE that touches only items in `new` or `later`
(D9):

| Action | Effect |
|---|---|
| `opened`, `labeled` (label now matches), `sync` (open) | create the item if absent (unique index) |
| `edited`, `sync` (open) | update title and body of a `new` or `later` item |
| `closed`, `sync` (closed) | `new`/`later` → `declined`, note "closed on GitHub", `metadata.github_closed=true` |
| `reopened`, `sync` (open, item declined with `github_closed`) | `declined` → `new` and clear `github_closed`; an item a person declined stays declined; no item yet → create, as for `opened` |
| `deleted`, `transferred` | `new`/`later` → `declined`, note "deleted on GitHub" / "transferred on GitHub" |
| `unlabeled` | no change: removing the label does not take back an item already in the inbox |
| anything else (`assigned`, `milestoned`, `pinned`, …) | 204, ignored |

**Hook subscription.** Existing project hooks were set up for `pull_request` only
(`invalidateFactsForDelivery`'s comment, `githubHookInfo`'s setup). Changing the setup text to
"Pull requests, Issues" does not change what GitHub sends. So:
- On `ping` (sent when a hook is created or its events are edited), the service stores
  `hook.events` on every source row of the project.
- The GitHub Issues panel warns when `hook_events` lacks `issues` (or `*`), or when it is unknown
  and `last_issues_delivery_at` is empty: "Your GitHub webhook is not sending Issues events. Edit
  it on GitHub, tick Issues, and save (GitHub then sends a ping)." The panel links to the hook's
  settings page.
- The 6-hour reconcile means an unsubscribed hook delays changes but does not lose them.

**Outbound (link-back).** This is off by default. The webhook dispatcher
(`internal/webhook/webhook.go`) leases deliveries, retries with backoff and survives restarts, but
today it knows only one kind of delivery: `webhook_deliveries.webhook_id` is `NOT NULL` with a
foreign key to `webhooks` (`011_webhooks.sql`), the outbox is filled by matching hub and audit
events to webhook subscriptions, every POST is HMAC-signed with that webhook's secret, and
`consecutive_failures` lives on the `webhooks` row. Link-back therefore needs a schema change and a
second path in the delivery loop. It is not a small addition, and it is Phase 3's main cost.
- Migration `055_outbound_github.sql`: `webhook_deliveries` gains `kind TEXT NOT NULL DEFAULT
  'webhook'`, `project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE`, `target_url TEXT`
  and `inbox_source_id INTEGER REFERENCES project_inbox_sources(id) ON DELETE CASCADE`.
  `webhook_id` becomes nullable, with `CHECK ((kind = 'webhook') = (webhook_id IS NOT NULL))`.
  Existing rows default to `webhook`, so nothing changes for them.
- The service inserts `github_issue_comment` rows directly when a `github` item becomes
  `planned`. The hub and audit matcher never produces them.
- The delivery loop branches on `kind`. A `webhook` row keeps the HMAC path. A
  `github_issue_comment` row resolves the project credential (`service/github_credentials.go`)
  at send time, sends `Authorization: Bearer`, and posts to `target_url` (`POST
  /repos/{o}/{r}/issues/{n}/comments`). Failures go to the source's `last_error`, not to
  `webhooks.consecutive_failures`. Leasing, backoff and retry limits are shared. Webhook list and
  redelivery queries add `WHERE kind = 'webhook'` so these rows never show under a webhook.
It writes the audit entry
`inbox.github.outbound` with `{repo, issue, item_id, bytes}`, and this entry is the "data left the
system" record the owner asked for (01:05:40). The comment contains only a link and the epic or
story name.

**Audited throughout.** `inbox.github.import` (counts, plus whether the run stopped early),
`inbox.github.intake` (one entry per item created or changed), `inbox.github.outbound`, and
connection changes (`inbox.source.create`, `.update`, `.delete`).

## Alternatives

- **Keep parked epics and add a `parked` EpicState.** This would be smaller, but it puts
  unplanned work inside the plan tree, where Mission, the Flight plan and `amp_check_plan` treat
  it as planned, and it gives no place to record a source, a link or idempotency for imports.
- **Inbox items as tickets in an "Inbox" story.** Tickets carry actors, dispatch, dependencies
  and merge state (`actor/task_actor.go`). That is all wrong for raw intake, and they would show
  on the Board.
- **Two-way sync with GitHub, or depending on an external sync tool.** This is rejected for v2
  because sync is a solved problem (Jd, 12:07) and mirroring state doubles the sources of truth.
  An external tool could still feed `amp_inbox_add` over MCP.
- **Handling issue events directly in the HTTP handler, without the actor.** That is how merges
  work today. The owner asked for an actor (01:05:19). The actor orders GitHub writes (push and
  pull) against each other. Races with UI and MCP triage are handled by the guarded SQL
  transitions, not by the actor.
- **Webhooks only, accepting drift.** This is simpler, but a missed `closed` delivery would leave
  a `new` item that can never be declined. It is rejected in favour of the 6-hour reconcile (D13).
- **A separate outbox table for link-back.** This would leave `webhook_deliveries` untouched, but
  it would copy leasing, backoff and restart recovery. The `kind` column reuses them (§6).
- **Calling GitHub directly from the service for link-back.** This would have no retries, would
  lose work on restart, and would bypass the egress audit. It is rejected under the owner's
  constraint.

## Risks

- **The inbox becomes a second backlog that nobody reads.** Mitigation: the badge, the Mission
  readout, the `inbox` count in `amp_get_plan`, and "age" on every row.
- **An agent floods the inbox.** Mitigation: a limit of 50 items created per principal per
  project per hour, counted in Postgres so that it holds across replicas (§2), with a 429 and a
  clear message, and the `from_task_id` metadata that traces each item to its source.
- **The hook is not subscribed to Issues.** Mitigation: the panel warning from the `ping` events,
  and the 6-hour reconcile (§6).
- **Label-filtered imports miss issues, or unfiltered imports pull in noise.** Mitigation: the
  filter is visible and editable, and the import reports its counts. Q3 asks about the default.
- **Token scope.** Reading issues needs `issues:read`, and link-back needs `issues:write`. Today
  the credential is used for reads only. Mitigation: link-back is off by default, and a 403 is
  stored in `last_error` and shown in settings, with the token redacted as `redact` does today.
- **The epic is created but the item is not updated.** Mitigation: the item is claimed
  (`planning`) before the actor call, so concurrent triage gets 409. Every claim looks for an epic
  or story with that `metadata.inbox_item_id` before creating one, and an `ErrNoReply` leaves the
  item in `planning` instead of reverting it (§2). Stuck items show under **Planning** with
  **Retry**, so they do not disappear from the UI.
- **Rollback.** All changes are additive. A server without inbox code ignores the tables, and
  `issues` deliveries go back to 204. The exception is migration 055: an older dispatcher would
  lease `github_issue_comment` rows that have no `webhook_id`. So rolling back Phase 3 means first
  deleting those rows (`DELETE FROM webhook_deliveries WHERE kind <> 'webhook' AND status =
  'pending'`).

## Decisions & Assumptions

| ID | Decision or assumption | Why | By |
|----|------------------------|-----|----|
| D1 | Each project gets an inbox for unplanned work, separate from the epic/story/ticket tree | Owner, 53:46 ("the projects need an Inbox") | human |
| D2 | GitHub Issues connect per project through the existing project GitHub credential (#3154) | Issue #161 acceptance criteria | human |
| D3 | Inbound events are handled by a Go actor, outbound calls by the webhook actor, both audit-logged | Owner, 01:05:19–01:05:40; issue #161 | human |
| D4 | Import and link back. No two-way sync, and no Jira or Linear in this spec | Jd, 12:07 ("a solved boring problem"); Q4 asks Jacob to confirm | human (Jd), pending owner |
| D5 | v1 (Phase 1) ships as one PR, with commits that each leave the build green. v2 (Phases 2 and 3) is one separate follow-up PR, started only after Q3 and Q4 are answered | Owner's process constraint (related changes in one PR); blocked work is not bundled with shippable work | human |
| D6 | Items are rows, not tickets: no actor, no dispatch, no dependencies | Keeps intake off the Board and out of plan checks | agent |
| D7 | Triage creates epics and stories through `MsgCreateEpic`/`MsgCreateStory`, as the design-system flow does | One creation path; the actor tree stays authoritative | agent |
| D8 | The source is set by the server from the channel or the importer, never by the caller | `github` items have to come from a signed delivery or an import | agent |
| D9 | GitHub changes only `new` items; planned items are AMP's | Avoids partial mirroring (D4) | agent |
| D10 | Idempotency comes from the unique `(project_id, source, source_ref)` index | Redeliveries and re-imports are routine | agent |
| D11 | Link-back is off by default and posts only a link and a name | Needs a write scope, and data leaves the system | agent |
| D12 | Only empty epics move to the inbox, and through the trash | Nothing planned is lost, and it can be undone | agent |
| D13 | GitHub sync is webhooks plus a 6-hourly reconcile poll (`state=all&since=<sync_cursor>`), with a lease separate from the watermark, not webhooks alone | Missed deliveries would otherwise never be repaired; a failed or capped run must not skip issues | agent |
| D14 | All GitHub writes (push and pull) go through `MsgIntakeIssue`; UI/MCP vs GitHub races are handled by guarded SQL transitions | The actor serializes only what it receives | agent |
| D15 | Triage claims the item (`planning`) before creating the epic or story, looks up an earlier result on every claim, and never reverts on `ErrNoReply` | A lookup alone does not stop two concurrent creates; a timed-out create may still happen | agent |

## Open questions (for Jacob)

- **Q1. Where does the inbox live?** The proposal is its own tab, with a one-line readout on
  Mission. Should it be a panel inside Mission instead? Also, My work already uses the `Inbox`
  icon. Should the new tab be named "Inbox" and My work get a different icon, or should the new
  tab be named "Intake"?
- **Q2. Who triages?** The proposal is any project member, as for `amp_create_epic`. Should
  triage be limited to leads and owners, or to the manager agent's person?
- **Q3. GitHub import scope.** Should the default be issues that carry an `amp` label (the
  proposal), or all open issues?
- **Q4. Two-way sync.** Do you accept D4 (import plus link-back only)? Or should closing the AMP
  epic close the GitHub issue?
- **Q5. Bulk migration of parked epics.** Should one call move every empty epic whose name
  contains "parked", or should it be one epic at a time (the proposal)?

## Implementation plan

Two PRs (D5). Paths are relative to `v2/`. **The v1 PR is Phase 1 only** and does not wait for
any open question except Q1 and Q2, whose proposals it implements and which are cheap to change.
**Phases 2 and 3 (v2) are one separate follow-up PR**, and work on it starts only after Q3 and Q4
are answered. Phase 3 alone changes `webhook_deliveries` (migration 055), so it is not held in
the v1 PR.

### Phase 1: Inbox (v1)

- `amp-api/cmd/server/migrations/053_project_inbox.sql` (create): §1. The number is final at
  merge time (§1).
- `amp-api/internal/domain/inbox.go` (create). `domain/models.go`: `EventInboxUpdated` in
  `AllEventTypes`.
- `amp-api/internal/repository/inbox.go` (create): CRUD, guarded state transitions
  (`ClaimForPlanning` storing `prev_state`/`claim_action`/`claim_epic_id`, `CompletePlanning`,
  `RevertPlanning` to `prev_state`, `Transition(from []State, to State)`), upsert by source,
  and `CountCreatedSince(project, user, since)`.
- `amp-api/internal/service/inbox.go` (create): §2, including the triage claim, the add limit and
  `InboxFromEpic`, the explicit `Await` deadline, the `reopen` and `retry` actions, and stale
  `planning` items in `ListInbox`.
- `amp-api/internal/api/rest.go`: §3 routes.
- `amp-api/internal/mcp/server.go`, `mcp/catalog.go`, `mcp/instructions.go`, `mcp/robust.go`
  (timeouts): §4. `amp_get_plan` gains the `inbox` count.
- `ui/src/pages/ProjectInbox.tsx` (create). `lib/projectLink.ts`, `components/ProjectNav.tsx`,
  `components/shell/AppShell.tsx`, `App.tsx`, `pages/Mission.tsx` (readout), `api/client.ts`,
  `types/index.ts`.
- `kb/working/planning-and-approval.md`: the inbox, triage, and moving parked epics.
- `plugins/amp/skills/amp-planning/SKILL.md`: park work with `amp_inbox_add`, not with empty
  epics.

### Phase 2: GitHub import and inbound events (v2)

- `054_inbox_sources.sql` (create): `project_inbox_sources`.
- `internal/githubfacts/client.go`: `OpIssues` and `ListIssues` (`state`, `since`, labels).
- `internal/service/inbox_github.go` (create): the connection CRUD (owners only), the import and
  the 6-hour reconcile ticker with its claim (D13). Both send `MsgIntakeIssue` and never write
  items directly.
- `internal/service/github_hook.go`: decode `ghIssuesEvent` before the claim, the
  repository and label filters, `MsgIntakeIssue`, storing `hook.events` from `ping`, and the
  setup string.
- `internal/actor/messages.go`, `project_actor.go`, `remote.go`: `MsgIntakeIssue`, its handler,
  and its remote registration.
- `ui/src/components/GitHubIssuesPanel.tsx` (create), in `ProjectSettings.tsx`, including the
  "hook not sending Issues" warning.

### Phase 3: Link-back (v2)

- `055_outbound_github.sql` (create): `kind`, `project_id`, `target_url` and `inbox_source_id`
  on `webhook_deliveries`, nullable `webhook_id` with its CHECK (§6).
- `internal/webhook/webhook.go` and the outbox repository: branch the delivery loop on `kind`, a
  credential-authenticated sender for `github_issue_comment`, failures written to the source's
  `last_error`, `kind = 'webhook'` filters on the existing webhook queries, and the
  `inbox.github.outbound` audit entry.
- `internal/service/inbox.go`: queue the delivery when a `github` item is planned and link-back
  is enabled.

## Verification

Run for each phase (Phase 1 in the v1 PR, Phases 2 and 3 in the v2 PR):

```sh
cd v2/amp-api && go vet ./... && go test -race -count=1 ./...
cd v2/amp-api && TEST_DATABASE_URL=postgres://amp:amp@localhost:5432/amp_test?sslmode=disable go test -race ./cmd/server/ -count=1 -timeout 120s
cd v2/amp-api && TEST_CLUSTER=1 TEST_DATABASE_URL=… go test -race -count=1 ./cmd/server   # cross-replica
cd v2/ui && npx tsc -b && npm run lint && npx vitest run
```

**Phase 1**
- `cmd/server/inbox_test.go`:
  - The source is `manual` from the UI and `mcp` from MCP. A caller that sets `source=github` is
    ignored.
  - A non-member gets 404.
  - `plan_epic` creates a backlog epic that carries the item's metadata and
    `inbox_item_id`, and the item becomes `planned` with `epic_id`.
  - `plan_story` without rights on the epic gets 403.
  - Two concurrent `plan_epic` calls on one item create one epic, and the other call gets 409.
  - After a forced failure on the item update, a retry past the stale window finishes the
    item with the existing epic (or story, for `plan_story`) and creates no second one.
  - With an actor stub that replies after the caller's deadline, the call gets 504, the item
    stays `planning` (not reverted), and the late epic is found by the next claim, so exactly one
    epic exists.
  - A definite actor error (missing target epic) returns the item to its `prev_state` (`new` or
    `later`).
  - A stale `planning` item appears under `state=planning` with `stale: true`, and `retry` uses
    the stored `claim_action` and `claim_epic_id`. `retry` on a fresh claim gets 409.
  - `reopen` moves `later` and `declined` items to `new`, and is refused with 409 for an item with
    `metadata.github_closed`.
  - Moves out of `planned` are refused.
  - Each write produces one audit entry and one `inbox.updated`.
- `InboxFromEpic`: an empty epic moves to the trash and its item keeps the name, description and
  metadata. An epic with stories gets 400 with the message from J4.
- The 51st `amp_inbox_add` in an hour gets 429, including under `TEST_CLUSTER=1` with the calls
  spread across both replicas. Another principal is not affected.
- `ProjectInbox.test.tsx`: quick add, the filters, the badge count, and the triage actions.
  `Mission.test.tsx`: the readout links to the tab.

**Phase 2**
- `githubfacts/client_test.go`: `ListIssues` skips pull requests, follows pagination up to the
  cap, and redacts the token in errors.
- `cmd/server/inbox_github_test.go`:
  - A signed `issues/opened` creates one item, and a redelivery is a replay with no new item.
  - `edited` updates a `new` item and leaves a `planned` one alone.
  - `closed` declines a `new` item, and `reopened` restores it. An item a person declined stays
    declined.
  - `deleted` and `transferred` decline a `new` item. `unlabeled` changes nothing.
  - A delivery for a repository that is not connected, or an `opened` without the filter label,
    gets 204 and creates nothing.
  - A malformed `issues` body gets 400 before the claim, and the claim records the issue action.
  - A `ping` whose `hook.events` lacks `issues` makes the panel show the warning.
  - Running the import twice creates nothing new. An import and an `edited` delivery for the
    same issue at the same moment end with the delivery's text.
  - Reconcile: an issue closed while no hook was delivering declines its `new` item on the next
    scheduled run, and two replicas running the ticker poll once.
  - A run that fails after its first page keeps `sync_cursor` at that page and the next tick
    resumes there. A first import of 501+ open issues completes over two runs with every issue
    imported. A source whose first import failed (`last_success_at` NULL) is polled on the next
    tick. A crashed run's lease expires after 15 minutes.
  - A non-owner cannot configure the connection.
  - It passes under `TEST_CLUSTER=1`, with the delivery handled on a replica that does not own
    the project.

**Phase 3**
- Against a stub GitHub server: planning a `github` item queues one delivery, the retry after a
  500 posts once, a 403 lands in `last_error`, and the `inbox.github.outbound` entry carries the
  repository, the issue and the byte count. With link-back off, nothing is queued.
- The request carries the project credential and no AMP HMAC signature header. A failure does not
  touch any `webhooks.consecutive_failures`, and the row does not appear in any webhook's
  delivery list. Existing webhook deliveries pass unchanged after migration 055.

**Manual check** (`docker compose up`): add an item in the UI and another with `amp_inbox_add`,
triage one to an epic with `amp_inbox_triage`, and confirm the epic in the Flight plan and both
audit entries in the Audit page. Then move an empty "parked" epic into the inbox and restore it
from the trash.
