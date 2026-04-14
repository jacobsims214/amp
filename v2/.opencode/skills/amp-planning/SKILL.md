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

## Task sizing — the most important rule

**Every task must be one thing.** If you can describe a task with "and", split it.

A correctly sized task:
- Touches **one file** or **one concern**
- Can be described in a single sentence
- A worker can complete it in one focused session without making judgment calls about scope
- Has a clear, binary done state — it either passes the acceptance criteria or it doesn't

**Signs a task is too large:**
- The description has multiple `##` sections covering different files
- The acceptance criteria has more than 4-5 bullet points
- It says things like "implement the full X feature" or "add all Y endpoints"
- A worker would need to make architectural decisions to complete it

**How to split:**
- One file changed = one task (e.g. "Add UpdateEpic to repo.go" is separate from "Add PATCH /epics/:id to rest.go")
- One layer = one task (domain model change, then repo change, then REST wiring are three tasks)
- One component = one task (CrudModal component is separate from wiring it into Board.tsx)

---

## Waves and dependencies — build in phases

Work flows in waves. Each wave unblocks the next. Think in phases before creating tasks:

1. **Foundation** — types, models, interfaces that everything else depends on
2. **Implementation** — the actual work, split by file/concern, can run in parallel within a wave
3. **Review** — a dedicated review task that gates the next wave
4. **Next wave** — only unblocks after review passes

**Every implementation wave must be followed by a review task before the next wave starts.**

The review task blocks the next wave via `dependency_ids`. This is non-negotiable.

```
Wave 1: #10 (model), #11 (repo)          ← parallel, no deps
Wave 1 review: #12                        ← blocked by #10, #11
Wave 2: #13 (REST), #14 (actor)          ← blocked by #12 (review)
Wave 2 review: #15                        ← blocked by #13, #14
Wave 3: #16 (frontend types)             ← blocked by #15 (review)
...
```

---

## Review tasks — mandatory after every implementation wave

**Any task that produces code, config, migrations, or other artifacts must be followed by a review task.**

The review task:
- Is blocked by all implementation tasks in that wave
- Runs `git diff` to see exactly what changed
- Verifies each acceptance criterion was actually met
- Runs the build/test command to confirm it passes
- Posts a comment with findings

**Critically: review tasks can and should create new fix tasks.**

If the reviewer finds issues, it does NOT fail or block itself. Instead it:
1. Creates small, targeted fix tasks in the backlog (one issue = one task)
2. Posts a comment listing what was found and what fix tasks were created
3. Completes itself — the fix tasks are now in the backlog for the next dispatch cycle

This means the manager must check `ready_to_dispatch` after every review completes — there may be new fix tasks waiting.

Review task description template:
```
## What to review
[List every implementation task in this wave with their task IDs]

## What to check
[Specific things to verify — file names, function signatures, SQL parameter counts, build commands]

## How to verify
[Exact commands: go build ./..., npm run build, git diff HEAD, etc.]

## If issues found
Create a new task for each issue found:
- Use amp_create_task with the same project_id, epic_id, story_id
- Make it small and targeted — one issue = one task
- Set dependency_ids if the fix depends on other work
- Post a comment on THIS review task listing all fix tasks created

## Acceptance criteria
- All items verified
- Build/tests pass
- Either "LGTM" comment posted OR fix tasks created for every issue found
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
- Does it depend on a review task passing before it should start?

If yes to any of these — add the dependency, regardless of which epic or story it's in.

---

## Plan presentation — show everything

Present before dispatching. The user needs to see assigned agents and blockers to review meaningfully.

```
EPIC: [name]
└── Story: [name]
    ├── Task #N: [name]           →  amp-worker   (ready)
    ├── Task #N: [name]           →  amp-worker   (ready)
    ├── Task #N: REVIEW wave 1    →  amp-worker   (blocked by #N, #N)
    ├── Task #N: [name]           →  amp-worker   (blocked by #N review)
    └── Task #N: REVIEW wave 2    →  amp-worker   (blocked by #N)

Phase 1 — dispatch immediately: #N, #N
Phase 2 — unblocks when phase 1 review passes: #N, #N
Phase 3 — unblocks when phase 2 review passes: #N

Total: X tasks (Y implementation + Z reviews)
```

Every task shows: its ID, name, assigned agent, and either `(ready)` or `(blocked by #N, ...)`.
Review tasks must be clearly labeled as reviews.
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

After dispatch: monitor with `amp_list_tasks`. When workers complete, blocked tasks auto-unblock and appear in `ready_to_dispatch`. Dispatch those. **After a review task completes, always check `ready_to_dispatch` — the reviewer may have created new fix tasks.** Repeat until `ready_to_dispatch` and `in_progress` are both empty.

Load `amp-mcp` if you need exact tool argument shapes.
