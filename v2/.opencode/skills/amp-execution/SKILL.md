---
name: amp-execution
description: How a worker agent reads its ticket, does the work, posts progress notes, and completes the task
---

# AMP Execution Skill

**The ticket is your entire context. Read it first. Everything you need is in it.**

---

## Step 1 — Read your ticket

```
amp_get_task(task_id=YOUR_TASK_ID)
```

Read every field:
- `description` — your complete instructions. Treat this as your prompt.
- `acceptance_criteria` — the exact conditions you must satisfy to mark this done.
- `epic_id` / `story_id` — the parent context (you can fetch these if you need more background).
- `agent_id` — should be your identifier.

If the description is missing or unclear, post a comment explaining what is missing
and stop. Do NOT guess at intent.

---

## Step 2 — Post a starting comment immediately

Before writing a single line of code or running any command:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Starting work.

Plan:
1. [first concrete step]
2. [second concrete step]
3. [etc.]

Reading: [any files or systems I need to understand first]
""", author="amp-worker")
```

This is your commitment to the ticket log. It tells the manager what you understood
and what you plan to do — before you do it.

---

## Step 3 — Work and log as you go

Post a comment every time you:
- Read a file and find something relevant
- Make a decision (explain WHY, not just what)
- Change a file (name the file and what changed)
- Hit a problem
- Complete a meaningful sub-step

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Finding: [what you found]
File: [path]
Decision: [what you chose and why]
""", author="amp-worker")
```

The ticket log is institutional memory. Future agents — and the manager — will
read it. Write for them.

---

## Step 4 — If you cannot proceed

Post a comment and stop immediately:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
CANNOT PROCEED.

Reason: [exact blocker]
What I tried: [steps attempted]
What is needed: [precise requirement — specific enough for the manager to act on]
""", author="amp-worker")
```

Do NOT call `amp_complete_task`. Do NOT make up a workaround without logging it.

---

## Step 5 — Complete

Before calling complete, verify EVERY acceptance criterion from `acceptance_criteria`.
If you cannot verify one, do not complete — post a blocking comment instead.

Post a completion summary:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Work complete.

Summary:
- [what was done]

Files changed:
- [path]: [what changed and why]

Acceptance criteria:
- [criterion 1]: VERIFIED — [how you verified it]
- [criterion 2]: VERIFIED — [how you verified it]
""", author="amp-worker")
```

Then:

```
amp_complete_task(task_id=YOUR_TASK_ID)
```

This triggers automatic unblocking of any tasks that were waiting on yours.
