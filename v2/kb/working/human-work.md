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
(`working/merging-and-ci.md`). It reaches you as a **Merge** item, never as a Do item, and it's
gone the moment the merge is recorded — by you, by CI, or by GitHub. Nothing asks you to
acknowledge a merge that already happened, and a merge onto an integration branch is not a chore:
whether it reaches your list at all depends on whether AMP could read that branch's rules (see
**Merge** below, and `working/merging-and-ci.md`).

## Waiting on you

Everything a person has to act on in a project is **waiting on you**. There are five kinds,
always shown in this order, oldest first:

| Kind | What it is | Where it comes from |
|---|---|---|
| **Approve** | Tickets that wait for your plan approval, one item per epic | `working/planning-and-approval.md` |
| **Answer** | A question an agent asked on a ticket you own | `working/questions.md` |
| **Merge** | A ticket you own is done with a PR (or a pushed branch) that isn't merged yet, or whose PR was closed without merging | `working/merging-and-ci.md` |
| **Start** | A ticket you own was dispatched to an agent and no worker has started it, so nothing is happening on it | `working/running-work.md` |
| **Do** | A human ticket you own that is ready or in progress | this page |

**Start** is the odd one: there's nothing wrong with the ticket, its worker was simply never
launched (a dispatch whose run died, or was never started). It shows an agent's ticket in
in-progress, and it appears once the project's flag time has passed (10 minutes by default), so
an ordinary slow start doesn't bother you. Nothing you click is required: leave it and AMP
returns the ticket to ready by itself after the project's return timeout (30 minutes by
default). **Return to ready** on the item does the same thing now, and clears the dispatch so
the ticket can be dispatched again.

**Merge** is one item per PR, never one per step. It clears the moment the merge is recorded,
however it was recorded, so a merge GitHub or CI already recorded never becomes a question for
you. A plan's `merge_for` step for that PR is folded into the item (see below) rather than counted
as a second thing to do. A merge onto an integration branch asks you for no acknowledgement; it
only appears as an item when AMP couldn't verify the branch's rules, and then it says so.

**Coming up** lists your human tickets that can't start yet: they wait on other work, a
schedule, or plan approval. They're shown but not counted, and move into Do when they're ready.

Questions on other people's tickets, and dispatches of them nobody started, wait on their
owner, not on you.

AMP works this out in one place, so every count agrees: the project's **My work** tab, the
badges in the sidebar, the top-level My work page, Mission, and what your manager agent sees
(`amp_list_tasks` → `waiting_on_people`, each item with a `kind`: `approve`, `answer`, `merge`,
`start` or `do`, plus a separate `coming_up` list).

## The My work tab: where you act

Each project has a **My work** tab (`/project/<id>/my-work`). It's the one place you act in a
project, and every action happens in place:

- **Approve**: a card per epic with **Review and approve N**. It opens the Approve view, where
  you read every ticket and approve all or some of them.
- **Answer**: the question, suggested answers when the question offers a choice, a text box and
  **Send answer**.
