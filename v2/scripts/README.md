# AMP v2 setup scripts

AMP's agent roster lives once, in `v2/.opencode/`, and is published to two hosts.

| Host | Script | What it does |
|---|---|---|
| Claude Code | `setup-claude-code.sh` | Installs the `amp` plugin from this repo |
| opencode | `sync-global-config.sh` | Copies agents/skills into `~/.config/opencode` and merges `opencode.json` |

## Claude Code

```
./scripts/setup-claude-code.sh
```

Registers this checkout as a plugin marketplace and installs the `amp` plugin (agents, skills,
`/amp:manager`, and the three MCP servers). Restart Claude Code afterwards, then:

```
/amp:manager plan <what you want built>
```

Anyone who just wants to use AMP without cloning can skip the script entirely:

```
/plugin marketplace add coulterac/amp
/plugin install amp@amp
```

Point at a self-hosted AMP server with `export AMP_MCP_URL=http://localhost:8000/mcp`.

## Rebuilding the plugin

`v2/plugins/amp/` is **generated output**, committed so users can install without a build step.
It is wiped and rebuilt by:

```
./scripts/build-claude-plugin.sh
```

Edit `v2/.opencode/` and rebuild — direct edits to `v2/plugins/amp/` are overwritten. The
conversion lives in `scripts/lib/opencode_to_claude.py`; its `MAPPING NOTES` header is the
opencode → Claude Code translation table.

### What the conversion does

| opencode | Claude Code |
|---|---|
| `mode: primary` | a slash command (`/amp:manager`) — Claude Code's "primary agent" is just the main session |
| `mode: subagent` | `agents/<name>.md` |
| `model: openrouter/...` | `haiku` / `sonnet` / `opus` (see `MODEL_MAP`) |
| `permission: <key>: deny` | `disallowedTools: [...]` |
| `permission` path/command globs | `Edit(.claude/**)`, `Bash(git status:*)` — commands only |
| `steps: N` | `maxTurns: N` |
| "Load the **x** skill first" | `skills: [x]`, preloaded at spawn |
| `edit`, `webfetch`, `skill("x")` in prose | Claude Code tool vocabulary |

### Permissions, specifically

opencode's `permission` block is **deny-based** — it names what an agent must not have and leaves
everything else alone. `disallowedTools` has exactly those semantics, so that is what the agents
get. An allowlist would be the wrong shape: it silently strips every tool nobody thought to
enumerate (`Skill`, `TodoWrite`, the whole amp MCP surface) and freezes the roster against tools
Claude Code adds later.

Scoped globs are the one thing that does not survive intact, and only for agents:

- **Commands keep them.** `/amp:manager` carries the manager's globs as real
  `allowed-tools: Bash(git status:*), Edit(.claude/**), ...` plus `disallowed-tools`. Note that
  `allowed-tools` is a *grant* list, not a cap — it pre-approves those calls without blocking
  others. The denials are enforced.
- **Plugin agents cannot.** Two independent reasons, both verified against Claude Code 2.1.220:
  a `tools:` entry is matched on bare tool name and its `(...)` specifier is discarded (only the
  Agent tool reads one, for `allowedAgentTypes`), and `permissionMode` is explicitly ignored for
  plugin agents — the loader warns *"Use .claude/agents/ for this level of control."*

So for a scoped subagent the converter restates the rule in the prompt and points at
`.claude/settings.json`. Hard enforcement means a `permissions.allow`/`permissions.deny` pair
there, or moving that one agent out of the plugin into `.claude/agents/`.

For reference, the fields a **plugin** agent actually honours: `name`, `description` /
`when_to_use`, `tools`, `disallowedTools`, `skills`, `model`, `color`, `maxTurns`, `memory`,
`effort`, `isolation`, `background`. It ignores `permissionMode`, `hooks`, and `mcpServers`.

Sources that document opencode's *own* schema can't be search-and-replaced into sense. Those are
hand-maintained in `scripts/lib/overrides/`, which is copied over the generated tree last.
`amp-agent-builder` is the current example.
