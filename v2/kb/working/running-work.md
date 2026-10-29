---
title: Running work — dispatch, workers and runs
tags: [dispatch, workers, runs, tokens, quiet, worktrees]
---

# Running work

## Dispatching and starting are two steps

A ticket is **dispatched** to an agent, and then a **worker starts** it. They are separate, and
between them the ticket is *in progress but not started* — that is the state AMP used to hide, and
the one that showed as "in progress" while nothing was happening.

1. **The manager dispatches:** `amp_dispatch_task(task_id, agent_id)`. The result carries
   `dispatch_id`, `worker_prompt` (a ready-made prompt for the worker) and `next`. Nothing has
   started the ticket yet.
2. **The manager launches the worker in the same message**, with `worker_prompt` as its prompt.
   A dispatch it doesn't launch is a ticket in progress with nobody on it.
3. **The worker starts its ticket:** its first call is
   `amp_claim_task(task_id, dispatch_id)`. From then on the ticket is *started*.
4. **The worker finishes its own ticket:** `amp_complete_task(…, dispatch_id)` or
   `amp_block_task(…, reason)`. Never a silent end.

A dispatched ticket nobody claims is **flagged after 10 minutes** and **returned to ready after
30 minutes** — no more tickets sitting in progress overnight. Both timeouts are project settings
(Settings → dispatch watchdog): a flag at 10 minutes and a return at 30, `0` turns them off.

Old plugins still work: a worker that never claims starts its ticket with its first comment, run,
block or completion (`started_by: "inferred"`), so nothing is flagged or returned. A manager on
an old plugin never sees `worker_prompt`, so update it (`amp-guide` note in the plugin's README).

## The dispatch loop

Your manager repeats:
1. `amp_list_tasks(project_id)` — by default, **your** tickets only. Three lists matter beyond
   `ready_to_dispatch`: `not_started` (dispatched, no worker started it), `returned` (nobody
   started it in time, so AMP sent it back to ready) and `quiet` (started, then silent).
2. For each ticket in `ready_to_dispatch`: `amp_dispatch_task`, then launch the worker with the
   returned `worker_prompt` **in the same message**, in the foreground, and wait for it.
3. When workers finish, check again: reviews may have created fix tickets, answers may have put
   questions back to ready, scheduled tickets may have come due. Clear `not_started` (launch it
   or undo the dispatch) and decide what to do with `returned` (re-dispatch knowingly, or ask).
4. Stop when nothing is ready or in progress. Report questions, work waiting to merge, and
   `waiting_on_people`: the human tickets the plan is waiting on, and whose they are.

Human tickets are never dispatched or listed as ready: a person does them in AMP. See
`working/human-work.md`.

Rules that keep this healthy:
- **Only your own tickets, and only after dispatch succeeds.** If AMP refuses the dispatch (it's
  someone else's), don't run a worker on it anyway. Teammates' agents run theirs.
- **At most 4 workers at once.** Wide fan-outs are where sessions hang, and a hung session
  orphans every ticket in flight.
- **Foreground, not background.** A background task dies with the session that started it. A
  headless run ends when the manager replies. There is no loop after it answers you.
- **One worktree per parallel worker**, inside the repo (`.worktrees/<ticket>`).
- **Never dispatch a ticket you aren't about to run.** If you decide not to after all, undo it:
  `amp_set_task_state(task_id, state="backlog", reason="not started")`.

## What a worker does

1. Starts its ticket: `amp_claim_task(task_id, dispatch_id)` from its dispatch prompt (or its
   harness does it for it). Without a dispatch ID it skips the claim and its first write starts
   the ticket.
2. Confirms the ticket is its to work (`in_progress`, owned by its user).
3. Reads the ticket, its comments (earlier attempts and answers live there) and the KB.
4. Works only inside its ticket's files and the project directory, commenting at each step.
5. Commits with the ticket ref in the message and pushes its branch.
6. Completes (`amp_complete_task` with its `dispatch_id`) or stops with a question
   (`amp_block_task`), recording its **run**: agent, model, input/output/cache tokens, cost,
   branch, commit, PR, pushed.

Tokens are never estimated: they come from the harness (`amp_session_usage` in opencode, the
subagent result in Claude Code) or are left out.

## Closeouts — a ticket finished by someone other than its worker

A worker completes its own ticket, and AMP records it as "completed by <agent>". When a worker
**can't** finish — it died, timed out, or lost its connection — the manager closes the ticket out:
`amp_complete_task(task_id, closeout_reason="worker timed out after 20 min")`, with no
`dispatch_id`.

A closeout is labelled: history says `Closed out by <who> (not the worker): <reason>`, the run's
note starts with `closeout:`, and the ticket shows a **Closeout** tag. It is allowed only for a
ticket a worker really couldn't close: one that was started and whose worker is gone, or a ticket
that is already ready, done or blocked and has earlier runs (a worker blocked with a question and
the answer arrived). A ticket with no runs at all can't be closed out — it needs a worker, or a
person in the web UI.

Not every non-worker completion is a closeout, and AMP knows the difference: a ticket started by
the fallback (an old plugin that never claimed), a dispatched ticket that never started, and a
person completing a ticket in the web UI are all ordinary completions.

**Never dispatch a ticket just to complete it.** Before dispatch tokens, a manager sometimes had
to dispatch a finished ticket so AMP would accept its completion. Now a ready ticket with its runs
is closed out directly.

## Runs

Each attempt at a ticket is a run, kept even if the ticket is later deleted. Outcomes:
`completed`, `blocked`, `abandoned` (a worker that died or was stopped; the manager records it
with `amp_report_run(outcome="abandoned", run_note=…)`). `amp_get_task(task_id, include=["runs"])` lists them; the
ticket panel shows them. Abandoned runs show rework and don't count as missing usage.

## Tickets that aren't moving

Three different problems, three different repairs:

| What you see | What happened | What fixes it |
|---|---|---|
| `not_started` — in progress, no worker started it (flagged at 10 min) | nobody launched the worker, or it died before its first call | launch the worker with its `worker_prompt`, or undo the dispatch with `amp_set_task_state(state="backlog", reason="not started")` |
| `returned` — back in ready, with a `returned_reason` | nobody started it before the return timeout (30 min), or a harness released it with `amp_return_task` | it's ready again: dispatch and launch it knowingly, or ask |
| `quiet` — in progress, started, no AMP activity for 15 min | the worker probably died (a crashed session, a hung permission prompt, a laptop closed) | read its comments, record the abandoned run, set it back to `backlog`, dispatch it again |

**Quiet tickets.** Mission and My work flag them and `amp_list_tasks` lists them under `quiet`.
To recover: read the comments to see how far it got, record the abandoned run, set it back with
`amp_set_task_state(state="backlog")`, and dispatch it again. The new worker continues from the
comments and the branch. A worker in a long build without commenting can look quiet too; workers
should comment at each step.

**`amp_return_task(task_id, reason)`** is the release path for a dispatched ticket whose worker
never started it (a crash, a timeout, a cancel, a lost connection): it clears the dispatch, records
why, and puts the ticket back in ready. It refuses a ticket a worker already claimed — a started
run isn't pulled out from under its worker; block it or close it out instead.

A manager's own view of these is `waiting_on_people` kind `start`: a ticket dispatched to you that
no worker has started, which is also one of the "Waiting on you" items in My work.
