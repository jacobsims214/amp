// Package mcp exposes AMP task operations as MCP tools over HTTP/SSE.
//
// Tool surface (task API only — scoped to what agents actually need):
//
//	amp_create_project   amp_list_projects   amp_get_project
//	amp_archive_project  amp_restore_project
//	amp_create_epic      amp_list_epics      amp_get_epic      amp_update_epic
//	amp_create_story     amp_list_stories    amp_get_story     amp_update_story
//	amp_create_task      amp_list_tasks      amp_get_task
//	amp_update_task      amp_dispatch_task   amp_complete_task
//	amp_block_task       amp_add_task_comment
//	amp_export_project   amp_import_project
//	amp_whoami           amp_list_project_members   amp_claim_task   amp_assign_task
//	amp_list_orgs        amp_create_org             amp_manage_org_members
//	amp_manage_teams     amp_manage_team_members    amp_manage_invites
//	amp_manage_project_teams
//
// Every tool runs as the signed-in AMP user and is checked by internal/authz.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// Server holds references to the actor registry (for task mutations),
// the repo (for project/epic/story operations), the hub (to push
// live events to the UI whenever the agent creates something),
// and the KB service for per-project knowledge base operations.
type Server struct {
	registry *actor.Registry
	repo     *repository.Repo
	hub      *hub.Hub
	kb       *kb.Service
	svc      *service.Service
}

func NewServer(registry *actor.Registry, repo *repository.Repo, h *hub.Hub, kbSvc *kb.Service) *Server {
	return &Server{registry: registry, repo: repo, hub: h, kb: kbSvc, svc: service.New(repo, registry, kbSvc)}
}

