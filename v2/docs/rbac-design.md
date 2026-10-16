# Multi-user access: orgs, teams, ownership

AMP started single-tenant: every signed-in user saw and changed everything.
This adds orgs, teams, project access and ticket ownership on top of the
existing model, additively. Existing data keeps working and nobody loses
access on upgrade.

## Model

```
Org ──< Team ──< team_members >── User
 │        │                        │
 │        └──< project_teams >── Project      (team's role on the project: owner | member)
 │                                  │
 └─────────< org_members >── User   ├─ Epic   created_by, owners: many users  (epic_owners)
                                    ├─ Story  created_by, owners: many users  (story_owners)
                                    └─ Task   created_by, owner_user_id: 0 or 1 user
```

- **Orgs** are created only by global admins (`user_roles` `admin`). A user
  can be in many orgs. Org roles: `owner`, `admin`, `member`.
- **Teams** belong to one org. Team roles: `lead`, `member`. Adding someone
  to a team adds them to its org as a member if they weren't already.
- **Projects** belong to one org (`projects.org_id`). Only that org's teams
  can be linked to it, as `owner` or `member`.
- **Tickets** (`tasks`) have at most one owner (`owner_user_id`, `NULL` =
  unassigned). The schema itself rules out two owners. `assigned_to` and
  `agent_id` keep their meaning: which agent profile works the ticket, not
  which person owns it.
- **Epics and stories** are shared groupings: anyone on the project can put
  stories and tickets under them. They record their creator and can have many
  owners; the creator is the first owner.
- Comments and activity log entries record the acting user
  (`author_user_id`, `actor_user_id`) next to the existing free-text fields.

## Access levels

A user's level on a project is the highest that applies:

| Level | Who |
|---|---|
| `global_admin` | `user_roles` admin (and local dev with auth disabled) |
| `org_admin` | owner or admin of the project's org |
| `owner` | member of a team linked to the project as owner |
| `lead` | lead of any team linked to the project |
| `member` | member of any team linked to the project |
| none | everyone else. The project is reported as **not found**. |

Org membership alone is not project access.

| Action | Needs |
|---|---|
| See a project and everything in it, comment, read history, use the KB | member |
| Create epics, stories, tickets | member |
| Edit or delete an epic or story, or change its owners | one of its owners, or owner level |
| Archive, restore, reset a project; link and unlink teams; change project settings | owner |
| Work a ticket (edit, complete, block, change state, schedule) | its owner, or anyone if it's unassigned. Leads and project owners reassign rather than work others' tickets; org admins can override |
| Dispatch | its owner, or anyone if unassigned (dispatch claims it); org admins may dispatch anyone's work |
| Claim | only an unassigned ticket, at any level. Claiming never takes a ticket from someone |
| Assign to someone, or release | its owner, or anyone if unassigned; lead and up for any ticket. With the project's open reassign setting on (the default), any member for a ticket that isn't in progress. The new owner must have project access |
| Delete a ticket | its owner or creator, or lead and up |
| Manage org members, teams, invites | org owner/admin. Only org owners grant or remove the owner role |
| Manage a team's members | org owner/admin, or that team's lead |
| Create orgs | global admin |

The rules live in `internal/authz` (pure decision functions plus a
`Checker` that queries access) and are applied by every REST route and every
MCP tool. MCP tools go through a `guard` wrapper that resolves the caller and
the project (from `project_id`, or from the task, story or epic the tool
names) before any handler runs. Operations shared by REST and MCP (project
placement, claim, assign, dispatch, grouping owners) live in
`internal/service`.

Ownership changes are a compare-and-set in the `TaskActor`: the handler
authorises against the owner it read, and the actor applies the change only
if the owner is still the same. Two people claiming the same ticket at once
can't both win. The loser gets "owner changed … reload and retry".

Open reassign (`projects.open_reassign`, migration 005, on by default) lets
any project member take over, hand on, or release someone else's ticket
while it isn't in progress. Project owners turn it off in the project's Team
access dialog, with `PATCH /api/projects/{id}`, or with
`amp_update_project_settings`. It never applies to `amp_claim_task`: a claim
only takes unassigned work, so agents racing for the pool never take each
other's tickets. Because a ticket can be started between the check and the
change, the `TaskActor` re-checks that it isn't in progress before applying
an open reassignment.

