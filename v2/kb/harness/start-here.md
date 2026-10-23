---
title: AMP Harness — set up, configure and customise
tags: [harness, amp-harness, setup, configuration, customisation, onboarding, start]
---

# AMP Harness

AMP Harness (`amp-harness`) is AMP's own agent harness: one binary for macOS and Linux that
chats with your manager agent, plans on the AMP board and runs worker agents on tickets. It
reports every worker's run (model, tokens, cost, branch, commit) to its ticket by itself.

You can use AMP from any harness that speaks MCP (`setup/connect-your-agent.md`). AMP Harness is
the one built for it: it already knows AMP's agents, skills, approvals and run records, so there
is nothing to wire up.

## The short version

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
```

The installer offers to run `amp-harness setup`. Setup signs you in to AMP, links the folder to a
project and connects a model provider. Then run `amp-harness` in your project folder and talk to
the manager. Until the first release is published, build it from source instead
(`harness/install.md`).

## Pages

| If you want to… | Read |
|---|---|
| install, upgrade or uninstall it, and what the first run asks | `harness/install.md` |
| point it at an AMP server, switch servers, log in | `harness/servers-and-login.md` |
| add a model provider key or your own endpoint | `harness/providers-and-keys.md` |
| choose which model each role (manager, workers, reviewers…) runs on | `harness/models-per-role.md` |
| add or change agents and skills | `harness/agents-and-skills.md` |
| add MCP servers (stdio, http, sse), import them from other tools | `harness/mcp-servers.md` |
| know which file a setting comes from, use another config, reload | `harness/configuration.md` |
| chat, run headless, dispatch workers, approvals, worker limits | `harness/running-work.md` |
| fix something (`doctor`, keychain, TLS, models) | `harness/troubleshooting.md` |
| use your company's own model endpoint, or roll it out to a team | `harness/enterprise-provider.md` |

## Ideas that run through every page

- **No file editing needed.** Every setting has a command (`server`, `connect`, `models`,
  `agents`, `skills`, `mcp`) and most have a chat command too. You may still edit config.toml by
  hand; the commands keep your comments and save a `config.toml.bak` before each change.
- **Secrets never go in a file.** API keys, MCP secrets and AMP logins go in the OS keychain (or
  come from environment variables in CI). Commands refuse a secret given as an argument, because
  other processes can see arguments.
- **Three layers.** The built-in defaults, then your user config (`~/.config/amp-harness`), then
  the repo's `.amp/harness/` (shared with everyone who clones it, limited to safe settings). See
  `harness/configuration.md`.
- **`amp-harness doctor` checks it all** and says how to fix what fails.

## For agents teaching this

Do each step with the person, in their terminal. Run `amp-harness doctor` first and start from
what it says. Never ask them to paste a key into the chat: keys are typed into the hidden prompt
of `amp-harness connect` (or `/connect` in the harness chat), and nowhere else.
