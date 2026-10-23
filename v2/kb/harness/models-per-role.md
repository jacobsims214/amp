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
amp-harness models profile opencode     # the shipped roster on OpenRouter (the default)
amp-harness models profile anthropic    # Anthropic only: Opus for the manager and reviewers, Sonnet for the rest
amp-harness models profile inherit --inherit openrouter/z-ai/glm-5.3   # one model for every role
```

A profile of your own is a `[profiles.<name>]` table in your config.toml. `AMP_HARNESS_PROFILE`
overrides the profile for one shell.

## Map a provider's models to the roles: `connect --role`

When you connect a provider, you can place its models on the roles in one go:

```sh
amp-harness connect example --role manager=orchestrator --role workers=engineer --role reviewers=orchestrator
```

- ROLE is a group (`manager`, `workers`, `reviewers`, `helpers`), `all`, or one role's name.
  MODEL is one of the provider's models. Both are checked before anything is saved.
- Roles you don't name keep their model if it is already on this provider. Otherwise they go to
  the workers' model (or the `all` model). Either way, every role ends up on this provider.
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
