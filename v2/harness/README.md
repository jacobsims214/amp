# amp-harness

AMP's own agent harness: a Rust binary built on pi-rust's core crates (`rpi-*`) with an MCP client on
`rmcp`. Design and build plan: AMP project 2 KB, `decisions/amp-harness-architecture.md`.

Status: **M1-5.** `amp-mcp` connects to AMP (both MCP protocol eras, static token or OAuth login),
and the CLI has `login`, `logout`, `doctor`, `whoami`, `tickets list|get` and the hidden `mcp call`.
`make smoke` runs them against a real local AMP.

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
dependent tickets, a comment) with `mcp call` and reads it back with `tickets list|get`. Finally it
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
