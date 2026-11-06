---
title: A tour of the AMP UI
tags: [ui, onboarding, navigation]
---

# A tour of the AMP UI

## Sidebar

- **My work** — your home page: a summary of what waits on you across every project (Approve,
  Answer, Merge, Start, Do), one card per project, plus your agents at work and what finished
  lately. It only informs: each row opens that project's My work with the item in focus. Its
  badge is your total waiting.
  My work is one person's list, so it needs a signed-in AMP user. When sign-in is off (local
  development, no `OIDC_ISSUER_URL`) or your sign-in has no AMP user (not invited, email domain
  not allowed, or email not verified), My work, the project's My work tab and Mission's Waiting on
  you strip say so and show no counts: use the project's Board, which has every ticket, or ask an
  admin for an invite and sign in again. When My work fails to load it shows the error and **Try again**,
  never zeros.
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
otherwise. Every project page that shows tickets takes `?focus=<ticket id>` to open on one
ticket, and switching views keeps it. A focus link to a ticket in another project goes there;
one to a ticket you can't see says so. Board filters (`status`, `q`, `epic`, `owner`, `done`),
the Graph's selected node (`sel`, `chain`) and Trace queries (`q`, `open`) are in the URL too, so
copy the address bar to share exactly what you see.

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
  return a dispatch no worker started to ready, and start and finish your own human tickets, each
  in place (`working/human-work.md`). Its badge is the number waiting on you here. Approving opens
  the Approve view (`working/planning-and-approval.md`).
- **Board** — tickets by state. Filter by person (**Mine** is the tickets you own; **Unowned**
  finds the rest), epic or text. Status filters split the two kinds of wait: **Needs a person**
  (an agent's question, red, `status=question`) and **Waiting on other tickets** (dependencies or
  a merge, grey, `status=waiting`); the Waiting column's header counts each. A ticket nobody has
  approved yet shows **Needs approval** on its card and in its drawer, and the drawer's State
  uses the same words as its badge (Ready, Waiting, Question …). A done ticket whose PR isn't
  merged shows **Merge needed**.
  **Hand over …** moves the filtered tickets to someone else with a reason. A ticket's drawer
  has **Story context** under its story (on Details): the story's description and acceptance
  criteria and the epic's description, as the planner wrote them. The Approve view has the same
  for each story (**About this story**).
- **Flight plan** — the plan as epics, stories and tickets in order, and the plan check verdict
  (GO / WARN / NO-GO). It's for reading: when tickets await your approval it says so and links
  to My work. Finished stories and epics are hidden by default (**Hide done**, kept in the URL as
  `?done=show` when off); epics and stories are ordered by last update, newest first, and
  **Jump to active** scrolls to the first such ticket. Each stage header counts its open tickets.
  Click a plan-check row that found something to narrow the plan to the tickets (or stories, epics)
  it flagged: they're underlined and ringed, the rest drop out, and the row lists every ticket. It's
  kept in the URL as `?check=<kind>` (e.g. `?check=isolated`); click the row again, **Clear** or Esc
  to go back.
- **Graph** — the dependency graph.
- Text people and agents write (descriptions, questions, comments, day-report summaries, KB
  annotations, artifact version notes, the plan check) is rendered as markdown everywhere it
  has room; one-line and clamped places (a card's question, a Graph node, a project card) show
  its words without the markdown syntax. A link to another AMP page opens in-app (a preview
  instead is proposed in `design/link-previews.md`).
- **Reports** — progress, velocity, people (with who each person is blocked by), the standup
  sheet, tokens and cost by model, epic, person and metadata, with a Markdown export.
- **Artifacts** — mockups, diagrams and other pages agents published for review
  (`working/artifacts.md`).
- **Library** — the project's knowledge base, plus links to the team and org KBs and this guide.
- **Settings** — general (including which org the project is in), plan approval, merges (whether
  dependents wait for merges), metadata, team access, knowledge bases, trash (restore deleted
  tickets), and for owners the GitHub token and the GitHub merge webhook, then webhooks, the audit
  log and the danger zone (archive, reset). `working/merging-and-ci.md` covers both GitHub
  settings in full.
  - **GitHub token** — for project owners only: the read credential AMP uses for this project's
    repository, and what lets a merge alert know which branch a pull request targets and which
    repository rules are holding it. It is **write-only** — pasting it seals it at rest, and the
    panel shows only the host, a label and when it was attached or replaced; no page, API answer,
    error or audit entry ever carries the value. **Save token** / **Replace token** attaches it or
    swaps it in place; **Remove token** deletes the credential *and every repository fact it
    produced*. No token is a real state: a public repository is read without one.

Each ticket opens a side panel with its description, acceptance criteria, comments, runs
(model, tokens, cost, git details) and history. When the ticket waits on you, the panel has the
same actions as My work (Answer, Mark merged, Start, Mark done); for approval it links to My
work.
