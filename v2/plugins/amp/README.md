# AMP — Claude Code plugin

A manager/worker/reviewer agent team that plans work onto the [AMP](https://amp.simstech.cloud)
board over MCP, dispatches specialists in review-gated waves, and accumulates project knowledge
in the AMP knowledge base.

## Install

```
/plugin marketplace add jacobsims214/amp
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

## Updating: dispatch and completion changed, please update

**Please update the plugin when you update AMP.** AMP made dispatching and starting two separate
steps in the server, and this release of the plugin is the first that knows it. `amp_dispatch_task`
now marks a ticket dispatched but **not started**, the worker starts it with
`amp_claim_task(task_id, dispatch_id)`, and a ticket nobody starts is flagged after 10 minutes and
returned to ready after 30. This release teaches the manager and workers that flow: the manager
launches the worker with the `worker_prompt` dispatch returns, the worker claims first, and a
manager only "closes out" a ticket its worker couldn't finish.

An older plugin keeps working against a new server — AMP has a fallback, so with no claim the
worker's first comment, run, block or completion starts the ticket (`started_by: "inferred"`) and
nothing is flagged or returned. What an older plugin misses is the rest: its manager never sees
`worker_prompt` or `next`, so a ticket it dispatches and doesn't start by hand is flagged at 10
minutes and returned to ready at 30 instead of being launched correctly. Update with
`/plugin update amp@amp` and restart Claude Code.

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
