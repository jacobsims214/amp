# amp-harness

AMP's own agent harness: a Rust binary built on pi-rust's core crates (`rpi-*`) with an MCP client on
`rmcp`. Design and build plan: AMP project 2 KB, `decisions/amp-harness-architecture.md`.

Status: **M2.** `amp-mcp` connects to AMP (both MCP protocol eras, static token or OAuth login).
`amp-harness` with no subcommand is a chat with the manager, which plans on the board and dispatches
specialist workers itself (`amp_run_ticket`); every worker's run, tokens and commit are reported to
its ticket automatically. The CLI also has `login`, `logout`, `doctor`, `whoami`, `tickets list|get`,
`config show`, `run -p` (a headless agent run) and the hidden `mcp call`. `make smoke` runs them
against a real local AMP.

## Try it

From the repo root of a worktree on `/Volumes/external-data` (see the Rust disk rule below):

```sh
make -C v2/harness build
alias amp-harness="$PWD/v2/harness/target/debug/amp-harness"

# 1. A provider key, from the environment or the keychain (never a config file).
export OPENROUTER_API_KEY=...       # the shipped roles run on OpenRouter models
# or: export ANTHROPIC_API_KEY=... and AMP_HARNESS_PROFILE=anthropic (Anthropic-only profile)
# or keep it in the macOS keychain (prompts for the key):
#   security add-generic-password -s amp-harness -a provider:openrouter -w

# 2. Every role resolves to a provider/model that has a key, and the model ids exist.
amp-harness config show --check

# 3a. Against compose (auth off; on the dev Mac amp-api is published on :18000, see Smoke test).
export AMP_MCP_URL=http://localhost:18000/mcp
amp-harness doctor
amp-harness                          # chat with the manager; /help lists the commands

# 3b. Or against the docker-desktop cluster (OAuth, self-signed CA).
kubectl --context docker-desktop -n amp get secret amp-ca-secret \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > amp-local-ca.crt
amp-harness login  --url https://amp.local/mcp --ca-file amp-local-ca.crt
amp-harness doctor --url https://amp.local/mcp --ca-file amp-local-ca.crt
amp-harness        --url https://amp.local/mcp --ca-file amp-local-ca.crt
```

Run it from a directory with an `.amp.json` (its `project_id` is the default project), or tell the
manager which project to use. `amp-harness run -p "What is on the board?"` does the same without
the TUI. To drop the flags, put `url` and `ca_file` under `[server]` in
`~/.config/amp-harness/config.toml`.

## Layout (D1)

```
Cargo.toml            workspace; exact pins: rpi-* =0.8.0, rmcp =3.6.0
rust-toolchain.toml   1.98.1 (+ clippy, rustfmt)
.cargo/config.toml    target-dir = "target" (stays inside v2/harness)
Makefile              build / test / lint / smoke; exports CARGO_HOME for the dev Mac
crates/
  amp-mcp/            rmcp client (connect, OAuth, tool catalog). No rpi deps.
  amp-core/           rpi glue: config, roles, tool policy, worker supervisor.
  amp-tui/            chat shell on rpi-tui, AMP theme.
  amp-harness/        the binary.
scripts/smoke.sh      smoke test (grows per milestone)
```

Dependencies only point one way: `amp-mcp <- amp-core <- amp-tui <- amp-harness`. Keep rpi out of
`amp-mcp` so the MCP and OAuth code can be tested on its own.

## Connecting to AMP (`amp-mcp`, D2/D4)

| setting  | flag          | env                | `~/.config/amp-harness/config.toml` | default                          |
|----------|---------------|--------------------|-------------------------------------|----------------------------------|
| URL      | `--url`       | `AMP_MCP_URL`      | `[server] url`                      | `https://amp.simstech.cloud/mcp` |
| extra CA | `--ca-file`   | `AMP_CA_FILE`      | `[server] ca_file`                  | none (system roots)              |
| protocol | `--protocol`  | `AMP_MCP_PROTOCOL` | `[server] protocol`                 | `auto`                           |
| token    | `--token-env` | `AMP_TOKEN`        | never                               | none                             |

Flags beat env, env beats the file (`$XDG_CONFIG_HOME` is honoured). A relative `ca_file` is relative
to the config file. `.amp.json` is a project marker only; its `amp_api` is not read.

