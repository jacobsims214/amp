---
name: amp-planning
description: Full protocol for planning, creating the task hierarchy, getting approval, and dispatching workers — including the hard approval gate
---

# AMP Planning Skill

---

## AMP is the only tracking mechanism

Every task, plan, and piece of progress in this system lives on the AMP board — epics, stories,
tasks, and their comments. There is no other planning surface. Never use a built-in tool like
`todowrite` (you shouldn't even have it — it's denied on this agent) or any ad-hoc list as a
substitute or supplement for AMP tickets. If work isn't represented as a task on the board, it
isn't tracked, and the user reviewing your session has no visibility into it. When in doubt,
create the ticket first, then do the work against it.

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

### The one-sentence diff test — check this before anything else

Before writing a ticket, you must be able to state the task's entire outcome in one sentence. If
you can't, the task hasn't been decomposed enough yet — split it further, even if it would still
pass the file-count and line-count rules below. This is independent of and in addition to those
rules: a task can touch exactly one file, stay under 20 lines, and still be too complex if its
outcome can't be stated in one sentence. (Adapted from Anthropic's Claude Code guidance — "if you
could describe the diff in one sentence, skip the plan" — inverted here as a decomposition test
rather than a skip-planning test.)

Example of a task that passes the file/line limits but fails this test: "Update the KB skill's
recency section" sounds like one file, one concern — but if satisfying it actually requires
deciding on wording for three unrelated sub-rules (when to re-search, what threshold to use, how
to phrase the staleness warning), that's not one sentence, it's three decisions bundled together.
Split it into three tickets.

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
- One file changed = one task (`Add UpdateEpic to repo.go` is separate from `Add PATCH /epics/:id to rest.go`) — this isn't only about avoiding concurrent edits to the same file (see the file conflict check below): a single worker applying "the same instruction" across multiple files risks making inconsistent decisions per file even when nothing fails outright — Cognition's "actions carry implicit decisions" principle, applied within one task instead of across parallel tasks.
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

### Describe intent, not implementation — never write the artifact for the worker

A ticket states the required file/symbol, the exact non-negotiable values (a specific model ID,
a specific permission action, a specific config key), and 3-6 behavioral bullets the worker must
cover in its own words. It never contains a finished file, a finished function body, or prose
written for the worker to transcribe verbatim.

**Test before writing any description:** could the worker literally copy-paste something from
this ticket and be done? If yes, you did the worker's job — rewrite the ticket as requirements,
not output.

**Exception — literal content with a single correct answer is not "doing the worker's job."**
The copy-paste test above is about decision-bearing content: prose, function bodies, anything
where multiple different-but-valid outputs could satisfy the ticket. It does not apply to a value
that has exactly one correct answer with no acceptable variation — an exact version string, a
specific config key/value, a single approved sentence with no room for rewording. Handing over
that literal content isn't doing the worker's job; it's what makes the task mechanical and safe
to route to any tier, including the docs tier. The test: **single correct answer → give it
literally. Multiple valid outputs → describe intent instead, per the rule above.**

- Wrong: "Create file X with this content: ```<full file>```"
- Right: "Create file X. Required: field A = exact-value, field B = exact-value. The body must
  cover: [bullet], [bullet], [bullet]."
- Also right (literal-content exception): "Change line 12 of config.yaml from `timeout: 30` to
  `timeout: 60`." — exactly one correct edit, nothing left to decide, safe to state verbatim.

This applies to KB references too — never point a worker at a large shared doc and say "copy the
relevant block." Give them the 4-6 facts they need inline; save the doc for the "why" a worker
doesn't need to complete the task.

### Description template

```
## What to do
[One clear goal. Name the file and symbol. Say what the outcome is, not how to get there.]

## Context
[Why this exists. What it connects to. 2-3 sentences max.]

## Where to look
[File paths and key symbols the worker needs to start from.]

## Files touched
[Exact file path(s) this task will create or edit — one per line. Not a description, not a
range, not "various config files." This feeds the file-conflict check below — if it's vague,
the conflict check can't do its job.]

## Acceptance criteria
[3 or fewer. Each must be a binary check — it either passes or it doesn't.]

## Gotchas
[Non-obvious traps only. Omit if none.]
```

---

## Research task sizing — the same rule applies to fetch/synthesis, not just code

