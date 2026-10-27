---
name: amp-mcp
description: Complete reference for all amp MCP tools — exact names, required arguments, and what each returns
---

# AMP v2 MCP Tool Reference

All tools are on the `amp` MCP server (http://localhost:8000/sse).

**This is the authoritative tool list. Do not call tools that are not listed here.**
If you need a capability that is not listed, say so — do not invent tool names.

**Your tool descriptions already tell you the mechanics** (required args, hierarchy rules,
state-transition behavior) — you see them every time you call a tool. This doc exists for the
facts that are NOT obvious from a single tool call: the full tool list, argument types, and the
one feature with no obvious entry point — scheduling.

---

## Full tool list by area

- **Projects**: `amp_create_project`, `amp_list_projects`, `amp_get_project`, `amp_update_project_settings` (owners). Rare project admin (archive, restore, reset, export, import) is in the AMP UI.
- **The plan**: `amp_get_plan` (epics → stories with ticket counts, one call), `amp_list_epics`
- **Epics**: `amp_create_epic`, `amp_update_epic`, `amp_delete_epic` (destructive, cascades)
- **Stories**: `amp_create_story`, `amp_update_story`, `amp_move_story`, `amp_delete_story`
- **Planning**: `amp_create_tasks` (several tickets and their dependencies in one call), `amp_set_task_dependencies`, `amp_move_task`, `amp_check_plan` — see the `amp-planning` skill. Dependencies drive dispatch: never leave them out, and never delete and re-create tickets to fix them
- **Tasks**: `amp_create_tasks`, `amp_list_tasks` (compact board; `format: "full"` for full records), `amp_get_task` (`include: ["comments","history","runs"]` for its comments, activity and runs), `amp_update_task`, `amp_dispatch_task`, `amp_complete_task`, `amp_block_task`, `amp_answer_question`, `amp_approve_plan` (approval status only: it never approves; see below), `amp_set_task_state`, `amp_set_task_start_at`, `amp_delete_task`, `amp_add_task_comment`, `amp_claim_task`, `amp_assign_task`, `amp_handover_tasks` (move a person's open tickets to someone else)
- **Runs and trace**: `amp_report_run` (record a run without changing the ticket's state: killed, abandoned or handed over, usage learned afterwards, a PR opened after completion), `amp_trace` (commit, branch or PR → the tickets that produced it), `amp_list_unmerged` (done tickets whose pushed work isn't merged yet), `amp_report_merged` (CI or a person records a merge by `commit`, `branch` or `pr_url`, with `merge_commit` for the merge SHA; every ticket whose runs produced it gets a run marked merged; an integration branch's merge also reaches the runs whose `base_branch` is it). Runs take `base_branch` (the PR's target, or the branch the work was merged into). Metadata `integration_branch` (epic/story) and `waits_for_merge: false` (a review ticket) keep dependents from waiting on merges to main. `amp_session_usage` is not an AMP server tool: the harness plugin provides it (Claude Code with the AMP plugin, Claude Code) to report your own model, tokens and cost, which you pass to `amp_complete_task` / `amp_block_task`
- **Trash**: `amp_list_deleted_tasks` (tickets, epics and stories deleted in the last 30 days), `amp_restore_tasks` (`task_ids`, `story_ids`, `epic_ids`; restore the epic, then its stories, then their tickets, in the same call)
- **Artifacts**: `amp_publish_artifact` (mockups, prototypes, diagrams, images, PDFs, documents for review), `amp_get_artifact`, `amp_list_artifacts`, `amp_review_artifact` and `amp_keep_artifact` (people only: request changes, keep it from expiring; approving, `approve=true`, is done by a person on the artifact page in the web UI and refused over MCP). Design systems: `amp_design_context`, `amp_design_system_start`, `amp_design_system_publish`
- **People**: `amp_whoami`, `amp_list_project_members`
- **Human tickets**: `performer: "human"` + `human_reason` on `amp_create_tasks` / `amp_update_task`; `waiting_on_people` in `amp_list_tasks`; `human_gates` in `amp_check_plan`; `amp_human_impact` (agents vs people, what held the plan up)
- **Daily reports**: `amp_day_report`, `amp_save_day_report`, `amp_list_day_reports`, `amp_velocity` — see the `amp-day-report` skill
- **Metadata**: `metadata` on the create/update tools for projects, epics, stories, and tasks; `amp_find_by_metadata`
- **Platform admin** (org owners/admins, team leads, global admins): `amp_list_orgs`, `amp_manage_org_members`, `amp_manage_teams`, `amp_manage_team_members`, `amp_manage_invites`, `amp_manage_project_teams`
- **Knowledge base**: `amp_kb_sources`, `amp_kb_search`, `amp_kb_get`, `amp_kb_write`, `amp_kb_list`, `amp_kb_delete`, `amp_kb_list_deleted`, `amp_kb_restore`, `amp_kb_annotate`, `amp_kb_resolve_annotation`, `amp_kb_status`, `amp_kb_reindex` — the project KB by default; `scope: "team"` / `"org"` reach the team and org KBs (write there only when your user decides). See the `amp-kb` skill
- **`amp_kb_reindex`**: rebuilds a KB's search index by re-embedding every document. Use it when search looks broken: scores far above 1, results that match only on keywords, or a doc that `amp_kb_get` reads but `amp_kb_search` never finds. It runs in the background and returns at once (search again in a minute), doesn't change documents, and needs write access to that KB, like `amp_kb_write`
- **`amp_kb_list_deleted` / `amp_kb_restore`**: a deleted KB doc stays in the KB's trash for 30 days. `amp_kb_list_deleted` lists them (path, title, deleted_at, deleted_by); `amp_kb_restore` brings one back by path with its content, tags and annotations, readable and searchable again. Both need write access to that KB, like `amp_kb_delete`, and refuse the global KB

---

## What only a person does, in the web UI

Some steps are refused over MCP for every caller, agent accounts and a person's own MCP login
alike. The refusal ends: "… is done by a person in the AMP web UI, not by an agent or over MCP:
<link>. Stop and ask your person to do it there." Do exactly that: give your person the link and
stop. Don't retry, and don't try another tool or the REST API.

| Step | Where the person does it | What you get over MCP |
|---|---|---|
| Approving a plan (per ticket: each person their own; a lead on someone's behalf) | The project's My work tab, "Approve" (Flight plan only links there) | `amp_approve_plan` always refuses, with `approve_url`, `awaiting_approval`, `require_plan_approval`, `plan_approved_at`. Never call it to approve. |
| Approving an artifact | Artifact page, Approve | `amp_review_artifact(approve=true)` refuses; `approve=false` with a note still works |
| Marking a human ticket done | The project's My work tab (or the ticket drawer), Mark done | `amp_complete_task` / `amp_set_task_state(completed)` refuse with the ticket link |
| Turning `require_plan_approval` off | Project settings | `amp_update_project_settings` refuses; turning it on works |

The approval gate in practice:

- **Approval is per ticket.** Each person approves their own tickets in the web UI; a project
  lead (or owner, admin) may approve someone else's on their behalf. A ticket you plan for your
  person is theirs to approve (an unassigned ticket belongs to whoever planned it).
- **`amp_check_plan`** returns `approve_url` (the My work link, `/project/N/my-work?approve=all`) whenever `awaiting_approval`
  isn't empty. `awaiting_approval` is every open ticket awaiting approval, anyone's;
  `awaiting_approval_tickets` lists them as `{task_id, owner_user_id, yours}`, and
  `awaiting_yours` is your person's own. That's the hand-off point: tell your person which of
  **their** tickets await approval (`awaiting_yours`) with the link, say others' tickets wait for
  their owners (or a lead on their behalf), and wait.
- **`amp_dispatch_task`** on a ticket that isn't approved refuses: "ticket N isn't approved …
  Its owner approves it in the AMP web UI (Approve plan; a project lead may approve it on their
  behalf): <link>". A ticket is re-planned (needs approving again) when it's created, restored
  from the trash, or its name, description or acceptance criteria change.
- **`amp_complete_task`** on a ticket that was never dispatched refuses: "never dispatched …
  Dispatch it first". Off the UI a ticket can only be completed from `in_progress`.
- **`amp_set_task_state`** can't move a ticket to `in_progress` (use `amp_dispatch_task`) or take a
  question back to backlog (answer it). Moving work back (to backlog or blocked) is allowed.
- **`amp_answer_question`** is allowed: it relays your person's answer, and AMP shows it as
  "<email> (relayed by agent)". Only relay what they said.

Reading is always fine. A status question ("what's on the board?") needs only `amp_list_tasks`,
`amp_get_plan`, `amp_get_task`, `amp_check_plan` and `amp_list_unmerged`: don't comment, claim,
dispatch or complete anything to answer it.

---

## Metadata — fields for integrations

Projects, epics, stories, and tasks carry free-form key/value `metadata` (values: string,
number, or boolean), e.g. `jira_board: "PLAT"`, `initiative: "Q3 growth"`,
`capitalizable: true`, or a ticket's `jira_key: "PLAT-123"`.

- Set it on create (`metadata` object) or change it on update: keys with a value are set,
  keys set to `null` are removed, others are kept.
- It's inherited downwards: a ticket's `effective_metadata` (on `amp_get_task`, and in daily
  reports) is its project's, epic's, and story's fields with its own on top.
- `amp_find_by_metadata(kind, match)` finds items by effective metadata across the projects you
  can see. Types matter: `true` the boolean doesn't match `"true"` the string.
- Keys are free-form, so reuse the names a project already uses (read them first) rather than
  inventing near-duplicates like `jiraBoard` next to `jira_board`.

---

## Scheduling — the one feature with no obvious entry point

Tasks can be held until a future time instead of sitting in the backlog:

- `amp_create_tasks` accepts an optional `start_at` (ISO 8601 datetime) per ticket. The task is created
  `blocked` until that time, then auto-unblocks — same mechanism as a dependency, but time-based
  instead of task-based.
- `amp_set_task_start_at` {task_id, start_at?} — set or clear the schedule on an existing task.
  Omit `start_at` to clear it.
- `amp_list_tasks` counts tasks waiting on their start time as `scheduled` — distinct from
  waiting on dependencies and from `ready_to_dispatch`.

Use this for genuinely time-gated work (e.g. "don't touch this until the migration window
opens") — not as a substitute for `depends_on` when the real gate is another task finishing.

---

## Questions — a worker that needs a person

A subagent can't wait for an answer, so a question lives on the ticket:

- `amp_block_task` {task_id, reason} — the worker's way to stop and ask. `reason` is required and
  is the question people see in AMP ("Avalara or Stripe Tax? …"). Not for waiting on other
  tickets — that's a dependency.
- `amp_list_tasks` returns a `questions` list: each blocked ticket with its question.
- `amp_answer_question` {task_id, answer} — the orchestrator's way to relay the user's answer:
  it's posted as a comment marked "relayed by agent" and the ticket goes back to ready (or to
  waiting, if it still depends on unfinished tickets). People answering in the AMP UI do exactly
  the same, under their own name. Never answer from your own judgment.
- The next worker dispatched to the ticket reads its comments first (`amp_get_task` with `include: ["comments"]`) and finds the answer.

---

## Human tickets — a person in the loop

A ticket is done by an agent (the default) or by a person: `performer: "human"`, with
`human_reason` one of `review`, `approval`, `access`, `judgment`, `external`, `physical`, `other`,
and its `owner` the person who does it (required). It sits in the dependency graph like any other
ticket, so whatever `depends_on` it waits for the person.

- **Never dispatched.** `amp_dispatch_task` refuses it. Its owner clicks Start and Mark done in
  AMP's web UI. It isn't in `ready_to_dispatch` or `in_progress`, and it's never `quiet`.
- **`amp_list_tasks` → `waiting_on_people`** (Waiting on you): what a person must do next that
  matters to you. Each item has a `kind`: `approve` (a plan awaiting their approval in the My work
  tab; `epic_id`, `count`, `task_ids`), `answer` (a question on their ticket; `question`), `merge`
  (their done ticket is Merge needed, or its PR closed: `merge_state`, `pr_url`) or `do` (their
  human ticket, ready or started). Each also has `id`, `owner` (email), `reason`, `state`,
  `waiting_hours`, `holding_up` (open ticket IDs downstream) and, for `do`, `stale` (48h with no
  activity). `coming_up` lists their human tickets not actionable yet (`waits_on`,
  `awaiting_approval`, `start_at`). Never dispatch these; tell your user who the work waits on.
- **Only the person marks it done, in the web UI.** `amp_complete_task` on a human ticket is
  refused for every MCP caller, even on your user's word; give them the ticket link instead.
- **Switching who does it**: `amp_update_task(task_id, performer, human_reason)`. Not while it's
  in progress; the change is on the ticket's history.
- **`amp_check_plan` → `human_gates`**: each open human ticket and how many open tickets wait on
  it. A human ticket with no owner is a NO-GO issue.
- **`amp_human_impact(project_id, days?)`**:
  - agents vs people: tickets, median wait (ready → started), median time once started, hours
    people reported;
  - human work by reason;
  - the human tickets that held the plan up longest.

---

## Ownership — whose work a ticket is

You act as the AMP user who signed you in. Every ticket has at most one owner (a person, not an
agent); `assigned_to`/`agent_id` still name the agent profile that runs it.

- `amp_list_tasks` shows **your work by default**: tickets you own plus unassigned tickets you
  created. Pass `owner: "unassigned"` for the pool anyone can pick up, `owner: "any"` for
  everything, or a teammate's email.
- Tickets you create are yours. Pass `owner: "none"` on a ticket in `amp_create_tasks` to leave it in the pool,
  or a teammate's email to give it to them.
- `amp_claim_task` takes an unassigned ticket, and never someone else's. `amp_dispatch_task` on an
  unassigned ticket claims it too. A ticket someone else owns can't be dispatched, completed, or
  edited by you.
- To take one over, `amp_assign_task` with `user: "me"` (`user: "none"` releases it). In projects
  with `open_reassign` on (the default; see `amp_get_project`), any member can do that while the
  ticket isn't in progress. Otherwise ask its owner or a team lead. Only take over a teammate's
  ticket when you've been told to.
- Epics and stories are shared: put stories and tickets under anyone's. Changing an epic or story
  needs you to be one of its owners (`add_owners` on `amp_update_epic` / `amp_update_story`).
- A project you aren't on (through one of your teams) is reported as not found.
  `amp_whoami` shows your orgs and teams; `amp_create_project` needs `org_id`/`team_id` only if
  you're in several.

---

## Platform administration

If you act for an admin you can run the platform for them. The `amp_manage_*` tools take an
`action`; check `amp_whoami` / `amp_list_orgs` for the IDs and your role first.

- New org (global admins only): `amp_create_org` {name, owner?}.
- People: `amp_manage_org_members` {org_id, action: list|add|set_role|remove, user, role}.
  Adding an email that has never signed in creates an invite; it applies on their first sign-in.
- Teams: `amp_manage_teams` {action: list|create|rename|delete, org_id or team_id, name};
  members via `amp_manage_team_members` {team_id, action, user, role: lead|member}.
- Project access: `amp_manage_project_teams` {project_id, action: list|link|unlink, team_id, role: owner|member}.
- You get a "requires …" error for anything your user isn't allowed to do — report it, don't retry.
  Creating login credentials (passwords) is still done on the Users page, not over MCP.

---

## Key argument types

- `project_id`, `task_id`, `epic_id`, `story_id` — integers
- `priority` — `"0"`=low, `"1"`=normal (default), `"2"`=high, `"3"`=critical
- `depends_on` (in `amp_create_tasks`) — refs of tickets in the batch or existing ticket IDs (can span epics and stories)
- `start_at` — ISO 8601 datetime string, e.g. `"2026-08-01T09:00:00Z"`
- `assigned_to` — free text string matching a live agent name, e.g. `"amp-worker-backend"`
- `owner` / `user` — a person: email, numeric user id, `"me"`, or `"none"`
