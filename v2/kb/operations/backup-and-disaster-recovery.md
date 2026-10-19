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
   - **From CNPG backups:** create the cluster with `bootstrap.recovery` from the object store
     (optionally a `recoveryTarget` time) instead of the chart's `initdb`. See the CloudNativePG
     docs for your operator version.
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
