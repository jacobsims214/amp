---
title: Planning work and approving plans
tags: [planning, approval, dependencies, plan-check, flight-plan]
---

# Planning work and approving plans

## How a plan is built

Your manager agent turns what you ask for into epics, stories and tickets:
1. Epics and stories first (`amp_create_epic`, `amp_create_story`).
2. Tickets with `amp_create_tasks`: each gets a `ref`, and `depends_on` lists the refs it waits
   on, so the whole graph is wired in one call.
3. `amp_check_plan` before anything runs.

Good tickets:
- **One thing each.** If it needs "and" to describe it, split it.
- **Say the outcome, not the code.** Exact values that matter (a path, a config key) are fine; a
  finished file for the worker to copy isn't.
- **Waves with review gates.** A wave of parallel tickets, then a review ticket that the next wave
  depends on. Reviewers turn problems into fix tickets wired into the next gate.
- **Owned by the right person.** Planning for a teammate is fine (with `owner`), but only their
  agent will run it.

## The plan check

`amp_check_plan` (and the verdict in **Flight plan**) returns:
- **NO-GO** for real problems: tickets connected to nothing (`isolated`), too much work that can
  start at once (`wide_start`), stories with no tickets (`empty_stories`), and an empty project
  KB (`no_context`: the decisions behind the plan live only in the planning session, so write an
  overview and a `decisions/` doc per decision).
- **WARN** for things worth a look:
  - `coupled_gate`: one ticket joins different people's work, so neither can finish alone.
  - `metadata_drift`: an epic is missing metadata keys its siblings carry (often a near-miss
    key like `jira_key` where the others say `jira_epic`).
- **GO** otherwise.

## Fixing a plan

Fix it in place, never by deleting and re-creating: `amp_set_task_dependencies`,
`amp_update_task`, `amp_update_epic` / `amp_update_story`, `amp_move_task`, `amp_move_story`.
Delete only unwanted work; deleted tickets go to the trash and can be restored.

## Approval

When a project has **Plan approval** turned on (Settings), tickets planned since the last
approval can't be dispatched until a person approves. Approval is **your "go" on the plan your
own agent drafted**: the manager waits for its human. Any person on the project can approve
(**Approve plan** in Flight plan, or `amp_approve_plan` when you tell your agent to); agent
accounts can't. Flight plan shows who approved and when. New scope added later needs approving
again.

## Scheduling

A ticket can wait for a time: `start_at` on create, or `amp_set_task_start_at` (an absolute
ISO 8601 time). Until then it sits in `scheduled`.
