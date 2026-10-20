---
name: amp-kb
description: How to search the AMP knowledge base before starting work, and how to write documents that embed well for semantic search
---

# AMP Knowledge Base Skill

> **Load this skill late, not early.** This skill is large (~13kb). Workers should
> search the KB directly with `amp_kb_search` at startup — no skill load needed.
> Load this skill only when you are in Step 4 and ready to write a KB doc.
> Loading it at the start of every task wastes context the worker needs for actual work.

> This skill is loaded by both `amp-execution` (workers) and `amp-planning` (manager).
> It defines the canonical rules for KB search and writing. Neither prompt nor any
> other skill repeats these rules — this is the single source of truth.

Each project has a KB: a document store with **hybrid keyword + semantic search**. The
semantic layer uses `nomic-embed-text` embeddings — it understands meaning, not just
keywords. "goroutine lifecycle" will find a doc about actors. "authentication system"
will find a doc about JWT middleware. But only if the doc is written in a way that
gives the model enough natural language to work with.

---

## Which knowledge base

There are three kinds of KB. **The project KB is the default for everything** — every
`amp_kb_*` call that passes only `project_id` uses it, and that is where almost all knowledge
belongs.

| KB | Holds | Who writes |
|---|---|---|
| Project (default) | This project's architecture, decisions, gotchas | anyone on the project |
| Team (`scope: "team"`) | What's true for every project the team works on: runbooks, services the team owns, team conventions | members of the team |
| Org (`scope: "org"`) | What's true across the whole org: brand and design system, process, standards, policy, glossary | org owners and admins; everyone else annotates |

- **Know where you are.** `amp_kb_sources(project_id)` lists the project's KB, its org's KB,
  and the KB of each team linked to the project (owner team first), with doc counts and
  whether you can read and write each. Call it once when you start in a project.
- **Read narrowly.** Search the project KB. Search a team or org KB when your user asks, or
  when you need something only it would hold — the brand and design system (org), the
  team's deploy runbook (team). Don't search all three on every task; it bloats your context.
- **Write to the project KB.** Put knowledge in a team or org KB **only when your user has
  decided it belongs there**. Never move or copy knowledge there on your own initiative. If
  you think something is true beyond this project, say so to your user, with why, and keep
  it in the project KB until they decide.
- **A team or org write needs `reason`:** specifically why this belongs to the whole team
  or org rather than one project, and that your user decided it. It's stored with the doc.
- **Org KB, not an admin?** Use `amp_kb_annotate` to flag something wrong or out of date.
- **Several of your teams on the project?** `scope: "team"` is refused with the list; pass
  `team_id`. Nothing crosses orgs: a team or org outside the project's org is refused.

```
amp_kb_sources(project_id=2)
amp_kb_search(project_id=2, scope="org", query="brand colour palette")
amp_kb_search(project_id=2, scope="team", query="how we deploy to staging")
amp_kb_write(project_id=2, scope="team", path="how-to/deploy-staging.md", ...,
             reason="Every Devs project deploys this way; Jane asked to keep it in the team KB")
```

---

## Rule 1: Search before you start

After reading your ticket, search the KB immediately:

```
amp_kb_search(project_id=PROJECT_ID, query="<task name + key terms>")
```

Use multiple searches if needed — the model understands concepts so use natural queries:

```
amp_kb_search(project_id=2, query="how does authentication work in this system")
amp_kb_search(project_id=2, query="database connection handling")
amp_kb_search(project_id=2, query="actor state machine task lifecycle")
```

- Relevant result → read with `amp_kb_get` before touching anything
- No results → proceed, but document what you learn when done
- These search the project KB. Reach for the team or org KB only as described in
  "Which knowledge base" above.

---

## Recency Awareness

When using `amp_kb_search`, always check the `updated_at` timestamp on search results to
assess recency. For time-sensitive information (e.g., architecture decisions, operational
procedures, API contracts), outdated docs can lead to mistakes.

### Search parameters for recency control

The `amp_kb_search` tool supports two optional parameters:

