---
title: AMP Harness — use your company's own model endpoint (walkthrough)
tags: [harness, amp-harness, enterprise, providers, openai-compatible, presets, models, roles, rollout, walkthrough]
---

# Use your company's own model endpoint

**The situation.** Your company runs an OpenAI-compatible endpoint at `api.example.com`. It
serves two models, `engineer` and `orchestrator`. You want AMP Harness to use them instead of
OpenRouter.

**The plan.** `orchestrator` plans and reviews, `engineer` does the work:

| role group | roles | model |
|---|---|---|
| manager | manager (plans and dispatches) | `example/orchestrator` |
| reviewers | reviewer, github-reviewer | `example/orchestrator` |
| workers | worker-backend, worker-frontend, worker-docs, designer | `example/engineer` |
| helpers | researcher, media, github-researcher, kb-curator | `example/engineer` |

`example` is the short id you give the endpoint in the harness, so its models are called
`example/engineer` and `example/orchestrator`.

**Pick your path:**

- **A.** You're setting up a new install for yourself.
- **B.** You already use the harness with OpenRouter and want to switch.
- **C.** You're an admin rolling it out to a team.
- **D.** One repo needs something different.
- **E.** Check it worked.
- **F.** Something went wrong.

**You need:** the endpoint address (`api.example.com`), how it authenticates (most gateways take
an API key as a bearer token), your own key, and your AMP server's address. None of this is
typed into a file. Keys go into a hidden prompt and from there into your OS keychain.

> **Before the first harness release** the `curl … install.sh` line doesn't work yet. Build
> from source as in `harness/install.md` ("Before the first release"), then run the
> `amp-harness` commands below as they are.

---

## A. On your own: the first run

Install, then run setup (the installer offers to run it for you):

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
amp-harness setup        # if you skipped it at install
```

Setup first asks for your AMP server, your login and this folder's project
(`harness/install.md`). Then, because no provider has a key yet, it asks which one to use.
Pick **4**:

```text
Which model provider do the agents use?
  1) OpenRouter — the opencode roster on OpenRouter (MiMo, DeepSeek, GLM, Kimi)
  2) Anthropic API key (console.anthropic.com) — Anthropic only: Opus for the manager and reviewers, Sonnet for the other workers
  3) OpenAI API key (platform.openai.com) — pick the model each role runs on
  4) Your own OpenAI-compatible endpoint (e.g. api.example.com: a company gateway, vLLM, LiteLLM, Ollama) — your models, mapped to the roles
Provider [1]: 4
Name for it (shown in the harness) [My models]: Example AI
Short id, used in model names like <id>/<model> (lowercase, digits, - and _) [example-ai]: example
Endpoint address (e.g. api.example.com or https://api.example.com/v1): api.example.com
Use https://api.example.com/v1? [Y/n]
How does your endpoint authenticate?
  1) An API key sent as a bearer token (Authorization: Bearer …) — most gateways
  2) An API key in a header of its own (Azure OpenAI's api-key, X-Api-Key, …)
  3) No authentication (a local or internal server)
  4) A command that prints a token (an SSO helper, Vault, az …)
  5) OAuth 2.0 client credentials (a client id and secret, a token endpoint)
  6) Microsoft Entra ID (Azure OpenAI / AI Foundry)
Authentication [1]:
Extra header the endpoint needs (name, Enter for none):
Example AI API key, Enter if it needs none (hidden):
  Example AI accepted the key.
It serves 2 models: engineer, orchestrator.
Which model each role runs on (picked by name; you can change them):
  manager    example/orchestrator   plans the work and dispatches it (the orchestrator) (manager)
  workers    example/engineer       write the code, docs and designs (worker-backend, worker-frontend, worker-docs, designer)
  reviewers  example/orchestrator   review the work (reviewer, github-reviewer)
  helpers    example/engineer       small, fast jobs: research, media, KB upkeep (researcher, media, github-researcher, kb-curator)
Use these? [Y/n]
```

What happens at each step:

1. **The address.** A bare host becomes `https://…/v1`, after you confirm it.
2. **The key.** It is typed hidden, checked against the endpoint's model list (`GET /models`),
   and kept in your OS keychain. If `EXAMPLE_API_KEY` is already set, that key is used and
   nothing is stored.
