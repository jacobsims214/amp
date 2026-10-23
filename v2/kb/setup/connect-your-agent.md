---
title: Connect your agent harness to AMP
tags: [setup, harness, mcp, tokens, claude-code, opencode, amp-harness, onboarding]
---

# Connect your agent harness to AMP

Your agent talks to AMP over MCP (Streamable HTTP, the `/mcp` endpoint of your AMP server) with
a token that acts as you.

## 1. Get a token

In the UI open **Agents & tokens** and create a token (give it a name and an expiry). It's shown
once. The page also shows the exact setup line for Claude Code and the JSON for other clients.
Tokens act as you: your agent can do what you can, and its actions are recorded as yours. For an
unattended agent (CI, a bot), create an **agent account** on the same page and give it its own
token instead of using yours.

## 2. Add the AMP MCP server

**Claude Code**

```
claude mcp add --transport http amp https://<your-amp>/mcp --header "Authorization: Bearer <token>"
```

or install the AMP plugin, which adds the manager command (`/amp:manager`), the worker agents and
skills, and the MCP servers. From the AMP repo run `v2/scripts/setup-claude-code.sh`; for a
self-hosted server set `AMP_MCP_URL`.

**opencode**

From the AMP repo, run `v2/scripts/setup-opencode.sh`. It adds the AMP opencode plugin (agents,
skills, MCP servers and model settings) as one `plugins` entry in your global
`~/.config/opencode/opencode.json` and touches nothing else in it. For a self-hosted server pass
`--url https://<your-amp>/mcp` (default `$AMP_MCP_URL`); pass `--token <token>`, or omit it to
sign in with OAuth. `--inherit-models` runs every agent on your own default model. If you used
the old `sync-global-config.sh`, rerun with `--clean` to remove the files it copied.

**AMP Harness** (`amp-harness`, AMP's own harness)

Nothing to wire up: `amp-harness setup` signs you in to AMP in the browser, links the folder to
a project and connects a model provider, and the manager, workers and skills are built in. Start
with `harness/start-here.md` (install: `harness/install.md`; your company's own model endpoint:
`harness/enterprise-provider.md`).

**Anything else** that speaks MCP: point it at `https://<your-amp>/mcp` with
`Authorization: Bearer <token>`.

Check it works: ask your agent to call `amp_whoami`. It should name you and your orgs and teams.

## 3. Settings that prevent most trouble

Learned the hard way in team trials (see `field-notes.md`):

- **Workers must never hit a permission prompt.** A subagent running headless can't answer one
  and waits forever, and the ticket goes quiet. Keep workers inside the project directory:
  parallel worktrees go *inside* the repo (`.worktrees/<ticket>`, gitignored), not in sibling
  folders. If your harness asks before touching files outside the project, that's the usual
  trigger.
- **Generous MCP and model timeouts.** In opencode: `mcp.amp.timeout` 60000, and for a provider
  `options.chunkTimeout` (e.g. 120000) and `options.headerTimeout` (e.g. 60000).
- **Let agents report their usage.** opencode: the AMP opencode plugin (installed by
  `setup-opencode.sh`) gives workers an `amp_session_usage` tool (model, tokens, cost) to put
  in their run record. Claude Code: the manager reads a subagent's token usage from its result
  and records it. Without this, spend reports are a floor.
- **Restart after AMP restarts.** If the `amp_*` tools vanish or fail to connect, restart the
  harness so it reconnects. Agents should stop and say so rather than work around it.
- **Sandbox credentials.** Workers don't need your cloud, cluster or GitHub admin credentials to
  do a ticket. Give them the least you can.
- **Don't let agents start host services.** A worker running `docker compose up` on your machine
  adds load you didn't plan for. Run dev stacks in the project's own sandbox.

## 4. The agents

The AMP template ships a **manager** (plans, dispatches, never implements), **workers** by
domain (backend, frontend, docs), a **designer** (visual design, mockups, standalone pages and
diagrams), a **media** specialist (reads images, video and audio; makes images), **reviewers**,
**researchers** and a **KB curator**, plus skills for planning, execution, review, day reports,
image generation and the KB. The manager is the one you talk to.

In opencode each agent runs on an OpenRouter model chosen for it, open-weight first, with its
input types and thinking level set in `opencode.json`; `v2/docs/opencode-models.md` in the AMP
repo has the table and the reasons. In Claude Code the agents use your session's model.