The "one thing per task" discipline above is usually described in terms of code tickets, but it
applies just as hard to research dispatches — and it's easier to violate by accident, because a
research question doesn't have an obvious file/line boundary the way a code change does.

**Never bundle "fetch N sources" + "read M local files" + "write a long synthesized report" into
one research task.** This was tested empirically and failed identically three times in a row
across two different models: the worker gathers all the data, says something like "now let me
write the report," and returns empty — the step/context budget spent gathering data leaves
nothing for the write-up, and this is not a capability-tier problem, a stronger model failed the
exact same way.

**Split it instead:**
- One task per source (or a small batch) that does nothing but fetch and relay raw content —
  no analysis, no scoring, no opinions, explicitly say so in the ticket so the worker doesn't
  drift into synthesizing anyway.
- Do the actual synthesis yourself (the manager), or in a separate task that only reads
  already-fetched material (never re-fetches), once all the raw material is in hand.

If a research question turns out to need more than 2-3 fetches plus a handful of file reads to
answer, that's the same signal as an oversized code ticket — split it, don't just hope a bigger
model powers through it.


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

## File conflict check — non-negotiable

Two subagents editing the same file at the same time is a lost-write hazard — one agent's
changes get silently clobbered by the other's. AMP has exactly one mechanism that prevents this:
`dependency_ids`. There is no lock, no mutex, no file-level checkout. A task description that
says "coordinate with the other task" or "be careful not to conflict" is not enforcement — it
does nothing, because two independent subagent sessions have no way to see each other's state
mid-run. The only thing that actually prevents simultaneous access is one task being `blocked`
on the other until it completes.

**Run this check every time you create or modify a plan, and again before every dispatch wave:**

1. Build a map of `file_path -> [task_ids]` from every task's "Files touched" field — across the
   ENTIRE plan you're creating or touching, not scoped to the current epic, story, or wave. A
   task in an unrelated epic touching the same file is exactly as dangerous as one in the same
   story — scope has no bearing on whether two edits can happen at once.
2. For every `file_path` with 2 or more task IDs mapped to it: those tasks must never be able to
   reach `in_progress` or `ready_to_dispatch` simultaneously. Check whether each pair already has
   a dependency edge — either directly (`dependency_ids` includes the other), or transitively
   (both sit behind the same wave check, or one is behind a chain that eventually includes the
   other). If no such edge exists for a pair, add one: pick an order and set `dependency_ids` on
   the later task so it can't start until the earlier one is `completed`.
3. This check spans plan creation AND re-planning. If you're adding tasks to an existing plan
   (including fix tasks from a review), re-run the whole map — not just a diff against the new
   tasks — because a new task might collide with something from an earlier, already-approved
   wave that's still incomplete.
4. Re-run this check immediately before dispatching any wave, not only once at initial planning
   time. Reviewers create fix tasks after a plan is approved and dispatched — those fix tasks are
   not covered by the original check and can reintroduce exactly this hazard. See `amp-review` for
   the fix-task-specific version of this rule.

If you find a genuine conflict at dispatch time that the original plan didn't catch, do not
dispatch both tasks in the same batch. Either add the missing `dependency_ids` (preferred — it's
permanent and correct for any future re-dispatch), or dispatch one, wait for it to complete, then
dispatch the other.

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
- Assigned to whichever specialist's domain matches the fix (check the current subagent list — see "assigned_to" rule below)
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

**1. `assigned_to`** — set at creation time, every task, matched to the right specialist by domain.

**Do not hardcode a roster here or memorize agent names across sessions.** Your own `task` tool
description lists every currently available subagent — their name, and a summary of what each
covers — because opencode generates that list live from whatever is actually configured under
`.opencode/agent/*.md`. Read that list each time you plan. It is always accurate to what's
deployed right now; a roster written into this skill would go stale the moment an agent is
added, renamed, or retired.

Pick the specialist whose description best matches the task's domain (backend/Go, frontend/React,
docs/git, review, research, etc). Set `assigned_to` to that agent's exact name.

