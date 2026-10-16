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

- **Projects**: `amp_create_project`, `amp_list_projects`, `amp_get_project`, `amp_reset_project` (destructive), `amp_export_project`, `amp_import_project`, `amp_archive_project`, `amp_restore_project`, `amp_update_project_settings` (owners)
- **Epics**: `amp_create_epic`, `amp_list_epics`, `amp_get_epic`, `amp_update_epic`, `amp_delete_epic` (destructive, cascades)
- **Stories**: `amp_create_story`, `amp_list_stories`, `amp_list_project_stories`, `amp_get_story`, `amp_update_story`
- **Tasks**: `amp_create_task`, `amp_list_tasks`, `amp_get_task`, `amp_update_task`, `amp_dispatch_task`, `amp_complete_task`, `amp_block_task`, `amp_set_task_state`, `amp_set_task_start_at`, `amp_delete_task`, `amp_add_task_comment`, `amp_get_task_comments`, `amp_get_ticket_history`, `amp_claim_task`, `amp_assign_task`
- **People**: `amp_whoami`, `amp_list_project_members`
- **Platform admin** (org owners/admins, team leads, global admins): `amp_list_orgs`, `amp_create_org`, `amp_manage_org_members`, `amp_manage_teams`, `amp_manage_team_members`, `amp_manage_invites`, `amp_manage_project_teams`
- **Knowledge base**: `amp_kb_search`, `amp_kb_get`, `amp_kb_write`, `amp_kb_list`, `amp_kb_delete`, `amp_kb_tags`, `amp_kb_reindex` — see `amp-kb` skill for how to use these well

---

## Scheduling — the one feature with no obvious entry point

Tasks can be held until a future time instead of sitting in the backlog:

- `amp_create_task` accepts an optional `start_at` (ISO 8601 datetime). The task is created
  `blocked` until that time, then auto-unblocks — same mechanism as a dependency, but time-based
  instead of task-based.
- `amp_set_task_start_at` {task_id, start_at?} — set or clear the schedule on an existing task.
  Omit `start_at` to clear it.
- `amp_list_tasks` returns a separate `scheduled` bucket for tasks waiting on their start time —
  distinct from `blocked` (which is waiting on dependencies) and `ready_to_dispatch`.

Use this for genuinely time-gated work (e.g. "don't touch this until the migration window
opens") — not as a substitute for `dependency_ids` when the real gate is another task finishing.

---

## Ownership — whose work a ticket is

You act as the AMP user who signed you in. Every ticket has at most one owner (a person, not an
agent); `assigned_to`/`agent_id` still name the agent profile that runs it.

- `amp_list_tasks` shows **your work by default**: tickets you own plus unassigned tickets you
  created. Pass `owner: "unassigned"` for the pool anyone can pick up, `owner: "any"` for
  everything, or a teammate's email.
- Tickets you create are yours. Pass `owner: "none"` to `amp_create_task` to leave one in the pool,
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
- `dependency_ids` — array of integer task IDs (can span epics and stories)
- `start_at` — ISO 8601 datetime string, e.g. `"2026-08-01T09:00:00Z"`
- `assigned_to` — free text string matching a live agent name, e.g. `"amp-worker-backend"`
- `owner` / `user` — a person: email, numeric user id, `"me"`, or `"none"`