- **Merge**: one item per PR, saying what its evidence is. `PR #N · head branch → main` names the
  target GitHub's own read of the pull request reported (that outranks a run's `base_branch`,
  which is only a hint and is shown as such), and **target not verified** where nothing reported
  one — never a guessed `main`. **Why you're asked** is the classifier's reason with a chip
  naming its evidence: `default: main · detected from GitHub` when the PR targets the
  repository's detected default branch; a warning chip (`default: main · rules: unknown`, or
  `default branch & rules: unknown`) when the read was missing, stale, partial or ambiguous, so
  the item is shown rather than hidden; and on a rule alert a box holding **the exact
  requirement** (the ruleset and what it demands). A rule alert offers **Review and approve
  PR #N ↗** and nothing else — no **Mark merged**, no **Not needed**: what's owed is your approval
  on GitHub, and the webhook records the merge after it. (AMP can't yet tell whose approval a
  rule wants, so a demanding rule it can't attribute lands on the warning chip today.) Every
  other arm carries **Review PR ↗**, plus **Mark merged** and **Not needed** where they do
  something. Only the tickets that really wait are counted next to the item. Two arms never become
  an item at all, because the server drops them before they reach your list: a PR onto a
  non-default branch where complete, fresh evidence proves no rule requires anyone (agent
  organization, nothing to acknowledge), and a PR GitHub already reports merged (nothing is asked;
  its ticket still reads **Merge needed** until the merge is recorded). When the plan has a merge
  step for that PR it's folded in here as **Tracked by #N**, with its open prerequisites listed and
  **Mark merged** withheld until they're met — one click then records the merge and completes the
  step. When nothing waits on the merge, **Mark merged** is still there but quiet: GitHub or CI
  records it by itself. A PR closed without merging offers **Reopen ticket** and **Not needed**.
- **Start**: the ticket, the agent it went to and how long ago, and **Return to ready** to clear
  the dispatch and dispatch it again. Leaving it alone returns it by itself.
- **Do**: **Start**, then **Mark done…**, which takes a note, a link and the hours you spent.
  Each card says **blocks N · waiting Xd** (or **Nothing waits on it · waiting Xd**), where N is
  every open ticket downstream of it, and the Do cards are sorted by that count, most first.
  Answer and Merge items count the same way. A judgment ticket with a decision doc offers
  **Start** and **Decide in the ticket**, which opens its panel: it is recorded there, on the
  doc, never by Mark done.

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
plan): Answer, Mark merged, Return to ready, Start and Mark done. Approval is the exception; the
panel says **Awaiting your approval → My work**.

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
     actually spent. On a **Judgment** or **Approval** ticket the note is **Your decision** and
     it's required ("Write what you decided"): the ticket is a decision others wait on.

  Only you can mark it done, and only in the web UI. Your manager agent can't record it for
  you (`amp_complete_task` on a human ticket is refused over MCP); if you tell it you've done
  it, it gives you the link.
- **Your note reaches whoever waits on it.** Marking a ticket done posts the note as a comment on
  it, "Decision by <name>: …" for a judgment or approval, "Done by <name>: …" for the other
  reasons (nothing when the note is empty). Every ticket that depends on it then gets the note
  with no extra call:
  - the ticket panel shows **Decided upstream** ("Decided by Jacob on #34: …", linking to #34);
  - an agent's `amp_get_task` and `amp_claim_task` carry it as `upstream_decisions`, and the claim
    tells the worker to follow it. Only direct dependencies are included: a ticket that is done
    and has a note, or one whose decision doc's record is under way (Record decision has claimed
    it), which is included before the ticket is done. An open panel refreshes when a dependency's
    decision changes.
