---
title: Traceability — how did this code come to be?
tags: [trace, audit, history, commit, pull-request, investigation]
---

# Traceability

Any change an AMP worker made can be traced back to why it exists.

## From a commit, branch or PR

- **UI**: **Trace** in the sidebar. Paste a commit sha (7+ characters), branch name or PR URL.
- **Agent**: `amp_trace(commit | branch | pr)`.

You get every ticket whose runs produced it, in projects you can see, each with its epic and
story, comments, runs (agent, model, tokens, cost, branch, commit, PR, merged) and state changes.

## Following the thread

From the ticket, the story continues:
- `amp_get_ticket_history`: comments, runs and every state change in order: who planned it, who
  dispatched it, what each worker tried, questions and their answers, handovers and their reasons.
- The epic's metadata and the project KB's `decisions/` docs: why the work was asked for.
- The audit log: access and settings changes around it.

## Asking an agent to investigate

"How did we get to commit 3e48284, and what did it cost?" is a fair question for any agent with
AMP access. A good answer names who asked for the work and where (a Slack decision recorded in
`decisions/`), the plan, what each worker did and chose, the models and tokens, whether it's
merged, and what it couldn't see (runs before usage reporting, work done outside AMP). The same
history is how you find what to optimize: tickets with many abandoned runs, expensive models on
simple work, review gates that keep generating fixes.

## What makes it work

Workers record branch, commit and PR in their runs and put the ticket ref in commit messages.
Work done outside AMP, or before run recording, won't trace by commit; the ticket history still
holds what was said.
