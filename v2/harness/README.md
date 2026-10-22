# amp-harness

AMP's own agent harness: a Rust binary built on pi-rust's core crates (`rpi-*`) with an MCP client on
`rmcp`. Design and build plan: AMP project 2 KB, `decisions/amp-harness-architecture.md`.

Status: **M2.** `amp-mcp` connects to AMP (both MCP protocol eras, static token or OAuth login).
`amp-harness` with no subcommand is a chat with the manager, which plans on the board and dispatches
specialist workers itself (`amp_run_ticket`); every worker's run, tokens and commit are reported to
its ticket automatically. The CLI also has `login`, `logout`, `doctor`, `whoami`, `tickets list|get`,
`config show`, `connect`/`disconnect` (model provider keys), `run -p` (a headless agent run) and the
hidden `mcp call`. `make smoke` runs them against a real local AMP.

## Try it

From the repo root of a worktree on `/Volumes/external-data` (see the Rust disk rule below):

```sh
make -C v2/harness build
alias amp-harness="$PWD/v2/harness/target/debug/amp-harness"

# 1. A model provider key: asked hidden, checked with the provider, kept in the OS keychain.
amp-harness connect                 # pick OpenRouter (the shipped roles), Anthropic, OpenAI or custom
amp-harness connect --list          # every provider and where its key comes from (never the key)
# CI/headless: set OPENROUTER_API_KEY (or ANTHROPIC_API_KEY) in the environment instead.

# 2. Every role resolves to a provider/model that has a key, and the model ids exist.
amp-harness config show --check

# 3a. Against compose (auth off; on the dev Mac amp-api is published on :18000, see Smoke test).
amp-harness server set localhost:18000 --name local   # checks it, finds the UI on :5173, saves it
amp-harness doctor
amp-harness                          # chat with the manager; /help lists the commands

# 3b. Or against the docker-desktop cluster (OAuth, self-signed CA).
kubectl --context docker-desktop -n amp get secret amp-ca-secret \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > amp-local-ca.crt
amp-harness server set amp.local --name cluster --ca-file amp-local-ca.crt   # offers the login
amp-harness doctor

# Switch any time; each server keeps its own login.
amp-harness server                   # the server in effect, where each setting comes from, the saved ones
amp-harness server use local
```

Run it from a directory with an `.amp.json` (its `project_id` is the default project), or tell the
manager which project to use. `amp-harness run -p "What is on the board?"` does the same without
the TUI. In the chat, `/server` lists the saved servers and `/server <name>` switches to one without
a restart. `--url` / `AMP_MCP_URL` still work for one command or one shell.

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
| web UI   |               | `AMP_WEB_URL`      | `[server] web_url`                  | the MCP URL's origin             |
| token    | `--token-env` | `AMP_TOKEN`        | never                               | none                             |

