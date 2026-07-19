---
name: amp-review
description: Reviewer protocol for AMP — wave checks and full code reviews, the fix-task template, and rules for creating fix tasks instead of failing. Use when your task name contains "check", "review", or "Code Review:".
---

# AMP Review Skill

When your task name contains "check", "review", "code review", or "Review:", you are a
reviewer — not an implementer.

---

## Two kinds of review tasks — read your task name

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

---

## Reviewer rules (both types)

1. **Never fail or block yourself** — issues become fix tasks, not a blocked review
2. **Never fix complex issues yourself** — create a targeted task and complete the review
3. **One issue = one fix task** — don't bundle multiple problems into one fix
4. **Always complete** — the review always ends with `amp_complete_task`

---

## Fix task template

Pick `assigned_to` for the fix task from whichever specialist's domain matches the issue — check
the current subagent list via your own tool context, don't reuse a name from an old plan.

**Before creating the fix task, check for file conflicts with in-flight work.** Run
`amp_list_tasks` and look at everything currently `in_progress` or `ready_to_dispatch`. If any of
those tasks touch the same file your fix will touch, add that task's ID to `dependency_ids` so
your fix can't start until it completes — two agents editing the same file at once is a
lost-write hazard, and `dependency_ids` is the only real way to prevent it. See the "File conflict
check" section in `amp-planning` for the full algorithm; this is the same rule applied to one
new task instead of a whole plan.

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

## Files touched
[Exact file path this fix will edit — one per line]

## Acceptance criteria
- [specific thing that must be true after the fix]
- go build ./... passes / npm run build passes
""",
  acceptance_criteria="[specific criterion]",
  assigned_to="[domain-matched specialist name]",
  dependency_ids=[/* any in-flight task IDs that touch the same file, per the check above */]
)
```

---

## What reviewers check

- **Correctness** — does the code actually do what the task asked?
- **Completeness** — were all acceptance criteria met? Check each one explicitly
- **Build** — run the exact build command — if it fails, that's a fix task
- **Code quality** (code review only) — apply the code-reviewer skill checklist
- **Scope** — did the worker change files they weren't supposed to? Extra changes = fix task to revert

## What reviewers do NOT do

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
```
