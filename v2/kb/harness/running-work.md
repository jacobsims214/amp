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
| `/amp` | the AMP connection: server, you, MCP health |
| `/ticket [id\|none]`, `/project [id]` | the current ticket or project; switch to another (`none` clears the ticket) |
| `/questions` | tickets waiting on an answer; answer them right there |
| `/plan`, `/open [id\|plan]` | plan approval, epics and stories; open a ticket or the plan in the browser |
| `/workers`, `/worker [id]` | workers here and elsewhere (Ctrl+X stops one, Ctrl+G opens its ticket); watch one live |
| `/remote [off\|pair\|devices]` | control this chat from the AMP web app or a phone (`harness/remote-control.md`) |
| `/model`, `/connect`, `/mcp`, `/agents`, `/skills`, `/config`, `/reload`, `/server` | settings (see the other harness pages) |
| `/usage`, `/abort`, `/stop`, `/close`, `/help`, `/quit` | tokens and cost, stop the reply (`/stop` also stops waiting for plan approval), close the open panel, every command, leave |

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
Esc stops the reply, and with it any worker this chat started (the transcript names them). Esc that
closes a panel, the `/` menu or a worker's session doesn't: for 2 s the spinner line says `Esc again to
stop the reply` (naming the live workers, e.g. `… and the worker on #222`), and only the next Esc stops it.
The model's thinking shows as one `▸ Thought for 12s` line: click it, or
press Ctrl+T, to open it (Ctrl+T opens the lowest one on screen), and `/config thinking` switches
between collapsed, full and hidden (see [configuration](configuration.md)). Ctrl+O shows tool
calls in full. Ctrl+W (with an empty message box) picks a running worker.

**Selecting text.** The chat reports the mouse to itself (wheel scrolling, clicking a worker), so
a plain drag doesn't select. Hold Fn (Terminal.app) or Option (iTerm2) while dragging, or set
`[tui] mouse = false` (plain drag selects again; clicking a worker then doesn't work, Ctrl+W
still does). The `/remote` pairing link is copied for you, and `/remote copy` and `/remote open`
copy or open it again (see [remote control](remote-control.md)).

## What to ask the manager

- **"Set up a new project here"** in a folder with no `.amp.json` (an empty folder or a repo with
  no commits is fine): it creates the AMP project, writes `.amp.json` in the folder (the one file
  the manager may write) and a project overview in the project's KB, then offers to plan. Commit
  `.amp.json` so everyone in the repo uses the same project.
- **"Plan this" / "plan out X"**: it checks the board and KB once, then creates the epics,
  stories and tickets with their dependencies (research tickets first when the stack needs study),
  runs the plan check and gives you the link to approve your tickets. It doesn't start anything
  until they're approved.
- **"What's on the board?"**: a read-only summary, with links for what waits on you.
- **"Run the ready tickets"**: it runs a worker on each approved, ready ticket.

The manager's shell is read-only: `git status`/`diff`/`log`/`show`, `ls`, `cat`, `head`, `tail`,
`grep`, `wc`, `sort` and pipes between them. Anything that writes or builds is a worker's job.

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

While `/workers` is open, everything you type goes to the message box, so the panel's own actions
are chords: ↑↓ select a row (with an empty message box), Enter or a click watches it, Ctrl+X stops
its worker (asks first: y or Enter stops, any other key keeps it running), Ctrl+G opens its ticket
in AMP, and Esc closes the panel.

The session shows the worker's transcript live (tool calls, collapsed thinking, messages) under a
header:

```text
worker #2142 Fix installer setup hand-off                                     running 3m12s
amp:amp-worker-backend · openrouter/deepseek/deepseek-v4.1 · AMP Harness › M4 · branch amp-2142
in 12.3k · out 1.2k · cached 34.5k · $0.04                       ctx ███░░░░░░░ 31% of 128.0k
```