- `recency_boost` (float, default 1.0): Multiplier that increases the weight of recency
  in the hybrid search ranking. Higher values prioritize newer documents. Range: 0.0–5.0.
- `min_recency_days` (int, default 0): Filters results to documents updated within the
  specified number of days. Use this for time-sensitive queries where stale information
  is unacceptable.

### RecencyLabel values

Search results include a `recency_label` field with one of these values:

| Label | Meaning |
|-------|---------|
| `recent` | Updated within the last 7 days |
| `month` | Updated within the last 30 days |
| `quarter` | Updated within the last 90 days |
| `old` | Updated more than 90 days ago |
| `unknown` | No `updated_at` timestamp available |

### Recommendation for time-sensitive info

For queries about architecture, deployment, APIs, or procedures that may have changed:

```
amp_kb_search(
  project_id=PROJECT_ID,
  query="database connection pool configuration",
  min_recency_days=30
)
```

Set `min_recency_days=30` as a baseline for time-sensitive topics. Increase to 7 or 14
for rapidly changing systems, or use `recency_boost=2.0` without a hard filter when you
want newer docs ranked higher but still allow older context.

---

## Rule 2: Tags are MANDATORY — pick them before you write

**Every `amp_kb_write` call must include 3–6 tags. No exceptions.**

A doc with no tags is invisible to the faceted browser and weakens the keyword layer
of hybrid search. Tags feed both discovery (browsing) and retrieval (search ranking).

### The 3-part tag formula

Pick at least one tag from each bucket:

```
1. Category — what kind of doc is this?
   architecture  decision  how-to  discovery  api  testing  security

2. Technology — what specific tools/languages/frameworks?
   go  typescript  react  postgres  docker  mcp  sse  tailwind  dagre
   typesense  ollama  redis  nginx  sqlite  grpc  protobuf

3. Domain — what system component or feature area?
   auth  tasks  epics  stories  kb  dag  board  agents  workers  planning
   realtime  migrations  search  embeddings  routing  websocket  actors
```

### Examples of correct tag sets

| Doc topic | Tags |
|-----------|------|
| How JWT auth works | `architecture`, `auth`, `jwt`, `go`, `security` |
| Why we chose pgvector | `decision`, `pgvector`, `database`, `postgres`, `search` |
| Actor / task state machine | `architecture`, `actors`, `go`, `tasks`, `concurrency` |
| DAG layout algorithm | `architecture`, `dag`, `dagre`, `typescript`, `react` |
| SSE event streaming | `architecture`, `sse`, `go`, `typescript`, `realtime` |
| Running DB migrations | `how-to`, `migrations`, `postgres`, `database`, `go` |
| KB write/search contract | `api`, `kb`, `mcp`, `search`, `typesense` |

### Tags to never use — too generic to help

`docs`, `notes`, `important`, `todo`, `misc`, `general`, `info`, `update`, `code`, `stuff`

If you can't think of a specific tag, you're probably writing a doc that's too broad.
Split it, or use the topic name itself as a tag (e.g. `epic-crud`, `sse-reconnect`).

### Tag placement in the write call

```python
amp_kb_write(
  project_id=PROJECT_ID,
  path="architecture/actors.md",
  title="Actor Model — Task Lifecycle and State Machine",
  content="[full prose doc]",
  tags=["architecture", "actors", "go", "tasks", "concurrency"],  # ← pick 3-6, always
  author="amp-worker"
)
```

When **updating** an existing doc, always preserve the existing tags and add any new
ones your work introduces:

```python
existing = amp_kb_get(project_id=ID, path="architecture/actors.md")
# ... merge content ...
amp_kb_write(..., tags=existing.tags + ["new-tag-if-needed"])
```

---

## Rule 3: Create or update — the decision criteria

Your `amp_kb_write` tool description already tells you to check for an existing doc first and
prefer updating. What it doesn't tell you is *how to decide* which path applies, and *how to
merge* without losing content — that's this rule.

**Update when:**
- The topic already has a doc (same path or clearly the same subject)
- You are adding a new finding to an existing architectural component
- You discovered something wrong in an existing doc — fix it in place
- You completed a task that extends something already documented

