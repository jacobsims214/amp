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

- **when it ends:** at the latest 12 hours after you turned it on, sooner after 30 minutes with no
  remote activity;
- **a pairing link**, and under it a **QR code** to scan with your phone. The QR code needs a
  window of about 60 columns by 40 rows; in a smaller one (80x24) you get the link alone, so
  enlarge the window or copy the link;
- this laptop's key fingerprint (`29CA-E6B5-…`).

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

The link works once, for 5 minutes. `/remote pair` makes a new one (the old one stops working).
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

## Timeouts and what happens when the laptop sleeps

- Remote control turns itself off after **30 minutes without remote activity** and always **12
  hours** after `/remote`. The chat says so; the laptop session keeps running.
- If the laptop sleeps or loses the network, the footer shows `REMOTE OFFLINE · reconnecting` and
  the page shows "Laptop offline". Messages sent meanwhile are refused, not kept for later. When
  the network is back, the harness reconnects by itself and the chat says "Remote control
  reconnected." If a time limit passed meanwhile, it stays off until you run `/remote` again.

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