// Register wires all MCP tools onto the provided MCPServer.
func (s *Server) Register(mcp *server.MCPServer) {
	ctx := context.Background()

	// ---- Projects (CRUD via repo, no actor needed) ----
	mcp.AddTool(tool("amp_create_project", "Create a new AMP project in one of your orgs, owned by one of your teams. Returns the created project with its id.",
		props{
			"name":        str("REQUIRED. Project name."),
			"code":        str("REQUIRED. Short unique code, e.g. 'my-project'."),
			"description": str("Optional description."),
			"org_id":      num("Optional. Org to create it in; required only if you belong to several (see amp_whoami)."),
			"team_id":     num("Optional. Team that will own it; required only if you are on several teams in the org."),
		}), wrap(ctx, s.open(s.createProject)))

	mcp.AddTool(tool("amp_whoami",
		"Who you are acting as in AMP: your user, your orgs and your teams. Tickets you create are yours by default; "+
			"amp_list_tasks shows your own work unless you pass owner.",
		props{}), wrap(ctx, s.open(s.whoami)))

	mcp.AddTool(tool("amp_list_project_members",
		"List the people with access to a project (id, email, access level). Use to find who a ticket can be assigned to.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Member, s.listProjectMembers)))

	// ---- Platform administration (orgs, teams, members, invites) ----
	// Same rules as the web UI: only global admins create orgs; org
	// owners/admins manage members, teams and invites; team leads manage
	// their own team. Anyone else gets a clear "requires …" error.
	mcp.AddTool(tool("amp_list_orgs",
		"List the orgs you belong to (global admins: every org), with your role in each.",
		props{}), wrap(ctx, s.open(s.listOrgs)))

	mcp.AddTool(tool("amp_create_org",
		"Create an org. Global admins only. The owner defaults to you; pass an email to make someone else owner "+
			"(if they have never signed in, they're invited as owner).",
		props{
			"name":  str("REQUIRED. Org name."),
			"slug":  str("Optional. URL-safe id (lowercase, digits, dashes); derived from the name if omitted."),
			"owner": str("Optional. Email or user id of the first owner."),
		}), wrap(ctx, s.open(s.createOrg)))

	mcp.AddTool(tool("amp_manage_org_members",
		"List, add, change the role of, or remove members of an org. Adding an email that has never signed in "+
			"creates an invite that applies on their first sign-in. Requires org owner/admin (owner role changes: org owners only).",
		props{
			"org_id": num("REQUIRED. Org ID."),
			"action": str("REQUIRED. One of: list, add, set_role, remove."),
			"user":   str("For add/set_role/remove: email or user id."),
			"role":   str("For add/set_role: owner, admin, or member (default member)."),
		}), wrap(ctx, s.open(s.manageOrgMembers)))

	mcp.AddTool(tool("amp_manage_teams",
		"List, create, rename, or delete teams in an org. Creating/renaming/deleting requires org owner/admin. "+
			"A team that is a project's only owner team can't be deleted.",
		props{
			"action":  str("REQUIRED. One of: list, create, rename, delete."),
			"org_id":  num("For list/create: org ID."),
			"team_id": num("For rename/delete: team ID."),
			"name":    str("For create/rename: team name."),
		}), wrap(ctx, s.open(s.manageTeams)))

	mcp.AddTool(tool("amp_manage_team_members",
		"List, add, change the role of, or remove members of a team. Adding someone also adds them to the team's org; "+
			"an email that has never signed in is invited. Requires the team's lead or an org owner/admin.",
		props{
			"team_id": num("REQUIRED. Team ID."),
			"action":  str("REQUIRED. One of: list, add, set_role, remove."),
			"user":    str("For add/set_role/remove: email or user id."),
			"role":    str("For add/set_role: lead or member (default member)."),
		}), wrap(ctx, s.open(s.manageTeamMembers)))

	mcp.AddTool(tool("amp_manage_invites",
		"List, create, or revoke pending invites for people who haven't signed in yet. Requires org owner/admin "+
			"(team leads may invite into their own team).",
		props{
			"org_id":    num("REQUIRED. Org ID."),
			"action":    str("REQUIRED. One of: list, create, revoke."),
			"email":     str("For create: the person's email."),
			"org_role":  str("For create: owner, admin, or member (default member)."),
			"team_id":   num("For create: optional team to join."),
			"team_role": str("For create: lead or member (default member)."),
			"invite_id": num("For revoke: invite ID."),
		}), wrap(ctx, s.open(s.manageInvites)))

	mcp.AddTool(tool("amp_manage_project_teams",
		"List the teams with access to a project, link a team (of the project's org) as owner or member, or unlink one. "+
			"Changing links requires project owner access. A project always keeps at least one owner team.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"action":     str("REQUIRED. One of: list, link, unlink."),
			"team_id":    num("For link/unlink: team ID."),
			"role":       str("For link: owner or member (default member)."),
		}), wrap(ctx, s.guard(authz.Member, s.manageProjectTeams)))

	mcp.AddTool(tool("amp_list_projects", "List the active projects you have access to.",
		props{}), wrap(ctx, s.open(s.listProjects)))

	mcp.AddTool(tool("amp_get_project", "Get a project by its numeric ID.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Member, s.getProject)))

	mcp.AddTool(tool("amp_archive_project", "Archive a completed project. Sets state=archived.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Owner, s.archiveProject)))

	mcp.AddTool(tool("amp_restore_project", "Restore an archived project. Sets state=active.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Owner, s.restoreProject)))

	mcp.AddTool(tool("amp_update_project_settings",
		"Change a project's name, description, or settings (project owners only); pass only what changes. "+
			"open_reassign: when true (the default), any project member can reassign or release a ticket that isn't "+
			"in progress; when false, only its owner, a team lead, or a project owner can.",
		props{
			"project_id":    num("REQUIRED. Project ID."),
			"name":          str("Optional. New project name."),
			"description":   str("Optional. New project description."),
			"open_reassign": boolean("Optional. Let any project member reassign tickets that aren't in progress."),
		}), wrap(ctx, s.guard(authz.Owner, s.updateProjectSettings)))

	// ---- Epics ----
	mcp.AddTool(tool("amp_create_epic", "Create an epic inside a project.",
		props{
			"project_id":  num("REQUIRED. Project ID."),
			"name":        str("REQUIRED. Epic name."),
			"description": str("Epic description."),
			"priority":    str("Priority: 0=low, 1=normal (default), 2=high, 3=critical."),
		}), wrap(ctx, s.guard(authz.Member, s.createEpic)))

	mcp.AddTool(tool("amp_list_epics", "List all epics for a project.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Member, s.listEpics)))

	mcp.AddTool(tool("amp_get_epic", "Get an epic by ID.",
		props{"epic_id": num("REQUIRED. Epic ID.")}), wrap(ctx, s.guard(authz.Member, s.getEpic)))

	mcp.AddTool(tool("amp_update_epic", "Update an epic's name, description, priority, or owners. Only its owners or a project owner can change it.",
		props{
			"epic_id":       num("REQUIRED. Epic ID."),
			"name":          str("New epic name."),
			"description":   str("New epic description."),
			"priority":      str("New priority: 0=low, 1=normal, 2=high, 3=critical."),
			"add_owners":    strArr("Optional. Users (email, id, or 'me') to add as owners."),
			"remove_owners": strArr("Optional. Users (email, id, or 'me') to remove as owners."),
		}), wrap(ctx, s.guard(authz.Member, s.updateEpic)))

	// ---- Stories ----
	mcp.AddTool(tool("amp_create_story", "Create a story inside an epic.",
		props{
			"project_id":          num("REQUIRED. Project ID."),
			"epic_id":             num("REQUIRED. Epic ID."),
			"name":                str("REQUIRED. Story name."),
			"description":         str("Story description."),
			"acceptance_criteria": str("Conditions that prove this story is done."),
			"priority":            str("Priority: 0=low, 1=normal, 2=high, 3=critical."),
		}), wrap(ctx, s.guard(authz.Member, s.createStory)))

	mcp.AddTool(tool("amp_list_stories", "List all stories for an epic.",
		props{"epic_id": num("REQUIRED. Epic ID.")}), wrap(ctx, s.guard(authz.Member, s.listStories)))

	mcp.AddTool(tool("amp_get_story", "Get a story by ID.",
		props{"story_id": num("REQUIRED. Story ID.")}), wrap(ctx, s.guard(authz.Member, s.getStory)))

	mcp.AddTool(tool("amp_update_story", "Update a story's name, description, acceptance criteria, priority, or owners. Only its owners or a project owner can change it.",
		props{
			"story_id":            num("REQUIRED. Story ID."),
			"name":                str("New story name."),
			"description":         str("New story description."),
			"acceptance_criteria": str("New acceptance criteria."),
			"priority":            str("New priority: 0=low, 1=normal, 2=high, 3=critical."),
			"add_owners":          strArr("Optional. Users (email, id, or 'me') to add as owners."),
			"remove_owners":       strArr("Optional. Users (email, id, or 'me') to remove as owners."),
		}), wrap(ctx, s.guard(authz.Member, s.updateStory)))

	// ---- Tasks (all mutations go through actors) ----
	mcp.AddTool(tool("amp_create_task",
		"Create a task. REQUIRED hierarchy: every task must belong to a story which must belong to an epic. "+
			"Set assigned_to at planning time so the user can review who will work each ticket before dispatch. "+
			"If dependency_ids are provided, state is set automatically (blocked/backlog). Never set state yourself. "+
			"Optional start_at schedules the task to be unblocked at a future time.",
		props{
			"project_id":          num("REQUIRED. Project ID."),
			"epic_id":             num("REQUIRED. Epic ID — tasks must belong to an epic."),
			"story_id":            num("REQUIRED. Story ID — tasks must belong to a story."),
			"name":                str("REQUIRED. Task name."),
			"description":         str("REQUIRED. Full instructions for the worker agent."),
			"acceptance_criteria": str("REQUIRED. Specific verifiable conditions that prove the task is done."),
			"priority":            str("Priority: 0=low, 1=normal (default), 2=high, 3=critical."),
			"assigned_to":         str("REQUIRED. Who should work this task, e.g. 'amp-worker'. Set this at planning time so the user can review and correct before dispatch."),
			"dependency_ids":      arr("Optional. Task IDs that must complete before this task can run. State is derived automatically — do not set it yourself."),
			"start_at":            str("Optional. ISO 8601 datetime (e.g. '2026-04-14T09:00:00Z') to schedule this task. Task will be held as blocked until this time arrives."),
			"owner":               str("Optional. The AMP user who owns the ticket: omit for yourself, 'none' to leave it unassigned for anyone on the project to pick up, or a project member's email/id."),
		}), wrap(ctx, s.guard(authz.Member, s.createTask)))

	mcp.AddTool(tool("amp_list_tasks",
		"List tasks for a project. By default only YOUR work: tickets you own plus unassigned tickets you created. "+
			"Pass owner='unassigned' to see the pool anyone can pick up, owner='any' for everything, or a user's email/id. "+
			"Response includes a ready_to_dispatch array (state=backlog) — dispatch everything in it. Blocked tasks include blocked_by_ids showing exactly which task IDs are in the way. Scheduled tasks are in a separate bucket waiting for their start_at time.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"state":      str("Optional state filter: backlog, in_progress, completed, blocked."),
			"owner":      str("Optional: 'mine' (default), 'unassigned', 'any', or a user's email/id."),
		}), wrap(ctx, s.guard(authz.Member, s.listTasks)))

	mcp.AddTool(tool("amp_get_task", "Get full task details by ID.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.guard(authz.Member, s.getTask)))

	mcp.AddTool(tool("amp_update_task", "Update task name, description, assigned_to, or agent_id.",
		props{
			"task_id":     num("REQUIRED. Task ID."),
			"name":        str("New task name."),
			"description": str("New task description."),
			"assigned_to": str("Who should work this task — can be corrected by the user before dispatch."),
			"agent_id":    str("Agent identifier — set at dispatch time, not plan time."),
		}), wrap(ctx, s.guard(authz.Member, s.updateTask)))

	mcp.AddTool(tool("amp_claim_task",
		"Take ownership of an unassigned ticket so you (and your agents) can work it. Fails if someone else already owns it: "+
			"claiming never takes a ticket from anyone (to take one over, use amp_assign_task with user='me').",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.guard(authz.Member, s.claimTask)))

	mcp.AddTool(tool("amp_assign_task",
		"Assign a ticket to a project member (it then shows up in their work), or release it with user='none' so anyone can pick it up. "+
			"You can assign tickets you own or unassigned ones; team leads and project owners can reassign anyone's. "+
			"In projects with open_reassign on (the default), any project member can also reassign or take over (user='me') "+
			"someone else's ticket while it isn't in progress.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"user":    str("REQUIRED. Email or user id of a project member, 'me', or 'none' to unassign."),
		}), wrap(ctx, s.guard(authz.Member, s.assignTask)))

	mcp.AddTool(tool("amp_dispatch_task", "Dispatch a task to an agent (sets state to in_progress). Fails if task is blocked. An unassigned ticket becomes yours; a ticket owned by someone else can't be dispatched.",
		props{
			"task_id":  num("REQUIRED. Task ID."),
			"agent_id": str("REQUIRED. Agent identifier, e.g. 'amp-worker'."),
		}), wrap(ctx, s.guard(authz.Member, s.dispatchTask)))

	mcp.AddTool(tool("amp_complete_task", "Mark a task completed. Auto-unblocks any tasks that were waiting on it.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.guard(authz.Member, s.completeTask)))

	mcp.AddTool(tool("amp_block_task", "Block a task with a reason.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"reason":  str("REQUIRED. Why the task is blocked."),
		}), wrap(ctx, s.guard(authz.Member, s.blockTask)))

	mcp.AddTool(tool("amp_add_task_comment", "Post a comment to a task's log. Used by agents to report progress.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"body":    str("REQUIRED. Comment text."),
			"author":  str("Optional. Who is posting (defaults to 'agent')."),
		}), wrap(ctx, s.guard(authz.Member, s.addComment)))

	mcp.AddTool(tool("amp_get_task_comments", "Get all comments for a task.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.guard(authz.Member, s.getComments)))

	mcp.AddTool(tool("amp_get_ticket_history",
		"Get the full activity log for a task — every state change, dispatch, completion, comment, and unblock, "+
			"in chronological order. Use this to understand what has happened on a ticket and who did it.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.guard(authz.Member, s.getTicketHistory)))

	// ---- Management / cleanup tools ----

	mcp.AddTool(tool("amp_reset_project",
		"Delete ALL epics, stories, and tasks for a project but keep the project itself. "+
			"Use this when replanning from scratch. DESTRUCTIVE — cannot be undone.",
		props{
			"project_id": num("REQUIRED. Project ID to reset."),
		}), wrap(ctx, s.guard(authz.Owner, s.resetProject)))

	mcp.AddTool(tool("amp_delete_task",
		"Delete a single task. Use this to remove tasks created by mistake during planning.",
		props{
			"task_id": num("REQUIRED. Task ID to delete."),
		}), wrap(ctx, s.guard(authz.Member, s.deleteTask)))

	mcp.AddTool(tool("amp_delete_epic",
		"Delete an epic and ALL its stories and tasks (cascades). Use with care.",
		props{
			"epic_id": num("REQUIRED. Epic ID to delete."),
		}), wrap(ctx, s.guard(authz.Member, s.deleteEpic)))

	mcp.AddTool(tool("amp_set_task_state",
		"Override a task's state directly. Use as a manager escape hatch — e.g. re-open a "+
			"completed task, reset an in_progress task back to backlog if its agent crashed. "+
			"Valid states: backlog, in_progress, completed, blocked.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"state":   str("REQUIRED. New state: backlog, in_progress, completed, or blocked."),
			"reason":  str("Optional. Why this manual override was needed (logged to activity)."),
		}), wrap(ctx, s.guard(authz.Member, s.setTaskState)))

	mcp.AddTool(tool("amp_set_task_start_at",
		"Set or clear a task's scheduled start time. If start_at is provided, the task will be blocked until that time. "+
			"If start_at is omitted or empty, clears any existing schedule.",
		props{
			"task_id":  num("REQUIRED. Task ID."),
			"start_at": str("Optional. ISO 8601 datetime (e.g. '2026-04-14T09:00:00Z') to schedule this task. Omit to clear the schedule."),
		}), wrap(ctx, s.guard(authz.Member, s.setTaskStartAt)))

	mcp.AddTool(tool("amp_list_project_stories",
		"List all stories for a project across all epics. Useful for getting a full picture "+
			"without iterating epics one by one.",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.guard(authz.Member, s.listProjectStories)))

	// ---- Export/Import ----
	mcp.AddTool(tool("amp_export_project",
		"Export a complete project as JSON. Returns the full ExportBundle with all epics, stories, tasks, and KB documents.",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.guard(authz.Member, s.exportProject)))

	mcp.AddTool(tool("amp_import_project",
		"Import a project from an ExportBundle JSON string. Creates a new project with all epics, stories, tasks, and KB documents. "+
			"Optional code and name parameters override the bundle's project identity.",
		props{
			"bundle_json": str("REQUIRED. The full JSON from amp_export_project as a string."),
			"code":        str("Optional. Override the project code from the bundle."),
			"name":        str("Optional. Override the project name from the bundle."),
			"org_id":      num("Optional. Org to import into (see amp_create_project)."),
			"team_id":     num("Optional. Team that will own the imported project."),
		}), wrap(ctx, s.open(s.importProject)))

	// ---- Knowledge Base ----
	//
	// Three kinds of KB: each project's (the default), each team's, and each
	// org's. Every tool takes the same scope arguments; see
	// docs/kb-scopes-design.md and internal/service/kb.go for the rules.
	kbArgs := func(extra props) props {
		p := props{
			"project_id": num("The project you're working in. Required for the project KB (the default); with scope 'team' or 'org' it selects that project's team or org."),
			"scope":      str("Optional. Which knowledge base: 'project' (default), 'team', or 'org'. Use 'team'/'org' only for knowledge shared beyond one project; see amp_kb_sources."),
			"team_id":    num("Optional, scope 'team': which team, when the project has several of yours (or there is no project_id)."),
			"org_id":     num("Optional, scope 'org' without a project_id: which org."),
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}

	mcp.AddTool(tool("amp_kb_sources",
		"List the knowledge bases around a project: its own KB, its org's KB, and the KB of each team linked to it "+
			"(owner team first), with document counts and whether you can read and write each. Call this when you start "+
			"work in a project to know what shared knowledge exists. The project KB is where project knowledge lives; "+
			"search a team or org KB when your user asks, or when you need something only it would hold (the org's brand "+
			"and design system, the team's deploy runbook). Teams you aren't on are listed without contents.",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.open(s.kbSources)))

	mcp.AddTool(tool("amp_kb_write",
		"Create or UPDATE a document in a knowledge base. Writes go to the PROJECT KB unless you pass scope, and that "+
			"is where almost everything belongs: this project's architecture, decisions, and gotchas. "+
			"Team KBs (scope 'team') hold what's true for every project a team works on: runbooks, services the team "+
			"owns, team conventions. Org KBs (scope 'org') hold what's true across the whole org: brand and design "+
			"system, process, standards, policy. Only write to a team or org KB when your user has decided the "+
			"knowledge belongs there; never move knowledge there on your own initiative. If you think something "+
			"belongs there, tell your user why and keep it in the project KB until they decide. Team and org writes "+
			"need `reason`. Only org owners and admins write to an org KB; anyone else annotates it. "+
			"This is a full upsert keyed on path — if a doc at that path already exists, its title, content, and tags are "+
			"completely replaced (its annotations are kept). "+
			"BEFORE writing, always call amp_kb_get to check if a doc exists at that path. If it does, read the "+
			"existing content and merge your new information into it rather than overwriting with only what you know. "+
			"Prefer updating an existing doc over creating a new one on the same topic.",
		kbArgs(props{
			"path":    str("REQUIRED. Document path e.g. 'architecture/auth.md'. Same path = update existing doc."),
			"title":   str("REQUIRED. Document title."),
			"content": str("REQUIRED. Full document content in markdown. For updates, include ALL existing content plus your additions — this fully replaces the doc."),
			"tags":    strArr("REQUIRED. Tag array e.g. ['auth','jwt']. For updates, include existing tags plus any new ones."),
			"author":  str("Optional. Who wrote this (defaults to you)."),
			"reason": str("Required for scope 'team' or 'org': specifically why this belongs to the whole team or org rather " +
				"than one project's KB, and that your user decided it (e.g. 'Brand colours apply to every product; asked by jane@ to add to the org KB'). Stored with the doc."),
		})), wrap(ctx, s.open(s.kbWrite)))

	mcp.AddTool(tool("amp_kb_search",
		"Search one knowledge base using hybrid keyword+semantic search: the project KB by default, or a team or "+
			"org KB with scope. Returns the most relevant document chunks with short excerpts. "+
			"Use this when you need to find existing documentation on a specific topic. "+
			"Default limit is 3 — only increase if initial results are insufficient.",
		kbArgs(props{
			"query":            str("REQUIRED. Natural language search query."),
			"tags":             strArr("Optional. Filter results to docs with these tags."),
			"limit":            num("Optional. Max results to return (default 3, max 10)."),
			"recency_boost":    num("Optional. Weight for recency in sorting (0 = no recency weight)."),
			"min_recency_days": num("Optional. Filter out documents older than N days (0 = no filter)."),
		})), wrap(ctx, s.open(s.kbSearch)))

	mcp.AddTool(tool("amp_kb_get",
		"Get the full content of a knowledge base document by its path (project KB unless scope says otherwise). "+
			"Returns a 404-style error if the doc does not exist — use this to check "+
			"existence before deciding whether to create or update. Team and org docs include the reason they were placed there.",
		kbArgs(props{
			"path": str("REQUIRED. Document path, e.g. 'architecture/auth.md'."),
		})), wrap(ctx, s.open(s.kbGet)))

	mcp.AddTool(tool("amp_kb_list",
		"List all documents in a knowledge base (project KB unless scope says otherwise). Optionally filter by tag.",
		kbArgs(props{
			"tag": str("Optional. Filter to documents with this tag."),
		})), wrap(ctx, s.open(s.kbList)))

	mcp.AddTool(tool("amp_kb_delete",
		"Delete a document from a knowledge base by its path (project KB unless scope says otherwise). "+
			"Only delete team or org docs when your user asks.",
		kbArgs(props{
			"path": str("REQUIRED. Document path to delete."),
		})), wrap(ctx, s.open(s.kbDelete)))

	mcp.AddTool(tool("amp_kb_tags",
		"List all tags used in a knowledge base (project KB unless scope says otherwise) with document counts.",
		kbArgs(nil)), wrap(ctx, s.open(s.kbTags)))

	mcp.AddTool(tool("amp_kb_reindex",
		"Re-embed all documents in a knowledge base (project KB unless scope says otherwise). "+
			"Use after changing the embedding model or if search quality degrades.",
		kbArgs(nil)), wrap(ctx, s.open(s.kbReindex)))

	mcp.AddTool(tool("amp_kb_annotate",
		"Append a lightweight annotation to an existing KB doc. Use instead of rewriting for small corrections, and "+
			"to flag a team or org doc that looks wrong or out of date (anyone who can read a KB can annotate it). "+
			"Annotations are returned in amp_kb_get and previewed in amp_kb_search results.",
		kbArgs(props{
			"path":   str("REQUIRED. Document path, e.g. 'architecture/auth.md'."),
			"text":   str("REQUIRED. Annotation text — the correction or note."),
			"author": str("Optional. Who is annotating (defaults to you)."),
		})), wrap(ctx, s.open(s.kbAnnotate)))

	mcp.AddTool(tool("amp_kb_status",
		"Get KB health metrics: doc counts, stale docs, annotation stats, and tag distribution (project KB unless scope says otherwise). Use this to assess KB health before curator maintenance.",
		kbArgs(nil)), wrap(ctx, s.open(s.kbStatus)))
}

// ---- Tool handlers ----

func (s *Server) createProject(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	req := domain.CreateProjectRequest{
		Name:        optStr(args, "name", ""),
		Code:        optStr(args, "code", ""),
		Description: optStr(args, "description", ""),
	}
	if req.Name == "" || req.Code == "" {
		return nil, fmt.Errorf("name and code are required")
	}
	home, err := s.svc.ResolveProjectHome(rctx, caller(rctx).p, optIntPtr(args, "org_id"), optIntPtr(args, "team_id"))
	if err != nil {
		return nil, err
	}
	req.OrgID, req.TeamID, req.CreatedBy = home.OrgID, home.TeamID, home.CreatedBy
	p, err := s.repo.CreateProject(context.Background(), req)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: p.ID, Payload: p, At: time.Now()})
	return jsonResult(p)
}

func (s *Server) listProjects(rctx context.Context, _ map[string]interface{}) (*mcpgo.CallToolResult, error) {
	visible, err := s.svc.Authz.VisibleProjects(rctx, caller(rctx).p)
	if err != nil {
		return nil, err
	}
	projects, err := s.repo.ListProjects(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]domain.Project, 0, len(projects))
	for _, p := range projects {
		if visible == nil || visible[p.ID] {
			out = append(out, p)
		}
	}
	return jsonResult(map[string]interface{}{"projects": out})
}

func (s *Server) whoami(rctx context.Context, _ map[string]interface{}) (*mcpgo.CallToolResult, error) {
	p := caller(rctx).p
	out := map[string]interface{}{"user_id": p.UserID, "email": p.Email, "global_admin": p.Super}
	if p.HasUser() {
		orgs, err := s.repo.ListOrgs(rctx, p.UserRef())
		if err != nil {
			return nil, err
		}
		type orgTeams struct {
			domain.Org
			Teams []domain.Member `json:"teams"`
		}
		withTeams := make([]orgTeams, 0, len(orgs))
		for _, o := range orgs {
			teams, err := s.repo.ListUserTeams(rctx, o.ID, p.UserID)
			if err != nil {
				return nil, err
			}
			// ListUserTeams packs team id/name into Member's UserID/Email.
			withTeams = append(withTeams, orgTeams{Org: o, Teams: teams})
		}
		out["orgs"] = withTeams
	}
	return jsonResult(out)
}

func (s *Server) listProjectMembers(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	members, err := s.svc.ProjectMembers(rctx, projectID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"members": members, "your_access": caller(rctx).lvl.String()})
}

func (s *Server) claimTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	t, err := s.svc.ClaimTask(rctx, caller(rctx).p, taskID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"task": t})
}

func (s *Server) assignTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	user, err := reqStr(args, "user")
	if err != nil {
		return nil, err
	}
	t, err := s.svc.AssignTask(rctx, caller(rctx).p, taskID, user)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"task": t})
}

func (s *Server) getProject(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	p, err := s.repo.GetProject(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"project": p})
}

func (s *Server) archiveProject(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	p, err := s.repo.ArchiveProject(context.Background(), id)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectArchived, ProjectID: p.ID, Payload: p, At: time.Now()})
	return jsonResult(map[string]interface{}{"project": p})
}

func (s *Server) updateProjectSettings(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	var u domain.ProjectSettings
	if on, ok := args["open_reassign"].(bool); ok {
		u.OpenReassign = &on
	}
	if v, ok := args["name"].(string); ok {
		u.Name = &v
	}
	if v, ok := args["description"].(string); ok {
		u.Description = &v
	}
	p, err := s.svc.UpdateProject(rctx, caller(rctx).p, id, u)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectUpdated, ProjectID: p.ID, Payload: p, At: time.Now()})
	return jsonResult(map[string]interface{}{"project": p})
}