**Create when:**
- This is a genuinely new concept, component, or decision with no existing doc
- The existing docs are in a different category (don't shoehorn unrelated things)
- The existing doc is already long and focused — a new sub-topic deserves its own doc

### How to merge correctly

`amp_kb_write` **fully replaces** the document at that path. When updating, read the full
existing content first, then write back everything good from it plus your additions plus any
corrections — never drop existing content just because you didn't touch that part:

```
existing = amp_kb_get(project_id=ID, path="architecture/auth.md")

amp_kb_write(
  project_id=ID,
  path="architecture/auth.md",    ← SAME path = update
  title=existing.title,           ← keep or improve the title
  content="[everything from existing.content, plus your new findings, plus any corrections]",
  tags=existing.tags + ["new-tag-if-needed"],
  author="amp-worker"
)
```

### Write when you learn something

Write or update a KB doc when you:
- Discover how a system component actually works (especially if non-obvious)
- Make an architectural decision with trade-offs
- Find a gotcha, edge case, or behaviour that surprised you
- Complete work that future agents will build on
- Correct something that turned out to be wrong

---

## Rule 4: How to write content that embeds well

This is the most important rule. **The embedding model converts your prose into a vector.
Sparse, code-heavy docs produce weak vectors. Dense, specific prose produces strong vectors that
surface on relevant queries.**

### Write in natural language paragraphs, not just bullet lists

The model was trained on prose — it extracts meaning from complete sentences, not fragments.

**Weak:** `- Uses JWT\n- Tokens expire in 24h\n- See internal/handler/auth.go`

**Strong:** "The authentication system uses JSON Web Tokens (JWT) with RS256 signing. When a
user logs in via POST /auth/login, the server validates their credentials, generates a signed
JWT containing the user ID and roles, and returns it as a bearer token that expires after 24
hours. RS256 was chosen over HS256 so the public key can be distributed to downstream services
for verification without sharing the signing secret."

The strong version surfaces on "how does login work," "JWT validation," "RS256 vs HS256" — none
of which appear verbatim. Naming a concept and then explaining it lets the model connect
synonyms: calling out "the actor model (also called message-passing concurrency)" before
describing it will match "goroutine channels," "mailbox pattern," and "Go actor pattern" alike.

### State decisions AND the reasoning behind them

The reasoning is often what future agents search for. Don't just record "we use pgvector" —
record why (already run PostgreSQL, sub-5ms latency under 10k docs, one less service to operate)
and the tradeoff being accepted (a dedicated vector DB would win at 100k+ docs). A future agent
asking "why not Qdrant" finds this because the reasoning, not just the choice, is written down.

### Include the specific symbols that will be searched

After the explanatory prose, list concrete references — file paths, function names, type names —
so the keyword layer of hybrid search can match them too:

```
Key files:
- internal/kb/service.go — KB business logic, WriteDoc, Search
Key types:
- kb.Service — main KB service, holds Typesense client
```

### One concept per document

Documents are chunked at ~500 tokens. A doc covering five unrelated topics embeds each chunk as
a confused mixture. Keep one focused doc per concept — `architecture/auth.md` is just auth,
`decisions/001-pgvector-vs-qdrant.md` is just that one decision. A focused 300-word doc embeds
better than a sprawling 2000-word one.

---

## Document structure template

Use this structure for most KB docs:

```markdown
# [Clear, specific title that names the concept]

[1-3 paragraph overview written in plain prose. Name the concept, explain what it
does, why it exists, and how it fits into the system. Use the vocabulary that someone
would search with — not just the vocabulary used internally.]

## How it works

[Explain the mechanism. Be specific about the steps, the data flow, the key
decisions. Write for someone who has never seen this codebase.]

## Why it was built this way

[The reasoning behind the design. What alternatives were considered and rejected.
What tradeoffs were made. This is gold for future agents making related decisions.]

## Gotchas and edge cases

[Non-obvious behaviours, things that broke, things to watch out for. Write these
in plain language: "If you do X without doing Y first, Z will happen."]

## Key files and types

[Concrete references: file paths, function names, type names, config keys. These
feed the keyword layer of hybrid search.]
```

---

## Path naming conventions

| Category | Path pattern | When to use |
|----------|-------------|-------------|
| `architecture/<topic>.md` | How a component works, system design |
| `decisions/<NNN>-<topic>.md` | Why a decision was made, ADRs |
| `how-to/<topic>.md` | Step-by-step operational guides |
| `discoveries/<topic>.md` | Findings from exploration, surprises |
| `apis/<service>.md` | API shapes, endpoint contracts |

---

## Annotations

Use annotations to add context or corrections to existing KB docs without rewriting them entirely.

### When to annotate vs. rewrite

- **Annotate when** you find a small error, outdated detail, or missing context that doesn't change the core meaning of the doc
- **Rewrite when** the doc is fundamentally wrong or more than 50% incorrect — in that case, update the full document with `amp_kb_write`

### How to annotate

```
amp_kb_annotate(project_id, path, text, author?)
```

- `project_id` — project ID
- `path` — document path (e.g., `architecture/auth.md`)
- `text` — the annotation content (markdown allowed)
- `author` — optional, defaults to 'agent'

### What agents see

- Annotations are returned in `amp_kb_get` responses (as `annotations` array)
- `amp_kb_search` results include `annotation_count` and `latest_annotation` fields
- Agents see the annotation when reading a doc or browsing search results

### Annotation lifecycle

- Each annotation has an `is_resolved` field (default: `false`)
- Once a note's correction is worked into the doc (rewrite it with `amp_kb_write` — rewrites keep
  the annotations), mark it resolved: `amp_kb_resolve_annotation(path, index)`. `index` is its
  position in `amp_kb_get`'s `annotations`, 0-based, oldest first. `resolved=false` reopens it.
