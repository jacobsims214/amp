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

## `blocked` means three different things — the buckets don't separate them

`amp_list_tasks` splits `scheduled` out of `blocked`, but everything else lands in one `blocked`
array, and the three cases need completely different handling:

| Case | How to recognise it | Who clears it |
|---|---|---|
| Waiting on a dependency | `blocked_by_ids` is non-empty | Nobody — the actor auto-unblocks it when the last dep completes |
| Waiting on a start time | `block_reason` starts with `scheduled:` (bucketed as `scheduled`) | Nobody — the timer unblocks it |
| **A worker blocked itself** | `blocked_by_ids` is **empty** and `block_reason` is free text | **The manager, by hand** — nothing auto-clears it |

The third case is the escalation path: `amp_block_task {task_id, reason}` is how a running
subagent says "I need an answer." It works from `in_progress`, sets the state to `blocked`, and
records the reason on the ticket. It is the only signal a worker has — a subagent cannot reach the
user, and its comments alone leave the ticket looking active.

Nothing ever unblocks that task on its own. To resume it, answer on the ticket and requeue:

```
amp_add_task_comment(task_id=ID, body="<the answer>", author="amp-manager")
amp_set_task_state(task_id=ID, state="backlog", reason="unblocked: <summary>")
```

`amp_set_task_state` is also the fix for a task stranded `in_progress` by a worker that crashed or
returned nothing — reset it to `backlog` and re-dispatch.

---

## Key argument types

- `project_id`, `task_id`, `epic_id`, `story_id` — integers
- `priority` — `"0"`=low, `"1"`=normal (default), `"2"`=high, `"3"`=critical
- `dependency_ids` — array of integer task IDs (can span epics and stories)
- `start_at` — ISO 8601 datetime string, e.g. `"2026-08-01T09:00:00Z"`
- `assigned_to` — free text string matching a live agent name, e.g. `"amp-worker-backend"`