Flags beat env, env beats the file (`$XDG_CONFIG_HOME` is honoured). A relative `ca_file` is relative
to the config file. `.amp.json` is a project marker only; its `amp_api` is not read. The web UI
setting only changes links (`/open`, the header's host): set it when the UI is not served from the
MCP origin, as on docker compose (`AMP_WEB_URL=http://localhost:5173`).

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

### Saved servers: `amp-harness server` and `/server` (#2164)

Nobody has to edit config.toml or export `AMP_MCP_URL` to point the harness at an AMP:

```sh
amp-harness server [--json]                      # the server in effect: URL, web UI, CA file, protocol, auth,
                                                 # each with its source (flag / env / config / default), and the saved servers
amp-harness server set <url> [--name N] [--web-url U] [--ca-file PEM] [--protocol P] [--yes] [--json]
amp-harness server use <name> [--yes] [--json]   # make a saved server the active one
amp-harness server remove <name>
```

- `set` takes `host`, `host:port` or a full URL, like the first run (`localhost` gets http, anything
  else https, `/mcp` is added when there is no path). It connects first (discovery, then
  `amp_whoami`): a server that does not answer is never saved; one that answers 401 is saved and,
  in a terminal, offers the browser login right away (`--yes` never asks: it prints the `login` to
  run). A refused `AMP_TOKEN` is reported by its variable, never its value.
- The web UI address is found, not asked for: nothing is saved when the server's own origin serves
  the UI; docker compose's UI on `:5173` is found by a probe. Only an address no probe finds is
  asked for (`--web-url` answers it up front).
- The name defaults to `hosted` (hosted AMP), `local` (localhost) or the host's first label. Running
  `set` again for a saved URL keeps its CA file, protocol and web UI unless given.
- `use` switches without checking first, but says when the server does not answer and offers the
  login it needs. Logins are kept per server URL, so every saved server keeps its own.
- `AMP_MCP_URL` and `--url` still win over the saved server; `server` and `set` say so when one is set.
- In the chat, `/server` lists the saved servers (the current one marked) and `/server <name>` (or
  `/server <address>`, which also saves it) switches the session without a restart: it connects to
  the new server first (a failure leaves everything as it was), then moves the session's AMP
  connection, gives the model the new server's AMP tools and server details in its system prompt
  (from the next reply; the transcript stays), and starts a new metadata poller for the header and
  the AMP commands. It is refused while a reply or a worker is running. The new server is saved as
  the active one, so the next start uses it too.

In config.toml, `[server]` is still the server in effect (so every command, and older builds, read
it), with `name` naming the saved server it came from; saved servers are `[servers.<name>]` tables
with the same keys. Every write keeps your comments and other settings (it is refused unless the
file parses to exactly what it was plus the change), backs the old file up to `config.toml.bak`, and
never holds a secret. A `[server]` you wrote by hand is kept as a saved server when another one
becomes active.

```toml
[server]                     # the server in effect (written by `server set` / `server use`)
name = "local"
url = "http://localhost:18000/mcp"
web_url = "http://localhost:5173"

[servers.local]
url = "http://localhost:18000/mcp"
web_url = "http://localhost:5173"

[servers.hosted]
url = "https://amp.simstech.cloud/mcp"
```

### When AMP can't be reached (#2178)

A connection failure is one sentence that says what went wrong and what to do next, never rmcp's
or reqwest's own error text:

| Failure | What you see (abridged) |
| --- | --- |
| Nothing listens there | `Can't reach AMP at localhost:18999 (connection refused). Is the server running? Check with `amp-harness doctor`, or switch servers with `amp-harness server set <url>` (or --url).` |
| Unknown host | `Can't reach AMP at amp.example.invalid (host not found). Check the address with `amp-harness server`, …` |
| Untrusted certificate, or https to an http port | `Can't connect securely to AMP at … For a private CA save it with `amp-harness server set <url> --ca-file <PEM>` …` |
| 401 / 403 | `AMP at … needs you to log in (401). Run `amp-harness login --url …`` (with AMP_TOKEN: `refused the token in AMP_TOKEN`, never its value) |
| Wrong path | `AMP at … has no MCP endpoint at … (HTTP 404). Check the address (it usually ends in /mcp) …` |
| Not an MCP server | `The server at … did not answer as AMP (it answered HTTP 501, not MCP). …` |
| No answer | `AMP at … did not answer (no answer within 30s). Check the server and your network with `amp-harness doctor` …` |

`--verbose` (after the subcommand: `amp-harness whoami --verbose`), or any non-empty `RUST_LOG`,
adds the full cause chain on a `cause:` line.

**The chat opens anyway.** When AMP is unreachable at start (or the first run can't reach the
server already configured), the chat opens with that sentence in the transcript, `(not connected)`
in the banner and a red dot in the header. Chatting with the model works meanwhile, without AMP
tools. It retries in the background (2 s, doubling to 60 s); once connected the transcript says
"Connected to AMP at …", and the manager gets its AMP tools and the server's instructions from its
next reply. `/server <name|address>` connects (or switches) at once. Headless commands (`run -p`,
`whoami`, `tickets`) print the sentence and exit 1.

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
- Re-login without a restart (#2156, #2159): run `amp-harness login` in another terminal. The chat's
  header reconnects within 15 s, and so does the session's own connection (the agent's `amp_*`
  tools, run reporting, workers): the next call after the login reconnects from the store, at most
  once every 15 s, and runs on the new connection. A call still running on the old connection fails
  as a tool error ("connection was replaced"), never hangs. With `AMP_TOKEN` nothing reconnects: set
  a new token and restart.
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

## Keychain on macOS (#2165)

Every keychain call goes through `amp_mcp::keychain_{get,set,delete}`:

- **One read per item per process.** The first lookup of a (service, account) asks the keychain;
  the answer is cached for the life of the process: the secret, "not there", or the error. Our own
  writes and deletes update the cache. A whole `run -p` reads each item once (the OAuth login for
  the server URL, `provider:anthropic`, `provider:openrouter`). The one deliberate re-read: just
  before an OAuth refresh, so a refresh token another harness process rotated is picked up.
- **The harness creates its own items.** On macOS a write deletes any existing item (falling back
  to `/usr/bin/security delete-generic-password`, which can delete items made by other programs
  without a dialog) and adds a new one, so this binary is the item's creator and is trusted to read
  it. An item made by Apple's `security` tool has partition `apple-tool:`; every read by
  amp-harness then raises a dialog and "Always Allow" does not stick. Updating it in place would
  keep that ACL. Store keys with `amp-harness connect` instead.
- **No dialog nobody can answer.** When stdin is not a terminal (agents, scripts, CI) keychain
  dialogs are turned off for each call, so an item that would need approval fails at once with one
  message naming the item and what to run, instead of blocking. `AMP_HARNESS_KEYCHAIN_PROMPTS=never`
  forces that; `allow` always lets the dialog show.
- **After a rebuild or an update** an unsigned (ad-hoc signed) binary has a new code hash, so the
  first read of each item by the new build asks once; "Always Allow" then holds for that build.
  Stable code signing (M5) removes that prompt.
- A provider item that can't be read says to run `amp-harness connect <provider>`; the OAuth
  login item says `amp-harness login`.
- `AMP_HARNESS_KEYCHAIN_TRACE=<file>` appends one line per real keychain call (operation, service,
  account, outcome, pid; never a secret).

`crates/amp-harness/tests/keychain.rs` (opt-in: `AMP_TEST_KEYCHAIN=1`, macOS) stores a key with
`init` and reads it in new processes with dialogs off, counting calls per process, and checks that
an item made by `security` fails fast with one message and is then replaced by `init`. Its #2163
cases store keys with `connect --key-stdin` (a mock provider checks them), remove them with
`disconnect`, keep a custom provider's secret header in the keychain, and prove that the chat's
`/connect` makes the running chat use a provider that had no key when it started. The smoke
test's `keychain` leg (`AMP_SMOKE_LEGS=keychain`, macOS, needs the compose server) does the same
with the built or installed binary (`AMP_HARNESS_BIN`). Both use throwaway services, never yours.

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

## Connecting model providers (#2163)

```sh
amp-harness connect [PROVIDER]                 # in a terminal: picker, hidden key, check, keychain
amp-harness connect --list [--json]            # providers, key yes/no and its source, roles using it
amp-harness disconnect PROVIDER [--keep-config] [--json]
echo "$KEY" | amp-harness connect openrouter --key-stdin [--use] [--json]       # scripts
amp-harness connect my-llm --base-url http://localhost:11434/v1 [--api openai-completions] \
  [--name "My LLM"] [--model ID]... [--header NAME=VALUE]... [--key-stdin] [--use [--role-model ID]]
```

- **Built in:** OpenRouter and Anthropic (shipped) and OpenAI (written to config.toml on first
  connect, with the models the API lists). **Custom:** any OpenAI-compatible endpoint (vLLM, Ollama,
  LM Studio, LiteLLM, a gateway) or an Anthropic Messages one: id, display name, base URL, API
  style, extra headers and model ids (listed by the endpoint, or `--model`). An empty key makes a
  keyless endpoint (`auth = "none"`).
- The key is read hidden (nothing is echoed) or from stdin with `--key-stdin`; never pass it as an
  argument. It is checked with a call that spends no tokens (Anthropic `GET /v1/models`, OpenRouter
  `GET /key`, others `GET /models`): a rejected key is asked again in a terminal and stores nothing
  in a script. It is stored only in the OS keychain (service `amp-harness`, account
  `provider:<id>`). With no keychain (`AMP_CREDENTIAL_STORE=file`, headless Linux) `connect` says to
  set the provider's variable instead; provider keys have no file fallback.
- Custom headers whose name or value looks secret (Authorization, *key*, *token*, `Bearer …`) go to
  the keychain (`provider:<id>:header:<name>`) and only their names to the file
  (`secret_headers = ["X-Api-Key"]`); `--header` refuses them, since arguments are visible to other
  processes.
- config.toml is the person's file: `connect` cuts out only `[providers.<id>]` (and its
  sub-tables), appends the new table, checks the result loads, and keeps the previous file as
  `config.toml.bak`. `disconnect` removes the key (and secret header values) and only a table that
  `connect` added (marked `# Added by amp-harness connect.`), never one written by hand, and not
  while roles still run on it.
