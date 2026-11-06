# In-app agent chat drawer and inbound channels

Status: Draft · Date: 2026-11-05 · Issue: #162 (related: #161 project inbox, epic 165 remote control,
tracking #141)

## Problem

To talk to a project's manager agent today, a person needs a harness running on their own machine,
either in a terminal next to the web UI or driven from a phone through remote control. AMP has no
agent of its own to talk to, and nothing outside AMP can hand a request to an agent:

- **The only agent runtime is on the laptop.** The relay joins "a person's amp-harness on their
  laptop" to "the same person's browser" (`v2/amp-api/internal/relay/relay.go`, package doc). The
  web UI's `/sessions/:id` page says it plainly: "The laptop does the work; AMP only relays"
  (`v2/ui/src/pages/Session.tsx`). Close the laptop and the chat stops.
- **The harness has no long-running headless mode.** `amp-harness run` is "one headless turn of an
  agent" (`v2/harness/crates/amp-harness/src/run.rs`). The chat is the TUI. Nothing ships the
  harness as a container either: `v2/harness/packaging` holds only Homebrew, and the Helm chart
  (`v2/deploy/helm/amp/templates/`) has no harness workload.
- **Agent identities can't use the remote stream.** Relay registration refuses "agent accounts" and
  `amp_` API tokens (`relay/harness.go`, `harnessPerson`: `RemoteCodeAgentNotAllowed`), and the
  harness refuses `AMP_TOKEN` for `/remote` (`amp-harness/src/remote.rs`). A harness running as a
  provisioned agent account cannot publish a session today.
- **Webhooks only go out.** `internal/webhook` delivers AMP events and audit rows to receivers
  (`webhook.go`), and `slack.go` formats them as Slack `{"text": ...}`. The only inbound hook is
  GitHub's (`api/github_hook.go`, `POST /api/hooks/github/:project_id`, with Envoy's
  `inboundHooks` route). No Slack mention, chat message or generic event can reach an agent.

Jd (55:24): *"meet users where they are ... I'm in Slack, I @AMP and I say, hey, I need you to go
take care of this feature."* Jacob (56:51): *"having like a chat drawer in the ui, that's like a
manager agent you can talk to. And that also could be like a webhook could come inbound to the
manager agent for a project ... would require having like a global harness ... Nvidia open shell
container in the helm stack ... in manager mode."*

**How we will know it is fixed**
- v1 (Phase 1): on a project with the hosted agent enabled, a project member opens the chat drawer
  from any project page, types "split epic 40 into stories", watches the manager's text, tool calls
  and status stream in, and sees the stories appear on the board. No harness runs on their machine.
- Phase 2 (separate PR): in a Slack channel linked to project 7, `@amp triage the login bug` gets a
  threaded reply from AMP, and the audit log has one `channel.message.received` row and one
  `channel.reply.sent` row, each naming the Slack user, the AMP user it maps to and the agent that
  acted.
- Every write the hosted agent makes shows in history and audit as `"<person>'s agent"`
  (`actor_type=agent`, `on_behalf_of_user_id=<person>`), never as the person.
- With the Helm value off (the default) or the project switch off, neither the drawer nor the
  inbound route exists for that install or project.

## Goals / Non-goals

- Goal: an optional Helm component, **off by default** like `relay.enabled`
  (`deploy/helm/amp/values.yaml`), that runs the AMP harness headless in a sandboxed container.
- Goal: a chat drawer in the web UI that streams a hosted agent session with the same event model
  as remote control (`ui/src/remote/session.ts`, `docs/remote-control/protocol.md` section 6).
- Goal: per-project enablement, project RBAC, and one identity model: each person's messages run
  as a provisioned agent account managed by that person, audit-logged as "<person>'s agent".
- Goal (Phase 2): one inbound path. An external event (Slack first) reaches a Go actor, and
  replies leave through the webhook outbox. Both legs are audit-logged with a data classification.
  This is the owner's constraint (01:05:19): *"Go actors are the thing to use. And then if you
  want a GO actor to be able to communicate externally, there's a web hook actor."*
- Goal: agent roles beyond the manager. Jd (57:40): *"I wouldn't limit it to a manager only."* The
  role list comes from the harness's role registry (`amp-core/src/roles.rs`), not a new list.
- Non-goal in v1: workers. A hosted agent is chat and planning only (Q7 answered "no" for v1):
  `serve` does not offer `amp_run_ticket` or launch workers, because the pod has no repo checkout,
  no git credentials and no egress to GitHub or git remotes (§1). Workers on the cluster need that
  egress and credential design first; it belongs with Phase 3.
- Non-goal in v1: inbound channels. Slack is Phase 2, its own PR, after the three gates in §4.
- Non-goal in v1: an operator that deploys one harness pod per agent. Jacob (57:54): *"you would
  have to have like an operator that can deploy harnesses as pods"*. That is Phase 3.
- Non-goal: replacing the laptop harness or remote control. Remote control stays as it is.
- Non-goal: approvals from a channel. Plan approval, artifact approval and marking a person's own
  ticket done stay `InUI()`-only (`authz/authz.go`). A Slack message or a drawer message relayed to
  an agent never counts as the person in the UI.
- Non-goal: Discord, Teams, email. The channel table takes an open `kind` so they can be added.

## Journeys

**J1. Drawer, manager.** Dana, a member of project 7, clicks **Ask agent** in the project header.
The drawer opens on the right, over the board. It shows the project's hosted session (or starts
one), her past turns, and a status line: model, current ticket, workers. She types "what's blocking
epic 12?". The manager calls `amp_list_tasks` (whose result carries `waiting_on_people`,
`mcp/server.go`), streams its answer and links the tickets.
History rows for any write read "Dana's agent".

**J2. Drawer, another role.** Dana picks **Reviewer** in the drawer's role menu and asks it to
review the ticket for PR #88, reading tickets and comments through `/mcp` (no checkout in v1). The
session runs the `amp-reviewer` role from the same pod. Roles the org did not enable are not in
the menu.

