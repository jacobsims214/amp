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
- **Tasks**: `amp_create_tasks`, `amp_list_tasks` (compact board; `format: "full"` for full records), `amp_get_task` (`include: ["comments","history","runs"]` for its comments, activity and runs), `amp_update_task`, `amp_dispatch_task`, `amp_complete_task`, `amp_block_task`, `amp_answer_question`, `amp_approve_plan`, `amp_set_task_state`, `amp_set_task_start_at`, `amp_delete_task`, `amp_add_task_comment`, `amp_claim_task`, `amp_assign_task`
- **People**: `amp_whoami`, `amp_list_project_members`
- **Daily reports**: `amp_day_report`, `amp_save_day_report`, `amp_list_day_reports`, `amp_velocity` — see the `amp-day-report` skill
- **Metadata**: `metadata` on the create/update tools for projects, epics, stories, and tasks; `amp_find_by_metadata`
- **Platform admin** (org owners/admins, team leads, global admins): `amp_list_orgs`, `amp_manage_org_members`, `amp_manage_teams`, `amp_manage_team_members`, `amp_manage_invites`, `amp_manage_project_teams`
- **Knowledge base**: `amp_kb_sources`, `amp_kb_search`, `amp_kb_get`, `amp_kb_write`, `amp_kb_list`, `amp_kb_delete`, `amp_kb_annotate`, `amp_kb_resolve_annotation`, `amp_kb_status` — the project KB by default; `scope: "team"` / `"org"` reach the team and org KBs (write there only when your user decides). See the `amp-kb` skill

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
- `amp_answer_question` {task_id, answer} — the orchestrator's way to record the user's answer:
  it's posted as a comment and the ticket goes back to ready (or to waiting, if it still depends
  on unfinished tickets). People answering in the AMP UI do exactly the same.
- The next worker dispatched to the ticket reads its comments first (`amp_get_task` with `include: ["comments"]`) and finds the answer.

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