Keys: Esc back to the chat (where you left it, scroll included; a second Esc there stops the reply, see above), Tab/→ and Shift+Tab/← the next
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
2. gives a role that edits files a place of its own (read-only roles work in your folder): in a
   git repository with a commit, a git worktree `<repo>/.worktrees/<ticket>` on branch
   `amp-<ticket>`; anywhere else (no git repository, a new folder another repository doesn't
   track, a repository with no commit yet), a folder `amp-work/<ticket>/`, and the worker is told
   not to commit or open pull requests. `[workers] isolation` changes this; see
   `harness/git-and-the-harness.md`;
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
isolation = "auto"        # auto | worktree | folder | shared (harness/git-and-the-harness.md)
```

The environment variables `AMP_HARNESS_MAX_WORKERS`, `AMP_HARNESS_WORKER_TIMEOUT_SECS` (in
seconds) and `AMP_HARNESS_WORKER_MAX_TURNS` and `AMP_HARNESS_WORKER_ISOLATION` win over the file. A model that keeps repeating the
same tool call is stopped after the fifth time, in the chat and in workers.

## Approvals

**Only a person approves a plan, in AMP's web UI.** No agent can, not even one signed in as you
(`working/planning-and-approval.md`). The harness makes the wait visible:

- The status line says `plan: N waiting for your approval`, linked to your My work tab in the
  project (Flight plan on an AMP older than My work). `/plan` shows the same, and `/open plan` opens the page.
- When AMP refuses the manager a step that is yours (dispatching a ticket planned after your
  last approval, marking your ticket done), the transcript shows:

  ```text
    ⏸ Waiting for your approval
      … A person approves it in the AMP web UI …
      Approve it in AMP: https://amp.example.com/project/2/plan   (/open plan)
      The manager stopped here; nothing else was run. Reply once it's done.
  ```

  The manager's turn ends there and it doesn't try another way. Approve in the web UI; the
  manager carries on by itself (below).
- **Per-ticket approval.** On servers where each ticket is approved by its owner, the status
  line counts `N of yours` apart from `others' waiting for their owners`, and a refusal names
  whose approval it waits for (`⏸ Waiting for its owner's approval: #2204 is someone else's`).

The harness also always denies agents `amp_approve_plan`, and changes to a project's
`require_plan_approval` or `open_reassign` setting.

### The manager carries on once you approve

You don't have to come back to the chat after approving. When a manager turn ends asking for
approval (it planned or checked the plan and your tickets await approval, or AMP refused it a
dispatch for want of one), the chat waits for your click in AMP:

```text
  ⏸ Waiting for plan approval in AMP (#2201, #2202). The manager carries on by itself once you
  approve; no need to come back here (Esc or /stop stops waiting). Approve it: https://amp.example.com/project/2/plan
```

and the status line says `⏸ waiting for plan approval in AMP · <link>`. Once a person approves in
the web UI, the chat says so and starts the manager's next turn itself:

```text
  ✓ Plan approved in AMP by you@example.com at 14:02 (#2201, #2202). The manager carries on.
  [amp-harness] The plan was approved in AMP by you@example.com: #2201, #2202 are approved now.
  Carry on with the plan: dispatch the approved tickets that are ready.
```

- **It only reacts.** The harness never approves anything: it reads the plan (`amp_check_plan`)
  on its own AMP connection, never through the model, and resumes when a person's approval
  shows there. Reads come every 5 seconds while tickets wait, every 15 seconds once the same
  tickets have waited 2 minutes, and every 60 seconds after 15 minutes, so a late approval can
  take up to a minute to be noticed. The fast reads stop as soon as nothing waits.
- **Partly approved.** If you approve some of the tickets, the manager carries on with those
  and is told which still wait, so it doesn't start them.
- **Changed instead.** If the tickets stop waiting without an approval (deleted, or done some
  other way), or plan approval is turned off, or the chat switches project or server, the chat
  says so and stops waiting; nothing starts until you reply.
- **Stop waiting:** Esc, or `/stop` (which also stops a running reply). Typing a new message
  ends the wait too: that turn decides for itself.
- **Turn it off** in config.toml (or with `AMP_HARNESS_AUTO_RESUME=false`); the chat then waits
  for your reply, as before:

  ```toml
  [manager]
  auto_resume_on_approval = false
  ```
- **Remote control** (following the chat from a browser) is coming with the remote-m1 branch.
  The waiting and approved lines are ordinary chat notices, so it will show them, and the resumed
  turn, as they happen.
