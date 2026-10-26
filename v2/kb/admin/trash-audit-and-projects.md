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
recorded, and so are refused attempts (`outcome: denied`). Where to read it:

- **Admin → Audit log** for global admins (every entry), **Teams & orgs → Audit log** for org
  owners and admins, **Settings → Audit log** for project owners, or `amp_audit_log`.
- Each entry shows who (person, agent or AMP), the action and its outcome, the client (web,
  MCP, API...) and the source address. Open an entry for the user agent, sign-in method, request
  id, target, the before/after of a change field by field, and its place in the chain.
- **Filters:** action (or a prefix like `plan.`), outcome, actor email, actor type, client, IP
  address or CIDR (`203.0.113.0/24`), target type and id, project, and a time range. REST and
  `amp_audit_log` take the same names: `action`, `outcome`, `actor`, `actor_type`, `client`,
  `ip`, `target_type`, `target_id`, `project_id`, `from`, `to` (RFC 3339 or a date; `to` is
  exclusive, and a date means the end of that day, UTC).

### Exporting

**CSV** and **NDJSON** next to the filters download every entry the filters select, not just
the loaded pages (`GET /api/audit/export?format=csv|ndjson&<filters>`, same access as reading).
The export is streamed, so there is no row limit, and each export is itself recorded as an
`audit.export` entry with its filter and row count.

- NDJSON has one entry per line, exactly as the API and webhooks send it.
- CSV has one flat row per entry. `details`, `before` and `after` are JSON in their cells.
- A CSV cell that a spreadsheet would run as a formula (starting with `=`, `+`, `-` or `@`) gets
  a leading `'`. User agents and summaries can be chosen by whoever made the request.

### Append-only and tamper-evident

- **Nobody can edit or delete an entry by SQL.** A database trigger refuses `UPDATE`, `DELETE`
  and `TRUNCATE` on `audit_log` for every role, AMP's own and superusers included. It makes two
  exceptions:
  - Deleting a user still works. The entry keeps the actor's email, and `actor_user_id` becomes
    empty. `actor_id_at_write` keeps the id the entry was written with; it has no link to the
    users table and is part of the hash, so pointing `actor_user_id` at someone else shows up in
    verify (`actor_mismatch`). Entries from before this column existed don't have it.
  - Retention can delete old entries (below), as the separate `amp_audit_purge` database role
    only. Setting a session flag, or inserting an anchor checkpoint by hand, no longer opens that
    path.
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
  if it finds a problem and 3 if the result is unproven.
- The result (`status`) is one of three:
  - `ok`: no problems, and a valid signature vouches for every chain.
  - `unproven`: no problems were found, but nothing signed proves it. There is no signing key,
    a chain has no signed checkpoint yet (they are hourly), or a chain starts at an unsigned
    anchor from before signing began. Someone with database access could have rewritten such a
    chain consistently, so this is **not** OK: `ok` is false, and the UI shows "Unproven".
  - `failed`: tampering or loss.
- What it reports:
  - `altered`: an entry's contents don't match its hash.
  - `missing`, `missing_start` and `missing_tail`: entries were deleted outside retention. Only
    an **anchor** checkpoint (written by retention) can start a chain. A periodic checkpoint
    can't.
  - `broken_link`: entries were reordered or replaced.
  - `checkpoint_mismatch`: history was rewritten after it was signed.
  - `signature`: a checkpoint has a bad signature or an unknown key. Or it has no signature
    although a key is configured. Since the review fix this applies to every checkpoint,
    whatever its id. If you had unsigned checkpoints before you added a key, set
    `AUDIT_SIGNING_SINCE` (an RFC 3339 time or a date) to when signing started: older unsigned
    checkpoints are then accepted as pre-key history.
  - `anchor`: an anchor retention can't have written. Either its entry was younger than
    `AUDIT_RETENTION_DAYS` when it was deleted, or the anchor is unsigned and signed history
    before it is gone. If you lengthened retention after a purge, older anchors can trip the
    age check.
  - `actor_mismatch`: `actor_user_id` was re-pointed to another user.
- Warnings, which don't fail the check: entries newer than the last signed checkpoint, and
  pre-key unsigned checkpoints accepted under `AUDIT_SIGNING_SINCE`.

### Retention

- `AUDIT_RETENTION_DAYS` (Helm `ampApi.audit.retentionDays`) defaults to **365**. The minimum
  is 30. `0` keeps entries forever.
- **One-time setup by a database superuser.** Retention deletes as a separate role,
  `amp_audit_purge`, which owns the database's `audit_purge()` function. Only a superuser can
  create that role. When AMP's migrations run as a superuser (docker compose, CI), they set it
  up themselves. On CloudNativePG they don't, because the app role isn't a superuser, so
  **retention deletes nothing until you run the setup**. amp-api logs a warning at startup until
  then. To set it up:
  1. Print the SQL: `kubectl -n amp exec deploy/amp-api -- ./amp-api audit purge-role-sql`.
  2. Run it with psql as the `postgres` superuser, connected to AMP's database. On
     CloudNativePG, open the session with `kubectl cnpg psql <cluster> -n amp -- -d amp` and
     paste the SQL.

  The SQL is idempotent. Re-run it if a later migration replaces `audit_purge()`.
