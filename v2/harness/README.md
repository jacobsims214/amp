# amp-harness

AMP's own agent harness: a Rust binary built on pi-rust's core crates (`rpi-*`) with an MCP client on
`rmcp`. Design and build plan: AMP project 2 KB, `decisions/amp-harness-architecture.md`.

Status: **M4.** `amp-harness` with no subcommand is a chat with the manager, which plans on the
board and dispatches specialist workers itself (`amp_run_ticket`); every worker's run, tokens and
commit are reported to its ticket automatically. The CLI sets everything up without editing a file:
`setup` (first run, org presets), `server`, `login`/`logout`, `connect`/`disconnect` (model
providers), `models`, `agents`, `skills`, `mcp` (any MCP server, import, trust), `config show|check`,
`doctor`, `whoami`, `tickets list|get` and `run -p` (a headless agent run). `make smoke` runs them
against a real local AMP.

**User guide:** the AMP guide's harness pages, `v2/kb/harness/` (served by `amp_guide`; start at
[`v2/kb/harness/start-here.md`](../kb/harness/start-here.md)). They cover install, servers,
providers and keys, models per role, agents and skills, MCP servers, configuration, running work and
troubleshooting, plus a step-by-step walkthrough for a company's own model endpoint
([`enterprise-provider.md`](../kb/harness/enterprise-provider.md)). This README is the reference
for developers and admins; keep the two in step.

## Install

macOS 13+ (Apple silicon or Intel) and Linux (x86_64 or aarch64, glibc 2.35+), one line:

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
```

With your company's org preset (its AMP server, its model endpoint and which model each role runs
on; no secrets, see [org presets](#your-companys-own-endpoint-models-per-role-org-presets-2275)):

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh -s -- --preset https://intranet.example.com/amp-preset.toml
```

The script downloads the newest `harness-v*` release for your platform, checks its SHA-256 and puts
`amp-harness` in `~/.local/bin` (no sudo). On a terminal it then asks
``Run `amp-harness setup` now? [Y/n]``. This works under `curl | sh` too, because the answer is
read from `/dev/tty`. Setup signs you in to AMP, links the current folder to a project and connects a
model provider, starting from the preset when you passed one. Without a terminal (CI, Dockerfiles)
it installs and prints the setup command instead.

| option (env) | |
|---|---|
| `--preset FILE\|URL` (`AMP_HARNESS_PRESET`) | handed to `amp-harness setup --preset`. A file is checked before anything is installed |
| `--no-setup` (`AMP_HARNESS_NO_SETUP=1`) | install only, then print the setup command |
| `--version X.Y.Z` (`AMP_HARNESS_VERSION`) | a specific release (default: the newest) |
| `--prefix DIR` (`AMP_HARNESS_PREFIX`) | where the binary goes (default `~/.local/bin`) |
| `--uninstall` | remove `DIR/amp-harness`. Config, keychain items and projects' `.amp.json` stay; it says where they are |
| `--modify-path` | add DIR to your login shell's profile when it isn't on PATH (a backup is kept). By default it only tells you the line to add |
| `--archive FILE [--sha256 HEX\|FILE]` | install a local archive (`make dist`) instead of downloading |
| `--target TRIPLE`, `--no-path-check` | override platform detection; skip the PATH check |

- **Upgrade:** run the same line again. It replaces the binary in place, without overwriting a
  running one, and says `Updated amp-harness 0.1.0 → 0.2.0`.
- **Uninstall:** `curl -fsSL …/install.sh | sh -s -- --uninstall`.
- **Linux:** any glibc distro with `curl` or `wget`, plus `tar`. The script stops with a clear message
  on Alpine and other musl systems (not supported yet), on glibc older than 2.35 (e.g. Ubuntu 20.04),
  and when a needed tool is missing.

