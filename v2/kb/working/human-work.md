---
title: People in the loop — human tickets and My work
tags: [human, people, review, approval, waiting, cycle-time, planning, my-work, waiting-on-you]
---

# People in the loop: human tickets and My work

Some steps need a person: someone signs off the release, logs into the vendor console, makes a
call. In AMP that step is a **human ticket**. It's a ticket in the same plan, done by its owner
instead of an agent, so it gets tracked just like agent work. The plan shows who it's waiting
on, and the reports show what that waiting costs.

Merging a pull request is **not** a human ticket. A ticket that finishes with a PR shows
**Merge needed**, and the work after it waits for the merge on its own
(`working/merging-and-ci.md`).

## Waiting on you

Everything a person has to act on in a project is **waiting on you**. There are four kinds,
always shown in this order, oldest first:

| Kind | What it is | Where it comes from |
|---|---|---|
| **Approve** | Tickets that wait for your plan approval, one item per epic | `working/planning-and-approval.md` |
| **Answer** | A question an agent asked on a ticket you own | `working/questions.md` |
| **Merge** | A ticket you own is done, but its PR isn't merged (or was closed without merging) | `working/merging-and-ci.md` |
| **Do** | A human ticket you own that is ready or in progress | this page |

**Coming up** lists your human tickets that can't start yet: they wait on other work, a
schedule, or plan approval. They're shown but not counted, and move into Do when they're ready.

Questions on other people's tickets wait on their owner, not on you.

AMP works this out in one place, so every count agrees: the project's **My work** tab, the
badges in the sidebar, the top-level My work page, Mission, and what your manager agent sees
(`amp_list_tasks` → `waiting_on_people`, each item with a `kind`: `approve`, `answer`, `merge`
or `do`, plus a separate `coming_up` list).

## The My work tab: where you act

Each project has a **My work** tab (`/project/<id>/my-work`). It's the one place you act in a
project, and every action happens in place:

- **Approve**: a card per epic with **Review and approve N**. It opens the Approve view, where
  you read every ticket and approve all or some of them.
- **Answer**: the question, suggested answers when the question offers a choice, a text box and
  **Send answer**.
- **Merge**: the PR and how many tickets wait on it, **Review PR ↗**, **Mark merged** and **Not
  needed**. A PR closed without merging offers **Reopen ticket** and **Not needed**.
- **Do**: **Start**, then **Mark done…**, which takes a note, a link and the hours you spent.

Next to the list: **Coming up** (with what each ticket waits on), **Your agents here** (counts,
linking to the Board), and **Done today**. When nothing waits on you, the tab says you're clear.

The tab shows a badge with the number waiting on you ("My work, 3 waiting on you"). The same
number shows on the project in the sidebar, and the sidebar's top **My work** shows your total
across projects.

**Opening a project** lands on My work when something there waits on you, and on Mission
otherwise. A link to a specific tab always opens that tab.

**Links to one item.** Every project page takes `?focus=<ticket id>`: My work scrolls to the item
holding that ticket and outlines it; the Board and Flight plan open the ticket. Agents and AMP's
own messages link this way, so a link always opens the thing it's about.

The same actions appear in the ticket panel when you open a ticket anywhere (the Board, Flight
plan): Answer, Mark merged, Start and Mark done. Approval is the exception; the panel says
**Awaiting your approval → My work**.

A lead can open a teammate's list (it's read-only: the actions are theirs) to see what's waiting
on them.

## Everywhere else is a summary

- **My work** at the top of the sidebar (`/`) is your summary across projects: a card per
  project with something waiting, its counts by kind, and its oldest items. Nothing on it is a
  button; each row opens that project's My work with the item focused.
- **Mission** shows the project for everyone, and doesn't act either. See `ui-tour.md`.

## Human tickets

- **It sits in the dependency graph like any ticket.** Agent tickets that need it depend on it,
  so they wait for the person and become ready the moment the ticket is marked done.