- Once a day, one replica runs the purge, chain by chain, **in batches of 5,000 entries**. Each
  batch is its own short transaction, and writers are never held up, so even a first purge of a
  large chain is safe. For each batch it:
  - signs an **anchor** checkpoint at the last entry it will delete;
  - passes the anchor to `audit_purge()`. The function takes the entry's hash and time from the
    entry itself, refuses entries younger than 30 days, stores the anchor and deletes the
    entries up to it. That is the only delete the trigger allows, and only for entries an
    anchor covers.
- When it is done, it records `audit.purge` with counts per chain, and an `audit.checkpoint` for
  each chain's last anchor.
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
3. Set `ampApi.audit.signingKey.existingSecret=amp-audit-signing`, then delete the file. The
   production values already point there. The reference is optional, so a missing Secret means
   no key, not a pod that can't start.
4. Keep the public key. After a rotation, list the old public keys in
   `ampApi.audit.retiredPublicKeys`, so checkpoints signed earlier still verify.

Without a key, amp-api starts normally and logs a warning. Checkpoints are written unsigned, and
verify reports every chain as **unproven**. A key it can't read is treated the same way, and its
value is never logged.

Restoring the database:
- A full `pg_restore` works as is.
- A **data-only** restore into a migrated database must use `--disable-triggers`. Otherwise
  the chain trigger renumbers the entries it loads.

## Webhooks

Projects and orgs can send events to your own endpoints (**Settings → Webhooks**), e.g. to post
completions into Slack or update another tracker.

**Streaming the audit log to a SIEM.** An org webhook with "Audit log entries" receives that
org's entries. Sign-ins, accounts and API tokens belong to no org. To stream those too, a global
admin adds an **instance webhook** (**Admin → Audit log → Stream to a SIEM**, or
`POST /api/webhooks`). Instance webhooks:

- receive every audit entry of the whole AMP, and audit entries only;
- send JSON with every audit field.

Any webhook can also send one **auth header** for collectors that want a static token, such as
Splunk HEC (`Authorization: Splunk <token>`) or Datadog (`DD-API-KEY: <key>`). The header:

- is set as `"auth_header": "Name: value"`;
- can't be read back, logged or recorded: the audit entry keeps only its name;
- is sent only to https URLs;
- must be entered again when the webhook's URL changes;
- can't replace a header AMP sets (`X-AMP-*`, `Content-Type`, `Host`...).

Deliveries are still signed (`X-AMP-Signature`). Webhook URLs can't point at private, loopback,
link-local, CGNAT or NAT64 addresses. AMP checks this when the URL is saved and again on every
connection, and never follows redirects.

Audit entries reach webhooks through a database outbox (`audit_outbox`). The trigger that
chains an entry also queues it, so an entry is delivered at least once, even if amp-api restarts
or is busy.

**Duplicates.** At least once means a receiver can get the same entry twice. If amp-api stops
after matching an entry to webhooks but before removing it from the outbox, the entry is matched
again when its one-minute lease runs out, and a second delivery is queued for each webhook.
Nothing in AMP removes these duplicates. The envelope `id` of an audit delivery is stable
(`evt_audit_<entry id>`), and so are `chain_key` and `chain_seq`, so **receivers should
de-duplicate on `id`**. Most SIEMs can do this at ingest.

**Request context is opt-in.** An audit entry's request context is the client IP
(`source_ip`), `user_agent`, `session_id`, `token_id`, `before`/`after`, and the raw
`X-Forwarded-For` in `details.xff`. It is personal data, so an org or project webhook receives it
only with `include_context` turned on. This is off for every webhook made before audit log v2,
and off by default for new ones. Turn it on with **Include request context** in the webhook form,
or `"include_context": true` on create and update. Instance webhooks have it on by default.

**A stream that stops is recorded.** If an org or project webhook's deliveries keep failing, AMP
turns it off after 20 deliveries in a row have failed for good, and writes a `webhook.disable`
entry (actor: system). Instance webhooks, the SIEM stream, are never turned off: their deliveries
keep being retried with backoff. A delivery that runs out of retries is dropped and recorded as
`webhook.delivery.dropped`. That entry is written at most once an hour per webhook, and counts
the drops since the previous one. Both entries go to the audit log and to the other audit
webhooks, so a SIEM that stops receiving can be noticed. **Admin → Audit log** shows a "SIEM
stream stopped" warning while an instance webhook is off or failing, and each webhooks panel
shows the same warning for its own audit webhooks.

## Project lifecycle

- **Archive** a finished project (read-only; `amp_archive_project`, owners), and restore it later.
- **Export / import** a project's plan as JSON (`amp_export_project`, `amp_import_project`). This copies one
  project under new IDs. To move or back up a whole AMP, see
  `operations/migrating-to-a-new-cluster.md` and `operations/backup-and-disaster-recovery.md`.
- **Reset** clears a project's work (danger zone; owners only).
