---
title: AMP Harness — MCP servers
tags: [harness, amp-harness, mcp, mcp-servers, stdio, http, sse, oauth, import, trust, permissions]
---

# MCP servers

Besides AMP itself, the harness connects any MCP server: a local program (**stdio**), a remote
**Streamable HTTP** server, or an older **HTTP+SSE** one. Its tools reach the agents as
`mcp__<server>__<tool>`. You add, change and remove servers with commands, the same way as in
Claude Code.

## Add one

```sh
amp-harness mcp add fs -- npx -y @modelcontextprotocol/server-filesystem '${repo}'   # stdio
amp-harness mcp add linear https://mcp.linear.app/mcp                                 # http (OAuth if it asks)
amp-harness mcp add github https://api.githubcopilot.com/mcp/ --bearer-env GITHUB_TOKEN
amp-harness mcp add docs --url https://docs.example.com/sse --transport sse -H 'X-Org: acme'
amp-harness mcp add search --secret-env BRAVE_API_KEY -- npx -y brave-search-mcp     # secret asked hidden
amp-harness mcp add-json linear '{"type":"http","url":"https://mcp.linear.app/mcp"}' # Claude Code / Cursor / VS Code JSON; - reads stdin
```

- **Transport:** a URL means `http` (it falls back to SSE by itself unless `--no-sse-fallback`).
  A command after `--` means `stdio`. `--transport sse` forces SSE.
- **Scope:** your config by default. `--scope repo` (or `--repo`) writes the repo's
  `.amp/harness/config.toml`, shared with everyone who clones it, and trusts it for you.
- After adding, it connects once and lists the tools (`--no-test` skips this).
- **More options:** `--env KEY=VALUE`, `--env-file FILE`, `--workdir DIR` and `--per-worktree`
  (one copy of the server per worker worktree) for stdio servers. `--roles` (below),
  `--startup background|eager|lazy`, `--startup-timeout SECS`, `--timeout SECS` (per tool call),
  `--disabled`, `--force` (replace) and `--json`.
- **Variables** in arguments, env and headers are filled in when the server starts:
  `${env:VAR}`, `${env:VAR:-default}`, `${repo}`, `${worktree}`, `${home}`, `${config_dir}`. Quote
  them in single quotes so your shell leaves them alone.

## Secrets and auth

Secrets never go in a file or on the command line:

- `--secret-header NAME` and `--secret-env KEY` are asked hidden (or read one per line with
  `--secret-stdin`) and kept in the OS keychain. `mcp secret NAME` stores or replaces them later.
- `--bearer-env VAR` sends `Authorization: Bearer $VAR` from your environment. Nothing is
  stored, so it works in CI too.
- A plain `--header` or `--env` value that looks like a secret is refused, with these
  alternatives.
- **OAuth:** `--auth auto` (the default) logs in when the server asks for it.
  `amp-harness mcp login NAME` opens the browser and `mcp logout NAME` forgets the login. For a
  server that needs a pre-registered app: `--client-id`, `--client-secret` (asked hidden),
  `--oauth-scope`, `--callback-port`.
- Where there is no keychain (`AMP_CREDENTIAL_STORE=file`), use `--bearer-env` or `${env:VAR}`
  references.

The values are given only to the server (its environment or request headers). They never reach
the model or the agents' shell.

## Manage them

```sh
amp-harness mcp list                    # every server with its health (connects to each; --no-health skips)
amp-harness mcp get github              # its config, where each secret comes from (never the value), trust, login
amp-harness mcp test github             # connect now: status, timing, tools; the server's stderr when it fails
amp-harness mcp edit github             # $EDITOR on its table; checked before it is saved
amp-harness mcp edit db --roles 'worker-*,reviewer'
amp-harness mcp disable github          # or enable; also switches off a repo server, for you only
amp-harness mcp remove github           # also deletes its keychain secrets and login (--keep-secrets, --keep-login)
amp-harness mcp tools --server github   # its tools
amp-harness mcp call --server github search_issues '{"q":"bug"}'
```