**Until the first release.** The one-liner works only once the first `harness-v*` release is
published (M5-1, #2122) and the harness branch has reached `main`; before that it stops with "no
harness-v* release found". Install from a checkout instead (git and rustup needed):

```sh
git clone https://github.com/jacobsims214/amp.git && cd amp
git checkout harness-m4                    # until the harness reaches main
make -C v2/harness install                 # release build, packed and installed by install.sh
cd ~/code/my-project                       # setup links the folder it runs in
amp-harness setup --preset https://intranet.example.com/amp-preset.toml   # with an org preset
```

`make install` uses this same script but passes `--no-setup` (make runs it in `v2/harness`, not your
project), so it prints the setup command instead of offering it; `PREFIX=DIR` installs elsewhere.
`setup --url` takes what `server set` takes: a bare `host`, `host:port` or a full URL (one
normalisation for both, and for a preset's `server.url`).

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
to the config file. Which config file and directory are used is described in
[Where configuration lives](#where-configuration-lives-2232). `.amp.json` is a project marker only; its `amp_api` is not read. The web UI
setting only changes links (`/open`, the header's host): set it when the UI is not served from the
MCP origin, as on docker compose (`AMP_WEB_URL=http://localhost:5173`). The file's `web_url` belongs to
the file's `url`: when `--url` or `AMP_MCP_URL` names another server, it is not used (#2380), so
`--url https://amp.local/mcp` with a compose config gets links on `https://amp.local`.

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
  - otherwise `~/.config/amp-harness/credentials.json` (`$XDG_CONFIG_HOME` and `--config-dir`
    honoured), mode 0600 in a 0700 directory. `AMP_CREDENTIAL_STORE=keychain|file|auto` forces one.
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
The keychain test uses a per-run service `amp-harness-test-<pid>` and deletes its entry. Like
every test that writes the real keychain, it is opt-in: it runs only with `AMP_TEST_KEYCHAIN=1`, so a
plain `make test` never touches your login keychain or raises a keychain dialog.

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

## Where configuration lives (#2232)

Two layers of files, built-in defaults below both:

| layer | directory | config file |
|-------|-----------|-------------|
| user  | `--config-dir DIR`, else `AMP_HARNESS_CONFIG_DIR`, else `$XDG_CONFIG_HOME/amp-harness`, else `~/.config/amp-harness` | `--config FILE`, else `AMP_HARNESS_CONFIG`, else `<user dir>/config.toml` |
| repo  | `<dir with .amp.json>/.amp/harness` | `<repo dir>/config.toml` |

`--config` and `--config-dir` are global flags: they go before or after any subcommand
(`amp-harness --config F run -p …` and `amp-harness run -p … --config F` are the same) and apply to
every setting and every command, readers and writers alike: `[server]`, `[models]`, `[roles]`,
`[providers]`, `[profiles]`, `[workers]`, `[tools]`, `[secrets]` and `[tui]`, and `server`,
`connect`, `disconnect`, `init` and the chat's `/connect` and `/server` write that file.
`--config` moves only the file; `--config-dir` moves the whole user layer: `config.toml`,
`agents/`, `skills/`, `AGENTS.md` and `credentials.json`. A file named with `--config` /
`AMP_HARNESS_CONFIG` must exist for commands that only read it (a typo is an error); one inside a
fresh `--config-dir` may be missing. Relative paths are taken from the current directory.

The repo file shares what belongs to the project and nothing else: `[mcp.*]` (MCP servers, read by
the MCP layer), `[roles.*]` (same schema as the user file) and `[models] profile`. Anything else
(`[server]`, `[providers]`, `[secrets]`, `[workers]`, `[tools]`, `[tui]`, `[profiles]`,
`[models] inherit`, …) is an error naming the key and the file, so a cloned repo can't redirect
your AMP server, keys, bash environment or workers. Repo `[roles]` merge over the user's per role,
and the repo's `[models] profile` beats the user's (`AMP_HARNESS_PROFILE` beats both).
`config show` prints the repo file it merged and, per role, the layer that set it (`repo <path>`).

Every write to a config.toml (`connect`, `mcp add`, `models set`, the chat's `/model` and `/mcp`
panels, …) takes an advisory lock on `config.toml.lock` next to the file, re-reads the file under
it and writes through a uniquely named temporary file, so a chat and a CLI writing at the same time
both keep their change. A symlinked config.toml (stow, chezmoi, home-manager) stays a symlink: the
file it points to is written, and its lock sits next to that file.

A repo's `[mcp.*]` servers start only once you trust them, because they can run commands or
receive what the agents send. The chat asks for each one before it starts its MCP servers (`y`,
`a` for all of them, Enter for not now). `amp-harness mcp trust` lists the servers and their trust,
`mcp trust <name>…|--all` trusts them, and `mcp untrust <name>…|--all` forgets them. Trust is
recorded in `~/.local/state/amp-harness/trust.toml` (outside the repo), keyed by repo path, server
name and a sha256 of the server's table and its `env_file`. If any of those change, you are asked
again. Headless runs (`run -p` and every worker) never ask: they skip untrusted servers with a
warning. `--trust-repo` or `AMP_HARNESS_TRUST_REPO=1` (older names: `--trust-repo-mcp`,
`AMP_HARNESS_TRUST_REPO_MCP=1`) lets them all start for that process (CI), and that is never
recorded. Servers in your own config.toml are always trusted. The question shows the whole table
(`mcp_trust::details`): the full command line, cwd, env and env_file (with the names it sets), the
full URL (secret-looking literal query values redacted), headers, auth, and every env variable,
keychain item and OAuth login it reads; trusting records exactly the server shown (#2310).

The repo's agents (`.amp/harness/agents/*.md`), skills (`skills/<name>/`), `[roles.*]` tables and
`[models] profile` need trust the same way (#2310): until trusted as they are now, they are left
out (the built-in or your own is used), so a `git pull` can't swap the manager's `permission:`
block or add `extra_body` to your requests, not even through live reload. The chat asks once per
item; `amp-harness trust [agent:<id>|skill:<name>|role:<id>|setting:profile|mcp:<name>|--all]`
and `untrust` do it from the CLI; headless runs warn and leave them out unless `--trust-repo`.
What you write with `agents|skills|models … --repo` is trusted as written.

`setup --preset` shows every table it will write, every command it makes the machine run (stdio
MCP servers, `auth_command`) and a diff for each of your tables it would replace, then asks once.
Without a person it needs `--yes`, and `--force` too if it would replace one of your tables.

A repo server never reaches your own secrets (#2308), trusted or not, override or not:
- Its keychain items are `mcp:repo:<repo id>:<name>:…` (repo id = 16 hex of sha256 of the
  canonical repo path), and its OAuth login is stored under `mcp-repo:<repo id>:<name>|<url>`.
  Your own servers keep `mcp:<name>:…` and the URL key, unchanged.
- A repo table with the name of one of your servers is ignored with a warning; yours is used.
- Trust records the environment variables the server reads (`env = […]` in trust.toml), and the
  chat question names them. A record without them (from before) is asked again if it reads any.
- It may never read provider `key_env`s, `AMP_TOKEN`, `--token-env`, `[secrets] pass_to_bash` or
  a variable your own MCP servers read: it is stopped with a config error naming them.
- Secrets stored for a repo server before this change are reported missing. If they were for this
  repo's server, `amp-harness mcp secret NAME --adopt` copies them and its login.

Every `${env:VAR}`, in any field, is secret-bearing: hidden from bash and stdio servers, and its
value is scrubbed from the server's errors. MCP errors show URLs without userinfo and with query
values as `<redacted>`. A secret-looking `${env:..}` in `args` (visible in `ps`) is a warning in
your config and an error in a repo's: pass it through `env` instead.

### MCP servers: `amp-harness mcp` (#2243)

Any MCP server, stdio, Streamable HTTP or legacy HTTP+SSE, is added, changed and removed without
editing a file. The commands take the shapes Claude Code users know:

```sh
amp-harness mcp add fs -- npx -y @modelcontextprotocol/server-filesystem '${repo}'   # stdio
amp-harness mcp add github https://api.githubcopilot.com/mcp/ --bearer-env GITHUB_TOKEN
amp-harness mcp add docs --url https://docs.example.com/sse --transport sse -H 'X-Org: acme'
amp-harness mcp add search --secret-env BRAVE_API_KEY -- npx -y brave-search-mcp     # asked hidden
amp-harness mcp add db --scope repo -- ./tools/db-mcp          # repo file; trusted for you
amp-harness mcp add-json linear '{"type":"http","url":"https://mcp.linear.app/mcp"}'  # or - (stdin)
amp-harness mcp list [--health [--timeout S]] [--json]   # last known health; --health checks now
amp-harness mcp get NAME [--scope user|repo] [--no-health] [--json]
amp-harness mcp edit NAME [--roles 'worker-*,reviewer']   # $EDITOR, checked, reopened on an error
amp-harness mcp remove NAME [--scope user|repo] [--keep-secrets] [--keep-login]
amp-harness mcp enable|disable NAME             # in your config.toml, also for a repo server
amp-harness mcp test NAME [--json]              # status, timing, tools; stderr tail when it fails
amp-harness mcp login|logout NAME               # OAuth for http/sse servers, like the AMP login
amp-harness mcp secret NAME [WHICH…] [--missing] [--secret-stdin]   # store or rotate secrets
amp-harness mcp secret NAME --adopt             # repo server: copy secrets stored before #2308
```

- Flags: `--scope user|repo` (also `--user` / `--repo`; `project` = repo; default user),
  `--transport stdio|http|sse` (default: http for a URL, stdio for a command after `--`),
  `--header "Name: value"`, `--env KEY=VALUE`, `--secret-header NAME`, `--secret-env KEY`,
  `--bearer-env VAR`, `--auth auto|oauth|none`, `--client-id`, `--client-secret`,
  `--oauth-scope`, `--callback-port`, `--no-sse-fallback`, `--workdir`, `--env-file`,
  `--per-worktree`, `--roles`, `--startup`, `--startup-timeout`, `--timeout`, `--disabled`,
  `--force` (replace), `--no-test`, `--json`.
- Secrets never go in a file or on the command line. `--secret-header`/`--secret-env`/
  `--client-secret` are asked hidden (or read one per line with `--secret-stdin`) and kept in the
  OS keychain (`mcp:<name>:header:<name>`, `mcp:<name>:env:<VAR>`; a repo server's under
  `mcp:repo:<repo id>:<name>:…`). A `--header`/`--env`/argument
  value that looks secret is refused with these alternatives, and the value is never echoed.
  `${VAR}` (Claude Code's spelling) is stored as `${env:VAR}`, read when the server starts.
  `add-json` moves secret-looking literal values to the keychain. Without a keychain
  (`AMP_CREDENTIAL_STORE=file`), use `${env:VAR}` references or `--bearer-env`.
- Every write replaces only that server's table, keeps the rest of the file (comments included),
  writes `config.toml.bak` first, and must load before it is saved. `add` connects once afterwards
  (`--no-test` skips it); an OAuth server then says to run `mcp login NAME`.
- `--scope repo` writes `<repo>/.amp/harness/config.toml` and trusts the server for you as written,
  since you wrote it; others who clone the repo are still asked. `edit` re-trusts your new version
  and `remove` forgets the trust. `remove` also deletes the server's keychain items and its stored
  OAuth login when nothing else uses them.
- `setup --preset` asks (hidden) for the secrets the preset's MCP servers declare. Without a
  terminal or a keychain, the summary says to run `amp-harness mcp secret NAME`.

#### Importing from other tools: `amp-harness mcp import` (#2244)

Other tools' MCP files are never loaded on their own; `mcp import` copies their servers in, once,
when you ask:

```sh
amp-harness mcp import --from claude --dry-run     # the TOML it would write; secrets only by name
amp-harness mcp import --from claude --yes         # .mcp.json + ~/.claude.json (user and local)
amp-harness mcp import --from vscode --only perplexity --secret-stdin < key.txt
amp-harness mcp import --from ./some/mcp.json --scope repo --rename GitHub=gh
amp-harness mcp import                              # lists what it finds here and asks
```

| `--from` | Files read (highest precedence first) |
|---|---|
| `claude` | `~/.claude.json` `projects[<this project>]` (local), `<project>/.mcp.json`, `~/.claude.json` root (user); `$CLAUDE_CONFIG_DIR/.claude.json` first when set |
| `opencode` | `<project>/opencode.json[c]`, `$OPENCODE_CONFIG`, `~/.config/opencode/opencode.json[c]` |
| `vscode` | `<project>/.vscode/mcp.json`, the user profile's `mcp.json` (`~/Library/Application Support/Code/User/` on macOS, `~/.config/Code/User/` on Linux) |
| `cursor` | `<project>/.cursor/mcp.json`, `~/.cursor/mcp.json` |
| `claude-desktop` | `~/Library/Application Support/Claude/claude_desktop_config.json` (Linux: `~/.config/Claude/`) |
| a file | any of these formats, recognised by name and keys |

- Variables become ours: Claude Code `${VAR}`/`${VAR:-d}` -> `${env:VAR}`/`${env:VAR:-d}`; opencode
  `{env:VAR}` -> `${env:VAR:-}` (it uses an empty string when unset); VS Code and Cursor
  `${workspaceFolder}` -> `${repo}`, `${userHome}` -> `${home}`, `${env:X}` -> `${env:X:-}`. A
  variable with no equivalent (`${workspaceFolderBasename}`, `${config:…}`) makes that server
  fail with the reason. Claude Desktop values are literal.
- Secrets go to the OS keychain, never to TOML: literal header/env values that look secret and
  OAuth client secrets (Cursor `auth.CLIENT_SECRET`, opencode `oauth.clientSecret`) are moved there.
  VS Code `${input:id}` and opencode `{file:path}` in a header or env value are asked at import
  (hidden for password inputs, or one line each with `--secret-stdin`); the file is not read.
  With `AMP_CREDENTIAL_STORE=file` a server that needs the keychain is refused before anything
  is written.
- Mapping: opencode `local`/`remote` -> stdio/http, its `timeout` (time to list tools) ->
  `startup_timeout_secs`, `oauth: false` -> `auth = "none"`; Claude Code `timeout` (ms) ->
  `call_timeout_secs`; Claude Code `disabledMcpServers` and opencode `enabled: false` ->
  `enabled = false`. Dropped with a warning: `headersHelper`, VS Code `dev`/`sandbox`. `ws` servers
  and pickString/command inputs can't be imported.
- Names are made valid (`GitHub` -> `github`). A different server of the same name gets `-2`
  (`--force` replaces it, `--rename OLD=NEW` picks one); an identical one is skipped, so a repeat
  import changes nothing. Any server that can't be imported stops the import before anything is
  written, naming `--only` for the rest. Writing asks for confirmation (`--yes` without a
  terminal); `--scope repo` also trusts what it writes for you; `--test` connects afterwards.

A tool result longer than the server's `max_output_tokens` (default 25 000, or `[tools]
mcp_max_output_tokens`) is cut, and the whole text is saved under
`~/.local/state/amp-harness/mcp-output/<ticket or session>/<call>.txt` for the agent to read. Those
files are private (0600, in 0700 directories). Each chat and `run -p` prunes them when it starts
and every hour: files older than `[tools] mcp_output_keep_days` (default 7) go, then the oldest
until the rest fit in `[tools] mcp_output_max_mb` (default 512). `0` turns either limit off.

```toml
# <repo>/.amp/harness/config.toml
[models]
profile = "anthropic"

[roles.worker-backend]
model = "anthropic/claude-sonnet-4-6"
thinking = "high"
```

## Commands (M1-4)

Every command below takes the connection flags `--url`, `--ca-file`, `--protocol` and
`--token-env` (precedence above), plus the global `--config` / `--config-dir`, and connects with D3's order: `AMP_TOKEN`, else the stored login,
else no credentials.

```sh
amp-harness doctor [--json] [--offline]  # config, credentials, connect, tools, contract, whoami, project,
                                        # roles, fallback, keys, models, mcp, skills, agents, config-check
amp-harness whoami [--json]
amp-harness tickets list [--project N] [--owner mine|unassigned|any|EMAIL] [--state S[,S]] [--json]
amp-harness tickets get ID [--comments] [--json]
amp-harness mcp call TOOL ['{"json": "args"}' | -] [--raw] [--timeout SECS] [--server NAME]
amp-harness mcp tools [--json] [--server NAME]     # AMP's tools, or a configured MCP server's
```

- `.amp.json` (nearest one upwards from the working directory) only supplies the default
  `project_id`; its `amp_api` field is ignored (D4). `AMP_HARNESS_CEILING_DIR` stops the search.
- Ticket states use the web UI's words: `question`, `in_progress`, `ready`, `scheduled`, `waiting`,
  `done` (labels Question, In progress, Ready, Scheduled, Waiting, Done). `--state` also takes
  `blocked` (waiting + question + scheduled), `open` (the default: all but done) and `all`.
- `doctor` exits 1 when a check fails (warnings don't count) and lists the D11 contract tools the
  server is missing. It describes auth by source only and never prints a token.
- Since #2246 `doctor` also checks the configuration: `mcp` and one `mcp:<server>` per MCP server
  (connected the way a session starts it: health and tool count, trust for repo servers, how it
  authenticates with secrets by their source and state only, and which roles may use it through
  the server's `roles` filter and each role's `mcp` permission; a table with errors fails, a server
  that is untrusted, needs a login or does not start warns with the fix; `--offline` connects
  nothing), `skills` and `agents` (they load, every agent resolves a model, which ones the manager
  can dispatch, yours listed with their model), `fallback` (the model a role with no
  `[roles.<id>]` runs on) and `config-check` (the rest of `amp-harness config check`). `roles` lists
  every agent with no model of its own as `(no model of its own: fallback worker-backend)`, and
  `config show`, `models list`, `/config` and `/model roles` show the same.
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

### Your company's own endpoint, models per role, org presets (#2275)

Say your company serves its own models on `api.example.com` (OpenAI-compatible), called
`engineer` and `orchestrator`. None of the three ways below needs OpenRouter, contacts it, or
asks you to edit a file.

**1. First run** (`amp-harness` in a new terminal, or `amp-harness setup`): when no provider has
a key, setup asks which one to use:

```text
Which model provider do the agents use? Keys are kept in your OS keychain, never in a file.
  1) OpenRouter — the opencode roster on OpenRouter (MiMo, DeepSeek, GLM, Kimi)
  2) Anthropic API key (console.anthropic.com) — …
  3) OpenAI API key (platform.openai.com) — pick the model each role runs on
  4) Your own OpenAI-compatible endpoint (e.g. api.example.com: …) — your models, mapped to the roles
Provider [1]: 4
Name for it (shown in the harness) [My models]: Example AI
Short id, used in model names like <id>/<model> (lowercase, digits, - and _) [example-ai]: example
Endpoint address (e.g. api.example.com or https://api.example.com/v1): api.example.com
Use https://api.example.com/v1? [Y/n]
Extra header the endpoint needs (name, Enter for none):
Example AI API key, Enter if it needs none (hidden):
  Example AI accepted the key.
It serves 2 models: engineer, orchestrator.
Which model each role runs on (picked by name; you can change them):
  manager    example/orchestrator   plans the work and dispatches it (the orchestrator) (manager)
  workers    example/engineer       write the code, docs and designs (worker-backend, …, designer)
  reviewers  example/orchestrator   review the work (reviewer, github-reviewer)
  helpers    example/engineer       small, fast jobs: research, media, KB upkeep (researcher, …)
Use these? [Y/n]
```

- The address: a bare host gets `https://` and `/v1` (after a confirmation); plain `http://` is
  accepted without a question only for localhost.
- The key: read hidden, checked with `GET /models` and kept in the OS keychain
  (`provider:example`). Enter means the endpoint takes none. A key already in `EXAMPLE_API_KEY`
  (`<ID>_API_KEY`) is used as is.
- The models: what the endpoint lists (or typed, when it lists none). The defaults match on
  names: `orchestrator`/`planner`/`lead` → manager; `engineer`/`coder` → workers; `review` →
  reviewers (else the manager's model); `mini`/`fast`/`flash` → helpers (else the workers' model).
- What gets written: `[providers.example]`, plus `[models] profile = "inherit"` with `inherit =
  "example/engineer"` (every other role, and agents added later), plus `[roles.<role>] model` for
  the roles on another model.
- Without a keychain the menu still shows. A keyless endpoint, or a key in `EXAMPLE_API_KEY`,
  works there; otherwise setup says to export the variable.

**2. `connect` with `--role`** (scripts, or after the first run):

```sh
amp-harness connect example --base-url api.example.com --name "Example AI" \
  --role manager=orchestrator --role worker=engineer   # key hidden, from --key-stdin or EXAMPLE_API_KEY
amp-harness connect example --role reviewers=engineer  # it has its key: only the reviewers change
```

The first command gives the same mapping as the first run: the reviewers follow the manager
(`orchestrator`) and the helpers follow the workers (`engineer`). One function,
`role_map::fill_groups`, does this for the first run, `connect --role`, `/connect` and presets.

- ROLE is a group (`manager`, `workers`/`worker`, `reviewers`, `helpers`/`helper`), `all`, or a
  role name (`kb-curator`, `amp-worker-docs`). MODEL is one of the provider's models (or
  `provider/model`). Both are checked before anything is stored.
- Groups you don't name follow the groups you do: the reviewers follow `manager=`, the helpers
  follow `workers=` (unless a model's name points at them: `review`, `mini`/`fast`/`flash`). Any
  other role you don't name keeps its model if it is already on this provider, else moves to its
  group's model (name match or follower). So every role ends up on the provider. `all` names
  every role.
- Running `connect <id>` again on a provider you added, with endpoint flags (`--ca-file`,
  `--header`, `--auth`, `--organization`, `--model`, ...), merges into its `[providers.<id>]`
  table. Only the given settings change, a new `--auth` drops the old auth's own keys, and its
  models (including `models set --no-check` ones), timeouts and comments stay. It prints what
  changed (`--json`: `changed`).
- In the chat: `/connect example manager=orchestrator worker=engineer` (`--role` is optional). It
  asks for no key when the provider already has one.

**3. An org preset**: an admin ships one TOML file with **no secrets**
([examples/enterprise-preset.toml](examples/enterprise-preset.toml)):

```sh
amp-harness setup --preset enterprise-preset.toml
amp-harness setup --preset https://intranet.example.com/amp/enterprise-preset.toml
```

```toml
[preset]
name = "Example Corp"
[server]
url = "https://amp.example.com"
[providers.example]
name = "Example AI"
base_url = "https://api.example.com/v1"
models = ["engineer", "orchestrator"]   # or [providers.example.models.<id>] tables with settings
[roles]
default = "example/engineer"
manager = "example/orchestrator"
reviewers = "example/orchestrator"
# [mcp.<name>]: optional, the config.toml schema
```

- Setup writes the preset into config.toml: the providers, the roles and the MCP servers. It
  tries the server, then asks each person only for their own key(s), any `secret_headers` values
  and their AMP login (plus the usual project link).
- `--yes` asks nothing and takes keys from the environment.
- The preset is refused, naming the key, when:
  - it has an unknown key;
  - anything looks like a secret (`api_key = …`, a `Bearer …` header, a token-shaped value);
  - a URL is not https (http only for localhost).
- A preset URL is fetched only over https (or http to localhost), and only up to 256 KiB.

**`amp-harness doctor`** shows the models line, each provider in use with its endpoint and key
source, and every role's `provider/model`:

```text
  models:   inherit example/engineer
  provider: example (Example AI) https://api.example.com/v1 (key: keychain)
  roles:    designer           example/engineer
            manager            example/orchestrator
            …
```

`models:example` checks that the endpoint answers `GET /models` with the key and lists every
model a role uses. `doctor --json` has the same data under `.models.roles` (role, provider,
model, endpoint, source).

### How a provider endpoint authenticates: keys, tokens, Azure, TLS and proxies (#2277)

Every `[providers.<id>]` takes `auth` and the settings that go with it. Secrets are never in
config.toml. The API key or client secret is the provider's key: `key_env`, else the OS keychain
(`provider:<id>`), as `connect` stores it. Tokens live only in memory.

| `auth` | What is sent | Settings | Secret |
|---|---|---|---|
| `bearer` (default; `key` is the same) | `Authorization: Bearer <key>` (Anthropic Messages: `x-api-key`) | | the API key |
| `header` | the key in its own header | `auth_header` (default `api-key`, Azure OpenAI's) | the API key |
| `none` | nothing | | |
| `command` | `Authorization: Bearer <token>` | `auth_command = ["prog", "arg"]`, `auth_command_timeout_secs` (30) | none (the tool logs in) |
| `oauth-client-credentials` | a client-credentials token | `token_url`, `client_id`, `scopes` | the client secret |
| `azure-entra` | a Microsoft Entra ID token for `https://cognitiveservices.azure.com/.default` | `tenant_id` + `client_id` (client credentials; `authority_host` for sovereign clouds), or neither (runs `az account get-access-token`), or your own `auth_command` | the client secret, or none with `az` |

- **Tokens.** `auth_command` prints a token, or JSON with `token` / `access_token` / `accessToken`
  and `expires_in` (seconds) or `expires_on` (Unix time; the shape `az` prints).
  - A token is cached in memory and fetched again shortly before it expires (60 s early, or a
    tenth of its lifetime if that is shorter).
  - It is also fetched again once if the endpoint answers 401; the request is then retried once.
  - A token with no expiry is kept until a 401.
  - A command that runs longer than `auth_command_timeout_secs` is killed.
  - A command's stderr is never logged. Only its last line goes into the error, with anything
    token-shaped removed.
  - This covers corporate SSO helpers, Vault, `az account get-access-token` and
    `gcloud auth print-access-token`.
- **OpenAI.** `organization` and `project` send `OpenAI-Organization` and `OpenAI-Project`.
- **Query parameters.** `query = { "api-version" = "2024-10-21" }` adds parameters to every
  request, the catalog call included.
- **Azure OpenAI.**
  - The v1 API (`https://<resource>.openai.azure.com/openai/v1`) needs no api-version, and the
    deployment name is the model id.
  - Classic deployments: `base_url = "https://<resource>.openai.azure.com/openai/deployments/{model}"`
    with `query = { "api-version" = "2024-10-21" }`. `{model}` is replaced by the model id, so
    again the deployment name is the model id.
  - Use `auth = "header"` (the `api-key` header) for key auth, or `auth = "azure-entra"`.
- **TLS.**
  - `ca_file` is a PEM bundle added to the OS trust store (macOS Keychain, Linux `/etc/ssl`), which
    is always used.
  - `client_cert` + `client_key` enable mTLS.
  - Without `client_key`, the key is read from the cert file, else from the keychain
    (`provider:<id>:client-key`; `connect --store-client-key` puts it there).
- **Proxies.**
  - `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` and `NO_PROXY` apply by default.
  - `proxy = "http://proxy:3128"` (honouring `NO_PROXY`) or `proxy = "direct"` overrides them for
    one provider.
  - A proxy password is refused in config: put that URL in `HTTPS_PROXY`.

```toml
[providers.azure]
name = "Azure OpenAI"
api = "openai-completions"
base_url = "https://contoso.openai.azure.com/openai/deployments/{model}"
query = { "api-version" = "2024-10-21" }
auth = "azure-entra"            # with `az login`; or tenant_id + client_id for a service principal
[providers.azure.models."gpt-4o"]  # the deployment name

[providers.gateway]
api = "openai-completions"
base_url = "https://llm.corp.example/v1"
auth = "command"
auth_command = ["corp-sso", "token", "--audience", "llm"]
ca_file = "/etc/ssl/corp-root.pem"
client_cert = "~/.config/corp/me.pem"
client_key = "~/.config/corp/me.key"
```

- **connect:**
  - Flags: `--auth TYPE`, `--auth-header NAME`, `--auth-command "prog args"`, `--token-url`,
    `--client-id`, `--scope` (repeatable), `--tenant-id`, `--organization`, `--project`,
    `--query NAME=VALUE` (repeatable), `--ca-file`, `--client-cert`, `--client-key`,
    `--store-client-key` and `--proxy`.
  - A client secret is read like a key: hidden, or `--key-stdin`.
  - `connect <id>` on a token provider stores nothing and checks that a token can be had and is
    accepted.
- **First run:** the own-endpoint path asks "How does your endpoint authenticate?".
  - It asks only the questions the chosen option needs: the header name; the command; the token
    URL, client id and scopes; or the Entra tenant and client id.
  - A CA bundle or client certificate is asked for only when the endpoint fails TLS without one.
    A proxy is asked for only when the endpoint can't be reached.
  - An Azure resource address becomes `…/openai/v1`. A deployment address is asked for its
    api-version. OpenAI is asked for the optional organization and project.
- **Org presets** take the same settings. A preset holding key material (PEM, tokens) is refused.
- **doctor:**
  - Shows each provider's auth (`auth: key in the api-key header`,
    ``auth: Microsoft Entra ID, token from `az`, token valid until …``) and any CA file, client
    certificate and proxy. It never shows a secret.
  - The `models:<id>` check calls the catalog through the provider's own TLS, proxy, query and
    auth. A failure names the fix:
    - 401/403: which credential was refused, and how to replace it;
    - unknown CA: `ca_file`;
    - client certificate required: `--client-cert` / `--client-key`;
    - a token tool missing or failing: install it, or run it yourself;
    - Entra: `az login`, or tenant/client id.

- **Redirects:** a provider request (and a token request) follows a redirect only to the same
  origin (scheme, host and port) as its base_url or token_url. A redirect to any other origin is
  refused with "refused a redirect to another origin (<origin>)", so `api-key`, `x-api-key`,
  secret headers and client secrets are never sent to another host. Set `base_url` to the address
  the endpoint redirects to. MCP servers already follow only same-origin 307/308 redirects.

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
  `opencode` (the shipped `[roles.*]`: the manager on OpenRouter's `openai/gpt-6.1-sol` at low
  effort, researchers and media on `xiaomi/mimo-v2.6-pro`, workers on
  `deepseek/deepseek-v4.1-flash-1017`, reviewers on `z-ai/glm-5.3`, the designer on
  `moonshotai/kimi-k3`), `anthropic` (Opus for the manager and reviewers, Sonnet for
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
amp-harness config show [--json] [--check]
```

It prints each provider (api, URL, where its key comes from, never the key, and its timeouts) and
each role's provider/model, thinking level, readiness and the layer that set it. `--check` asks each
provider in use for its live model list (OpenRouter's is public; Anthropic's needs the key) and
flags ids that are missing. It exits 1 if a role does not resolve or a checked id is missing.

`config show` reads the same files a session in this folder would: the user config.toml
(`--config` / `--config-dir` apply) and the repo's `.amp/harness/config.toml`.

### Changing models without editing files (#2235)

```sh
amp-harness models list [--available] [--check] [--json]
amp-harness models set reviewer anthropic/claude-opus-5-5 [--thinking high] [--user|--repo] [--no-check]
amp-harness models reset reviewer [--user|--repo]
amp-harness models profile anthropic            # or opencode, a [profiles.X] of yours, or:
amp-harness models profile inherit --inherit openrouter/z-ai/glm-5.3 [--thinking high]
amp-harness config check [--json] [--live]
```

`--user` (the default) edits your config.toml; `--repo` edits the repo's `.amp/harness/config.toml`,
which holds only `[roles.*]` and `[models] profile`. Edits keep your comments and other settings,
copy the old file to `config.toml.bak`, and are written only if the result loads and the role
resolves. `models set` refuses an unknown provider; a model your config doesn't define yet is looked
up in the provider's live catalog (when it has a key) and added to your user file as an empty
`[providers.<p>.models."<id>"]` table, and a model the catalog doesn't list is refused unless
`--no-check`. `config check` reports every problem in every layer (files, server, models,
providers, roles, workers, tools, tui, mcp; `--live` adds each role's model against the live catalog)
with the file, the key and a fix, and exits 1 on errors (a missing key is a warning).

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

A model stuck on one tool call is stopped, for every role (#2225). The harness counts identical
calls (same tool, same arguments) in a row since the user's last message; any other call resets
the count. The 3rd and 4th identical call still run, with a line in the result telling the model
it is repeating itself. The 5th is not run and ends the run. `run -p` then exits 1 with outcome
`stopped` and one stderr line ("The model kept repeating `bash ls` and was stopped; ..."); the chat
shows the same sentence in the transcript, and a new message starts the count again.

### Manager eval (#2363)

`scripts/manager-eval.py` runs the manager headless through four scripted scenarios against a
compose AMP and measures how it behaves, so a manager model, prompt or policy change can be
compared before and after:

| scenario | setup | passes when |
|---|---|---|
| `init` | an empty git folder, no commits, no `.amp.json`: "set up a new project here, it's a Go API" | the project exists, `.amp.json` is written here, `architecture/project-overview.md` is in its KB |
| `plan` | a repo with no commits linked to an empty project (plan approval on): the user's real "plan out this project" prompt | epics, stories, 4+ tickets with dependencies, `amp_check_plan`, no dispatch, the approve link in the answer |
| `status` | a seeded board: "What's on the board? Where are we?" | only read tools |
| `run` | a seeded project with plan approval off and one small docs ticket: "Run the ready tickets." | `amp_run_ticket` ran it and the ticket is done |

Every scenario also fails on a schema error, a policy denial or a run that doesn't complete. Per
run it reports wall time, time to the first tool call, silent time (a model turn with no text or
tool call yet: what a person sees as "Thinking"), turns, tool calls and errors, tokens, the
harness's cost and the cost at OpenRouter's live list price.

```sh
# in a subshell, so the key never lands in your shell history or the output
( source ~/.config/amp-harness/dev-keys.env
  python3 -I scripts/manager-eval.py --models default,sonnet --scenarios all )
python3 -I scripts/manager-eval.py --report target/manager-eval/<dir> ...   # one table, no runs
```

- `--models`: presets (`default`, `mimo-high`, `mimo-off`, `glm-low`, `gpt-sol`, `sonnet`,
  `haiku`, `gemini-flash`) or `label=provider/model@thinking`. `sonnet` and `haiku` go through
  OpenRouter's Anthropic Messages endpoint (provider `orm`), which caches the prompt; OpenRouter's
  chat endpoint doesn't cache Claude.
- Each run gets its own HOME, git repo and `QA 2363 …` project under `target/manager-eval/`; the
  project is archived afterwards. Plan approval is turned off over REST (`--rest`, default
  `http://localhost:3001`) only for the `run` scenario's project. The eval never approves a plan.
- `--budget` (USD, default 3) stops starting runs once the harness's cost passes it.
- Runs of different models can go in parallel (one process per model); each run only touches its
  own project.

Results for #2363 are in the project KB (`discoveries/amp-harness-manager-eval.md`).

### Prompt size (#2367)

A role's system prompt and tool schemas go with every request, so they set most of a session's
cost. `amp-harness run --print-system-prompt` and `--print-tools` print them without calling a
model; `scripts/prompt-size.py` breaks them down (role prompt, server instructions, AMP context,
preloaded skills, skill list, built-in and AMP tools, largest tools first):

```sh
( source ~/.config/amp-harness/dev-keys.env
  python3 -I scripts/prompt-size.py --cwd /path/to/repo [--role amp-manager] [--json] )
```

#2367 took the manager from ~38k to ~22k prompt tokens (first request, measured) and the four
eval scenarios from $0.56 to $0.40 at list price, all still passing: no preloaded skills (it reads
amp-planning when it plans), no AMP tools a manager never uses (the `amp_*: deny` lines in
`harness_overrides.yaml`), no role-prompt paragraphs the server instructions already give, AMP
descriptions without their "Use for:" keyword line, and compact built-in schemas.

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
amp-harness [--role ROLE] [--cwd DIR] [--no-session] [--theme auto|dark|light|FILE] [--url URL] [--ca-file PEM] [--protocol P] [--config FILE] [--config-dir DIR] [--token-env VAR]
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
  servers). `/mcp`, `/agents`, `/skills`, `/model roles` and `/config` are panels over the same
  settings as the CLI (health, details, enable/disable, trust, per-role models, every setting with
  its source). `/reload` picks up agents, skills, models and MCP servers changed from another
  terminal (see below). AMP: `/ticket`, `/project`, `/questions`, `/plan`, `/open`, `/workers`,
  `/worker`, `/amp`. `/usage` prints the session's tokens and cost, `/help`, `/abort`, `/close`,
  `/quit` (also `/exit`, `/q`).
- Keys: Enter sends, Shift+Enter is a new line, Esc aborts the running reply. A new line also comes
  from Ctrl+J (every terminal), Alt+Enter (`ESC CR`: Option as Esc+/Meta on a Mac) and `\` then
  Enter (#2349). Without the kitty keyboard protocol most terminals send Shift+Enter as a plain CR,
  so it sends; a local Terminal.app is the exception (rpi-tui reads the Shift key from macOS), and
  xterm's modifyOtherKeys `CSI 27;2;13~` is decoded by the patched crossterm in `vendor/crossterm`.
  There the empty editor's hint reads `/ for commands · Ctrl+J new line` and `/help` says Shift+Enter
  sends (`amp_tui::newline`). Ctrl-C aborts a run,
  else clears the editor, else quits; Ctrl-D on an empty editor quits. Ctrl+T opens (or closes
  again) the lowest thinking section on screen, else the last one (#2345); Ctrl+O shows (or hides) every tool call's arguments and result.
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
- The `/remote` pairing link (#2423) is copied as it is shown: OSC 52 to the terminal plus
  `pbcopy` (macOS), `wl-copy` (Wayland) or `xclip`/`xsel` (X11) when found in `PATH`
  (`amp_tui::clipboard`). `/remote copy` copies it again, `/remote open` opens it with `open` /
  `xdg-open`. Its rows start at the window's left edge (`inset::FLUSH_LEFT`: no padding), so a
  modifier-drag from its first character to its last is the link plus line breaks. Only the
  terminal loop turns the real clipboard on (`Shell::attach_terminal`); tests use
  `Desktop::with_path` and recording stand-ins.
- Tool calls (#2183) are one line each: `✓ list_tasks · 3 tickets: 1 ready, 1 waiting · 12 ms`.
  AMP calls get a sentence for what they did (the contract tools, `kb_search` and `create_tasks`;
  any other falls back to the result's `key: value` list), durations under a second are in ms,
  and consecutive calls share one block (more than five fold their earliest successful ones into
  `✓ 4 calls · get_task ×3, list_tasks`). Only running and failed calls keep the tinted surface,
  and a failed call shows its error under it.
- Thinking (#2177) is collapsed by default: one dim line per section, `▸ Thinking · 18s`, which
  shows the model's latest sentence while it thinks; when the answer starts its timer stops and it
  reads `▸ Thought for 18s` (#2345). The transcript's last one adds `· Ctrl+T or click to open`
  (`Ctrl+T to open` with `[tui] mouse = false`). Expanded (`▾`), it is plain dim text behind a `│`
  gutter, with no markdown styling, so it never reads like the answer. A click on a section's line
  opens or closes that one; Ctrl+T the lowest one on screen (scroll up to an older one and press
  it), else the last. Opening one while following the stream scrolls to its header if its text
  would push the header off the top. `/config thinking [collapsed|full|hidden]` (or `t` in the
  `/config` panel) changes the mode for the chat, sections already shown included; without a
  value it goes to the next. The session file always keeps the full thinking.
- AMP panels (`/amp`, `/ticket`, `/workers`, `/questions`, `/plan`, `/project`) open docked above the
  editor with the keys: with an empty editor, ↑↓ select a row and Enter acts on it (answers a
  question, otherwise opens it in the browser). Typing, a paste or a sent prompt hands the keys back
  to the editor and the panel stays docked (#2161); printable keys reach the editor, except the
  `/workers` panel's `x` (stop) and `o` (open in AMP) and a configuration panel's letters. Tab, with
  an empty editor, switches between the panel and the editor. Esc closes it.
- The terminal is restored on exit, on a signal and on a panic. Quitting mid-run aborts the run
  (and any workers it started) and waits briefly so the session file is complete.

### Changes from another terminal: `/reload` (#2242)

`amp-harness agents|skills|models|mcp …` (or an edit to config.toml, `agents/`, `skills/`, in your
config dir or the repo's `.amp/harness/`) reaches a running chat without a restart. `/reload` does it
at once. A watcher does it by itself a couple of seconds after the files settle (it checks sizes and
modification times every 2 s; `AMP_HARNESS_AUTO_RELOAD=0` turns it off). The transcript lists what
changed (`agents: +sql-expert`, `model amp-manager: … → …`, `mcp: +github ~fs -old`).

| Change | When it takes effect |
|---|---|
| New, edited or removed agent (the roster) | `amp_run_ticket` lists it, and the manager may dispatch it, from its next reply. Workers dispatched from then on are built from the new file. |
| Skills | Listed (and preloaded) from the manager's next reply and in every new worker. |
| A role's model or thinking (`[roles]`, `[models]`, `[providers]`, keys) | Workers dispatched from then on. If the chat's own role changed, the chat switches to it from the next reply. `/model` still picks one for this chat only. |
| The manager's permissions | From its next tool call. |
| `[mcp.<name>]` | Only servers whose table changed restart. New ones start. Removed or disabled ones leave the tool list at once and stop once no running worker still holds their tools. A server that was stopped or needed a login is tried again, so `mcp login` in another terminal followed by `/reload` brings it back. A broken table is named and its server left out. |
| `AGENTS.md` | Re-read for the manager's next reply. |

Workers already running keep what they were built with (prompt, tools, model) until they finish.
`[workers]`, `[tools]` and `[secrets]` are read when the chat starts. A broken agent file or an
unparsable config.toml changes nothing, and the error says what to fix. A secret variable a new
provider or MCP server reads is hidden from bash from then on.

### Theme (M3-1)

The chat uses the AMP theme: `amp-dark` or `amp-light` (provisional simstech tokens v1, #1815),
with square borders and AMP's ticket-state, epic and brand colours.

| setting  | flag      | env                         | `config.toml` `[tui]` | default     |
|----------|-----------|-----------------------------|-----------------------|-------------|
| theme    | `--theme` | `AMP_HARNESS_THEME`         | `theme`               | `auto`      |
| colours  | -         | `AMP_HARNESS_COLOR`         | `color`               | `auto`      |
| thinking | -         | `AMP_HARNESS_SHOW_THINKING` | `show_thinking`       | `collapsed` |
| mouse    | -         | `AMP_HARNESS_MOUSE`         | `mouse`               | `true`      |
| padding  | -         | `AMP_HARNESS_PADDING`       | `padding`             | `2`         |

- `theme`: `auto` asks the terminal for its background (OSC 11) and picks light on a light
  background, else dark. Use `dark` / `light` to skip the query, or give a path to your own theme
  file (same JSON format as `crates/amp-tui/themes/amp-dark.json`).
- `color`: `auto` uses 24-bit colour when `COLORTERM` is `truecolor`/`24bit`, else the 256-colour
  fallbacks. `truecolor` or `256` forces one.
- `show_thinking`: `collapsed` (one `▸ Thought for Ns` line per section; a click or Ctrl+T opens
  one), `full` (every section expanded as plain dim text) or `hidden` (none shown; Ctrl+T still
  shows the last one). `/config thinking` changes it for one chat.
- `mouse`: `true` (the wheel scrolls the transcript; select with Fn-drag in Terminal.app,
  Option-drag in iTerm2) or `false` (no mouse reporting: plain click-drag selects). The env var
  takes `on`/`off`.
- `padding`: columns of space left and right of every part of the chat (header, transcript,
  panels, editor, status line, footer), 0 to 8. At 80 columns or fewer it is halved, rounded up
  (the default 2 becomes 1), and it shrinks further so the content keeps at least 40 columns
  (none at 41 columns or fewer). A blank row sits above the header and below the footer too,
  except on terminals shorter than 24 rows.

The message box shows the terminal's cursor as a blinking bar at the insertion point whenever it
has the keys: on start, after a panel closes, and after a click into it (with mouse reporting on,
a click also moves the insertion point). It is hidden while a panel has the keys (a panel open
with an empty message box, an answer waiting for `y`). The terminal's own cursor shape comes back
on exit.

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

**Per-ticket approval (#2198, #2201).** On servers that approve each ticket (its owner approves it;
a project lead may approve it on their behalf), `amp_check_plan` says whose each awaiting ticket
is. The status line then says `plan: N of yours waiting for your approval`, and others' tickets get
a quieter `M others' waiting for their owners` (dropped first on a narrow line). `/plan` shows an
`approval` row for yours and an `others'` row for theirs, both linking the Flight plan page. A
dispatch refusal names whose approval it waits for:

```text
  ⏸ Waiting for your approval: #2203 is yours
  ⏸ Waiting for its owner's approval: #2204 is someone else's (a project lead may approve it on their behalf)
```

(`⏸ Waiting for #N's owner to approve it …` when the ticket isn't in the last plan check). Older
servers, without `awaiting_approval_tickets`, keep the words above: one approval covers the plan.

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
a built-in table) names the skills preloaded into its prompt in full. Workers preload
`amp-execution`, reviewers `amp-review` plus their review skill. The manager preloads nothing
(#2367): its prompt says to read `amp-planning` when it plans. Every other skill is listed with its
path, for the agent to read when it needs it.

You can override or add agents and skills from disk. Later layers win:

1. embedded: `v2/harness/assets/{agents,skills}`;
2. user: `~/.config/amp-harness/{agents,skills}/` (`$XDG_CONFIG_HOME`, `--config-dir` and
   `AMP_HARNESS_CONFIG_DIR` honoured);
3. repo: `<dir with .amp.json>/.amp/harness/{agents,skills}/`.

An agent override (`agents/amp-manager.md`) replaces the whole agent. Skills override per file:
`skills/amp-kb/SKILL.md` replaces only that file, and the skill keeps its other embedded files. Files
that don't exist in the embedded set add new agents or skills. Names are lowercase letters, digits
and hyphens. A malformed override is an error naming the file, never skipped silently.

`amp-harness agents list|show|new|edit|remove|reset|validate` and `agents skills <id> add|remove
<skill>`, and `amp-harness skills list|show|new|import|edit|remove|reset|validate`, do this without
editing files by hand (#2234). Writers take `--user` (default) or `--repo` and validate before they
write; `edit` opens `$VISUAL`/`$EDITOR` and can reopen an invalid edit. `agents new <id> --from
worker|reviewer|researcher|blank|<agent id>` writes `dispatch: true` (the manager may run it whatever
its id; a manager `task` rule that names it with `deny` still wins) and `based_on: <role>`. A new
agent needs no `[roles]` entry: its model is `[roles.<id without amp->]` (repo, then user), else the
frontmatter `model` if that provider/model is configured, else the `based_on` role's model, else
`[models] fallback` (a role name or `provider/model`; default `worker-backend`). `agents new
--model p/m [--thinking L]` writes `[roles.<id>]` to the chosen layer's config.toml.

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
- A worker that ends without closing its ticket (error, timeout, cancel, turn budget, stopped for
  repeating one tool call, or it just stopped) is reported with `amp_report_run`, outcome `abandoned` and a note
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

Smoke v4 (M4-5) follows human-only plan approval. Each leg creates its three dispatchable tickets up
front, checks that dispatching before approval is refused with the plan link, and approves the plan
once as the person: on compose with `POST /api/projects/P/approve-plan` on the REST listener
(`AMP_SMOKE_REST_URL`; auth off, so it counts as the web UI), sending `as_of` = the newest
`planned_at` of the open tickets. The cluster leg tries `amp_approve_plan`, which only servers from
before human-only approval accept, and otherwise fails with the Flight plan link, or waits
`AMP_SMOKE_APPROVE_WAIT` seconds for a person to approve there. Ticket one is dispatched before it is
completed (completing it undispatched must be refused as "never dispatched"). On compose a person's
ticket must be refused over MCP and is marked done with `POST /api/tasks/T/complete`. Three more legs
run against the compose server:

- `config` (#2246): MCP servers, agents, skills and models set up through the CLI in a HOME under
  `target/`, then used. The smoke MCP server (`examples/mcp_smoke_server`, built by `make build`)
  runs as a stdio server, a Streamable HTTP server behind a bearer token from an environment
  variable, and a legacy HTTP+SSE server; each is added with `mcp add` (its test must list the
  tools), and a Claude Code `.mcp.json` is brought in with `mcp import`. `agents new smoke-helper
  --from blank` and `skills new` follow. `doctor` must show all four servers ready with their auth
  and roles, the agent and skill as yours, and the agent on worker-backend's model through the
  fallback (and, without the bearer variable, a warning naming it); the chat's `/model roles` must
  say the agent falls back; `models set` then gives it a model of its own. Last, the manager (faux)
  calls the imported server's tool and dispatches smoke-helper, whose worker calls the stdio, http
  and sse servers' tools and completes its ticket.

- `firstrun`: a brand-new HOME under `target/`. The chat's first run with scripted answers
  (`AMP_HARNESS_PROMPTS=stdin`: the server as `host:port`, `n`, a name, a code), started in a
  subfolder, must save the server and web UI to config.toml, create the project, write `.amp.json`
  at the git root and prefill the editor; a second start asks nothing. Then
  `init --yes --url … --project N --json` in another new HOME links a second folder, and
  `doctor --json` there, with no environment, must be ok. The project is archived on exit. A made-up
  provider key comes from the environment and must not appear in any file or output.
- `install`: packs the binary under test with `scripts/package.sh --bin` (or installs
  `AMP_SMOKE_ARCHIVE`, e.g. a `make dist` archive), installs it with `install.sh --archive` into a
  prefix under `target/` (HOME and the login shell under `target/` too, so `~/.local/bin` and your
  dotfiles are never touched), installs again over itself, then runs the installed binary's
  `--version` and `doctor --json` against compose.

| env | default | |
|---|---|---|
| `AMP_MCP_URL` | `http://localhost:8000/mcp` | compose leg (`cd v2 && make up`; auth off, expects `local-dev@amp.local`) |
| `AMP_SMOKE_PROTOCOLS` | `auto 2025-11-25` | one run per mode. `auto` must land on the stateless 2026-07-28 era |
| `AMP_SMOKE_LEGS` | `compose config firstrun install cluster` | add `keychain` (macOS) for the keychain-only leg: no key in the environment, real login keychain under a throwaway service, dialogs off, each item read at most once per process |
| `AMP_TOKEN` + `AMP_CA_FILE` | unset, so the cluster leg is skipped | cluster leg: an `amp_` token from the UI and `amp-local-ca.crt`. The token is only sent to this leg |
| `AMP_SMOKE_CLUSTER_URL` | `https://amp.local/mcp` | envoy on 443, not `:8443` (RFC 9728 resource match) |
| `AMP_SMOKE_REST_URL` | `http://localhost:3001` | compose's REST listener: plan approval and marking a person's ticket done, as the person |
| `AMP_SMOKE_APPROVE_WAIT` | `0` | cluster leg: seconds to wait for a person to approve the plan at the printed link when the server refuses agents (0 = fail at once) |
| `AMP_SMOKE_MCP_SERVER` | `target/debug/examples/mcp_smoke_server` | config leg: the MCP test server |
| `AMP_SMOKE_ARCHIVE` | unset | install leg: a release archive (with its `.sha256`) to install instead of packing the binary under test |
| `AMP_SMOKE_WEB_URL` | `http://localhost:5173` | compose leg's web UI, passed as `AMP_WEB_URL`; `/open` must print it. The cluster leg uses the MCP origin. An unreachable UI is only a WARN |

On the dev Mac, the docker-desktop cluster's LoadBalancer can hold host :8000. In that case, publish
compose's amp-api on another port and pass e.g. `AMP_MCP_URL=http://localhost:18000/mcp`. Don't run
`make up`: it kills whatever holds :8000, which there is Docker Desktop itself.

CI: the `harness` job in `.github/workflows/harness.yml` runs fmt, clippy `-D warnings` and the tests
on any push or PR that touches `v2/harness/**`. `.github/workflows/harness-release.yml`'s
`install-test` job runs `scripts/test-install.sh` on ubuntu-latest and macos-latest
(`make install-test`): a stub binary in a file:// release mirror, HOME under `target/`, covering
fresh install, upgrade, `--uninstall`, `--preset` passthrough, the no-terminal output and the setup
handoff on a pseudo-terminal (`scripts/pty-run.py`). On Linux it also runs
`scripts/test-install-distros.sh` (`make install-test-distros`, needs docker): Debian slim with no
curl or wget, Ubuntu 24.04 with wget only, Ubuntu 20.04 (glibc 2.31, refused) and Alpine (musl,
refused).

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