- After connecting, `connect` offers to point the roles at the provider (`--use` does it without
  asking): Anthropic → the `anthropic` profile, OpenRouter → the default roster, anything else →
  `inherit` with one model for every role. It then prints each role's provider/model like
  `config show`.
- In the chat, `/connect` lists the providers (and picks up keys stored from another terminal) and
  `/connect PROVIDER` (or `/connect ID BASE_URL [API]` for a custom endpoint) asks for the key: it
  is typed or pasted into a hidden buffer, never the editor, the transcript, its history or the
  session file. The chat uses the new key at once: its providers are rebuilt and handed to the
  running agent, and `/model` offers the new provider's models. Workers started later use them too.
- **No Claude subscription login.** Anthropic allows Claude Free/Pro/Max subscription logins
  (OAuth) only in its own products (Claude.ai, Claude Code, Claude Desktop, Cowork) and blocks them
  in third-party tools, so amp-harness offers only an Anthropic **API key** from
  console.anthropic.com. rpi-ai 0.8.0 has no subscription path (its Anthropic provider sends
  `x-api-key` only), and `connect`, `init` and `/connect` refuse a subscription token
  (`sk-ant-oat…`) with that explanation.
- Environment variables stay supported and take precedence: they are the CI/headless path.

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
  keychain (service `amp-harness`, account `provider:<id>`, stored by `amp-harness connect`;
  skipped with `AMP_CREDENTIAL_STORE=file`). Never from config files. A provider's `name` is its
  display name; `secret_headers` lists headers whose values come from the keychain.
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

