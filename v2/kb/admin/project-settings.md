---
title: Project settings — watchdog, metadata, and webhooks explained
tags: [settings, watchdog, dispatch, metadata, webhooks, slack, siem, audit-export, not-started]
---

# Project settings

Settings is where a project's rules live: **Project → Settings**. Project owners and org admins
change them; everyone else sees them read-only. Each section there has a "How it works" link to
this guide. Plan approval, merges, access, knowledge bases, trash and the audit log have their
own pages (see the end); this page covers the three that need an example.

## Dispatch watchdog

A ticket is *dispatched* when the manager hands it to a worker, and *started* when the worker's
first call is `amp_claim_task`. The watchdog covers the gap between the two.

- **Flag it for its owner after** N minutes (default 10): a dispatched ticket no worker started
  shows "Dispatched · not started" on the board and its owner is told.
- **Return it to ready after** N minutes (default 30): the ticket goes back to ready, and the
  manager dispatches it again or asks you.
- `0` turns either one off. A ticket a worker has started is never returned this way.
- Set the return higher than the flag, or the ticket is returned before it can be flagged.

Example: flag at 10, return at 30. A ticket dispatched at 09:00 that nobody claims shows
"Dispatched · not started" from 09:10 and is back in ready at 09:30.

See `working/running-work.md` for the dispatch loop.

## Metadata

Free-form key/value fields for integrations and reporting. Set them on the project and every
epic, story and ticket in it inherits them; any of those can add the same key to override it.
Values are text, numbers or yes/no.

Example keys:

| Key | Value | Use |
| --- | --- | --- |
| `repo` | `github.com/org/repo` | which repository the work lands in |
| `jira_board` | `PLAT` | where an integration should file the ticket |
| `initiative` | `q4-onboarding` | group work in reports |
| `capitalizable` | `true` | mark work for finance reporting |

Saving fields changes only the metadata, nothing else about the project.

## Webhooks

A webhook sends the project's ticket changes to another service. Add one under **Settings →
Webhooks**: a URL, the events you want (none picked means every ticket and project event), and a
format.

- **Slack** posts a message to an incoming-webhook URL. The body is `{"text": "…"}`; Slack does
  not check signatures.
- **JSON** posts the event. Each POST has `X-AMP-Signature: t=<unix>,v1=<hex>`, where `v1` is the
  HMAC-SHA256 of `<t>.<body>` with the webhook's signing secret. The secret is shown once, when
  the webhook is created or rotated. Reject old `t` values to stop replays.

Org admins can add webhooks for every project in the org, and a global admin can add an instance
webhook that sends audit entries (`audit.*`) to a SIEM. Audit entries carry a hash chain so the
copy you keep can prove later what an entry said; see `admin/trash-audit-and-projects.md` for the
chain, the export and retention.

## Other settings

- Plan approval: `working/planning-and-approval.md`
- Merges, the GitHub token and the merge webhook: `working/merging-and-ci.md`
- Team access: `admin/orgs-teams-and-access.md`
- Knowledge bases: `knowledge/kb-scopes.md`
- Trash, audit log, archiving: `admin/trash-audit-and-projects.md`
