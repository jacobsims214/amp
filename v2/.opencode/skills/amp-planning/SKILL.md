---
name: amp-planning
description: How to decompose work into epics/stories/tasks, set up DAG dependencies including cross-epic and cross-story blocking, and dispatch workers in parallel
---

# AMP Planning Skill

## .amp.json — the project anchor file

Every workspace has a `.amp.json` in its root directory that maps it to a project in amp-api.

```json
{
  "project_id": 1,
  "project_name": "My Project",
  "project_code": "my-project",
  "amp_api": "http://localhost:8000"
}
```

**Manager**: Read this first with `cat .amp.json`. If it doesn't exist, create the project
via `amp_create_project` and write the file. This is the source of truth for `project_id`.

**Worker**: Read this to get the `project_id` if you need broader context beyond your task.

**Commit this file** — everyone working in the directory uses the same project.

---

## The hierarchy — strictly enforced

```
Project → Epic → Story → Task
```

This is not optional. The API rejects tasks without both `epic_id` AND `story_id`. It
rejects stories without `epic_id`. No orphaned tasks or stories.

Create in order — you need each ID before you can reference it:

```
1. amp_create_project {name, code}           → project_id
2. amp_create_epic    {project_id, name}     → epic_id
3. amp_create_story   {project_id, epic_id, name, acceptance_criteria}  → story_id
4. amp_create_task    {project_id, epic_id, story_id, name, description, acceptance_criteria, assigned_to}
```

The `epic_id` on a task MUST match the `epic_id` on its story. The API enforces this.

**Important:** `dependency_ids` reference task IDs from ANY epic or story in the project.
A task in "Epic: Payments" can and should block a task in "Epic: Dashboard" if the
dashboard genuinely needs payment data to exist first. The hierarchy organises ownership
and display — dependencies express execution order, regardless of hierarchy.

---

## Writing task descriptions — the ticket IS the agent's prompt

The worker agent reads nothing except the ticket. Write as if you are writing a
complete, standalone instruction set. The agent has no other context.

```markdown
## What to do
[Exact steps — be specific about files, commands, and interfaces]

## Context
[Why this exists. What decision was already made. What the system looks like now.]

## Where to look
[File paths, module names, relevant functions]

## Acceptance criteria (same as the acceptance_criteria field)
[Concrete, checkable conditions. The worker MUST verify each one before completing.]

## Gotchas
[Non-obvious traps, known edge cases, things that have already failed]
```

**If the description and acceptance_criteria fields are thin, the worker will produce thin results.**

---

## Acceptance criteria — must be verifiable, not vague

Bad:  "auth works"
Good: "POST /auth/login with valid credentials returns 200 and a JWT. POST with
       invalid credentials returns 401. Protected routes return 403 without a token."

---

## assigned_to — set this at planning time

Every task must have an `assigned_to` field set when it is created. This is a free-text
string naming which sub-agent should work the task. It is shown on the kanban board
**before dispatch** so the user can review and correct any assignments before approving.

```
amp_create_task {
  ...
  assigned_to: "amp-worker"    ← REQUIRED — set this on every task at plan time
}
```

The `assigned_to` field is separate from `agent_id`:
- `assigned_to` — planned assignment, set by manager at create time, visible on board
- `agent_id` — actual worker, set automatically at dispatch time by the system

When presenting the plan, include the planned assignee for every task:

```
Task #3: Build login API endpoint    → amp-worker
Task #4: Build login UI form         → amp-worker-frontend   (if you have specialised workers)
Task #8: Wire dashboard to auth      → amp-worker  [BLOCKS ON #3]
```

If the user wants to change an assignment before dispatch, use:
```
amp_update_task {task_id: X, assigned_to: "amp-worker-other"}
```

---

## DAG dependencies — the critical planning step

`dependency_ids` is an array of task IDs that must all complete before this task
can be dispatched. The actor sets state automatically:
- no deps → `backlog` (ready immediately)
- any incomplete dep → `blocked` (auto-moves to `backlog` when all deps complete)

You never set state directly. Never.

### Step: Dependency sweep (do this before presenting the plan)

After creating all tasks but BEFORE presenting the plan to the user, do a
**dependency sweep** across the entire plan. For every task you created, ask:

> "Does this task need output, data, infrastructure, or decisions from any other
> task — in ANY epic or story — before it can meaningfully start?"

Common cross-boundary dependency patterns to look for:

**Cross-epic dependencies:**
- A "Dashboard" epic task that renders auth-protected data → blocks on the
  "Auth" epic task that creates the auth middleware