### Secrets never reach an agent's bash (#2142)

Every session's `bash` (manager and workers) gets the harness's environment minus its own secrets:
`AMP_TOKEN`, the variable named by `--token-env`, and every provider's `key_env` in the loaded
models config (`OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, ...). A worker that runs `env` while
debugging would otherwise put them in its transcript, the provider request and possibly an AMP
comment or commit. Everything else is passed through unchanged. To hand a named secret to bash on
purpose (off by default), list it in config.toml:

```toml
[secrets]
pass_to_bash = ["OPENROUTER_API_KEY"]
```

### Bash never waits on a person (#2143)

Every `bash` call has a default timeout of 600 s, used when the model passes no `timeout` (a
model-supplied one still wins). Set it in config.toml, or override it with
`AMP_HARNESS_BASH_TIMEOUT_SECS`; `0` turns the default off:

```toml
[tools]
bash_timeout_secs = 600
```

Bash children also get a non-interactive environment: `GIT_EDITOR`/`EDITOR`/`VISUAL`/
`GIT_SEQUENCE_EDITOR=true` (so `git commit` without `-m` aborts on the empty message),
`GIT_TERMINAL_PROMPT=0`, `GCM_INTERACTIVE=never`, `SSH_ASKPASS_REQUIRE=never`,
`DEBIAN_FRONTEND=noninteractive`, `npm_config_yes=true`, and, only when unset,
`GIT_SSH_COMMAND="ssh -o BatchMode=yes"` and `CI=1`. `GIT_ASKPASS` and `SSH_ASKPASS` are removed.
rpi closes stdin and starts each command in its own process group, but it has no `pre_exec` hook
for `setsid`. Under a terminal, a program that reads `/dev/tty` is therefore stopped (SIGTTIN)
until the timeout kills it. With no controlling terminal (CI, supervised workers), the open fails
at once.

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

- Commands: `/model` opens a picker over every model whose provider has a key, the current one
  marked (↑↓ select, Enter switches, Esc closes); `/model provider/model` switches this session's
  model directly. `/connect [provider]` lists providers or
  stores a key without leaving the chat (see Connecting model providers). `/server` lists the saved
  AMP servers and `/server <name|address>` switches the session to one without a restart (see Saved
  servers). `/usage` prints the session's tokens and cost, `/help`, `/abort`, `/quit` (also
  `/exit`, `/q`).
- Keys: Enter sends, Shift+Enter is a new line, Esc aborts the running reply. Ctrl-C aborts a run,
  else clears the editor, else quits; Ctrl-D on an empty editor quits. Ctrl+T expands (or collapses
  again) the last thinking section; Ctrl+O shows (or hides) every tool call's arguments and result.
- Scrolling the transcript (#2185): the mouse wheel; PageUp/PageDown a page; with an empty editor,
  Ctrl+U/Ctrl+D half a page and End back to the latest output (Ctrl+End jumps from anywhere; with
  text in the editor these keys stay the editor's). Ctrl+D at the latest output still quits, but not
  within a second of scrolling with it, so holding it stops at the bottom. While you are scrolled up
  and output arrives below, a `↓ new output · End` pill shows on the transcript's last row; End or a
  click on it jumps down and follows the stream again. Sending a prompt also jumps to the end.
- Selecting text: the chat turns mouse reporting on (for the wheel), so a plain click-drag goes to
  the chat instead of the terminal. Hold **Fn** while dragging in Terminal.app, or **Option (⌥)** in
  iTerm2 (checked on macOS with both: Option-drag does not select in Terminal.app, nor Fn-drag in
  iTerm2). To get plain click-drag selection back, turn mouse reporting off with `[tui] mouse =
  false` (or `AMP_HARNESS_MOUSE=off`); the wheel then no longer scrolls the transcript (Terminal.app
  may send it as ↑↓ instead), and PageUp/PageDown do.
- Tool calls (#2183) are one line each: `✓ list_tasks · 3 tickets: 1 ready, 1 waiting · 12 ms`.
  AMP calls get a sentence for what they did (the contract tools, `kb_search` and `create_tasks`;
  any other falls back to the result's `key: value` list), durations under a second are in ms,
  and consecutive calls share one block (more than five fold their earliest successful ones into
  `✓ 4 calls · get_task ×3, list_tasks`). Only running and failed calls keep the tinted surface,
  and a failed call shows its error under it.
- Thinking (#2177) is collapsed by default: one dim line per section, `Thinking · 18s ▸`, which
  shows the model's latest sentence while it thinks and stops its timer when the answer starts.
  Expanded (Ctrl+T, or `show_thinking = "full"`), it is plain dim text behind a `│` gutter, with no
  markdown styling, so it never reads like the answer. The session file always keeps the full
  thinking.
- The terminal is restored on exit, on a signal and on a panic. Quitting mid-run aborts the run
  (and any workers it started) and waits briefly so the session file is complete.

### Theme (M3-1)

The chat uses the AMP theme: `amp-dark` or `amp-light` (provisional simstech tokens v1, #1815),
with square borders and AMP's ticket-state, epic and brand colours.

| setting  | flag      | env                         | `config.toml` `[tui]` | default     |
|----------|-----------|-----------------------------|-----------------------|-------------|
| theme    | `--theme` | `AMP_HARNESS_THEME`         | `theme`               | `auto`      |
| colours  | -         | `AMP_HARNESS_COLOR`         | `color`               | `auto`      |
| thinking | -         | `AMP_HARNESS_SHOW_THINKING` | `show_thinking`       | `collapsed` |
| mouse    | -         | `AMP_HARNESS_MOUSE`         | `mouse`               | `true`      |

- `theme`: `auto` asks the terminal for its background (OSC 11) and picks light on a light
  background, else dark. Use `dark` / `light` to skip the query, or give a path to your own theme
  file (same JSON format as `crates/amp-tui/themes/amp-dark.json`).
- `color`: `auto` uses 24-bit colour when `COLORTERM` is `truecolor`/`24bit`, else the 256-colour
  fallbacks. `truecolor` or `256` forces one.
- `show_thinking`: `collapsed` (one `Thinking · Ns ▸` line per section; Ctrl+T expands the last
  one), `full` (every section expanded as plain dim text) or `hidden` (none shown; Ctrl+T still
  shows the last one).
- `mouse`: `true` (the wheel scrolls the transcript; select with Fn-drag in Terminal.app,
  Option-drag in iTerm2) or `false` (no mouse reporting: plain click-drag selects). The env var
  takes `on`/`off`.

### Header and status line (M3-3)

Above the transcript, a two-line AMP header; between the editor and pi's footer, one AMP status
line. The harness fetches what they show on its own AMP connection (the metadata poller), never
through the model.

```text
▀▀▀▀▀▀▀▀ amp.simstech.cloud · ada@example.com · simstech › AMP #2        ● connected 2026-07-28
■ In progress #2106 M3-3 Header and AMP status line              ▌ AMP Harness › M3 theme + metadata
■  2 QUESTIONS  │ workers 3 ■ running · 1 queued │ plan: 2 waiting for your approval │ ■ 3 In progress · □ 5 Ready   unreported #2106
```

- Line 1: the AMP host, you, `org › project` (a link to the project in the web UI) and the MCP
  connection (green connected, amber connecting/slow/degraded, red disconnected/re-login needed).
- Line 2, while a ticket is current: its state in the web UI's words and colours, `#id` (a link
  to the board with the ticket open), its name and `Epic › Story` in the epic's colour.