3. **The models.** The list comes from the endpoint, and the roles are matched by name. If the
   table isn't what you want, answer `n` and pick a model for each group.

Setup writes this to `~/.config/amp-harness/config.toml`. The key isn't in it:

```toml
[providers.example]
# Added by `amp-harness connect`. Its key is in the OS keychain (or EXAMPLE_API_KEY), never here.
name = "Example AI"
api = "openai-completions"
base_url = "https://api.example.com/v1"
key_env = "EXAMPLE_API_KEY"

[providers.example.models."engineer"]

[providers.example.models."orchestrator"]

[models]
profile = "inherit"
inherit = "example/engineer"      # every role not listed below, and agents you add later

[roles.manager]
model = "example/orchestrator"

[roles.reviewer]
model = "example/orchestrator"

[roles.github-reviewer]
model = "example/orchestrator"
```

Nothing runs on OpenRouter, and the harness never contacts it. Go to **E** to check.

## B. Switch an existing install off OpenRouter

One command adds the endpoint, asks for your key (hidden) and moves every role onto it:

```sh
amp-harness connect example --base-url api.example.com --name "Example AI" \
  --role manager=orchestrator --role workers=engineer
```

- You get the same mapping as the first run. The reviewers follow the manager onto
  `orchestrator`, and the helpers follow the workers onto `engineer`.
- The roles and models are checked before anything is saved. A typo stores nothing.
- The output shows the new mapping per group and per role.

Then remove the OpenRouter key from your keychain. No role uses it any more:

```sh
amp-harness disconnect openrouter
amp-harness connect --list        # example: yes (keychain), 11 roles; openrouter: no
```

Want to change the mapping later? Run `connect` again with only `--role`. It knows the key and
doesn't ask for it:

```sh
amp-harness connect example --role helpers=orchestrator
```

**In the chat** you can do the same without leaving it:
`/connect example api.example.com manager=orchestrator workers=engineer`.
Workers started from then on use the new models, and `/model` switches the chat itself.

**Scripts and CI:** `printf '%s\n' "$KEY" | amp-harness connect example … --key-stdin`. Or set
`EXAMPLE_API_KEY` and run `connect` without a terminal: it checks the key from the variable and
stores nothing.

## C. Roll it out to a team (admins)

You write one file, the **org preset**. It holds the AMP server, the endpoint, its models and
which model each role runs on. It holds **no secrets**. Each person then installs with one line,
and is asked only for their own key and their AMP login.

### 1. Write the preset

Start from `v2/harness/examples/enterprise-preset.toml` in the AMP repo:

```toml
[preset]
name = "Example Corp"
description = "Example Corp's own models on api.example.com"

[server]
url = "https://amp.example.com"        # your AMP server; optional

[providers.example]
name = "Example AI"
base_url = "https://api.example.com/v1"
models = ["engineer", "orchestrator"]

[roles]
default = "example/engineer"           # every role not named here, and agents added later
manager = "example/orchestrator"
reviewers = "example/orchestrator"
```

- `[roles]` takes the groups (`manager`, `workers`, `reviewers`, `helpers`), `default`, or one
  role's name (`kb-curator = "example/orchestrator"`).
- Not a bearer key? Add the endpoint's settings to `[providers.example]`. They are the same as
  in config.toml (`harness/providers-and-keys.md`):
  - `auth = "header"` with `auth_header = "X-Api-Key"`;
  - `auth = "command"` with `auth_command = ["corp-sso", "token"]`;
  - `auth = "oauth-client-credentials"` or `"azure-entra"`;
  - `ca_file`, `client_cert`, `proxy`, `query`;
  - `secret_headers = ["X-Team-Token"]`: each person is asked for the value, which goes to their
    keychain.
- MCP servers everyone should have go in `[mcp.<name>]` tables, in the config.toml schema
  (`harness/mcp-servers.md`). Their secrets are asked from each person at setup.

The preset is **refused**, naming the key, when:

