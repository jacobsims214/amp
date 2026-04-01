---
name: amp-planning
description: How to decompose work into epics/stories/tasks, write tickets, set up DAG dependencies, and dispatch workers in parallel
---

# AMP Planning Skill

Use this skill whenever you need to plan and create work in Odoo.

---

## The non-negotiable rules

**1. Every ticket must use the project_id from `.amp.json`. Always. No exceptions.**
Read `.amp.json` first. Use that `project_id` for every single MCP call.

**2. The hierarchy is: Epic → Story → Task. You cannot skip levels.**
- Tasks MUST have a `story_id` — no orphan tasks
- Stories MUST have an `epic_id` — no orphan stories
- Epics MUST have a `project_id` from `.amp.json`

**3. Create in order — you need the IDs before you can use them:**
```
1. amp_create_epic(project_id=PROJECT_ID, ...)  → epic_id
2. amp_create_story(project_id=PROJECT_ID, epic_id=epic_id, ...)  → story_id
3. amp_create_task(project_id=PROJECT_ID, epic_id=epic_id, story_id=story_id, ...)
```

If you try to create a task without `story_id` and `epic_id`, the MCP will return an error.

---

## Workflow

```
Read .amp.json → Decompose → Create in Odoo → Present to human → Approval → Dispatch → Monitor
```

---

## Step 1: Read .amp.json

Before anything else:
```
project_id = .amp.json["project_id"]
project_code = .amp.json["project_code"]
```
Every subsequent MCP call uses this `project_id`. Never use a different one.

---

## Step 2: Decompose

- **Epic**: one per major initiative
- **Stories**: user-facing outcomes, 3-8 per epic
- **Tasks**: specific technical work items, 2-6 per story

Identify which tasks can run in parallel and which must be sequential.

---

## Step 3: Create in Odoo (strict order)

```
# 1. Create the epic
result = amp_create_epic(
    name="...",
    project_id=PROJECT_ID,   ← from .amp.json
    description="..."
)
epic_id = result["id"]

# 2. Create stories (one or more per epic)
result = amp_create_story(
    name="...",
    project_id=PROJECT_ID,   ← from .amp.json
    epic_id=epic_id,          ← from step 1
    description="...",
    acceptance_criteria="..."
)
story_id = result["id"]

# 3. Create tasks (one or more per story)
result = amp_create_task(
    name="...",
    project_id=PROJECT_ID,   ← from .amp.json
    epic_id=epic_id,          ← from step 1
    story_id=story_id,        ← from step 2
    description="...",        ← FULL instructions for the worker
    acceptance_criteria="...",
    dag_level=0,
    dependency_ids=[]         ← list of task IDs this must wait on
)
```

### Task description — be explicit, this IS the worker's instruction

```
## What to do
[Specific steps. What to run, what to open, what to change.]

## Context
[Why this needs doing. What system is involved. Relevant prior decisions.]

## Where to look
[File paths, module names, commands.]

## What correct looks like
[Concrete end state.]

## Gotchas
[Anything non-obvious that will trip up the worker.]
```

### Acceptance criteria — specific and verifiable

```
- [Concrete checkable condition 1]
- [Concrete checkable condition 2]
- Findings posted as ticket comment before marking complete
```

### DAG dependencies

- `dependency_ids`: list of task IDs that must complete first
- `dag_level`: 0=runs immediately, 1=waits for level-0, 2=waits for level-1
- A task with `dependency_ids` is **automatically set to blocked** by Odoo
- When all deps complete, Odoo **automatically moves it to backlog**

---

## Step 4: Present the plan

```
PROJECT: [name] (ID: X)

EPIC: [name]
├── Story 1: [name]
│   ├── Task 1.1  dag_level=0  (runs immediately)
│   └── Task 1.2  dag_level=1  (waits for 1.1)
└── Story 2: [name]  ← parallel with Story 1
    └── Task 2.1  dag_level=0

Phase 1 (immediate): Task 1.1, Task 2.1
Phase 2 (after 1.1): Task 1.2
```

Wait for explicit human approval before dispatching.

---

## Step 5: Dispatch (after approval)

Tasks in `state=backlog` are workable right now. Tasks in `state=blocked` are waiting on deps.

```
# 1. Post a manager planning note on the ticket
amp_add_task_comment(task_id=X, body="""
Manager note:
Part of [story name] in [epic name].
Project: [name] (ID: PROJECT_ID)
Context: [decisions, constraints, anything the worker needs]
Unblocks: [dependent task names if any]
""")

# 2. Dispatch in Odoo
amp_dispatch_task(task_id=X, agent_id="amp-worker")

# 3. Spawn worker — pass ONLY task_id and project_id
task(
    description="AMP task {task_id}: {task_name}",
    prompt="You are an amp-worker. Odoo task ID: {task_id}. Project ID: {PROJECT_ID}. Call amp_get_task to read your instructions.",
    subagent_type="amp-worker"
)
```

**Dispatch ALL backlog tasks simultaneously.**

---

## Step 6: Monitor

```
loop:
  tasks = amp_list_tasks(project_id=PROJECT_ID)

  backlog  = [t for t in tasks if t["state"] == "backlog"]   → dispatch all
  active   = [t for t in tasks if t["state"] == "in_progress"]
  waiting  = [t for t in tasks if t["state"] == "blocked"]   → auto-unblocks when deps complete
  done     = [t for t in tasks if t["state"] == "completed"]

  → done when all tasks are completed
```

---

## Story and epic lifecycle — manager owns this

Workers never change story or epic state.

Auto-progression:
- First task dispatched → story + epic auto-move to `in_progress`
- All tasks in a story complete → story auto-completes
- All stories in an epic complete → epic auto-completes

Manual override only when something went wrong:
- `amp_set_story_state` {story_id, state, reason?}
- `amp_set_epic_state` {epic_id, state, reason?}
