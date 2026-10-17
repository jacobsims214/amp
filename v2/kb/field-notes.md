---
title: Field notes — what we learned running teams on AMP
tags: [lessons, best-practices, wargame, field-notes]
---

# Field notes

Lessons from running AMP with real team patterns (two engineers and a lead, each steering their
own agents, with scope arriving through Slack). Each lesson is also in the page it belongs to.

1. **Hung agents are usually configuration, not models.** Headless subagents can't answer
   permission prompts. Keep agents inside the project; worktrees inside the repo.
2. **Run at most 4 workers, in the foreground.** Wide fan-outs hang sessions, and background
   work dies with its session.
3. **Never work a ticket that wasn't dispatched to you.** In a trial, a manager started a worker
   on a teammate's ticket after AMP refused the dispatch; it commented under the wrong person and
   edited files in the wrong checkout. Dispatch success is the permission to work.
4. **Quiet is the most useful signal a lead has.** It caught every dead worker. Recover from
   comments, runs and branches: nothing is lost if workers comment as they go.
5. **Off-platform decisions only exist in AMP if someone says them to their agent.** Do it in the
   moment. The agent turns them into tickets, handovers, metadata and `decisions/` docs.
6. **Metadata drifts.** One agent wrote `jira_key` where the others used `jira_epic`. Use keys
   already in use; the plan check now warns.
7. **Approval is the engineer's go, not a manager's sign-off.** The agent waits for its own
   human.
8. **Record usage from the start.** Work done before usage reporting has no spend and doesn't
   trace by commit. Reports say so, but you can't backfill it.
9. **Put the ticket ref in every commit.** It's what lets anyone trace code back to its story.
10. **Don't let agents start host services.** A worker's `docker compose up` added load at the
    worst time.
11. **Restore, don't re-create.** Deleted tickets keep their history in the trash.
12. **Plan for a sick day.** Handovers with a reason on each ticket meant the work carried on, and
    the returning engineer's agent picked up the full story.