**File type alone does not tell you which tier to use — whether the task is a decision-bearing
action does.** This is Cognition's "actions carry implicit decisions, and conflicting decisions
carry bad results" principle (from "Don't Build Multi-Agents") applied to tier selection: a task
is decision-bearing if completing it requires the assignee to decide something not already
decided in the ticket — wording, structure, synthesis, a tradeoff. A docs/git-tier model is sized
for the opposite: literal, templated work with no decision left to make — swap a config value,
copy an established structure, follow an exact template line for line. It is not sized for
decision-bearing work, even when that work happens to live in a markdown or config file. If the
task requires explaining a concept, synthesizing a rule out of several considerations, weighing a
tradeoff, or writing original prose that teaches something the reader didn't already know — it's
decision-bearing. Route it to a stronger tier (backend/frontend), regardless of the file
extension.

- Wrong for docs tier: "Add a new skill section explaining the file-conflict algorithm and why
  dependency_ids is the only real enforcement" — this requires synthesizing several rules into
  coherent original prose. Route to a stronger tier.
- Wrong for docs tier: "Write the reasoning section for why we chose X over Y" — this requires
  weighing a tradeoff, not transcribing one. Route to a stronger tier.
- Right for docs tier: "Change the version string in package.json from 1.2.0 to 1.3.0" — pure
  literal substitution, no decision required.

A docs-tier agent returning empty output on a task is often not a fluke — it's a signal the task
itself needed judgment the model doesn't have. Don't just retry it as-is on the same tier.

**Scale effort to the task, with a concrete heuristic rather than per-ticket judgment.** Anthropic's
own multi-agent research system uses explicit scaling rules for delegation instead of leaving tier
selection to case-by-case guessing — apply the same idea here:

| Task shape | Guidance |
|---|---|
| Single literal substitution, no research needed | Docs tier, no research subagent, no need to read surrounding context |
| One concept explained or synthesized, needs 1-2 file reads first | Stronger tier (backend/frontend); dispatch a research subagent first only if the concept isn't already clear from the ticket-writer's own knowledge |
| Spans multiple systems, or the codebase area is unfamiliar | Dispatch a research subagent to investigate BEFORE writing the ticket at all — don't guess at scope, then write the ticket from what it reports |
| Reviewing/verifying existing work | Reviewer tier — never the tier that implemented it |

This is a heuristic, not a rigid formula — use judgment when a task doesn't cleanly fit a row.

This is how the kanban board shows who owns what. A task without `assigned_to` is invisible to
the user during review. If nothing in the current list fits the work, load
`skill("amp-agent-builder")` to mint a new specialist — don't force-fit an existing one.

**Before creating tickets: if you need to understand the existing codebase, dispatch a research
subagent to investigate and report back — do not read project files or webfetch yourself.**

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
- **Does it reference a function, type, struct field, or MCP tool that ANOTHER TASK IN THIS SAME
  PLAN is creating?** This is different from "calls an API another task creates" above — that's
  about a pre-existing system another task modifies. This is about something that doesn't exist
  yet until a sibling task in this exact plan builds it. Example: task A adds a new MCP tool;
  task B's description tells a worker to call that tool. At a glance A and B can look independent
  — different files, same wave — but B cannot actually be attempted correctly until A exists. B
  must depend on A.

If yes to any of these — add the dependency, regardless of which epic or story it's in.

---

## Plan presentation — show everything

Present before dispatching. The user needs to see assigned agents and blockers to review meaningfully.

`[agent]` below stands for whichever specialist name you actually set as `assigned_to` — pull the real name from your current subagent list, don't reuse a name from an old plan.

```
EPIC: [name]
└── Story: [name]
    ├── Task #N: [name]                    →  [agent]   (ready)
    ├── Task #N: [name]                    →  [agent]   (ready)
    ├── Task #N: Wave 1 check              →  [agent]   (blocked by #N, #N)
    ├── Task #N: [name]                    →  [agent]   (blocked by #N check)
    ├── Task #N: [name]                    →  [agent]   (blocked by #N check)
    ├── Task #N: Wave 2 check              →  [agent]   (blocked by #N, #N)
    └── Task #N: Code Review: [story name] →  [agent]   (blocked by #N check)

Phase 1 — dispatch immediately: #N, #N
Phase 2 — unblocks when wave 1 check passes: #N, #N
Phase 3 — unblocks when wave 2 check passes: #N (code review)

Total: X tasks (Y implementation + Z wave checks + 1 code review per story)
```

Every task shows: its ID, name, assigned agent, and either `(ready)` or `(blocked by #N, ...)`.
Wave checks and code reviews must be clearly labeled.
If a task has no agent or no blocker status shown — fix it before presenting.

