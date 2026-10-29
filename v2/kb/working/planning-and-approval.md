---
title: Planning work and approving plans
tags: [planning, approval, dependencies, plan-check, my-work, approve-view]
---

# Planning work and approving plans

## How a plan is built

Your manager agent turns what you ask for into epics, stories and tickets:
1. Epics and stories first (`amp_create_epic`, `amp_create_story`).
2. Tickets with `amp_create_tasks`: each gets a `ref`, and `depends_on` lists the refs it waits
   on, so the whole graph is wired in one call.
3. `amp_check_plan` before anything runs.
4. It presents the plan, tells you which of **your** tickets await approval, gives you the
   approval link (`approve_url` from `amp_check_plan`, which opens your My work tab), and stops
   until you've approved them in the web UI.

Good tickets:
- **One thing each.** If it needs "and" to describe it, split it.
- **Say the outcome, not the code.** Exact values that matter (a path, a config key) are fine; a
  finished file for the worker to copy isn't.
- **Waves with review gates.** A wave of parallel tickets, then a review ticket that the next wave
  depends on. Reviewers turn problems into fix tickets wired into the next gate.
- **Owned by the right person.** Planning for a teammate is fine (with `owner`), but only their
  agent will run it.
- **People in the loop where a person has to act.** A step only a person can do (a sign-off,
  access agents don't have, a call) is a human ticket. The work after it waits for that person.
  See `working/human-work.md`.
- **No merge tickets.** Don't plan "review and merge" steps: a ticket that finishes with a PR is
  **Merge needed**, and the work after it waits for the merge by itself
  (`working/merging-and-ci.md`).

## The plan check

`amp_check_plan` (and the verdict in **Flight plan** and the Approve view) returns:
- **NO-GO** for real problems: tickets connected to nothing (`isolated`), too much work that can
  start at once (`wide_start`), stories with no tickets (`empty_stories`), and an empty project
  KB (`no_context`: the decisions behind the plan live only in the planning session, so write an
  overview and a `decisions/` doc per decision), and a human ticket with no owner
  (`human_without_owner`).
- **WARN** for things worth a look:
  - `coupled_gate`: one ticket joins different people's work, so neither can finish alone.
  - `metadata_drift`: an epic is missing metadata keys its siblings carry (often a near-miss
    key like `jira_key` where the others say `jira_epic`).
- **GO** otherwise.

It also lists `human_gates`: each open human ticket and how many tickets wait on it.

## Fixing a plan

Fix it in place, never by deleting and re-creating: `amp_set_task_dependencies`,
`amp_update_task`, `amp_update_epic` / `amp_update_story`, `amp_move_task`, `amp_move_story`.
Delete only unwanted work; deleted tickets, stories and epics go to the trash and can be restored.

## Approval

**Plan approval** is on by default (Settings). It's **per ticket**: a ticket can't be dispatched
until it's approved, and each ticket is approved by its owner. Approval is **your "go" on the
plan your own agent drafted**, and only a person gives it, in the web UI, in the project's
**My work** tab. It's the one place a plan is approved; Flight plan, the Board and the ticket
panel only link there ("N tickets await your approval → My work").

### In My work

Each epic with tickets awaiting you is an **Approve** item in your My work tab
(`working/human-work.md`). The card shows the epic, how many tickets, who planned it, and a line
per story. **Review and approve N** opens the **Approve view**; when several epics wait on you,
**Review both plans together** (or all of them) opens them in one view.

The Approve view shows what you're approving, top to bottom:
- **What approving does:** **Starts right away** (the tickets that start the moment you
  approve), **Steps for you** (your human tickets in the plan), **Since your last approval** (new
  and changed tickets) and the **Plan check**, with any warnings listed below.
- **The plan, in run order:** stories, and the tickets in each, in the order they can run. A
  **step** is when a ticket can start: step 1 needs nothing else in the plan, step 2 waits on
  step 1, and so on; tickets in the same step run in parallel. "after #x" chips show where a
  ticket waits on another story or on work outside the plan.
- **The ticket reader:** select a ticket (or use J/K) to read its full description and acceptance
  criteria, what it runs after and what it unlocks, and, for a ticket edited since you approved
  it, what changed.
- **The approval bar:** **Approve N tickets**.

You can approve **all or some**. Your own tickets are ticked; untick a ticket, a story or an epic
to leave it out. A left-out ticket keeps waiting for you, and ticked tickets that depend on it
are **Held**: approved, but they can't start until you approve the one you left out. The bar
offers to leave those out too.

The approval covers the tickets **as you saw them**. If someone edits a ticket while you're
reading, it isn't approved; the view says how many changed and offers to review them.

`/project/<id>/my-work?approve=all` opens everything awaiting you in the project; that's the
link your manager gives you.

### Whose tickets

- **Your own tickets.** You approve the tickets that are yours (you own them, or unowned ones you
  planned).
- **Other people's tickets** wait for their owners, in their My work. Two people planning in one
  project each approve their own; your click doesn't start a teammate's work.
- **On someone's behalf.** A project lead (or owner, org admin) can approve others' tickets on
  their behalf in the Approve view; they're shown separately and unticked. The ticket's history
  says "Plan approved on behalf of <owner> by <lead>". Plain members approve only their own.

Each ticket records who approved it and when; Flight plan shows the project's last approval.

No agent can approve, not even your own manager running under your login. `amp_approve_plan`
only reports the approval status and the link; it never approves. Saying "yes" or "go" in chat
doesn't approve anything either: the manager answers with the link and keeps waiting. That's
deliberate. A plan is approved only when a person has read it and clicked.

What your manager does:
1. Plans, runs `amp_check_plan`, and presents the plan. `amp_check_plan` lists the tickets
   awaiting approval with their owners (`awaiting_approval_tickets`) and which are yours
   (`awaiting_yours`).
2. Tells you which of **your** tickets await approval, gives you the `approve_url` (your My work
   link), mentions any others' tickets that wait for their owners (or a lead on their behalf),
   and **stops**.
3. Dispatches each ticket only once it's approved, that is, after you (or a lead on your behalf)
   have clicked.

A ticket needs approving again when it's created, restored from the trash, or its name,
description or acceptance criteria change. Comments, priority, assignee and dependency changes
don't. A dispatch of such a ticket is refused with "ticket N isn't approved … Its owner approves
it in the AMP web UI (My work, Review and approve; a project lead may approve it on their
behalf): <link>".

Agents also can't complete a ticket that was never dispatched and has no runs on it ("it was never
dispatched and has no work recorded on it"), so work can't skip the gate by being marked done. A
ticket that already has runs is closed out rather than dispatched again — that's a labelled
closeout, not a bypass (`working/running-work.md`).

Only a person, in the UI, can turn **Plan approval** off for a project; agents can turn it on.

## Asking where things stand

"What's on the board?", "what's waiting on me?" and similar questions are read-only. Your
manager answers from the board (`amp_list_tasks`, `amp_get_plan`, `amp_check_plan`,
`amp_list_unmerged`) without commenting, claiming, dispatching or completing anything, and gives
you links for anything that needs you: a plan to approve, a question, a merge, a human ticket. What waits on you is the same list as
your My work tab. It only
touches the tickets you ask it to work on.

In the AMP harness chat you don't need to go back and say you've approved: the chat waits for
your approval in AMP and starts the manager's next turn by itself once it sees it
(`harness/running-work.md`).

## Scheduling

A ticket can wait for a time: `start_at` on create, or `amp_set_task_start_at` (an absolute
ISO 8601 time). Until then it sits in `scheduled`.

For anything you should see before it is built (a new screen, a redesign, a diagram), the plan
starts with a design ticket and the build depends on it: see `working/artifacts.md`.