- Resolved annotations stay on the doc as a record (with `resolved_by` / `resolved_at`), but search
  previews (`latest_annotation`) show only unresolved ones
- Resolving needs write access to the KB; in an org KB that's org owners and admins

## Knowledge Discovery

When researching a technology, use this hierarchy:
1. Search the KB first with amp_kb_search
2. Query Context7 MCP tools if not found
3. Web fetch as last resort

After finding useful information from Context7, persist it: write a KB doc with amp_kb_write including the source URL as a reference. This caches the knowledge so future agents find it in the KB instead of querying Context7 again.

## MCP tool reference

Every tool below also takes `scope` (`project` default, `team`, `org`), `team_id`, and
`org_id` to name a team or org KB; with only `project_id` it's the project KB.

```
amp_kb_sources {project_id}
  → the project's KB, its org's KB, and its teams' KBs, with can_read / can_write / docs

amp_kb_search {project_id, query, tags?, limit?}
  → {results: [{path, title, excerpt, tags, score, annotation_count, latest_annotation}]}
  Use natural language queries. The model understands concepts.

amp_kb_get {project_id, path}
  → full doc with content field (full markdown), annotations array
  Read this before touching related code.

amp_kb_write {project_id, path, title, content, tags, author?, reason?}
  → writes and re-indexes the doc (embeddings regenerated automatically)
  reason is required for team and org KBs

amp_kb_list {project_id, tag?, limit?, offset?}
  → {docs: [{path, title, tags, updated_at}], count, total, next_offset?} — newest first, no content
  One page: 250 by default (limit up to 1000). If next_offset is present, pass it as offset for the next page.

amp_kb_delete {project_id, path}
  → removes doc and all its chunks from the index

amp_kb_status {project_id}
  → stale docs, open annotations, tag counts — browse what's in the KB

amp_kb_annotate {project_id, path, text, author?}
  → adds an annotation to the document

amp_kb_resolve_annotation {project_id, path, index, resolved?}
  → marks annotation `index` (0-based, oldest first) resolved, or reopens it with resolved=false

amp_kb_reindex {project_id}
  → re-embeds every doc in the background (needs KB write access); search again in a minute
  Use when search looks broken: scores far above 1, keyword-only matches, or a doc
  amp_kb_get reads but amp_kb_search never finds
```
