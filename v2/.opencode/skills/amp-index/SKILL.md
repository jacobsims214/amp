---
name: amp-index
description: Skill index for AMP agents — load this first to understand what skills are available and when to use them
---

# AMP Skills

Load skills on demand using `skill("name")` when you determine they are relevant.
Do not load skills you don't need. Skills cost context — use them purposefully.

---

## Skills

**amp-init**
Use when there is no `.amp.json` in the current directory.
Covers: creating a new project, scanning the codebase, seeding the knowledge base.

**amp-planning**
Use when planning and dispatching work for an existing project.
Covers: the full planning protocol, task hierarchy, DAG dependencies, dispatch, approval gate.

**amp-execution**
Use when executing an assigned task as a worker agent.
Covers: reading a ticket, logging progress, writing to the KB, completing a task.

**amp-kb**
Use when you need to search or write to the project knowledge base.
Covers: search strategy, create vs update rules, writing content that embeds well.

**amp-mcp**
Use when you need the exact tool names, arguments, or return shapes for AMP MCP calls.
Covers: full tool reference for projects, epics, stories, tasks, KB, and DAG rules.
