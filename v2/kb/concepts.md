---
title: Concepts — how AMP models work
tags: [concepts, onboarding, tickets, dependencies]
---

# Concepts

## The hierarchy

- **Org** — a company or business unit. Has owners, admins and members, an org knowledge base,
  and a Portfolio of its projects.
- **Team** — people who work together. Teams are linked to projects, as an *owner* team (they
  run the project) or a *member* team (they work in it). Each team has a lead and its own KB.
- **Project** — a body of work with its own board, plan, reports, knowledge base and settings.
- **Epic → story → ticket.** An epic is an outcome ("B2B quotes"); a story is a slice of it; a
  **ticket** (task) is one small unit of work that one agent can finish in one run: one file, one
  function, one concern.

## Dependencies are the plan

AMP runs work as a dependency graph (DAG). Every ticket whose dependencies are complete is ready
to dispatch, and all ready tickets can run at once. A ticket with no dependencies is ready
immediately, so a missing dependency means work starts too early and fails. Dependencies often
cross stories and epics (the API before the UI that calls it).

State follows from dependencies: a ticket is `backlog` (ready once its dependencies are done),
`blocked` (waiting on dependencies, or on a person's answer), `in_progress` or `completed`.
Don't set state by hand to work around a missing dependency; add the dependency.

Those four states are the whole state machine. Two things you see in the UI are *within*
`in_progress`: **dispatched · not started** (sent to an agent, but no worker has claimed it yet;
AMP returns it to ready if nobody does) and **quiet** (started, then silent for 15 minutes). Both
are derived on read, so nothing about a ticket's state means something new to an old client.

## People and agents

- Every ticket has an **owner**: a person. The ticket is theirs whichever agent works it.
- A person's **manager agent** (in their own harness: Claude Code, opencode, …) plans and runs
  that person's tickets, calling AMP's MCP tools with that person's token.
- **Workers** are subagents the manager starts for one ticket each. They read the ticket, do the
  work, comment as they go, and complete or block it with a run record.
- AMP enforces ownership: only the owner (or their agent) can dispatch, work and complete a
  ticket. Anyone on the project can read it and comment on it. Leads can hand work over.
- **Agent accounts** are separate AMP users for unattended agents (CI, bots). They can do work
  but can't approve plans. No agent can, whoever it runs as: plans and artifacts are approved,
  and human tickets marked done, by a person in the web UI.
- **Human tickets** are done by their owner, not an agent: a sign-off, a decision, a
  step that needs access agents don't have. (Merging a PR isn't one: the ticket shows **Merge
  needed** and the work after it waits for the merge; see `working/merging-and-ci.md`.) They sit in the same dependency graph, so the agent
  work after them waits for the person. Agents never dispatch them. See `working/human-work.md`.

## Access levels on a project

From least to most: **Member** (on a linked team), **Lead** (lead of a linked team), **Owner**
(on an owner team). Org owners and admins can see and manage everything in their org. See
`admin/orgs-teams-and-access.md`.

## The ticket's record

Everything that happens to a ticket is kept: comments, state changes, who dispatched it, every
**run** (agent, model, tokens, cost, branch, commit, PR, pushed, merged, outcome) and its plan
context. `amp_get_task` with `include: ["comments","history","runs"]` returns all of it in one call. That record is what makes
standups, spend reports and traceability work without anyone writing status updates.

## Metadata

Projects, epics and tickets carry free-form **metadata**: key/value pairs such as a tracker key
(`jira_epic`, `linear_team`, `ado_area_path`), `cost_center` or `capitalizable`. AMP assumes no
particular tracker or finance scheme. Reports and the Portfolio group by whichever keys your org
actually uses. See `visibility/reports-and-spend.md`.
