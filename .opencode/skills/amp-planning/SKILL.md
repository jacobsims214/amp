---
name: amp-planning
description: How to decompose work into epics/stories/tasks, write tickets, set up DAG dependencies, and dispatch workers in parallel
---

# AMP Planning Skill

Use this skill whenever you need to plan and create work in Odoo.
No time estimates. No hours. Just work breakdown and dependencies.

## Workflow

```
Decompose → Write tickets in Odoo → Present to human → Get approval → Dispatch all ready tasks in parallel → Monitor
```

---

## Step 1: Decompose

- **Epic**: one per major initiative
- **Stories**: user-facing outcomes, 3-8 per epic
- **Tasks**: specific technical work items, 2-6 per story

Identify which tasks can run in parallel and which must be sequential.

---

## Step 2: Write tickets (in this order)

```
1. amp_create_epic  → epic_id
2. amp_create_story × N → story_ids
3. amp_create_task × N  → task_ids  (pass dependency_ids to wire the DAG)
4. amp_update_epic_dag  → store DAG as JSON string
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
- `dag_level`: 0=runs immediately, 1=waits for level-0, 2=waits for level-1, etc.
- Tasks with `is_ready=true` and `state=ready` are dispatchable right now

---

## Step 3: Present the plan

```
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

## Step 4: Find what to dispatch

**Never use dashboard counts to decide what to dispatch. Always read actual ticket data.**

```
# See every task and its real state
amp_list_tasks(project_id=PROJECT_ID)

# Or narrow to specific epic/story
amp_list_tasks(epic_id=EPIC_ID)
amp_list_tasks(story_id=STORY_ID)
```

Tasks that can be dispatched right now: `state=backlog` AND `is_ready=true`.
You do NOT need to move tasks to "ready" first — dispatch works on backlog tasks directly.

```
# Find all immediately dispatchable work
tasks = amp_list_tasks(project_id=X, state="backlog")
dispatchable = [t for t in tasks if t["is_ready"]]
```

## Step 5: Dispatch (after approval)

For each dispatchable task:

```
# 1. Post a planning comment with manager context
amp_add_task_comment(task_id=X, body="""
Manager note:
Part of [story name] in [epic name].
Key context: [decisions, constraints, anything the worker needs to know]
After this completes, it unblocks: [dependent task names if any]
""")

# 2. Mark dispatched in Odoo (sets state=in_progress, writes full context_data)
amp_dispatch_task(task_id=X, agent_id="amp-worker")

# 3. Spawn the worker — pass ONLY the task_id, everything else is on the ticket
task(
    description="AMP task {task_id}: {task_name}",
    prompt="You are an amp-worker. Odoo task ID: {task_id}. Project ID: {project_id}. Call amp_get_task to read your instructions.",
    subagent_type="amp-worker"
)
```

**Dispatch ALL dispatchable tasks simultaneously — not one at a time.**

## Step 6: Monitor

```
loop:
  # Check actual ticket states — not just counts
  amp_list_tasks(project_id=X)
  
  # Find newly unblocked tasks (deps just completed)
  newly_ready = [t for t in tasks if t["state"] == "backlog" and t["is_ready"]]
  → dispatch any newly ready tasks immediately
  
  # Check for problems
  blocked = [t for t in tasks if t["state"] == "blocked"]
  → alert user with task names and ids if any are blocked
  
  → done when all tasks are completed
```

When a task completes, Odoo automatically re-evaluates `is_ready` on dependent tasks.
Check `amp_list_tasks` after each completion to find what just became dispatchable.
