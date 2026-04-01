# AMP — Agentic Management Platform

AMP is a local-first platform for running fleets of AI agents on software projects.
Agents plan work, execute tasks, and document findings — coordinated through a custom
API built on the actor model, with a real-time kanban UI and a semantic knowledge base.

**The primary project is in [`v2/`](./v2/).**

---

## What it is

AMP gives AI agents (running in [OpenCode](https://opencode.ai)) a structured way to:

- **Plan** work as epics → stories → tasks with DAG dependencies
- **Execute** tasks in parallel fleets, with automatic unblocking when deps complete
- **Document** findings into a searchable knowledge base so agents don't re-discover things
- **Coordinate** through a single API so the human can monitor everything on a live board

The core idea: a manager agent plans the work and waits for your approval, then
dispatches worker agents in parallel. Workers run, log progress to their tickets,
write docs to the KB, and complete. Blocked tasks auto-unblock when their
dependencies finish. You watch it all happen in real time on the kanban board.

---

## Quick start

```bash
cd v2
make up          # starts postgres + typesense + ollama + amp-api + ui
make kb-setup    # pull nomic-embed-text for semantic search (~274MB, run once)
```

Open `http://localhost:5173` for the board.

Open OpenCode from `v2/` to get the `amp-manager` and `amp-worker` agents configured.
Or run `make sync-global` once to make them available from any directory.

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  OpenCode (agents)                                       │
│  amp-manager → plans, dispatches, monitors              │
│  amp-worker  → executes one task end-to-end             │
└────────────────────────┬────────────────────────────────┘
                         │ MCP over HTTP/SSE (:8000)
┌────────────────────────▼────────────────────────────────┐
│  amp-api (Go)                                            │
│                                                          │
│  Actor hierarchy (protoactor-go):                        │
│    ProjectActor → EpicActor → StoryActor → TaskActor    │
│  Each actor owns its state machine, serialises writes,   │
│  auto-unblocks dependents on completion, fires SSE.      │
│                                                          │
│  KB service (Typesense + Ollama):                        │
│    Hybrid keyword + semantic search on markdown docs     │
│    nomic-embed-text embeddings at index + query time     │
└────────┬───────────────┬───────────────┬────────────────┘
         │               │               │
    PostgreSQL       Typesense        Ollama
    (task state)    (KB index)     (embeddings)
                         │
┌────────────────────────▼────────────────────────────────┐
│  UI (React + Vite + Tailwind, :5173)                     │
│  Kanban  — epic rows → story rows → status columns       │
│  DAG     — dependency graph, dagre layout, hover trails  │
│  Report  — activity feed, cycle times, completions       │
│  KB      — document tree, editor, semantic search        │
└─────────────────────────────────────────────────────────┘
```

---

## v2/ structure

```
v2/
├── amp-api/              Go API — actors, MCP server, REST API, KB
│   ├── cmd/server/       main.go + 32 integration tests
│   └── internal/
│       ├── actor/        ProjectActor EpicActor StoryActor TaskActor
│       ├── domain/       Task Epic Story KB types and events
│       ├── repository/   PostgreSQL (plain pgx, no ORM)
│       ├── mcp/          25 MCP tools over HTTP/SSE
│       ├── api/          REST endpoints + SSE hub
│       ├── kb/           Typesense client, chunking, semantic search
│       └── hub/          SSE event broadcast
├── ui/                   React + Vite + Tailwind UI
├── .opencode/
│   ├── prompts/          amp-manager.txt  amp-worker.txt  (thin — load skills)
│   └── skills/           amp-planning  amp-execution  amp-kb  amp-mcp
├── scripts/
│   └── sync-global-config.sh   Merges v2 config into ~/.config/opencode
├── opencode.json         Scoped OpenCode config (agents + MCP)
├── docker-compose.yml    postgres, typesense, ollama, amp-api, ui
└── Makefile
```

---

## Make targets

```bash
make up              # start full stack
make down            # stop everything (including local dev processes)
make dev             # run api + ui locally against dockerised postgres
make api-test        # run all 32 integration tests
make kb-setup        # pull nomic-embed-text into Ollama (first time only)
make kb-status       # check Typesense health and KB collections
make sync-global     # merge v2 skills + prompts into ~/.config/opencode
```

---

## How agents work

**Manager** (`amp-manager`) loads `amp-planning` + `amp-kb` skills:
1. Reads `.amp.json` for `project_id` (creates project if missing)
2. Searches the KB for relevant prior work
3. Creates the full epic → story → task hierarchy via MCP
4. Presents the plan and **stops — waits for your explicit approval**
5. After approval: calls `amp_dispatch_task` then spawns worker sub-agents in parallel
6. Monitors, re-dispatches as blocked tasks auto-unblock

**Worker** (`amp-worker`) loads `amp-execution` + `amp-kb` skills:
1. Searches KB before starting (non-optional)
2. Reads its ticket — the `description` field is its complete prompt
3. Posts a starting comment
4. Does the work, logging every step as ticket comments
5. Writes to the KB when it discovers something worth keeping
6. Calls `amp_complete_task` — auto-unblocks dependent tasks

---

## Services

| Service | Port | Purpose |
|---------|------|---------|
| amp-api MCP | 8000 | MCP server — OpenCode agents |
| amp-api REST | 3001 | REST + SSE API — UI |
| UI | 5173 | Kanban, DAG, KB, Reports |
| PostgreSQL | 5432 | Task/project state |
| Typesense | 8108 | KB document search |
| Ollama | 11434 | Local embedding model |

---

## Archive

`archive/` contains the original v1 implementation which used Odoo as the backend.
Kept for reference only. All active development is in `v2/`.