```toml
[server]
url = "https://amp.local/mcp"
ca_file = "amp-local-ca.crt"
protocol = "auto"            # auto | 2026-07-28 | 2025-11-25
```

- `auto` speaks the stateless 2026-07-28 protocol (`server/discover`) and falls back to the legacy
  `initialize` handshake at 2025-11-25 when the server is older. Forced modes are for compatibility
  tests.
- `AMP_TOKEN` (an `amp_…` token) is sent as `Authorization: Bearer`. It is held in memory only and
  redacted from `Debug`. When set, it wins over any stored login.
- Server instructions come from `server/discover` (or `initialize`). `tools/list` is cached for as
  long as the server's `ttlMs` hint allows (AMP: 5 min, private). Every call has a 120 s timeout,
  and a timeout is returned as an error.

Tests run rmcp's own Streamable HTTP server in-process (`crates/amp-mcp/tests/`), in both eras, plus
an HTTPS server signed by a throwaway CA for `--ca-file`.

### Logging in (OAuth, D3)

```sh
amp-harness login  [--url URL] [--ca-file PEM] [--no-browser] [--timeout SECS]
amp-harness logout [--url URL]
```

- `login` probes the server, follows its 401 challenge through RFC 9728 and RFC 8414 discovery,
  registers a public client (DCR), and opens the browser on a PKCE S256 authorization URL whose
  redirect is a loopback listener on `127.0.0.1:<random port>/callback`. The scopes include
  `offline_access`, so a refresh token comes back. Against a server with auth off (compose) it
  says there is nothing to log in to.
- Logins are stored per server URL (normalised), so local, cluster and hosted logins coexist:
  - OS keychain, service `amp-harness`, account = server URL (default when the keychain answers);
  - otherwise `~/.config/amp-harness/credentials.json` (`$XDG_CONFIG_HOME` honoured), mode 0600 in
    a 0700 directory. `AMP_CREDENTIAL_STORE=keychain|file|auto` forces one.
- Connecting (`amp_mcp::oauth::connect`): `AMP_TOKEN` first, then the stored login (refreshed
  before expiry, and once more if the server rejects the token), then no credentials. When the
  refresh token is rejected, calls fail with "AMP login required: run `amp-harness login`" and
  health reads `re-login needed`.
- docker-desktop cluster: the cluster's protected-resource metadata names `https://amp.local/mcp`
  and the authorization server `https://amp.local/mcp-oauth` (port 443). rmcp requires the resource
  to match the URL you log in to (RFC 9728), so log in to `https://amp.local/mcp` with envoy
  reachable on 443; a port-forward on 8443 fails discovery with a resource mismatch.
- No token value is ever printed or logged. Do not enable `debug` tracing for
  `rmcp::transport::auth`: rmcp logs the authorization code and token response at that level.

Tests (`crates/amp-mcp/tests/oauth.rs`) run a mock authorization server (protected-resource and AS
metadata, DCR, PKCE S256 check, rotating refresh tokens, `invalid_grant`) with a scripted browser.
The keychain test uses a per-run service `amp-harness-test-<pid>`, deletes its entry, and skips
where no keychain answers (Linux CI) or when `AMP_TEST_SKIP_KEYCHAIN` is set.

## Commands (M1-4)

Every command below takes the connection flags `--url`, `--ca-file`, `--protocol`, `--config` and
`--token-env` (precedence above) and connects with D3's order: `AMP_TOKEN`, else the stored login,
else no credentials.

```sh
amp-harness doctor [--json]          # config, credentials, connect, tools, contract, whoami, project
amp-harness whoami [--json]
amp-harness tickets list [--project N] [--owner mine|unassigned|any|EMAIL] [--state S[,S]] [--json]
amp-harness tickets get ID [--comments] [--json]
amp-harness mcp call TOOL ['{"json": "args"}' | -] [--raw] [--timeout SECS]   # hidden debug command
amp-harness mcp tools [--json]                                                # hidden
```

- `.amp.json` (nearest one upwards from the working directory) only supplies the default
  `project_id`; its `amp_api` field is ignored (D4). `AMP_HARNESS_CEILING_DIR` stops the search.
- Ticket states use the web UI's words: `question`, `in_progress`, `ready`, `scheduled`, `waiting`,
  `done` (labels Question, In progress, Ready, Scheduled, Waiting, Done). `--state` also takes
  `blocked` (waiting + question + scheduled), `open` (the default: all but done) and `all`.
