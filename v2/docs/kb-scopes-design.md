# Shared knowledge bases: org, team, and project KBs

Every project already has a knowledge base. This adds one per **team** and
one per **org**, for knowledge that outlives a single project: an org's
brand and design system or engineering process, a team's runbooks and
conventions.

## Scopes

| KB | Typesense collection | What belongs there |
|---|---|---|
| Project | `kb_{project_id}` (unchanged) | This project: its architecture, decisions, gotchas. **The default for everything.** |
| Team | `kb_team_{team_id}` | True for every project the team works on: runbooks, services the team owns, team conventions. |
| Org | `kb_org_{org_id}` | True across the whole org: brand and design system, process, standards, policy, glossary. |

Project collections and their document ids are unchanged, so existing
knowledge needs no migration. Team and org collections are created on
their first write and dropped when the team or org is deleted. There is no
Postgres change.

## Who can do what

| KB | Read and annotate | Write and delete |
|---|---|---|
| Project | project members | project members |
| Team | members of that team | members of that team |
| Org | every org member | org owners and admins |

Global admins can do everything, as elsewhere. Org admins do **not** see a
team's KB unless they are on the team: add someone to a team if they need
its knowledge. Access is checked on every request, so removing someone from
a team or org cuts off its KB immediately.

## Choosing a KB (scope resolution)

Every KB call names one KB. MCP tools take `scope` (`project`, the default,
`team`, or `org`) plus ids; REST takes exactly one of `project_id`,
`team_id`, `org_id`.

- `scope: "project"` needs `project_id`, as before. Calls that only pass
  `project_id` behave exactly as they always have.
- `scope: "org"` with `project_id` means that project's org. A project is in
  exactly one org, so this is never ambiguous. Without a project, pass
  `org_id`, unless the caller is in exactly one org.
- `scope: "team"` with `project_id` means the one team that is both linked
  to the project and has the caller on it. If there are several, the call is
  refused with the list and the caller passes `team_id`; the server never
  guesses. Without a project, pass `team_id`, unless the caller is on
  exactly one team.
- An explicit `team_id`/`org_id` from a different org than `project_id` is
  refused, so an agent working on an org-A project can't read or write org
  B's knowledge by accident, even when its user is in both.

`amp_kb_sources(project_id)` tells an agent where it is: the project's org
and its KB, the project's teams (owner first) with their KBs, and whether
the caller can read and write each. Teams the caller is not on are listed
without their contents, so the agent can say "team Y has a KB, but you're
not on that team".

## Search stays narrow

`amp_kb_search` searches one KB, the project's unless `scope` says
otherwise. Team and org KBs are searched when the user asks, or when the
agent needs something only they would hold (the brand rules, the team's
deploy runbook). This keeps agents' context small.

## Writing outside the project is a human decision

There is no promote feature. Writes default to the project KB. Writing to a
team or org KB:

- needs an explicit `scope`, and over MCP a `reason`: why this belongs to
  the whole team or org rather than one project. The reason is stored on
  the document, returned by `amp_kb_get`, and shown in the UI so placements
  can be reviewed.
- happens only when the user has decided it belongs there. The tool
  descriptions and the `amp-kb` skill tell agents to propose a placement,
  not to make it.
- for an org KB, needs an org owner or admin; anyone else in the org
  annotates instead.

## UI

- The KB page is one component for all three scopes: `/project/:id/kb`,
  `/teams/:id/kb`, `/orgs/:id/kb`. It is read-only (annotations still
  allowed) where the viewer can't write, and shows each document's
  placement reason.
- The project KB page links to the project's team and org KBs.
- The Orgs admin page links to the org KB and each team's KB.
- New project settings page, `/project/:id/settings`: name and
  description, team access, ticket reassignment, knowledge bases, archive.
