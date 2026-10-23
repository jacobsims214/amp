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
approved. Approval is per ticket: its owner approves it in **Flight plan** in the web UI, or a
project lead approves it on their behalf; the refusal carries the link
(`working/planning-and-approval.md`). Your click doesn't approve a teammate's tickets, and an
agent can't approve anything for you.

## "… is done by a person in the AMP web UI, not by an agent or over MCP"

The agent tried a step only a person can take: approving a plan or an artifact, marking a human
ticket done, or turning plan approval off. Open the link in the message and do it there. Your
agent should stop and hand you the link; it isn't meant to find another way.

## "never dispatched … Dispatch it first"

An agent tried to complete a ticket that was never dispatched. Dispatch it (after the plan is
approved) and let the worker finish it.

## Spend looks too low

Runs without usage: see the "reported no usage" count in Reports and Portfolio. Make sure your
harness can report usage (`setup/connect-your-agent.md`), and that the manager records usage for
subagents when the harness gives it.

## A ticket is blocked but its dependencies are done

Run `amp_check_plan`; it repairs tickets left stuck that way.

## I deleted the wrong thing

Tickets, stories and epics: the trash (`admin/trash-audit-and-projects.md`).