- `doctor` exits 1 when a check fails (warnings don't count) and lists the D11 contract tools the
  server is missing. It describes auth by source only and never prints a token.
- `mcp call` prints the tool's JSON (or text); a tool error goes to stderr with exit 1. Bad
  arguments exit 2.
- `--json` output is a contract for scripts: fields are added, never renamed or removed. The full
  shapes are pinned in `crates/amp-harness/tests/cli.rs` against a mock AMP (`tests/common`).

## Providers and models (M2-3)

Like opencode, several providers are active at once and every role (agent) picks its own
`provider/model` and thinking level. There is no default provider. The shipped defaults are
`crates/amp-core/models.toml` (embedded): the `anthropic` and `openrouter` providers, their models,
`[roles.*]` mirroring `v2/.opencode/agent/*.md` (a unit test reads those files and fails on drift),
and an Anthropic-only `[profiles.anthropic]`. Override any of it in
`~/.config/amp-harness/config.toml` (same file as `[server]`):

```toml
[models]
profile = "opencode"          # default | "anthropic" | "inherit" | a [profiles.<name>] of yours
# inherit = "anthropic/claude-sonnet-5-5"   # with profile = "inherit": every role uses this
# inherit_thinking = "medium"

[roles.reviewer]              # one role on another provider; the rest keep theirs
model = "anthropic/claude-opus-5-5"
thinking = "high"             # off minimal low medium high xhigh max

[providers.openrouter]        # field-by-field over the shipped provider
stream_idle_timeout_secs = 180

[providers.llamabarn]         # a provider of your own
api = "openai-completions"    # anthropic-messages | openai-completions | faux
base_url = "http://localhost:2276/v1"
auth = "none"
[providers.llamabarn.models."qwen-3.6-35b-a3b"]
context_window = 262144
```

- A role's key is its agent id without `amp-`: `manager`, `worker-backend`, `worker-frontend`,
  `worker-docs`, `reviewer`, `github-reviewer`, `researcher`, `github-researcher`, `kb-curator`,
  `designer`, `media`, plus any agent you add (see Roles below).
- A role resolves from the defaults' `[roles.<role>]`, then the selected profile (or the inherit
  model), then your `[roles.<role>]`. `AMP_HARNESS_PROFILE` overrides `[models] profile`. Profiles:
  `opencode` (the shipped `[roles.*]`: the manager, researchers and media on OpenRouter's
  `xiaomi/mimo-v2.6-pro`, workers on `deepseek/deepseek-v4.1-flash-1017`, reviewers on `z-ai/glm-5.3`,
  the designer on `moonshotai/kimi-k3`), `anthropic` (Opus for the manager and reviewers, Sonnet for
  the other workers) and `inherit` (one `[models] inherit` model for every role).
- When a layer changes a role's `model`, the lower layers' `extra_body` is dropped; `thinking`
  carries over unless that layer sets it.
- Keys come from the provider's `key_env` (`ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`), then the
  keychain (service `amp-harness`, account `provider:<id>`; skipped with
  `AMP_CREDENTIAL_STORE=file`). Never from config files.
- `header_timeout_secs` (first streamed event) and `stream_idle_timeout_secs` (gap between events)
  cancel a stalled request and end the turn with an error; `request_timeout_secs` bounds the whole
  request.

```sh
amp-harness config show [--json] [--check] [--config PATH]
```

It prints each provider (api, URL, where its key comes from, never the key, and its timeouts) and
each role's provider/model, thinking level, readiness and the layer that set it. `--check` asks each
provider in use for its live model list (OpenRouter's is public; Anthropic's needs the key) and
flags ids that are missing. It exits 1 if a role does not resolve or a checked id is missing.

`config show --config PATH` reads that file. The chat and `run` always read `[models]`, `[roles]`,
`[providers]` and `[workers]` from the default config.toml; their `--config` only changes `[server]`.

## Headless runs (M2-4)

```sh
amp-harness run -p "What is on the board?" [--json] [--role ROLE] [--cwd DIR] [--no-session]
amp-harness run --print-system-prompt [--role ROLE]
```

