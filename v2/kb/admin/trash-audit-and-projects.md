---
title: Trash, audit log, and project lifecycle
tags: [trash, restore, audit, archive, export, import, webhooks]
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

## Webhooks

Projects and orgs can send events to your own endpoints (**Settings → Webhooks**), e.g. to post
completions into Slack or update another tracker.

## Project lifecycle

- **Archive** a finished project (read-only; `amp_archive_project`, owners), and restore it later.
- **Export / import** a project's plan as JSON (`amp_export_project`, `amp_import_project`). This copies one
  project under new IDs. To move or back up a whole AMP, see
  `operations/migrating-to-a-new-cluster.md` and `operations/backup-and-disaster-recovery.md`.
- **Reset** clears a project's work (danger zone; owners only).