**J3. Slack mention (Phase 2).** In `#web-team`, linked to project 7, Dana writes `@amp file a
story for the broken password reset`. Slack's Events API calls `POST /api/hooks/slack`. AMP checks
the signature and that the event's `team_id` and channel match the linked channel, maps Dana's
`(team_id, user_id)` to her AMP user, and the `ChannelActor` for project 7 queues a turn on Dana's
agent. The manager adds an inbox item (#161, `amp_inbox_add`), and AMP replies in the thread from
a fixed template: "Added to Inbox: item 412 (link)". The template has no title, because the
model wrote it (§4, Replies). If Dana's Slack user maps to
no AMP user, AMP replies "I don't know you yet: link your Slack account in AMP settings" and runs
nothing.

**J4. Off.** On an install without `hostedAgent.enabled`, `/api/me` has no `hosted_agent`
capability and the button is not rendered. On a project with the switch off, the button is not
rendered, and (Phase 2) the Slack hook answers 200 with no reply (Slack retries a non-2xx), plus an audit row
`channel.message.refused` with reason `project_disabled`.

**J5. Stop.** Dana clicks **Stop** in the drawer. The run aborts and the follow-up queue clears,
exactly as the remote `stop` command does (protocol 5.1). (v1 has no workers to keep running.)

## Design

### Overview

```
 v1   browser drawer ──GET  /api/agent-sessions/:id/events (SSE, cookie)─▶ amp-relay ◀─events── hosted pod
                     ──POST /api/agent-sessions/:id/commands (cookie)──▶ amp-api ─┐          (serve)
      amp-api ── hosted control channel (session_start, command, agent_token) ────┴─────▶ hosted pod
 P2   Slack Events ─▶ POST /api/hooks/slack ─▶ ChannelActor ─▶ hosted control channel ─▶ hosted pod
      hosted pod ─ POST /api/internal/hosted/replies ─▶ ChannelActor ─▶ outbox (kind slack_reply)
      every leg ─▶ audit_log (+ data_class) ─▶ audit_outbox ─▶ SIEM webhooks (unchanged)
```

The hosted harness is the same binary as the laptop harness, in a new `serve` mode. It publishes
each **hosted session** on the existing relay, and the drawer reads that event stream. Commands do
not take the relay path: hosted sessions use no device signing (§2, "Commands and the trust
root"). The drawer posts commands to amp-api with the person's cookie, and amp-api forwards them
over a second, narrow link, the **hosted control channel** (§2). That channel joins amp-api and the
pod directly. amp-api uses it to start sessions, forward commands, hand over tokens and (Phase 2)
deliver channel prompts. The pod uses it to return channel replies. The drawer adds no payload storage in
Postgres: the relay keeps payloads in memory only (D3 of the remote control protocol,
`relay/relay.go`), and the drawer's history comes from the harness's own session file, as the
TUI's does. Phase 2 has one exception, stated in §4: a Slack reply's text sits in the outbox until
it is delivered.

### 1. The hosted harness (Helm and `amp-harness serve`)

- **Helm.** A new `hostedAgent` block in `values.yaml`, `enabled: false`, rendered only when
  `relay.enabled` is also true (the stream rides on the relay) and Dex is on (same rule as
  amp-relay). It deploys `templates/hosted-agent.yaml`: one Deployment, `replicas: 1` in v1, a PVC
  for the harness state dir (session files, dispatch ledger), an egress policy (next bullet), and a
  `runtimeClassName` / `securityContext` slot for a sandbox runtime. The call named an NVIDIA
  OpenShell container (45:34); the chart exposes the runtime class and image so an operator can
  choose it, and defaults to a non-root, read-only-root-filesystem pod (Q3).
- **Egress.** This is new ground for the chart: its policies today limit ingress only
  (`templates/networkpolicy.yaml`: "Egress is not limited"). A vanilla Kubernetes NetworkPolicy
  matches IPs, CIDRs and selectors, not hostnames, so it cannot allow "the model provider" by
  name. `hostedAgent.egress.mode` picks one of two enforced setups, and the chart fails to render
  with `hostedAgent.enabled=true` and no mode:
  - `proxy` (default): a vanilla NetworkPolicy allows egress only to Envoy (AMP's `/mcp`,
    `/relay/` and `/api/internal/hosted/`), cluster DNS, and an egress proxy
    (`hostedAgent.egress.proxy`, set as `HTTPS_PROXY` in the pod). The proxy allowlists the
    provider hostnames (`hostedAgent.egress.allowHosts`). The chart can deploy a small proxy for
    this or point at an existing one.
  - `cilium`: a `CiliumNetworkPolicy` with `toFQDNs` for `allowHosts`, plus the same Envoy and DNS
    rules. This needs Cilium as the CNI.

  Either way there is no route to GitHub or git remotes and no repo checkout in v1. Without a CNI
  that enforces policy, both setups are inert, so R3 and R4 assume one (Verification checks it).
- **Image.** A `Dockerfile` in `v2/harness/` (none exists today) builds `amp-harness` for linux
  amd64/arm64 with the embedded assets (`assets/agents/*.md`, `assets/skills/`). Model provider
  config is the same TOML the installer takes (`install.sh`, `examples/enterprise-preset.toml`),
  mounted from a ConfigMap; provider keys come from a Secret (`hostedAgent.providerSecret`).
- **`amp-harness serve`.** A new subcommand in `amp-harness/src/main.rs`. It holds no TUI. For each
  `session_start` that amp-api sends over the control channel (§2, "Session creation"), it builds
  the role's runtime as `run` does (`amp_core::agent::Runtime`,
  `run.rs`), feeds the `ManagerFeed` (`amp-core/src/manager_feed.rs`, "for every client that isn't
  the TUI itself"), and keeps the session file under the existing lock
  (`amp-core/src/session_store.rs`, `session_lock.rs`). The follow-up queue, `stop`, the repeat
  guard and orphan release (`run.rs`, AMP #2910) behave as in the chat.
- **Roles.** `serve` loads the role registry (`amp-core/src/roles.rs`). The org enables a subset
  (§3); the manager is always first. In `serve` mode the supervisor is not started
  (`supervisor.rs`, `worker_session.rs`) and `amp_run_ticket` and other worker-launch tools are
  removed from every role's tool list, so v1 is chat and planning only. A role that needs a
  checkout (an implementer) is not offered.
- **Budgets.** `serve` enforces, per session, a cap on tool calls per turn and on turns per hour,
  and per org a daily token cap (`hostedAgent.limits` in Helm: `toolCallsPerTurn`, `turnsPerHour`,
  `orgDailyTokens`). When a cap is hit the turn ends with a visible status and an audit row
  `hosted_agent.limit.hit`. Every turn writes `hosted_agent.turn.usage` with the org, person, role
  and token counts from `Totals`, so one install-wide `providerSecret` still gives per-org usage.
  Whether to add per-org provider keys or a monthly spend cap is Q4.

### 2. Identity and tokens

Every hosted session runs as an **agent account managed by the person** who owns it:

- On first use, AMP creates (or reuses) one agent account per (person, project), `"<Dana>'s agent"`
  with `ManagedBy = Dana`, through the existing `CreateAgent` path (`service/accounts.go`;
  `domain.User.Kind = agent`, email in `agents.amp`). It is **not** put on teams. Team membership
  would grant every project the team links to (too much), would give nothing to an org owner or
  admin or a global admin who sees the project without a team (`remoteOwnerHasAccess` in
  `repository/remote.go` allows both; too little), and would outlive Dana's removal from the team.
- Instead migration 056 adds `hosted_agent_grants(agent_user_id, project_id)`, and `authz` gives an
  agent principal with a grant row the project level of its `ManagedBy` person on that one project,
  capped at Member, and nothing anywhere else. The check is evaluated per request against the
  person's current access (the same predicate as `remoteOwnerHasAccess`), so removing Dana from the
  team, the org, or the project cuts the agent off at once, whatever its token's expiry. The session
  is also ended (`EndRemoteWithoutAccess`), as today.
- AMP mints the agent a short-lived API token (v1 uses 1 day; amp-api renews it over the control
  channel while the session lives) and hands it to the pod over the hosted control channel, never
  through Helm values.
- **Hosted agent accounts are not person-manageable for tokens.** Today `accountFor`
  (`service/accounts.go`) lets the `ManagedBy` person manage the agent's tokens, up to
  `MaxTokenDays = 3650` or 0 for "never". Dana could then mint a long-lived token and use her
  hosted agent outside the pod, bypassing the hosted budgets and the 1-day token. Migration 056
  adds `users.hosted_agent BOOLEAN NOT NULL DEFAULT FALSE`, set when the account is provisioned.
  For such an account, `CreateToken` refuses every principal, Dana and global admins included
  ("hosted agent tokens are issued by AMP only"). Only the internal control path mints them.
  Dana and admins may still list and revoke its tokens, and revoking one ends the session.
- Audit: `domain.AuditEntry` has `ActorType`, `OnBehalfOfUserID` and `AgentProfile`, but no
  `ManagedBy`; `ManagedBy` lives on `domain.User` and `authz.Principal`, and
  `service/audit_context.go` copies it into `OnBehalfOfUserID` for agent principals. The history
  label renders "Dana's agent" from `OnBehalfOfUserID`. No new audit field is needed.
- **Session creation.** amp-api creates hosted sessions; the pod never does. With one shared pod
  for every org (R3), a pod that could register sessions and name their owners could act as
  anyone, so the pod gets no such power. The order is:
  1. `POST /api/projects/:id/agent-sessions {role}` (cookie, `InUI()`, project member). amp-api
     provisions or reuses the agent account and its grant, and inserts the `remote_sessions` row
     with `kind = 'hosted'`, `state = 'starting'`, and `owner_user_id`, `agent_user_id`,
     `project_id` and `role` all fixed server-side. It mints the agent token and generates a
     one-time `claim_secret` (only its hash is stored on the row).
  2. amp-api sends `session_start{session_id, owner, agent_user_id, project_id, role,
     agent_token, claim_secret}` over the control channel and returns the session (still
     `starting`) to the drawer, which opens the event stream and shows "Starting…".
  3. The pod builds the runtime and calls a new relay route,
     `POST /relay/v1/hosted/sessions/:id/claim`, with the pod token and the `claim_secret`. The
     relay accepts only an existing `kind = 'hosted'`, `starting`, unclaimed row whose hash
     matches. It takes the owner from the row, never from the pod, stores the harness key the pod
     publishes in the claim (`harness_id`, `harness_pub`; next bullets) and moves the row to
     `active`.
  4. If no claim arrives within 30 seconds, amp-api ends the row (`start_timeout`) and the drawer
     shows the error. A second claim of the same ID is refused.

  `harnessPerson` (`relay/harness.go`) keeps refusing agent accounts on the laptop path. The
  browser leg's owner check is unchanged: the drawer's session is visible only to its owner
  (`relay/browser.go`: "someone else's session, global admins included, is the same 404").
  Residual risk: the pod still holds the token of every active hosted agent. Those tokens are
  1-day and each is limited by `hosted_agent_grants` to one project at Member level, but a
  compromised pod can act as any of those agents until the tokens expire or are revoked (R3).
- **Hosted control channel.** The pod holds one long-lived, outbound connection to amp-api at
  `/api/internal/hosted/` through Envoy (already in its egress), authenticated by a hosted-pod
  token from a Kubernetes Secret that only amp-api and the pod mount (like the relay cluster token;
  the relay never holds it). amp-api sends `session_start`, `session_end`, `command` (prompt or
  stop), `agent_token` and, in Phase 2, `channel_prompt`; the pod sends `channel_reply`. It is not
  the relay's `relayctl` (today `ctl/end`, `ctl/device-revoked`, `ctl/presence` in
  `relay/cluster.go`), which stays relay-internal.
- **Commands and the trust root (no device signing for hosted sessions).** Laptop sessions trust a
  device only after a pairing confirmed at the laptop (protocol section 7), and the browser signs
  each prompt with its device key. The drawer cannot do that: `ui/src/remote/keyGate.ts` refuses
  every private-key operation (`signCommand`, `ensureDevice`, `signPairRequest`, `pinHarness`)
  unless the document was fully loaded at `/sessions/…` under `SESSION_CSP`, and a drawer inside
  the main app runs under `BASELINE_CSP` (`csp.test.ts` also forbids router links into that path).
  We do not weaken the gate: it guards the same IndexedDB key that drives laptop sessions. A
  full-page route would not be a drawer, and `frame-ancestors 'none'` rules out an iframe. So
  hosted commands are not device-signed. The drawer posts `prompt` or `stop` to
  `POST /api/agent-sessions/:id/commands`. amp-api authenticates the cookie, checks the owner and
  the M1 allowlist (protocol 5.1), writes an audit row, and forwards the command over the control
  channel. `serve` accepts commands for a hosted session only from the control channel. It ignores
  any command that arrives through the relay, so the relay alone still cannot inject one. This
  also removes device pinning for hosted sessions: there is no `pin_device` and no auto-pair.
- **Events and the harness key.** The session reducer checks each event's signature against a
  harness pin. `deviceStore` writes harness pins only after a pairing confirmed at the laptop, and
  `pinHarness` is behind the same key gate. For hosted sessions the pin is served by amp-api
  instead. The pod generates a harness key per session and publishes its public half in the
  claim. amp-api returns it as `harness_pub` in `GET /api/agent-sessions/:id`. The drawer keeps it
  in memory and passes it to `verifyEvent(env, pin)`, which the gate leaves open. It is never
  written to `deviceStore`, so no laptop pin can be confused with a hosted one.
- **What this makes amp-api.** amp-api is the trust root for hosted sessions on both legs. It
  decides who may send commands (by cookie), and it vouches for the harness key the drawer checks
  events against. What this keeps: the relay alone can neither inject a command nor forge an event
  the drawer accepts. What it gives up: anyone who controls amp-api can drive any hosted session
  and show its owner forged events. That is no new power over AMP data, because amp-api already
  enforces every RBAC check the agent's writes go through. It is still a real difference from
  laptop sessions, and it is R1b.

### 3. Data model (`056_hosted_agent.sql`, Phase 1; `057_outbox_kinds.sql` and `058_channels.sql`, Phase 2)

The project inbox spec claims 053–055 (052 is #146); these follow and are renumbered at merge time.
`057_outbox_kinds.sql` is the generic outbox change that both this spec and inbox Phase 3 need
(§4, Gates); whichever PR lands first carries it.

```sql
-- 056: per-org roles and per-project switch
ALTER TABLE orgs     ADD COLUMN IF NOT EXISTS hosted_agent_roles TEXT[] NOT NULL DEFAULT '{amp-manager}';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS hosted_agent_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE remote_sessions ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'laptop'
    CHECK (kind IN ('laptop','hosted'));
ALTER TABLE remote_sessions ADD COLUMN IF NOT EXISTS role TEXT;          -- amp-manager, amp-reviewer…
ALTER TABLE remote_sessions ADD COLUMN IF NOT EXISTS agent_user_id INTEGER REFERENCES users(id);
ALTER TABLE remote_sessions ADD COLUMN IF NOT EXISTS claim_secret_hash TEXT;  -- hosted, until claimed
-- hosted rows exist before the pod claims them: allow 'starting' and no harness key yet
ALTER TABLE remote_sessions DROP CONSTRAINT IF EXISTS remote_sessions_state_check;
ALTER TABLE remote_sessions ADD CONSTRAINT remote_sessions_state_check
    CHECK (state IN ('starting','active','ended'));
ALTER TABLE remote_sessions ALTER COLUMN harness_id  DROP NOT NULL;
ALTER TABLE remote_sessions ALTER COLUMN harness_pub DROP NOT NULL;
ALTER TABLE remote_sessions ADD CONSTRAINT remote_sessions_harness_key_check
    CHECK (state = 'starting' OR (harness_id IS NOT NULL AND harness_pub IS NOT NULL));
ALTER TABLE users ADD COLUMN IF NOT EXISTS hosted_agent BOOLEAN NOT NULL DEFAULT FALSE;  -- §2 tokens
CREATE TABLE IF NOT EXISTS hosted_agent_grants (    -- one project per agent account (§2)
    agent_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (agent_user_id, project_id)
);

-- 057 (Phase 2, shared with inbox Phase 3): generic outbox kinds. Today webhook_deliveries rows
-- always belong to a webhook (011_webhooks.sql); a kind-tagged row may not.
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'webhook';
ALTER TABLE webhook_deliveries ALTER COLUMN webhook_id DROP NOT NULL;
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE;
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS target_url TEXT;
ALTER TABLE webhook_deliveries ADD CONSTRAINT webhook_deliveries_target_check
    CHECK (kind = 'webhook' AND webhook_id IS NOT NULL
        OR kind <> 'webhook' AND project_id IS NOT NULL AND target_url IS NOT NULL);
-- Failure accounting is per kind: 'webhook' rows keep webhooks.consecutive_failures; every other
-- kind reports to its owner row (slack_reply → project_channels, below; inbox's
-- github_issue_comment → project_inbox_sources, added by inbox 055).

-- 058 (Phase 2): inbound channels. One install-level Slack app owned by the AMP admin; its
-- signing secret is install config (Helm Secret), never per project.
CREATE TABLE IF NOT EXISTS slack_workspaces (       -- one row per OAuth install of AMP's app
    team_id       TEXT PRIMARY KEY,
    bot_user_id   TEXT NOT NULL,                 -- to drop AMP's own messages
    bot_token_ref TEXT NOT NULL,                 -- secretbox
    installed_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    installed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS project_channels (
    id            SERIAL PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,                 -- slack (open set)
    team_id       TEXT NOT NULL REFERENCES slack_workspaces(team_id) ON DELETE CASCADE,
    channel_id    TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'amp-manager',
    data_class    TEXT NOT NULL DEFAULT 'internal'
                  CHECK (data_class IN ('public','internal','confidential','restricted')),
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,   -- slack_reply deliveries (057)
    last_failure_at      TIMESTAMPTZ,
    last_error           TEXT NOT NULL DEFAULT '',
    created_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (kind, team_id, channel_id)
);
CREATE TABLE IF NOT EXISTS channel_identities (     -- Slack user → AMP user, from Sign in with Slack
    kind TEXT NOT NULL, team_id TEXT NOT NULL, external_user TEXT NOT NULL,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    linked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (kind, team_id, external_user)
);
CREATE TABLE IF NOT EXISTS channel_events (         -- idempotency only, no message text
    kind TEXT NOT NULL, event_id TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (kind, event_id)
);
```

- Hosted sessions reuse `remote_sessions` (`domain/remote.go`) so the session list, idle timeout,
  `RevokeAllRemote`, `EndRemoteWithoutAccess` and `ExpireRemoteSessions` (`service/remote.go`)
  cover them with no second lifecycle.
- Bot tokens go through `secretbox` (`internal/secretbox`), as webhook and GitHub credential
  secrets do (`045_github_credential_secrets.sql`). Project owners never paste a Slack secret: they
  pick a channel from the workspaces the AMP admin installed the app into. One app, one signing
  secret, so no project owner holds a key that can sign events.
- `channel_events` makes Slack's retries idempotent, like the inbox's unique `source_ref` index
  (`project-inbox.md` §1). Rows older than 7 days are deleted by the existing ticker.
- **Data classification** (`data_class`) is new: no column for it exists today, and no project
  class exists either (Q5). Each channel declares the highest class it may carry, and the audit
  rows for that channel record it in `Details`. The control that matters, refusing to link a
  channel to a project of a higher class, needs a project class, so Q5 is a gate for Phase 2
  (§4), not a follow-up. In v1 the drawer keeps data inside AMP; its audit rows
  (`hosted_agent.session.start|end`, `hosted_agent.turn.usage`) carry `data_class` once the
  project class exists. Until then #162's "audit-logged with data classification" criterion is
  only half met, and this spec says so rather than claiming it.

### 4. Inbound path: the `ChannelActor` (Phase 2, its own PR)

**Gates.** Phase 2 starts only when all three hold: (a) the generic outbox migration
`057_outbox_kinds.sql` (§3: `webhook_deliveries.kind`, nullable `webhook_id`, `project_id`,
`target_url`, per-kind failure accounting, and the delivery loop branching on `kind`) has merged,
since `slack_reply` cannot fit `011_webhooks.sql` as it stands. Slack does not wait on inbox
migration 055: that is inbox Phase 3 (GitHub link-back), held on inbox Q3/Q4, and is GitHub-shaped
(`055_outbound_github.sql`, an `inbox_source_id` FK). Instead the generic part
is split out so both specs depend on 057, and inbox 055 keeps only its GitHub columns. The inbox
spec needs a matching edit when either PR lands. (b) Q5 has settled where a project's class lives.
(c) Jacob has accepted the trust root in §2 for channel prompts (Q1).

- **Route.** `POST /api/hooks/slack`, one URL for the install's Slack app, registered beside
  `GitHubHookPrefix` (`api/rest.go`), left out of the auth middleware and routed by Envoy with
  ext_authz off (`envoy.inboundHooks`). Body cap as `githubHookMaxBody`. Slack's
  `X-Slack-Signature` / `X-Slack-Request-Timestamp` HMAC, checked with the install's one signing
  secret, is the only auth; timestamps over 5 minutes old are refused. `url_verification` is
  answered inline. GitHub's per-address limiter is not reused: every request comes from Slack's
  addresses, so it would throttle all of Slack or nothing.
- **What is accepted.** Only `app_mention` events. Dropped before anything else, with no reply:
  events with `bot_id`, any `subtype`, or `user == slack_workspaces.bot_user_id`. This stops loops
  with AMP's own replies and with other bots that mention `@amp`.
- **Actor.** A `ChannelActor` per project, spawned by the project actor beside its epics
  (`actor/project_actor.go`, `spawnEpicActor`), so cluster placement (`actor/registry.go`,
  `remote.go`) already routes it to the replica that owns the project and its rate state is
  cluster-wide, not per replica. Messages:
  `MsgChannelInbound{team_id, channel_id, event_id, external_user, text, thread_ref}` and
  `MsgAgentReply{session_id, prompt_id, thread_ref, outcome, links}`.
- **Handling, in order.** (1) look up `project_channels` by the event's own `team_id` and
  `channel`; no row, or a row whose `team_id` differs from the payload's, is a refusal (one
  workspace's events never reach another's channel); (2) insert `channel_events` or drop the
  duplicate; (3) refuse if the project switch or channel is off, or if `conversations.info` says the
  channel is shared outside the workspace (`is_ext_shared`, Slack Connect), checked at link time
  and cached for 10 minutes; (4) map `(team_id, external_user)` through `channel_identities`, else
  reply with the link prompt; (5) check the mapped person is still a project member (`authz`);
  (6) apply limits: per person 10 prompts per 10 minutes, per channel 30 per 10 minutes, at most one
  queued prompt per person per channel; over the limit is a short "slow down" reply and
  `channel.message.refused` with `rate_limited`; (7) find or start that person's hosted session for
  the channel's role; (8) send `channel_prompt{session_id, prompt_id, text}` over the hosted control
  channel (§2). The text is tagged `[slack: <AMP user name>]`, the mapped AMP user's name, never the
  Slack display name, which the sender controls (remote prompts are tagged `[remote: <device>]`,
  protocol 5.1, R-M16). The text is always literal; a leading `/` is never a slash command.
- **Channel prompts and signing.** `channel_prompt` is not a relay command and does not pass the
  relay. `serve` accepts it only from the control channel, only as a `prompt`, only for a hosted
  session whose owner is the named person; a laptop harness has no control channel and never
  accepts it. This is the "new command path" the protocol does not have today, stated as such: it
  is authenticated by the pod token, not by a device signature, and its trust root is amp-api (§2).
- **Replies.** When the run ends, `serve` posts `channel_reply{session_id, prompt_id, outcome,
  links}` to `POST /api/internal/hosted/replies` (control channel, pod token). amp-api routes it to
  the project's `ChannelActor` through the actor registry. The reply posted to Slack is built by
  the actor from a fixed template: the outcome plus the kind, ID and link of each AMP object the
  turn created or changed (at most 5), for example "Added to Inbox: item 412 (link)". It carries
  **no titles and no model text**. Titles are model-written: a prompt-injected agent can create or
  rename a ticket or inbox item with a 300-character title that holds confidential project
  content, and a title in the template would post it to a channel whose members need not be
  project members. amp-api builds the list from the audit rows of the turn, not from the pod's
  `links`, so the pod cannot add objects either. Residual risk, stated: the reply still shows the
  outcome, how many objects changed, their kinds and their IDs. An agent could encode a few bits
  in which objects it touches, and Slack sees that the project is active. That is accepted for
  `internal` channels. A channel whose `data_class` is `confidential` or `restricted` gets only the
  outcome and one link to the turn in AMP. The actor writes one outbox row of kind `slack_reply`
  (057; `project_id`, `target_url = chat.postMessage`), sent with the workspace's bot token, not
  HMAC. Failures count on the channel row (`project_channels.consecutive_failures`,
  `last_failure_at`, `last_error`, added in 058), not on `webhooks.consecutive_failures`. That row's `body` holds the reply until delivered and is cleared
  on delivery or final failure; this is the one place Phase 2 stores a payload in Postgres. Note:
  the "webhook actor" in the call is today the outbox `Dispatcher`, not a protoactor actor; this
  spec keeps that and does not convert it.
- **Shared intake.** The manager's natural move for "file this" is `amp_inbox_add` (#161), so a
  Slack request becomes an inbox item with `source = mcp`. Its Slack provenance must come from the
  server, not the model: `amp_inbox_add` takes only `title` + `body` or `from_epic_id`
  (`project-inbox.md`, MCP table), and the inbox sets `source` from the principal's channel, never
  from the caller (inbox D8). So: when amp-api sends `channel_prompt`, it records a prompt context
  `{prompt_id, session_id, agent_user_id, kind: slack, team_id, channel_id, permalink}` that lives
  until the turn ends. During that turn `serve` sends `Amp-Prompt-Id: <prompt_id>` on its `/mcp`
  requests. The model cannot set this header. amp-api accepts it only when the context exists and
  belongs to the calling agent. `CreateInboxItem` then stamps `metadata.via = {kind, team_id,
  channel_id, permalink}`, and callers cannot set `metadata.via` themselves (reserved key,
  refused with 400). This needs a small change to #161's `CreateInboxItem` and metadata rules,
  done in the Phase 2 PR. Q6 asks whether `slack` should instead be a full inbox `source`
  (`source_ref` = permalink), which would be stamped the same way.

### 5. REST and MCP

```
GET    /api/me                                → capabilities.hosted_agent: bool (Helm + org)
PUT    /api/orgs/:id/hosted-agent             → {roles: [...]}           org admins
PUT    /api/projects/:id/hosted-agent         → {enabled: bool}          project owners
POST   /api/projects/:id/agent-sessions       → {role} → RemoteSession (state starting)   members, UI only
GET    /api/agent-sessions/:id                → RemoteSession + harness_pub (owner only)
GET    /api/agent-sessions/:id/events?from_seq=N   (SSE via relay, owner only)
POST   /api/agent-sessions/:id/commands            (prompt | stop; cookie, owner only; amp-api → control channel)
POST   /relay/v1/hosted/sessions/:id/claim    (pod token + claim_secret; relay-internal)
GET    /api/internal/hosted/connect           (hosted control channel; pod token; not public)
POST   /api/internal/hosted/replies           (Phase 2; pod token)
-- Phase 2
GET    /api/admin/slack/install               → Slack OAuth install of AMP's app   AMP admins
GET    /api/projects/:id/channels             POST /api/projects/:id/channels   project owners
DELETE /api/projects/:id/channels/:cid
POST   /api/me/channel-identities/slack/start → link URL ("Sign in with Slack": team_id + user_id)
POST   /api/hooks/slack                       (public; Slack signature)
```

- The `events` route is the browser leg (`relay/browser.go`) mounted under a second prefix and
  checking `kind = 'hosted'`. The `commands` route is not the relay's: amp-api serves it, takes no
  device signature (§2), applies the M1 command allowlist (protocol 5.1) and forwards over the
  control channel. `answer`, `set_model` and the other reserved ops stay refused. The relay's
  command route answers 404 for a hosted session, and `serve` ignores relay commands anyway. Envoy
  gets the same cookie-authenticated, unbounded-timeout route for `/api/agent-sessions` that
  `relay.enabled` adds for `/api/sessions` (`values.yaml`).
- No new MCP tools. The hosted agent uses AMP's existing tools through `/mcp` with its own token.
  `amp_inbox_add` comes from #161.

### 6. UI: the chat drawer

- **Placement.** An **Ask agent** button in the project header opens a right-side drawer, like the
  ticket drawer. It is rendered only when `/api/me` reports `hosted_agent` and the project switch
  is on.
- **Reuse.** The drawer is the session page's transcript and status in a narrower frame: the
  reducer `ui/src/remote/session.ts` (`TranscriptItem`, `Totals`, `Outgoing`), `useRemoteSession.ts`,
  `transcriptView.ts` (thinking label, worker lines) and `components/remote/SessionParts.tsx` and
  `SafeMarkdown.tsx`. `useRemoteSession` takes a **session transport** instead of a hard-coded
  path. The laptop transport is today's: `/api/sessions`, harness pin from `deviceStore`, commands
  signed through the key gate. The hosted transport uses `/api/agent-sessions`, takes the harness
  key from amp-api's `harness_pub` in memory, and sends commands unsigned, by cookie. The hosted
  transport never calls `signCommand`, `ensureDevice` or `pinHarness`, so `keyGate.ts` stays as it
  is and laptop sessions keep their protection.
- **CSP.** The session page is served "with a strict CSP (src/remote/csp.ts)" and is reached by full
  page loads. A drawer inside the main app does not get that header. This matters twice. For
  signing, it is why the drawer uses no device key (§2). For rendering, Phase 1 shows agent text
  only through `SafeMarkdown` (no raw HTML, no images from model output), and Phase 1 verification
  checks that the main app's CSP blocks inline scripts (R2).
- **Controls.** Role menu (org-enabled roles), prompt box, **Stop**, "Open full page" (a plain
  `<a href>` to `/sessions/:id`, which also works for hosted sessions with the hosted transport). No model picker and no approve buttons.

## Alternatives

| Option | Why not |
|--------|---------|
| Drawer drives the person's laptop harness through remote control | Already possible with `/sessions/:id`; fails Jacob's "instead of having to have your TUI open" and stops when the laptop sleeps |
| A new chat backend in amp-api that calls the model directly | Duplicates the harness's roles, policy, workers and repeat guard (`roles.rs`, `policy.rs`, `repeat.rs`); two agent runtimes to keep in step |
| Inbound Slack posts straight into amp-api's service, no actor | Breaks the owner's constraint (01:05:19); loses per-project serialization the inbox's GitHub path also relies on |
| Slack replies through the existing `WebhookFormatSlack` incoming webhook | Incoming webhooks post to one fixed channel and cannot thread; replies would lose context |
| One shared "AMP agent" account for everyone | Audit and RBAC lose the person; the owner wants "<user>'s agent" |
| Operator with a pod per agent in v1 | Right end state (57:54), but it needs a CRD, a controller and pod lifecycle before anyone can chat; Phase 3 |

## Risks

| ID | Risk | Mitigation |
|----|------|------------|
| R1 | Hosted sessions skip laptop pairing, so a stolen Dex cookie can drive the agent | Owner-only sessions; agent limited to one project by `hosted_agent_grants`; org idle timeout applies; `RevokeAllRemote` ends hosted sessions; audit every command |
| R1b | amp-api is the trust root for hosted sessions on both legs: it authenticates drawer commands by cookie (no device signing), sends channel prompts, and serves the harness key the drawer checks events against. Whoever controls amp-api can drive any hosted session and show its owner forged events | Stated in §2, not hidden. The relay alone still can't inject a command or forge an accepted event. The agent can only do what amp-api's RBAC already lets the person do. Laptop sessions keep the full model, and `keyGate.ts` is unchanged |
| R2 | Model output rendered in the main app (not the CSP'd session page); the drawer cannot use the device key there | `SafeMarkdown` only; CSP check in Verification; "Open full page" for anything richer; hosted commands use the cookie, not the key (§2) |
| R3 | One pod runs every person's sessions and holds every active agent's token: a tool escape or pod compromise reaches others' state and can act as those agents | The pod cannot create sessions or name owners (amp-api creates the row; the pod only claims it); tokens are 1-day and one-project, Member-capped; read-only root, non-root, enforced egress (proxy or Cilium FQDN, §1), per-session state dirs; Phase 3 isolates per agent |
| R4 | Prompt injection: the agent reads text others wrote (tickets, inbox items, including external GitHub issue authors via #161) and Slack text, and may act on it | Tagging alone does not stop this. Limits: the agent has one project at Member level; no workers; no approvals off the UI (`InUI()`); tool-call and turn budgets; every write is audited as "<person>'s agent" |
| R4b | Slack impersonation: a forged or cross-workspace event names another person's Slack user | One install-level app and signing secret (no per-project secrets); payload `team_id` and channel must match the linked row; identities keyed on `(team_id, user_id)` from Sign in with Slack |
| R5 | Slack retries or replica failover run a turn twice | `channel_events` primary key; actor placement serializes per project |
| R6 | Data leaves AMP through replies, to channel members who need not be project members | Replies are a fixed template of outcome plus object kinds, IDs and links (links still need AMP login), never titles or model text, built from audit rows; outcome only for `confidential`/`restricted` channels; residual: counts, kinds and IDs still leak; Slack Connect channels refused; `data_class` per channel; Phase 2 is gated on Q5 so the class check can be enforced |
| R7 | Cost and runaways: hosted agents spend provider tokens with no laptop owner watching; bot loops | `hostedAgent.limits` (tool calls per turn, turns per hour, org daily tokens) in v1; per-org usage audit rows; no workers in v1; Phase 2 per-person and per-channel limits in the actor and bot/self/subtype drops. Per-org keys or a monthly cap is Q4 |
| R8 | The relay was built for one laptop per session; a pod holds many | Keep `RemoteMaxLiveSessions` per person; load-test 50 hosted sessions on one pod in Phase 1 |

## Decisions & Assumptions

| ID | Decision or assumption | Why | By |
|----|------------------------|-----|----|
| D1 | The hosted agent is an optional Helm component, off by default, and needs `relay.enabled` | Owner (56:51): "optional thing you can enable for your server"; reuses the relay stream | human |
| D2 | Inbound events go to a Go actor; external replies leave through the webhook outbox | Owner constraint (01:05:19) | human |
| D3 | Roles beyond the manager are in scope, from the harness role registry | Jd (57:40); the registry exists (`roles.rs`) | human |
| D4 | v1 is one shared pod; operator and pod-per-agent come later | Owner (57:54) and #162's phased-plan criterion | human |
| D5 | Each person's hosted session runs as an agent account they manage, labeled "<person>'s agent" | #162 acceptance criteria; audit fields already exist | human |
| D5b | The agent account gets one project through `hosted_agent_grants`, derived per request from the person's access, not team membership | Teams over-grant, miss org admins, and outlive removal | agent |
| D6 | Hosted sessions use no device signing: drawer commands are cookie-authenticated by amp-api and forwarded over the control channel; amp-api serves the harness key; amp-api is the trust root (R1b) | `keyGate.ts` refuses the device key outside a full `/sessions/` load under `SESSION_CSP`; weakening it would weaken laptop sessions; a full-page route is not a drawer and `frame-ancestors 'none'` rules out an iframe | agent |
| D6b | amp-api creates hosted sessions and fixes owner, agent, project and role; the pod only claims an existing ID with a one-time secret | A shared pod must not be able to register sessions as anyone | agent |
| D6c | Hosted agent accounts get tokens only from the internal control path; `CreateToken` refuses everyone else | Otherwise the managing person could mint a 10-year token and bypass the pod's budgets | agent |
| D7 | Hosted sessions are rows in `remote_sessions` with `kind = 'hosted'` | One lifecycle, one revoke path | agent |
| D8 | The drawer reuses the remote session reducer and parts; what changes is the session transport (base path, where the harness key comes from, signed vs cookie commands) | One transcript model; the key gate stays intact | agent |
| D9 | Slack replies use `chat.postMessage` with the workspace bot token as a delivery kind in the generic outbox migration 057, shared with inbox Phase 3 | Threads; incoming webhooks can't; 011's outbox can't carry it; Slack must not wait on GitHub link-back | agent |
| D12 | v1 is Phase 1 only: drawer plus hosted manager, chat and planning, no workers, no channels | Phase 2 has unresolved security gates (§4); bundling does not require shipping an unsettled design | agent |
| D13 | One install-level Slack app (AMP admin OAuth install); no per-project Slack secrets | A project owner's own app could sign events as anyone | agent |
| D14 | Slack replies are actor-built templates from audit rows: outcome, kinds, IDs and links; no titles, no model text | Titles are model-written and could carry project content; residual leak stated in §4 and R6 | agent |
| D10 | No approvals or done-marking from the drawer or a channel | `InUI()` rule stands | agent |
| D11 | Slack requests become inbox items through `amp_inbox_add`, not a new intake path; amp-api stamps `metadata.via` from the prompt context, never the model | #161 owns intake; inbox D8 sets provenance server-side | agent |

## Open questions (for Jacob)

- **Q1.** Hosted sessions make amp-api the trust root (R1b, D6): drawer commands are
  cookie-authenticated with no device signature, amp-api serves the harness key, and in Phase 2 it
  sends channel prompts over the control channel. Is that acceptable? The alternatives were
  weakening `keyGate.ts` (which also weakens laptop sessions) or a full-page session route instead
  of a drawer.
- **Q2.** This spec proposes Phase 1 alone as v1 (D12) and Slack as a second PR once its gates in
  §4 clear. Your bundling rule asks for related changes in one PR; we read it as not requiring an
  unsettled security design to ship with the drawer. Agree?
- **Q3.** Sandbox runtime: do we ship a default `runtimeClassName` (gVisor, Kata, OpenShell), or
  only the slot? Is OpenShell's image something the chart may reference?
- **Q4.** Provider keys and spend: v1 uses one install-wide provider Secret with per-org usage audit
  rows and a per-org daily token cap. Do you want per-org provider keys or a monthly cap instead?
- **Q5.** Data classification: is there an existing class list you use for the audit/SIEM path, or
  are `public | internal | confidential | restricted` fine? Where does a project's class live?
  Phase 2 waits on this answer.
- **Q6.** Should `slack` be an inbox `source` (with `source_ref` = message permalink) so triage shows
  where it came from?
- **Q7.** v1 answers "no workers" (chat and planning only). When workers come, should they run in
  the hosted pod (which then needs git egress, a checkout and repo credentials) or wait for
  Phase 3's pod per agent?

## Implementation plan

v1 is Phase 1 alone, one PR, each commit green (D12). Phase 2 is a second PR once its gates
(§4) clear.

### Phase 1: hosted manager and the drawer (v1)

1. `v2/harness/Dockerfile`; `amp-harness serve` (session per relay assignment, `ManagerFeed`,
   session lock, stop and queue, no supervisor, worker tools removed, budgets), tests with
   `remote/mock_relay.rs`.
2. Migration 056; `domain/remote.go` `Kind`, `Role`, `AgentUserID`, `starting` state; agent-account
   provisioning with `users.hosted_agent`, `hosted_agent_grants` in `authz`, internal-only token
   minting and the `CreateToken` refusal in `service/accounts.go`; audit actions
   `hosted_agent.session.start|end`, `hosted_agent.turn.usage`, `hosted_agent.limit.hit`.
3. Hosted control channel (`/api/internal/hosted/connect`, pod token): `session_start`,
   `session_end`, `command`, `agent_token`; `POST /api/agent-sessions/:id/commands` in amp-api.
4. Relay: `POST /relay/v1/hosted/sessions/:id/claim` (pod token, claim secret, row must exist);
   `/api/agent-sessions/.../events` browser leg; relay command route 404 for hosted; kind checks.
5. REST switches (org roles, project enable), `/api/me` capability.
6. Helm `hostedAgent` block, template, `egress.mode` (`proxy` with NetworkPolicy, or `cilium`
   `toFQDNs`), `limits`, `requirePinnedSecrets` parity with
   relay.
7. UI drawer, `useRemoteSession` session transport (laptop vs hosted), role menu (org-enabled roles that need no checkout).

### Phase 2: Slack inbound (second PR, after the §4 gates)

1. Migration 057 (generic outbox kinds, unless inbox Phase 3 landed it first) and 058; Slack app install (AMP admin OAuth), `slack_workspaces`, channel linking from
   installed workspaces (project owners), Slack Connect refusal.
2. `POST /api/hooks/slack` (signature, timestamp, body cap, bot/subtype/self drops, Envoy route).
3. `ChannelActor` under the project actor: team/channel match, identity link flow on
   `(team_id, user_id)`, per-person and per-channel limits, `channel_prompt` over the control
   channel.
4. `POST /api/internal/hosted/replies`; templated `slack_reply` kind (057) built from audit rows,
   failures on `project_channels`; prompt context, `Amp-Prompt-Id` and the `metadata.via` stamp in
   `CreateInboxItem`.
5. Audit actions `channel.message.received|refused`, `channel.reply.sent|failed`, with
   `data_class` in `Details`; channel-vs-project class check.

### Phase 3: operator and pod per agent (later)

A `HarnessSession` CRD and a controller that runs one pod per hosted agent, with its own PVC,
NetworkPolicy and token Secret; `serve` unchanged. Other channels (Discord, Teams, generic signed
webhook) as new `project_channels.kind` values.

## Verification

- **Off by default.** `helm template` with defaults renders no `hosted-agent` objects;
  `hostedAgent.enabled=true` with `relay.enabled=false` fails to render. `/api/me` reports no
  `hosted_agent`; the drawer button is absent (UI test).
- **Identity.** Go tests: Dana starts a hosted session; the agent account has `ManagedBy = Dana`,
  no teams, and one `hosted_agent_grants` row; it gets 403 on another project Dana's team links
  to; a write it makes produces an audit row with `actor_type=agent`, `on_behalf_of_user_id=Dana`.
  An org admin with no team gets a working agent. Removing Dana from the team makes the agent's
  next call 403 with its token still unexpired, and ends the session (`EndRemoteWithoutAccess`).
  `CreateToken` for the hosted agent is refused for Dana and for a global admin; Dana can revoke
  its token, which ends the session.
- **Session creation.** Go and relay tests: a claim for an ID amp-api did not create, a second
  claim, or a wrong `claim_secret` is refused; the claim cannot change the row's owner, agent or
  project; a row with no claim after 30 s ends with `start_timeout`.
- **No workers, budgets.** Harness test: in `serve` mode no role lists `amp_run_ticket` and the
  supervisor does not start; a turn over `toolCallsPerTurn` ends with `hosted_agent.limit.hit`.
- **Trust root.** Harness test: in `serve`, a command for a hosted session that arrives through
  the relay is ignored, and one from the control channel runs. Relay test: the relay's command
  route is 404 for a hosted session, and the relay cannot reach `/api/internal/hosted/` (no pod
  token). UI test: the hosted transport never calls `signCommand`, `ensureDevice` or `pinHarness`,
  writes no `deviceStore` pin, and drops an event not signed by the session's `harness_pub`.
  `keyGate.test.ts` and `csp.test.ts` pass unchanged.
- **Egress.** `helm template` with `hostedAgent.enabled=true` and no `egress.mode` fails. On a
  cluster with an enforcing CNI, a pod `curl` to `github.com` fails and one to a provider host in
  `allowHosts` succeeds, in both `proxy` and `cilium` modes.
- **Owner only.** Another member and a global admin get 404 on Dana's `/api/agent-sessions/:id`.
- **Allowlist.** A `set_model` command is refused with `unknown_op`; a prompt starting with `/` is
  run as text (harness test).
- **Laptop path unchanged.** `relay/hardening_test.go` and `vectors_test.go` pass; an agent token on
  `POST /relay/v1/sessions` is still refused.
- **Drawer.** Vitest: the drawer reduces the same fixture events as `Session.test.tsx` to the same
  transcript; Stop sends `stop` and clears the queue. Manual: run the hosted pod on docker-desktop
  (KB how-to/remote-control-local-test.md), ask the manager to add a story, see it on the board.
- **CSP.** The main app's response header blocks inline script; a model reply containing
  `<img src=x onerror=...>` renders as text.
- **Slack (Phase 2).** Go tests: a bad signature or a timestamp over 5 minutes old is 401; an event
  whose `team_id` or channel does not match a linked row is refused; the same Slack user ID in
  another `team_id` does not map; `bot_id`, `subtype` and self events are dropped; a duplicate
  `event_id` runs once; an unlinked user gets the link reply and no prompt; a disabled project or a
  Slack Connect channel is 200 with a `channel.message.refused` audit row; the 11th prompt in 10
  minutes from one person is refused `rate_limited`; the prompt tag carries the AMP user name; a
  good mention produces one `channel_prompt` and one templated `slack_reply` delivery with no model
  text and no titles (an inbox item titled with a secret string does not appear in the reply body),
  retried on a 500 from a mock Slack, body cleared after delivery, failures counted on
  `project_channels`; a `confidential` channel's reply has only the outcome and one link. The
  inbox item the turn creates has `metadata.via.kind = slack`; an `amp_inbox_add` call that passes
  `metadata.via` itself is 400, and a forged `Amp-Prompt-Id` stamps nothing.
- **Classification (Phase 2).** Reply and receive audit rows carry the channel's `data_class`; a
  channel whose class is below its project's cannot be created.
