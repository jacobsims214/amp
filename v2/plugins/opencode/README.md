# AMP for opencode

One plugin gives opencode AMP's whole setup:

- **Agents**: the manager (your default agent) and its specialists: backend, frontend and docs
  workers, designer, media, reviewers, researchers, KB curator. Each on the model AMP picked for
  it, with its thinking level and permissions.
- **Skills**: AMP's protocol (planning, execution, review, KB, design systems, image
  generation) and engineering skills.
- **MCP servers**: AMP, context7 (library docs), chrome-devtools (headless, for visual checks).
- **Model settings**: OpenRouter models' input types and thinking variants.
- **`amp_session_usage`**: agents report their own token use on their tickets.

## Install

```bash
opencode plugin add amp-opencode            # from npm (when published)
opencode plugin add file:///path/to/amp/v2/plugins/opencode   # from a checkout
```

Then sign in to AMP the first time the `amp` server connects (OAuth), or set a token.

## Options

In `opencode.json`:

```json
{
  "plugins": [
    { "package": "amp-opencode", "options": {
        "url": "https://amp.example.com/mcp",
        "token": "{env:AMP_TOKEN}",
        "models": "openrouter"
    } }
  ]
}
```

| Option | Default | |
|---|---|---|
| `url` | `$AMP_MCP_URL`, else the hosted AMP | Your AMP's MCP endpoint |
| `token` | `$AMP_TOKEN`, else OAuth | An AMP API token (Agents & tokens in AMP) |
| `models` | `"openrouter"` | `"inherit"`: every agent uses your default model instead |
| `context7` | `true` | Add the context7 MCP server |
| `chromeDevtools` | `true` | Add chrome-devtools MCP (headless) |

Your own agents, skills and MCP servers of the same name win over the plugin's.

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
minutes and returned to ready at 30 instead of being launched correctly, and its workers' runs are
closed out from the manager side more often than they should be. Update with
`opencode plugin add amp-opencode` (or re-add the checkout path) and restart opencode.

## Regenerating

Built from AMP's repo (`v2/.opencode`, `v2/opencode.json`) by `v2/scripts/build-opencode-plugin.sh`,
the same sources as the Claude Code plugin. Edit those, not this directory.
