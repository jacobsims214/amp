---
title: AMP Harness — chat, workers, approvals and limits
tags: [harness, amp-harness, chat, run, workers, dispatch, approval, worker-limits, worktrees]
---

# Chat, workers, approvals and limits

## The chat

Run `amp-harness` in your project folder. You talk to the manager. It plans on the AMP board
and, once you approve the plan, runs a worker agent on each ticket. The header shows the server,
you, the project and the current ticket. The status line shows questions waiting for an answer,
workers, plan approval and the board counts.

| command | what it does |
|---|---|
| `/ticket [id]`, `/project [id]` | the current ticket or project; switch to another |
| `/questions` | tickets waiting on an answer; answer them right there |
| `/plan`, `/open [id\|plan]` | plan approval, epics and stories; open a ticket or the plan in the browser |
| `/workers`, `/worker [id]` | workers here and elsewhere; watch one live (`x` stops it) |
| `/model`, `/connect`, `/mcp`, `/agents`, `/skills`, `/config`, `/reload`, `/server` | settings (see the other harness pages) |
| `/usage`, `/abort`, `/help`, `/quit` | tokens and cost, stop the reply, every command, leave |

Enter sends and Shift+Enter starts a new line. Esc stops the reply. Ctrl+T expands the thinking
and Ctrl+O shows tool calls in full. To select text with the mouse, hold Fn (Terminal.app) or
Option (iTerm2) while dragging, or set `[tui] mouse = false`.

## Headless: `run -p`

```sh
amp-harness run -p "What is on the board?"
amp-harness run -p - --json < prompt.txt     # one JSON event per line, for scripts
amp-harness run --role researcher -p "…"     # another role than the manager
```

It builds the same session as the chat and exits 0 when the run completes.

## How workers run

When the manager dispatches a ticket (its `amp_run_ticket` tool), the harness:

1. waits for a free worker slot, then marks the ticket dispatched in AMP;
2. for roles that edit files, creates a git worktree `<repo>/.worktrees/<ticket>` on branch
   `amp-<ticket>` (read-only roles work in your folder);
3. runs the worker on its role's model until it finishes, times out or uses up its turns;
4. **reports the run to the ticket by itself**: model, tokens, cost, branch, commit. A worker
   that stops without closing its ticket is recorded as abandoned, with the reason.

Workers never wait on a prompt: their shell runs with no terminal input, and every command has a
time limit.

## Worker limits

```toml
# ~/.config/amp-harness/config.toml
[workers]
max_parallel = 4          # workers at once; extra dispatches wait for a slot
timeout_minutes = 60      # wall clock per worker
max_turns = 400           # model turns per worker; 0 = no limit
```

The environment variables `AMP_HARNESS_MAX_WORKERS`, `AMP_HARNESS_WORKER_TIMEOUT_SECS` (in
seconds) and `AMP_HARNESS_WORKER_MAX_TURNS` win over the file. A model that keeps repeating the
same tool call is stopped after the fifth time, in the chat and in workers.

## Approvals

**Only a person approves a plan, in AMP's web UI.** No agent can, not even one signed in as you
(`working/planning-and-approval.md`). The harness makes the wait visible:

- The status line says `plan: N waiting for your approval`, linked to the project's Flight plan
  page. `/plan` shows the same, and `/open plan` opens the page.
- When AMP refuses the manager a step that is yours (dispatching a ticket planned after your
  last approval, marking your ticket done), the transcript shows:

  ```text
    ⏸ Waiting for your approval
      … A person approves it in the AMP web UI …
      Approve it in AMP: https://amp.example.com/project/2/plan   (/open plan)
      The manager stopped here; nothing else was run. Reply once it's done.
  ```

  The manager's turn ends there and it doesn't try another way. Approve in the web UI, then
  reply.
- **Per-ticket approval.** On servers where each ticket is approved by its owner, the status
  line counts `N of yours` apart from `others' waiting for their owners`, and a refusal names
  whose approval it waits for (`⏸ Waiting for its owner's approval: #2204 is someone else's`).

The harness also always denies agents `amp_approve_plan`, and changes to a project's
`require_plan_approval` or `open_reassign` setting.
