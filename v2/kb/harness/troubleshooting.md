---
title: AMP Harness — troubleshooting with doctor
tags: [harness, amp-harness, troubleshooting, doctor, keychain, tls, models, errors]
---

# Troubleshooting AMP Harness

## Start with `doctor`

```sh
amp-harness doctor            # everything, with a fix under anything that isn't ok
amp-harness doctor --offline  # don't ask the model providers for their model lists
amp-harness doctor --json     # for scripts and for an agent helping you
```

It prints the server, how you're signed in, each provider with its endpoint and key source, and
every role's `provider/model`. Then one line per check:

| check | looks at |
|---|---|
| `config`, `credentials`, `ca`, `connect`, `versions` | the AMP server setting, where logins are kept, TLS, the connection |
| `tools`, `contract`, `whoami`, `project` | AMP's tools, who you are, the folder's `.amp.json` project |
| `roles`, `fallback` | every role resolves to a model; what a role with no model of its own runs on |
| `key:<provider>`, `models:<provider>` | the key is there; the endpoint answers with it and lists every model a role uses |
| `mcp`, `mcp:<server>` | each MCP server: health, trust, how it authenticates, which roles may use it |
| `skills`, `agents`, `config-check` | your agents and skills load; a repo's untrusted ones are `not trusted, not used` (a warning); every problem `config check` finds |

It exits 1 when a check fails (warnings don't count). It never prints a key or token. Only
where a secret comes from.

## Common problems

**"Can't reach AMP at …"**: the server is down, the address is wrong, or the network is in the
way. Check it with `amp-harness server` and switch with `amp-harness server set <url>`.
`--verbose` shows the full cause. The chat still opens and reconnects by itself.

**"needs you to log in" / "AMP login required"**: run `amp-harness login`. An open chat
reconnects within about 15 seconds.

**"Can't connect securely"**: your AMP uses a private certificate authority. Save it with the
server: `amp-harness server set <url> --ca-file company-ca.pem`.

**A role "has no key" / a run fails before it starts**: the role's provider has no key. Run
`amp-harness connect <provider>` (or set its `*_API_KEY` variable), or move the role with
`amp-harness models set`. `amp-harness models list` shows which roles are ready.

**A model "is not listed" / "not found"**: the provider doesn't offer that id. See what it does
offer with `amp-harness models list --available --check`, and check every role with
`amp-harness config check --live`. If the endpoint serves a model it doesn't list,
`amp-harness models set <role> <provider>/<model> --no-check` sets it anyway and adds it to your
config.

**Your company endpoint fails TLS, auth or the proxy**: see the troubleshooting section of
`harness/enterprise-provider.md`.

**macOS keeps asking for keychain access**:

- Once per item after an upgrade or rebuild is expected. Choose "Always Allow".
- Asking on every read means the item was made by Apple's `security` tool. Store the key again
  with `amp-harness connect <provider>` (or `amp-harness login`), and the harness will own the item.
- In scripts and for agents, dialogs are switched off. The command fails at once and names the
  item and the fix. `AMP_HARNESS_KEYCHAIN_TRACE=<file>` logs every keychain call (never a
  secret).

**No keychain (Linux server, container)**: logins go to `~/.config/amp-harness/credentials.json`
(mode 0600). Provider keys must come from environment variables (`OPENROUTER_API_KEY`,
`EXAMPLE_API_KEY`…).

**An MCP server is missing or failing**: run `amp-harness mcp test <name>`, and see
`harness/mcp-servers.md`.

**"needs a terminal"**: the chat needs one, and exits with code 2 without it. Use
`amp-harness run -p "…"` in scripts.

**A worker went quiet or stopped**: the harness reports it on the ticket as abandoned, with the
reason (timeout, turn budget, error). Raise the limits in `[workers]` if the work needs them
(`harness/running-work.md`). Then recover the ticket as in `troubleshooting.md`.

**A setting doesn't take effect**: `amp-harness config check` names the file and key at fault.
`/config` in the chat shows where each value comes from. A repo's `.amp/harness/config.toml` may
set the role differently: `models list` shows `repo …` in its SOURCE column.
