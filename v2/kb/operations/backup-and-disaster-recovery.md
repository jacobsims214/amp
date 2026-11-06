---
title: Backups and disaster recovery
tags: [operations, backup, disaster-recovery, restore, postgres, typesense, cnpg, runbook]
---

# Backups and disaster recovery

This page covers what to back up so an AMP can be rebuilt after losing its cluster, and how to
rebuild it. A restore is the same procedure as a move (`operations/migrating-to-a-new-cluster.md`,
phases 1, 4 and 5), with backups standing in for the old stack.
`operations/where-amp-keeps-data.md` lists what must be kept.

## What to back up

| What | How | How often |
|---|---|---|
| Postgres `amp` and `dex` | CNPG continuous backup (`barmanObjectStore` on the `Cluster`, to S3-compatible storage) plus a `ScheduledBackup`, so you can restore to any point in time | Continuous WAL, base backup daily |
| Postgres, a second way | `pg_dump -Fc` of both databases to off-cluster storage (Phase 3 of the move) | Daily, and before every upgrade |
| **The knowledge bases (Typesense)** | Export each KB collection's schema and documents (Phase 3 of the move), to the same place as the dumps | Daily, at the same time as the dump |
| Secrets and values | The values file in git; secrets (`global.secretOverrides`, `typesense.apiKey`, SSO connector secrets) in your secret store | On every change |

- **Typesense is the only copy of every project, team and org KB.** A Postgres backup without a
  KB export restores every project with an empty knowledge base.
- **Take the KB export and the Postgres dump together,** because KB collection names carry
  Postgres IDs.
  - A KB a few minutes older than the database is fine: the latest writes are missing, but
    nothing is mismatched.
  - A KB from a different day than the database can point at the wrong projects. Don't restore
    one.
- **Today's chart doesn't configure any of this.** `values-production.yaml` has a commented-out
  `barmanObjectStore` block. Until it's set up, the daily dump and KB export are the only backups.
  - If you're an agent and find no backups configured, tell your person. It's the largest risk
    in an AMP install.

## SeaweedFS + CNPG barmanObjectStore

CNPG's continuous backup writes to S3-compatible storage. On your own hardware, run SeaweedFS
(open source, Apache-2.0) and use its S3 gateway. Set `endpointURL` on the `barmanObjectStore`
block to the gateway, and create a bucket for the backups. AMP's bundled SeaweedFS
(`seaweedfs.enabled`, for receipt evidence) works too. Use its own bucket and credentials, and
remember that backups stored inside the same cluster don't survive losing that cluster. Any
S3-compatible endpoint works, including AWS S3 or Backblaze B2. The chart wires it up for you.

1. Create the bucket (for example `amp-backups`) and a key pair that can read and write only it.
2. Create the Secret in AMP's namespace, out-of-band so the keys never sit in values files:
   `kubectl -n amp create secret generic amp-backup-creds --from-literal=ACCESS_KEY_ID=... --from-literal=SECRET_ACCESS_KEY=...`
3. Turn it on (it is off by default):
   ```yaml
   cnpg:
     backup:
       enabled: true
       destinationPath: s3://amp-backups/
       endpointURL: https://seaweedfs-s3.example.com   # empty for AWS S3
       existingSecret: amp-backup-creds
       retentionPolicy: 30d
       schedule: "0 0 3 * * *"                         # CNPG cron, seconds first
   ```
   `values-production.yaml` carries the same block, ready to flip on. The chart refuses to render
   if `destinationPath` or `existingSecret` is missing.
4. Check it:
   - `kubectl -n amp get backup,scheduledbackup` shows a `completed` base backup (one is taken
     right away).
   - `kubectl cnpg status amp-postgres -n amp` shows continuous archiving working, and
     `kubectl -n amp get cluster amp-postgres -o jsonpath='{.status.conditions[?(@.type=="ContinuousArchiving")].status}'`
     prints `True`. A completed base backup alone doesn't prove WAL is being archived.
   - `kubectl -n amp get cluster amp-postgres -o jsonpath='{.status.firstRecoverabilityPoint}'`
     shows the earliest point you can restore to.

Status: untested. Nobody has yet run WAL archiving or the restore below against a real store.
Do the check above and the practice restore at the end of this page, and write down what you
find.

### Restoring Postgres from the object store

The chart can bootstrap its own Cluster from a backup, so Helm owns the restored cluster and
nothing is created by hand. Set `cnpg.recovery` and point `cnpg.backup.destinationPath` at a
**new** path in the same release. CNPG refuses to archive into a path that holds another
timeline's WALs, and the chart refuses to render if the two paths (and server names) match.

