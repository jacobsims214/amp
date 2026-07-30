---
name: amp-execution
description: Full protocol for executing an AMP task — reading the ticket, doing the work, logging progress, writing to the KB, and completing
---

# AMP Execution Skill

You have one job: execute the assigned task completely and correctly.
Your task ID and project ID are in your dispatch prompt.

Your ticket's comments are the only place you log progress. Never use a built-in tool like
`TodoWrite` (you shouldn't even have it — it's denied on this agent) or any other mechanism as a
substitute. Progress lives in `amp_add_task_comment`, nowhere else.

---

## Step 0 — Read .amp.json

```bash
cat .amp.json
```

This gives you `project_id`. Every MCP call uses this value.

---

## Step 0.5 — Search the KB before starting

Before reading the ticket, search the KB for relevant context:

```
amp_kb_search(project_id=PROJECT_ID, query="<task name + key terms>")
```

- Got relevant results → read them with `amp_kb_get` before touching anything
- Nothing relevant → proceed

After searching, check the `updated_at` of each result. If the most relevant result is more than 30 days old, search again with `recency_boost=0.5` or `min_recency_days=30`.

If nothing recent exists, note the staleness in your starting comment so the reviewer knows the info may be outdated.

**Do not load the **amp-kb** skill here.** You have the search tool — use it directly.
Load the **amp-kb** skill only in Step 4 when you are ready to write a KB doc.
The skill is large. Loading it now wastes context you need for the actual work.

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

Load the KB skill now — only at this step, not before:
```
the **amp-kb** skill
```

It defines the required tag formula, create-vs-update rules, and how to write content
that embeds well for semantic search.

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

Verify every acceptance criterion. "VERIFIED" means you actually ran something that proves it —
the build command, a test, a linter, a grep/read that confirms the exact text or value exists, a
YAML/JSON parse check. Writing "VERIFIED" without having run a real check this task is not
allowed — it's a guess wearing a confident word, and it's how build errors and missed edits ship
undetected. If you cannot actually verify something (no build command applies, no way to check
programmatically), write "COULD NOT VERIFY: [why]" instead of asserting VERIFIED anyway — that's
honest and still useful; a false VERIFIED is not.

Post a completion summary:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Work complete.

Summary: [what was done]

Files changed:
- [path]: [what and why]

KB docs written:
- [path]: [what was documented]

Acceptance criteria:
- [criterion]: VERIFIED — [the exact command/check you ran and its result]
- [criterion]: COULD NOT VERIFY — [why, if a real check wasn't possible]
""", author="amp-worker")
```

Then:
```
amp_complete_task(task_id=YOUR_TASK_ID)
```

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
