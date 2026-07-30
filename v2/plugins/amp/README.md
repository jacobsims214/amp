# AMP — Claude Code plugin

A manager/worker/reviewer agent team that plans work onto the [AMP](https://amp.simstech.cloud)
board over MCP, dispatches specialists in review-gated waves, and accumulates project knowledge
in the AMP knowledge base.

## Install

```
/plugin marketplace add coulterac/amp
/plugin install amp@amp
```

Then restart Claude Code.

## Use

```
/amp:manager plan the auth refactor
```

`/amp:manager` puts the current session into the manager role: it plans, builds the board, and
dispatches the specialist subagents. It never edits project code itself.

On a repo with no `.amp.json`, ask it to initialise first — it will pick up the `amp-init` skill.

## What's inside

| Component | Contents |
|---|---|
| Command | `/amp:manager` — adopt the manager protocol in the main session |
| Agents | workers (backend/frontend/docs), reviewers, researchers, KB curator |
| Skills | AMP protocol (planning, execution, review, KB, MCP reference) plus engineering skills (Go, React, Docker, testing, git, TFE, UI/UX) |
| MCP | `amp` (board + knowledge base), `context7` (library docs), `chrome-devtools` |

## Pointing at a different AMP server

The `amp` MCP server URL defaults to the hosted instance. Override it per machine:

```
export AMP_MCP_URL=http://localhost:8000/mcp
```

## Regenerating

This directory is generated from `v2/.opencode/` by `v2/scripts/build-claude-plugin.sh`.
Edit the opencode sources and rebuild — direct edits here are overwritten.
