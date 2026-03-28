---
name: amp-mcp
description: Complete reference for all amp-odoo MCP tool names and their arguments
---

# AMP MCP Tool Reference

All tools are on the `amp-odoo` MCP server. Use these exact names.

## Project
- `amp_list_projects` — list all projects
- `amp_get_project` {project_id} — get by numeric ID
- `amp_get_project_by_code` {code} — get by code string
- `amp_create_project` {name, code, description}
- `amp_update_project` {project_id, name?, description?, state?}
- `amp_get_dashboard` {project_id} — task counts by state, progress, active agents

## Epics
- `amp_create_epic` {name, project_id, description?, priority?}
- `amp_get_epic` {epic_id}
- `amp_list_epics` {project_id}
- `amp_update_epic_dag` {epic_id, dag_json}

## Stories
- `amp_create_story` {name, project_id, epic_id, description?, acceptance_criteria?}
- `amp_get_story` {story_id}
- `amp_list_stories` {epic_id}

## Tasks
- `amp_list_tasks` {project_id?, epic_id?, story_id?, state?} — **USE THIS to see actual task data**. Returns tasks with id, name, state, is_ready, agent_id, story/epic names. Filter by any combination. Omit state to see all.
- `amp_create_task` {name, project_id, epic_id?, story_id?, description?, acceptance_criteria?, priority?, dag_level?, dependency_ids?[]}
- `amp_get_task` {task_id} — full task detail including description, acceptance_criteria, context_data
- `amp_update_task` {task_id, name?, description?, state?, agent_id?, context_data?}
- `amp_dispatch_task` {task_id, agent_id} — sets state=in_progress, writes context_data, records dispatch_time
- `amp_complete_task` {task_id} — sets state=completed, triggers dependent task unblocking
- `amp_block_task` {task_id, reason} — sets state=blocked
- `amp_list_ready_tasks` {project_id} — shortcut: tasks where state=ready AND is_ready=true (dependency-free, ready to dispatch now)
- `amp_add_task_comment` {task_id, body} — post to ticket chatter

## Task states — what they mean

| state | is_ready | meaning | action |
|-------|----------|---------|--------|
| backlog | true | No deps, not yet dispatched | **Dispatch now** |
| backlog | false | Has incomplete dependencies | Wait for deps |
| ready | true | Explicitly moved to ready | **Dispatch now** |
| in_progress | — | Agent currently working it | Monitor |
| review | — | Agent submitted for review | Review or complete |
| completed | — | Done | Nothing |
| blocked | — | Agent couldn't proceed | Investigate |

**IMPORTANT**: `backlog` + `is_ready=true` means the task is available to dispatch.
You do NOT need to move tasks from backlog to ready — `amp_dispatch_task` works directly on backlog tasks.
`amp_list_ready_tasks` only returns tasks already in state=ready. To find ALL dispatchable work use:
`amp_list_tasks(project_id=X, state="backlog")` then filter for `is_ready=true`.

## Knowledge Base
- `amp_create_kb_entry` {title, content, project_id, entry_type?, epic_id?, story_id?, task_id?, tags?[], created_by_agent?}
- `amp_search_kb` {query, project_id?, limit?}
- `amp_get_project_kb` {project_id, limit?}
- `amp_get_task_kb` {task_id} — KB entries matching task's project/epic/story/task chain

## Notes
- `project_id`, `task_id` etc. are numeric Odoo IDs
- `priority`: "0"=low, "1"=normal, "2"=high, "3"=critical
- `dag_level`: 0=no deps, 1=depends on level-0, etc.
- `dependency_ids`: array of task IDs that must complete before this task
- `entry_type`: "finding", "decision", "howto", "issue", "reference", "context"
