---
name: amp-init
description: Initialize a new AMP project — create the project, scan the codebase, seed the knowledge base with a project overview
---

# AMP Init

Use this skill when there is no `.amp.json` in the current directory.

---

## What needs to happen

You need to establish three things before any planning can begin:

1. **A project exists in AMP** — created via `amp_create_project`
2. **`.amp.json` is written** — so every agent in this directory knows the project ID
3. **The KB has a project overview** — so future agents have context without scanning the codebase themselves

How you get there is up to you. The guidance below covers what each step involves.

---

## Creating the project

If your user didn't give a name and code, pick them from what they told you about the project (the folder name only when that's all you have), create the project, and say they can rename it. Don't stop to ask first. The code is the ticket prefix — refs read `CODE-149` everywhere (reports, commits, branches) — so keep it to 2–6 letters, like `NWC` for Northwind Commerce, not the full name.

You also need the URL of the user's AMP server for `amp_api`: in the harness it is the
origin of `amp_server` in your `<amp_context>` (e.g. `http://localhost:18000` for
`http://localhost:18000/mcp`). Use it; don't ask.

```json
{ "project_id": <id>, "project_name": "<name>", "project_code": "<code>", "amp_api": "<the AMP server URL the user confirmed>" }
```

Write that to `.amp.json` in the working directory with the `write` tool (the one file you may write). Don't commit it yourself: tell your user to commit it, or let the first worker's ticket include it, so everyone working here shares the same project.

---

## Understanding the codebase

Before writing the overview, explore enough to describe the project accurately. Look at whatever gives you a clear picture — directory structure, README, package manifests, key source files. You don't need to read everything, just enough to answer:

- What does this project do?
- What is the tech stack?
- What are the major components or services?
- What is the current state — early, mid-development, production?

---

## Writing the project overview

Write a KB doc at `architecture/project-overview.md`. This doc is the first thing any agent will find when they search the KB. Write it in prose so it embeds well — not bullet lists.

Cover: what the project is, what it does, the tech stack, the structure, the current state, and anything non-obvious about how it's organized.

A new project with an empty repo still gets an overview — write it from what the user told you (goal, users, stack, constraints) and mark what's still undecided. Don't skip it because there's no code to scan; this is the doc every later session starts from.

See `amp-kb` if you need guidance on writing content that embeds well for semantic search.

---

## Handoff

Once the project exists, `.amp.json` is written, and the overview is in the KB, tell the user what you found and what you created. Let them correct anything before planning begins.
