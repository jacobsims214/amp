---
title: AMP Harness — remote control from the web or a phone
tags: [harness, amp-harness, remote, remote-control, pairing, phone, sessions]
---

# Remote control: keep talking to your harness chat from the AMP web app

Type `/remote` in a running harness chat, pair your phone or another computer, and walk away.
From the AMP web app you see the chat as it happens and can send messages and press Stop. The
laptop still does all the work (shell, files, workers); AMP only carries the messages.

Remote control is off until you turn it on, every time, and only you can use it: the same AMP
user, from a device you approved at the laptop.

## Before you start

- **Your own AMP login.** Run `amp-harness login` (`harness/servers-and-login.md`). An `amp_` API
  token (`AMP_TOKEN`) can't turn remote control on, and neither can an AMP server with sign-in
  turned off (local docker compose).
- **A project.** Run the chat in a folder linked to an AMP project (`.amp.json`).
- **Allowed for your org.** An org admin turns remote control on in the org settings; it is off
  by default. A self-hosted AMP also needs the relay switched on (`relay.enabled` in Helm).
- **The AMP web address, exactly.** Open the pairing link in a browser signed in to that same
  address. Locally that is `https://amp.local`, not `https://amp.local:8443` or `localhost`:
  a browser signed in at another address has another login and can't pair.

## Turn it on

In the chat:

```
/remote
```

The harness registers the chat with AMP and prints:

- **when it ends:** by default only when you end it (it has no time limit); if your org set an
  idle limit or a maximum length, the time it ends;
- **a pairing link**, and under it a **QR code** to scan with your phone. The QR code needs a
  window of about 60 columns by 40 rows; in a smaller one (80x24) you get the link alone, so
  enlarge the window or copy the link;
- this laptop's key fingerprint (`29CA-E6B5-…`).

The link is **copied to your clipboard** as it is shown: the chat says "Pairing link copied to the
clipboard." Paste it into a message to your phone, or into a browser on another computer.

### Getting the link out of the chat

| You want to | Do this |
|---|---|
| copy it (again) | `/remote copy` |
| open it in this computer's browser | `/remote open` (uses `open` on macOS, `xdg-open` on Linux) |
| click it | Cmd-click (macOS) or Ctrl-click it, in terminals that show links (iTerm2, kitty, WezTerm, recent GNOME Terminal); Terminal.app doesn't |
| select it yourself | drag across it holding **Option** (iTerm2) or **Fn** (Terminal.app); see below |

How the copy works: the chat sends the link to the terminal's clipboard (OSC 52, which also
works over SSH where the terminal allows it: iTerm2 needs "Applications in terminal may access
clipboard" on, tmux needs `set -g set-clipboard on`) and runs the system's tool when there is
one: `pbcopy` on macOS, `wl-copy` under Wayland, `xclip` or `xsel` under X11. If none of them
works, the chat says so; use `/remote open` or select it. The link is never written to a log.

Selecting it: the chat reports the mouse to itself (for the wheel and clicking workers), so a
plain drag doesn't select. Hold Option (iTerm2) or Fn (Terminal.app) while you drag, or set
`[tui] mouse = false` in your config.toml (`harness/configuration.md`) for plain click-drag
selection. The link is drawn from the left edge of the window, so selecting from its first
character to its last copies a working link: the line breaks inside it are dropped when you paste
it into a browser's address bar.

`/remote open` opens it on *this* computer: useful to pair the AMP web app in this computer's
browser. The link works once, so open it where you mean to pair.

The footer shows the state at its left: `REMOTE ON · 0 devices` (devices watching right now),
`REMOTE OFFLINE · reconnecting` while the connection to AMP is down, and `· 2 queued` while
messages from the web wait for the current reply to finish.

## Pair a device (once per device, for 30 days)

1. Open the link (or scan the QR code) on the device, signed in to AMP as you.
2. The page shows a code, for example `4821-7730`. The laptop asks:
   `Allow "Safari on iPhone" (you@example.com) to control this session? code 4821-7730 [y/N]`
3. Check that the two codes match, then press `y` **on the laptop**. Any other key, Enter
   included, refuses; so does no answer before the link expires. The question can only be
   answered at the laptop's keyboard: nothing sent from the web can answer it.

