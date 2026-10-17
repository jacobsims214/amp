---
title: Merging and CI — closing the loop from ticket to main
tags: [merge, ci, pull-request, unmerged, github]
---

# Merging and CI

A completed ticket isn't shipped until its work is merged. AMP tracks the gap.

- **Waiting to merge**: `amp_list_unmerged(project_id)` and the panel in **Mission** list
  completed tickets whose runs pushed work that hasn't been marked merged.
- **Marking merged**: the **Merged** button in Mission; or your agent records
  `amp_report_run(task_id, outcome="completed", merged=true, commit=<merge sha>)` when you merge or
  tell it to; or CI reports it automatically.
- **From CI**: after a merge, `POST /api/trace/merged` with the commit, branch or PR (and the merge
  commit) marks every ticket whose runs produced that work as merged. The AMP repo's
  `docs/ci-merge-reporting.md` has a GitHub Actions step.

Workers put the ticket ref in their commit messages and record branch, commit, PR and pushed in
their run, which is what lets merges, and later traces, find the ticket.