func (s *Server) restoreProject(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	p, err := s.repo.RestoreProject(context.Background(), id)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectRestored, ProjectID: p.ID, Payload: p, At: time.Now()})
	return jsonResult(map[string]interface{}{"project": p})
}

func (s *Server) createEpic(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), projectID); err != nil {
		return nil, err
	}
	req := domain.CreateEpicRequest{
		ProjectID:   projectID,
		Name:        optStr(args, "name", ""),
		Description: optStr(args, "description", ""),
		Priority:    optStr(args, "priority", "1"),
		CreatedBy:   caller(rctx).p.UserRef(),
	}
	// Route through ProjectActor so EpicActor is spawned and registered.
	pid, err := s.registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplyCreateEpic, 1)
	s.registry.System().Root.Send(pid, &actor.MsgCreateEpic{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(reply.Epic)
}

func (s *Server) listEpics(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	epics, err := s.repo.ListEpics(context.Background(), projectID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"epics": epics})
}

func (s *Server) getEpic(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, err
	}
	e, err := s.repo.GetEpic(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"epic": e})
}

func (s *Server) createStory(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), projectID); err != nil {
		return nil, err
	}
	epicID, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, fmt.Errorf("epic_id is required: every story must belong to an epic")
	}

	// Cross-reference: verify epic belongs to this project.
	epic, err := s.repo.GetEpic(context.Background(), epicID)
	if err != nil {
		return nil, fmt.Errorf("epic_id %d not found", epicID)
	}
	if epic.ProjectID != projectID {
		return nil, fmt.Errorf("epic %d belongs to project %d, not project %d", epicID, epic.ProjectID, projectID)
	}

	name := optStr(args, "name", "")
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	req := domain.CreateStoryRequest{
		ProjectID:          projectID,
		EpicID:             epicID,
		Name:               name,
		Description:        optStr(args, "description", ""),
		AcceptanceCriteria: optStr(args, "acceptance_criteria", ""),
		Priority:           optStr(args, "priority", "1"),
		CreatedBy:          caller(rctx).p.UserRef(),
	}
	// Route through ProjectActor → EpicActor so StoryActor is spawned and registered.
	pid, err := s.registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplyCreateStory, 1)
	s.registry.System().Root.Send(pid, &actor.MsgCreateStory{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(reply.Story)
}

func (s *Server) listStories(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	epicID, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, err
	}
	stories, err := s.repo.ListStories(context.Background(), epicID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"stories": stories})
}

