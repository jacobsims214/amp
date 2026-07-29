---
name: amp-agent-builder
description: Manager-only skill for creating or updating Claude Code subagents and skills when the existing roster doesn't fit a new domain. Use when a task needs a specialist that doesn't exist yet, or an existing specialist/skill is underperforming.
---

# AMP Agent Builder

This is a manager-only skill. Load it when the live subagent roster (check your `Task` tool
context) has no specialist whose description matches the domain of work you need done, or when
an existing specialist keeps producing bad results for its supposed domain.

---

## When to use it

- **Roster gap**: you need work done in a domain (e.g. Python, mobile, a new cloud provider) and
  nothing in the current roster's descriptions is a good match.
- **Underperformance**: an existing specialist keeps failing acceptance criteria in ways that
  point to a bad model/skill fit rather than a bad ticket — before rebuilding, check whether the
  problem is really the agent (wrong model, missing skill) or just underspecified tickets first.

Do not use this to work around a ticket being too big or too vague — fix the ticket instead. Only
reach for this when the roster itself is the problem.

---

## Where new agents and skills go

The AMP roster ships inside the `amp` plugin, which is read-only — never edit files under
`${CLAUDE_PLUGIN_ROOT}`, because the next plugin update overwrites them. Anything you mint goes
in the project instead:

| What | Path |
|---|---|
| New subagent | `.claude/agents/<name>.md` |
| New skill | `.claude/skills/<name>/SKILL.md` |

Project-level definitions take precedence over plugin ones with the same name, so this is also
how you fix an underperforming shipped agent: copy it out of the plugin, adjust it, save it under
`.claude/agents/` with the same name.

If a change belongs upstream rather than in one project, say so and let the user carry it back
into `v2/.opencode/` and rerun `v2/scripts/build-claude-plugin.sh`.

---

## Agent file shape

```markdown
---
name: amp-worker-python
description: Python specialist — FastAPI, pytest, packaging. Executes one assigned AMP task end-to-end.
model: sonnet
color: green
tools: ["Read", "Glob", "Grep", "Bash", "Edit", "Write", "mcp__amp__amp_get_task"]
---

You are ... (this body becomes the agent's system prompt)
```

- `name`: kebab-case, must match the filename stem
- `description`: one sentence stating exactly when to dispatch this agent — this is what the
  manager's `Task` tool reads to decide who fits a job, so make it specific and keyword-rich
- `model`: `haiku`, `sonnet`, `opus`, or `inherit`
- `tools`: an allowlist. **Omit it entirely** to inherit every tool, which is right for
  implementation specialists whose blast radius is scoped by the ticket. Set it when the role is
  defined by what it must *not* do — reviewers and researchers should not get `Edit`/`Write`.
  Listing it means listing the `mcp__amp__amp_*` tools the agent needs too, since an allowlist
  excludes everything it doesn't name; load the **amp-mcp** skill for the exact names.
- `maxTurns`: the hard ceiling on how many turns the agent gets before the harness kills it
  mid-run. **This is enforced, and it is the most common cause of an agent "returning nothing."**
  It counts *turns*, not tool calls — parallel calls in one turn count once — so an agent that
  batches its calls stretches much further than one working step by step. When an agent is cut
  off it dies wherever it happens to be, which is usually right before `amp_complete_task`: the
  work often landed but the board never records it, and the truncated final text gets surfaced as
  if it were the result. Budget generously. The `amp-execution` protocol alone spends ~9 turns on
  ceremony (load skill, get task, kb search, progress comment, kb write, verify, completion
  comment, complete) before any real work, and discovery in a large repo can eat 15-20 more.
  In the opencode sources this field is `steps: N`; the build script translates it to `maxTurns`.
- There is no `mode`, `hidden`, `temperature`, or `permission` field — those were opencode
  concepts. Claude Code has no path-scoped permission globs; if a role needs a path restriction,
  state it as a rule in the body and back it with a `permissions` entry in
  `.claude/settings.json`.

Nested delegation is structurally impossible: subagents cannot dispatch subagents. Don't design a
role that depends on it.

### Model-selection heuristic

1. `haiku` for templated, low-complexity roles (docs, config swaps, KB maintenance).
2. `sonnet` for implementation and code review — the default choice for real work.
3. `opus` for roles that make judgment calls without a human in the loop, where a wrong judgment
   propagates further than a wrong line of code a reviewer would catch anyway.
4. `inherit` when the role should track whatever the user picked for the session.

---

## Skill file shape

```markdown
---
name: python-engineer
description: Use when writing or reviewing Python — FastAPI handlers, pytest, packaging.
---

# Python Engineer
...
```

Only `name` and `description` are needed. The description is the entire trigger — Claude Code
loads a skill when the description matches the work at hand, so write it as "Use when ..." with
the keywords someone would actually be working with.

---

## Registering a new agent or skill

Agents in `.claude/agents/*.md` are auto-discovered — no registration needed for the agent file
itself to be dispatchable. But **update `amp-index/SKILL.md`** to add a one-line entry for any new
skill you create, so future planning sessions know it exists. (You don't need to add new *agents*
to amp-index's roster listing — the manager discovers the live agent list from its own `Task` tool
context each session, so a stale roster in amp-index doesn't cause a new agent to be missed. But
undocumented skills have no discovery path at all, so those must be added.)

Since amp-index ships in the read-only plugin, adding a skill entry means either copying
`amp-index/SKILL.md` to `.claude/skills/amp-index/SKILL.md` and editing that, or carrying the
change upstream. Prefer upstream when the skill is generally useful.

---

## Restart required — this matters immediately

New or changed agent files are **not hot-reloaded**. The current session already loaded its agent
list at startup — the `Task` tool in this session cannot dispatch to a brand-new agent name until
the user quits and restarts Claude Code. After creating or significantly changing an agent, say so
explicitly and tell the user a restart is needed before the new agent can actually be used.
