---
title: Running work — dispatch, workers and runs
tags: [dispatch, workers, runs, tokens, quiet, worktrees]
---

# Running work

## The dispatch loop

Your manager repeats:
1. `amp_list_tasks(project_id)` — by default, **your** tickets only.
2. For each ticket in `ready_to_dispatch`: `amp_dispatch_task`, then start a worker on it **in
   the foreground** and wait for it. Both in the same turn.
3. When workers finish, check again: reviews may have created fix tickets, answers may have put
   questions back to ready, scheduled tickets may have come due.
4. Stop when nothing is ready or in progress. Report questions and work waiting to merge.

Rules that keep this healthy:
- **Only your own tickets, and only after dispatch succeeds.** If AMP refuses the dispatch (it's
  someone else's), don't run a worker on it anyway. Teammates' agents run theirs.
- **At most 4 workers at once.** Wide fan-outs are where sessions hang, and a hung session
  orphans every ticket in flight.
- **Foreground, not background.** A background task dies with the session that started it. A
  headless run ends when the manager replies. There is no loop after it answers you.
- **One worktree per parallel worker**, inside the repo (`.worktrees/<ticket>`).
- **Never mark a ticket dispatched you aren't about to run.** Everyone sees it as in progress.

## What a worker does

1. Confirms the ticket is its to work (`in_progress`, owned by its user).
2. Reads the ticket, its comments (earlier attempts and answers live there) and the KB.
3. Works only inside its ticket's files and the project directory, commenting at each step.
4. Commits with the ticket ref in the message and pushes its branch.
5. Completes (`amp_complete_task`) or stops with a question (`amp_block_task`), recording its
   **run**: agent, model, input/output/cache tokens, cost, branch, commit, PR, pushed.

Tokens are never estimated: they come from the harness (`amp_session_usage` in opencode, the
subagent result in Claude Code) or are left out.

## Runs

Each attempt at a ticket is a run, kept even if the ticket is later deleted. Outcomes:
`completed`, `blocked`, `abandoned` (a worker that died or was stopped; the manager records it
with `amp_report_run(outcome="abandoned", run_note=…)`). `amp_get_task_runs` lists them; the
ticket panel shows them. Abandoned runs show rework and don't count as missing usage.

## Quiet tickets

A ticket in progress with no AMP activity for 15 minutes is **quiet**: its worker probably died
(a crashed session, a hung permission prompt, a laptop closed). Mission and My work flag it, and
`amp_list_tasks` lists it under `quiet`. To recover: read its comments to see how far it got,
record the abandoned run, set it back with `amp_set_task_state(state="backlog")`, and dispatch it
again. The new worker continues from the comments and the branch.

A worker in a long build without commenting can look quiet too; workers should comment at each
step.