- An "Email Notifications" epic task that hooks into user lifecycle events → blocks
  on the "User Registration" epic task that defines those events
- Any epic that consumes a shared service, schema, or API → blocks on the epic
  that creates that shared resource

**Cross-story dependencies (same epic):**
- A "UI story" task that calls an endpoint → blocks on the "API story" task that
  creates that endpoint
- A "Testing story" task → blocks on the implementation story tasks it tests
- Any "Integration story" task that wires two subsystems together → blocks on both
  subsystem stories' completion tasks

**Signal words that indicate a dependency:**
- "uses", "calls", "connects to", "relies on", "needs", "requires", "integrates with"
- "after X is done", "once X exists", "assuming X is available"
- Any task whose description includes a file, function, endpoint, or schema that
  another task will create

### How to set cross-boundary dependencies

`dependency_ids` accepts task IDs from ANY epic or story. You created all the tasks
and have all the IDs. Wire them:

```
# Task in Epic 2, Story B depends on task from Epic 1, Story A:
amp_create_task {
  project_id: X,
  epic_id: epic2_id,       ← belongs to Epic 2
  story_id: story_b_id,    ← belongs to Story B
  name: "Wire dashboard to auth middleware",
  dependency_ids: [task_id_from_epic1]   ← ← ← cross-epic dependency
}
```

The task belongs to Epic 2 / Story B for ownership purposes. It is blocked by a
task in Epic 1 for execution-order purposes. These are independent concepts.

### Dependency sweep checklist

For each task in your plan, check these questions and add deps if any answer is yes:

```
□ Does this task call an API endpoint created in another story/epic?
  → Block on the task that creates that endpoint

□ Does this task use a database schema, model, or migration from another story/epic?
  → Block on the task that creates that schema/migration

□ Does this task implement auth/auth middleware checks?
  → Block on the task that creates the auth system

□ Does this task send emails, notifications, or events?
  → Block on the task that sets up the email/notification/event service

□ Does this task use a shared utility, library, or configuration created elsewhere?
  → Block on the task that creates it

□ Does this task integrate two subsystems?
  → Block on BOTH subsystem tasks it connects

□ Is this task a deployment or environment setup step?
  → Block on all tasks that produce artifacts it deploys
```

### Show cross-boundary deps explicitly in the plan presentation

When presenting the plan to the user, call out cross-epic and cross-story deps clearly:

```
EPIC: User Authentication
└── Story: Login & Session
    ├── Task #3: Build login API endpoint          (no deps)
    └── Task #4: Build login UI form               (no deps)

EPIC: Dashboard & Analytics
└── Story: Metrics Overview
    ├── Task #7: Design KPI card component         (no deps)
    └── Task #8: Wire KPI cards to metrics API     [BLOCKS ON #3 — cross-epic]
                                                    └── needs login API before it
                                                        can test authenticated routes

EPIC: Notifications
└── Story: Email Notifications
    └── Task #12: Hook emails to user lifecycle    [BLOCKS ON #3 — cross-epic]
                                                    └── needs user auth events

Phase 1 (immediate): #3, #4, #7
Phase 2 (after #3):  #8, #12
```

This makes the dependency graph visible to the user before they approve.
If you can't explain WHY a cross-boundary dep exists, reconsider whether it's real.

---

## The manager dispatch loop

1. Create the full hierarchy: epics → stories → tasks
2. Do the dependency sweep — wire all cross-boundary deps
3. Present the plan (with cross-boundary deps called out) and wait for approval
4. Call `amp_list_tasks` — dispatch everything in `ready_to_dispatch` simultaneously:

```
# All in one message — runs in parallel
task(prompt="amp-worker. Task ID: 1. Project ID: X.", subagent_type="amp-worker")
task(prompt="amp-worker. Task ID: 2. Project ID: X.", subagent_type="amp-worker")
```

5. As workers complete, blocked tasks auto-unblock and appear in `ready_to_dispatch`
6. Call `amp_list_tasks` again → dispatch new `ready_to_dispatch` → repeat
7. Done when `ready_to_dispatch=[]` AND `in_progress=[]`

---

## Monitoring a running task

```
amp_get_ticket_history(task_id=X)
```

Returns the full log: created → dispatched → comments → completed.
Use this to understand what happened on any ticket.

---

## What happens if you miss a dependency

If a worker starts a task that needed something from an incomplete task in another epic,
the worker will fail or produce broken work. The `amp_block_task` tool exists for this
but it's a recovery tool, not a planning tool. Get the deps right at planning time.

The DAG view at `/project/:id/dag` visualises all deps including cross-epic edges.
Use it to sanity-check your plan before dispatching.
