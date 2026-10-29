---
title: Connect your agent harness to AMP
tags: [setup, harness, mcp, tokens, claude-code, opencode, amp-harness, onboarding]
---

# Connect your agent harness to AMP

Your agent talks to AMP over MCP (Streamable HTTP, the `/mcp` endpoint of your AMP server) and
acts as you. There are two ways in:

- **AMP Harness (`amp-harness`), the default.** AMP's own harness. It signs in to AMP in the
  browser, and the manager, workers, skills, approvals and run reporting are built in. Nothing to
  wire up. Use it unless your team already works in another harness.
- **Any other harness that speaks MCP** (Claude Code, opencode and others) with an AMP token:
  sections 1 and 2 below.

## AMP Harness (the default)

One binary for macOS and Linux. Install it, then run setup in your project folder:

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
cd ~/code/my-project
amp-harness setup        # the installer offers to run this for you
amp-harness              # chat with the manager
```

Until the first harness release is published, the one-liner stops with "no harness-v* release
found"; build it from source as `harness/install.md` describes.

`amp-harness setup` asks only for what isn't set up yet:

1. **The AMP server**: `amp.example.com`, `host:port` or a full URL. It is checked before it is
   saved. Switch later with `amp-harness server set` and `amp-harness server use`.
2. **Your login**: AMP's sign-in page opens in your browser. The login is kept in your OS
   keychain, one per server. No token to copy. For CI or a bot, put a token in `AMP_TOKEN`
   instead (section 1).
3. **The project**: setup links this folder to an AMP project, existing or new, and writes
   `.amp.json`. A folder below another folder's `.amp.json` is asked about rather than silently
   using the parent's project.
4. **A model provider**: OpenRouter (the shipped roles), an Anthropic or OpenAI API key, or your
   company's own endpoint. The key is typed hidden and kept in the keychain. The manager runs on
   OpenRouter's `openai/gpt-6.1-sol` by default; each role's model can be changed
   (`harness/models-per-role.md`).

Then check everything with `amp-harness doctor`: every check prints ok, warn, fail or skip, with
the fix for anything that isn't ok.

Where to go next:

- start here: `harness/start-here.md`; install, upgrade and the first run: `harness/install.md`;
- servers and login: `harness/servers-and-login.md`;
- keys and providers: `harness/providers-and-keys.md`; your company's own model endpoint:
  `harness/enterprise-provider.md`;
- running work, workers and approvals: `harness/running-work.md`;
- working without git (business or ops work in a plain folder):
  `harness/git-and-the-harness.md`;
- fixing problems: `harness/troubleshooting.md`.

AMP Harness already does what section 3 asks of other harnesses: workers never wait on a
permission prompt, every worker gets a folder or git worktree of its own, and every worker's
model, tokens, cost and branch are reported to its ticket by the harness itself.

## 1. Get a token (other harnesses)

In the UI open **Agents & tokens** and create a token (give it a name and an expiry). It's shown
once. The page also shows the exact setup line for Claude Code and the JSON for other clients.
Tokens act as you: your agent can do what you can, and its actions are recorded as yours. For an
unattended agent (CI, a bot), create an **agent account** on the same page and give it its own
token instead of using yours.

## 2. Add the AMP MCP server (other harnesses)

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

**Anything else** that speaks MCP: point it at `https://<your-amp>/mcp` with
`Authorization: Bearer <token>`.

Check it works: ask your agent to call `amp_whoami`. It should name you and your orgs and teams.

**Keep the plugin up to date.** AMP's plugins carry the manager and worker instructions, and the
instructions changed when dispatching did: a dispatched ticket is no longer started until its
worker claims it, so the manager has to launch the worker with the `worker_prompt` dispatch
returns, and the worker's first call is `amp_claim_task(task_id, dispatch_id)`. An older plugin
still works — AMP's fallback starts a ticket on its worker's first write — but its manager never
sees `worker_prompt`/`next` and simply stops after dispatching, so the tickets it dispatches are
flagged at 10 minutes and returned to ready at 30. Update with `claude plugin update amp@amp`
(Claude Code) or `opencode plugin add amp-opencode` (opencode), and restart the harness. The
plugin's README has the same note.

## 3. Settings that prevent most trouble (other harnesses)

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

In AMP Harness each role runs on its own model: by default the manager on OpenRouter's
`openai/gpt-6.1-sol` (low thinking) and the others on open-weight OpenRouter models, or every role
on Anthropic or on your own endpoint (`harness/models-per-role.md`). In opencode each agent runs
on an OpenRouter model chosen for it, open-weight first, with its input types and thinking level
set in `opencode.json`; `v2/docs/opencode-models.md` in the AMP repo has the table and the
reasons. In Claude Code the agents use your session's model.
