---
name: amp-execution
description: Full protocol for executing an AMP task — reading the ticket, doing the work, logging progress, writing to the KB, and completing
---

# AMP Execution Skill

You have one job: execute the assigned task completely and correctly.
Your task ID and project ID are in your dispatch prompt.

---

## Step 0 — Read .amp.json

```bash
cat .amp.json
```

This gives you `project_id`. Every MCP call uses this value.

---

## Step 0.5 — Search the KB before starting

**This is not optional.** Before reading the ticket, search the KB:

```
amp_kb_search(project_id=PROJECT_ID, query="<your task name + key terms>")
```

- Got relevant results → read them with `amp_kb_get` before touching anything
- Nothing relevant → proceed, but you are expected to document what you discover

See the `amp-kb` skill for full search and writing guidance. Load it now if you
haven't already: `skill("amp-kb")`

---

## Step 1 — Read your ticket

```
amp_get_task(task_id=YOUR_TASK_ID)
```

Read every field:
- `description` — your complete instructions
- `acceptance_criteria` — exactly what done looks like
- `assigned_to` — should match your agent ID

If description is missing or unclear: post a comment explaining what is missing and
**STOP**. Do not guess at intent.

---

## Step 2 — Post a starting comment

Before touching anything:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Starting work.

KB search result: [what I searched for and what I found / didn't find]

My understanding: [one sentence]

Plan:
1. [step]
2. [step]
""", author="amp-worker")
```

---

## Step 3 — Work and log every meaningful step

Post a comment every time you:
- Find something non-obvious in the codebase
- Make a decision — explain WHY, not just what
- Change a file — name it and describe what changed
- Hit a problem

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Finding: [what you found and where]
Decision: [what and why]
Changed: [file — what changed]
""", author="amp-worker")
```

---

## Step 4 — Write to the KB

After completing substantive work, write at least one KB doc if you:
- Discovered how something works (especially if non-obvious)
- Made an architectural decision with trade-offs
- Found a gotcha or edge case
- Completed work future agents will build on

**Tags are mandatory on every KB write. Never call `amp_kb_write` without 3–6 tags.**
Load `amp-kb` now if you haven't — it defines the required tag formula, what good tags
look like, and how to write content that embeds well for semantic search:

```
skill("amp-kb")
```

---

## Step 5 — Cannot proceed

If you hit a blocker:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
CANNOT PROCEED.

Reason: [exact blocker]
What I tried: [steps]
What is needed: [specific requirement]
""", author="amp-worker")
```

Stop. Do NOT call amp_complete_task.

---

## Step 6 — Complete

Verify every acceptance criterion. Post a completion summary:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Work complete.

Summary: [what was done]

Files changed:
- [path]: [what and why]

KB docs written:
- [path]: [what was documented]

Acceptance criteria:
- [criterion]: VERIFIED — [how]
""", author="amp-worker")
```

Then:
```
amp_complete_task(task_id=YOUR_TASK_ID)
```

---

## If you are a REVIEWER

When your task name contains "check", "review", "code review", or "Review:", you are a reviewer.

### Two kinds of review tasks — read your task name

**Wave check** (task name contains "check" or "Wave N check"):
- Your job is lightweight verification: did the build pass and were acceptance criteria met?
- Run the build command from the task description
- Check each acceptance criterion — read the relevant code and confirm each one
- You do NOT run the full code-reviewer checklist
- Post LGTM or create targeted fix tasks, then complete

**Code review** (task name starts with "Code Review:"):
- Your job is a full tiered code review using the `code-reviewer` skill
- Load it first: `skill("code-reviewer")`
- Run `git diff` to see all changes from the story's implementation tasks
- Apply the Blocker / Significant / Suggestion checklist to every changed file
- Post your findings in the standard format (Overall Verdict → Blockers → Significant → Suggestions)
- Create a fix task for every Blocker and Significant finding
- Suggestions are noted but do not block completion
- Complete the review task when done — fix tasks land in the backlog

### Reviewer rules (both types)

1. **Never fail or block yourself** — issues become fix tasks, not a blocked review
2. **Never fix complex issues yourself** — create a targeted task and complete the review
3. **One issue = one fix task** — don't bundle multiple problems into one fix
4. **Always complete** — the review always ends with `amp_complete_task`

### Fix task template

```
amp_create_task(
  project_id=PROJECT_ID,
  epic_id=SAME_EPIC_ID,
  story_id=SAME_STORY_ID,
  name="Fix: [specific issue]",
  description="""
## What to fix
[Exact file path, line number or function name, what is wrong]

## What to change
[Exact change needed — be specific]

## Context
Found during review of task #[REVIEWED_TASK_ID].
[One sentence on why this matters]

## Acceptance criteria
- [specific thing that must be true after the fix]
- go build ./... passes / npm run build passes
""",
  acceptance_criteria="[specific criterion]",
  assigned_to="amp-worker"
)
```

### What reviewers check

- **Correctness** — does the code actually do what the task asked?
- **Completeness** — were all acceptance criteria met? Check each one explicitly
- **Build** — run the exact build command — if it fails, that's a fix task
- **Code quality** (code review only) — apply the code-reviewer skill checklist
- **Scope** — did the worker change files they weren't supposed to? Extra changes = fix task to revert

### What reviewers do NOT do

- Do not rewrite working code because you'd do it differently
- Do not create fix tasks for style preferences (Suggestion tier only)
- Do not try to fix complex issues yourself — create a targeted task
- Do not block on minor issues — create a fix task and complete the review

---

## MCP tools

```
amp_get_task {task_id}
amp_add_task_comment {task_id, body, author}
amp_complete_task {task_id}
amp_get_ticket_history {task_id}
amp_get_epic / amp_get_story
amp_create_task {project_id, epic_id, story_id, name, description, acceptance_criteria, assigned_to}
amp_kb_search / amp_kb_get / amp_kb_write  ← see amp-kb skill
```
