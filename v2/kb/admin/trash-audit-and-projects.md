---
title: Trash, audit log, and project lifecycle
tags: [trash, restore, audit, audit-integrity, retention, privacy, archive, export, import, webhooks]
---

# Trash, audit log, and project lifecycle

## Trash

Deleted tickets, stories and epics are kept for 30 days: tickets with their comments, history,
runs and links; stories and epics with their tickets (and an epic with its stories and
metadata), all under their original IDs.
- UI: **Settings → Trash**. A deleted epic or story is listed once, with the tickets that went
  with it.
- Agents: `amp_list_deleted_tasks`, then `amp_restore_tasks` with `task_ids`, `story_ids` and/or
  `epic_ids`.
Restore rather than re-create: the history comes back, and restored tickets are re-linked to each
other and to the rest of the plan. A ticket needs its story and a story its epic; restore them in
the same call and AMP brings them back in order. The person who deleted something, or a lead,
can restore it.

## Audit log

Every change to access (org, teams, project links, roles, accounts, tokens) and to settings is
recorded: **Settings → Audit log** for a project, or `amp_audit_log`.

### Append-only and tamper-evident

- **Nobody can edit or delete an entry by SQL.** A database trigger refuses `UPDATE`, `DELETE`
  and `TRUNCATE` on `audit_log` for every role, AMP's own and superusers included. It makes two
  exceptions:
  - Deleting a user still works. The entry keeps the actor's email, and `actor_user_id` becomes
    empty.
  - Retention can delete old entries (below).
- **Every entry is hash-chained.** Each org has its own chain (`org:<id>`); entries with no org
  are in `global`. Each entry stores its position (`chain_seq`), the previous entry's hash and
  its own hash, `sha256(previous hash + the entry's contents)`. Changing, removing or reordering
  an entry breaks the chain. Entries and audit webhooks carry `chain_key`, `chain_seq` and
  `hash`, so a copy kept elsewhere (a SIEM) can prove later what an entry said.
- **Signed checkpoints.** Every hour amp-api signs the newest hash of each chain that moved,
  using an Ed25519 key kept in a Kubernetes Secret, never in the database. It also records the
  checkpoint as an `audit.checkpoint` entry, so the signature goes out through audit webhooks
  too. Someone with full database access could turn the trigger off, rewrite history and
  recompute every hash, but they can't produce a signature for the new hashes.

### Checking it

- **REST:** `GET /api/audit/verify?org_id=<id>` checks one org's chain (org owners and admins,
  and global admins). Without `org_id` it checks every chain (global admins only).
- **CLI**, in an amp-api pod (it reads `DATABASE_URL` and the key from the environment):
  `kubectl -n amp exec deploy/amp-api -- ./amp-api audit verify [-org <id>] [-json]`. It exits 1
  if it finds anything.
- What it reports:
  - `altered`: an entry's contents don't match its hash.
  - `missing`, `missing_start` and `missing_tail`: entries were deleted outside retention.
  - `broken_link`: entries were reordered or replaced.
  - `checkpoint_mismatch`: history was rewritten after it was signed.
  - `signature`: a checkpoint has a bad signature, an unknown key, or no signature although
    earlier checkpoints were signed.
- Warnings, which don't fail the check: unsigned checkpoints (no key was configured), and entries
  newer than the last signed checkpoint.

### Retention

- `AUDIT_RETENTION_DAYS` (Helm `ampApi.audit.retentionDays`) defaults to **365**. `0` keeps
  entries forever.
- Once a day, one replica runs the purge, chain by chain:
  - It signs an **anchor** checkpoint at the last entry it will delete.
  - It deletes the entries up to that point through the database's `audit_purge()` function.
    That is the only path the trigger allows, and only for entries an anchor covers.
  - It records `audit.purge`, with counts per chain, and an `audit.checkpoint` entry for each
    anchor.
- Verification starts from the newest anchor, so the remaining chain still checks out.
- Only the oldest entries of a chain are deleted. If an old entry comes after a newer one, it
  waits for a later run.

### Personal data

Entries can hold IP addresses, user agents and email addresses, which are personal data:
- **Why they're kept:** for security monitoring, under legitimate interest (GDPR Art. 6(1)(f)).
- **For how long:** until the retention period runs out, then they are deleted with the entry.
- **No edits in place:** entries are never redacted or truncated in place, since that would break
  the chain.
- **Erasure requests:** an erasure request doesn't remove security log entries before their
  retention period ends. Say so in your privacy notice.
- **Access requests:** answer them with the entries filtered by actor.

### Signing key (operators)

1. Make a key with `amp-api audit keygen -out signing-key`. It writes the private key to that
   file with mode 0600, and prints only the public key and its id.
2. Store it in a Secret:
   `kubectl -n amp create secret generic amp-audit-signing --from-file=signing-key=signing-key`.
3. Set `ampApi.audit.signingKey.existingSecret=amp-audit-signing`, then delete the file.
4. Keep the public key. After a rotation, list the old public keys in
   `ampApi.audit.retiredPublicKeys`, so checkpoints signed earlier still verify.

Without a key, amp-api starts normally and logs a warning. Checkpoints are written unsigned, and
verify reports them as unsigned. A key it can't read is treated the same way, and its value is
never logged.

Restoring the database:
- A full `pg_restore` works as is.
- A **data-only** restore into a migrated database must use `--disable-triggers`. Otherwise
  the chain trigger renumbers the entries it loads.

## Webhooks

Projects and orgs can send events to your own endpoints (**Settings → Webhooks**), e.g. to post
completions into Slack or update another tracker.

Audit entries reach webhooks through a database outbox (`audit_outbox`). The trigger that
chains an entry also queues it, so an entry is delivered at least once, even if amp-api restarts
or is busy. The envelope `id` of an audit delivery is stable (`evt_audit_<entry id>`), so a
receiver can drop a repeat.

## Project lifecycle

- **Archive** a finished project (read-only; `amp_archive_project`, owners), and restore it later.
- **Export / import** a project's plan as JSON (`amp_export_project`, `amp_import_project`). This copies one
  project under new IDs. To move or back up a whole AMP, see
  `operations/migrating-to-a-new-cluster.md` and `operations/backup-and-disaster-recovery.md`.
- **Reset** clears a project's work (danger zone; owners only).