- **Agents never pick it up.** It can't be dispatched. The manager doesn't offer it as ready
  work; it lists it as **waiting on people**: who, for how long, and what it's holding up.
- **Its owner does it in AMP**, in their My work tab (or the ticket panel):
  1. Click **Start**.
  2. When you're done, click **Mark done…**. You can add a note, a link and the hours you
     actually spent.

  Only you can mark it done, and only in the web UI. Your manager agent can't record it for
  you (`amp_complete_task` on a human ticket is refused over MCP); if you tell it you've done
  it, it gives you the link.
- **It's never flagged as quiet.** People work in days, not the minutes an agent takes. It shows as
  **stale** after two days with no activity.

### Why a person: the reasons

Every human ticket says why a person does it. Reports group by this, so you can see what kind of
work your org does by hand and whether that's expected.

| Reason | When |
|---|---|
| Review | A person reviews something that isn't a merge: a document, a design outside AMP, a vendor's work |
| Approval | A sign-off only a person can give (release, spend, legal, security) |
| Access | Needs access or credentials agents don't have |
| Judgment | A decision or judgment call |
| External | Depends on someone outside the team |
| Physical | Offline work: hardware, a site visit, a phone call, a signature |
| Other | Anything else a person has to do |

### When to plan one, and when not

Plan a human ticket for a step you can see coming that only a person can do. Don't plan one for:
- **Merging a PR.** That's Merge needed, recorded by GitHub, CI or **Mark merged**
  (`working/merging-and-ci.md`).
- **A question that comes up while an agent works.** The agent blocks its own ticket with the
  question, and you answer it (`working/questions.md`).
- **Approving a mockup.** That's the design ticket's artifact review (`working/artifacts.md`).
- **Approving the plan.** That's the Approve item in each person's My work tab, for their own
  tickets (or a lead on their behalf).

A good human ticket:
- has **one person and one action**: "Sign off the release notes", not "QA and release";
- says **what done means** for a person: "release approved in the vendor portal";
- gives them **what they need**: the link, what to check, where to do it;
- is **wired both ways**: it waits on the work it needs, and the work after it waits on it.

Managers plan these with `amp_create_tasks`: `performer: "human"`, a `human_reason`, and `owner`
set to the person. A human ticket must have an owner; the plan check calls one without an owner a
NO-GO, since nobody would ever do it. `amp_check_plan` lists every human ticket under
`human_gates`, with how much work waits on each.

Older projects may still have "review and merge" human tickets. They keep working: marking one
done with the PR link also records that PR as merged on the tickets that produced it.

### In the app

- **Board:** a human ticket shows **Person · Judgment** (or its reason) in place of an agent.
- **Ticket panel:** says who does it and why; for its owner, **Start** and **Mark done…**.
  - To change who does it: **Edit → Done by** (an agent or a person). You can't change it while
    work is in progress.
  - Every change is on the ticket's history.
- **New ticket:** choose **Done by: A person**, then the reason and the owner.
- **Reports → People in the loop:**
  - agents vs people: tickets, how long each **waited to start** (ready → started) and **took
    once started** (started → done), and the hours people reported;
  - human work by reason;
  - the human tickets that held the plan up longest, with how many tickets waited on each.
- **Day reports** mark human items, e.g. `(human: approval)`.

## Reading the numbers

- **Waited to start** shows hand-offs: work sitting ready while nobody picked it up. A long wait
  usually means the step needs a time slot, not that it's slow.
- **Took once started** vs **hours reported**: elapsed time includes everything else a person
  did that day. Hours reported is the work itself.
- **Held up** is the open work downstream right now. One human ticket holding up ten agent
  tickets is the place to look first.
- **By reason** answers "what does AI not do for us here?" Approval is expected. A
  lot of Access often means agents are missing a tool or a permission, which is a gap you can
  close. Then the work can move to agents: switch the ticket with **Done by: An agent**.

Agents read the same numbers with `amp_human_impact(project_id, days)`.
