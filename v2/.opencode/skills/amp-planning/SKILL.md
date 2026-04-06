---
name: amp-planning
description: Full protocol for planning, creating the task hierarchy, getting approval, and dispatching workers — including the hard approval gate
---

# AMP Planning Skill

---

## Before you plan

Read `.amp.json` to get the project ID. If it doesn't exist, load `amp-init` first.

Search the KB before creating anything:
```
amp_kb_search(project_id=PROJECT_ID, query="<the user's request>")
```
Existing architecture docs and past decisions should shape your plan. Load `amp-kb` if you need search guidance.

---

## The hierarchy — enforced by the API

```
Project → Epic → Story → Task
```

Create in order. The API rejects out-of-order creation.

```
amp_create_epic   {project_id, name, description}
amp_create_story  {project_id, epic_id, name, acceptance_criteria}
amp_create_task   {project_id, epic_id, story_id, name, description,
                   acceptance_criteria, assigned_to, dependency_ids?}
```

---

## Every task requires these three things — no exceptions

**1. `assigned_to`** — set at creation time, every task, always `"amp-worker"` unless told otherwise. This is how the kanban board shows who owns what. A task without `assigned_to` is invisible to the user during review.

**2. `dependency_ids`** — think about this during creation, not after. For every task you create, ask before submitting it: does this task need anything from another task to exist before it can start? If yes, include those task IDs in `dependency_ids`. The system sets state automatically — deps incomplete → `blocked`, all complete → `backlog`. You never set state directly.

**3. A complete description** — workers have no other context. The ticket is their entire world. Write it as a self-contained brief:

```
## What to do
[Exact steps. Specific files, commands, interfaces.]

## Context
[Why this exists. What the system looks like. Relevant decisions.]

## Where to look
[File paths, module names, key functions.]

## Acceptance criteria
[Concrete and checkable — same as the acceptance_criteria field.]

## Gotchas
[Non-obvious traps, edge cases, known failures.]
```

Thin description = thin results. Workers are local LLMs — give them everything they need in the ticket.

---

## Dependencies — think cross-boundary

`dependency_ids` accepts task IDs from any epic or story. The most common mistake is only thinking about dependencies within the same story. Ask for every task:

- Does it call an API another task creates?
- Does it use middleware, auth, shared utilities another task sets up?
- Does it integrate two things that each come from different tasks?
- Does it need a schema, config, or interface another task defines?

If yes to any of these — add the dependency, regardless of which epic or story it's in.

---

## Plan presentation — show everything

Present before dispatching. The user needs to see assigned agents and blockers to review meaningfully.

```
EPIC: [name]
└── Story: [name]
    ├── Task #N: [name]  →  amp-worker   (ready)
    ├── Task #N: [name]  →  amp-worker   (ready)
    └── Task #N: [name]  →  amp-worker   (blocked by #N, #N)

Phase 1 — dispatch immediately: #N, #N
Phase 2 — unblocks when phase 1 completes: #N

Total: X tasks across Y epics
```

Every task shows: its ID, name, assigned agent, and either `(ready)` or `(blocked by #N, ...)`.
If a task has no agent or no blocker status shown — fix it before presenting.

---

## ⛔ Stop after presenting — do not dispatch

Wait for explicit approval. Nothing further until the user says so.

Approval: "approved", "go ahead", "yes", "do it", or similar.

Changes requested → update → present again → wait again.

---

## Dispatch — only after approval

For every task in `ready_to_dispatch`, do both steps in this order:

**Step 1 — dispatch each task** (marks it in_progress on the board):
```
amp_dispatch_task(task_id=ID, agent_id="amp-worker")
```

**Step 2 — spawn workers in a single message** (runs them in parallel):
```
task(prompt="amp-worker. Task ID: {id}. Project ID: {project_id}.", subagent_type="amp-worker")
task(prompt="amp-worker. Task ID: {id}. Project ID: {project_id}.", subagent_type="amp-worker")
```

Step 1 must happen before step 2. This is what shows live progress on the board.

After dispatch: monitor with `amp_list_tasks`. When workers complete, blocked tasks auto-unblock and appear in `ready_to_dispatch`. Dispatch those. Repeat until `ready_to_dispatch` and `in_progress` are both empty.

Load `amp-mcp` if you need exact tool argument shapes.
