---
title: Reports, agent spend and capitalization
tags: [reports, tokens, cost, spend, capitalization, finance, portfolio, metadata]
---

# Reports, agent spend and capitalization

## Where the numbers come from

Every run a worker records carries its model, input/output/cache tokens and cost. Reports add
those up by ticket, epic, person, model and metadata. Nobody fills in a timesheet.

Spend is only as complete as the runs that reported it. A harness that can't tell its agent its
usage leaves runs without numbers, and the reports say so ("N runs reported no usage"), so
treat totals as **a floor** until that count is zero. Abandoned runs are counted as rework, not
as missing usage.

## Project Reports

**Reports** in a project shows, for a date range:
- progress and velocity;
- a **Tokens** readout and tokens and cost **by model**;
- tokens and cost per **epic**, per **person**, and per **metadata value**, so a key like
  `capitalizable` splits spend into capex and opex;
- tokens per completed ticket by agent profile, and abandoned-run (rework) counts;
- **People** with done / in progress / questions / blocked by;
- **People in the loop**:
  - agents vs people: how long work waited to start and took once started, and the hours people
    reported;
  - human work by reason;
  - the human tickets that held the plan up longest. See `working/human-work.md`. Agents get the
    same numbers with `amp_human_impact`.
- the **standup sheet**;
- **Export** to Markdown.

## Portfolio (org-wide)

**Portfolio** lists every project in your org that you can see: progress, open questions and
spend, by model, with CSV export. It's tracker-agnostic:
- **Columns** are whatever metadata your projects carry (a Jira board, Linear team, Azure DevOps
  area path, cost centre…).
- **Split by** any ticket or epic metadata key your org uses (`capitalizable`, `cost_type`,
  `workstream`…).
Your column and split choices are remembered in your browser.

## Answering Finance

"What did agents cost on the quotes epic, and how much is capitalizable?"
1. Make sure the epics carry the keys Finance cares about (`working/changes-outside-amp.md`); the
   plan check warns about drift.
2. Portfolio, split by `capitalizable`, for the period: spend per project and per value.
3. Reports in the project for the per-epic and per-model detail.
4. Give the number with its caveat: how many runs reported no usage.

Agents can answer this too: `amp_get_task` with `include: ["runs","history"]`, and the project
report, and they should state the same caveat.
