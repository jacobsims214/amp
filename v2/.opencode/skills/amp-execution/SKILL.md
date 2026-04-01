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

See `amp-kb` skill for exactly how to write docs that embed well for semantic search.
The critical rule: **write in prose paragraphs, not bullet lists and code blocks**.
A sparse doc embeds as noise. A rich prose doc surfaces on dozens of related queries.

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

## MCP tools

```
amp_get_task {task_id}
amp_add_task_comment {task_id, body, author}
amp_complete_task {task_id}
amp_get_ticket_history {task_id}
amp_get_epic / amp_get_story
amp_kb_search / amp_kb_get / amp_kb_write  ← see amp-kb skill
```