- it has an unknown key (a typo);
- anything in it looks like a secret (`api_key = …`, a `Bearer …` header, a token);
- a URL isn't https (plain http is allowed only for localhost).

### 2. Try it yourself

```sh
amp-harness setup --preset ./enterprise-preset.toml
amp-harness doctor
```

### 3. Host it

Put the file on an internal **https** web server, for example
`https://intranet.example.com/amp-preset.toml`. Setup fetches only https (or http on
localhost), and at most 256 KiB.

### 4. Send people the install line

```sh
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh -s -- --preset https://intranet.example.com/amp-preset.toml
```

The installer installs the binary and runs `amp-harness setup --preset …`. Setup:

1. shows what the preset writes and asks once (below), then writes its provider, roles and MCP
   servers into the person's config.toml;
2. saves the preset's AMP server and offers the browser login;
3. links their folder to a project;
4. asks for their own key, hidden, and checks it.

People who already have the harness run `amp-harness setup --preset https://intranet.example.com/amp-preset.toml`.

Until the first harness release, have people build from source (`harness/install.md`). Then
they run `amp-harness setup --preset https://intranet.example.com/amp-preset.toml`.

**Machines with no person (CI, build agents):**

```sh
EXAMPLE_API_KEY=… amp-harness setup --preset https://intranet.example.com/amp-preset.toml --yes --no-project
```

`--yes` asks nothing and takes keys from the environment. The AMP login comes from `AMP_TOKEN`.

### What people see before anything is written

A preset goes into each person's own config, which is always trusted, so setup shows **exactly
what it writes** first and asks once:

```text
The org preset "Example Corp" (https://intranet.example.com/amp-preset.toml) writes this into ~/.config/amp-harness/config.toml:
  It makes this machine run:
    npx -y @acme/tools-mcp --root ${repo} (MCP server tools: starts with every chat and run)
    get-token --tenant 'acme corp' (provider example's auth_command: runs each time a token is needed)
  adds [providers.example]:
    [providers.example]
    …every line of the table…
  REPLACES your [mcp.docs] (- yours, + the preset's):
      [mcp.docs]
      type = "http"
    - url = "https://docs.mine.example.com/mcp"
    + url = "https://mcp.example.com/mcp"
  sets the model of each role: default = example/engineer, manager = example/orchestrator (your [roles.*] model for those roles is replaced)
Apply it, replacing your [mcp.docs]? [y/N]
```

