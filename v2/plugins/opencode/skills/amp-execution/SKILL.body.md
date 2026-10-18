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

Then search the team KB once for how this team works — branches, commits, pushing, review,
secrets. Team conventions live there, not in the project KB:

```
amp_kb_search(project_id=PROJECT_ID, scope="team", query="conventions branches commits review")
```

Follow what you find. They override the defaults in this skill.

After searching, check the `updated_at` of each result. If the most relevant result is more than 30 days old, search again with `recency_boost=0.5` or `min_recency_days=30`.

If nothing recent exists, note the staleness in your starting comment so the reviewer knows the info may be outdated.

**Do not load `skill("amp-kb")` here.** You have the search tool — use it directly.
Load `skill("amp-kb")` only in Step 4 when you are ready to write a KB doc.
The skill is large. Loading it now wastes context you need for the actual work.

---

## Step 1 — Read your ticket and its comments

```
amp_get_task(task_id=YOUR_TASK_ID, include=["comments"])
```

**First, confirm this ticket is yours to work:** its `state` is `in_progress` and its
`owner_user_id` is your user (`amp_whoami`). If it isn't — it's someone else's, or it was never
dispatched — stop without touching any files and tell your manager. Another person's session may
be running it; working it from here edits their files under your name.

Read every field:
- `description` — your complete instructions
- `acceptance_criteria` — exactly what done looks like
- `assigned_to` — should match your agent ID

Then read **every comment**. You may not be the first worker on this ticket: an earlier worker
may have asked a question (a `QUESTION` comment) and stopped, and a person or the orchestrator
answered it in a later comment. **An answer on the ticket is binding** — it overrides the
description where they differ. Start from what the earlier worker found and decided; don't redo
it or ask the same question again.

If the description is missing or unclear and the comments don't settle it, don't guess at
intent: ask (Step 5).

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

Work only inside the project directory (a worktree you were given at `.worktrees/<ticket>` is
inside it). Reading or running anything outside it asks your user for permission, and as a
subagent you can't see that prompt — you'll wait on it until someone kills the session.

Stay inside your ticket's "Files touched". Other tickets — often other people's, in other
checkouts — own the rest, and the plan's file-conflict check only holds if you do. If the work
needs a change outside that list, make it only if it's trivial and say so in a comment;
otherwise ask (Step 5).

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

### Keep it moving

Every turn costs time. Progress comments are short: a few lines on what you're doing and
why, not your whole plan (the starting comment stays under ~150 words). Make related changes
to a file in one edit, or rewrite the section, instead of many one-line edits. Batch
independent tool calls in the same turn.

### Never wait on something that can't finish

Nobody is watching a subagent's tool calls: a call that never returns stalls the ticket until a
person notices. Every wait must be bounded. In code (opencode's `execute`, scripts, tests),
never `await` a promise that might not resolve (`await new Promise(r => {})` hangs forever);
wait for a condition with a limit instead (chrome-devtools `wait_for`, or
`await new Promise(r => setTimeout(r, 1000))`), and give long commands a timeout (`timeout 300
<cmd>`). If something takes more than a few minutes, stop it, comment what happened, and try a
different way.

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

## Step 5 — Cannot proceed: ask, then stop

You can't wait for an answer: when you stop, your run ends and the orchestrator gets your final
message. So a question goes on the ticket, where a person (in the AMP UI) or the orchestrator (in
its chat with the user) can answer it, and the next worker picks it up from there.

First decide which kind of stop this is:

- **You can decide it yourself.** A reasonable call within the ticket's scope (a name, an
  approach, an edge case the criteria don't cover) is not a question. Make it, record it as a
  `Decision:` comment with your reasoning, and carry on.
- **You need another ticket's output** (a schema, an API, a fix that doesn't exist yet). That's a
  missing dependency, not a question for a person. Comment what's missing and which ticket should
  provide it, don't block, and say so in your final message so the orchestrator adds the
  dependency and re-dispatches.
- **You need a person**: a decision only they can make, information you can't find, access or a
  credential you don't have, or requirements that contradict each other. Ask:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
QUESTION: [the question, in one sentence]

