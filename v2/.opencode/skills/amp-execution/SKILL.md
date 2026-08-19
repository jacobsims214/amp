---
name: amp-execution
description: Full protocol for executing an AMP task — reading the ticket, doing the work, logging progress, writing to the KB, and completing
---

# AMP Execution Skill

You have one job: execute the assigned task completely and correctly.
Your task ID and project ID are in your dispatch prompt.

Your ticket's comments are the only place you log progress. Never use a built-in tool like
`todowrite` (you shouldn't even have it — it's denied on this agent) or any other mechanism as a
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

**Do not load `skill("amp-kb")` here.** You have the search tool — use it directly.
Load `skill("amp-kb")` only in Step 4 when you are ready to write a KB doc.
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

If description is missing or unclear: do not guess at intent — go to Step 5 and block. A
comment on its own is not enough; the ticket has to change state or nobody finds out.

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
skill("amp-kb")
```

It defines the required tag formula, create-vs-update rules, and how to write content
that embeds well for semantic search.

---

## Step 5 — Cannot proceed, or need an answer

**You cannot ask a question and wait for a reply.** You have no channel to the user and no
channel back to the manager while you are running — nobody is reading your output until you
finish. Ending your turn with a question in it is the same as ending it with silence: the wave
stalls, the ticket sits `in_progress` forever, and nobody knows why. If you need a decision,
a credential, a missing file, or a call the ticket doesn't make for you, you **block** — you
never ask and hope.

Blocking is three actions, all three required, in this order:

**1. Comment the detail on the ticket:**
```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
CANNOT PROCEED.

Reason: [exact blocker]
What I tried: [steps]
What is needed: [the specific question or requirement — phrase it so it can be answered
                 without re-reading the whole ticket]
""", author="amp-worker")
```

**2. Move the ticket into the blocked state** — this is what the manager actually sees. A comment
alone does not change the ticket's state, so a commented-but-not-blocked task is indistinguishable
from one still being worked:
```
amp_block_task(task_id=YOUR_TASK_ID, reason="[one line — the question or missing thing]")
```

**3. End your turn with the report, as plain text** (see Step 7). Start it with `BLOCKED:` so the
manager sees it without opening the ticket.

Do NOT call `amp_complete_task`. Do not guess at the answer and carry on — a wrong guess costs
more than a blocked ticket.

**Running out of room counts as a blocker.** If you are burning through your step budget and the
work is not going to fit, do not keep going until you are cut off mid-tool-call — being cut off
reports nothing to anybody. Stop while you still have turns left, comment what is done and what
remains, `amp_block_task` with reason "ran out of steps — [what remains]", and report back.

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

## Step 7 — Report back in your final message

**`amp_complete_task` is not the end of the task. Your final message is.**

The last thing you emit is your entire report to the manager — it is the only part of your run the
manager ever sees directly. If your turn ends on a tool call, or on empty output, the manager gets
nothing back and has to guess from the board whether you succeeded, died, or are still thinking.
That guess is where waves stall.

So: after the last tool call, always write a short plain-text summary and end there. No tool call
after it.

Completed work:
```
DONE — task #<id>: <task name>

Changed: <file>: <one line>, <file>: <one line>   (or "no files changed")
KB: <doc path or "none">
Criteria: <N>/<N> verified   (name any that were COULD NOT VERIFY, and why)
```

Blocked work (from Step 5):
```
BLOCKED — task #<id>: <task name>

Needs: <the exact question or missing thing>
Done so far: <one line>
Ticket state: blocked via amp_block_task
```

Keep it to a handful of lines — the detail already lives in the ticket comments. What matters is
that the manager learns three things without opening anything: which task, whether it finished,
and what it needs next if it didn't.

---

## MCP tools

```
amp_get_task {task_id}
amp_add_task_comment {task_id, body, author}
amp_complete_task {task_id}
amp_block_task {task_id, reason}   ← Step 5; the only way to escalate
amp_get_ticket_history {task_id}
amp_get_epic / amp_get_story
amp_create_task {project_id, epic_id, story_id, name, description, acceptance_criteria, assigned_to}
amp_kb_search / amp_kb_get / amp_kb_write  ← see amp-kb skill
```
