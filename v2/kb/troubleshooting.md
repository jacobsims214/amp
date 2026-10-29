---
title: Troubleshooting
tags: [troubleshooting, stuck, hung, quiet, workers, errors, forbidden, mcp, timeout, permissions]
---

# Troubleshooting

## A ticket is "quiet" / a worker seems hung

Most often the worker is stuck on a **permission prompt** nobody can answer: a headless subagent
waits forever. The usual trigger is touching files outside the project directory, e.g. a git
worktree in a sibling folder. Fix: worktrees inside the repo (`.worktrees/<ticket>`), and keep
workers in the project. Then recover the ticket: read its comments, record the abandoned run, set
it back to backlog and re-dispatch (`working/running-work.md`).

Other causes: the session that ran it ended (background tasks die with it), the model provider
stalled (set stream timeouts), or a long build without comments (it may still be fine; check).

## The `amp_*` tools are missing or failing to connect

AMP restarted, or the harness started while AMP was unreachable. Restart the harness. Agents
should stop and say so; they must never work around it through the web UI's API, someone else's
token, or AMP's source.

## "Forbidden" on dispatch or complete

The ticket isn't yours. AMP only lets the owner's agent run a ticket. Ask its owner, or have a
lead hand it over (`working/changes-outside-amp.md`). Don't work it anyway.

## Dispatch is refused because the plan isn't approved

The project requires plan approval and the ticket was planned (or edited) since it was last
approved. Approval is per ticket: its owner approves it in the project's **My work** tab in the web UI, or a
project lead approves it on their behalf; the refusal carries the link
(`working/planning-and-approval.md`). Your click doesn't approve a teammate's tickets, and an
agent can't approve anything for you.

## "… is done by a person in the AMP web UI, not by an agent or over MCP"

The agent tried a step only a person can take: approving a plan or an artifact, marking a human
ticket done, or turning plan approval off. Open the link in the message and do it there. Your
agent should stop and hand you the link; it isn't meant to find another way.

## "it was never dispatched and has no work recorded on it"

An agent tried to complete or close out a ticket that has no dispatches and no runs: there is no
work on it to complete. Give it a worker (dispatch it once the plan is approved and launch that
worker), or complete it yourself in the web UI. A ticket that *does* have runs — a worker blocked
with a question and the answer arrived, work done in an earlier run — can be closed out directly;
the manager passes `closeout_reason` and doesn't need to dispatch it first.

## "dispatched · not started" won't clear

A ticket shows it when no worker has claimed it since it was dispatched. Usually the manager
dispatched and the worker never launched (or died before its first call). It clears by itself:
AMP flags it at 10 minutes and returns it to ready at 30 (`not_started` then `returned` in
`amp_list_tasks`). To act sooner: launch the worker, or have the manager undo the dispatch with
`amp_set_task_state(state="backlog", reason="not started")`, or release it with
`amp_return_task(task_id, reason)`. Both timeouts are per project in Settings; `0` turns the
watchdog off, which puts you back to a silent dispatch that stays in progress.

If it keeps happening, the manager is stopping after `amp_dispatch_task`: the result says to
launch the worker in the same turn with its `worker_prompt`, and an old plugin doesn't know that —
update the plugin (`setup/connect-your-agent.md`).

## "Closed out by … (not the worker)" on my ticket

That's a closeout: someone other than the ticket's worker completed it, because the worker
couldn't (it died, timed out, or lost its connection), or the work was already there and only the
completion was missing. It's on the ticket's history with the reason, and the run has a
`closeout:` note. Nothing is wrong with the work; the label records who closed it.

## A ticket went back to ready by itself

Nobody started it in time. AMP returns a dispatched ticket that no worker claims after the
project's timeout (30 minutes by default), with `returned_reason` like "not started within 30 min
of dispatch to <agent>". It shows in `amp_list_tasks`' `returned` list until it is dispatched
again. Re-dispatch it and make sure the worker actually starts.

## Spend looks too low

Runs without usage: see the "reported no usage" count in Reports and Portfolio. Make sure your
harness can report usage (`setup/connect-your-agent.md`), and that the manager records usage for
subagents when the harness gives it.

## A ticket is blocked but its dependencies are done

Run `amp_check_plan`; it repairs tickets left stuck that way.

## I deleted the wrong thing

Tickets, stories and epics: the trash (`admin/trash-audit-and-projects.md`).

## AMP Harness problems

Run `amp-harness doctor`: every failing check says how to fix it. More in
`harness/troubleshooting.md`.
