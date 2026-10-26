---
title: Where AMP keeps its data
tags: [operations, migration, backup, disaster-recovery, postgres, typesense, dex, data]
---

# Where AMP keeps its data

Read this before you back up, restore or move an AMP. One AMP stack (the Helm chart in
`v2/deploy/helm/amp`, namespace `amp` by default) keeps its state in four places, and **a
Postgres dump alone is not enough**. The knowledge bases live only in Typesense.

## What must move

| Data | Where | Notes |
|---|---|---|
| Projects, epics, stories, tickets, dependencies, comments, history, runs, trash, day reports, metadata | Postgres, database `amp` | The core of AMP. |
| Orgs, teams, members, invites, roles, project access | Postgres `amp` | |
| AMP users and API tokens | Postgres `amp` (`users`, `api_tokens`) | Tokens are stored as SHA-256 hashes with no server secret, so they keep working on a new stack. |
| Artifacts (mockups, diagrams, images, documents) and every version | Postgres `amp` (`artifacts`, `artifact_versions`, content in `BYTEA`) | Usually the largest part of the dump. Retention dates come along unchanged. |
| Design system versions, audit log (with its hash chains, checkpoints and webhook outbox), webhooks and their secrets | Postgres `amp` | Restore audit tables with a full dump, or a data-only restore with `--disable-triggers`. See `admin/trash-audit-and-projects.md`. |
| Audit checkpoint signing key | Kubernetes Secret named in `ampApi.audit.signingKey.existingSecret` (never in the database) | Move it with the database. Without it, `audit verify` can't check old signatures unless you list its public key in `ampApi.audit.retiredPublicKeys`. |
| Sign-in: local passwords, registered MCP clients, refresh tokens, Dex's signing keys | Postgres, database `dex` (same CNPG cluster) | Only Dex reads it. With SSO connectors there are no passwords here, but the rest still matters. |
| **Project, team and org knowledge bases** | **Typesense only**, one collection per KB: `kb_<project id>`, `kb_team_<id>`, `kb_org_<id>` | **No copy anywhere else.** Collection names carry Postgres IDs, so the KBs and the database must come from the same moment. |
| Secrets the stack was installed with | Kubernetes Secrets, ideally pinned in `global.secretOverrides` | `amp-oauth2-proxy` (client and cookie secret), `amp-valkey-users`, `amp-bootstrap-admin`, `typesense.apiKey`, any SSO connector secrets. |
| The domain | `global.domain` | It's the issuer of every token and in every link people have saved. Keeping it makes a move nearly invisible. See `operations/migrating-to-a-new-cluster.md`. |

## What doesn't need to move

| Data | Why |
|---|---|
| The global KB (`kb_global_0`, the AMP guide) | Synced from the amp-api image on every start. |
| Ollama's embedding model | The post-install job pulls it. |
| The KB indexing queue (Valkey) | Transient, but **drain it first**. A KB write waits in this queue until it reaches Typesense, so a write still queued at export time is lost. |
| `api_nodes`, `project_owners`, `live_events` | Replica bookkeeping. Stale rows from the old stack expire on their own within about 10 seconds. |
| TLS certificates, internal CA, Dex gRPC certificates | cert-manager issues new ones. |
| Browser sessions | Survive only if `oauth2ProxyCookieSecret` is the same. Otherwise people sign in again, which does no harm. |

## How the pieces depend on each other

- **Typesense collections are keyed by Postgres IDs.** `kb_14` belongs to project 14. Restore the
  database as a dump, never by re-importing projects one at a time, which would give them new
  IDs. The KBs and the dump must also come from the same moment.
- **Users are matched by Dex subject, then by email.** If the `dex` database isn't moved, or
  people's identities are recreated, AMP re-links each person by email on first sign-in. Their
  tickets, roles and tokens are kept.
- **A KB collection's embedding settings name the Ollama service URL** (default
  `http://ollama.amp.svc.cluster.local:11434`). That is only the same on the new stack when the
  namespace is the same.
- **amp-api runs its migrations on start and only goes forward.** Restore a dump into a stack
  running the same AMP version as the source, then upgrade.

## Project export is not a backup

`amp_export_project` / `amp_import_project` copy one project's plan and KB docs as JSON into a
new project with new IDs. They don't carry:

- people, orgs or teams;
- runs, history or artifacts;
- team and org KBs.

Use them to copy a plan. To move or back up an AMP, follow `operations/migrating-to-a-new-cluster.md`
and `operations/backup-and-disaster-recovery.md`.