func (s *Server) getStory(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "story_id")
	if err != nil {
		return nil, err
	}
	st, err := s.repo.GetStory(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"story": st})
}

func (s *Server) createTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), projectID); err != nil {
		return nil, err
	}

	// Enforce hierarchy: epic_id and story_id are both required.
	epicID, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, fmt.Errorf("epic_id is required: every task must belong to an epic")
	}
	storyID, err := reqInt(args, "story_id")
	if err != nil {
		return nil, fmt.Errorf("story_id is required: every task must belong to a story")
	}

	name := optStr(args, "name", "")
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	// Cross-reference validation: verify story belongs to this epic and project.
	// This prevents tasks from being mis-parented across projects or epics.
	ctx := context.Background()
	story, err := s.repo.GetStory(ctx, storyID)
	if err != nil {
		return nil, fmt.Errorf("story_id %d not found", storyID)
	}
	if story.ProjectID != projectID {
		return nil, fmt.Errorf("story %d belongs to project %d, not project %d", storyID, story.ProjectID, projectID)
	}
	if story.EpicID != epicID {
		return nil, fmt.Errorf("story %d belongs to epic %d, not epic %d — epic_id must match the story's epic", storyID, story.EpicID, epicID)
	}

	// Verify epic also belongs to this project.
	epic, err := s.repo.GetEpic(ctx, epicID)
	if err != nil {
		return nil, fmt.Errorf("epic_id %d not found", epicID)
	}
	if epic.ProjectID != projectID {
		return nil, fmt.Errorf("epic %d belongs to project %d, not project %d", epicID, epic.ProjectID, projectID)
	}

	// Parse optional start_at parameter
	var startAt *time.Time
	startAtStr := optStr(args, "start_at", "")
	if startAtStr != "" {
		t, err := time.Parse(time.RFC3339, startAtStr)
		if err != nil {
			return nil, fmt.Errorf("invalid start_at format: %w", err)
		}
		startAt = &t
	}

	req := domain.CreateTaskRequest{
		ProjectID:          projectID,
		EpicID:             epicID,
		StoryID:            storyID,
		Name:               name,
		Description:        optStr(args, "description", ""),
		AcceptanceCriteria: optStr(args, "acceptance_criteria", ""),
		Priority:           optStr(args, "priority", "1"),
		AssignedTo:         optStr(args, "assigned_to", ""),
		DependencyIDs:      optIntSlice(args, "dependency_ids"),
		StartAt:            startAt,
		CreatedBy:          caller(rctx).p.UserRef(),
	}
	if req.OwnerUserID, err = s.svc.ResolveNewTaskOwner(rctx, caller(rctx).p, projectID, optStr(args, "owner", "")); err != nil {
		return nil, err
	}

	replyCh := make(chan ReplyCreateTask, 1)
	pid, err := s.registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgCreateTask{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(reply.Task)
}

