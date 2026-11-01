---
title: A tour of the AMP UI
tags: [ui, onboarding, navigation]
---

# A tour of the AMP UI

## Sidebar

- **My work** — your home page: a summary of what waits on you across every project (Approve,
  Answer, Merge, Do), one card per project, plus your agents at work and what finished lately.
  It only informs: each row opens that project's My work with the item in focus. Its badge is
  your total waiting.
- **Projects** — every project you can see. A project with something waiting on you shows the
  count.
- **Day reports** — your saved end-of-day reports and your completed tickets.
- **Trace** — paste a commit sha, branch or PR URL and get back the tickets, plan, comments and
  runs that produced it (`visibility/traceability.md`).
- **AMP guide** — this guide.
- **Portfolio** — every project in your org: progress, open questions and agent spend, grouped
  by your org's own metadata (`visibility/reports-and-spend.md`).
- **Agents & tokens** — API tokens for your harness, and agent accounts you manage
  (`setup/connect-your-agent.md`).
- **Teams & orgs** — membership, roles, teams, and the team and org knowledge bases.
- **Your name** at the bottom of the sidebar shows your role; the arrow next to it signs you out.

## Inside a project

Opening a project lands on **My work** when something there waits on you, and on **Mission**
otherwise. Every project page takes `?focus=<ticket id>` to open on one ticket.

- **Mission** — the project at a glance, for everyone. It informs and has no action buttons:
  - a **Waiting on you** strip with your counts by kind and a link to My work (or "Nothing waits
    on you here");
  - readouts for the project's work, including **Merge needed** and questions, each with how many
    are yours;
  - the epics, what's in progress, quiet tickets (no activity for 15 minutes: the worker likely
    died) and the live activity feed;
  - **Waiting on people**: each person with what waits on them, by kind. Rows open the item in
    that person's My work.
- **My work** — where you act in this project: approve plans, answer questions, record merges,
  and start and finish your own human tickets, each in place (`working/human-work.md`). Its badge
  is the number waiting on you here. Approving opens the Approve view
  (`working/planning-and-approval.md`).
- **Board** — tickets by state. Filter by person (**Mine** is the tickets you own; **Unowned**
  finds the rest), epic or text. A done ticket whose PR isn't merged shows **Merge needed**.
  **Hand over …** moves the filtered tickets to someone else with a reason.
- **Flight plan** — the plan as epics, stories and tickets in order, and the plan check verdict
  (GO / WARN / NO-GO). It's for reading: when tickets await your approval it says so and links
  to My work.
- **Graph** — the dependency graph.
- **Reports** — progress, velocity, people (with who each person is blocked by), the standup
  sheet, tokens and cost by model, epic, person and metadata, with a Markdown export.
- **Artifacts** — mockups, diagrams and other pages agents published for review
  (`working/artifacts.md`).
- **Library** — the project's knowledge base, plus links to the team and org KBs and this guide.
- **Settings** — general (including which org the project is in), plan approval, merges (whether
  dependents wait for merges), metadata, team access, knowledge bases, trash (restore deleted
  tickets), and for owners the GitHub merge webhook (`working/merging-and-ci.md`), webhooks, the
  audit log and the danger zone (archive, reset).

Each ticket opens a side panel with its description, acceptance criteria, comments, runs
(model, tokens, cost, git details) and history. When the ticket waits on you, the panel has the
same actions as My work (Answer, Mark merged, Start, Mark done); for approval it links to My
work.