`run` builds one agent session (the manager unless `--role` names another) and runs the prompt
until the agent stops. `-p -` reads the prompt from stdin. The session has:
- the role's model from `config show`; a role whose provider has no key fails with the key to set;
- rpi-tools (read, bash, edit, write, grep, find, ls) in the working directory plus every AMP tool
  under AMP's own name, filtered by the role's policy, which also blocks denied calls;
- a system prompt made of the role prompt, AMP's server instructions, an `<amp_context>` block
  (role, server, the `.amp.json` project, working directory), the role's preloaded skills in full,
  `AGENTS.md`/`CLAUDE.md` files (`~/.config/amp-harness/AGENTS.md`, then the working directory and
  its parents) and the list of every skill with its path;
- a JSONL transcript under `~/.local/state/amp-harness/sessions/<role>/` (`--no-session` keeps it
  in memory).

Text output streams the answer to stdout and logs tool calls on stderr. `--json` prints one JSON
object per line: `session`, then `turn_start`, `text_delta`, `assistant` (text, tool calls, stop
reason, usage), `tool_start`, `tool_end` (`is_error` covers policy denials, AMP errors and timeouts),
`retry`, `turn_end`, and last `result` (outcome, final text, turns, tool counts, usage totals). Fields
may be added, never renamed or removed. The exit code is 0 when the run completed, otherwise 1;
Ctrl-C aborts the run.

Offline runs use a faux provider and a scripted model:

```toml
# ~/.config/amp-harness/config.toml
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]
[roles.manager]
model = "faux/faux-1"
```

```sh
echo '[{"tool_calls":[{"name":"amp_whoami"}]},{"text":"You are {{last_tool_result}}"}]' >script.json
AMP_HARNESS_FAUX_SCRIPT=script.json amp-harness run -p "who am I?" --json
```

Each step is one model response: `text`, `tool_calls` (`[{name, arguments, id?}]`) and `thinking`.
`{{last_tool_result}}` and `{{system_prompt}}` in `text` are filled from what the model was sent.
The file can also map provider ids to steps. A script that runs out fails the run instead of hanging.

## Chat (M2-7)

```sh
amp-harness [--role ROLE] [--cwd DIR] [--no-session] [--theme auto|dark|light|FILE] [--url URL] [--ca-file PEM] [--protocol P] [--config PATH] [--token-env VAR]
```

With no subcommand, `amp-harness` opens a full-screen chat with the manager (or `--role`), built
exactly as `run -p` builds it, so the conversation carries on across prompts. The transcript is a
JSONL session under `~/.local/state/amp-harness/sessions/<role>/` unless `--no-session`. It needs a
terminal on stdin and stdout; without one it exits 2 and points at `run -p`.

- Commands: `/model` lists the current model and every model whose provider has a key;
  `/model provider/model` switches this session's model. `/usage` prints the session's tokens and
  cost, `/help`, `/abort`, `/quit` (also `/exit`, `/q`).
- Keys: Enter sends, Shift+Enter is a new line, Esc aborts the running reply. Ctrl-C aborts a run,
  else clears the editor, else quits; Ctrl-D on an empty editor quits.
- The terminal is restored on exit, on a signal and on a panic. Quitting mid-run aborts the run
  (and any workers it started) and waits briefly so the session file is complete.

### Theme (M3-1)