func (s *Server) listTasks(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	state := optStr(args, "state", "")
	me := caller(rctx).p
	view, err := s.svc.Authz.ParseTaskView(rctx, me, optStr(args, "owner", ""))
	if err != nil {
		return nil, err
	}

	replyCh := make(chan ReplyListTasks, 1)
	pid, err := s.registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgListTasks{ProjectID: projectID, State: state, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}

	// Bucket tasks by state so the manager loop needs zero reasoning.
	// Initialize all slices so empty buckets marshal as [] not null.
	readyToDispatch := make([]domain.Task, 0)
	inProgress := make([]domain.Task, 0)
	blocked := make([]domain.Task, 0)
	scheduled := make([]domain.Task, 0)
	completed := make([]domain.Task, 0)
	tasks := authz.FilterTasks(reply.Tasks, view, me.UserID)
	for _, t := range tasks {
		switch t.State {
		case domain.TaskStateBacklog:
			readyToDispatch = append(readyToDispatch, t)
		case domain.TaskStateInProgress:
			inProgress = append(inProgress, t)
		case domain.TaskStateBlocked:
			// Separate scheduled tasks (block_reason starts with "scheduled:") from dependency-blocked tasks
			if strings.HasPrefix(t.BlockReason, "scheduled:") {
				scheduled = append(scheduled, t)
			} else {
				blocked = append(blocked, t)
			}
		case domain.TaskStateCompleted:
			completed = append(completed, t)
		}
	}

	return jsonResult(map[string]interface{}{
		"ready_to_dispatch": readyToDispatch, // dispatch ALL of these immediately
		"in_progress":       inProgress,
		"blocked":           blocked,   // each has blocked_by_ids showing what's in the way
		"scheduled":         scheduled, // tasks waiting for their start_at time
		"completed":         completed,
		"count":             len(tasks),
		"owner_filter":      view.Mode, // pass owner='unassigned' or 'any' to widen
	})
}

func (s *Server) getTask(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	// We need the project_id to route to the right actor.
	// Get it from the repo first (cheap indexed lookup).
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	// Return from actor's in-memory snapshot for freshness.
	replyCh := make(chan ReplyGetTask, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgGetTask{TaskID: taskID, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task": reply.Task})
}

func (s *Server) updateTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	if _, _, err := s.svc.LoadTask(rctx, caller(rctx).p, taskID, authz.TaskWork); err != nil {
		return nil, err
	}
	// Route via repo lookup then actor.
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}
	req := domain.UpdateTaskRequest{
		TaskID:      taskID,
		Name:        optStr(args, "name", ""),
		Description: optStr(args, "description", ""),
		AssignedTo:  optStr(args, "assigned_to", ""),
		AgentID:     optStr(args, "agent_id", ""),
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgUpdateTask{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "updated": true})
}

func (s *Server) updateEpic(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	epicID, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, err
	}
	if _, err := s.svc.RequireEditEpic(rctx, caller(rctx).p, epicID); err != nil {
		return nil, err
	}
	epic, err := s.repo.GetEpic(context.Background(), epicID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), epic.ProjectID); err != nil {
		return nil, err
	}
	req := domain.UpdateEpicRequest{
		EpicID:      epicID,
		Name:        optStr(args, "name", ""),
		Description: optStr(args, "description", ""),
		Priority:    optStr(args, "priority", ""),
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(epic.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgUpdateEpic{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	if err := s.changeOwners(rctx, "epic", epicID, epic.ProjectID, args); err != nil {
		return nil, err
	}
	updated, err := s.repo.GetEpic(rctx, epicID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"epic_id": epicID, "updated": true, "epic": updated})
}

func (s *Server) updateStory(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	storyID, err := reqInt(args, "story_id")
	if err != nil {
		return nil, err
	}
	if _, err := s.svc.RequireEditStory(rctx, caller(rctx).p, storyID); err != nil {
		return nil, err
	}
	story, err := s.repo.GetStory(context.Background(), storyID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), story.ProjectID); err != nil {
		return nil, err
	}
	req := domain.UpdateStoryRequest{
		StoryID:            storyID,
		Name:               optStr(args, "name", ""),
		Description:        optStr(args, "description", ""),
		AcceptanceCriteria: optStr(args, "acceptance_criteria", ""),
		Priority:           optStr(args, "priority", ""),
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(story.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgUpdateStory{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	if err := s.changeOwners(rctx, "story", storyID, story.ProjectID, args); err != nil {
		return nil, err
	}
	updated, err := s.repo.GetStory(rctx, storyID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"story_id": storyID, "updated": true, "story": updated})
}

func (s *Server) dispatchTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	agentID, err := reqStr(args, "agent_id")
	if err != nil {
		return nil, err
	}
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}
	if _, err := s.svc.Dispatch(rctx, caller(rctx).p, task.ID, agentID); err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "dispatched_to": agentID})
}

func (s *Server) completeTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	if _, _, err := s.svc.LoadTask(rctx, caller(rctx).p, taskID, authz.TaskWork); err != nil {
		return nil, err
	}
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgCompleteTask{TaskID: taskID, By: caller(rctx).p.UserRef(), ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "state": "completed"})
}

func (s *Server) blockTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	if _, _, err := s.svc.LoadTask(rctx, caller(rctx).p, taskID, authz.TaskWork); err != nil {
		return nil, err
	}
	reason := optStr(args, "reason", "")
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgBlockTask{TaskID: taskID, Reason: reason, By: caller(rctx).p.UserRef(), ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "state": "blocked"})
}

func (s *Server) addComment(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	body, err := reqStr(args, "body")
	if err != nil {
		return nil, err
	}
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}
	req := domain.AddCommentRequest{
		TaskID:       taskID,
		Body:         body,
		Author:       optStr(args, "author", "agent"),
		AuthorUserID: caller(rctx).p.UserRef(),
	}
	replyCh := make(chan ReplyAddComment, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgAddComment{Req: req, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "comment_added": true})
}

func (s *Server) getComments(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan ReplyGetComments, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgGetComments{TaskID: taskID, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"comments": reply.Comments})
}

func (s *Server) getTicketHistory(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	history, err := s.repo.GetTicketHistory(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	// Also include the task itself so the agent gets full context in one call.
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{
		"task":    task,
		"history": history,
		"count":   len(history),
	})
}

func (s *Server) resetProject(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	p, err := s.repo.GetProject(context.Background(), projectID)
	if err != nil {
		return nil, fmt.Errorf("project %d not found", projectID)
	}
	if p.State == domain.ProjectStateArchived {
		return nil, fmt.Errorf("⚠️  Project %d (%s) is archived. Restore it first with amp_restore_project before making changes.", p.ID, p.Name)
	}
	// 1. Wipe postgres
	if err := s.repo.ResetProject(context.Background(), projectID); err != nil {
		return nil, fmt.Errorf("reset project: %w", err)
	}
	// 2. Tell the actor to clear its in-memory snapshot — must happen AFTER db wipe
	pid, err := s.registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.registry.System().Root.Send(pid, &actor.MsgReset{ReplyCh: replyCh})
	<-replyCh
	return jsonResult(map[string]interface{}{
		"project_id": projectID,
		"reset":      true,
		"message":    "All epics, stories, and tasks deleted. Project record and actor snapshot cleared.",
	})
}

