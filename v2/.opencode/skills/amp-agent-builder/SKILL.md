---
name: amp-agent-builder
description: Manager-only skill for creating or updating opencode subagents and skills when the existing roster doesn't fit a new domain. Use when a task needs a specialist that doesn't exist yet, or an existing specialist/skill is underperforming.
---

# AMP Agent Builder

This is a manager-only skill. Load it when the live subagent roster (check your `task` tool
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

## Agent file shape

New agents live at `.opencode/agent/<name>.md`. Frontmatter fields (unknown fields are silently
routed into `options`, so stick to these):

```
name, model, variant, description, mode, hidden, color, steps, options, permission,
disable, temperature, top_p
```

- `mode`: `"subagent"` for a specialist dispatched by the manager, `"primary"` only for a
  manager-role agent
- `hidden: true` on every subagent so it doesn't clutter the `@` autocomplete menu
- `description`: one sentence stating exactly when to dispatch this agent — this is what the
  manager's `task` tool reads to decide who fits a job, so make it specific and keyword-rich
- `permission`: scope `edit`/`bash`/`webfetch` to exactly what the role needs — deny by default,
  allow only what's required (see the existing roster for the pattern: reviewers/researchers deny
  edit, docs specialists get full access since their blast radius is small)
- The frontmatter is followed by the prompt body in markdown — this becomes the agent's system
  prompt. Do not also put `prompt:` in the frontmatter.

See the built-in `customize-opencode` skill for the full authoritative schema if any field's
exact shape is unclear — it's more complete than this summary.

### Steps sizing

Set `steps` deliberately — don't leave it unset and inherit whatever the global default is.
Starting points, adjust after observing real runs:

| Role shape | steps |
|---|---|
| Templated, low-complexity (docs, config swaps) | 10 |
| Investigation + edit + verify cycles | 15-20 |
| Primary agent driving many sequential tool calls per turn | 30 |

### Model-selection heuristic

1. Dispatch a research subagent to check current model pricing/availability before picking —
   don't rely on memorized prices, they go stale within months.
2. Prefer open-weight models for implementation roles (coding, docs) — cheaper, and quality is
   usually sufficient for well-scoped tickets.
3. Reserve a stronger reasoning-oriented model for roles that make judgment calls without a human
   in the loop — review and research — since a wrong judgment there propagates further than a
   wrong line of code a reviewer would catch anyway.
4. Match temperature to the role: low (0.1) for deterministic implementation, moderate (0.2) for
   roles that weigh tradeoffs (review, research, planning).

---

## Registering a new agent or skill

Agents are auto-discovered from `.opencode/agent/*.md` — no registration needed for the agent
file itself to be dispatchable. But **update `amp-index/SKILL.md`** to add a one-line entry for
any new skill you create, so future planning sessions know it exists. (You don't need to add new
*agents* to amp-index's roster listing — the manager should be discovering the live agent list
from its own `task` tool context each session, not from a skill file, so a stale roster in
amp-index doesn't cause the manager to miss a new agent. But undocumented skills have no
discovery path at all, so those must be added.)

---

## Restart required — this matters immediately

New or changed `.opencode/agent/*.md` files are **not hot-reloaded**. The current opencode
session already loaded its agent list at startup — the `task` tool in this session cannot
dispatch to a brand-new agent name until the user quits and restarts opencode. After creating or
significantly changing an agent, say so explicitly and tell the user a restart is needed before
the new agent can actually be used.