- **A decision opens as a decision doc.** A judgment ticket planned by an agent comes with an
  **ask**: a question, 2 to 4 options with what each costs, the option the agent recommends and
  why. AMP drafts the ticket's decision doc from it (a markdown artifact, kept, never expiring).
  The ticket panel shows it as a card: the **Question**, the **Options** with the recommended one
  marked **Recommended** (nothing is chosen for you), **Something else**, the doc's context,
  trade-offs and reasoning, and **Decided means** (the acceptance criteria).
  - **Who wrote what.** The agent's lines and options have a coloured rule on the left and an
    "Agent" tag when you hover; the lines and options you changed have no mark.
  - The card's header says **blocks N · waiting Xd**, as on the My work card.
  - **Edit doc** opens the doc's markdown (with a preview) and the option editor: 2 to 4 options,
    each with a label and what it costs, and which one is recommended. **Save** writes both as one
    new version. If the doc changed in another tab meanwhile, your draft stays and AMP says "The
    doc changed in another tab. Reload to see it". A doc keeps at most 100 versions. You can edit
    the doc while the ticket still waits on its dependencies, to prepare the decision; recording
    waits until they're done. A new version clears the option you had picked, since the options
    may have changed.
  - **Record decision.** Pick an option (or **Accept recommendation**, or **Something else**),
    write **Your decision** and click **Record decision**. AMP freezes the version you decided on
    (its text and its options), approves and keeps it, marks the ticket done and posts "Decision
    by <name>: <option> — <your decision>" with a link to that version (just "<option>" when your
    decision only repeats its label, as after Accept recommendation). The tickets that wait on
    it receive the option as it reads on that version, your text and the version number. Mark
    done isn't offered on a ticket with a doc. If recording stops part way, the card says
    "Recording didn't finish" after a minute; **Retry** finishes it with what you sent.
  - **After recording** the card reads "Decided by <name> · vN · <date>" and the doc is
    read-only. There is no reopening: to change your mind, plan a new judgment ticket that depends
    on this one and says what it supersedes.

  The doc changes only in AMP, in the web UI, by a person who can work the ticket (its owner, a
  project lead or owner, or anyone on an unassigned ticket): an agent can't
  publish to it, edit it, approve or unkeep it, or record the decision (it is told "Only a person
  records a decision, in AMP: <link>"). A judgment ticket you make yourself in the web UI has no
  doc; you write your decision when you mark it done. Once a ticket has a doc, who does it and
  why are fixed: changing its **Done by** or reason is refused ("This ticket's decision is in its
  doc; record it there or plan a new judgment ticket: <link>"), from the web UI and over MCP.
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
- **Merging a PR**, as a separate step to acknowledge. A PR is Merge needed, recorded by GitHub,
  CI or **Mark merged** (`working/merging-and-ci.md`). If the plan genuinely has to sequence a
  merge after other work, that step is linked to the PR it merges with `merge_for` — it then folds
  into the PR's own Merge item, and one click both records the merge and completes the step. An
  unlinked "review and merge" ticket is a second card for one merge; `amp_check_plan` warns about
  it (`unlinked_merge_step`).
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
NO-GO, since nobody would ever do it. It must also have its **brief**: a description and acceptance
criteria written for the person. A ticket without them is refused, from any agent and in the web
UI (the new-ticket and edit forms mark both required for a person's ticket); older tickets without
one stay editable and get a plan-check warning (`human_without_brief`). An agent's judgment ticket
also needs an **ask**, which drafts its decision doc (above); one without a doc gets
`judgment_without_doc`. `amp_check_plan` lists every human ticket under `human_gates`, with how
much work waits on each.

Older projects may still have unlinked "review and merge" human tickets. They keep working exactly
as before: marking one done with the PR link also records that PR as merged on the tickets that
produced it. Nothing about a ticket without `merge_for` changes — it just doesn't get the folded
card, and a merge recorded by GitHub or CI won't clear it, so its owner still marks it done.

A merge step's completion is never a sign-off someone gave. When a recorded merge finishes a
linked step, the step's history says whose report finished it ("its tracked merge (#N) was
recorded by github") and no run is recorded for its owner.

### In the app

- **Board:** a human ticket shows **Person · Judgment** (or its reason) in place of an agent.
- **Ticket panel:** an open human ticket opens on its brief, not its history. The panel at the
  top says, in order:
  - **what to do**, headed by the reason as a verb (**Decide**, **Sign off**, **Review**, **Do with
    your access**, **Get from someone outside**, **Do offline**, **Do**) and who does it;
  - where it sits (epic › story), its reason, and who planned it;
  - **What you need to do** (the description) and **Done when** (the acceptance criteria, as a
    checklist when they're written as a list). When both are empty it says **The planner left no
    instructions**, names who to ask, and offers **Edit** to whoever can change the plan;
  - **Unblocks N**: the open tickets that depend on it, each a link, or **Nothing waits on this**;
  - for its owner, **Start** and **Mark done…**; on a judgment ticket with a decision doc, the
    decision card (Edit doc, Record decision) in place of the checklist and Mark done….
- **Artifacts:** the **Decisions** filter lists recorded decision docs.

  The rules (agents never pick it up or mark it done) are in the heading's tooltip.
  - To change who does it: **Edit → Done by** (an agent or a person). You can't change it while
    work is in progress.
  - Every change is on the ticket's history. Comments there, an agent's question, and a run's
    note are shown as markdown (lists, code, links).
  - The open tab is in the link (`&tab=details` or `&tab=history`), and **Copy link** in the
    panel's header copies a link to the ticket on that tab. A ticket that isn't started, and an
    open human ticket, open on Details; other tickets open on History.
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
