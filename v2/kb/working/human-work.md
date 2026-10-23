---
title: People in the loop — human tickets
tags: [human, people, review, approval, waiting, cycle-time, planning, my-work]
---

# People in the loop: human tickets

Some steps need a person: someone reviews and merges the PR, signs off the release, logs into the
vendor console, makes a call. In AMP that step is a **human ticket**. It's a ticket in the same
plan, done by its owner instead of an agent, so it gets tracked just like agent work. The
plan shows who it's waiting on, and the reports show what that waiting costs.

## How it works

- **It sits in the dependency graph like any ticket.** Agent tickets that need it depend on it,
  so they wait for the person and become ready the moment the ticket is marked done. The chain
  of work isn't broken by "and then someone has to merge it."
- **Agents never pick it up.** It can't be dispatched. The manager doesn't offer it as ready
  work; it lists it as **waiting on people**: who, for how long, and what it's holding up.
- **Its owner does it in AMP:**
  1. Open the ticket and click **Start**.
  2. When you're done, click **Mark done**. You can add a note, a link (the PR, the doc) and the
     hours you actually spent.

  Only you can mark it done, and only in the web UI. Your manager agent can't record it for
  you (`amp_complete_task` on a human ticket is refused over MCP); if you tell it you've merged,
  it gives you the ticket's link.
- **It's never flagged as quiet.** People work in days, not the minutes an agent takes. It shows as
  **stale** after two days with no activity.

## Why a person: the reasons

Every human ticket says why a person does it. Reports group by this, so you can see what kind of
work your org does by hand and whether that's expected.

| Reason | When |
|---|---|
| Review | A person reviews or merges (code review, PR merge) |
| Approval | A sign-off only a person can give (release, spend, legal, security) |
| Access | Needs access or credentials agents don't have |
| Judgment | A decision or judgment call |
| External | Depends on someone outside the team |
| Physical | Offline work: hardware, a site visit, a phone call, a signature |
| Other | Anything else a person has to do |

## When to plan one, and when not

Plan a human ticket for a step you can see coming that only a person can do. Don't plan one for:
- **A question that comes up while an agent works.** The agent blocks its own ticket with the
  question, and you answer it (`working/questions.md`).
- **Approving a mockup.** That's the design ticket's artifact review (`working/artifacts.md`).
- **Approving the plan.** That's Flight plan's Approve, done in the web UI by each person for
  their own tickets (or a lead on their behalf).

A good human ticket:
- has **one person and one action**: "Review and merge: quotes API PR", not "QA and release";
- says **what done means** for a person: "PR merged to main";
- gives them **what they need**: the link, what to check, where to do it;
- is **wired both ways**: it waits on the work it needs (the PR exists), and the work after it
  waits on it (deploy after the merge).

Managers plan these with `amp_create_tasks`: `performer: "human"`, a `human_reason`, and `owner`
set to the person. A human ticket must have an owner; the plan check calls one without an owner a
NO-GO, since nobody would ever do it. `amp_check_plan` lists every human ticket under
`human_gates`, with how much work waits on each.

## In the app

- **Board:** a human ticket shows **Person · Review** (or its reason) in place of an agent.
- **Ticket:** a panel says who does it and why. For its owner, it has **Start** and **Mark
  done**.
  - To change who does it: **Edit → Done by** (an agent or a person). You can't change it while
    work is in progress.
  - Every change is on the ticket's history.
- **New ticket:** choose **Done by: A person**, then the reason and the owner.
- **My Work:** your human tickets are under **To do myself**, ready ones first.
- **Reports → People in the loop:**
  - agents vs people: tickets, how long each **waited to start** (ready → started) and **took
    once started** (started → done), and the hours people reported;
  - human work by reason;
  - the human tickets that held the plan up longest, with how many tickets waited on each.
- **Day reports** mark human items, e.g. `(human: review)`.

## Reading the numbers

- **Waited to start** shows hand-offs: work sitting ready while nobody picked it up. A long wait
  on review tickets usually means reviews need a time slot, not that reviewing is slow.
- **Took once started** vs **hours reported**: elapsed time includes everything else a person
  did that day. Hours reported is the work itself.
- **Held up** is the open work downstream right now. One human ticket holding up ten agent
  tickets is the place to look first.
- **By reason** answers "what does AI not do for us here?" Review and approval are expected. A
  lot of Access often means agents are missing a tool or a permission, which is a gap you can
  close. Then the work can move to agents: switch the ticket with **Done by: An agent**.

Agents read the same numbers with `amp_human_impact(project_id, days)`.
