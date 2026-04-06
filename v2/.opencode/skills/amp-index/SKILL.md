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

**code-reviewer**
Use when reviewing Go, TypeScript/React, Dockerfile, or Terraform code changes.
Covers: tiered code review framework (Blocker/Significant/Suggestion/Skip), focused feedback on critical issues.

**docker-engineering**
Use when writing Dockerfiles, docker-compose configs, or troubleshooting container builds.
Covers: multi-stage builds, layer caching, security hardening, health checks, corporate CA certificate injection.

**git-workflow**
Use when making commits, naming branches, or opening pull requests.
Covers: conventional commit format, branch naming conventions, PR hygiene, rebase vs merge rules, force-push safety.

**go-engineer**
Use when writing or reviewing Go backend code.
Covers: Go 1.22+ patterns, error handling, pgx/v5, HTTP handlers, chi routing, concurrency, and testing.

**react-engineer**
Use when writing or reviewing React frontend code.
Covers: React 18+ with TypeScript and Vite patterns, Zustand state management, TanStack Query, Tailwind CSS, async UX patterns.

**testing-strategy**
Use when writing or reviewing tests in Go or React/TypeScript.
Covers: table-driven tests, testify assertions, interface mocking, RTL query priority, MSW v2 handlers, what not to test.

**tfe-manager**
Use when creating, configuring, or investigating Terraform Cloud/Enterprise workspaces.
Covers: workspace lifecycle, variable sets, run types, run investigation, VCS integration.