- Every table is shown as it will be written, and every command it makes the machine run (stdio
  MCP servers and a provider's `auth_command`) is listed first.
- A table the person already has with other contents is never replaced silently: setup shows the
  diff and asks. A table they already have as the preset has it is left as it is.
- With no person (no terminal, no `AMP_HARNESS_PROMPTS=stdin`), setup needs `--yes`. If the
  preset would replace one of their tables, it also needs `--force`; otherwise nothing is
  written and the diff says why.
- If config.toml changes between the question and the answer, nothing is written; run setup
  again.

## D. Per-repo overrides

A repo can share model choices and MCP servers with everyone who clones it, in
`<repo>/.amp/harness/config.toml`. Write it with commands, then commit it:

```sh
amp-harness models set worker-frontend example/orchestrator --repo
amp-harness models reset worker-frontend --repo
amp-harness mcp add fs --scope repo -- npx -y @modelcontextprotocol/server-filesystem '${repo}'
```

```toml
# <repo>/.amp/harness/config.toml
[roles.worker-frontend]
model = "example/orchestrator"

[mcp.fs]
type = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "${repo}"]
```

- A repo file may hold only `[roles.*]`, `[models] profile` and `[mcp.*]`. Providers, keys and
  the AMP server stay in each person's own config, so everyone needs the `example` provider
  (from **A**, **B** or **C**) first.
- Repo roles, once trusted, win over a person's own. `amp-harness models list` shows `repo …` in
  its SOURCE column for them.
- Each person is asked to trust the repo's MCP servers, agents, skills and `[roles.*]` before
  they are used (`harness/mcp-servers.md`, `harness/agents-and-skills.md`). Until then their own
  settings win. `--trust-repo` uses them all on a build machine.
- Agents and skills for the repo go in `.amp/harness/agents/` and `skills/`, written with
  `--repo` (`harness/agents-and-skills.md`).

## E. Check it worked

```sh
amp-harness doctor
```

Look for your provider and every role on it, and the endpoint check:

```text
  models:   inherit example/engineer
  provider: example (Example AI) https://api.example.com/v1 (key: keychain)
            auth: bearer key
  roles:    designer           example/engineer
            github-researcher  example/engineer
            github-reviewer    example/orchestrator
            kb-curator         example/engineer
            manager            example/orchestrator
            …
  [ok  ] roles          11 roles resolve (inherit example/engineer)
  [ok  ] key:example    key from keychain
  [ok  ] models:example 2 models listed: engineer, orchestrator
```

- `models:example` is the real test: the endpoint answered with your key and lists every model a
  role uses.
- `openrouter` should not appear anywhere.
- `amp-harness models list` shows every role with the setting that chose it.
- `amp-harness run -p "What is on the board?"` is a quick end-to-end run on `orchestrator`.

## F. Troubleshooting

`doctor`'s `models:example` line names the fix for most of these.

Running `connect example` again with endpoint flags (`--ca-file`, `--header`, `--auth`, `--proxy`
and so on) changes only what you give. The provider's models (including ones added with `models
set --no-check`) and its other settings stay, and `connect` prints what changed.

**TLS / a corporate CA.** `connect`, the first run and doctor's `models:example` say it in one
sentence, e.g. "api.example.com's certificate isn't trusted: it's signed by a CA this machine
doesn't know." (add `--verbose` to see the full TLS error). It means the endpoint uses your
company's certificate authority. Give the CA bundle once:
`amp-harness connect example --ca-file /etc/ssl/corp-root.pem`. An expired certificate or one
issued for another host name is said as such; a CA bundle won't fix those. In a preset, put
`ca_file = "/etc/ssl/corp-root.pem"`, a path that exists on each machine. The bundle is added to
the OS trust store, which is always used too. If the endpoint asks for a client certificate, add
`--client-cert me.pem --client-key me.key` (`--store-client-key` keeps the key in the keychain).
The first run asks for a CA bundle (or a client certificate) only when the endpoint fails without
one.

**A proxy.** `HTTPS_PROXY` and `NO_PROXY` apply by default. For this endpoint only, use
`--proxy http://proxy.example.com:3128` (or `--proxy direct`). A proxy password goes in
`HTTPS_PROXY`, never in config.

**Auth headers / 401 or 403.** doctor names which credential was refused.

- A key that changed: run `amp-harness connect example` again (or update `EXAMPLE_API_KEY`).
- A key in its own header rather than `Authorization: Bearer`:
  `amp-harness connect example --auth header --auth-header X-Api-Key`.
- An extra header that isn't secret (`X-Team: platform`): `--header X-Team=platform`.
- An extra header that is secret: add it in a terminal, where its value goes to the keychain.
  `--header` refuses secret values.
- Tokens from an SSO tool: `--auth command --auth-command "corp-sso token --audience llm"`.

**A model isn't listed.** Check what the endpoint returns with `amp-harness models list
--available --check`. If it serves a model it doesn't list:
`amp-harness models set manager example/orchestrator --no-check` sets it anyway and adds it to
your config. Model ids are case-sensitive, and the name you map must match exactly.

**An http:// endpoint.** Plain http is accepted without a question only for localhost. For any
other host:

- the first run asks first, and the default answer is no;
- `connect --base-url http://…` takes it as given;
- a preset refuses it.

Use https wherever the key crosses a network.

**Some roles still run on OpenRouter.**

- Run `amp-harness models list` and read the SOURCE column. A `[roles.<role>]` in your config
  from earlier is replaced by `connect --role`, but a repo's `.amp/harness/config.toml` can still
  set one.
- `amp-harness connect example --role manager=orchestrator --role workers=engineer` puts every
  role on the endpoint.

**The key isn't kept.** On a machine with no keychain (`AMP_CREDENTIAL_STORE=file`, a headless
Linux box), keys are never written to a file. Set `EXAMPLE_API_KEY` in the environment instead.
