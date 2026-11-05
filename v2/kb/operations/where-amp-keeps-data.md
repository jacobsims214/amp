---
title: Where AMP keeps its data
tags: [operations, migration, backup, disaster-recovery, postgres, typesense, dex, data, receipts, evidence]
---

# Where AMP keeps its data

Read this before you back up, restore or move an AMP. One AMP stack (the Helm chart in
`v2/deploy/helm/amp`, namespace `amp` by default) keeps its state in five places, and **a
Postgres dump alone is not enough**. The knowledge bases live only in Typesense, and receipt
evidence only in its store (a volume by default, or S3).

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
| **Receipt evidence files** (screenshots, videos, logs workers upload) | **The evidence store only.** By default the `amp-receipts` volume, mounted at `/data/receipts` (`RECEIPT_DIR`): a PVC (ReadWriteMany on `nfs-storage` in production, ReadWriteOnce by default; `ampApi.receipts.storage`), or the `amp-receipts` docker volume. Under `make dev`, `v2/amp-api/data/receipts/`. With `RECEIPT_STORE=s3`, a bucket instead: the bundled SeaweedFS (its PVC `data-amp-seaweedfs-0`, or the `amp-seaweedfs` docker volume) or an S3 store you run. See "Receipt evidence in S3 or SeaweedFS" below. | Not in Postgres and not artifacts. Files (objects, in S3) are keyed `<project>/<ticket>/attempts/<receipt>/<tree>/<name>` and the rows name the keys, so the store and the dump must come from the same moment. Empty while every project's receipts setting is Off. The PVC is covered by `nfs-storage` backups in production. |
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
`ampApi.receipts.storage.size` (default 20Gi), or `seaweedfs.storage.size` (default 20Gi) with
the bundled SeaweedFS.

If the store can't start (a missing or read-only volume, a bad S3 setting, or S3 credentials the
store refuses), amp-api logs it and keeps running:
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
  chart refuses to render otherwise. With S3 there is no shared volume, so any number of replicas
  works.

## Receipt evidence in S3 or SeaweedFS

amp-api can keep evidence in any S3-compatible bucket (`RECEIPT_STORE=s3`) instead of on a
volume. The chart and compose bundle **SeaweedFS** for that. SeaweedFS is Apache-2.0, as is
`minio-go`, the client amp-api uses. It is opt-in and off by default, and production
(`values-production.yaml`) stays on the PVC. Switching is config only.

Whichever store you use, **amp-api still serves every file**. It does the members-only check,
Range, and the same safety headers. SeaweedFS is never exposed: it has no host port, no Envoy
route and no Ingress. It keeps three layers:

- **Loopback.** Its master, volume and filer listen on loopback inside its pod or container.
  Iceberg and Lance are off. Only the S3 gateway, which needs the key pair, is on the network,
  along with its gRPC port; SeaweedFS has no flag to move that one.
- **A signing key.** The gRPC port's admin calls (identities, policies, lifecycle deletes) and
  filer writes need a token signed with SeaweedFS's own key: `jwt-signing-key` in
  `amp-seaweedfs-s3`, or `SEAWEEDFS_JWT_SIGNING_KEY` in compose. Without it, as with an
  `existingSecret` that lacks the key, SeaweedFS still refuses those calls from other hosts, but
  allows them from inside its pod.
- **A NetworkPolicy, in Helm.** It admits only amp-api, only to the S3 port. It is rendered
  whenever `seaweedfs.enabled` is set, whatever `networkPolicy.enabled` says, and it needs a CNI
  that enforces NetworkPolicy. Without such a CNI (kind, Docker Desktop, flannel alone), the S3
  port and the gRPC port are reachable from any pod in the cluster. Both still need their
  credentials or a signed token. **Don't add
bucket lifecycle rules.** AMP's own sweep deletes unkept evidence after `RECEIPT_RETENTION_DAYS`.
A bucket rule can't tell kept receipts from unkept ones, so it would delete both.

**Compose** (local):

```sh
cd v2
docker compose -f docker-compose.yml -f docker-compose.seaweedfs.yml --profile seaweedfs up -d
```

`docker-compose.seaweedfs.yml` sets amp-api's `RECEIPT_STORE=s3` and points it at
`seaweedfs:8333`. The key pair is `SEAWEEDFS_ACCESS_KEY` / `SEAWEEDFS_SECRET_KEY`, with local-dev
defaults. Those defaults are well known, so set both variables on any shared host. The identity
is allowed bucket `amp-receipts` only, so if you change the bucket, change it in both files. Data
is in the `amp-seaweedfs` volume. A plain `docker compose up` is unchanged. The override needs
the profile (or `COMPOSE_PROFILES=seaweedfs`), and the stack needs Docker Compose v2.23.1 or
later, for the inline `configs:` entry.

**Helm**: `--set seaweedfs.enabled=true`. This renders:

- a one-replica StatefulSet `amp-seaweedfs` running `weed server -s3`, non-root (uid 1000), with
  telemetry off and its own PVC (`seaweedfs.storage`);
- a ClusterIP Service on the S3 port only;
- a key Secret, `amp-seaweedfs-s3`, which is random and kept across upgrades. Under ArgoCD, pin
  it with `global.secretOverrides.seaweedfsAccessKey`, `seaweedfsSecretKey` and
  `seaweedfsJwtSigningKey`, or bring your own with `seaweedfs.existingSecret` (keys `access-key`,
  `secret-key`, and `jwt-signing-key`, which is recommended). Changing the chart's keys restarts
  SeaweedFS. After rotating an existing Secret, restart `amp-seaweedfs` and `amp-api`
  together.

amp-api creates the bucket on first start. The `amp-receipts` PVC is no longer rendered; an
existing one is kept, with its files.

**An S3 store you run** (R2, AWS S3, Garage): set `ampApi.receipts.s3.endpoint` (for example
`https://<account>.r2.cloudflarestorage.com`, with `region: auto` for R2) and
`ampApi.receipts.s3.existingSecret`, a Secret with `access-key` and `secret-key`. The keys need
read, write and delete on the objects, and `s3:ListBucket` on the bucket: amp-api checks the
bucket with HeadBucket at startup and leaves the store out if that is denied. They also need
create if the bucket doesn't exist yet. Only SeaweedFS is tested; any store that speaks S3
should work, and one that rejects conditional PUTs (`If-None-Match`) gets a plain PUT. Set
either this or `seaweedfs.enabled`, never both; the chart refuses to render with both.

**Moving existing evidence.** Switching stores doesn't copy anything. Files already on the volume
stay there and are unreachable while amp-api uses S3. To bring them along, copy the volume's tree
into the bucket under the same keys (for example `aws s3 sync /data/receipts
s3://amp-receipts/ --exclude '.tmp/*'` with the endpoint set). Do it while amp-api is stopped,
or right after switching, before workers upload again.

**Backups.** Back up the evidence store with the database: the rows name the keys.

- Bundled SeaweedFS: back up its PVC (`data-amp-seaweedfs-0`) with the rest of the cluster's
  volumes. Or run `weed filer.backup` to stream the filer to another store continuously.
- A store you run: use that store's own replication or versioning.

**Sizing.** SeaweedFS makes a collection per bucket, made of volume files of up to 1 GiB
(`-master.volumeSizeLimitMB=1024`, the image's default). One bucket is all AMP uses. A full disk
shows up as "receipt store s3: We encountered an internal error" on upload, so size
`seaweedfs.storage.size` like the PVC it replaces.

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
