---
title: Orgs, teams and access
tags: [access, orgs, teams, roles, permissions, invites, agents]
---

# Orgs, teams and access

## Who can see and do what

- **Org roles**: *owner*, *admin*, *member*. Owners and admins manage the org: members, teams,
  projects, the org KB, webhooks. Every member sees the org's Portfolio for the projects they can
  see.
- **Team roles**: *lead* and *member*. Leads manage their team's members.
- **Projects** are reached through teams. Link a team to a project as an **owner** team (it runs
  the project) or a **member** team (it works in it). On a project you are:
  - **Member**: on a linked team. Read everything, comment, plan, own and run your tickets,
    approve their own tickets in the plan (in the web UI).
  - **Lead**: lead of a linked team. Also hand over others' work, and approve others' tickets
    on their behalf.
  - **Owner**: on an owner team. Also project settings (plan approval, project metadata,
    reassignment rules), archive and reset.
- **Your tickets**: only you (and agents acting with your token) dispatch, work and complete
  them. Anyone on the project can read and comment.

## Managing it

- UI: **Teams & orgs** (members, roles, teams, invites), and a project's **Settings → Team
  access**.
- **A project's org** is set under **Settings → General → Org**. Global admins can place any
  project, including one from before orgs existed (it has no org, so nobody else can reach it).
  Org owners and admins can move a project between orgs they administer. Moving unlinks the old
  org's teams; pick an owner team from the new org in the same step so people keep access.
  Agents: `amp_manage_project_teams(action="set_org", org_id, team_id?)`.
- **Team leads** can link their own team to a project they're on as a member team, and unlink
  it. Making a team an owner takes a project owner or an org admin.
- Agents: `amp_manage_org_members`, `amp_manage_teams`, `amp_manage_team_members`,
  `amp_manage_project_teams`, `amp_manage_invites`, `amp_list_project_members`.

## Tokens and agent accounts

**Agents & tokens** issues API tokens. Your token acts as you: use it for the harness you drive
yourself. For something unattended (CI, a scheduled bot), create an **agent account**, put it on
the teams it needs, and give it its own token. You manage its tokens and can deactivate it.
Agent accounts can do work but can't approve plans. Nothing that comes through a token or MCP
can, including your own token: plans, artifacts and human tickets are approved or marked done by
a person in the web UI.

Revoke a token when a laptop is lost or someone leaves; every token change is in the audit log.