## Default views

Task lists default to **the caller's own work**, in the API
(`GET /api/projects/:id/tasks`, `amp_list_tasks`) and on the board:

- tickets I own,
- unassigned tickets I created,
- unassigned tickets with no creator. These are rows from before attribution
  existed, which would otherwise vanish from every default board after the
  upgrade.

`owner=unassigned` shows the pool anyone can pick up, `owner=any` shows
everything, and `owner=<email or id>` shows one person's work. Visibility
is not restricted below the project level. Anyone on a project can see all of
its tickets with a filter; the default only decides what you see first.

New tickets are owned by their creator unless created with `owner: "none"`.

## Invites

Users only exist in AMP after their first sign-in (JIT provisioning from the
Dex token). To add someone before that, org admins (and team leads, for
their own team) add them by email. If no AMP user has that email, a pending
row is written to `invites`. On every sign-in, `UpsertUserFromClaims` checks
for pending invites for the user's email (case-insensitively) and applies
them. Adding an email that already belongs to a user applies immediately.

## Live updates

The SSE stream (`/api/events`) filters per connection: each connection only
receives events for projects its user can see. The visible set is cached per
connection and refreshed every 15s, or sooner when an event arrives for an
unknown project. The check runs on the connection's goroutine, never on the
actors' publish path.

## Upgrade path

Migrations are now versioned (`schema_migrations`, applied under a
Postgres advisory lock so overlapping pods can't both apply one). Migrations
001–003 were already idempotent; on an existing database they run once more
and get recorded.

`004_orgs_teams.sql` adds the new tables and nullable columns, then runs a
one-time backfill:

- an org **Default** with a team **Everyone**;
- every existing user joins both. Global admins become org owners and team
  leads; everyone else becomes a member;
- every existing project moves into Default, with Everyone linked as owner.

Every existing user keeps owner-level access to every existing project, so
the day-one experience is the same as before. Split teams up from there.
Existing tickets are unassigned and have no creator, so they appear in
everyone's default view until someone claims them. Because the backfill runs
exactly once, unlinking Everyone from a project later sticks.

What changes for people who sign in for the first time after the upgrade:
they have no project access until they're added to a team (or invited
before their first sign-in).

`cmd/server/migration_test.go` replays an existing deployment (old schema,
applied the old way, with data) and checks all of the above, including that
a second boot doesn't re-link Everyone.

## Also fixed

- MCP tool handlers previously ignored the request context, so they couldn't
  tell who was calling. They now receive it in both protocol eras.
- `/api/kb/config` returned the Typesense **admin** key, valid for every
  project's collection, to any signed-in user. It is now limited to global
  admins (the UI never used it; it searches through `/api/kb/search`).

## API summary

REST (see `internal/api/orgs.go` and the `RestHandler` doc comment):
`/api/orgs`, `/api/orgs/:id/{members,teams,invites}`,
`/api/teams/:id[/members]`, `/api/projects/:id/{members,teams}`,
`/api/tasks/:id/assign`, `/api/{epics,stories}/:id/owners`.
`POST /api/projects` and import take `org_id` and `team_id`, inferred when
unambiguous.

MCP — platform administration (same rules as the UI, so an admin's agent
can run the platform): `amp_list_orgs`, `amp_create_org`,
`amp_manage_org_members`, `amp_manage_teams`, `amp_manage_team_members`,
`amp_manage_invites`, `amp_manage_project_teams`. The `manage_*` tools take
an `action` (list/add/set_role/remove, list/create/rename/delete, …) rather
than being split into many small tools. The rules live in
`internal/service/orgs.go`, shared with the REST handlers.

MCP — work: new tools `amp_whoami`, `amp_claim_task`, `amp_assign_task`,
`amp_list_project_members`. `amp_create_task` takes `owner`. `amp_list_tasks`
takes `owner` and defaults to your work. `amp_update_epic`/`amp_update_story`
take `add_owners`/`remove_owners`. `amp_create_project`/`amp_import_project`
take `org_id`/`team_id`.
