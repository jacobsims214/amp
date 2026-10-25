---
title: AMP Harness — chat, workers, approvals and limits
tags: [harness, amp-harness, chat, run, workers, dispatch, approval, worker-limits, worktrees, worker-session, mouse]
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
| `/workers`, `/worker [id]` | workers here and elsewhere; watch one live (`x` stops it, `o` opens its ticket) |
| `/model`, `/connect`, `/mcp`, `/agents`, `/skills`, `/config`, `/reload`, `/server` | settings (see the other harness pages) |
| `/usage`, `/abort`, `/help`, `/quit` | tokens and cost, stop the reply, every command, leave |

Enter sends and Shift+Enter starts a new line. Ctrl+J also starts a new line in every terminal,
as does Alt+Enter where Option/Alt sends Esc+ (iTerm2's Option key set to "Esc+", Terminal.app's
"Use Option as Meta key", most Linux terminals), and so does Enter right after a `\` at the end
of what you typed (the backslash becomes the line break).

Shift+Enter needs a terminal that tells it apart from Enter: one with the kitty keyboard protocol
(kitty, Ghostty, WezTerm, foot, and iTerm2 when it answers the protocol's query), xterm with
modifyOtherKeys, or Terminal.app on your own Mac. Others, including tmux without extended keys and
Terminal.app over ssh, send Shift+Enter as a plain Enter, so it **sends** the message. In those terminals the empty editor says
`/ for commands · Ctrl+J new line` and `/help` says so too: use Ctrl+J (or `\` then Enter) there.

Up in an empty (or one-line) editor recalls the prompts you sent earlier in this chat, newest first; Down goes forward again, and Down past the
newest gives back what you had typed. In a multi-line draft Up and Down move the cursor, and in
the `/` menu and the panels they move the selection. Workers' prompts are not in this history.
Esc stops the reply. The model's thinking shows as one `▸ Thought for 12s` line: click it, or
press Ctrl+T, to open it (Ctrl+T opens the lowest one on screen), and `/config thinking` switches
between collapsed, full and hidden (see [configuration](configuration.md)). Ctrl+O shows tool
calls in full. Ctrl+W (with an empty message box) picks a running worker.

**Selecting text.** The chat reports the mouse to itself (wheel scrolling, clicking a worker), so
a plain drag doesn't select. Hold Fn (Terminal.app) or Option (iTerm2) while dragging, or set
`[tui] mouse = false` (plain drag selects again; clicking a worker then doesn't work, Ctrl+W
still does).

## Watching a worker

While workers run, each one has a line above the message box, with a hint under them:

```text
▸ #2142 Fix installer setup hand-off · AMP Harness › M4 · worker-backend (deepseek-v4.1) · running 3m12s · 48.0k tok · ctx 31%
▸ #2143 Thinking toggle · worker-frontend (deepseek-v4.1) · running 1m02s · 12.4k tok · ctx 9%
2 running · click one or Ctrl+W to watch
```

Every line shows what the worker is for: the ticket's id and name, story › epic, the agent and
its model, the state and how long it has run, tokens so far and how full the model's context is.
The numbers update every second. At 80 columns the name is shortened with `…` first; the id,
agent and state always stay. The ticket's name, story and epic come from AMP (the metadata
poller reads each worker's ticket every 30 s, never through the model); without AMP a line shows
the bare id. A ticket in progress elsewhere (another chat or person) shows in `/workers` with AMP's
record of its last run and when that was recorded.

The same details are on the transcript's lines: `▶ worker started #2142 …`, the dispatch line
`run_ticket #2142` (its second row), and `■ worker finished #2142 … completed after 3m10s ·
48.0k tok`.

**Open a worker's session** any of these ways:

- click it: a line above the message box, a `/workers` row, a worker notice or dispatch line, or
  any `#2142` in the chat that has a worker session;
- Ctrl+W, then ←/→ to choose and Enter;
- `/workers`, ↑↓ and Enter; or `/worker 2142`.

The session shows the worker's transcript live (tool calls, collapsed thinking, messages) under a
header:

```text
worker #2142 Fix installer setup hand-off                                     running 3m12s
amp:amp-worker-backend · openrouter/deepseek/deepseek-v4.1 · AMP Harness › M4 · branch amp-2142
in 12.3k · out 1.2k · cached 34.5k · $0.04                       ctx ███░░░░░░░ 31% of 128.0k
```

Keys: Esc back to the chat (where you left it, scroll included), Tab/→ and Shift+Tab/← the next
or previous worker, `x` stop (asks first), `o` open the ticket in AMP, PgUp/PgDn scroll. The chat
keeps running underneath. A finished worker's session stays viewable (read-only) until the chat
ends; an earlier chat's worker opens from its session file with `/worker <id>`.

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
   `amp-<ticket>` (read-only roles work in your folder). When your folder is in no git
   repository, is a new folder that another repository doesn't track, or is a repository with no
   commit yet, the worker works in your folder directly and its report says why; `git init` and
   one commit give workers their own worktrees;
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