- Status line: tickets waiting on a person's answer, workers (here and elsewhere), plan approval,
  the board counts, the ticket's artifacts in review, and whether this session reported its run.
  When the terminal is narrow, the board counts go first.

Links are OSC 8 hyperlinks (Cmd/Ctrl-click in most terminals). Text from AMP is shown with control
characters removed. `amp-harness debug render-chrome --width 100 [--theme dark|light] [--color
truecolor|256] [--plain] [--live [--ticket ID]]` prints the header and status line alone, from a
built-in sample or (`--live`) from AMP, for screenshots and render checks.

AMP slash commands come in M3-4.

### Waiting for your approval (#2171)

Only a person approves a plan, in AMP's web UI (epic 162): no agent can, not even one signed in as
you. When tickets wait for approval, the status line says `plan: N waiting for your approval` (a
link to the project's Flight plan page, kept even on a narrow line), `/plan` puts the same words
and the link first, and `/open plan` opens the page.

When AMP refuses the manager a step that is a person's (approving the plan, dispatching a ticket
planned after the last approval, marking a person's ticket done, turning plan approval off), the
transcript shows, under the refused call:

```text
  ⏸ Waiting for your approval
    ticket 2108 was planned after the plan was last approved; … A person approves it in the AMP web UI …
    Approve it in AMP: https://amp.simstech.cloud/project/2/plan   (/open plan)
    The manager stopped here; nothing else was run. Reply once it's done.
```

and the manager's turn ends: it doesn't retry another way. Approve in the web UI, then reply.

The manager's tool policy adds (all roles for the first two):

- `amp_approve_plan` is always denied, with a visible error pointing at `amp_check_plan`'s
  `approve_url`;
- `amp_update_project_settings` is denied when it changes `require_plan_approval` or
  `open_reassign` (name, description and metadata stay allowed);
- the manager's `amp_complete_task` and `amp_claim_task` work only on tickets you named in the
  session or that it dispatched itself. Status questions are read-only; whether a ticket is a
  person's is AMP's call (completing one is refused there, with its link).

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
errors and never run. Cost is not asserted: the faux provider always reports 0. Smoke v3 (M3-6)
adds a ticket with a question: `debug render-chrome --live` must show the project's name and `#id`
and the same status counts as `amp_list_tasks`, before and after the question is answered through
the chat's `/questions` panel (`--snapshot --meta`, Enter, the answer, `y`; the ticket must leave the
Question state with the answer as a comment), and `/open` must print the ticket's web UI URL. Finally it
deletes the tickets, story and epic and archives the project. AMP has no hard project delete, so the
leg checks that the project is gone from `amp_list_projects`. Cleanup runs from an exit trap, so it
also happens when a check fails. The harness runs with HOME under `target/` and
`AMP_CREDENTIAL_STORE=file`, so your keychain and config.toml are never touched.

| env | default | |
|---|---|---|
| `AMP_MCP_URL` | `http://localhost:8000/mcp` | compose leg (`cd v2 && make up`; auth off, expects `local-dev@amp.local`) |
| `AMP_SMOKE_PROTOCOLS` | `auto 2025-11-25` | one run per mode. `auto` must land on the stateless 2026-07-28 era |
| `AMP_SMOKE_LEGS` | `compose cluster` | add `keychain` (macOS) for the keychain-only leg: no key in the environment, real login keychain under a throwaway service, dialogs off, each item read at most once per process |
| `AMP_TOKEN` + `AMP_CA_FILE` | unset, so the cluster leg is skipped | cluster leg: an `amp_` token from the UI and `amp-local-ca.crt`. The token is only sent to this leg |
| `AMP_SMOKE_CLUSTER_URL` | `https://amp.local/mcp` | envoy on 443, not `:8443` (RFC 9728 resource match) |
| `AMP_SMOKE_LEGS` | `compose cluster` | |
| `AMP_SMOKE_WEB_URL` | `http://localhost:5173` | compose leg's web UI, passed as `AMP_WEB_URL`; `/open` must print it. The cluster leg uses the MCP origin. An unreachable UI is only a WARN |

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
