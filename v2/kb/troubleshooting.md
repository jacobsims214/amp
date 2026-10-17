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

The project requires plan approval and tickets were planned since the last one. Approve in
**Flight plan** (`working/planning-and-approval.md`).

## Spend looks too low

Runs without usage: see the "reported no usage" count in Reports and Portfolio. Make sure your
harness can report usage (`setup/connect-your-agent.md`), and that the manager records usage for
subagents when the harness gives it.

## A ticket is blocked but its dependencies are done

Run `amp_check_plan`; it repairs tickets left stuck that way.

## I deleted the wrong thing

Tickets, stories and epics: the trash (`admin/trash-audit-and-projects.md`).