```yaml
cnpg:
  backup:
    enabled: true
    destinationPath: s3://amp-backups/restored-2026-11-05/   # NEW path, not the old one
    endpointURL: https://seaweedfs-s3.example.com
    existingSecret: amp-backup-creds                         # same store, same keys
  recovery:
    enabled: true
    sourcePath: s3://amp-backups/                            # the OLD destinationPath
    sourceServerName: amp-postgres                           # the OLD cluster's name
    # targetTime: "2026-11-04 14:30:00+00"                   # omit for the latest WAL
```

The recovery bootstrap sets `database: amp` and `owner: amp` from `cnpg.appDatabase` and
`cnpg.appOwner`. Without them CNPG would create an `app` role and a `<cluster>-app` Secret with
the wrong user, and amp-api, jobs and dex could not log in. The relay's managed role and its
password Secret are still rendered by the chart, so CNPG re-creates the role on the restored
cluster. The `dex` database comes back with the data.

**Never restore over the only copy.** The old cluster's volumes may hold writes newer than the
last archived WAL.

1. **Rehearse first (scratch namespace).** Every chart object takes its namespace from
   `global.namespace`, not from `helm install -n`, so a second release must set it or it renders
   into `amp` and collides with the production release. Releasing into the same namespace is not
   supported: fixed names (`amp-dex-db`, `amp-api`, Services, Secrets) would collide whatever
   `cnpg.clusterName` is.
   - `kubectl create namespace amp-scratch`
   - Create the backup Secret there too (the restore reads the store with it):
     `kubectl -n amp-scratch create secret generic amp-backup-creds --from-literal=ACCESS_KEY_ID=... --from-literal=SECRET_ACCESS_KEY=...`
   - `helm install amp-scratch ./v2/deploy/helm/amp -n amp-scratch -f <your values> --set global.namespace=amp-scratch`
     with the `cnpg.backup` and `cnpg.recovery` values above.
   - Wait for the Cluster to be `Cluster in healthy state`, then run the verification in step 5.
     The source store is only read, so this is safe.
2. **For a real disaster, build into a fresh release or namespace** (the "Restoring after a
   disaster" steps below). The old Cluster is already gone.
3. **Restoring in place under the same release is destructive.** `helm upgrade` renders a
   different Cluster spec (recovery instead of initdb), and CNPG will not re-bootstrap a running
   cluster. You have to delete the Cluster and its PVCs first. Before that:
   - keep the old PVCs (`kubectl -n amp get pvc -l cnpg.io/cluster=amp-postgres`), or take a
     `pg_dump -Fc` of both databases from the old cluster if it still runs;
   - set `cnpg.backup.destinationPath` and `cnpg.recovery` in the **same** upgrade, so the new
     cluster never archives into the old path.
4. Do not rename `cnpg.clusterName` to move to a restored cluster. Helm would prune the old
   Cluster and CNPG would delete its volumes, and the `Database` and `ScheduledBackup` objects
   would be re-pointed in the same step.
5. Check the restored cluster: `kubectl cnpg status <cluster> -n <ns>` is healthy,
   ContinuousArchiving is `True` against the new path, and AMP signs in and shows its projects.
6. Once it is verified, remove `cnpg.recovery` from the values. It is only used at first
   bootstrap, and leaving it set confuses the next reader. Then continue with the KB restore and
   the verification steps below.

## How much can be lost

- **With CNPG continuous backup:** Postgres loses seconds. The KBs lose everything written since
  the last KB export.
- **With daily dumps only:** up to a day of everything.
- **Lowering the KB loss** means exporting the KBs more often (the export reads Typesense and
  doesn't block anyone) or snapshotting Typesense's volume.
  - KB docs written by agents can often be recovered from the ticket comments and runs that
    produced them.

## Restoring after a disaster

1. Build a new stack on the same AMP version, with the old values and pinned secrets (move,
   Phase 1).
2. Stop its writers (move, Phase 4 step 1).
3. Restore Postgres, from either source:
   - **From CNPG backups:** set `cnpg.recovery` and a new `cnpg.backup.destinationPath` (see
     above), so the chart bootstraps the Cluster with `bootstrap.recovery` instead of `initdb`.
   - **From dumps:** `pg_restore` both databases (move, Phase 4 step 2).
4. Restore the KBs from the export that matches the database (move, Phase 4 step 3).
5. Start and verify (move, Phase 5). Tell people what was lost: anything after the backup time.
6. Look for KB docs written after the KB export but before the disaster. Agents' ticket comments
   and run summaries usually say what they wrote. Rewrite the ones that matter.

## Practise it

A backup nobody has restored is a guess. Each quarter, and after a large upgrade, restore the
latest backups into a scratch namespace or cluster:

- Run Phase 5's checks against it.
- Write down how long it took and anything that surprised you.
- Add what you learned to this page.
