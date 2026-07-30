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

If you don't already have a name and code from the user, infer them from the directory name and ask for confirmation. Keep the code short and slug-friendly.

```json
{ "project_id": <id>, "project_name": "<name>", "project_code": "<code>", "amp_api": "http://localhost:8000" }
```

Write that to `.amp.json` in the current directory. Commit it — everyone working here shares the same project.

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

See `amp-kb` if you need guidance on writing content that embeds well for semantic search.

---

## Handoff

Once the project exists, `.amp.json` is written, and the overview is in the KB, tell the user what you found and what you created. Let them correct anything before planning begins.