func (s *Server) deleteTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	if _, _, err := s.svc.LoadTask(rctx, caller(rctx).p, taskID, authz.TaskDelete); err != nil {
		return nil, err
	}
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("task %d not found", taskID)
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}
	// 1. Delete from postgres
	if err := s.repo.DeleteTask(context.Background(), taskID); err != nil {
		return nil, fmt.Errorf("delete task: %w", err)
	}
	// 2. Evict from actor snapshot
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.registry.System().Root.Send(pid, &actor.MsgDeleteTask{TaskID: taskID, ReplyCh: replyCh})
	<-replyCh
	return jsonResult(map[string]interface{}{"task_id": taskID, "deleted": true})
}

func (s *Server) deleteEpic(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	epicID, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, err
	}
	if _, err := s.svc.RequireEditEpic(rctx, caller(rctx).p, epicID); err != nil {
		return nil, err
	}
	epic, err := s.repo.GetEpic(context.Background(), epicID)
	if err != nil {
		return nil, fmt.Errorf("epic %d not found", epicID)
	}
	if err := s.checkProjectNotArchived(context.Background(), epic.ProjectID); err != nil {
		return nil, err
	}
	// 1. Delete from postgres (cascades to stories and tasks)
	if err := s.repo.DeleteEpic(context.Background(), epicID); err != nil {
		return nil, fmt.Errorf("delete epic: %w", err)
	}
	// 2. Evict all tasks for this epic from actor snapshot
	pid, err := s.registry.Get(epic.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.registry.System().Root.Send(pid, &actor.MsgDeleteEpic{EpicID: epicID, ReplyCh: replyCh})
	<-replyCh
	return jsonResult(map[string]interface{}{"epic_id": epicID, "deleted": true})
}

func (s *Server) setTaskState(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	if _, _, err := s.svc.LoadTask(rctx, caller(rctx).p, taskID, authz.TaskWork); err != nil {
		return nil, err
	}
	stateStr := optStr(args, "state", "")
	if stateStr == "" {
		return nil, fmt.Errorf("state is required: backlog, in_progress, completed, or blocked")
	}
	validStates := map[string]bool{"backlog": true, "in_progress": true, "completed": true, "blocked": true}
	if !validStates[stateStr] {
		return nil, fmt.Errorf("invalid state %q — must be: backlog, in_progress, completed, blocked", stateStr)
	}
	reason := optStr(args, "reason", "manual override by manager")

	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("task %d not found", taskID)
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}
	fromState := string(task.State)

	// Route through actor hierarchy so the TaskActor's in-memory state is updated.
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.registry.System().Root.Send(pid, &actor.MsgSetTaskState{
		TaskID:  taskID,
		State:   domain.TaskState(stateStr),
		Reason:  reason,
		By:      caller(rctx).p.UserRef(),
		ReplyCh: replyCh,
	})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}

	return jsonResult(map[string]interface{}{
		"task_id":    taskID,
		"from_state": fromState,
		"to_state":   stateStr,
		"reason":     reason,
	})
}

func (s *Server) setTaskStartAt(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	if _, _, err := s.svc.LoadTask(rctx, caller(rctx).p, taskID, authz.TaskWork); err != nil {
		return nil, err
	}

	// Parse optional start_at parameter
	var startAt *time.Time
	startAtStr := optStr(args, "start_at", "")
	if startAtStr != "" {
		t, err := time.Parse(time.RFC3339, startAtStr)
		if err != nil {
			return nil, fmt.Errorf("invalid start_at format: %w", err)
		}
		startAt = &t
	}

	// Get task to find project_id for routing
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("task %d not found", taskID)
	}
	if err := s.checkProjectNotArchived(context.Background(), task.ProjectID); err != nil {
		return nil, err
	}

	// Route through actor hierarchy so the TaskActor's in-memory state is updated
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.registry.System().Root.Send(pid, &actor.MsgSetTaskStartAt{
		TaskID:  taskID,
		StartAt: startAt,
		ReplyCh: replyCh,
	})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}

	return jsonResult(map[string]interface{}{
		"task_id":  taskID,
		"start_at": startAt,
	})
}

func (s *Server) listProjectStories(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	stories, err := s.repo.ListStoriesByProject(context.Background(), projectID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"stories": stories, "count": len(stories)})
}

// ---- Export/Import handlers ----

func (s *Server) exportProject(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	// 1. Build the export bundle from the database
	bundle, err := s.repo.BuildExportBundle(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("build export bundle: %w", err)
	}

	// 2. Fetch all KB documents for this project
	docSummaries, err := s.kb.ListDocs(ctx, kb.Project(projectID), "")
	if err != nil {
		return nil, fmt.Errorf("list kb docs: %w", err)
	}

	// 3. Fetch full content for each KB document
	kbDocs := make([]domain.ExportKBDoc, 0, len(docSummaries))
	for _, summary := range docSummaries {
		doc, err := s.kb.GetDoc(ctx, kb.Project(projectID), summary.Path)
		if err != nil {
			// Log warning but continue (graceful degradation)
			slog.Default().Warn("failed to fetch kb doc", "path", summary.Path, "err", err)
			continue
		}
		// Convert kb.Doc to ExportKBDoc
		exportDoc := domain.ExportKBDoc{
			Path:    doc.Path,
			Title:   doc.Title,
			Content: doc.Content,
			Tags:    doc.Tags, // kb.Doc.Tags is []string
			Author:  doc.Author,
			// Annotations travel with the doc.
			Annotations: doc.Annotations,
		}
		kbDocs = append(kbDocs, exportDoc)
	}

	// 4. Assemble the complete bundle with KB docs
	bundle.KBDocs = kbDocs

	return jsonResult(bundle)
}

func (s *Server) importProject(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	bundleJSON, err := reqStr(args, "bundle_json")
	if err != nil {
		return nil, err
	}

	// Parse the bundle JSON string
	var bundle domain.ExportBundle
	if err := json.Unmarshal([]byte(bundleJSON), &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal bundle json: %w", err)
	}

	// Get optional code and name overrides
	newCode := optStr(args, "code", "")
	newName := optStr(args, "name", "")

	ctx := context.Background()

	// 1. Import the bundle (creates project, epics, stories, tasks)
	home, err := s.svc.ResolveProjectHome(rctx, caller(rctx).p, optIntPtr(args, "org_id"), optIntPtr(args, "team_id"))
	if err != nil {
		return nil, err
	}
	newProject, err := s.repo.ImportBundle(ctx, bundle, newCode, newName, home)
	if err != nil {
		return nil, fmt.Errorf("import bundle: %w", err)
	}

	// 2. Spawn the ProjectActor for the new project
	pid, err := s.registry.Get(newProject.ID)
	if err != nil {
		return nil, fmt.Errorf("spawn project actor: %w", err)
	}
	_ = pid // actor is now registered

	// 3. Re-index KB documents into Typesense
	for _, kbDoc := range bundle.KBDocs {
		_, err := s.kb.WriteDoc(ctx, kb.Project(newProject.ID), kbDoc.Path, kbDoc.Title, kbDoc.Content, kbDoc.Author, kbDoc.Tags, "", kbDoc.Annotations)
		if err != nil {
			// Log warning but continue (graceful degradation)
			slog.Default().Warn("failed to write kb doc during import", "path", kbDoc.Path, "err", err)
			continue
		}
	}

	// 4. Publish event for real-time UI updates
	s.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: newProject.ID, Payload: newProject, At: time.Now()})

	return jsonResult(map[string]interface{}{"project": newProject})
}

// ---- KB handlers (scope rules in internal/service/kb.go) ----

// What a KB tool call does to the KB it names.
type kbUse int

const (
	kbRead     kbUse = iota // read or search
	kbAnnotate              // annotate: anyone who can read
	kbChange                // write, delete, reindex
)

