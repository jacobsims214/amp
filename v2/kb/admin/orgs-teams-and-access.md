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
    approve plans.
  - **Lead**: lead of a linked team. Also hand over others' work.
  - **Owner**: on an owner team. Also project settings (plan approval, project metadata,
    reassignment rules), archive and reset.
- **Your tickets**: only you (and agents acting with your token) dispatch, work and complete
  them. Anyone on the project can read and comment.

## Managing it

- UI: **Teams & orgs** (members, roles, teams, invites), and a project's **Settings → Team
  access**.
- Agents: `amp_manage_org_members`, `amp_manage_teams`, `amp_manage_team_members`,
  `amp_manage_project_teams`, `amp_manage_invites`, `amp_list_project_members`.

## Tokens and agent accounts

**Agents & tokens** issues API tokens. Your token acts as you: use it for the harness you drive
yourself. For something unattended (CI, a scheduled bot), create an **agent account**, put it on
the teams it needs, and give it its own token. You manage its tokens and can deactivate it.
Agent accounts can do work but can't approve plans.

Revoke a token when a laptop is lost or someone leaves; every token change is in the audit log.
