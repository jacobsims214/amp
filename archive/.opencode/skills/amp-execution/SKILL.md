---
name: amp-execution
description: How a worker agent reads its ticket, does the work, posts progress notes, and completes the task
---

# AMP Execution Skill

Use this skill when executing an assigned Odoo task.
The ticket is the source of truth. The chatter is the permanent log.

---

## Workflow

### 1. Read your ticket first

```
amp_get_task(task_id=YOUR_ID)
```

Read:
- `description_text` — your complete instructions
- `acceptance_criteria` — exactly what done looks like
- `context_data` — JSON with project/epic/story context from the manager
- Manager's planning comment in the chatter for additional context

### 2. Post a starting comment immediately

Before touching anything:

```
amp_add_task_comment(task_id=YOUR_ID, body="""
Starting work.
Plan:
1. [first step]
2. [second step]
""")
```

### 3. Check for prior context

```
amp_search_kb(query="[task name or key topic]", project_id=YOUR_PROJECT_ID)
```

Read what prior agents found. Don't repeat work already done.

### 4. Do the work — post notes as you go

Post a comment every time you discover something, change something, or make a decision:

```
amp_add_task_comment(task_id=YOUR_ID, body="""
Finding: [what you found]
File: [path if relevant]
Detail: [what it means / what you did]
""")
```

Post when you:
- Find a bug or issue
- Make a decision (explain WHY)
- Modify a file (list which files and what changed)
- Hit a problem and work around it
- Complete a significant sub-step

These comments are the project's institutional memory. Future agents read them.

### 5. If you can't proceed

Post a comment and stop. Do not call `amp_block_task` — workers never set the blocked state.
The `blocked` state is managed automatically by the dependency system, not by agents.

```
amp_add_task_comment(task_id=YOUR_ID, body="""
CANNOT PROCEED.
Reason: [exact reason]
What I tried: [what you attempted]
What is needed: [specific requirement — be precise so the manager can act]
""")
```

Stop. The manager will see the comment and decide what to do.

### 6. Complete

Verify every acceptance criterion. Then post a completion summary:

```
amp_add_task_comment(task_id=YOUR_ID, body="""
Work complete.
Done:
- [what was done]
Files changed:
- [path]: [what changed]
Acceptance criteria:
- [criterion 1]: ✓ [how verified]
- [criterion 2]: ✓ [how verified]
""")
```

Then: `amp_complete_task(task_id=YOUR_ID)`

Use `amp_complete_task`, not `amp_update_task` with state — the former triggers automatic unblocking of dependent tasks.

### 7. KB entry (only if genuinely reusable)

Only create a KB entry if the knowledge is useful beyond this ticket:
- A pattern or convention that applies across the codebase
- A non-obvious gotcha others will hit
- An architectural decision with reasoning that should outlive this ticket

Do NOT create one just to summarize what's already in the comments.

```
amp_create_kb_entry(
    title="[short searchable description]",
    content="""
## Context
[when/where this applies]

## The knowledge
[the actual insight, pattern, gotcha, or decision]

## Why it matters
[what goes wrong if you don't know this]
""",
    project_id=YOUR_PROJECT_ID,
    task_id=YOUR_TASK_ID,
    entry_type="finding",
    created_by_agent="amp-worker"
)
```