// kbOf resolves the knowledge base a KB tool call names and checks the
// caller may use it that way. Annotations and changes to a project KB also
// need the project not to be archived.
func (s *Server) kbOf(ctx context.Context, args map[string]interface{}, use kbUse) (*service.KB, error) {
	k, err := s.svc.ResolveKB(ctx, caller(ctx).p, service.KBRef{
		Scope:     optStr(args, "scope", ""),
		ProjectID: optInt(args, "project_id", 0),
		TeamID:    optInt(args, "team_id", 0),
		OrgID:     optInt(args, "org_id", 0),
	})
	if err != nil {
		return nil, err
	}
	if use == kbChange {
		if err := k.RequireWrite(); err != nil {
			return nil, err
		}
	}
	if use != kbRead && k.Kind == kb.ScopeProject {
		if err := s.checkProjectNotArchived(ctx, k.ID); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// requireKB fails when no knowledge base is configured. Handlers call it
// after the access and argument checks, so those are reported first.
func (s *Server) requireKB() error {
	if s.kb == nil {
		return fmt.Errorf("the knowledge base is not configured")
	}
	return nil
}

// authorOf is the author recorded for a write or annotation: what the
// caller passed, or who they are.
func authorOf(ctx context.Context, args map[string]interface{}) string {
	if a := strings.TrimSpace(optStr(args, "author", "")); a != "" {
		return a
	}
	if e := caller(ctx).p.Email; e != "" {
		return e
	}
	return "agent"
}

// minPlacementReason is the shortest reason accepted for a team or org write:
// enough to force a real sentence, not a token.
const minPlacementReason = 20

func (s *Server) kbSources(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	var count func(kb.Scope) int
	if s.kb != nil {
		count = func(sc kb.Scope) int {
			n, _ := s.kb.CountDocs(ctx, sc)
			return n
		}
	}
	sources, err := s.svc.KBSources(ctx, caller(ctx).p, projectID, count)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{
		"sources": sources,
		"note": "The project KB is the default for reading and writing. Use scope 'team' (with team_id if you're on several of " +
			"these teams) or 'org' on the amp_kb_* tools to reach the others. Write to them only when your user decides the knowledge belongs there.",
	})
}

func (s *Server) kbWrite(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbChange)
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	title, err := reqStr(args, "title")
	if err != nil {
		return nil, err
	}
	content, err := reqStr(args, "content")
	if err != nil {
		return nil, err
	}
	tags := optStrSlice(args, "tags")
	reason := ""
	if k.Kind != kb.ScopeProject {
		reason = strings.TrimSpace(optStr(args, "reason", ""))
		if len(reason) < minPlacementReason {
			return nil, fmt.Errorf("writing to the %s %s KB needs a reason: specifically why this belongs to the whole %s rather than one project's KB, "+
				"and that your user decided it. If your user hasn't decided, write it to the project KB and suggest the move to them instead",
				k.Name, k.Kind, k.Kind)
		}
	}

	if err := s.requireKB(); err != nil {
		return nil, err
	}
	doc, err := s.kb.WriteDoc(ctx, k.Scope, path, title, content, authorOf(ctx, args), tags, reason, nil)
	if err != nil {
		return nil, err
	}
	return jsonResult(doc)
}

func (s *Server) kbSearch(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbRead)
	if err != nil {
		return nil, err
	}
	query, err := reqStr(args, "query")
	if err != nil {
		return nil, err
	}
	tags := optStrSlice(args, "tags")
	limit := optInt(args, "limit", 10)
	recencyBoost := optFloat(args, "recency_boost", 0)
	minRecencyDays := optInt(args, "min_recency_days", 0)

	if err := s.requireKB(); err != nil {
		return nil, err
	}
	results, err := s.kb.Search(ctx, k.Scope, query, tags, limit, recencyBoost, minRecencyDays)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"kb": k, "results": results, "count": len(results)})
}

func (s *Server) kbGet(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbRead)
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	doc, err := s.kb.GetDoc(ctx, k.Scope, path)
	if err != nil {
		return nil, err
	}
	return jsonResult(doc)
}

func (s *Server) kbList(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbRead)
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	docs, err := s.kb.ListDocs(ctx, k.Scope, optStr(args, "tag", ""))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"kb": k, "docs": docs, "count": len(docs)})
}

func (s *Server) kbDelete(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbChange)
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	if err := s.kb.DeleteDoc(ctx, k.Scope, path); err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"deleted": true, "path": path, "kb": k})
}

func (s *Server) kbTags(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbRead)
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	tags, err := s.kb.ListTags(ctx, k.Scope)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"tags": tags})
}

func (s *Server) kbReindex(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbChange)
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	go func() {
		if err := s.kb.Reindex(context.Background(), k.Scope); err != nil {
			slog.Default().Error("kb reindex failed", "kb", k.Scope.String(), "err", err)
		}
	}()
	return jsonResult(map[string]interface{}{"reindex": "started", "kb": k})
}

func (s *Server) kbAnnotate(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	// Anyone who can read a KB can annotate it.
	k, err := s.kbOf(ctx, args, kbAnnotate)
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	text, err := reqStr(args, "text")
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	ann, err := s.kb.AnnotateDoc(ctx, k.Scope, path, text, authorOf(ctx, args))
	if err != nil {
		return nil, err
	}
	return jsonResult(ann)
}

func (s *Server) kbStatus(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbRead)
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	status, err := s.kb.KBStatus(ctx, k.Scope)
	if err != nil {
		return nil, err
	}
	return jsonResult(status)
}

// ---- Platform administration handlers (rules in internal/service/orgs.go) ----

func action(args map[string]interface{}) string {
	return strings.ToLower(strings.TrimSpace(optStr(args, "action", "")))
}

func badAction(a string, valid string) error {
	return fmt.Errorf("unknown action %q; use one of: %s", a, valid)
}

func (s *Server) listOrgs(rctx context.Context, _ map[string]interface{}) (*mcpgo.CallToolResult, error) {
	orgs, err := s.svc.ListOrgs(rctx, caller(rctx).p)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"orgs": orgs})
}

func (s *Server) createOrg(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	org, inv, err := s.svc.CreateOrg(rctx, caller(rctx).p, optStr(args, "name", ""), optStr(args, "slug", ""), optStr(args, "owner", ""))
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"org": org}
	if inv != nil {
		out["owner_invite"] = inv
	}
	return jsonResult(out)
}

func (s *Server) manageOrgMembers(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	orgID, err := reqInt(args, "org_id")
	if err != nil {
		return nil, err
	}
	p, user, role := caller(rctx).p, optStr(args, "user", ""), optStr(args, "role", "")
	switch a := action(args); a {
	case "list":
		members, err := s.svc.ListOrgMembers(rctx, p, orgID)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"members": members})
	case "add", "set_role":
		res, err := s.svc.AddOrgMember(rctx, p, orgID, user, role, a == "add")
		if err != nil {
			return nil, err
		}
		return jsonResult(res)
	case "remove":
		uid, err := s.svc.RemoveOrgMember(rctx, p, orgID, user)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"org_id": orgID, "user_id": uid, "removed": true})
	default:
		return nil, badAction(a, "list, add, set_role, remove")
	}
}

func (s *Server) manageTeams(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	p, name := caller(rctx).p, optStr(args, "name", "")
	switch a := action(args); a {
	case "list", "create":
		orgID, err := reqInt(args, "org_id")
		if err != nil {
			return nil, err
		}
		if a == "list" {
			teams, err := s.svc.ListTeams(rctx, p, orgID)
			if err != nil {
				return nil, err
			}
			return jsonResult(map[string]interface{}{"teams": teams})
		}
		team, err := s.svc.CreateTeam(rctx, p, orgID, name)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"team": team})
	case "rename", "delete":
		teamID, err := reqInt(args, "team_id")
		if err != nil {
			return nil, err
		}
		if a == "delete" {
			if err := s.svc.DeleteTeam(rctx, p, teamID); err != nil {
				return nil, err
			}
			return jsonResult(map[string]interface{}{"team_id": teamID, "deleted": true})
		}
		team, err := s.svc.RenameTeam(rctx, p, teamID, name)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"team": team})
	default:
		return nil, badAction(a, "list, create, rename, delete")
	}
}

func (s *Server) manageTeamMembers(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	teamID, err := reqInt(args, "team_id")
	if err != nil {
		return nil, err
	}
	p, user, role := caller(rctx).p, optStr(args, "user", ""), optStr(args, "role", "")
	switch a := action(args); a {
	case "list":
		team, members, err := s.svc.GetTeam(rctx, p, teamID)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"team": team, "members": members})
	case "add", "set_role":
		res, err := s.svc.AddTeamMember(rctx, p, teamID, user, role, a == "add")
		if err != nil {
			return nil, err
		}
		return jsonResult(res)
	case "remove":
		uid, err := s.svc.RemoveTeamMember(rctx, p, teamID, user)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"team_id": teamID, "user_id": uid, "removed": true})
	default:
		return nil, badAction(a, "list, add, set_role, remove")
	}
}

