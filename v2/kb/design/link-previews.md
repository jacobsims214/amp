---
title: "Spec (proposed, not built): in-app previews for AMP links"
tags: [spec, ui, links, preview, proposed]
---

# Spec (proposed, not built): in-app previews for AMP links

Status: **proposed**. Nothing here ships yet; today an AMP link in markdown navigates in-app
(`components/ui/Markdown.tsx` `MarkdownLink`). Issue #133, tracking #141. The two open questions
at the end need the owner's decision before this is built.

## Why

Following a ticket link from My work, Mission or the Approve view (`/project/3/board?focus=41`)
takes the reader off the page they were working through. They lose their place, then come back
to a re-rendered list. What they usually want is to see what #41 is (its state, what it asks,
what it unblocks) and carry on. The ticket drawer can't do this now: each page mounts its own
`TaskDrawer` with its own data (`?focus=` opens it only on Board, Flight plan and Mission;
`lib/projectLink.ts`).

## What previews and what navigates

| Link | Plain click | Modifier-click (⌘/Ctrl/Shift), middle-click, or "Open" in the preview |
|---|---|---|
| A ticket: `#41` refs (`remarkTicketRefs`), `/project/:id/<view>?focus=N` | **Preview** | Navigate, as today |
| A KB doc: `/project/:id/kb…`, `/teams/:id/kb…`, `/orgs/:id/kb…`, `/guide…` with a doc path | **Preview** | Navigate |
| An artifact: `/artifacts/:id` | **Preview** | Navigate |
| Any other AMP route (a view, settings, `/reports`, a project) | Navigate | Navigate |
| Full-load paths (`/sessions/`, `/api/`, `/home`) and external links | As today (full load / new tab) | As today |

A link the reader can't see (403/404) previews as "You can't see #41" with no data, the same
words `FocusNotFound` uses. A preview never opens from hover alone (see Q1).

## Shape

- `PreviewProvider`, a context mounted once in `AppShell`, exposes
  `openPreview({ kind: 'ticket' | 'kb' | 'artifact', id, projectId?, path? })` and renders one
  preview at a time. Opening another replaces it; a "Back" link returns to the previous one
  (a stack of at most 5).
- `MarkdownLink` calls `openPreview` for the kinds above when a provider is mounted, and
  navigates as today when there is none (tests, the Session page outside `AppShell`).
- The preview is read-only (see Q2) and keeps no URL state: closing it leaves the page exactly as
  it was, including scroll and any unsaved text.

## Contents

**Ticket**: `#41 · <state tag> · <Needs approval tag when it applies>` and its name; owner and
performer; the question with its options when it has one (#130); the description and acceptance
criteria as `<Markdown size="compact">`; "Waits on" and "Unblocks" as ticket refs (which preview
in turn); the last 3 history entries; **Open full ticket** (`projectLink(projectId, 'board', id)`).

**KB doc**: title, path, tags, updated at, and the doc as `<Markdown size="doc">`, capped at
about 60 lines with "Open in the KB" for the rest.

**Artifact**: title, kind, status, current version and its note (markdown), who made it and
when, and **Open artifact**. HTML artifacts aren't rendered in the preview (they need the
sandboxed viewer); images show the current version.

## Keyboard and focus

- The preview is a modal dialog (`role="dialog"`, `aria-modal`, labelled by its title). Opening
  it moves focus to its heading; focus is trapped inside; **Esc** closes it and returns focus to
  the link that opened it.
- **Enter** on a focused link previews, like a click; **⌘/Ctrl+Enter** navigates.
- Inside: **Tab** order is heading, body links, Back, Open, Close. **Alt+←** is Back.
- An open TaskDrawer (Board) or CrudModal stays underneath; Esc closes only the preview first.

## API calls

| Kind | Calls | Notes |
|---|---|---|
| Ticket | `api.getTask(id)` (`GET /api/tasks/:id`), `api.getHistory(id)` (`GET /api/tasks/:id/history`) | "Unblocks" needs the ticket's dependents, which `GET /api/tasks/:id` doesn't return. Today that means `api.listTasks(projectId)`, a whole project for one preview. **A lighter endpoint is needed**: `GET /api/tasks/:id/preview` returning the task, its dependents' IDs and names, and its last 3 history rows in one read (authorized as `getTask` is). |
| KB doc | `api.kbGet(ref, path)` (`GET /api/kb/doc`) | Enough as is. KB links need a doc path in the URL (e.g. `?doc=<path>`); KB pages don't take one today, so that comes with this work. |
| Artifact | `api.getArtifact(id)` (`GET /api/artifacts/:id`) | Enough; the preview uses the current version from its versions list. |

All three are cached for the life of the page (keyed by kind and id) and refreshed when the SSE
stream reports a change to that ticket or artifact.

## Open questions (owner decision)

- **Q1. Modal, hover popover, or the TaskDrawer over any page?** Recommendation: a **modal**
  (above). A hover popover is too easy to open by accident while scanning a list, can't hold the
  description and criteria, and is unreachable by keyboard and touch. Reusing `TaskDrawer` over
  every page would give tickets full editing everywhere, but it only covers tickets (not KB docs
  or artifacts) and brings its per-page data loading with it.
- **Q2. Read-only, or Answer / Mark done in place?** Recommendation: **read-only first**, with
  **Open full ticket** for acting. The actions already live in one component (`HumanActions`), so
  adding Answer and Mark done to the ticket preview later is small, but it makes the preview a
  second place to act on a ticket, with its own refresh and permission states to get right.