Context: [what you found that raised it — files, errors, constraints]
Options: [the choices you see, with the trade-off of each]
Recommendation: [what you'd do and why, if you have a view]
Done so far: [what's already in place, so the next worker doesn't redo it]
""", author="amp-worker")

amp_block_task(task_id=YOUR_TASK_ID, reason="[the question in one line, with the options]")
```

The `reason` is what people see in AMP as the question to answer, so make it stand on its own:
"Avalara or Stripe Tax? Both work in the sandbox; Stripe is cheaper under 10k invoices a month."
— not "blocked" or "see comments".

Then **stop**. Do NOT call `amp_complete_task`. End your final message with:

```
QUESTION on task YOUR_TASK_ID: [the question]
```

so the orchestrator can put it to the user straight away.

---

## Step 6 — Complete

Verify every acceptance criterion. "VERIFIED" means you actually ran something that proves it —
the build command, a test, a linter, a grep/read that confirms the exact text or value exists, a
YAML/JSON parse check. Writing "VERIFIED" without having run a real check this task is not
allowed — it's a guess wearing a confident word, and it's how build errors and missed edits ship
undetected. If you cannot actually verify something (no build command applies, no way to check
programmatically), write "COULD NOT VERIFY: [why]" instead of asserting VERIFIED anyway — that's
honest and still useful; a false VERIFIED is not.

Commit your work, following the team's conventions from Step 0.5 (branch naming, commit
format). Put the ticket ref (e.g. `NWC-149`) in the commit message, so `git log` leads back to AMP
just as AMP leads to the commit. `git status` should show nothing of yours left uncommitted: changes left on disk are
invisible to everyone else, and the next agent in this checkout can't tell whose they are. Then make it reachable: other people's tickets may depend on yours, and a ticket that is
done in AMP but whose code exists only in one checkout blocks them without anyone noticing. If the
conventions say to push (or open a PR), do it; if they say nothing, push the branch you committed
on. If you can't push (no remote, no access), say so in the completion comment.

Post a completion summary:

```
amp_add_task_comment(task_id=YOUR_TASK_ID, body="""
Work complete.

Summary: [what was done]

Where it is: [branch] @ [commit sha] — [pushed to origin / PR #N / NOT pushed: why]

Files changed:
- [path]: [what and why]

KB docs written:
- [path]: [what was documented]

Acceptance criteria:
- [criterion]: VERIFIED — [the exact command/check you ran and its result]
- [criterion]: COULD NOT VERIFY — [why, if a real check wasn't possible]
""", author="amp-worker")
```

Then complete it **with the run record** — what you used and where the work is. AMP reports and
capitalizes token spend from these numbers, and traces code back to tickets from the git fields:

```
amp_session_usage()          ← if your harness has it (opencode with the AMP plugin): your model,
                               tokens and cost for this session. Pass them straight through.
amp_complete_task(task_id=YOUR_TASK_ID,
  model="<the model you ran on>",
  input_tokens=…, output_tokens=…, cache_read_tokens=…, cost_usd=…,   ← only numbers your harness gave you
  branch="<branch>", commit="<sha>", pushed=true|false, pr_url="<if one>")
```

Report what you know and leave the rest out — never estimate token counts. If your harness can't
tell you your usage, still report the model and the git fields. The same fields go on
`amp_block_task` when you stop on a question.

---

## MCP tools

```
amp_get_task {task_id, include: ["comments"]}   ← read before starting; answers to questions are in the comments
amp_add_task_comment {task_id, body, author}
amp_block_task {task_id, reason, …run fields}   ← only to ask a person a question (Step 5)
amp_complete_task {task_id, model?, input_tokens?, output_tokens?, cache_read_tokens?, cost_usd?, branch?, commit?, pushed?, pr_url?}
amp_report_run {task_id, outcome, …run fields}  ← a run that ended another way, or a PR opened later
amp_session_usage {}                  ← opencode plugin: your own model/tokens/cost
amp_get_task {task_id, include: ["history","runs"]}   ← what happened before you
amp_get_plan {project_id, epic_id?}
amp_create_tasks {project_id, tasks: [{ref, story_id, name, description, acceptance_criteria, assigned_to, depends_on}]}
amp_kb_search / amp_kb_get / amp_kb_write  ← see amp-kb skill
```