**In the chat:** `/mcp` lists the servers with their health. Keys act on the selected one:
`r` restart, `t` test, `e` enable, `d` disable, `l` login, `T` trust, `U` untrust. `/mcp <name>`
shows its details. `/mcp add <name> <url> [--sse]` adds a remote server. Anything that needs a
secret, a stdio command, an edit or an import prints the `amp-harness mcp …` command to run in a
terminal. The status line shows `mcp 3/4` (ready of the servers that should run).

## Bring servers in from other tools: `mcp import`

```sh
amp-harness mcp import                         # lists what it finds here and asks
amp-harness mcp import --from claude --dry-run # the TOML it would write; secrets only by name
amp-harness mcp import --from claude --yes     # .mcp.json and ~/.claude.json
amp-harness mcp import --from vscode --only perplexity
amp-harness mcp import --from ./some/mcp.json --scope repo --rename GitHub=gh
```

`--from` takes `claude`, `opencode`, `vscode`, `cursor`, `claude-desktop`, or a file in any of
their formats. Their variables are translated (`${workspaceFolder}` becomes `${repo}`, and so
on). Secret-looking values are moved to the keychain, and VS Code inputs are asked for at import.
Importing again changes nothing. Other tools' files are never loaded on their own: only what you
import is used.

## Repo servers need your trust

A server in a repo's `.amp/harness/config.toml` can run commands on your machine or receive what
the agents send, so it starts only once you trust it:

- The chat asks for each one before it starts its MCP servers: `y`, `a` for all of them, Enter
  for not now.
- `amp-harness mcp trust` lists the repo's servers and whether each is trusted.
  `mcp trust <name>…|--all` trusts them and `mcp untrust <name>…|--all` forgets them.
- Trust covers the server **as configured now**. If someone changes its table (or its
  `env_file`), you are asked again. Trust is recorded outside the repo, in
  `~/.local/state/amp-harness/trust.toml`.
- Headless runs (`run -p` and every worker) never ask. They skip untrusted servers with a
  warning. In CI, `--trust-repo-mcp` (or `AMP_HARNESS_TRUST_REPO_MCP=1`) lets them all start for
  that one process.
- Servers in your own config are always trusted.

## Which agents may use a server

Two levels, applied in this order:

1. The server's `roles` (globs without `amp-`): `mcp edit db --roles 'worker-*,reviewer'`, or
   `roles = ["worker-*", "reviewer"]` in its table. Default: every role.
2. Each agent's `mcp` permission (`harness/agents-and-skills.md`), for example
   `"github/delete_*": deny`.

A tool an agent may not use isn't offered to it at all.

## Big results

A tool result over `max_output_tokens` (default 25,000; set it per server, or for all servers
with `[tools] mcp_max_output_tokens` or `AMP_HARNESS_MCP_MAX_OUTPUT_TOKENS`) is cut. The model
gets the start and the path of a file with the whole result, which it can read.

## The table, for reference

```toml
[mcp.github]
type = "http"                         # stdio | http | sse
url = "https://api.githubcopilot.com/mcp/"
bearer_env = "GITHUB_TOKEN"
roles = ["worker-*", "reviewer"]
call_timeout_secs = 120
max_output_tokens = 25000

[mcp.fs]
type = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "${repo}"]
secret_env = ["BRAVE_API_KEY"]        # the value is in the keychain
```

## When a server doesn't work

- `amp-harness mcp test NAME` connects and prints the error, and for a stdio server the end of
  its stderr. The full stderr log is `~/.local/state/amp-harness/mcp-logs/<name>.log`.
- `amp-harness doctor` has one `mcp:<name>` line per server. It shows health, trust, how the
  server authenticates and which roles may use it, with the fix.
- `needs login`: run `amp-harness mcp login NAME` (then `/reload` in an open chat).
- `not trusted`: run `amp-harness mcp trust NAME` (or press `T` in `/mcp`).
- A missing secret: run `amp-harness mcp secret NAME --missing`.
- A server you just added in another terminal: the chat picks it up by itself after a couple of
  seconds, or at once with `/reload`.
