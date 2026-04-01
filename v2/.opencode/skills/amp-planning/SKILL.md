---
name: amp-planning
description: Full protocol for planning, creating the task hierarchy, getting approval, and dispatching workers — including the hard approval gate
---

# AMP Planning Skill

---

## Step 0 — Read or create .amp.json

```bash
cat .amp.json
```

**If it exists:** use `project_id` for all MCP calls. Do not create a new project.

**If it doesn't exist:** ask the user for a name and short code, then:
```
amp_create_project {name, code, description}
```
Write `.amp.json`:
```json
{
  "project_id": <id>,
  "project_name": "<name>",
  "project_code": "<code>",
  "amp_api": "http://localhost:8000"
}
```
Commit this file — everyone working in the directory uses the same project.

---

## Step 0.5 — Search the KB before planning

Before creating anything, search the KB for relevant prior work:

```
amp_kb_search(project_id=PROJECT_ID, query="<the user's request>")
```

Existing architecture docs and past decisions should inform your plan.
See `amp-kb` skill for search guidance. Load it: `skill("amp-kb")`

---

## The hierarchy — enforced by the API

```
Project → Epic → Story → Task
```

Tasks require both `epic_id` AND `story_id`. Stories require `epic_id`.
The API rejects violations. Create in order:

```
1. amp_create_epic    {project_id, name, description}
2. amp_create_story   {project_id, epic_id, name, acceptance_criteria}
3. amp_create_task    {project_id, epic_id, story_id, name, description,
                       acceptance_criteria, assigned_to, dependency_ids?}
```

---

## Task descriptions — the ticket IS the worker's prompt

Workers have no other context. Write each description as a complete standalone brief:

```
## What to do
[Exact steps. Specific files, commands, interfaces.]

## Context
[Why this exists. What decisions were made. What the system looks like now.]

## Where to look
[File paths, module names, key functions]

## Acceptance criteria
[Concrete and checkable — same as the acceptance_criteria field]

## Gotchas
[Non-obvious traps, edge cases, things that have already failed]
```

Thin description = thin results.

---

## assigned_to — required on every task

Set `assigned_to` on every task at planning time (e.g. `"amp-worker"`). This shows
on the kanban board so the user can review and correct assignments before approving.

---

## DAG dependencies — including cross-epic and cross-story

`dependency_ids` accepts task IDs from any epic or story.
After creating all tasks, do a **dependency sweep**:

For each task ask: "Does this need output, infrastructure, or a decision from any
other task — in any epic or story — before it can start?"

Patterns to look for:
- Calls an API created in another story → block on that task
- Uses auth middleware → block on the auth task
- Integrates two subsystems → block on BOTH
- Uses shared utilities from another epic → block on that utility task

Signal words: "uses", "calls", "relies on", "needs X to exist", "after X is done"

Call out cross-epic and cross-story deps in the plan presentation:
```
Task #8: Wire dashboard to auth  [BLOCKS ON #3 — cross-epic, needs auth middleware]
```

The actor sets state automatically: deps incomplete → `blocked`, all done → `backlog`.
You never set state directly.

---

## Plan presentation format

```
Here is the plan. Please review and reply "approved" to dispatch, or tell me what to change.

EPIC: [name]
└── Story: [name]
    ├── Task #N: [name]  → amp-worker           (no deps — ready immediately)
    ├── Task #N: [name]  → amp-worker           (no deps — ready immediately)
    └── Task #N: [name]  → amp-worker  [BLOCKS ON #N, #N — cross-epic]

Phase 1 (dispatch immediately): #N, #N
Phase 2 (unblocks after phase 1): #N

Total: X tasks, Y epics
```

---

## ⛔ STOP HERE — DO NOT DISPATCH YET

After presenting the plan, **stop and wait**. Output nothing further.

You must receive explicit approval before dispatching any workers.

Approval: "approved", "go ahead", "looks good", "yes", "do it", or similar.

If the user asks for changes → make them → present again → wait again.
Never dispatch without explicit approval for the current plan.

---

## Dispatch (only after approval)

For each task in `ready_to_dispatch`, do **both steps** in this exact order:

**Step 1 — Call `amp_dispatch_task` for each task** (moves it to `in_progress` on the board):
```
amp_dispatch_task(task_id={id}, agent_id="amp-worker")
```

**Step 2 — Spawn the worker sub-agent** (in a single message for all tasks — runs parallel):
```
task(prompt="amp-worker. Task ID: {id}. Project ID: {project_id}.", subagent_type="amp-worker")
task(prompt="amp-worker. Task ID: {id}. Project ID: {project_id}.", subagent_type="amp-worker")
```

`amp_dispatch_task` must happen before the worker spawns. This is what makes the task
show as `in_progress` on the kanban board while the agent is working. Without it, the
board stays at `backlog` until the worker completes — the user sees no live progress.

Monitor: when workers complete, blocked tasks auto-unblock.
Call `amp_list_tasks` → dispatch new `ready_to_dispatch` → repeat.
Done when `ready_to_dispatch=[]` AND `in_progress=[]`.

---

## MCP tool reference

```
amp_create_project / amp_list_projects / amp_get_project
amp_create_epic    / amp_list_epics    / amp_get_epic
amp_create_story   / amp_list_stories  / amp_get_story
amp_create_task    / amp_list_tasks    / amp_get_task
amp_update_task    / amp_dispatch_task / amp_complete_task
amp_block_task     / amp_set_task_state
amp_add_task_comment / amp_get_ticket_history
amp_reset_project  / amp_delete_task  / amp_delete_epic
amp_kb_search      / amp_kb_get       ← see amp-kb skill for full KB reference
```
