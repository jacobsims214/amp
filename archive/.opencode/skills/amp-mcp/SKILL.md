---
name: amp-mcp
description: Complete reference for all amp-odoo MCP tools — exact names, arguments, and what each returns
---

# AMP MCP Tool Reference

All tools are on the `amp-odoo` MCP server. 26 tools total.

## Project

- `amp_list_projects` → `{projects: [...]}`
- `amp_get_project` {project_id} → `{project: {...}}` full counts and progress
- `amp_get_project_by_code` {code} → `{project: {...}}`
- `amp_create_project` {name, code, description}
- `amp_update_project` {project_id, name?, description?, state?}

**On startup: call `amp_get_project` + `amp_list_tasks` to see real state. Nothing else.**

## Epics

- `amp_create_epic` {name, project_id, description?, priority?} → returns epic with id
- `amp_get_epic` {epic_id} → `{epic: {...}}`
- `amp_list_epics` {project_id} → `{epics: [...]}`
- `amp_set_epic_state` {epic_id, state, reason?} — manager override: `'in_progress'`, `'completed'`, `'blocked'`, `'backlog'`

## Stories

- `amp_create_story` {name, project_id, epic_id, description?, acceptance_criteria?, priority?} → returns story with id
- `amp_get_story` {story_id} → `{story: {...}}`
- `amp_list_stories` {epic_id} → `{stories: [...]}`
- `amp_set_story_state` {story_id, state, reason?} — manager override: same states as epic

## Tasks

- `amp_list_tasks` {project_id?, epic_id?, story_id?, state?} → `{tasks: [...], count: N}`
  **Use this to see what work exists and what state it's in.**

- `amp_get_task` {task_id} → full task with description_text, acceptance_criteria, context_data

- `amp_create_task` {name, project_id, epic_id?, story_id?, description?, acceptance_criteria?, priority?, dag_level?, dependency_ids?[]}

- `amp_update_task` {task_id, name?, description?, state?, agent_id?, context_data?}

- `amp_dispatch_task` {task_id, agent_id}
  Sets state=in_progress. **Raises a structured error if the task is blocked** (lists which dep task IDs must complete first).

- `amp_complete_task` {task_id} — sets completed, auto-unblocks downstream tasks, auto-progresses story/epic

- `amp_add_task_comment` {task_id, body} — permanent chatter log entry

## Knowledge Base

- `amp_create_kb_entry` {title, content, project_id, entry_type?, task_id?, story_id?, epic_id?, tags?, created_by_agent?}
- `amp_search_kb` {query?, project_id?, entry_type?, limit?}

## Cleanup

- `amp_delete_task` {task_id}
- `amp_delete_epic` {epic_id} — cascades to all stories and tasks
- `amp_reset_project` {project_id} — delete all epics/stories/tasks, keep the project

---

## Task states — the complete picture

| state | meaning | how it gets there | what to do |
|-------|---------|------------------|------------|
| `backlog` | Workable — no incomplete deps | created with no deps, or all deps just completed | `amp_dispatch_task` |
| `in_progress` | Agent working it | `amp_dispatch_task` called | monitor |
| `review` | Agent submitted for review | agent calls `action_mark_review` | review, then complete |
| `completed` | Done | `amp_complete_task` | nothing — downstream auto-unblocks |
| `blocked` | Has incomplete dependencies | **set automatically** when task is created with deps | wait for deps to complete |

## The DAG rules

1. Task created with `dependency_ids` → **automatically set to `blocked`**
2. All dependencies reach `completed` → **automatically moved to `backlog`** (workable)
3. `amp_dispatch_task` on a `blocked` task → **error listing which dep IDs are incomplete**
4. `amp_complete_task` → triggers check on all downstream tasks, unblocks any whose deps are now all done

**No manual state management of blocked/unblocked. It's all automatic.**

## Key argument types

- `project_id`, `task_id`, `epic_id`, `story_id` — numeric Odoo IDs (integers)
- `priority` — "0"=low, "1"=normal, "2"=high, "3"=critical
- `dag_level` — 0=runs immediately, 1=depends on level-0 tasks, etc.
- `dependency_ids` — array of numeric task IDs this task waits on
