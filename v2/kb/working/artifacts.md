---
title: Artifacts — see it before it's built
tags: [artifacts, mockups, design, review, approval]
---

# Artifacts

An **artifact** is something an agent makes for people to look at: a UI mockup or clickable
prototype, a diagram, an image, a written spec. AMP stores it on the ticket it's for, keeps every
version, and shows it to everyone on the project, so it works the same from Claude Code, opencode
or anything else that speaks MCP.

## The flow

1. **Plan a design ticket first.** When you should see something before it's coded (a new
   screen, a redesign, a landing page, an architecture diagram), the manager plans a
   `Mockup: …` ticket for the designer and makes the build tickets depend on it.
2. **The agent publishes** with `amp_publish_artifact` and blocks its ticket asking you to review
   it. The ticket shows as a question; the artifact shows under the project's **Artifacts** tab
   and in the ticket.
3. **You review.** Open the artifact: HTML mockups run live (switch Desktop, Tablet, Phone), and
   older versions are a click away. **Approve**, or **Request changes** and say what. Approving
   is yours to click: agents can't approve over MCP. You can tell your manager agent in chat what
   to change, and it records that with `amp_review_artifact(approve=false, note=…)`.
4. **Your verdict goes on the ticket** and the ticket goes back to ready. The manager dispatches
   it again: the designer publishes a new version, or completes the ticket once you've approved.
   Completing it unblocks the build.
5. **Builders build to the approved version** (`amp_get_artifact` with `approved_version`) and
   compare their result with it.

Who can review: the ticket's owner or creator, a lead, or a project owner (anyone on the project
for an artifact without a ticket), in the web UI. No agent can approve, not even one under your
own login: a person approves, like plan approval.

## Keeping or letting go

Artifacts don't pile up: each one is deleted 45 days after its last version (the server's
`ARTIFACT_RETENTION_DAYS`) unless a person keeps it. Two weeks before, the person who made it
and the owner of its ticket see it on their home page: **Keep**, or **Let it expire**. The
artifact page has **Keep it** too, and agents can keep one with `amp_keep_artifact` when their
person asks. A kept artifact never expires; a new version pushes the date back. When one
expires, a comment on its ticket says so.

## For agents

- Doing design work? Read the design guide first: `design/start-here.md`.

- `amp_publish_artifact(task_id, title, kind, content | content_base64, note)`. Kinds: `html`,
  `svg`, `image`, `markdown`, `pdf`, `text`. Up to 10 MB. Pass `artifact_id` for a new version.
- HTML must be one self-contained file: inline CSS and JS; CDN scripts and fonts work; nothing
  else is reachable.
- After publishing, `amp_block_task` with the review request and the url, and stop. Don't
  complete a design ticket that isn't approved.
- `amp_list_artifacts(project_id | task_id)` and `amp_get_artifact(artifact_id, version)`;
  images come back as images.

## Safety

Artifacts are served under a sandbox: their scripts run, but in an isolated origin with no
access to your AMP session, cookies or API. Over REST, scripts and CI can publish with
`POST /api/projects/{id}/artifacts`.
