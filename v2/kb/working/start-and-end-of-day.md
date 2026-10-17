---
title: Start of day, end of day, and standup
tags: [day-report, standup, start-of-day, session, routine]
---

# Start of day, end of day, and standup

## Starting a session

A new day or a restart after a crash. Before anything new, the manager:
1. `amp_list_tasks(format="compact")`: looks at `quiet` (in progress, no activity: the worker
   died with an earlier session), reads their comments, records abandoned runs, puts them back to
   backlog and re-dispatches them.
2. Checks `questions`: answers may have come in through the UI overnight.
3. Reads yesterday's reports for the whole project, `amp_project_day_reports(project_id, date,
   tz)`, for handoffs ("ticketed #204 for Alex") and blockers. Also checks `amp_list_unmerged`.
4. Tells you where things stand and what it will start.

## Ending the day

Ask your agent to "wrap up today". It drafts the report from your actual work (`amp_day_report`:
tickets completed, in progress, questions, by project), writes the summary with you, saves it
(`amp_save_day_report`) and gives you copy-ready Markdown for Slack or email. Use an IANA
timezone (e.g. `America/Chicago`) so "today" is your day. Saved reports are under **Day reports**.

## Standup

Everything for a standup is already in AMP:
- **Reports → standup sheet**: everyone's day report for a date, and who hasn't written one
  (`amp_project_day_reports`).
- **Reports → People**: each person's done, in progress, questions, tokens, and **blocked by**:
  whose unfinished tickets they're waiting on.
- **Mission**: what's quiet, what's waiting to merge, open questions.

A lead can run standup from these without asking anyone for status, and spend the meeting on
blockers and decisions.
