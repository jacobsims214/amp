---
title: AMP Harness — AMP servers and login
tags: [harness, amp-harness, server, login, oauth, token, ca, tls]
---

# AMP servers and login

The harness talks to one AMP server at a time over MCP. You can save several (hosted, a local
one, your company's) and switch between them. Each keeps its own login.

## Save and switch servers

```sh
amp-harness server                                   # the server in effect, where each setting comes from, the saved ones
amp-harness server set amp.example.com               # check it, save it, make it active
amp-harness server set localhost:18000 --name local  # localhost gets http, anything else https
amp-harness server use local                         # switch to a saved server
amp-harness server remove local
```

- `set` takes `host`, `host:port` or a full URL, and adds `/mcp` when there is no path. It
  connects first: a server that doesn't answer is never saved. If it needs a login, `set` offers
  the browser login straight away (`--yes` never asks, and prints the command instead).
- The web UI address is found by itself. Pass `--web-url` only when the UI is somewhere no probe
  finds it.
- A private certificate authority: `--ca-file company-ca.pem`. A forced protocol (rarely needed):
  `--protocol 2026-07-28` or `2025-11-25`.
- Names default to `hosted`, `local` or the host's first label.

**In the chat:** `/server` lists the saved servers and `/server <name>` (or `/server <address>`,
which saves it too) switches the session without a restart. It is refused while a reply or a
worker is running.

For one command or one shell, `--url URL` or `AMP_MCP_URL` still wins over the saved server.

## Log in

```sh
amp-harness login                  # the active server
amp-harness login --url https://amp.example.com/mcp [--no-browser]
amp-harness logout
```

`login` opens AMP's sign-in page in your browser (OAuth). `--no-browser` prints the link instead,
for a remote machine. The login is kept in the OS keychain (service `amp-harness`, one item per
server). Where there is no keychain, it goes to `~/.config/amp-harness/credentials.json` (mode
0600). `AMP_CREDENTIAL_STORE=keychain|file|auto` forces one.

Logged out while the chat is open? Run `amp-harness login` in another terminal. The chat
reconnects within about 15 seconds, without a restart.

## A token instead of a login (CI, bots)

Create a token in AMP's web UI under **Agents & tokens** (for a bot, create an agent account
first) and put it in `AMP_TOKEN`. It is used instead of the stored login, held in memory only,
never printed, and hidden from the agents' shell. `--token-env OTHER_VAR` reads it from another
variable.

## When AMP can't be reached

The harness says what went wrong in one sentence (connection refused, host not found, untrusted
certificate, needs login, wrong path) and what to run next. The chat opens anyway, marked
`(not connected)`. It retries in the background, and the manager gets its AMP tools once it
connects. `--verbose` adds the full cause. More fixes: `harness/troubleshooting.md`.

## Where it is saved

`server set` and `server use` write `[server]` (the server in effect) and `[servers.<name>]` in
your config.toml. They keep your comments and save a `config.toml.bak` first:

```toml
[server]
name = "amp"
url = "https://amp.example.com/mcp"

[servers.amp]
url = "https://amp.example.com/mcp"
```

A repo's `.amp/harness/config.toml` can't set the server, so a cloned repo can never point your
harness somewhere else. `.amp.json` only names the folder's project.
