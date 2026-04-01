---
name: amp-mcp
description: Complete reference for all amp MCP tools — exact names, required arguments, and what each returns
---

# AMP v2 MCP Tool Reference

All tools are on the `amp` MCP server (http://localhost:8000/sse).

**This is the authoritative tool list. Do not call tools that are not listed here.**
If you need a capability that is not listed, say so — do not invent tool names.

---

## Projects

- `amp_create_project` {name, code, description?} → project with id
- `amp_list_projects` → `{projects: [...]}`
- `amp_get_project` {project_id} → `{project: {...}}`
- `amp_reset_project` {project_id} → deletes ALL epics/stories/tasks, keeps project — **destructive**

---

## Epics

- `amp_create_epic` {project_id, name, description?, priority?} → epic with id
- `amp_list_epics` {project_id} → `{epics: [...]}`
- `amp_get_epic` {epic_id} → `{epic: {...}}`
- `amp_delete_epic` {epic_id} → deletes epic + all its stories and tasks (cascades) — **destructive**

---

## Stories

- `amp_create_story` {project_id, epic_id, name, description?, acceptance_criteria?, priority?} → story with id
  - `epic_id` is **REQUIRED** — the API rejects stories without a parent epic
  - The epic must belong to the same project
- `amp_list_stories` {epic_id} → `{stories: [...]}`
- `amp_list_project_stories` {project_id} → `{stories: [...], count: N}` — all stories across all epics
- `amp_get_story` {story_id} → `{story: {...}}`

---

## Tasks

- `amp_create_task` {project_id, epic_id, story_id, name, description, acceptance_criteria, assigned_to, priority?, dependency_ids?} → task with id
  - `epic_id` is **REQUIRED** — tasks must belong to an epic
  - `story_id` is **REQUIRED** — tasks must belong to a story
  - `epic_id` must match the story's epic — the API rejects mismatches
  - `assigned_to` is **REQUIRED** — set at planning time who should work this task (e.g. "amp-worker")
  - State is **derived automatically**: no deps → `backlog`, any incomplete dep → `blocked`
  - Never set state yourself

- `amp_list_tasks` {project_id, state?} → `{ready_to_dispatch: [...], in_progress: [...], blocked: [...], completed: [...], count: N}`
  - `ready_to_dispatch` = all tasks in `backlog` state. **Dispatch all of these immediately.**
  - `blocked[].blocked_by_ids` = exactly which task IDs are still in the way

- `amp_get_task` {task_id} → `{task: {...}}`
  - Returns: id, project_id, epic_id, story_id, name, description, acceptance_criteria, state, priority, assigned_to, dependency_ids, blocked_by_ids, agent_id, dispatched_at, completed_at

- `amp_update_task` {task_id, name?, description?, assigned_to?, agent_id?}
  - Use `assigned_to` to correct a planned assignment before dispatch
  - Use `agent_id` only at dispatch time (system sets this automatically via amp_dispatch_task)

- `amp_dispatch_task` {task_id, agent_id}
  - Sets state=in_progress. **Returns error if task is blocked** — includes which dep IDs are incomplete.

- `amp_complete_task` {task_id}
  - Sets state=completed. **Auto-unblocks** any tasks that were waiting on this one.

- `amp_block_task` {task_id, reason}
  - Manually blocks a task with a reason.

- `amp_set_task_state` {task_id, state, reason?}
  - Manager escape hatch: override state directly.
  - Use when: re-opening a completed task, resetting a crashed in_progress task back to backlog.
  - Valid states: `backlog`, `in_progress`, `completed`, `blocked`
  - Logs the override to the task's activity log.

- `amp_delete_task` {task_id} → deletes a single task — use to remove planning mistakes

- `amp_add_task_comment` {task_id, body, author?}
  - Appends to the ticket log. Permanent. Used by workers to report progress.

- `amp_get_task_comments` {task_id} → `{comments: [...]}`

- `amp_get_ticket_history` {task_id} → `{task: {...}, history: [...], count: N}`
  - Returns the full activity log in chronological order.
  - Each entry: actor, action, from_state, to_state, detail, created_at
  - Actions: `created`, `dispatched`, `completed`, `blocked`, `unblocked`, `state_change`, `comment`

---

## Task states

| state | meaning | how it gets there |
|-------|---------|------------------|
| `backlog` | Ready to dispatch | created with no deps, or all deps just completed |
| `in_progress` | Agent working it | `amp_dispatch_task` |
| `completed` | Done | `amp_complete_task` |
| `blocked` | Waiting on dependencies | created with `dependency_ids` (auto), or `amp_block_task` |

---

## DAG rules

1. Task created with `dependency_ids` → **automatically set to `blocked`**
2. All dependencies reach `completed` → **automatically moved to `backlog`**
3. `amp_dispatch_task` on a blocked task → **error listing which dep IDs are incomplete**
4. `amp_complete_task` → scans all blocked tasks, unblocks any whose deps are now all met

No manual state management needed. The system handles all transitions automatically.

---

## Hierarchy enforcement

```
project_id must exist
  ↓
epic must belong to that project
  ↓
story must belong to that epic (and project)
  ↓
task must reference both epic and story (and they must match)
```

`dependency_ids` reference task IDs from **any** epic or story — cross-boundary
dependencies are normal and expected.

---

## Key argument types

- `project_id`, `task_id`, `epic_id`, `story_id` — integers
- `priority` — `"0"`=low, `"1"`=normal (default), `"2"`=high, `"3"`=critical
- `dependency_ids` — array of integer task IDs (can span epics and stories)
- `assigned_to` — free text string, e.g. `"amp-worker"` or `"amp-worker-frontend"`