The chat uses the AMP theme: `amp-dark` or `amp-light` (provisional simstech tokens v1, #1815),
with square borders and AMP's ticket-state, epic and brand colours.

| setting | flag      | env                 | `config.toml` `[tui]` | default |
|---------|-----------|---------------------|-----------------------|---------|
| theme   | `--theme` | `AMP_HARNESS_THEME` | `theme`               | `auto`  |
| colours | -         | `AMP_HARNESS_COLOR` | `color`               | `auto`  |

- `theme`: `auto` asks the terminal for its background (OSC 11) and picks light on a light
  background, else dark. Use `dark` / `light` to skip the query, or give a path to your own theme
  file (same JSON format as `crates/amp-tui/themes/amp-dark.json`).
- `color`: `auto` uses 24-bit colour when `COLORTERM` is `truecolor`/`24bit`, else the 256-colour
  fallbacks. `truecolor` or `256` forces one.

The header, status line and AMP slash commands come in M3.

## Roles, agents and skills (D5, D6)

The roster and skills are generated from `v2/.opencode` into `v2/harness/assets/` by
`v2/scripts/build-harness-assets.sh` (`make -C v2/harness assets`; `assets-check` fails on drift)
and embedded in the binary. Each agent file `agents/amp-<id>.md` is one role: its markdown body is
the system prompt, its frontmatter `permission` block is the tool policy, and a `skills:` list (else
a built-in table) names the skills preloaded into its prompt in full. The manager preloads
`amp-index` and `amp-planning`, workers `amp-execution`, reviewers `amp-review` plus their review
skill. Every other skill is listed with its path, for the agent to read when it needs it.

You can override or add agents and skills from disk. Later layers win:

1. embedded: `v2/harness/assets/{agents,skills}`;
2. user: `~/.config/amp-harness/{agents,skills}/` (`$XDG_CONFIG_HOME` honoured);
3. repo: `<dir with .amp.json>/.amp/harness/{agents,skills}/`.

An agent override (`agents/amp-manager.md`) replaces the whole agent. Skills override per file:
`skills/amp-kb/SKILL.md` replaces only that file, and the skill keeps its other embedded files. Files
that don't exist in the embedded set add new agents or skills. Names are lowercase letters, digits
and hyphens. A malformed override is an error naming the file, never skipped silently. A new agent
needs a `[roles.<id without amp->]` model in config.toml.

Tool policy uses opencode's `permission` semantics, enforced by the harness before each tool call:
the last matching rule wins, `ask` is treated as `deny` (nothing ever prompts), and a denied call
comes back to the model as a tool error ("Denied by the amp-<role> tool policy"). Workers can never
call `question` or dispatch other agents. Under a restricted bash policy, command substitution and
redirection to a file are denied too.

## Dispatching workers (M2-5)

A primary role (the manager) gets the native tool `amp_run_ticket(task_id, agent_id, instructions?,
worktree?, branch?, base?)`. It runs one specialist on one ticket in the foreground of the tool
call and returns the worker's final message, outcome, usage and where its work is:

1. The agent must be in the roster and allowed by the manager's `task` policy; otherwise nothing
   happens in AMP.
2. It waits for a worker slot, then calls `amp_dispatch_task` (AMP refusing it ends the call).
3. Roles that edit files run in a git worktree `<repo>/.worktrees/<ticket>` on branch
   `amp-<ticket>` off `HEAD` (reused if it exists; `worktree`, `branch` and `base` override this).
   Read-only roles run in the manager's directory. `.worktrees/` is added to the repo's
   `info/exclude` if nothing ignores it yet.
4. The worker is its own agent session: its role's model and thinking, its prompt plus the ticket,
   worktree and branch, tools bound to its directory, and a JSONL session under
   `~/.local/state/amp-harness/sessions/<ticket>/`.
5. It runs until it stops, hits the timeout or the turn budget, or is cancelled (the manager's run
   was aborted). Ending a worker early aborts its in-flight tools.

Limits, in `~/.config/amp-harness/config.toml` or the environment (env wins):

```toml
[workers]
max_parallel = 4          # AMP_HARNESS_MAX_WORKERS: workers at once; extra calls wait for a slot
timeout_minutes = 60      # AMP_HARNESS_WORKER_TIMEOUT_SECS (seconds): wall clock per worker
max_turns = 400           # AMP_HARNESS_WORKER_MAX_TURNS: model turns per worker; 0 = no limit
```

Unknown keys and non-positive `max_parallel` or timeouts are errors.

## Run reporting (M2-6)

AMP reports token spend per ticket and traces code to tickets from the run fields on
`amp_complete_task` / `amp_block_task`. Models forget them or can't know them, so the harness fills
them in for every worker it dispatches:

- When a worker completes or blocks **its own ticket**, the harness fills `model`, `input_tokens`,
  `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `cost_usd`, `branch`, `commit` and
  `pushed`, but only the fields the model left out. Usage is the worker's live totals; git comes from
  its worktree.
- A worker that ends without closing its ticket (error, timeout, cancel, turn budget, or it just
  stopped) is reported with `amp_report_run`, outcome `abandoned` and a note
  `abandoned: <reason>`, with the same usage and git fields, so the board shows the run and its
  tokens.
- Only numbers the provider reported are sent: no token fields without usage, and no `cost_usd` when
  the cost is 0 (a model without prices in models.toml, or the faux provider).
- Every session also has a native `amp_session_usage` tool (the session's model, tokens and cost so
  far), so the amp-execution skill's completion step works unchanged.

## Dev loop

```sh
make -C v2/harness build      # cargo build --workspace --all-targets --locked
make -C v2/harness test       # cargo test --workspace
make -C v2/harness lint       # fmt --check + clippy -D warnings (same as CI)
make -C v2/harness smoke      # build + scripts/smoke.sh
make -C v2/harness version    # amp-harness --version: harness version + rpi/rmcp pins
make -C v2/harness clean      # delete target/ (do this before removing a worktree)
```

### Smoke test (`make smoke`)

`scripts/smoke.sh` runs the built binary against a real AMP and exits non-zero on any failure. Each
leg runs `doctor --json` and `whoami --json`. It then creates a throwaway project (epic, story, two
dependent tickets, a comment) with `mcp call` and reads it back with `tickets list|get` and with
`run -p` (the manager on a scripted faux model calling `amp_get_task`). Then it runs the dispatch
cases (M2-8): the manager (faux `boss`) dispatches worker-backend (faux `wf`) with `amp_run_ticket`
in a worktree of a throwaway git repo under `target/`; the worker comments and completes with only
`task_id`, and the ticket's run must carry `wf/wf-1` and tokens > 0. A second worker's script runs
out, which must leave one abandoned run with a note `abandoned: error: ...`. Calls the role policy
denies (the manager's bash, the worker writing outside its worktree) must come back as visible tool
errors and never run. Cost is not asserted: the faux provider always reports 0. Finally it
deletes the tickets, story and epic and archives the project. AMP has no hard project delete, so the
leg checks that the project is gone from `amp_list_projects`. Cleanup runs from an exit trap, so it
also happens when a check fails. The harness runs with HOME under `target/` and
`AMP_CREDENTIAL_STORE=file`, so your keychain and config.toml are never touched.

| env | default | |
|---|---|---|
| `AMP_MCP_URL` | `http://localhost:8000/mcp` | compose leg (`cd v2 && make up`; auth off, expects `local-dev@amp.local`) |
| `AMP_SMOKE_PROTOCOLS` | `auto 2025-11-25` | one run per mode. `auto` must land on the stateless 2026-07-28 era |
| `AMP_TOKEN` + `AMP_CA_FILE` | unset, so the cluster leg is skipped | cluster leg: an `amp_` token from the UI and `amp-local-ca.crt`. The token is only sent to this leg |
| `AMP_SMOKE_CLUSTER_URL` | `https://amp.local/mcp` | envoy on 443, not `:8443` (RFC 9728 resource match) |
| `AMP_SMOKE_LEGS` | `compose cluster` | |

On the dev Mac, the docker-desktop cluster's LoadBalancer can hold host :8000. In that case, publish
compose's amp-api on another port and pass e.g. `AMP_MCP_URL=http://localhost:18000/mcp`. Don't run
`make up`: it kills whatever holds :8000, which there is Docker Desktop itself.

CI: the `harness` job in `.github/workflows/harness.yml` runs fmt, clippy `-D warnings` and the tests
on any push or PR that touches `v2/harness/**`.

Pins are exact. Bumping `rpi-*` or `rmcp` is its own ticket, gated on the smoke test. `--version`
prints the versions resolved in `Cargo.lock`, and a test fails if they drift from the pins.

## The Rust disk rule (mandatory on the dev Mac)

The internal disk has only a few GiB free; Rust builds have filled it before. So:

- Build only inside the repo on `/Volumes/external-data`, in a worktree under
  `/Volumes/external-data/development/simstech/amp/.worktrees/<ticket>`.
- Use `make -C v2/harness ...` (it exports `CARGO_HOME` for you), or run
  `export CARGO_HOME=/Volumes/external-data/development/.cargo-home` before **every** raw cargo
  command. Agent shells don't load `~/.zshrc`, and without it cargo fills `~/.cargo`.
- The target dir is `v2/harness/target` in your worktree. Never build in `/tmp`, `/private/tmp` or a
  Claude scratchpad.
- Don't install another rustup toolchain; 1.98.1 (with clippy and rustfmt) is already installed.
- Check `df -h /System/Volumes/Data` before and after building. Under 3 GiB free: stop.
- `make -C v2/harness clean` (or delete `target/`) before you remove the worktree.

Details: AMP KB `how-to/rust-builds-on-external-drive.md`.
