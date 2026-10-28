# Changelog

All notable changes to `amp-harness` are listed here. The harness has its own version, separate
from AMP's server images, and follows [Semantic Versioning](https://semver.org/). Releases are
tagged `harness-vX.Y.Z`. The versioning and compatibility policy is D14 in the AMP project 2 KB,
`decisions/amp-harness-architecture.md`. How to cut a release: [RELEASING.md](RELEASING.md).

## [0.1.0] - unreleased

The first release. AMP Harness is AMP's own coding-agent harness: a single Rust binary, built on
pi-rust's core crates (`rpi-*` 0.8.0) with an MCP client on `rmcp` 3.6.0. You chat with AMP's
manager agent, which plans on the AMP board and dispatches specialist worker agents itself. The
chat runs next to the AMP web UI. It needs no Claude Code or opencode setup.

Platforms: macOS 13+ (Apple silicon and Intel) and Linux on x86_64 and aarch64 with glibc 2.35+.
Not yet supported: Windows, and musl-based Linux such as Alpine.

### Install and first run

- One-line install: `curl -fsSL …/install.sh | sh`. It downloads the newest `harness-v*`
  release for your platform, checks its SHA-256 and installs into `~/.local/bin` without sudo.
  Options: `--version`, `--prefix`, `--preset`, `--no-setup`, `--modify-path`, `--archive`,
  `--uninstall`. Running it again upgrades in place. Prereleases install only with `--version`.
  install.sh refuses musl and glibc older than 2.35 with a clear message (#2115, #2276, #2122).
- `amp-harness setup` (alias `init`) is the first run. It sets the server, logs you in, links the
  folder to a project (picking one or creating one and writing `.amp.json`) and connects a model
  provider. It can also run non-interactively with flags, or from an org preset (`--preset`).
  install.sh offers to run it, and it works under `curl | sh` (#2113, #2275).
- A folder below a linked folder is asked about, not linked silently. Setup never overwrites a
  parent's `.amp.json` (#2356).
- `make -C v2/harness install` builds, packs and installs from a checkout (#2115).

### Connecting to AMP

- MCP over Streamable HTTP. The harness negotiates the stateless 2026-07-28 protocol and falls
  back to the legacy 2025-11-25 `initialize` protocol on servers that only speak that.
  `--protocol` / `AMP_MCP_PROTOCOL` forces one (#2083).
- OAuth login in the browser (PKCE, dynamic client registration, refresh). Credentials go in the
  OS keychain, or in a 0600 file when no keychain is available. `AMP_TOKEN` is supported for
  headless use (#2084, #2165).
- Saved servers: `amp-harness server show|set|use|remove` and `/server` in the chat. Each server
  keeps its own login (#2164).
- `--ca-file` / `AMP_CA_FILE` for self-signed AMP certificates (#2083).
- The chat opens even when AMP is unreachable and keeps retrying. Connection errors are one
  sentence with a next step (#2178).

### Chat with the manager

- `amp-harness` with no subcommand opens the full-screen chat with the manager (#2097).
- AMP theme (dark and light, truecolor and 256 colours), with a header and status line showing
  the org, project, current ticket, MCP health and board counts. Ticket ids, state words and
  colours match the web UI (#2103, #2104, #2106).
- Slash commands: `/amp`, `/ticket`, `/workers`, `/questions` (answer a question in place),
  `/plan`, `/project`, `/open`, `/server`, `/connect`, `/model`, `/mcp`, `/agents`, `/skills`,
  `/config`, `/reload`, `/usage`, `/stop`, `/help`. Typing `/` opens a command menu (#2108,
  #2182, #2245).
- The transcript: one line per tool call with a short summary, collapsible thinking
  (Ctrl+T or a click), word wrap, markdown tables, keyboard and mouse scrolling with a
  "new output" pill, Up/Down prompt history, compact or comfortable density (`[tui] density`),
  and a spinner that shows the tool call being written (#2176, #2177, #2180, #2183, #2185,
  #2345, #2346, #2348, #2384).
- Plan approval stays with people. The manager cannot approve its own plan. It shows "waiting
  for your approval" and resumes by itself once you approve in the AMP web UI
  (`[manager] auto_resume_on_approval`) (#2171, #2201, #2425).
- The manager's system prompt is about 22k tokens, down from about 38k (#2367).
- Asks before quitting while a worker is running, and hands abandoned tickets back (#2204).

### Workers and run reporting

- The manager dispatches workers with its own `amp_run_ticket` tool. Each worker is an
  independent agent session with its role's model, prompt and tools. By default up to 4 run in
  parallel, each with a 60-minute limit and 400 turns, set under `[workers]` (#2096).
- Where a worker works (`[workers] isolation = auto|worktree|folder|shared`): a git worktree per
  ticket in a repository, or a folder per ticket (`amp-work/<ticket>/`) for work outside git
  (#2364).
- Open a running worker's live session from the chat (Ctrl+W or a click), switch between
  workers, and stop one (#2252, #2347).
- Run reporting is automatic. Model, tokens, cost, branch, commit and pushed are filled in on
  `amp_complete_task` / `amp_block_task`. A worker that ends without closing its ticket is
  reported as an abandoned run (#2098, #2146).
- `amp-harness run -p "…"` runs the manager headless, with optional `--json` events (#2095).

### Agents, skills, roles and tool policy

- The manager and 10 worker roles ship embedded. They are generated from the same source as
  AMP's opencode and Claude Code plugins (#2090).
- Each role's tool policy is enforced by the harness and never asks a person. A denied call is a
  tool error the model sees. Restricted bash fails closed (#2093, #2139, #2140, #2145).
- `amp-harness agents …` and `skills …` add, edit, remove, reset and validate agents and skills
  from templates, at user or repo level (#2234).
- A repository's own agents, skills, roles, model profile and MCP servers are used only after
  `amp-harness trust` (#2241, #2310).
- Changes from another terminal reach an open chat live, or straight away with `/reload` (#2242).

### Models and providers

- Several providers can be active at once, and each role resolves its own provider, model and
  thinking level. The defaults follow AMP's opencode roster on OpenRouter, with an `anthropic`
  profile and an `inherit` mode as alternatives (#2091).
- The default manager model is `openai/gpt-6.1-sol` on OpenRouter at low thinking, chosen by the
  manager eval (#2363).
- `amp-harness connect` / `disconnect` and `/connect` add a provider. Keys are entered hidden,
  checked against the provider and kept in the OS keychain (#2163).
- Your company's own OpenAI-compatible endpoint, with models per role and org presets. Endpoint
  auth can be a bearer token, a header, a token command, OAuth client credentials or Azure
  Entra, with CA, mTLS and proxy settings (#2275, #2277).
- `amp-harness models list|set|reset|profile` and `config show|check` (#2235).
- Every model request carries the current date, time and timezone (#2250).

### MCP servers

- Any MCP server, not only AMP: stdio, Streamable HTTP (headers, bearer, OAuth) and legacy
  HTTP+SSE (#2236, #2237, #2238).
- `amp-harness mcp …`: list, add, edit, remove, enable, disable, test, log in, set secrets and
  trust servers (#2243).
- `amp-harness mcp import` from Claude Code, opencode, VS Code, Cursor and Claude Desktop (#2244).
- External MCP tools work in the chat, in `run -p` and in workers (#2239).
- Per-role MCP access policy with `mcp` rules on `<server>/<tool>` (#2240).
- A repository's MCP servers can't read your secrets, and URLs are redacted in errors (#2308).

### Remote control

- `/remote` in the chat pairs your phone or another computer through the AMP web app. You watch
  the chat live and can send messages and press Stop. The laptop still does all the work. It is
  off until you turn it on each time. It needs your own AMP login (not `AMP_TOKEN`), and it must
  be allowed for your org. A self-hosted AMP also needs the relay enabled (#2373, #2376, #2423,
  #2424).
- Text sent to the remote is redacted. This covers provider keys, MCP secrets and keys added
  later (#2391, #2399).

### Safety

- Agents' bash never sees `AMP_TOKEN` or provider keys unless you list them in
  `[secrets] pass_to_bash` (#2142).
- Bash never waits on a person. There is a default 600 s timeout and a non-interactive
  environment for git, ssh and package managers (#2143).
- A model that repeats one tool call is stopped (#2225).

### Diagnostics

- `amp-harness doctor [--json] [--offline]` checks the connection, negotiated protocol, auth,
  the AMP tool contract, CA trust, provider keys, pinned models, MCP servers, agents, skills,
  trust and the worker workspace. Every warning or failure comes with a fix (#2085, #2114,
  #2246).
- `amp-harness --version` prints the harness version, the rpi and rmcp pins, the embedded assets
  commit and the build commit (#2114, #2122).
- `whoami`, `tickets list|get` (#2085).

### Release and CI

- Release archives for four targets, published as GitHub Releases on `harness-v*` tags with
  checksums, `SHA256SUMS`, `install.sh` and a rendered Homebrew formula (#2115, #2122).
- Compatibility CI: the smoke test runs against a real AMP stack in three MCP protocol modes
  (auto, forced 2026-07-28, forced 2025-11-25) (#2086, #2123).

### Known limitations

- Binaries are not signed or notarized.
- There is no Homebrew tap yet. `amp-harness.rb` is attached to each release for when there
  is one.
- No Windows or musl builds.

[0.1.0]: https://github.com/jacobsims214/amp/releases/tag/harness-v0.1.0