**After the task tree, state the result of the file conflict check explicitly** — don't leave it
implied. Run the algorithm from "File conflict check — non-negotiable" above, then say one of:

```
File conflict check: no conflicts — no two tasks share a "Files touched" entry without a
dependency edge between them.
```

or, if you found and fixed conflicts:

```
File conflict check: found 2 conflicts, both serialized —
  - #14 and #19 both touch internal/kb/service.go → added #19 depends on #14
  - #22 and #31 both touch .opencode/skills/amp-kb/SKILL.md → added #31 depends on #22
```

The user needs to see this line to trust the plan is actually safe to dispatch, not just
plausible-looking.

---

## ⛔ Stop after presenting — do not dispatch

Wait for explicit approval. Nothing further until the user says so.

Approval: "approved", "go ahead", "yes", "do it", or similar.

Changes requested → update → present again → wait again.

---

## Dispatch — only after approval

**Step 0 — re-run the file conflict check against the current batch:** before dispatching
anything, look at everything currently in `ready_to_dispatch` (this includes fix tasks a reviewer
may have created since the plan was approved — they were not covered by the original check).
Build the file_path -> task_ids map again for this batch and cross-check it against anything
still `in_progress`. If two tasks about to go out together share a file with no dependency edge
between them, do not dispatch both in the same wave: either add the missing `dependency_ids` (if
there's time to do so before dispatching), or dispatch one now and hold the other until the first
completes.

For every task in `ready_to_dispatch`, do both steps in this order, using the task's own `assigned_to` value as the agent — not a fixed name:

**Step 1 — dispatch each task** (marks it in_progress on the board):
```
amp_dispatch_task(task_id=ID, agent_id=<task's assigned_to>)
```

**Step 2 — spawn workers in a single message** (runs them in parallel):
```
task(prompt="Task ID: {id}. Project ID: {project_id}.", subagent_type=<task's assigned_to>)
task(prompt="Task ID: {id}. Project ID: {project_id}.", subagent_type=<task's assigned_to>)
```

Step 1 must happen before step 2. This is what shows live progress on the board.

After dispatch: monitor with `amp_list_tasks`. When workers complete, blocked tasks auto-unblock and appear in `ready_to_dispatch`. Dispatch those. **After a review task completes, always check `ready_to_dispatch` — the reviewer may have created new fix tasks.** Re-run Step 0 against that new batch before dispatching it. Repeat until `ready_to_dispatch` and `in_progress` are both empty.

If work is genuinely time-gated (not blocked on another task, just not due yet), use `start_at` on `amp_create_task` or `amp_set_task_start_at` instead of dependencies — those tasks show up in a separate `scheduled` bucket and unblock automatically when their time arrives. Load `amp-mcp` for the exact shape.

Load `amp-mcp` if you need exact tool argument shapes.

---

## Worker failure — don't just retry silently

A dispatched task can fail in ways that don't look like a normal "cannot proceed" comment: it
returns empty output, times out, or produces no comments and no completion within a reasonable
window. Treat this as a real signal, not noise to route around quietly.

1. **Detect it.** If a dispatched task has been in `in_progress` with no comments and no
   completion for longer than the work plausibly takes, or the subagent's response was empty,
   that's a failure — don't assume it's still working silently.

2. **Log it before doing anything else.** Post a comment on that exact task documenting what
   failed and which agent/model was dispatched to it — e.g. "amp-worker-docs (qwen3.5-9b)
   returned empty output on this task; re-dispatching to amp-worker-backend." This is what leaves
   a paper trail. Do not silently re-dispatch and let the failure disappear from the record.

3. **Re-dispatch to a stronger tier if the failure looks like a capability mismatch** — the task
   required judgment the assigned model doesn't have (see the complexity test under `assigned_to`
   above). Update `assigned_to` on the task to the new agent before re-dispatching.

4. **Check every other queued task on the same tier — don't wait for each one to fail
   individually.** One failure from a tier is a signal that tier may be misassigned for this
   entire plan, not just this one ticket. Before continuing, scan every other not-yet-dispatched
   task still assigned to the tier that just failed, apply the same complexity test to each, and
   proactively reassign any that look like the same mismatch — rather than dispatching them one
   at a time and discovering the same failure repeatedly.