func (s *Server) manageInvites(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	orgID, err := reqInt(args, "org_id")
	if err != nil {
		return nil, err
	}
	p := caller(rctx).p
	switch a := action(args); a {
	case "list":
		invites, err := s.svc.ListInvites(rctx, p, orgID)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"invites": invites})
	case "create":
		res, err := s.svc.Invite(rctx, p, domain.Invite{
			Email: optStr(args, "email", ""), OrgID: orgID,
			OrgRole: optStr(args, "org_role", ""), TeamID: optIntPtr(args, "team_id"), TeamRole: optStr(args, "team_role", ""),
		})
		if err != nil {
			return nil, err
		}
		return jsonResult(res)
	case "revoke":
		id, err := reqInt(args, "invite_id")
		if err != nil {
			return nil, err
		}
		if err := s.svc.RevokeInvite(rctx, p, orgID, id); err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"invite_id": id, "revoked": true})
	default:
		return nil, badAction(a, "list, create, revoke")
	}
}

func (s *Server) manageProjectTeams(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	p := caller(rctx).p
	var teams []domain.ProjectTeam
	switch a := action(args); a {
	case "list":
		teams, err = s.svc.ListProjectTeams(rctx, p, projectID)
	case "link", "unlink":
		teamID, terr := reqInt(args, "team_id")
		if terr != nil {
			return nil, terr
		}
		if a == "link" {
			teams, err = s.svc.SetProjectTeam(rctx, p, projectID, teamID, optStr(args, "role", ""))
		} else {
			teams, err = s.svc.RemoveProjectTeam(rctx, p, projectID, teamID)
		}
	default:
		return nil, badAction(a, "list, link, unlink")
	}
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"project_id": projectID, "teams": teams})
}

// changeOwners applies add_owners / remove_owners for an epic or story.
// The caller's edit rights were already checked.
func (s *Server) changeOwners(rctx context.Context, kind string, id, projectID int, args map[string]interface{}) error {
	p := caller(rctx).p
	if add := optStrSlice(args, "add_owners"); len(add) > 0 {
		ids, err := s.svc.ResolveOwnerRefs(rctx, p, projectID, add)
		if err != nil {
			return err
		}
		if kind == "epic" {
			err = s.repo.AddEpicOwners(rctx, id, ids)
		} else {
			err = s.repo.AddStoryOwners(rctx, id, ids)
		}
		if err != nil {
			return err
		}
	}
	if remove := optStrSlice(args, "remove_owners"); len(remove) > 0 {
		ids := make([]int, 0, len(remove))
		for _, ref := range remove {
			uid, err := s.svc.Authz.ResolveUser(rctx, p, ref)
			if err != nil {
				return err
			}
			ids = append(ids, uid)
		}
		var err error
		if kind == "epic" {
			err = s.repo.RemoveEpicOwners(rctx, id, ids)
		} else {
			err = s.repo.RemoveStoryOwners(rctx, id, ids)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkProjectNotArchived returns an error if the project is archived.
// Call this at the start of every mutating MCP tool handler.
// Read-only tools and lifecycle tools (archive/restore) are exempt.
func (s *Server) checkProjectNotArchived(ctx context.Context, projectID int) error {
	p, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project %d not found", projectID)
	}
	if p.State == domain.ProjectStateArchived {
		return fmt.Errorf("⚠️  Project %d (%s) is archived. Restore it first with amp_restore_project before making changes.", p.ID, p.Name)
	}
	return nil
}

// ---- Type aliases to avoid cross-package repetition ----
type ReplyCreateEpic = actor.ReplyCreateEpic
type ReplyCreateStory = actor.ReplyCreateStory
type ReplyCreateTask = actor.ReplyCreateTask
type ReplyListTasks = actor.ReplyListTasks
type ReplyGetTask = actor.ReplyGetTask
type ReplySimple = actor.ReplySimple
type ReplyAddComment = actor.ReplyAddComment
type ReplyGetComments = actor.ReplyGetComments

// ---- Schema helpers ----

type props = map[string]interface{}

func str(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func num(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "number", "description": desc}
}

func boolean(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

func strArr(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":        "array",
		"description": desc,
		"items":       map[string]interface{}{"type": "string"},
	}
}

func arr(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":        "array",
		"description": desc,
		"items":       map[string]interface{}{"type": "number"},
	}
}

func tool(name, desc string, schema props) mcpgo.Tool {
	raw, _ := json.Marshal(map[string]interface{}{
		"type":       "object",
		"properties": schema,
	})
	return mcpgo.NewToolWithRawSchema(name, desc, raw)
}

type handlerFunc = func(context.Context, map[string]interface{}) (*mcpgo.CallToolResult, error)

// wrap adapts a handler to mcp-go. The request context is passed through:
// it carries the caller's identity from the auth middleware, in both the
// session-based and the stateless protocol eras.
func wrap(_ context.Context, fn handlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return fn(ctx, request.GetArguments())
	}
}

type principalKey struct{}

type callerInfo struct {
	p   authz.Principal
	lvl authz.Level // on the tool's project, when it has one
}

func caller(ctx context.Context) callerInfo {
	c, _ := ctx.Value(principalKey{}).(callerInfo)
	return c
}

// open resolves the caller for tools that aren't scoped to one project.
func (s *Server) open(fn handlerFunc) handlerFunc {
	return func(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
		p, err := authz.FromContext(ctx)
		if err != nil {
			return nil, err
		}
		return fn(context.WithValue(ctx, principalKey{}, callerInfo{p: p}), args)
	}
}

// guard resolves the caller and the project the tool acts on — from
// project_id, or from the task/epic/story it names — and requires at least
// min access there. Every project-scoped tool goes through it, so none can
// reach a project the caller isn't on. A project the caller can't see is
// reported as not found.
func (s *Server) guard(min authz.Level, fn handlerFunc) handlerFunc {
	return func(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
		p, err := authz.FromContext(ctx)
		if err != nil {
			return nil, err
		}
		projectID, err := s.projectOf(ctx, args)
		if err != nil {
			return nil, err
		}
		lvl, err := s.svc.Authz.RequireProject(ctx, p, projectID, min)
		if err != nil {
			if authz.StatusOf(err, 0) == 404 {
				// Name only what the caller asked for, not the project it's in.
				return nil, notFoundFor(args)
			}
			return nil, err
		}
		return fn(context.WithValue(ctx, principalKey{}, callerInfo{p: p, lvl: lvl}), args)
	}
}

func notFoundFor(args map[string]interface{}) error {
	for _, key := range []string{"project_id", "task_id", "story_id", "epic_id"} {
		if v, ok := args[key]; ok {
			return fmt.Errorf("%s %v not found", strings.TrimSuffix(key, "_id"), v)
		}
	}
	return fmt.Errorf("not found")
}

func (s *Server) projectOf(ctx context.Context, args map[string]interface{}) (int, error) {
	if _, ok := args["project_id"]; ok {
		return reqInt(args, "project_id")
	}
	if _, ok := args["task_id"]; ok {
		id, err := reqInt(args, "task_id")
		if err != nil {
			return 0, err
		}
		t, err := s.repo.GetTask(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("task %d not found", id)
		}
		return t.ProjectID, nil
	}
	if _, ok := args["story_id"]; ok {
		id, err := reqInt(args, "story_id")
		if err != nil {
			return 0, err
		}
		st, err := s.repo.GetStory(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("story %d not found", id)
		}
		return st.ProjectID, nil
	}
	if _, ok := args["epic_id"]; ok {
		id, err := reqInt(args, "epic_id")
		if err != nil {
			return 0, err
		}
		e, err := s.repo.GetEpic(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("epic %d not found", id)
		}
		return e.ProjectID, nil
	}
	return 0, fmt.Errorf("project_id is required")
}

func jsonResult(v interface{}) (*mcpgo.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcpgo.CallToolResult{
		Content: []mcpgo.Content{mcpgo.TextContent{Type: "text", Text: string(data)}},
	}, nil
}

// ---- Argument helpers ----

func reqInt(args map[string]interface{}, key string) (int, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("missing required argument: %q", key)
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	}
	return 0, fmt.Errorf("argument %q must be a number", key)
}

func reqStr(args map[string]interface{}, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", fmt.Errorf("missing required argument: %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

func optStr(args map[string]interface{}, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func optInt(args map[string]interface{}, key string, def int) int {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}

func optFloat(args map[string]interface{}, key string, def float64) float64 {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return def
}

func optIntPtr(args map[string]interface{}, key string) *int {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			i := int(n)
			return &i
		case int:
			return &n
		}
	}
	return nil
}

func optStrSlice(args map[string]interface{}, key string) []string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func optIntSlice(args map[string]interface{}, key string) []int {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		switch n := item.(type) {
		case float64:
			out = append(out, int(n))
		case int:
			out = append(out, n)
		}
	}
	return out
}
