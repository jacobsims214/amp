# AMP — Agentic Management Platform

**Mission control for humans and AI agents working together.**

AMP is an open-source work management platform built for teams where people and AI agents share
the work. It doesn't care which agent harness you use (Claude Code, opencode, or anything else
that speaks MCP), and it keeps one board, one plan and one knowledge base for the whole crew,
across teams and orgs.

You plan in AMP and approve before anything launches. Your agent then dispatches workers in
parallel, and every run, comment, token and commit lands on the ticket it belongs to.

The active codebase is in [`v2/`](./v2/). AMP is licensed under [Apache-2.0](./LICENSE).

**Install the AMP harness** (macOS and Linux) with one line:

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
```

With your company's preset (AMP server, model endpoint, models per role; no secrets):

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh -s -- --preset https://intranet.example.com/amp-preset.toml
```

It installs `amp-harness` to `~/.local/bin`, then offers to run `amp-harness setup`. Run the same line
again to upgrade; add `--uninstall` to remove it. Options:
[v2/harness/README.md](./v2/harness/README.md#install).

---

## What it does

- **Harness-agnostic.** Agents connect over MCP (Streamable HTTP) with OAuth or an API token.
  Claude Code gets a plugin; opencode gets a config sync; anything else that speaks MCP works too.
- **Plan, approve, launch.** Work is epics → stories → tickets, wired into a dependency graph.
  A person's manager agent proposes the plan, and that person approves it before anything runs.
  Tickets whose dependencies are done dispatch in parallel, and finished work unblocks the next
  tickets automatically.
- **People own the work.** Every ticket has a human owner, whichever agent works it. Agents act
  with their person's token, so everything they do is recorded as that person. Unattended agents
  (CI, bots) get their own agent accounts.
- **Orgs, teams and access.** Orgs hold teams, and teams are linked to projects as owner or
  member teams. Access runs Member → Lead → Owner → org admin → global admin.
- **A full record per ticket.** Comments, state changes, and every run: agent, model, tokens,
  cost, branch, commit, PR, and merge. Standups, spend reports and traceability come from that
  record. Nobody writes status updates.
- **Knowledge that carries over.** Project, team, org and global knowledge bases, with hybrid
  keyword and semantic search, so agents don't rediscover what the crew already learned. AMP's
  own guide ships as the global KB, so agents can onboard their person.
- **Live UI.** Board, plan, dependency graph, reports, portfolio, traceability, day reports,
  knowledge bases, the AMP guide, trash and restore, and team and org admin. Everything updates
  in real time.
- **Tracker-agnostic.** Free-form metadata (`jira_epic`, `linear_team`, `cost_center`, …) on
  projects, epics and tickets. Reports group by whatever keys your org uses.

---

## Using AMP: deploy the chart

There's no hosted AMP yet. Every AMP is self-hosted from one Helm chart that brings the whole
stack: Postgres (CloudNativePG), Dex sign-in, oauth2-proxy, an MCP OAuth broker, Envoy, the API,
the UI, Typesense, Ollama and Valkey. Images are published to
`ghcr.io/jacobsims214/amp`.

**You need:** a Kubernetes cluster with **cert-manager** and the **CloudNativePG** operator,
a DNS name for AMP, and Helm.

```bash
# 1. Get the chart
git clone https://github.com/jacobsims214/amp
cd amp/v2/deploy/helm/amp
helm dependency build

# 2. Start from the production values and set your own:
#    global.domain, global.bootstrapAdminEmails, storage classes, global.imageTag
cp values-production.yaml my-values.yaml

# 3. Launch
helm install amp . -n amp --create-namespace -f my-values.yaml
```

TLS can be terminated by Envoy (cert-manager: self-signed or Let's Encrypt) or by a gateway in
front of the cluster (`global.tls.enabled: false`). There's an Argo CD `Application` in
[`v2/deploy/argocd/`](./v2/deploy/argocd/). For how the pieces fit, including auth flows and
the local and production differences, see
[`v2/docs/deploy-architecture.md`](./v2/docs/deploy-architecture.md).

**Then:** sign in as the bootstrap admin, create an org and a team, make a token under
**Agents & tokens**, and connect your agent.

Signed-out visitors to the root URL see the public home page at `/home/`. Everything else,
including the API and MCP endpoint, requires sign-in.

---

## Connecting your agent

**Claude Code**: add the MCP server:

```bash
claude mcp add --transport http amp https://<your-amp>/mcp --header "Authorization: Bearer <token>"
```

You can also install the AMP plugin, which adds `/amp:manager`, the worker, reviewer and
researcher agents, the skills, and the MCP servers:

```bash
v2/scripts/setup-claude-code.sh               # from a checkout
export AMP_MCP_URL=https://<your-amp>/mcp     # point it at your server
```

**opencode**: `v2/scripts/setup-opencode.sh` installs the AMP opencode plugin from this
checkout and adds it to the `plugins` list in `~/.config/opencode/opencode.json`. The plugin
brings the agents, skills, MCP servers and model settings. Pass `--url https://<your-amp>/mcp`
for your server and `--token <token>`, or leave the token out to sign in with OAuth. If you
copied AMP's agents into `~/.config/opencode` the old way, add `--clean` to remove those copies.

**Anything else**: point it at `https://<your-amp>/mcp` with `Authorization: Bearer <token>`.

Check the connection by asking your agent to call `amp_whoami`. Then ask it to onboard you:
it reads the built-in guide (`amp_guide`) and walks you through AMP.

More detail: [`v2/kb/setup/connect-your-agent.md`](./v2/kb/setup/connect-your-agent.md) and
[`v2/scripts/README.md`](./v2/scripts/README.md).

---

## How a mission flies

1. **Plan.** You tell your manager agent what you want. It searches the knowledge base for prior
   work, then builds epics, stories and tickets with dependencies on the board.
2. **Approve.** The manager presents the plan and stops. Nothing runs until you say go.
3. **Launch.** The manager dispatches every ready ticket and starts a worker for each. Workers
   read their ticket, comment as they go, write findings to the KB, and complete or block the
   ticket with a run record.
4. **Gate.** Reviewers check each wave. Fixes become tickets wired into the next wave.
5. **Land.** Branches, PRs and merges are recorded on the run (CI can report merges too), and
   finished tickets unblock their dependents.

If an agent needs a decision, it asks a question on the ticket and blocks until a person answers.

---

## Architecture

```
  Claude Code · opencode · any MCP client          Browser
                 │  MCP (OAuth / token)               │
                 ▼                                    ▼
  ┌───────────────────────────────────────────────────────────────┐
  │  Envoy: TLS, routing, JWT checks                              │
  │  oauth2-proxy (browser) · Dex (sign-in) · MCP OAuth broker    │
  └───────────────────────────────────────────────────────────────┘
                 │                                    │
                 ▼                                    ▼
  ┌──────────────────────────────┐        ┌───────────────────────────┐
  │  amp-api (Go)                │  REST  │  UI (React/Vite/Tailwind) │
  │  MCP tools + REST + SSE      │◄──────►│  board · plan · DAG ·     │
  │  actor per project/epic/     │  SSE   │  reports · portfolio ·    │
  │  story/ticket (protoactor),  │        │  trace · KB · guide       │
  │  clustered across replicas   │        └───────────────────────────┘
  └──┬────────────┬────────────┬─┘
     │            │            │
  Postgres    Typesense      Valkey (asynq)  ── KB indexing queue
  (CNPG)      + Ollama embeddings
```

- **amp-api** serves the MCP tools, the REST and SSE API, and authorization. Each project,
  epic, story and ticket is an actor that owns its state machine, serializes writes, and
  unblocks dependents. Replicas form a cluster.
- **Knowledge base**: Typesense hybrid search with local `nomic-embed-text` embeddings from
  Ollama. Indexing runs through an asynq queue on Valkey.
- **Identity**: Dex (local users or your SSO), oauth2-proxy for the browser, and a small broker
  that gives MCP clients standard OAuth (discovery, dynamic client registration, PKCE).

Design notes are in [`v2/docs/`](./v2/docs/): actor model, RBAC, KB scopes, CI merge
reporting, deploy architecture, and the wargame write-ups.

---

## Developing AMP

There are two ways to run AMP locally.

**docker compose** is quickest. It has no sign-in, and every request acts as an anonymous
local-dev user, so use it for development only:

```bash
cd v2
make up          # postgres, typesense, ollama, amp-api, ui
make kb-setup    # pull nomic-embed-text into Ollama (once)
```

The UI is at `http://localhost:5173`. MCP is at `http://localhost:8000/mcp` and REST at `:3001`.

**The Helm chart on docker-desktop Kubernetes** matches production, with sign-in and TLS. See
[`v2/docs/deploy-architecture.md`](./v2/docs/deploy-architecture.md).

Common targets (run `make help` for the full list):

```bash
make dev           # api + ui as local processes against dockerised postgres
make api-test      # API integration tests (needs postgres)
make check         # go vet + UI typecheck + tests
make ui-dev        # Vite dev server, proxies /api to :3001
```

The Claude Code plugin in `v2/plugins/amp/` is generated from `v2/.opencode/`. Edit the
sources and run `v2/scripts/build-claude-plugin.sh`. To use the agents in opencode, run
`v2/scripts/setup-opencode.sh` once; it installs the opencode plugin from `v2/plugins/opencode/`.

---

## Repository layout

```
v2/
├── amp-api/            Go API
│   ├── cmd/server/     main, migrations, integration tests
│   ├── cmd/mcpoauth/   MCP OAuth broker
│   ├── cmd/authadmin/  user admin (Dex gRPC)
│   └── internal/       actor, api, auth, authz, cluster, kb, mcp, queue,
│                       repository, service, webhook, …
├── ui/                 React app; public home page in ui/public/home/
├── kb/                 the AMP guide, shipped as the global knowledge base
├── deploy/helm/amp/    Helm chart (values.yaml, values-production.yaml)
├── deploy/argocd/      Argo CD Application
├── docs/               design docs and wargame write-ups
├── .opencode/          agents, skills, plugin (source of truth for both harnesses)
├── plugins/amp/        generated Claude Code plugin
├── scripts/            harness setup and plugin build
├── docker-compose.yml  local dev stack
└── Makefile
archive/                v1 (Odoo-based), kept for reference only
```

---

## Contributing and license

AMP is open source under the [Apache License 2.0](./LICENSE).

Contributions are welcome: see [CONTRIBUTING.md](./CONTRIBUTING.md). Contributors sign the
[Contributor License Agreement](./CLA.md) once, by commenting on their first pull request.
