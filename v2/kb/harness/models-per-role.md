---
title: AMP Harness — which model each role runs on
tags: [harness, amp-harness, models, roles, profiles, thinking, model-per-role]
---

# Which model each role runs on

Every agent is a **role**, and every role runs on its own `provider/model` with its own thinking
level. A role's name is its agent id without `amp-`:

| group | roles |
|---|---|
| `manager` | manager: plans the work and dispatches it (the orchestrator) |
| `workers` | worker-backend, worker-frontend, worker-docs, designer |
| `reviewers` | reviewer, github-reviewer |
| `helpers` | researcher, media, github-researcher, kb-curator |

Agents you add are roles too (`harness/agents-and-skills.md`).

## See them

```sh
amp-harness models list               # each role: provider/model, thinking, ready, and which setting chose it
amp-harness models list --available   # every model your providers offer
amp-harness models list --check       # also ask each provider whether the models exist (network)
```

**In the chat:** `/model roles` lists every role. `/model <role>` opens a picker: Enter uses the
model in this chat only, `s` saves it.

## The manager's model

The manager plans, keeps the board and runs the workers. It is the role you talk to, so it needs
a model that follows instructions, calls tools correctly and doesn't think for minutes before
acting. The shipped default is OpenRouter's `openai/gpt-6.1-sol` at `low` thinking; the workers
stay on cheaper models.

What the manager eval (#2363) measured on four tasks, against a test AMP:

| manager model | new project, plan, status, run | about a session like that |
|---|---|---|
| `openrouter/openai/gpt-6.1-sol` (low, default) | all pass; planning a new project ~2 min | $0.55 |
| Claude Sonnet 5.5 via OpenRouter's Messages endpoint (low) | all pass, fewest tool errors; planning ~45 s | $0.60–0.75 |
| `openrouter/z-ai/glm-5.3` (low) | passes most runs, but sometimes repeats a broken call | $0.10 |
| `openrouter/xiaomi/mimo-v2.6-pro`, reasoning off | passes, slow (up to 85 s of silent thinking) | $0.05 |
| `openrouter/xiaomi/mimo-v2.6-pro`, reasoning on (the old default) | thinks for minutes; never finished a plan in 8 min | $0.05 |

To run the manager on Claude through your OpenRouter key with prompt caching, add OpenRouter's
Anthropic-compatible endpoint as a second provider and give it the same key
(`amp-harness connect openrouter-messages`, or `OPENROUTER_API_KEY` in the environment):

```toml
# ~/.config/amp-harness/config.toml
[providers.openrouter-messages]
api = "anthropic-messages"
base_url = "https://openrouter.ai/api"
key_env = "OPENROUTER_API_KEY"

[providers.openrouter-messages.models."anthropic/claude-sonnet-5.5"]
reasoning = true
context_window = 1000000
max_tokens = 64000
cost = { input = 2.0, output = 10.0, cache_read = 0.1, cache_write = 2.5 }
unsupported_thinking = ["off"]
compat = { force_adaptive_thinking = true, supports_temperature = false }

[roles.manager]
model = "openrouter-messages/anthropic/claude-sonnet-5.5"
thinking = "low"
```

Through OpenRouter's usual (chat) endpoint Claude isn't cached, so every manager step pays for
the whole prompt again; use the Messages endpoint above, or an Anthropic key (`models profile
anthropic`).

## Change one role

```sh
amp-harness models set reviewer anthropic/claude-opus-5-5 --thinking high
amp-harness models reset reviewer      # back to what the layer below says
```

`models set` refuses a provider you don't have, and asks the provider's live list whether the
model exists (`--no-check` skips that). A model your config doesn't list yet is added for you.
Thinking levels: `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`.

`--repo` writes the repo's `.amp/harness/config.toml` instead of your own config, so everyone
working in the repo gets it (`harness/configuration.md`).

In the chat: `/model <role> <provider/model> [thinking]` for this chat, add `--save` to keep
it, and `/model <role> --reset` drops a chat-only choice. Plain `/model provider/model` switches
only the model of the chat you're in.

## Change them all at once: profiles

```sh
amp-harness models profile opencode     # the shipped roster on OpenRouter (the default; manager on GPT-6.1 Sol)
amp-harness models profile anthropic    # Anthropic only: Opus for the manager and reviewers, Sonnet for the rest
amp-harness models profile inherit --inherit openrouter/z-ai/glm-5.3   # one model for every role
```

A profile of your own is a `[profiles.<name>]` table in your config.toml. `AMP_HARNESS_PROFILE`
overrides the profile for one shell.

## Map a provider's models to the roles: `connect --role`

When you connect a provider, you can place its models on the roles in one go:

```sh
amp-harness connect example --role manager=orchestrator --role workers=engineer
```

- ROLE is a group (`manager`, `workers`, `reviewers`, `helpers`), `all`, or one role's name.
  MODEL is one of the provider's models. Both are checked before anything is saved.
- Groups you don't name follow the same rule as the first run (below). The reviewers follow the
  manager and the helpers follow the workers, unless a model's name points at them (`review`,
  or `mini`/`fast`/`flash`). So the command above also puts the reviewers on `orchestrator` and
  the helpers on `engineer`.
- Naming only some roles changes only those. For example, `--role reviewers=engineer` leaves
  the rest where they are. A role still on another provider moves to its group's model, so
  every role ends up on this provider. `all` names every role.
- A provider that already has its key isn't asked for it again, so the same command remaps the
  roles any time. In the chat: `/connect example manager=orchestrator workers=engineer`.

The first run's own-endpoint path does the same mapping by matching names. A model called
`orchestrator`, `planner` or `lead` goes to the manager, and `engineer` or `coder` to the
workers. Reviewers get a `review` model, else the manager's. Helpers get a `mini`, `fast` or
`flash` model, else the workers'.

## How a role's model is chosen

From lowest to highest: the shipped defaults, then the profile (or the `inherit` model), then
your config's `[roles.<role>]`, then the repo's `[roles.<role>]`. `models list` shows which one
won, in its SOURCE column. A role with no model of its own (a new agent) runs on the
`[models] fallback` (default: worker-backend's model).

What `connect --role` writes, for example:

```toml
[models]
profile = "inherit"
inherit = "example/engineer"     # every role not listed below, and agents added later

[roles.manager]
model = "example/orchestrator"

[roles.reviewer]
model = "example/orchestrator"

[roles.github-reviewer]
model = "example/orchestrator"
```

Changes reach a running chat without a restart: workers dispatched afterwards use the new
models (`harness/configuration.md`, "Reload").
