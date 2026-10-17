---
title: Trash, audit log, and project lifecycle
tags: [trash, restore, audit, archive, export, import, webhooks]
---

# Trash, audit log, and project lifecycle

## Trash

Deleted tickets are kept for 30 days with their comments, history, runs and links.
- UI: **Settings → Trash**: restore one or several.
- Agents: `amp_list_deleted_tasks`, `amp_restore_tasks`.
Restore rather than re-create: the history comes back, and tickets restored together are
re-linked to each other. Restore covers tickets; a ticket whose story was deleted can't be
restored until the story exists again. The person who deleted a ticket, or a lead, can restore it.

## Audit log

Every change to access (org, teams, project links, roles, accounts, tokens) and to settings is
recorded: **Settings → Audit log** for a project, or `amp_audit_log`.

## Webhooks

Projects and orgs can send events to your own endpoints (**Settings → Webhooks**), e.g. to post
completions into Slack or update another tracker.

## Project lifecycle

- **Archive** a finished project (read-only; `amp_archive_project`, owners), and restore it later.
- **Export / import** a project's plan as JSON (`amp_export_project`, `amp_import_project`).
- **Reset** clears a project's work (danger zone; owners only).
