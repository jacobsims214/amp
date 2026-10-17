---
title: Changes agreed outside AMP — scope, handovers, tracker and finance facts
tags: [handover, hand-over, reassign, scope, metadata, slack, decisions, sick, tickets]
---

# Changes agreed outside AMP

Real decisions happen in Slack, meetings and hallways: a customer signs and scope grows, someone
is out sick, Finance says the epic is capitalizable. AMP only knows what someone tells it. The
habit that makes it work: **when you hear it, tell your agent, and your agent puts it in AMP in
the same turn.** Say it in your own words; the manager knows where each kind of fact goes.

| What was decided | Where it goes in AMP |
|---|---|
| New scope | New epic, stories and tickets; the plan needs approving again if the project requires it |
| Work moves to someone else | `amp_handover_tasks` (all or some of a person's open tickets, with a reason posted on each), or `amp_assign_task` for one ticket |
| A tracker key, cost centre, capitalizable, budget | Metadata on the project (`amp_update_project_settings`, project owners) or epic (`amp_update_epic`) |
| Why any of the above | A doc under `decisions/` in the project KB |

The agent should tell you what it changed.

## Handovers

- In the UI: filter the **Board** to the person (and epic, if only some of their work moves) and
  use **Hand over …**. Only the tickets on screen move, and each gets a comment with your reason.
- Anyone can hand over their own tickets. By default (the project's `open_reassign` setting) any
  project member can also reassign a single ticket that isn't in progress. Handing over someone else's takes a project lead (or an
  org owner or admin).
- In-progress tickets stay put unless you include them: their worker is still running under the
  old owner.
- When the person comes back, their manager finds the handover and its reason in the tickets'
  comments and history, and can recommend whether to take the work back.

## Metadata, consistently

Metadata is free-form, so consistency is on you. Use the keys your org already uses: the
metadata editor suggests keys already in use, and the plan check warns (`metadata_drift`) when an
epic is missing a key its siblings carry. AMP doesn't assume Jira or any other tracker: use
`jira_epic`, `linear_team`, `ado_area_path`, `servicenow_ref` or whatever your org runs on.
