# amp-harness

AMP's own agent harness: a Rust binary built on pi-rust's core crates (`rpi-*`) with an MCP client on
`rmcp`. Design and build plan: AMP project 2 KB, `decisions/amp-harness-architecture.md`.

Status: **M1-2.** The workspace builds and `amp-harness --version` works. `amp-mcp` connects to AMP
(both MCP protocol eras), lists tools and calls them; the CLI does not use it yet (M1-4).

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
url = "https://amp.local:8443/mcp"
ca_file = "amp-local-ca.crt"
protocol = "auto"            # auto | 2026-07-28 | 2025-11-25
```

- `auto` speaks the stateless 2026-07-28 protocol (`server/discover`) and falls back to the legacy
  `initialize` handshake at 2025-11-25 when the server is older. Forced modes are for compatibility
  tests.
- `AMP_TOKEN` (an `amp_…` token) is sent as `Authorization: Bearer`. It is held in memory only and
  redacted from `Debug`. OAuth lands in M1-3.
- Server instructions come from `server/discover` (or `initialize`). `tools/list` is cached for as
  long as the server's `ttlMs` hint allows (AMP: 5 min, private). Every call has a 120 s timeout,
  and a timeout is returned as an error.

Tests run rmcp's own Streamable HTTP server in-process (`crates/amp-mcp/tests/`), in both eras, plus
an HTTPS server signed by a throwaway CA for `--ca-file`.

## Dev loop

```sh
make -C v2/harness build      # cargo build --workspace --all-targets --locked
make -C v2/harness test       # cargo test --workspace
make -C v2/harness lint       # fmt --check + clippy -D warnings (same as CI)
make -C v2/harness smoke      # build + scripts/smoke.sh
make -C v2/harness version    # amp-harness --version: harness version + rpi/rmcp pins
make -C v2/harness clean      # delete target/ (do this before removing a worktree)
```

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
