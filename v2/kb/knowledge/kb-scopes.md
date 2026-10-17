---
title: Knowledge bases — project, team, org, and this guide
tags: [kb, knowledge, library, scopes, decisions]
---

# Knowledge bases

Agents forget between sessions; the knowledge base is how a project remembers. Every agent
searches it before starting and writes what it learned after.

## Four kinds

| KB | Holds | Who writes |
|---|---|---|
| **Project** (default) | This project's architecture, decisions, APIs, gotchas | Anyone on the project |
| **Team** | What's true for every project the team works on: runbooks, services it owns, conventions | Team members, with a reason |
| **Org** | What's true across the org: brand and design system, process, standards, policy | Org owners and admins, with a reason; everyone else annotates |
| **AMP guide** (`global`) | How to use AMP itself: these pages | Nobody in AMP; it ships from the AMP repo |

`amp_kb_sources(project_id)` lists the KBs around a project. Every `amp_kb_*` tool takes the
same scope arguments: `project_id`, and `scope` = `team` / `org` / `global`.

Knowledge goes to a team or org KB only when a person decides it belongs there. An agent that
thinks so proposes it, with why, and keeps it in the project KB meanwhile.

## Folders

Keep one tree so the Library stays navigable:
- `architecture/`: how things work (start with `architecture/project-overview.md`);
- `decisions/`: one doc per decision, with what, why, who and when (including decisions made in
  Slack);
- `how-to/`: step by step;
- `apis/`: contracts;
- `discoveries/`: findings along the way.

Search before writing, and update the doc on the same subject rather than starting a second one.
`amp_kb_get` before `amp_kb_write`, because a write replaces the whole doc.

## Annotations

For small corrections, or to flag a team or org doc that looks wrong, annotate it
(`amp_kb_annotate`) instead of rewriting. Whoever can write the KB folds the note into the doc
and resolves it. The AMP guide takes no annotations: propose changes to the AMP repo's `kb/`
folder.

## Curating

`amp_kb_status` shows stale docs, open annotations and tag spread. The KB curator agent keeps
things tidy: merges duplicates, fixes paths, resolves folded-in annotations.
