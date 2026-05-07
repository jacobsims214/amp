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
- Names the exact file and symbol being added or changed
- Has 3 acceptance criteria or fewer — each binary (true or false, no interpretation)
- A worker can complete it without making scope decisions

**Signs a task is too large — split it:**
- The description references multiple unrelated files
- The acceptance criteria has more than 4 bullet points
- It uses vague scope words: "implement the full X", "add all Y endpoints", "handle Z"
- A worker would need to decide what to build, not just how to build it

**How to split:**
- One file changed = one task (`Add UpdateEpic to repo.go` is separate from `Add PATCH /epics/:id to rest.go`)
- One layer = one task (model, repo, HTTP handler, and frontend component are four tasks)
- One component = one task (`CrudModal` component is separate from wiring it into `Board.tsx`)

### Description size limit — hard rule

**Worker descriptions must fit in ~20 lines.** Small models have limited context.
A description that consumes the worker's context budget before they start is a failed task.

**Never embed in a description:**
- Full file contents or large YAML/JSON/config blocks → use "see file X, copy and change Y to Z"
- Long code examples → name the function, describe the change in one sentence
- Repetition of the acceptance criteria in the description body

If you need more than 20 lines to explain a task — **split the task**.

### Description template

```
## What to do
[One clear goal. Name the file and symbol. Say what the outcome is, not how to get there.]

## Context
[Why this exists. What it connects to. 2-3 sentences max.]

## Where to look
[File paths and key symbols the worker needs to start from.]

## Acceptance criteria
[3 or fewer. Each must be a binary check — it either passes or it doesn't.]

## Gotchas
[Non-obvious traps only. Omit if none.]
```

---

## Waves and dependencies — build in phases

Work flows in waves. Each wave unblocks the next. Think in phases before creating tasks:

1. **Foundation** — types, models, interfaces that everything else depends on
2. **Implementation** — the actual work, split by file/concern, runs in parallel within a wave
3. **Wave check** — verifies the build passes and acceptance criteria are met, gates the next wave
4. **Next wave** — only unblocks after the wave check passes
5. **Code review** — at the end of the story, a proper code review of all changes

The review task blocks the next wave via `dependency_ids`. This is non-negotiable.

```
Wave 1: #10 (model), #11 (repo)          ← parallel, no deps
Wave 1 check: #12                         ← blocked by #10, #11
Wave 2: #13 (REST), #14 (actor)          ← blocked by #12
Wave 2 check: #15                         ← blocked by #13, #14
Code review: #16                          ← blocked by #15 (all story work done)
```

---

## Two kinds of review tasks

### 1. Wave check — after every implementation wave

A wave check is a lightweight verification task. It confirms the build passes and
every acceptance criterion was actually met. It does NOT do a full code review.

**Wave check description template:**
```
## Tasks in this wave
[List every implementation task ID and name from this wave]

## What to verify
- [Acceptance criterion from task #N]
- [Acceptance criterion from task #N]
- Build passes: [exact command — go build ./... or npm run build]

## How to check
Run: [build command]
Run: git diff HEAD~[N] --name-only  (to confirm only expected files changed)

## If issues found
Create a small fix task for each issue (one issue = one task), then complete this check.

## Acceptance criteria
- Build passes
- Each acceptance criterion above is met
- Either LGTM comment posted OR fix tasks created for every issue
```

### 2. Code review — once per story, after all waves complete

Every story that produces code gets **one code review task** at the end, blocked by the
final wave check. The reviewer loads the `code-reviewer` skill and does a proper
tiered review (Blocker / Significant / Suggestion) of all changes in the story.

This is the quality gate. Blockers must become fix tasks. The next story or epic wave
should not start until the code review is clean.

**Code review task name:** `Code Review: [story name]`

**Code review description template:**
```
## Story being reviewed
[Story name and ID]

## Implementation tasks completed
[List all task IDs and names from this story's implementation waves]

## How to review
1. Load the code-reviewer skill: skill("code-reviewer")
2. Run git diff to see all changes: git diff [base-branch]...HEAD
   Or for specific commits: git diff [first-impl-commit]..HEAD
3. Apply the tiered checklist from the code-reviewer skill to every changed file
4. Post findings in the standard format (Overall Verdict, Blockers, Significant, Suggestions)

## Tech stack in this story
[Go / TypeScript+React / Dockerfile / Terraform — list what applies]

## Files changed in this story
[List the files the implementation tasks touched, so the reviewer knows where to focus]

## If blockers or significant issues found
Create a fix task for each one:
- One issue = one task
- Assigned to amp-worker
- Describe the exact file, the exact problem, and the exact fix needed
- Post a comment on this review listing all fix tasks created

## Acceptance criteria
- code-reviewer skill loaded and checklist applied
- Overall verdict posted as a task comment
- Fix tasks created for every Blocker and Significant finding
- Suggestions noted but do not block completion
```

**Reviewers create fix tasks — not failures.** A review task always completes. Issues
become fix tasks in the backlog. The manager dispatches fix tasks after the review.

---

## Every task requires these three things — no exceptions

**1. `assigned_to`** — set at creation time, every task, always `"amp-worker"` unless told otherwise. This is how the kanban board shows who owns what. A task without `assigned_to` is invisible to the user during review.

**2. `dependency_ids`** — think about this during creation, not after. For every task you create, ask before submitting it: does this task need anything from another task to exist before it can start? If yes, include those task IDs in `dependency_ids`. The system sets state automatically — deps incomplete → `blocked`, all complete → `backlog`. You never set state directly.

**3. A complete description** — workers have no other context. The ticket is their entire world. Use the description template above. Every ticket needs a clear goal, the right file paths, and binary acceptance criteria.

Thin description = thin results. Workers are local LLMs — give them enough context to execute, not so much that you've done the work for them.

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
    ├── Task #N: [name]                    →  amp-worker   (ready)
    ├── Task #N: [name]                    →  amp-worker   (ready)
    ├── Task #N: Wave 1 check              →  amp-worker   (blocked by #N, #N)
    ├── Task #N: [name]                    →  amp-worker   (blocked by #N check)
    ├── Task #N: [name]                    →  amp-worker   (blocked by #N check)
    ├── Task #N: Wave 2 check              →  amp-worker   (blocked by #N, #N)
    └── Task #N: Code Review: [story name] →  amp-worker   (blocked by #N check)

Phase 1 — dispatch immediately: #N, #N
Phase 2 — unblocks when wave 1 check passes: #N, #N
Phase 3 — unblocks when wave 2 check passes: #N (code review)

Total: X tasks (Y implementation + Z wave checks + 1 code review per story)
```

Every task shows: its ID, name, assigned agent, and either `(ready)` or `(blocked by #N, ...)`.
Wave checks and code reviews must be clearly labeled.
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
