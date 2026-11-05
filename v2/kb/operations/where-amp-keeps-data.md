---
title: Where AMP keeps its data
tags: [operations, migration, backup, disaster-recovery, postgres, typesense, dex, data, receipts, evidence]
---

# Where AMP keeps its data

Read this before you back up, restore or move an AMP. One AMP stack (the Helm chart in
`v2/deploy/helm/amp`, namespace `amp` by default) keeps its state in five places, and **a
Postgres dump alone is not enough**. The knowledge bases live only in Typesense, and receipt
evidence only on its volume.

## What must move

| Data | Where | Notes |
|---|---|---|
| Projects, epics, stories, tickets, dependencies, comments, history, runs, trash, day reports, metadata | Postgres, database `amp` | The core of AMP. |
| Orgs, teams, members, invites, roles, project access | Postgres `amp` | |
| AMP users and API tokens | Postgres `amp` (`users`, `api_tokens`) | Tokens are stored as SHA-256 hashes with no server secret, so they keep working on a new stack. |
| Artifacts (mockups, diagrams, images, documents) and every version | Postgres `amp` (`artifacts`, `artifact_versions`, content in `BYTEA`) | Usually the largest part of the dump. Retention dates come along unchanged. |
| Design system versions, audit log (with its hash chains, checkpoints and webhook outbox), webhooks and their secrets | Postgres `amp` | Restore audit tables with a full dump, or a data-only restore with `--disable-triggers`. See `admin/trash-audit-and-projects.md`. |
| Audit checkpoint signing key | Kubernetes Secret named in `ampApi.audit.signingKey.existingSecret` (never in the database) | Move it with the database. Without it, `audit verify` can't check old signatures unless you list its public key in `ampApi.audit.retiredPublicKeys`. |
| Verification receipts and their checks; evidence **metadata** (name, size, sha256, status) | Postgres `amp` (`task_receipts`, `receipt_checks`, `receipt_evidence`) | Small. See `working/verification-receipts.md`. |
| **Receipt evidence files** (screenshots, videos, logs workers upload) | **The `amp-receipts` volume only**, mounted at `/data/receipts` (`RECEIPT_DIR`): a PVC (ReadWriteMany on `nfs-storage` in production, ReadWriteOnce by default; `ampApi.receipts.storage`), or the `amp-receipts` docker volume. Under `make dev`, `v2/amp-api/data/receipts/`. | Not in Postgres and not artifacts. Files are keyed `<project>/<ticket>/attempts/<receipt>/<tree>/<name>` and the rows name the keys, so the volume and the dump must come from the same moment. Empty while every project's receipts setting is Off. Covered by `nfs-storage` backups in production. |
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

## Receipt evidence: size and retention

Evidence is written once per key and never edited. Unkept evidence (a receipt whose ticket hasn't
merged and that no person accepted) is deleted after `RECEIPT_RETENTION_DAYS` (default 90) by an
hourly sweep on one replica; its rows stay, marked expired, so the drawer says "Evidence expired
on <date>". Kept evidence lives until its ticket is purged from the trash (or its project is
reset). Each file is at most `RECEIPT_MAX_BYTES` (default 200 MiB). Plan for about 5–20 MB per
recorded journey; screenshots alone are far less. The PVC's size is
`ampApi.receipts.storage.size` (default 20Gi).

If the store can't start (a missing or read-only volume), amp-api logs it and keeps running:
evidence uploads are refused, and a visual check can't count, so Require holds those tickets for a
person. While it is down, purging tickets from the trash waits and a project reset says "try
again", so no file is ever left on the volume with no row pointing at it.

Things to know when operating it:

- **One file that can't be removed blocks the whole trash purge** (tickets, epics, stories and KB
  docs alike) until it is fixed: the purge deletes a ticket's evidence files before its rows, and
  retries hourly with a warning in amp-api's log ("trash purge failed"). Fix the volume (or the
  file's permissions) and the next pass catches up.
- **Upload URLs appear in access logs.** The one-time token is in the path
  (`/api/receipt-uploads/<token>`), so Envoy's and nginx's access logs record it. It is single use,
  valid for 15 minutes, and only accepts bytes with the size and sha256 the worker declared, so a
  logged token can't be used to store anything else.
- **Deleting a project outright.** amp-api has no hard project delete today (archive and reset
  only). Anything that adds one must call `service.DeleteProjectReceipts` first: the receipt rows
  cascade with the project, but the files would stay on the volume. Deleting a project by hand in
  SQL leaves its files under `<project id>/` on the volume; remove that directory too.
- **Access mode.** The chart's default is ReadWriteOnce, which works with one amp-api replica on
  any cluster. More than one replica needs ReadWriteMany (production sets it, on `nfs-storage`); the
  chart refuses to render otherwise.

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
- runs, history, artifacts or receipts (and their evidence);
- team and org KBs.

Use them to copy a plan. To move or back up an AMP, follow `operations/migrating-to-a-new-cluster.md`
and `operations/backup-and-disaster-recovery.md`.
