---
title: AMP Harness — configuration layers, files and reload
tags: [harness, amp-harness, configuration, config-toml, precedence, layers, reload, settings]
---

# Configuration: layers, files and reload

You rarely need to open a file: every setting has a command. This page is for knowing where a
setting comes from, sharing settings with a repo, and running with another configuration.

## Three layers

| layer | where | holds |
|---|---|---|
| built in | inside the binary | the shipped providers, models, roles, agents and skills |
| user (yours) | `~/.config/amp-harness/`: `config.toml`, `agents/`, `skills/`, `AGENTS.md`, `credentials.json` | everything |
| repo (shared) | `<repo>/.amp/harness/`: `config.toml`, `agents/`, `skills/`, next to `.amp.json` | only `[mcp.*]`, `[roles.*]` and `[models] profile`, plus agents and skills |

Later layers win. The repo layer is deliberately limited. A repo `config.toml` that sets anything
else (`[server]`, `[providers]`, `[secrets]`, `[workers]`, `[tools]`, `[tui]`…) is an error naming
the key and the file. A cloned repo can't redirect your AMP server or your keys, or change what
your agents' shell sees. What it does bring is used only once you trust it, as it is now: its
MCP servers (`harness/mcp-servers.md`), its agents and skills (`harness/agents-and-skills.md`),
and its `[roles.*]` tables and `[models] profile`. A repo role table can pick another model or
add `extra_body` fields to every request of that role, so until you trust it your own setting is
used. `amp-harness trust` lists and trusts them; the chat asks at start; `--trust-repo` (or
`AMP_HARNESS_TRUST_REPO=1`) uses them all for one process (CI).

For a single setting the order is **flag, then environment variable, then config file, then
default**. For example, the AMP server is `--url`, then `AMP_MCP_URL`, then `[server] url`, then
hosted AMP.

## See what is in effect

```sh
amp-harness config show          # providers and each role's model, with the layer that set it
amp-harness config check         # every problem in every layer, with file, key and fix (exit 1 on errors)
amp-harness config check --live  # also each role's model against the provider's live list
amp-harness server               # the AMP server settings and where each comes from
```

**In the chat:** `/config` shows every setting with its source (built in, user file, repo file,
env VAR, flag) and the paths in use. Files in the repo are shown as `<repo>/…`, and its `repo` row
says which folder that is. Secrets show only where they come from.

## Use another configuration: `--config` and `--config-dir`

```sh
amp-harness --config ./ci-config.toml run -p "What is on the board?"
amp-harness --config-dir ~/amp-work doctor
```

- `--config FILE` (or `AMP_HARNESS_CONFIG`) uses another `config.toml` for every setting and
  every command, readers and writers alike. Agents, skills and logins stay where they are.
- `--config-dir DIR` (or `AMP_HARNESS_CONFIG_DIR`) moves the whole user layer: `config.toml`,
  `agents/`, `skills/`, `AGENTS.md` and `credentials.json`.
- Both are global flags: they go before or after the command.
- `$XDG_CONFIG_HOME` is honoured too. Sessions, logs and the trust list (MCP servers, agents, skills, settings) live in
  `~/.local/state/amp-harness` (`$XDG_STATE_HOME`).

## Reload without a restart

Changes made from another terminal (`amp-harness agents|skills|models|mcp …`, or an edit to a
config file, `agents/` or `skills/` in either layer) reach an open chat by themselves a
couple of seconds after the files settle. `/reload` applies them at once, and
`AMP_HARNESS_AUTO_RELOAD=0` turns the automatic reload off. The transcript says what changed.

| change | takes effect |
|---|---|
| agents (new, edited, removed) | the manager may dispatch them from its next reply; new workers use the new file |
| skills | from the manager's next reply, and in every new worker |
| a role's model, providers, keys | workers dispatched from then on; the chat itself if its own role changed |
| the manager's permissions | from its next tool call |
| MCP servers | only the servers whose table changed restart; new ones start, removed ones stop |
| `AGENTS.md` | re-read for the manager's next reply |

Workers already running keep what they started with. `[workers]`, `[tools]` and `[secrets]` are
read when the chat starts: restart the chat for those.

## Other settings in config.toml

```toml
[workers]
max_parallel = 4          # workers at once (AMP_HARNESS_MAX_WORKERS)
timeout_minutes = 60      # per worker (AMP_HARNESS_WORKER_TIMEOUT_SECS, in seconds)
max_turns = 400           # model turns per worker, 0 = no limit (AMP_HARNESS_WORKER_MAX_TURNS)

[tools]
bash_timeout_secs = 600   # default bash timeout (AMP_HARNESS_BASH_TIMEOUT_SECS; 0 = none)
mcp_max_output_tokens = 25000

[secrets]
pass_to_bash = []         # provider keys and AMP_TOKEN are hidden from agents' bash unless listed here

[tui]
theme = "auto"            # auto | dark | light | a theme .json file (--theme, AMP_HARNESS_THEME)
show_thinking = "collapsed"   # collapsed | full | hidden
mouse = true              # false gives plain click-drag text selection back
padding = 2
density = "compact"       # compact | comfortable (AMP_HARNESS_DENSITY)
```

`density` decides how tightly the chat is laid out (`AMP_HARNESS_DENSITY` beats it):

- `compact` (the default): your message is one shaded row with one blank row before the reply; a
  collapsed `▸ Thought for 12s` line, tool calls and the reply's text follow each other with no
  blank rows; a failed tool call is one line too (Ctrl+O shows the whole error); markdown
  headings are bold text, not underlined, with no blank row under them; notices have one blank
  row above them and none below; the start banner is three lines; `/help` has no blank rows
  between its groups; the footer is one row at any width. Bold is kept for headings.
- `comfortable`: the roomier layout from before: padded message bubbles, blank rows around
  thinking, tool calls and notices, underlined top-level headings, a two-row footer from 100
  columns.

Compact fits about a third more of the same conversation on screen (at 120x40, 24 lines of it
instead of 18; at 80x24, 13 instead of 10). The harness can't change the font size: to make the
text itself smaller, set it in your terminal (Cmd+- in Terminal.app and iTerm2, Cmd+0 to reset).

`show_thinking` decides how the model's thinking shows in the chat (`AMP_HARNESS_SHOW_THINKING`
beats it):

- `collapsed` (the default): one dim line per thinking section. While the model thinks it reads
  `▸ Thinking · 12s` with its latest sentence; once the answer starts, `▸ Thought for 12s`. The
  newest one adds `· Ctrl+T or click to open`.
- `full`: every section open, its text dimmed behind a `│` gutter under a `▾ Thought for 12s`
  line.
- `hidden`: no thinking lines at all (Ctrl+T still opens the last one).

Click a thinking line to open or close it. Ctrl+T opens or closes the lowest one on screen, so
scroll up to an older one (PgUp) and press Ctrl+T to read it. `/config thinking` switches this
chat to the next mode (collapsed, full, hidden), and `/config thinking full` (or `collapsed`,
`hidden`) picks one; `t` in the `/config` panel does the same. That lasts for this chat; set
`show_thinking` here to keep it. Session files and `run -p` always keep the full thinking.

Environment variables beat the file. Unknown keys are errors, so a typo never goes unnoticed.

`AGENTS.md` in your config folder, and `AGENTS.md` or `CLAUDE.md` in the working directory and
its parents, add standing instructions to every agent's prompt.

## Every write is safe

Commands that write a config file change only their own table, keep your comments and the rest
of the file, save the old file as `config.toml.bak`, and refuse to save a result that doesn't
load.
