---
title: AMP Harness — model providers and keys
tags: [harness, amp-harness, providers, keys, connect, keychain, openrouter, anthropic, openai, custom-endpoint]
---

# Model providers and keys

A provider is where the models run: OpenRouter, Anthropic, OpenAI, or any OpenAI-compatible
endpoint (a company gateway, vLLM, LiteLLM, Ollama, LM Studio). Several can be active at once:
each role picks its own `provider/model` (`harness/models-per-role.md`).

## Add a key: `amp-harness connect`

```sh
amp-harness connect                 # a picker; then the key, typed hidden
amp-harness connect openrouter      # or anthropic, openai
amp-harness connect --list          # every provider, whether it has a key and where it comes from
```

The key is typed into a hidden prompt (nothing is echoed). It is checked with the provider by a
call that spends no tokens, and kept **only** in the OS keychain (service `amp-harness`, account
`provider:<id>`). A rejected key is asked for again. After connecting, `connect` offers to point
the roles at the new provider (`--use` does it without asking).

Scripts pipe the key in on stdin. Never put a key on the command line:

```sh
printf '%s\n' "$KEY" | amp-harness connect openrouter --key-stdin --use
```

**In the chat:** `/connect` lists the providers, and `/connect <provider>` asks for the key in a
hidden field. The running chat uses it at once, and so do workers started after that.

## Environment variables (CI and headless)

A key in the provider's variable wins over the keychain: `OPENROUTER_API_KEY`,
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `<ID>_API_KEY` for a provider you add (for example
`EXAMPLE_API_KEY`). With no terminal, `connect` checks the key in that variable and stores
nothing. Where there is no keychain (`AMP_CREDENTIAL_STORE=file`, a headless Linux box), use the
variables: provider keys never fall back to a file.

Agents never see these values: their shell gets your environment minus every provider key and
AMP token. To hand one to the shell on purpose, list it in `[secrets] pass_to_bash` in your
config.toml.

## Your own endpoint

```sh
amp-harness connect example --base-url api.example.com --name "Example AI" \
  --role manager=orchestrator --role workers=engineer
```

`api.example.com` becomes `https://api.example.com/v1`. The models come from the endpoint's own
list (or `--model ID`, repeatable). Non-secret headers go in `--header NAME=VALUE`. A secret
header (an `Authorization` or `X-Api-Key` value) is refused there, because other processes can
read command arguments. Add it in a terminal instead, where it goes to the keychain. The full
walkthrough, including the first run and org presets, is `harness/enterprise-provider.md`.

To change an endpoint you added, run `connect example` again with just the flags to change (for
example `--ca-file corp.pem` or `--header X-Team=blue`). Only those settings change. Its models,
including ones added with `models set --no-check`, stay, and `connect` prints what changed.

How the endpoint authenticates (`--auth`, or the first run's question):

| `--auth` | sends | also give |
|---|---|---|
| `bearer` (default) | `Authorization: Bearer <key>` | the key |
| `header` | the key in its own header | `--auth-header X-Api-Key` (default `api-key`, Azure's) |
| `none` | nothing | |
| `command` | a token a program prints (SSO helper, Vault, `gcloud`) | `--auth-command "corp-sso token"` |
| `oauth-client-credentials` | a client-credentials token | `--token-url`, `--client-id`, `--scope`; the client secret is asked like a key |
| `azure-entra` | a Microsoft Entra ID token | `--tenant-id` + `--client-id`, or neither to use `az login` |

Other endpoint settings: `--organization` / `--project` (OpenAI's headers), `--query
api-version=2024-10-21` (Azure classic deployments), `--ca-file` (a company CA bundle),
`--client-cert` / `--client-key` (mTLS; `--store-client-key` keeps the key in the keychain) and
`--proxy URL` or `--proxy direct` (default: `HTTPS_PROXY` / `NO_PROXY`). `--api
anthropic-messages` is for an endpoint that speaks Anthropic's API instead of OpenAI's.

## Remove a key: `amp-harness disconnect`

```sh
amp-harness disconnect openrouter
```

This deletes the key from the keychain. It also removes the provider's table from config.toml when
`connect` added it (`--keep-config` keeps it). A table you wrote by hand is never removed, and a
provider is not removed while roles still run on it: move them first (`harness/models-per-role.md`).

## No Claude subscription login

Anthropic allows Claude Free, Pro and Max subscription logins only in its own apps (Claude.ai,
Claude Code, Claude Desktop), not in other tools. So the harness takes only an Anthropic **API
key** from console.anthropic.com, and refuses a subscription token (`sk-ant-oat…`) with that
explanation.

## The keychain on macOS

- The harness creates its own keychain items. Store keys with `amp-harness connect`, not with
  Apple's `security` tool: an item `security` made asks for permission on every read, and
  "Always Allow" doesn't stick.
- After an upgrade or a rebuild, macOS may ask once per item whether the new build may read it.
  Choose "Always Allow".
- With no terminal (agents, scripts, CI) the harness never shows a keychain dialog nobody can
  answer. An item that would need one fails at once with a message naming the item and the
  command to fix it (`connect <id>` for a provider key, `mcp secret`/`mcp login` for an MCP
  server's secret or login, `login` for the AMP login; see `harness/troubleshooting.md`). `AMP_HARNESS_KEYCHAIN_PROMPTS=never` forces that.