The link works once, for 5 minutes. `/remote pair` makes a new one (the old one stops working),
and copies it too. Once a link is used or expired, `/remote copy` says so: run `/remote pair`.
A paired device stays trusted for 30 days (your org may set it shorter); after that, pair again.
Pair your phone before you leave the laptop: pairing always needs the laptop.

## What the web can and can't do

From the session page (My Work → My sessions, or the link) you can:

- read the chat live: your messages, the replies, folded thinking, tool calls and their results
  (shortened), and the worker chips;
- send a message. On the laptop it shows tagged `[remote: Safari on iPhone]`. If a reply is still
  running, it is **queued** and runs when the reply ends (up to 5 wait); the laptop shows
  `[remote: …] queued #1: "…"`;
- press **Stop** (stops the reply, its workers, and drops the queue);
- **Disconnect** (ends remote control).

You can't, from the web: run slash commands (a message starting with `/` is sent as text),
connect providers or keys, trust MCP servers or repo items, change configuration or the AMP
server, open URLs on the laptop, pair another device, extend the timeouts, or approve plans
(that stays on the Flight plan page in AMP).

Secrets the harness knows (provider keys, tokens) are blanked out of everything it sends.

## Paired devices

```
/remote devices                 # the paired devices, newest first
/remote devices revoke 1        # unpin one (by its number in the list)
/remote devices revoke all      # unpin every one
```

A revoked device's commands are refused straight away; it must pair again. A device revoked in
AMP shows a notice in the chat too.

## Sleep, network loss and restarts

- Remote control has **no time limit** by default: it stays on until you end it (below). An org
  admin can add an idle limit or a maximum length in the org settings; then the chat says when it
  ends, and turns it off at that time. The laptop session keeps running either way.
- If the laptop sleeps, loses the network, or AMP's relay restarts or is down for a while, the
  footer shows `REMOTE OFFLINE · reconnecting` and the page shows "Laptop offline". Messages sent
  meanwhile are refused, not kept for later. The harness keeps trying (every 30 seconds at most)
  however long it takes, and when AMP is reachable again it reconnects by itself and the page
  catches up. Nothing needs pairing again.
- If the harness is killed or the laptop restarts, remote control is off in the new chat (the
  harness can't resume a chat). AMP lists the old session as offline until you press
  **Disconnect** on it, or run `/remote` again on this laptop, which ends it and starts a new
  one. Your paired devices keep working with the new session; you don't pair again.

## Turn it off

Any of these ends it within seconds, and the chat says which:

- `/remote off`, or quitting the harness;
- **Disconnect** on the web page;
- **Revoke all my remote sessions** in AMP;
- an org admin turning the org setting off.

To make sure it can never be turned on from a computer, set `AMP_HARNESS_DISABLE_REMOTE=1`, or
`[remote] enabled = false` in your config.toml; a repo's `.amp/harness/config.toml` can turn it
off for everyone who clones the repo, but never on.

## When /remote refuses

| The chat says | What to do |
|---|---|
| `needs your AMP login, and isn't available on an AMP server with sign-in turned off` | `amp-harness login`; a local compose AMP has no sign-in, use the local k8s one |
| `needs your own AMP login: an API token (AMP_TOKEN, amp_…) can't enable it` | unset `AMP_TOKEN`, then `amp-harness login` |
| `Remote control isn't available on this AMP server.` | the server is older, or its relay is off (`relay.enabled`) |
| `Remote control is turned off for this project's organization` | an org admin turns it on in the org settings |
| `turned off on this computer (AMP_HARNESS_DISABLE_REMOTE)` or `by your config` | the off switch above |
| `needs an AMP project` | run the chat in a folder with `.amp.json` (`amp-harness init`) |
| `plain http` | AMP must be https; `AMP_HARNESS_REMOTE_INSECURE_LOOPBACK=1` allows http to localhost for development only, with a warning |

## For agents teaching this

Have the person type `/remote` themselves and pair at the laptop: an agent must never answer the
pairing question, open the pairing link, or drive the AMP web page in a browser for them. Remote
control only carries chat and Stop; anything else (approvals, keys, config) is done at the laptop
or in the AMP web app as usual.
