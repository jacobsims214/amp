---
title: AMP Harness — agents and skills
tags: [harness, amp-harness, agents, skills, templates, permissions, dispatch, customisation]
---

# Agents and skills

The harness ships AMP's roster: the manager, workers (backend, frontend, docs), the designer,
media, reviewers, researchers and the KB curator, plus AMP's skills (planning, execution,
review, KB and so on). You can change any of them or add your own, for yourself or for everyone
in a repo.

- An **agent** is one markdown file. Its frontmatter holds the description the manager reads,
  the skills it preloads and its tool rules. Its body is the system prompt.
- A **skill** is a folder with a `SKILL.md` (and any other files). An agent either preloads it
  in full or sees it in a list and reads it when it needs it.

## Where they live (later layers win)

1. built in (shipped inside the binary);
2. yours: `~/.config/amp-harness/agents/` and `skills/`;
3. the repo's: `<repo>/.amp/harness/agents/` and `skills/`, next to `.amp.json`, for everyone who
   clones it.

An agent file replaces the whole agent. A skill overrides file by file: your `SKILL.md` replaces
only that file and the skill keeps its other files. Every command below takes `--user` (the
default) or `--repo` to pick the layer.

## A repo's agents and skills need your trust

A repo agent can change what the manager may run: a file named `amp-manager.md` replaces the
built-in manager, its `permission:` block included. A repo skill adds instructions and scripts.
So, like a repo's MCP servers, the repo's agents and skills (and its `[roles.*]` and
`[models] profile`, `harness/configuration.md`) are used only once you trust them, **as they are
now**:

- The chat asks about each one before it starts, showing everything it sets:

  ```text
    repo agent `amp-manager`:
      file        ~/src/app/.amp/harness/agents/amp-manager.md
      replaces    the built-in agent amp-manager
      description The manager, from the repo
      mode        primary
      permission  bash: {"*":"allow"}
                  edit: allow
      prompt      1 line (read it in the file above)
    Trust agent `amp-manager`? [y: yes, a: all, N: not now]
  ```

  A skill shows its directory, what it replaces and every file (size, and whether it is
  executable). Until you trust one, the built-in agent or skill (or your own) is used. The first
  time a repo is asked about, the chat says why it asks.
- A change to the file (a `git pull`) asks again. While a chat runs, the reload that picks up the
  change leaves it out and says so: `repo agent `amp-manager` not used: it changed since you
  trusted it …`. Trust it, then `/reload`.
- Headless runs (`run -p`, workers) never ask: they leave untrusted items out with a warning.
  `amp-harness doctor` warns about them too: `agents … not trusted, not used: amp-manager (repo)`,
  with the fix (`--trust-repo` for one run, or `amp-harness trust agent:amp-manager`). The ones
  in use are listed as `yours:` (your own config) and `the repo's (trusted):`.
- The listings show what runs, through the same check: `agents list`, `skills list` and
  `models list` leave an untrusted repo agent or skill out of their table (it is not dispatchable,
  has no model row, and preloads nothing) and print under it
  `not trusted, not used: repo-helper (repo)` with the same fix. `agents show repo-helper` and
  `skills show repo-lint` still show the file, marked `(repo, not trusted, not used)`, with
  `dispatch: no (not trusted, ...)` and the model it would run on once trusted. In `--json` they
  are under `untrusted` (`models list`: `untrusted_agents`), and `show --json` has
  `"trusted": false`.
  `--trust-repo` (or `AMP_HARNESS_TRUST_REPO=1`) uses everything for that one process, without
  recording it.
- What you write into the repo layer with these commands (`agents new --repo`,
  `skills edit --repo`, `models set … --repo`) is yours, so it is trusted as written.

```sh
amp-harness trust                      # everything this repo brings and whether each is trusted
amp-harness trust agent:amp-manager    # trust one as it is now (prints what it trusted)
amp-harness trust skill:pdf-tools role:manager setting:profile mcp:fs
amp-harness trust --all
amp-harness untrust agent:amp-manager  # or --all
```

Trust is recorded outside the repo, in `~/.local/state/amp-harness/trust.toml`, next to the
MCP servers you trusted.

## Agents

```sh
amp-harness agents list                     # where each comes from, its model, whether the manager can dispatch it
amp-harness agents show worker-backend      # file, model and why, skills, tool rules
amp-harness agents new sql-expert --from worker --description "Writes and reviews SQL migrations"
amp-harness agents edit sql-expert          # opens $EDITOR; checked before it is saved, reopened on an error
amp-harness agents skills sql-expert add postgres-style
amp-harness agents remove sql-expert
amp-harness agents reset worker-backend     # drop your overrides of a built-in agent
amp-harness agents validate
```

- **Templates:** `--from worker` (the default), `reviewer`, `researcher`, `blank`, or the id of
  an existing agent to copy.
- **It works at once.** A new agent needs no other setup. It runs on the model of the role it is
  based on, unless you give it one (`--model provider/model [--thinking L]`, or `amp-harness
  models set sql-expert …` later). The manager may dispatch it straight away: `agents new`
  writes `dispatch: true`.
- `agents edit` on a built-in agent first copies it to your layer.
- **In the chat:** `/agents` lists them, `/agents <id>` shows one, and `m` opens its model
  picker. Agents you create or edit in another terminal are picked up without a restart
  (`harness/configuration.md`).

What `agents new` writes (`~/.config/amp-harness/agents/sql-expert.md`):

```markdown
---
description: "Writes and reviews SQL migrations"
mode: subagent
dispatch: true
based_on: amp-worker-backend
skills: ["amp-execution","postgres-style"]
permission:
  edit: allow
  bash: allow
  webfetch: allow
  todowrite: deny
  question: deny
---

# Sql Expert

Writes and reviews SQL migrations
…
```

### Tool rules (`permission`)

The same rules as opencode: for each tool, the **last** matching rule wins (put `"*"` first).
`allow` or `deny`. `ask` counts as `deny`, because a worker can never stop to ask. A denied call
comes back to the model as an error ("Denied by the amp-… tool policy") and never runs. Workers
can never ask a person questions or dispatch other agents.

MCP tools have their own key. Its patterns are `server/tool`:

```yaml
permission:
  mcp:
    "*": allow
    "github/*": allow
    "github/delete_*": deny
```

No `mcp` key means every MCP server is allowed. A server can also be limited to some roles from
its own side (`harness/mcp-servers.md`).

### Which agents the manager dispatches

The manager runs a worker on a ticket with its `amp_run_ticket` tool. It may dispatch every
built-in worker, and any agent whose file says `dispatch: true`. A `deny` for that agent in the
manager's own `task` rules still wins. `agents list` shows who is dispatchable.

## Skills

```sh
amp-harness skills list                     # where each comes from, which agents preload it
amp-harness skills show amp-execution
amp-harness skills new postgres-style --description "Our Postgres conventions"
amp-harness skills import ~/.claude/skills/pdf      # a Claude Code or opencode skill folder, as is
amp-harness skills edit postgres-style
amp-harness skills remove postgres-style
amp-harness skills reset amp-kb             # drop your overrides of a built-in skill
amp-harness skills validate
```

- **Preloading:** an agent's `skills:` list is put into its prompt in full
  (`agents skills <id> add|remove <skill>`). Workers preload `amp-execution`, reviewers
  `amp-review`. The manager preloads nothing: its prompt goes with every request, so it reads
  `amp-planning` when it plans. Every other skill is listed by name, description and path, and the
  agent reads it when the work calls for it. So a good `--description` says when to use the skill.
- Names are lowercase letters, digits and hyphens.
- **In the chat:** `/skills` and `/skills <name>`.
