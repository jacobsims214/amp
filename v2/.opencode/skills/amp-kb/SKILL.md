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

The KB is a per-project document store with **hybrid keyword + semantic search**. The
semantic layer uses `nomic-embed-text` embeddings — it understands meaning, not just
keywords. "goroutine lifecycle" will find a doc about actors. "authentication system"
will find a doc about JWT middleware. But only if the doc is written in a way that
gives the model enough natural language to work with.

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

## Rule 3: Create or update — always check first

Before writing any KB doc, **decide whether to create or update**.
Prefer updating over creating. Fragmented docs on the same topic hurt search quality —
a single well-maintained doc on auth is far more useful than four partial ones.

### The decision flow

```
1. Decide what you want to document (topic, category)

2. Search for existing docs on this topic:
   amp_kb_search(project_id=ID, query="<topic>")
   amp_kb_list(project_id=ID, tag="<relevant-tag>")

3. If a relevant doc exists at a known path:
   amp_kb_get(project_id=ID, path="architecture/auth.md")
   → Read the full content
   → Merge: keep everything good, add your new information, fix anything wrong
   → Write back the complete merged content to the SAME path

4. If no relevant doc exists:
   → Create a new one at the appropriate path
```

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

When updating, `amp_kb_write` **fully replaces** the document. You must include ALL
the existing content plus your additions. Do not drop anything from the existing doc.

```
existing = amp_kb_get(project_id=ID, path="architecture/auth.md")

# Read the full existing content
# Write a new version that includes:
#   - Everything from existing.content (unchanged where still correct)
#   - Your new findings added to the right section
#   - Any corrections to things that were wrong
#   - Existing tags PLUS any new ones you need

amp_kb_write(
  project_id=ID,
  path="architecture/auth.md",    ← SAME path = update
  title=existing.title,           ← keep or improve the title
  content="[full merged content]",
  tags=existing.tags + ["new-tag"],
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

This is the most important rule. **The embedding model converts your prose into a
vector. Sparse, code-heavy docs produce weak vectors. Dense, specific prose produces
strong vectors that surface on relevant queries.**

### Write in natural language paragraphs, not just bullet lists

The model was trained on prose. It extracts meaning from complete sentences.

**Weak embedding** — the model gets almost nothing to work with:
```
# Auth System
- Uses JWT
- Tokens expire in 24h  
- See internal/handler/auth.go
```

**Strong embedding** — the model understands the concept and all its related terms:
```
# Auth System

The authentication system uses JSON Web Tokens (JWT) with RS256 signing. When a user
logs in via POST /auth/login, the server validates their credentials against the users
table, generates a signed JWT containing the user ID and roles, and returns it as a
bearer token. The token expires after 24 hours. All protected routes check for a valid
Authorization: Bearer header via the auth middleware in internal/handler/auth.go.

The decision to use RS256 over HS256 was made so the public key can be distributed to
downstream services for verification without sharing the signing secret. The private key
is loaded from the SIGNING_KEY environment variable at startup.
```

The second version will surface on queries like: "how does login work", "JWT token
validation", "bearer token auth", "RS256 vs HS256", "protected routes middleware" —
none of which appear verbatim in the doc.

### Name the concepts explicitly, then explain them

The model connects synonyms when concepts are named clearly. Say the thing, then
explain it:

```
The actor model (also called the message-passing concurrency model) is the core
concurrency primitive in this system. Each actor is a goroutine with a private
mailbox channel. Actors never share memory — they communicate only by sending
immutable messages to each other's mailboxes.
```

This single paragraph will match: "goroutine channels", "concurrency model", "message
passing", "immutable state", "mailbox pattern", "Go actor pattern" — all by semantic
proximity, not keyword match.

### State decisions AND the reasoning behind them

The reasoning is often what future agents are searching for:

```
We chose pgvector over a dedicated vector database (Qdrant, Weaviate) because:
1. The project already uses PostgreSQL — no new service to operate
2. At fewer than 10,000 documents per project, query latency is under 5ms
3. Migrations and schema changes stay in one place

The tradeoff is that at very large scale (100k+ documents) a dedicated vector DB
would offer better ANN index performance. This can be reconsidered if needed.
```

A future agent asking "why not use Qdrant" or "when should we switch vector databases"
will find this document.

### Include the specific symbols that will be searched

After the explanatory prose, include the concrete details:

```
Key files:
- internal/kb/service.go — KB business logic, WriteDoc, Search
- internal/kb/service.go:collectionHasEmbedding — checks if semantic search is active

Key types:
- kb.Service — main KB service, holds Typesense client
- kb.Doc — full document with content field
- kb.SearchResult — search hit with excerpt and score
```

These exact identifiers (file paths, function names, type names) are still matched by
the keyword layer of the hybrid search.

### One concept per document

Documents are chunked at ~500 tokens. If a single doc covers five unrelated topics,
each chunk will embed as a confused mixture. Write focused documents:

- `architecture/auth.md` — just about authentication
- `architecture/actors.md` — just about the actor model
- `decisions/001-pgvector-vs-qdrant.md` — just about that one decision

A focused 300-word doc embeds better than a sprawling 2000-word doc.

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

## Path naming conventions

| Category | Path pattern | When to use |
|----------|-------------|-------------|
| `architecture/<topic>.md` | How a component works, system design |
| `decisions/<NNN>-<topic>.md` | Why a decision was made, ADRs |
| `how-to/<topic>.md` | Step-by-step operational guides |
| `discoveries/<topic>.md` | Findings from exploration, surprises |
| `apis/<service>.md` | API shapes, endpoint contracts |

---

## MCP tool reference

```
amp_kb_search {project_id, query, tags?, limit?}
  → {results: [{path, title, excerpt, tags, score}]}
  Use natural language queries. The model understands concepts.

amp_kb_get {project_id, path}
  → full doc with content field (full markdown)
  Read this before touching related code.

amp_kb_write {project_id, path, title, content, tags, author?}
  → writes and re-indexes the doc (embeddings regenerated automatically)

amp_kb_list {project_id, tag?}
  → [{path, title, tags, updated_at}] — all docs, no content

amp_kb_delete {project_id, path}
  → removes doc and all its chunks from the index

amp_kb_tags {project_id}
  → [{tag, count}] — browse what's in the KB

amp_kb_reindex {project_id}
  → re-embeds all docs (use if Ollama model was changed)
```
