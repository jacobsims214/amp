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

- **Projects**: `amp_create_project`, `amp_list_projects`, `amp_get_project`, `amp_reset_project` (destructive), `amp_export_project`, `amp_import_project`, `amp_archive_project`, `amp_restore_project`
- **Epics**: `amp_create_epic`, `amp_list_epics`, `amp_get_epic`, `amp_update_epic`, `amp_delete_epic` (destructive, cascades)
- **Stories**: `amp_create_story`, `amp_list_stories`, `amp_list_project_stories`, `amp_get_story`, `amp_update_story`
- **Tasks**: `amp_create_task`, `amp_list_tasks`, `amp_get_task`, `amp_update_task`, `amp_dispatch_task`, `amp_complete_task`, `amp_block_task`, `amp_set_task_state`, `amp_set_task_start_at`, `amp_delete_task`, `amp_add_task_comment`, `amp_get_task_comments`, `amp_get_ticket_history`
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

## Key argument types

- `project_id`, `task_id`, `epic_id`, `story_id` — integers
- `priority` — `"0"`=low, `"1"`=normal (default), `"2"`=high, `"3"`=critical
- `dependency_ids` — array of integer task IDs (can span epics and stories)
- `start_at` — ISO 8601 datetime string, e.g. `"2026-08-01T09:00:00Z"`
- `assigned_to` — free text string matching a live agent name, e.g. `"amp-worker-backend"`
