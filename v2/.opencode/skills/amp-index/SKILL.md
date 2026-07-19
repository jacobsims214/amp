---
name: amp-index
description: Skill index for AMP agents — load this first to understand what skills are available and when to use them
---

# AMP Skills

Load skills on demand using `skill("name")` when you determine they are relevant.
Do not load skills you don't need. Skills cost context — use them purposefully.

---

## Agents (snapshot — always confirm against your live `task` tool context)

These are the agents currently defined under `.opencode/agent/*.md`. This list is for onboarding
context only — when actually picking who to dispatch, read the live list from your own `task`
tool description, since the roster can change after this doc was last updated.

- **amp-manager** (primary) — plans work, builds the board via AMP MCP, dispatches specialist subagents; never edits project code, runs builds, or fetches the web itself
- **amp-worker-backend** (subagent) — Go, chi/pgx, Docker, Terraform/TFE, backend tests
- **amp-worker-frontend** (subagent) — React, TypeScript, Tailwind, frontend tests
- **amp-worker-docs** (subagent) — git commits/PRs, KB writes, markdown docs, config-only edits
- **amp-reviewer** (subagent) — wave checks and full code reviews, never original implementation
- **amp-researcher** (subagent) — read-only investigation, dispatched by the manager to answer a question before planning

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
Covers: reading a ticket, logging progress, writing to the KB, completing a task. Does not cover reviewer protocol — see amp-review for that.

**amp-review**
Use when your task name contains "check", "review", or "Code Review:".
Covers: wave check vs full code review distinction, the fix-task template, rules for creating fix tasks instead of failing.

**amp-agent-builder**
Use when the existing agent/skill roster doesn't fit a new domain, or an existing specialist is underperforming. Manager-only.
Covers: agent-file frontmatter shape, steps sizing, model-selection heuristic, registering new skills, the restart-required caveat.

**amp-kb**
Use when you need to search or write to the project knowledge base.
Covers: search strategy, create vs update rules, writing content that embeds well.

**amp-mcp**
Use when you need the exact tool names, arguments, or return shapes for AMP MCP calls.
Covers: full tool list by area, task scheduling (start_at), and key argument types.

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
