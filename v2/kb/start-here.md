---
title: Start here — onboarding a new AMP user
tags: [onboarding, start]
---

# Start here

This page is the onboarding script. If you are an agent reading it through `amp_guide`, your job
is to get your user productive in AMP. Don't read this page to them. Find out where they are,
then do each step *with* them in their own projects. They should end the session having planned,
approved, run and checked real work.

## What AMP is, in one breath

AMP is where a team's agentic work is planned, run and accounted for. A person asks their own
agent (the *manager*) for something. The manager plans it in AMP as a dependency graph of small
tickets, the person approves the plan, and the manager runs each ticket with a worker subagent.
Everyone on the team sees the same board: what's moving, what's stuck, who is waiting on whom,
what it cost, and how any line of code came to be.

The rule underneath everything: **people own the work, and agents act for their person.** Your
tickets are yours; your agent runs only yours. Teammates' agents run theirs.

## Step 0: find out who you're teaching

Call `amp_whoami`, then `amp_list_projects`. Ask (briefly) what they want out of AMP today:
- **"I'm an engineer starting on a project"** → steps 1–6.
- **"I lead a team"** → steps 1, 2, then `visibility/reports-and-spend.md`,
  `working/start-and-end-of-day.md` and `admin/orgs-teams-and-access.md`.
- **"Finance / PM wants numbers"** → `visibility/reports-and-spend.md`.
- **"Just tell me what something does"** → look it up with `amp_guide(topic=…)`.

## Step 1: the map (5 minutes)

Explain the shape using *their* data, not an abstract example. Open one of their projects with
`amp_get_project` and `amp_list_tasks(project_id, format="compact")` and say what you see:
- **Project → epics → stories → tickets.** A ticket is one small unit of work for one agent.
- **Dependencies.** AMP runs every ticket whose dependencies are done, in parallel. The graph is
  the plan. (`concepts.md`)
- **The buckets** in `amp_list_tasks`: `ready_to_dispatch`, `in_progress`, `questions`,
  `scheduled`, `quiet`, `blocked`, `completed`.

Point them at the UI as you go: **My work** (home), **Projects**, and inside a project
**Mission**, **Board**, **Flight plan**, **Graph**, **Reports**, **Library**, **Settings**
(`ui-tour.md`).

## Step 2: make sure their agent is set up right

Most early trouble is harness setup, not AMP. Check `setup/connect-your-agent.md` with them:
the AMP MCP server is connected with *their* token, the manager agent and skills are installed,
and workers can't get stuck on permission prompts.

## Step 3: plan something real, together

Ask for a small real goal ("add a health endpoint", "write the README"). Plan it with them
(`working/planning-and-approval.md`):
1. Create an epic and stories, then tickets with `amp_create_tasks` (refs + `depends_on`).
2. Run `amp_check_plan` and explain what it says.
3. Show them the plan in **Flight plan** and **Graph**.
4. Have *them* approve it. Approval is their "go"; you never approve your own plan.

## Step 4: run it, and show them where to watch

Dispatch and run tickets as in `working/running-work.md`: foreground, at most 4 at a time.
While a worker runs, have them open **Mission** and the ticket's history. When it finishes, show
the run record: model, tokens, cost, branch, commit.

## Step 5: a question, and an answer

If a worker blocks with a question, walk them through answering it, either to you or in the UI
(`working/questions.md`). If none comes up, show where questions appear.

## Step 6: wrap up the day

Draft a day report with them (`working/start-and-end-of-day.md`). Tell them what tomorrow's
session will do first: recover quiet tickets, read answers, read the team's reports.

## After onboarding

Offer the pages they'll want next: handovers and scope changes (`working/changes-outside-amp.md`),
merging (`working/merging-and-ci.md`), tracing a commit (`visibility/traceability.md`), the
knowledge bases (`knowledge/kb-scopes.md`). When they ask "can AMP do X?", look it up with
`amp_guide` before answering.

## How to teach

- Show, don't recite. Every concept should land on something in their project.
- One step at a time; check they've seen it before moving on.
- Use their words for their work, and AMP's words for AMP (ticket, epic, dispatch, run).
- Never invent a feature. If the guide doesn't cover it, say so.
