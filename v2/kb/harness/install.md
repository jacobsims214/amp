---
title: Install AMP Harness, first run, upgrade and uninstall
tags: [harness, amp-harness, install, setup, first-run, upgrade, uninstall, onboarding]
---

# Install AMP Harness

## Before the first release: build from source

The one-line installer downloads a published `harness-v*` release from GitHub. **It works only
once the first harness release is published and the harness branch has reached `main`.** Until
then, running it ends with "no harness-v* release found".

In the meantime, build and install from a checkout of the AMP repo. You need git and a Rust
toolchain (rustup installs the pinned version from `v2/harness/rust-toolchain.toml` by itself):

```sh
git clone https://github.com/jacobsims214/amp.git
cd amp
git checkout harness-m4          # until the harness reaches main
make -C v2/harness install       # release build, packed and installed by install.sh
```

`make install` puts `amp-harness` in `~/.local/bin` through the same installer, so upgrade and
uninstall below work the same way. It does not offer to run setup (make runs the installer inside
`v2/harness`, which is not your project); it prints the command instead. Then go to your project
folder and run setup there:

```sh
cd ~/code/my-project
amp-harness setup                # or: amp-harness setup --preset <file or URL your admin gave you>
```

`PREFIX=/some/bin` installs it somewhere else. To upgrade, `git pull` and run it again.

## The one-liner (macOS and Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
```

With your company's org preset (its AMP server, model endpoint and which model each role runs
on; no secrets), as your admin gives it to you:

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh -s -- --preset https://intranet.example.com/amp-preset.toml
```

The script finds the newest release for your platform, checks its SHA-256 and puts `amp-harness`
in `~/.local/bin`. It needs no sudo. Then, in a terminal, it asks
``Run `amp-harness setup` now? [Y/n]``. This works under `curl | sh` too, because the answer is
read from the terminal. Without a terminal (CI, a Dockerfile) it only installs and prints the
setup command to run.

| option (env var) | what it does |
|---|---|
| `--preset FILE\|URL` (`AMP_HARNESS_PRESET`) | passes the org preset to `amp-harness setup --preset` |
| `--no-setup` (`AMP_HARNESS_NO_SETUP=1`) | install only; print the setup command |
| `--version X.Y.Z` (`AMP_HARNESS_VERSION`) | a specific release (default: the newest) |
| `--prefix DIR` (`AMP_HARNESS_PREFIX`) | where the binary goes (default `~/.local/bin`) |
| `--modify-path` | add that folder to your shell profile if it isn't on PATH (a backup is kept). By default it only prints the line to add |
| `--uninstall` | remove the binary (see below) |
| `--archive FILE [--sha256 HEX\|FILE]` | install a local archive instead of downloading |

Platforms: macOS 13 or newer (Apple silicon or Intel); Linux on x86_64 or aarch64 with glibc 2.35
or newer (Ubuntu 22.04+, Debian 12+, Fedora, RHEL 10…), with `curl` or `wget`, and `tar`. Alpine
and other musl systems, and older glibc (Ubuntu 20.04), are refused with a clear message.

## The first run: `amp-harness setup`

Setup (also `amp-harness init`; it's the same command) asks only for what isn't set up yet.
Press Enter to keep the value in [brackets]:

1. **AMP server.** Type `amp.example.com`, `host:port` or a full URL. It is checked before it is
   saved. See `harness/servers-and-login.md`. Once a server is saved, setup in another folder
   doesn't ask again: it prints `AMP server: … (already set up …)`. To change it, pass `--url` or
   run `amp-harness server set`.
2. **Login.** "Log in with your browser now?" Setup opens AMP's sign-in page and keeps the login
   in your OS keychain.
3. **Project.** Setup links this folder to an AMP project: an existing one, or a new one it
   creates. It writes `.amp.json` at the repository root (or in this folder when it isn't in a
   git repository). See "A folder inside a linked folder" below.
4. **Model provider.** If no provider has a key yet, setup asks which one to use:
   OpenRouter (the shipped roster), an Anthropic API key, an OpenAI API key, or your own
   OpenAI-compatible endpoint (`harness/enterprise-provider.md`). The key is typed hidden,
   checked with the provider and kept in the OS keychain.

Running plain `amp-harness` in a new terminal does the same first run before the chat opens
(`--no-setup` skips it). Setup is safe to run again: what is already done is kept.

For scripts, everything has a flag (`amp-harness setup --help`): `--url`, `--project ID` or
`--create NAME`, `--no-project`, `--key-env VAR`, `--profile NAME`, `--preset FILE|URL`,
`--yes` (never ask) and `--json`. With `--yes` and no `--login`, setup doesn't open a browser.
`--url` takes the same forms as `server set`: `amp.example.com`, `localhost:18000` or a full URL.

## A folder inside a linked folder

The harness uses the nearest `.amp.json` in this folder or any folder above it. A subfolder of a
linked repository (`cd src/`) uses the repository's project without asking. A folder that may be
a project of its own is asked about once:

- a new, empty or untracked folder inside a linked repository,
- a folder with its own git repository below a linked folder,
- a folder that is in no git repository, below a linked folder.

The first run says `This folder (my-app) has no .amp.json of its own: it would use project #45
Harness play from ../.amp.json in a parent folder` and shows the project picker. Pick or create
a project to give this folder its own `.amp.json`, or press Enter to use the parent's project
here too (that also writes a `.amp.json` here, so you aren't asked again). `amp-harness init` in
that folder does the same. Setup never writes over a parent folder's `.amp.json`: the new one
goes in this folder (or at its own repository's root), and setup says the parent's file is
unchanged. The chat banner and `/project` show where the `.amp.json` is when it is in a parent
folder (`project #45 Harness play (from ../.amp.json)`).

You don't need git. In a git repository with at least one commit, workers each get their own
worktree and branch. In a folder that isn't one (or a new folder inside another repository), each
worker gets its own folder `amp-work/<ticket>/` and nothing is committed: the chat banner and
`amp-harness doctor` say so in one line. Run `git init` and commit once to give them worktrees.
See `harness/git-and-the-harness.md`.

## Check it

```sh
amp-harness doctor
```

Every check prints `ok`, `warn`, `fail` or `skip`, with a fix for anything that isn't ok
(`harness/troubleshooting.md`).

## Upgrade

Run the same install line again. It replaces the binary in place (a running one is never
overwritten) and says `Updated amp-harness 0.1.0 → 0.2.0`. Your config, keys and logins are kept.

On macOS, the first time a new build reads each keychain item, macOS may ask once whether to
allow it. Choose "Always Allow" (`harness/troubleshooting.md`).

## Uninstall

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh -s -- --uninstall
```

This removes only the binary. It lists what it kept, so you can delete those by hand if you want:
the config folder (`~/.config/amp-harness`), keychain items under the service `amp-harness`
(macOS: `security delete-generic-password -s amp-harness`, once per item), and each project's
`.amp.json` and `.amp/harness/`.
