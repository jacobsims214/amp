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
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	if kbSvc != nil && repo != nil {
		// One reindex per KB across every replica, not just this one.
		kbSvc.SetReindexLock(repo.TryNamedLock)
	}
	return &Server{registry: registry, repo: repo, hub: h, kb: kbSvc, svc: service.New(repo, registry, kbSvc)}
}

// Register wires all MCP tools onto the provided MCPServer.
func (s *Server) Register(mcp *server.MCPServer) {
	ctx := context.Background()

	// ---- Projects (CRUD via repo, no actor needed) ----
	mcp.AddTool(tool("amp_create_project", "Create a new AMP project in one of your orgs, owned by one of your teams. Returns the created project with its id.",
		props{
			"name":        str("REQUIRED. Project name."),
			"code":        str("REQUIRED. Short unique code, used as the ticket prefix (refs read CODE-149): 2-6 letters, e.g. 'NWC' for Northwind Commerce."),
			"description": str("Optional description."),
			"org_id":      num("Optional. Org to create it in; required only if you belong to several (see amp_whoami)."),
			"team_id":     num("Optional. Team that will own it; required only if you are on several teams in the org."),
			"metadata":    obj("Optional. Key/value metadata for integrations and reporting, e.g. {\"jira_board\": \"PLAT\", \"initiative\": \"Q3 growth\", \"capitalizable\": true}. Values: string, number, or boolean. Everything under it inherits it."),
		}), wrap(ctx, s.open(s.createProject)))

	mcp.AddTool(tool("amp_whoami",
		"Who you are acting as in AMP: your user, your orgs and your teams. Tickets you create are yours by default; "+
			"amp_list_tasks shows your own work unless you pass owner.",
		props{}), wrap(ctx, s.open(s.whoami)))

	mcp.AddTool(tool("amp_guide",
		"The AMP guide: how to use AMP itself, shipped with AMP and the same for everyone. Call it with no topic when "+
			"your user is new to AMP, asks how AMP works or what to do next, or wants to be onboarded: it returns the "+
			"onboarding script (start-here) and the list of guide pages, and you then walk your user through AMP step by "+
			"step, showing them in their own projects rather than reciting. Call it with a topic (a question, a feature "+
			"name like 'handover' or 'plan approval', or a page path) to look something up: it returns the best page in "+
			"full plus other matches. Also call it before telling a user AMP can't do something. Read-only.",
		props{
			"topic": str("Optional. A question, feature or page path (e.g. 'how do I hand tickets over', 'traceability', 'running-work.md'). Omit for the onboarding script and the page index."),
		}), wrap(ctx, s.open(s.guide)))

	mcp.AddTool(tool("amp_audit_log",
		"Read the audit log: who changed access (org, team, project links, roles, accounts, API tokens), settings, or "+
			"integrations, and who deleted what. Global admins read everything; org owners/admins pass org_id; "+
			"project owners pass project_id. Newest first.",
		props{
			"org_id":     num("Optional. Entries for this org and its projects."),
			"project_id": num("Optional. Entries for this project."),
			"actor_id":   num("Optional. Only changes made by this user id."),
			"action":     str("Optional. An action (e.g. 'org.member.set') or a prefix ending in '.' (e.g. 'team.')."),
			"before_id":  num("Optional. Page back: entries older than this id."),
			"limit":      num("Optional. Default 100, max 500."),
		}), wrap(ctx, s.open(s.auditLog)))

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
		"List the teams with access to a project, link a team (of the project's org) as owner or member, unlink one, "+
			"or move the project to another org (set_org). Project owners and org admins change any link; a team's lead may "+
			"link or unlink their own team as a member team. A project always keeps at least one owner team. "+
			"set_org: global admins may place any project (including one with no org); org owners/admins may move a "+
			"project between orgs they administer. Links to teams outside the new org are removed; pass team_id to make "+
			"a team of the new org an owner team.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"action":     str("REQUIRED. One of: list, link, unlink, set_org."),
			"team_id":    num("For link/unlink: team ID. For set_org (optional): a team of the new org to make an owner team."),
			"role":       str("For link: owner or member (default member)."),
			"org_id":     num("For set_org: the org to put the project in."),
		}), wrap(ctx, s.guard(authz.Member, s.manageProjectTeams)))

	mcp.AddTool(tool("amp_find_by_metadata",
		"Find projects, epics, stories, or tasks by their effective metadata (their own plus what they inherit from "+
			"their project, epic, and story), across every project you can see. E.g. kind='story', match={\"initiative\": \"Q3 growth\"}, "+
			"or kind='task', match={\"capitalizable\": true}. Use it to find work to sync to Jira, Odoo, or another tracker.",
		props{
			"kind":       str("REQUIRED. project, epic, story, or task."),
			"match":      obj("REQUIRED. Key/value pairs that must all match, with the same type as stored (\"true\" the string is not true the boolean)."),
			"project_id": num("Optional. Only this project."),
			"limit":      num("Optional. Max results (default 50, max 200)."),
		}), wrap(ctx, s.open(s.findByMetadata)))

	// ---- Daily reports ----
	mcp.AddTool(tool("amp_day_report",
		"Build your end-of-day report: use this when your user says 'wrap up today', 'end of day', 'day end report', or "+
			"asks what they got done. Returns a draft from the day's AMP activity across all their projects — tickets "+
			"completed, started, created, still in progress, blocked, and others worked on — plus any summary already "+
			"saved for that day. Nothing is saved: write a short summary with your user, then call amp_save_day_report.",
		props{
			"date": str("Optional. The day, YYYY-MM-DD (default: today in tz)."),
			"tz":   str("Optional. Your IANA time zone, e.g. 'America/New_York' (default UTC). Pass it: it decides where the day starts and ends."),
		}), wrap(ctx, s.open(s.dayReport)))

	mcp.AddTool(tool("amp_save_day_report",
		"Save your end-of-day report for a day, with a summary in your user's words (markdown: highlights, blockers, "+
			"what's next). The ticket lists are re-captured at save time. Saving again for the same day replaces it. "+
			"People in your orgs can read it (amp_list_day_reports), and the AMP UI has a copy-ready version.",
		props{
			"summary": str("REQUIRED. The day's summary in markdown, in your user's voice."),
			"date":    str("Optional. The day, YYYY-MM-DD (default: today in tz)."),
			"tz":      str("Optional. Your IANA time zone, e.g. 'America/New_York' (default UTC). Pass it: it decides where the day starts and ends."),
		}), wrap(ctx, s.open(s.saveDayReport)))

	mcp.AddTool(tool("amp_list_day_reports",
		"Read saved daily reports — yours or anyone's in your orgs — e.g. for a status update, or to sync the work to "+
			"Jira, Odoo, or another tracker. Each report has a summary, a copy-ready markdown version, and structured "+
			"ticket lists (with project, epic, story, and effective metadata such as a jira_key). Tickets in projects "+
			"you can't see are left out.",
		props{
			"user": str("Optional. Whose reports: 'me' (default), an email, or a user id."),
			"from": str("Optional. First day, YYYY-MM-DD (default: the 7 days ending on 'to')."),
			"to":   str("Optional. Last day, YYYY-MM-DD (default: today in tz)."),
			"tz":   str("Optional. IANA time zone for 'today' (default UTC)."),
		}), wrap(ctx, s.open(s.listDayReports)))

	mcp.AddTool(tool("amp_velocity",
		"Tickets someone completed per day (by project) over a range: yours or anyone's in your orgs. Every day in "+
			"the range is listed, zeros included. Only projects you can see are counted.",
		props{
			"user": str("Optional. Whose: 'me' (default), an email, or a user id."),
			"from": str("Optional. First day, YYYY-MM-DD (default: the 7 days ending on 'to')."),
			"to":   str("Optional. Last day, YYYY-MM-DD (default: today in tz)."),
			"tz":   str("Optional. IANA time zone that days are counted in (default UTC)."),
		}), wrap(ctx, s.open(s.velocity)))

	mcp.AddTool(tool("amp_human_impact",
		"How a project's tickets split between agents and people over a range: counts, how long each waited to be "+
			"picked up and took once begun, hours people reported, human tickets by reason, and the human tickets that "+
			"held the plan up longest (with how many tickets waited on them). For questions like 'what is the human impact', "+
			"'what slowed us down', or 'what could agents take over'.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"days":       num("Optional. How many days back (default 30)."),
		}), wrap(ctx, s.guard(authz.Member, s.humanImpact)))

	mcp.AddTool(tool("amp_list_projects", "List the active projects you have access to.",
		props{}), wrap(ctx, s.open(s.listProjects)))

	mcp.AddTool(tool("amp_get_project", "Get a project by its numeric ID.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Member, s.getProject)))

	mcp.AddTool(tool("amp_archive_project", "Archive a completed project. Sets state=archived.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Owner, s.archiveProject)))

	mcp.AddTool(tool("amp_restore_project", "Restore an archived project. Sets state=active.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Owner, s.restoreProject)))

	mcp.AddTool(tool("amp_update_project_settings",
		"Change a project's name, description, metadata, or settings (project owners only); pass only what changes. "+
			"open_reassign: when true (the default), any project member can reassign or release a ticket that isn't "+
			"in progress; when false, only its owner, a team lead, or a project owner can.",
		props{
			"project_id":    num("REQUIRED. Project ID."),
			"name":          str("Optional. New project name."),
			"description":   str("Optional. New project description."),
			"open_reassign": boolean("Optional. Let any project member reassign tickets that aren't in progress."),
			"require_plan_approval": boolean("Optional. When true (the default), tickets planned after the plan was last approved can't be " +
				"dispatched until a person approves the plan again in AMP's web UI. Agents can turn it on; only a person in the web UI can turn it off."),
			"metadata": obj("Optional. Metadata changes: keys with a value are set (string, number, or boolean), keys set to null are removed, other keys are kept."),
		}), wrap(ctx, s.guard(authz.Owner, s.updateProjectSettings)))

	mcp.AddTool(tool("amp_approve_plan",
		"Plan approval status. Agents can't approve a plan: a person approves it in AMP's web UI (Flight plan, Approve plan), "+
			"never through MCP and never because of something said in chat. This tool never approves: it always fails, with "+
			"approve_url (the page to send your person to), awaiting_approval and require_plan_approval. Use amp_check_plan to see "+
			"whether the plan is approved; until it is, dispatch refuses tickets planned since the last approval.",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.guard(authz.Member, s.approvePlan)))

	// ---- Epics ----
	mcp.AddTool(tool("amp_create_epic", "Create an epic inside a project. Epics and stories group work; what AMP schedules is the "+
		"dependency graph between tickets, so plan which tickets depend on which before creating them (see amp_create_tasks).",
		props{
			"project_id":  num("REQUIRED. Project ID."),
			"name":        str("REQUIRED. Epic name."),
			"description": str("Epic description."),
			"priority":    str("Priority: 0=low, 1=normal (default), 2=high, 3=critical."),
			"metadata":    obj("Optional. Key/value metadata for integrations and reporting, e.g. {\"jira_board\": \"PLAT\", \"initiative\": \"Q3 growth\", \"capitalizable\": true}. Values: string, number, or boolean. Everything under it inherits it."),
		}), wrap(ctx, s.guard(authz.Member, s.createEpic)))

	mcp.AddTool(tool("amp_list_epics", "List all epics for a project.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Member, s.listEpics)))

	mcp.AddTool(tool("amp_get_epic", "Get an epic by ID, with its effective_metadata (its project's metadata with its own on top).",
		props{"epic_id": num("REQUIRED. Epic ID.")}), wrap(ctx, s.guard(authz.Member, s.getEpic)))

	mcp.AddTool(tool("amp_update_epic", "Update an epic's name, description, priority, metadata, or owners. Only its owners or a project owner can change it.",
		props{
			"metadata":      obj("Optional. Metadata changes: keys with a value are set (string, number, or boolean), keys set to null are removed, other keys are kept."),
			"epic_id":       num("REQUIRED. Epic ID."),
			"name":          str("New epic name."),
			"description":   str("New epic description."),
			"priority":      str("New priority: 0=low, 1=normal, 2=high, 3=critical."),
			"clear":         strArr(clearDesc("description")),
			"add_owners":    strArr("Optional. Users (email, id, or 'me') to add as owners."),
			"remove_owners": strArr("Optional. Users (email, id, or 'me') to remove as owners."),
		}), wrap(ctx, s.guard(authz.Member, s.updateEpic)))

	// ---- Stories ----
	mcp.AddTool(tool("amp_create_story", "Create a story inside an epic. A story's tickets usually depend on each other, and often on "+
		"tickets in other stories (an API before the UI that calls it): wire those dependencies when you create the tickets.",
		props{
			"project_id":          num("REQUIRED. Project ID."),
			"epic_id":             num("REQUIRED. Epic ID."),
			"name":                str("REQUIRED. Story name."),
			"description":         str("Story description."),
			"acceptance_criteria": str("Conditions that prove this story is done."),
			"priority":            str("Priority: 0=low, 1=normal, 2=high, 3=critical."),
			"metadata":            obj("Optional. Key/value metadata for integrations and reporting, e.g. {\"jira_board\": \"PLAT\", \"initiative\": \"Q3 growth\", \"capitalizable\": true}. Values: string, number, or boolean. Everything under it inherits it."),
		}), wrap(ctx, s.guard(authz.Member, s.createStory)))

	mcp.AddTool(tool("amp_list_stories", "List all stories for an epic.",
		props{"epic_id": num("REQUIRED. Epic ID.")}), wrap(ctx, s.guard(authz.Member, s.listStories)))

	mcp.AddTool(tool("amp_get_story", "Get a story by ID, with its effective_metadata (its project's and epic's metadata with its own on top).",
		props{"story_id": num("REQUIRED. Story ID.")}), wrap(ctx, s.guard(authz.Member, s.getStory)))

	mcp.AddTool(tool("amp_update_story", "Update a story's name, description, acceptance criteria, priority, metadata, or owners. Only its owners or a project owner can change it.",
		props{
			"metadata":            obj("Optional. Metadata changes: keys with a value are set (string, number, or boolean), keys set to null are removed, other keys are kept."),
			"story_id":            num("REQUIRED. Story ID."),
			"name":                str("New story name."),
			"description":         str("New story description."),
			"acceptance_criteria": str("New acceptance criteria."),
			"priority":            str("New priority: 0=low, 1=normal, 2=high, 3=critical."),
			"clear":               strArr(clearDesc("description, acceptance_criteria")),
			"add_owners":          strArr("Optional. Users (email, id, or 'me') to add as owners."),
			"remove_owners":       strArr("Optional. Users (email, id, or 'me') to remove as owners."),
		}), wrap(ctx, s.guard(authz.Member, s.updateStory)))

	mcp.AddTool(tool("amp_move_story",
		"Move a story, with all its tickets, to another epic of the project. Dependencies and states are kept.",
		props{
			"story_id": num("REQUIRED. The story to move."),
			"epic_id":  num("REQUIRED. The epic to move it into."),
		}), wrap(ctx, s.guard(authz.Member, s.moveStory)))

	mcp.AddTool(tool("amp_delete_story",
		"Delete a story and all its tickets. Tickets elsewhere that depended on them are updated (and unblocked if "+
			"nothing else holds them). Prefer amp_move_task / amp_move_story to fix structure.",
		props{
			"story_id": num("REQUIRED. Story ID."),
		}), wrap(ctx, s.guard(authz.Member, s.deleteStory)))

	// ---- Tasks (all mutations go through actors) ----
	mcp.AddTool(tool("amp_create_task",
		"Create one ticket in a story. DEPENDENCIES ARE CRITICAL: AMP dispatches every ticket whose dependencies are done, "+
			"in parallel. A ticket created without dependency_ids is ready immediately, so if it needs another ticket's "+
			"output (a schema, an API, a design, a fix) it will start too early and fail. Before creating a ticket, decide "+
			"what it waits on and pass those ticket IDs in dependency_ids. To create several related tickets, prefer "+
			"amp_create_tasks, which wires dependencies between new tickets in one call. Fix mistakes with "+
			"amp_set_task_dependencies, amp_update_task, and amp_move_task — never by deleting and re-creating tickets. "+
			"State (blocked/backlog) is derived from dependencies; never set it yourself. Optional start_at schedules the ticket.",
		props{
			"project_id":          num("REQUIRED. Project ID."),
			"story_id":            num("REQUIRED. Story ID — every ticket belongs to a story."),
			"epic_id":             num("Optional. The story's epic (checked if given; otherwise taken from the story)."),
			"name":                str("REQUIRED. Ticket name."),
			"description":         str("REQUIRED. Full instructions for the worker agent."),
			"acceptance_criteria": str("REQUIRED. Specific verifiable conditions that prove the ticket is done."),
			"dependency_ids":      arr("The ticket IDs this ticket needs finished first. Leave empty ONLY if it truly needs nothing — it will be dispatchable immediately."),
			"priority":            str("Priority: 0=low, 1=normal (default), 2=high, 3=critical."),
			"assigned_to":         str("REQUIRED. Who should work this ticket, e.g. 'amp-worker'. Set at planning time so the user can review before dispatch."),
			"start_at":            str("Optional. ISO 8601 datetime (e.g. '2026-04-14T09:00:00Z') to schedule this ticket. It is held as blocked until then."),
			"owner":               str("Optional. The AMP user who owns the ticket: omit for yourself, 'none' to leave it unassigned for anyone on the project to pick up, or a project member's email/id."),
			"metadata":            obj("Optional. Key/value metadata for integrations, e.g. {\"jira_key\": \"PLAT-123\"}. Values: string, number, or boolean. Project, epic, and story metadata is inherited."),
		}), wrap(ctx, s.guard(authz.Member, s.createTask)))

	mcp.AddTool(tool("amp_create_tasks",
		"Create tickets (tasks), one or many, AND the dependencies between them in one call — the way to plan a story or "+
			"feature. Give each ticket a short `ref` (e.g. 'schema', 'api', 'ui'); `depends_on` lists refs of other tickets in "+
			"this batch, or IDs of existing tickets. Everything is checked first (unknown refs, other projects, cycles); then "+
			"tickets are created in dependency order. Returns the ID created for each ref. Tickets not connected to "+
			"anything are reported, since they can all start at once. A step only a person can do (review and merge a PR, "+
			"a sign-off, prod access, a decision, someone outside the team) is a ticket too: performer 'human' with its "+
			"owner as the person, wired into depends_on, so the agent tickets after it wait for them.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"tasks": map[string]interface{}{
				"type":        "array",
				"description": "REQUIRED. The tickets to create.",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"ref":                 str("REQUIRED. A short name for this ticket in the batch."),
						"story_id":            num("REQUIRED. The story it belongs to."),
						"name":                str("REQUIRED. Ticket name."),
						"description":         str("REQUIRED. Full instructions for the worker agent."),
						"acceptance_criteria": str("REQUIRED. Verifiable conditions that prove it's done."),
						"depends_on": map[string]interface{}{
							"type":        "array",
							"description": "Refs of tickets in this batch, or IDs of existing tickets, that must finish first.",
							"items":       map[string]interface{}{"type": []string{"string", "number"}},
						},
						"assigned_to":  str("Who should work it, e.g. 'amp-worker'."),
						"priority":     str("0=low, 1=normal (default), 2=high, 3=critical."),
						"owner":        str("Optional: omit for yourself, 'none' for unassigned, or a project member."),
						"start_at":     str("Optional. ISO 8601 datetime to schedule it; it's held until then."),
						"metadata":     obj("Optional key/value metadata."),
						"performer":    str("Optional. 'agent' (default: dispatched to an agent) or 'human': its owner does it and marks it done in AMP; agents never dispatch it."),
						"human_reason": str("With performer 'human': why a person does it. " + humanReasonList() + "."),
					},
					"required": []string{"ref", "story_id", "name"},
				},
			},
		}), wrap(ctx, s.guard(authz.Member, s.createTasks)))

	mcp.AddTool(tool("amp_set_task_dependencies",
		"Change which tickets a ticket waits on — fix a missing or wrong dependency without re-creating anything. "+
			"Either pass dependency_ids (the complete new list; [] removes all), or add and/or remove. Cycles, unknown "+
			"tickets, and other projects' tickets are refused. A ready ticket that now waits on unfinished work becomes "+
			"blocked; a ticket blocked only by its dependencies becomes ready when none are left.",
		props{
			"task_id":        num("REQUIRED. The ticket whose dependencies change."),
			"dependency_ids": arr("Optional. The complete list of ticket IDs it depends on (replaces the current list)."),
			"add":            arr("Optional. Ticket IDs to add as dependencies."),
			"remove":         arr("Optional. Ticket IDs to stop depending on."),
		}), wrap(ctx, s.guard(authz.Member, s.setTaskDependencies)))

	mcp.AddTool(tool("amp_move_task",
		"Move a ticket to another story (in any epic of the project). Its dependencies, owner, comments, and state are kept.",
		props{
			"task_id":  num("REQUIRED. The ticket to move."),
			"story_id": num("REQUIRED. The story to move it into."),
		}), wrap(ctx, s.guard(authz.Member, s.moveTask)))

	mcp.AddTool(tool("amp_check_plan",
		"Check a project's plan before dispatching: tickets connected to nothing, too many tickets that can start at once "+
			"(a sign of missing dependencies), empty stories, and the longest dependency chain. Also repairs tickets stuck "+
			"blocked on dependencies that are all done. Run it after planning and after changing the plan. "+
			"awaiting_approval lists open tickets planned since a person last approved the plan; when it isn't empty, approve_url is the "+
			"page in AMP's web UI where your person approves it (agents can't).",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.guard(authz.Member, s.checkPlan)))

	mcp.AddTool(tool("amp_list_tasks",
		"List tasks for a project. By default only YOUR work: tickets you own plus unassigned tickets you created. "+
			"Pass owner='unassigned' to see the pool anyone can pick up, owner='any' for everything, or a user's email/id. "+
			"ready_to_dispatch (state=backlog): dispatch everything in it. Waiting tickets show what they wait on; scheduled ones wait for their start_at time. "+
			"questions are blocked tickets with a question for a person: ask the user, then amp_answer_question.",
		props{
			"project_id":        num("REQUIRED. Project ID."),
			"state":             str("Optional state filter: backlog, in_progress, completed, blocked."),
			"owner":             str("Optional: 'mine' (default), 'unassigned', 'any', or a user's email/id."),
			"format":            str("Optional. 'compact' (default): ready_to_dispatch and in_progress as ticket IDs, questions with their text, and each open ticket as one line under its epic (id, name, state, owner, assigned_to, what it waits on); read a ticket with amp_get_task. 'full': every ticket's full record in state buckets."),
			"include_completed": boolean("Optional. Compact format: also list completed tickets under their epics (they're counted either way)."),
		}), wrap(ctx, s.guard(authz.Member, s.listTasks)))

	mcp.AddTool(tool("amp_get_task", "Read a ticket (task) by ID, with its effective_metadata (its project's, epic's, and story's metadata with its own on top). "+
		"Add include for its comments (plan, progress, questions, answers), history (state changes, dispatches, who did what) and runs (model, tokens, branch, commit, PR).",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"include": strArr("Optional. Any of 'comments', 'history', 'runs'."),
			"limit":   num("Optional. With include: the most recent N comments and history entries (default 30; 0 for all)."),
		}), wrap(ctx, s.guard(authz.Member, s.getTask)))

	mcp.AddTool(tool("amp_get_plan",
		"A project's plan as a tree: its epics, each with its stories and their ticket counts by state. "+
			"Pass epic_id for one epic. Read tickets with amp_list_tasks or amp_get_task.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"epic_id":    num("Optional. Only this epic."),
		}), wrap(ctx, s.guard(authz.Member, s.getPlan)))

	mcp.AddTool(tool("amp_update_task",
		"Update a ticket's name, description, acceptance criteria, priority, assigned_to, agent_id, metadata, or who does "+
			"it (performer: hand it to a person; turning a person's ticket into agent work is done by a person in the AMP web UI, "+
			"and is refused here with the ticket's link) — pass only what changes. To change what it depends on use amp_set_task_dependencies; to move it to another story, "+
			"amp_move_task; to schedule it, amp_set_task_start_at. Its creator, its owner, and team leads can change it.",
		props{
			"task_id":             num("REQUIRED. Task ID."),
			"name":                str("New ticket name."),
			"description":         str("New description."),
			"acceptance_criteria": str("New acceptance criteria."),
			"priority":            str("New priority: 0=low, 1=normal, 2=high, 3=critical."),
			"assigned_to":         str("Who should work this ticket — can be corrected by the user before dispatch."),
			"agent_id":            str("Agent identifier — set at dispatch time, not plan time."),
			"metadata":            obj(patchMetadataDesc),
			"performer":           str("'agent' or 'human' (its owner does it in AMP). Not while it's in progress. 'human' -> 'agent' only in the AMP web UI."),
			"human_reason":        str("Why a person does it. " + humanReasonList() + "."),
			"clear":               strArr(clearDesc("description, acceptance_criteria, assigned_to, agent_id")),
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

	mcp.AddTool(tool("amp_dispatch_task", "Start work on a ticket (task): dispatch it to an agent (sets state to in_progress). Fails if task is blocked. An unassigned ticket becomes yours; a ticket owned by someone else can't be dispatched. Human tickets (performer 'human') are never dispatched: their owner does them.",
		props{
			"task_id":  num("REQUIRED. Task ID."),
			"agent_id": str("REQUIRED. Agent identifier, e.g. 'amp-worker'."),
		}), wrap(ctx, s.guard(authz.Member, s.dispatchTask)))

	mcp.AddTool(tool("amp_complete_task", "Mark a task completed. Auto-unblocks any tasks that were waiting on it. "+
		"Also record the run: the model you used, your token usage if your harness can tell you (opencode: amp_session_usage), "+
		"and where the work is (branch, commit, PR, pushed). AMP reports token spend and traces code back to tickets from these. "+
		"Only a ticket that was dispatched (in progress) can be completed by an agent. A person's own human ticket is marked done by "+
		"that person in AMP's web UI, not through this tool.",
		withRunProps(props{
			"task_id":      num("REQUIRED. Task ID."),
			"hours_worked": num("Human tickets: the hours the person says they spent on it."),
		})), wrap(ctx, s.guard(authz.Member, s.completeTask)))
	mcp.AddTool(tool("amp_report_run",
		"Record an agent run on a ticket without changing its state: the model, token usage, and where the work went. "+
			"Use it for a run that ended some other way (killed, abandoned, handed over), for usage you learn about afterwards "+
			"(e.g. a manager reporting a subagent's usage from its result), or for a PR opened after completion.",
		withRunProps(props{
			"task_id":  num("REQUIRED. Task ID."),
			"outcome":  str("Optional. completed | blocked | progress (default) | abandoned."),
			"agent_id": str("Optional. The agent profile that ran (defaults to the ticket's agent)."),
		})), wrap(ctx, s.guard(authz.Member, s.reportRun)))
	mcp.AddTool(tool("amp_list_deleted_tasks", "A project's trash: tickets, epics and stories deleted in the last 30 days, which amp_restore_tasks can bring back.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Member, s.listDeleted)))
	mcp.AddTool(tool("amp_restore_tasks",
		"Bring deleted work back from the trash. Tickets come back with their comments, history, owner, state and "+
			"dependency links; a deleted epic comes back with its stories and their tickets, a deleted story with its "+
			"tickets, all with their original IDs. A project lead, or whoever deleted it, may restore it. A ticket needs "+
			"its story and a story its epic: restore those in the same call (epics, then stories, then tickets).",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"task_ids":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "number"}, "description": "Ticket IDs to restore."},
			"epic_ids":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "number"}, "description": "Deleted epic IDs to restore, with their stories and tickets."},
			"story_ids":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "number"}, "description": "Deleted story IDs to restore, with their tickets."},
		}), wrap(ctx, s.guard(authz.Member, s.restoreTasks)))
	mcp.AddTool(tool("amp_project_day_reports",
		"The project's standup sheet: everyone on the project and the day report they saved for a day, narrowed to this "+
			"project's tickets, plus who hasn't saved one. Use it to start the day (yesterday's) or to run a standup.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"date":       str("Optional. YYYY-MM-DD (default today in tz)."),
			"tz":         str("Optional. IANA time zone, e.g. America/New_York (default UTC)."),
		}), wrap(ctx, s.guard(authz.Member, s.projectDayReports)))
	mcp.AddTool(tool("amp_handover_tasks",
		"Move a person's open tickets in a project to someone else — they're out, leaving, or scope moved between people. "+
			"Each ticket is reassigned the normal way and gets a comment saying why. In-progress tickets stay unless "+
			"include_in_progress is set. A project lead may hand over anyone's work; anyone may hand over their own.",
		props{
			"project_id":          num("REQUIRED. Project ID."),
			"from":                str("REQUIRED. Whose tickets: user id or email."),
			"to":                  str("REQUIRED. Who takes them: user id or email."),
			"task_ids":            map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "number"}, "description": "Optional. Only these tickets (default: all of from's open tickets in the project)."},
			"include_in_progress": boolean("Optional. Also move tickets in progress (their agent works under the old owner)."),
			"reason":              str("Optional. Why, posted on each ticket, e.g. 'Sam out sick Monday; Harbor demo'."),
		}), wrap(ctx, s.guard(authz.Member, s.handover)))
	mcp.AddTool(tool("amp_list_unmerged", "Completed tickets whose pushed work isn't merged yet — done by an agent, waiting on a person to review and merge.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.guard(authz.Member, s.listUnmerged)))
	mcp.AddTool(tool("amp_get_task_runs", "A ticket's recorded runs (model, tokens, cost, branch, commit, PR) and their totals.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.guard(authz.Member, s.taskRuns)))
	mcp.AddTool(tool("amp_trace",
		"From code back to the work: find the tickets whose runs produced a commit, a branch, or a PR, across the projects you "+
			"can see. Then amp_get_task(task_id, include=[comments,history,runs]) for how it got there (plan, comments, decisions, runs).",
		props{
			"commit": str("A commit SHA, or at least its first 7 characters."),
			"branch": str("A branch name."),
			"pr_url": str("A pull request URL."),
		}), wrap(ctx, s.open(s.trace)))
	mcp.AddTool(tool("amp_report_merged",
		"Record that code is merged: every ticket whose runs produced the commit, branch or PR gets a run marked merged, "+
			"across the projects you can see. One call after merging, made by CI (an amp_ token that can read the project is "+
			"enough) or by a person. Give merge_commit to record the merge SHA. Returns the ticket IDs it marked.",
		props{
			"commit":       str("The commit SHA the work was recorded with, or at least its first 7 characters."),
			"branch":       str("The branch that was merged."),
			"pr_url":       str("The pull request URL that was merged."),
			"merge_commit": str("Optional. The merge commit SHA on the main line."),
		}), wrap(ctx, s.open(s.reportMerged)))

	mcp.AddTool(tool("amp_block_task",
		"Stop work on a ticket because it needs a person: a question, a decision, missing information, or access you don't have. "+
			"The reason is shown to people in AMP as the question to answer, so write it as one, with the options if there are any. "+
			"The ticket leaves in-progress and appears in amp_list_tasks' questions list. When someone answers (in the AMP UI, or with "+
			"amp_answer_question) the answer is posted on the ticket and it goes back to ready to be dispatched again. "+
			"Not for waiting on other tickets: that's what dependencies are for.",
		withRunProps(props{
			"task_id": num("REQUIRED. Task ID."),
			"reason":  str("REQUIRED. The question for a person, e.g. 'Avalara or Stripe Tax? Both work in the sandbox; Stripe is cheaper under 10k invoices a month.'"),
		})), wrap(ctx, s.guard(authz.Member, s.blockTask)))
	mcp.AddTool(tool("amp_answer_question",
		"Answer a ticket's question (a ticket an agent blocked with amp_block_task): the answer is posted on the ticket as a comment "+
			"and the ticket goes back to ready (or to waiting, if it still depends on unfinished tickets), so the next dispatch picks it up "+
			"with the answer in its comments. Use this only to relay an answer your person gave you in chat: it is recorded as relayed by "+
			"agent. People can also answer in the AMP UI.",
		props{
			"task_id": num("REQUIRED. Task ID of the ticket with the question."),
			"answer":  str("REQUIRED. The answer, as the person gave it, with any detail the next worker needs."),
		}), wrap(ctx, s.guard(authz.Member, s.answerQuestion)))

	s.registerArtifacts(mcp)
	s.registerDesign(mcp)

	mcp.AddTool(tool("amp_add_task_comment", "Post a comment to a task's log. Used by agents to report progress.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"body":    str("REQUIRED. Comment text."),
			"author":  str("Optional. Who is posting (defaults to 'agent'): your agent name. A person's email is recorded as '<email> (relayed by agent)'; to pass on a person's answer to a question, use amp_answer_question."),
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
		"Delete a single task. Use this to remove tasks created by mistake during planning. Deleted tickets stay in the project trash for 30 days (amp_list_deleted_tasks, amp_restore_tasks).",
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
			"Valid states: backlog, in_progress, completed, blocked. Agents can move work back (to backlog or blocked) "+
			"but not forward: in_progress needs amp_dispatch_task, completed needs a dispatched ticket, and a question is "+
			"answered (amp_answer_question), not cleared.",
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
			"scope":      str("Optional. Which knowledge base: 'project' (default), 'team', or 'org'. Use 'team'/'org' only for knowledge shared beyond one project; see amp_kb_sources. 'global' is the read-only AMP guide (how to use AMP itself; see amp_guide)."),
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
		"List the documents in a knowledge base (project KB unless scope says otherwise), most recently updated first, "+
			"without content. Optionally filter by tag. Returns one page: the newest 250 by default. total is how many docs "+
			"the listing has (with the tag, if given); next_offset is present when more remain: pass it as offset for the next page.",
		kbArgs(props{
			"tag":    str("Optional. Filter to documents with this tag."),
			"limit":  num(fmt.Sprintf("Optional. Docs per page (default %d, max %d).", kbListDefault, kbListMax)),
			"offset": num("Optional. How many docs to skip (default 0): the next_offset of the previous page."),
		})), wrap(ctx, s.open(s.kbList)))

	mcp.AddTool(tool("amp_kb_delete",
		"Delete a document from a knowledge base by its path (project KB unless scope says otherwise). "+
			"Only delete team or org docs when your user asks.",
		kbArgs(props{
			"path": str("REQUIRED. Document path to delete."),
		})), wrap(ctx, s.open(s.kbDelete)))

	mcp.AddTool(tool("amp_kb_list_deleted",
		"List a knowledge base's deleted documents that can still be restored (project KB unless scope says otherwise): "+
			"docs deleted in the last 30 days, with path, title, deleted_at and deleted_by, most recently deleted first. "+
			"Restore one with amp_kb_restore. Needs write access to that KB, the same as amp_kb_delete.",
		kbArgs(nil)), wrap(ctx, s.open(s.kbListDeleted)))

	mcp.AddTool(tool("amp_kb_restore",
		"Bring a knowledge base document back from the KB trash by its path (project KB unless scope says otherwise), with "+
			"the content, title, tags and annotations it had; it is readable and searchable again. Works for docs removed in the "+
			"last 30 days; list the KB trash first to find the path. Needs write access to that KB, the same as amp_kb_delete.",
		kbArgs(props{
			"path": str("REQUIRED. Path of the document in the trash, e.g. 'architecture/auth.md'."),
		})), wrap(ctx, s.open(s.kbRestore)))

	mcp.AddTool(tool("amp_kb_tags",
		"List all tags used in a knowledge base (project KB unless scope says otherwise) with document counts.",
		kbArgs(nil)), wrap(ctx, s.open(s.kbTags)))

	mcp.AddTool(tool("amp_kb_reindex",
		"Rebuild a knowledge base's search index: re-embed every document (project KB unless scope says otherwise). "+
			"Use it when search looks broken: scores far above 1, results that match only on keywords, or a doc "+
			"amp_kb_get can read that amp_kb_search never finds; also after the embedding model changes. "+
			"It runs in the background and returns at once; search again in a minute. Documents aren't changed. "+
			"Needs write access to that KB, the same as amp_kb_write (for an org KB: org owners and admins).",
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

	mcp.AddTool(tool("amp_kb_resolve_annotation",
		"Mark an annotation on a KB doc resolved, once its correction or note has been worked into the doc "+
			"(rewrite the doc with amp_kb_write first). Pass resolved=false to reopen one. Resolved annotations stay "+
			"on the doc as a record, but search previews show only unresolved ones. Needs write access to the KB "+
			"(for an org KB: org owners and admins).",
		kbArgs(props{
			"path":     str("REQUIRED. Document path, e.g. 'architecture/auth.md'."),
			"index":    num("REQUIRED. The annotation's position in the doc's annotations from amp_kb_get, 0-based, oldest first."),
			"resolved": boolean("Optional. true (default) to resolve, false to reopen."),
		})), wrap(ctx, s.open(s.kbResolveAnnotation)))

	mcp.AddTool(tool("amp_kb_status",
		"Get KB health metrics (project KB unless scope says otherwise): total_docs, stale_docs, docs_with_annotations, "+
			"total_annotations, unresolved_annotations and docs_by_tag. Also durability: stored_docs (live docs in Postgres, "+
			"the source of truth), indexed_docs (docs in the search index; index_error instead if it can't be reached) and "+
			"backfill {status: complete|failed|started|not_started, started_at, completed_at, doc_count, last_error}. "+
			"stored_docs equal to indexed_docs means search covers every doc. The global KB has no Postgres copy, so "+
			"stored_docs and backfill are absent there (note says why). Use this to assess KB health before curator maintenance.",
		kbArgs(nil)), wrap(ctx, s.open(s.kbStatus)))
}

// ---- Tool handlers ----

func (s *Server) createProject(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	meta, err := optNewMetadata(args)
	if err != nil {
		return nil, err
	}
	req := domain.CreateProjectRequest{
		Name:        optStr(args, "name", ""),
		Code:        optStr(args, "code", ""),
		Description: optStr(args, "description", ""),
		Metadata:    meta,
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
	s.svc.Record(rctx, caller(rctx).p, projectAudit("project.create", p.ID, "created project "+p.Name))
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
	out := map[string]interface{}{"user_id": p.UserID, "email": p.Email, "global_admin": p.Super,
		"account": map[bool]string{true: domain.UserKindAgent, false: domain.UserKindHuman}[p.Agent], "via_api_token": p.ViaToken}
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

func (s *Server) auditLog(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	entries, err := s.svc.Audit(rctx, caller(rctx).p, domain.AuditFilter{
		OrgID: optInt(args, "org_id", 0), ProjectID: optInt(args, "project_id", 0), ActorUserID: optInt(args, "actor_id", 0),
		Action: optStr(args, "action", ""), BeforeID: int64(optInt(args, "before_id", 0)), Limit: optInt(args, "limit", 0),
	})
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"entries": entries, "count": len(entries)})
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

func (s *Server) archiveProject(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	p, err := s.repo.ArchiveProject(context.Background(), id)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectArchived, ProjectID: p.ID, Payload: p, At: time.Now()})
	s.svc.Record(rctx, caller(rctx).p, projectAudit("project.archive", p.ID, "archived project "+p.Name))
	return jsonResult(map[string]interface{}{"project": p})
}

func (s *Server) approvePlan(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	// Never approves: a person approves in the web UI, and MCP is never the
	// web UI. Kept in the catalog (with the same input) so an old manager
	// prompt that calls it gets the link to send its person to, and stops.
	p := caller(rctx).p
	if p.InUI() {
		// Unreachable over the MCP listener; kept so the rule lives in one place.
		proj, err := s.svc.ApprovePlan(rctx, p, id)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"project_id": id, "plan_approved_at": proj.PlanApprovedAt})
	}
	status := map[string]interface{}{"approve_url": s.svc.PlanURL(id)}
	if r, err := s.svc.CheckPlan(rctx, p, id); err == nil {
		status["awaiting_approval"] = r.AwaitingApproval
		status["require_plan_approval"] = r.RequirePlanApproval
		status["plan_approved_at"] = r.ApprovedAt
	}
	detail, _ := json.Marshal(status)
	return nil, fmt.Errorf("%w %s", authz.UIOnly("Plan approval", s.svc.PlanURL(id)), detail)
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
	if on, ok := args["require_plan_approval"].(bool); ok {
		u.RequirePlanApproval = &on
	}
	if v, ok := args["name"].(string); ok {
		u.Name = &v
	}
	if v, ok := args["description"].(string); ok {
		u.Description = &v
	}
	if u.Metadata, err = optMetadataPatch(args); err != nil {
		return nil, err
	}
	p, err := s.svc.UpdateProject(rctx, caller(rctx).p, id, u)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectUpdated, ProjectID: p.ID, Payload: p, At: time.Now()})
	return jsonResult(map[string]interface{}{"project": p})
}

func (s *Server) restoreProject(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	id, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	p, err := s.repo.RestoreProject(context.Background(), id)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectRestored, ProjectID: p.ID, Payload: p, At: time.Now()})
	s.svc.Record(rctx, caller(rctx).p, projectAudit("project.restore", p.ID, "restored project "+p.Name))
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
	meta, err := optNewMetadata(args)
	if err != nil {
		return nil, err
	}
	req := domain.CreateEpicRequest{
		ProjectID:   projectID,
		Name:        optStr(args, "name", ""),
		Description: optStr(args, "description", ""),
		Priority:    optStr(args, "priority", "1"),
		CreatedBy:   caller(rctx).p.UserRef(),
		Metadata:    meta,
	}
	// Route through ProjectActor so EpicActor is spawned and registered.
	pid, err := s.registry.Get(projectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplyCreateEpic, 1)
	s.registry.System().Root.Send(pid, &actor.MsgCreateEpic{Req: req, ReplyCh: replyCh})
	reply := actor.Await(rctx, replyCh)
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
	eff, err := s.effectiveMetadata(context.Background(), "epic", e.ID, e.Metadata)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"epic": e, "effective_metadata": eff})
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

	meta, err := optNewMetadata(args)
	if err != nil {
		return nil, err
	}
	req := domain.CreateStoryRequest{
		Metadata:           meta,
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
	reply := actor.Await(rctx, replyCh)
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
	eff, err := s.effectiveMetadata(context.Background(), "story", st.ID, st.Metadata)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"story": st, "effective_metadata": eff})
}

func (s *Server) createTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(rctx, projectID); err != nil {
		return nil, err
	}
	storyID, err := reqInt(args, "story_id")
	if err != nil {
		return nil, fmt.Errorf("story_id is required: every task must belong to a story")
	}
	var startAt *time.Time
	if v := optStr(args, "start_at", ""); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("invalid start_at format: %w", err)
		}
		startAt = &t
	}
	meta, err := optNewMetadata(args)
	if err != nil {
		return nil, err
	}
	task, warnings, err := s.svc.CreateTask(rctx, caller(rctx).p, domain.CreateTaskRequest{
		ProjectID:          projectID,
		EpicID:             optInt(args, "epic_id", 0),
		StoryID:            storyID,
		Name:               optStr(args, "name", ""),
		Description:        optStr(args, "description", ""),
		AcceptanceCriteria: optStr(args, "acceptance_criteria", ""),
		Priority:           optStr(args, "priority", "1"),
		AssignedTo:         optStr(args, "assigned_to", ""),
		DependencyIDs:      optIntSlice(args, "dependency_ids"),
		StartAt:            startAt,
		Metadata:           meta,
	}, optStr(args, "owner", ""))
	if err != nil {
		return nil, err
	}
	return taskWithWarnings(task, warnings)
}

// taskWithWarnings returns a task as before, with any planning warnings
// alongside its fields.
func taskWithWarnings(t *domain.Task, warnings []string) (*mcpgo.CallToolResult, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return jsonResult(out)
}

func (s *Server) createTasks(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(rctx, projectID); err != nil {
		return nil, err
	}
	raw, ok := args["tasks"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("tasks is required: an array of tickets")
	}
	specs := make([]service.TaskSpec, 0, len(raw))
	for i, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("tasks[%d] must be an object", i)
		}
		meta, err := optNewMetadata(m)
		if err != nil {
			return nil, fmt.Errorf("tasks[%d]: %w", i, err)
		}
		var deps []string
		if list, ok := m["depends_on"].([]interface{}); ok {
			for _, d := range list {
				switch v := d.(type) {
				case string:
					deps = append(deps, v)
				case float64:
					deps = append(deps, fmt.Sprintf("%d", int(v)))
				default:
					return nil, fmt.Errorf("tasks[%d].depends_on: use refs (strings) or ticket ids (numbers)", i)
				}
			}
		}
		var startAt *time.Time
		if v := optStr(m, "start_at", ""); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("tasks[%d].start_at: %w", i, err)
			}
			startAt = &t
		}
		specs = append(specs, service.TaskSpec{
			Ref: optStr(m, "ref", ""), StoryID: optInt(m, "story_id", 0), Name: optStr(m, "name", ""),
			Description: optStr(m, "description", ""), AcceptanceCriteria: optStr(m, "acceptance_criteria", ""),
			Priority: optStr(m, "priority", "1"), AssignedTo: optStr(m, "assigned_to", ""), Owner: optStr(m, "owner", ""),
			DependsOn: deps, StartAt: startAt, Metadata: meta,
			Performer: optStr(m, "performer", ""), HumanReason: optStr(m, "human_reason", ""),
		})
	}
	created, warnings, err := s.svc.CreateTasks(rctx, caller(rctx).p, projectID, specs)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"created": created, "count": len(created)}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return jsonResult(out)
}

func (s *Server) setTaskDependencies(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	t, err := s.repo.GetTask(rctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task %d not found", taskID)
	}
	if err := s.checkProjectNotArchived(rctx, t.ProjectID); err != nil {
		return nil, err
	}
	_, replace := args["dependency_ids"]
	add, remove := optIntSlice(args, "add"), optIntSlice(args, "remove")
	var task *domain.Task
	var warnings []string
	switch {
	case replace && (len(add) > 0 || len(remove) > 0):
		return nil, fmt.Errorf("pass either dependency_ids (the full list) or add/remove, not both")
	case replace:
		task, warnings, err = s.svc.SetTaskDependencies(rctx, caller(rctx).p, taskID, optIntSlice(args, "dependency_ids"))
	case len(add) > 0 || len(remove) > 0:
		task, warnings, err = s.svc.EditDependencies(rctx, caller(rctx).p, taskID, add, remove)
	default:
		return nil, fmt.Errorf("pass dependency_ids (the full list, [] for none), or add and/or remove")
	}
	if err != nil {
		return nil, err
	}
	return taskWithWarnings(task, warnings)
}

func (s *Server) moveTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	storyID, err := reqInt(args, "story_id")
	if err != nil {
		return nil, err
	}
	t, err := s.repo.GetTask(rctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task %d not found", taskID)
	}
	if err := s.checkProjectNotArchived(rctx, t.ProjectID); err != nil {
		return nil, err
	}
	task, err := s.svc.MoveTask(rctx, caller(rctx).p, taskID, storyID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"task": task})
}

func (s *Server) moveStory(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	storyID, err := reqInt(args, "story_id")
	if err != nil {
		return nil, err
	}
	epicID, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, err
	}
	st, err := s.repo.GetStory(rctx, storyID)
	if err != nil {
		return nil, fmt.Errorf("story %d not found", storyID)
	}
	if err := s.checkProjectNotArchived(rctx, st.ProjectID); err != nil {
		return nil, err
	}
	story, err := s.svc.MoveStory(rctx, caller(rctx).p, storyID, epicID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"story": story})
}

func (s *Server) deleteStory(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	storyID, err := reqInt(args, "story_id")
	if err != nil {
		return nil, err
	}
	story, err := s.svc.RequireEditStory(rctx, caller(rctx).p, storyID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectNotArchived(rctx, story.ProjectID); err != nil {
		return nil, err
	}
	if err := s.svc.DeleteStory(rctx, caller(rctx).p, story); err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"story_id": storyID, "deleted": true})
}

func (s *Server) checkPlan(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	report, err := s.svc.CheckPlan(rctx, caller(rctx).p, projectID)
	if err != nil {
		return nil, err
	}
	return jsonResult(report)
}

// QuietAfter is how long an in-progress ticket can go without activity
// before amp_list_tasks reports it as quiet (the UI uses the same 15 minutes).
const QuietAfter = 15 * time.Minute

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
	reply := actor.Await(rctx, replyCh)
	if reply.Err != nil {
		return nil, reply.Err
	}

	// Bucket tasks by state so the manager loop needs zero reasoning.
	// Initialize all slices so empty buckets marshal as [] not null.
	readyToDispatch := make([]domain.Task, 0)
	inProgress := make([]domain.Task, 0)
	blocked := make([]domain.Task, 0)
	scheduled := make([]domain.Task, 0)
	questions := make([]domain.Task, 0)
	completed := make([]domain.Task, 0)
	tasks := authz.FilterTasks(reply.Tasks, view, me.UserID)
	people, err := s.waitingOnPeople(rctx, projectID, reply.Tasks, tasks)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		switch {
		case t.IsHuman() && (t.State == domain.TaskStateBacklog || t.State == domain.TaskStateInProgress):
			continue // a person's to do: listed in waiting_on_people, never dispatched
		}
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
				if t.BlockReason != "" {
					questions = append(questions, t)
				}
			}
		case domain.TaskStateCompleted:
			completed = append(completed, t)
		}
	}

	// In progress but silent: usually a worker that died with its session.
	// In progress but silent: usually a worker that died with its session.
	// Human tickets aren't in inProgress: people have days, see HumanStaleAfter.
	quiet := make([]map[string]interface{}, 0)
	if len(inProgress) > 0 {
		ids := make([]int, len(inProgress))
		for i, t := range inProgress {
			ids[i] = t.ID
		}
		last, err := s.repo.LastActivity(rctx, ids)
		if err != nil {
			return nil, err
		}
		for _, t := range inProgress {
			seen := last[t.ID]
			if silent := time.Since(seen); !seen.IsZero() && silent > QuietAfter {
				quiet = append(quiet, map[string]interface{}{
					"id": t.ID, "name": t.Name, "agent_id": t.AgentID, "silent_minutes": int(silent.Minutes()),
				})
			}
		}
	}

	switch optStr(args, "format", "compact") {
	case "full":
	case "compact":
		withDone, _ := args["include_completed"].(bool)
		return s.compactTasks(rctx, projectID, tasks, view.Mode, quiet, people, withDone)
	default:
		return nil, fmt.Errorf("format: use 'compact' (default) or 'full'")
	}

	return jsonResult(map[string]interface{}{
		"ready_to_dispatch": readyToDispatch, // dispatch ALL of these immediately
		"in_progress":       inProgress,
		"blocked":           blocked,   // each has blocked_by_ids showing what's in the way
		"scheduled":         scheduled, // tasks waiting for their start_at time
		"questions":         questions, // blocked with a question for a person (also listed in blocked); answer with amp_answer_question
		"completed":         completed,
		"quiet":             quiet,  // in progress with no activity for 15+ min: the worker likely died; check comments, reset to backlog, re-dispatch
		"waiting_on_people": people, // human tickets a person can do now, and how much agent work waits on each; never dispatch these
		"count":             len(tasks),
		"owner_filter":      view.Mode, // pass owner='unassigned' or 'any' to widen
	})
}

func (s *Server) getTask(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
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
	reply := actor.Await(ctx, replyCh)
	if reply.Err != nil {
		return nil, reply.Err
	}
	eff, err := s.effectiveMetadata(context.Background(), "task", taskID, reply.Task.Metadata)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"task": reply.Task, "effective_metadata": eff}
	limit := optInt(args, "limit", 30)
	for _, inc := range optStrSlice(args, "include") {
		switch inc {
		case "comments":
			c, err := s.repo.GetComments(ctx, taskID)
			if err != nil {
				return nil, err
			}
			out["comments"], out["comments_total"] = lastN(c, limit), len(c)
		case "history":
			h, err := s.repo.GetTicketHistory(ctx, taskID)
			if err != nil {
				return nil, err
			}
			out["history"], out["history_total"] = lastN(h, limit), len(h)
		case "runs":
			runs, err := s.repo.TaskRuns(ctx, taskID)
			if err != nil {
				return nil, err
			}
			var u domain.Usage
			for _, r := range runs {
				u.Add(r)
			}
			out["runs"], out["usage"] = runs, u
		default:
			return nil, fmt.Errorf("include: unknown %q (use comments, history, runs)", inc)
		}
	}
	return jsonResult(out)
}

// lastN keeps the most recent n of a chronological list (all when n <= 0).
func lastN[T any](xs []T, n int) []T {
	if n > 0 && len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

func (s *Server) updateTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	// Changing a ticket's text is planning: its creator, owner, and leads may.
	p := caller(rctx).p
	task, _, err := s.svc.LoadTask(rctx, p, taskID, authz.TaskPlan)
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
		Priority:    optStr(args, "priority", ""),

		AcceptanceCriteria: optStr(args, "acceptance_criteria", ""),
		Clear:              optStrSlice(args, "clear"),
		Performer:          optStr(args, "performer", ""),
		HumanReason:        optStr(args, "human_reason", ""),
	}
	if req.Metadata, err = optMetadataPatch(args); err != nil {
		return nil, err
	}
	// A person's ticket isn't switched to agent work off the web UI.
	if err := s.svc.UpdateTask(rctx, p, task, req); err != nil {
		return nil, err
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
		Clear:       optStrSlice(args, "clear"),
	}
	if req.Metadata, err = optMetadataPatch(args); err != nil {
		return nil, err
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(epic.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgUpdateEpic{Req: req, ReplyCh: replyCh})
	reply := actor.Await(rctx, replyCh)
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
		Clear:              optStrSlice(args, "clear"),
	}
	if req.Metadata, err = optMetadataPatch(args); err != nil {
		return nil, err
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(story.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgUpdateStory{Req: req, ReplyCh: replyCh})
	reply := actor.Await(rctx, replyCh)
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
	if task.IsHuman() {
		// Refused unless the caller is the person in the web UI (never, over
		// MCP): the person marks their own ticket done there.
		done := service.HumanDone{Note: optStr(args, "note", ""), PRURL: optStr(args, "pr_url", "")}
		if h, ok := args["hours_worked"].(float64); ok {
			done.HoursWorked = &h
		}
		_, run, err := s.svc.CompleteHumanTask(rctx, caller(rctx).p, taskID, done, true)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"task_id": taskID, "state": "completed", "human_work": true, "run": run})
	}
	// Only dispatched (in-progress), approved work: see Service.CompleteTask.
	if _, err := s.svc.CompleteTask(rctx, caller(rctx).p, taskID); err != nil {
		return nil, err
	}
	out := map[string]interface{}{"task_id": taskID, "state": "completed"}
	if run, err := s.recordRunFromArgs(rctx, taskID, args, domain.RunCompleted); err != nil {
		out["run_error"] = err.Error()
	} else if run != nil {
		out["run"] = run
	}
	return jsonResult(out)
}

func (s *Server) blockTask(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	if _, _, err := s.svc.LoadTask(rctx, caller(rctx).p, taskID, authz.TaskWork); err != nil {
		return nil, err
	}
	reason, err := reqStr(args, "reason")
	if err != nil || strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("reason is required: the question or problem a person needs to resolve")
	}
	if strings.HasPrefix(reason, "scheduled:") {
		return nil, fmt.Errorf("reason can't start with \"scheduled:\"; use amp_set_task_start_at to schedule a ticket")
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
	s.registry.System().Root.Send(pid, &actor.MsgBlockTask{TaskID: taskID, Reason: reason, By: caller(rctx).p.UserRef(), ReplyCh: replyCh})
	reply := actor.Await(rctx, replyCh)
	if reply.Err != nil {
		return nil, reply.Err
	}
	out := map[string]interface{}{"task_id": taskID, "state": "blocked"}
	if run, err := s.recordRunFromArgs(rctx, taskID, args, domain.RunBlocked); err != nil {
		out["run_error"] = err.Error()
	} else if run != nil {
		out["run"] = run
	}
	return jsonResult(out)
}

func (s *Server) answerQuestion(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	answer, err := reqStr(args, "answer")
	if err != nil || strings.TrimSpace(answer) == "" {
		return nil, fmt.Errorf("answer is required")
	}
	t, err := s.repo.GetTask(rctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task %d not found", taskID)
	}
	if err := s.checkProjectNotArchived(rctx, t.ProjectID); err != nil {
		return nil, err
	}
	task, err := s.svc.AnswerQuestion(rctx, caller(rctx).p, taskID, answer)
	if err != nil {
		return nil, err
	}
	next := "It's ready: dispatch it again and the worker will find the answer in the ticket's comments."
	if task.State == domain.TaskStateBlocked {
		next = "It still waits on unfinished dependencies; it becomes ready when they're done."
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "state": task.State, "next": next})
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
		Author:       s.svc.CommentAuthor(rctx, caller(rctx).p, optStr(args, "author", "agent")),
		AuthorUserID: caller(rctx).p.UserRef(),
	}
	replyCh := make(chan ReplyAddComment, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgAddComment{Req: req, ReplyCh: replyCh})
	reply := actor.Await(rctx, replyCh)
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "comment_added": true})
}

func (s *Server) getComments(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
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
	reply := actor.Await(ctx, replyCh)
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
	comments, err := s.repo.GetComments(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	runs, err := s.repo.TaskRuns(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{
		"task":     task,
		"history":  history,
		"count":    len(history),
		"comments": comments, // the worker's plan, progress, questions and answers
		"runs":     runs,     // model, tokens, branch, commit, PR per agent run
	})
}

func (s *Server) resetProject(rctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
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
	actor.Await(rctx, replyCh)
	s.svc.Record(rctx, caller(rctx).p, projectAudit("project.reset", projectID, "reset the project: deleted all its epics, stories, and tickets"))
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
	// Tickets that waited on this one are updated (and unblocked if nothing else holds them).
	dependents, err := s.svc.DeleteTask(rctx, caller(rctx).p, task)
	if err != nil {
		return nil, fmt.Errorf("delete task: %w", err)
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "deleted": true, "updated_dependents": dependents})
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
	// Deletes its stories and tickets; tickets elsewhere that waited on them are updated.
	if err := s.svc.DeleteEpic(rctx, caller(rctx).p, epic); err != nil {
		return nil, fmt.Errorf("delete epic: %w", err)
	}
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

	// Through the service, which keeps agents from moving work forward by
	// hand (see Service.SetTaskState), then the actor hierarchy.
	if _, err := s.svc.SetTaskState(rctx, caller(rctx).p, taskID, domain.TaskState(stateStr), reason); err != nil {
		return nil, err
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
	reply := actor.Await(rctx, replyCh)
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

	// 2. Every live KB doc, with annotations and original dates (Postgres
	// for a stored KB; none when the knowledge base isn't configured). A
	// failed read fails the export rather than leave docs out of it.
	kbDocs := []domain.ExportKBDoc{}
	if s.kb != nil {
		if kbDocs, err = s.kb.ExportDocs(ctx, kb.Project(projectID)); err != nil {
			return nil, fmt.Errorf("export kb docs: %w", err)
		}
	}

	// 3. Assemble the complete bundle with KB docs
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
	s.svc.Record(rctx, caller(rctx).p, projectAudit("project.import", newProject.ID, "imported project "+newProject.Name+" from an export"))

	// 2. Spawn the ProjectActor for the new project
	pid, err := s.registry.Get(newProject.ID)
	if err != nil {
		return nil, fmt.Errorf("spawn project actor: %w", err)
	}
	_ = pid // actor is now registered

	// 3. Recreate the KB docs, keeping their annotations and dates.
	imported, failed := 0, []string{}
	for _, kbDoc := range bundle.KBDocs {
		if s.kb == nil {
			slog.Default().Warn("knowledge base not configured; skipping the bundle's KB docs", "docs", len(bundle.KBDocs))
			break
		}
		if err := s.kb.ImportDoc(ctx, kb.Project(newProject.ID), kbDoc); err != nil {
			// Log and carry on: the project itself is already imported.
			slog.Default().Warn("failed to import kb doc", "path", kbDoc.Path, "err", err)
			failed = append(failed, kbDoc.Path)
			continue
		}
		imported++
	}

	// 4. Publish event for real-time UI updates
	s.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: newProject.ID, Payload: newProject, At: time.Now()})

	return jsonResult(map[string]interface{}{"project": newProject, "kb_docs_imported": imported, "kb_docs_failed": failed})
}

// ---- Metadata ----

// optMetadataPatch reads the "metadata" argument as a change to existing
// metadata (null removes a key); nil when it's absent.
// projectAudit is an audit entry about a whole project.
func projectAudit(action string, projectID int, summary string) domain.AuditEntry {
	return domain.AuditEntry{Action: action, ProjectID: &projectID, TargetType: "project", TargetID: strconv.Itoa(projectID), Summary: summary}
}

// clearDesc documents an update tool's clear parameter.
func clearDesc(fields string) string {
	return "Optional. Fields to blank (" + fields + "). An empty or omitted field is left unchanged, so name it here to remove its text."
}

func optMetadataPatch(args map[string]interface{}) (domain.MetadataPatch, error) {
	v, ok := args["metadata"]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("metadata must be an object of key/value pairs")
	}
	p := domain.MetadataPatch(m)
	if _, _, err := p.Split(); err != nil {
		return nil, err
	}
	return p, nil
}

// optNewMetadata reads the "metadata" argument for a new item.
func optNewMetadata(args map[string]interface{}) (domain.Metadata, error) {
	p, err := optMetadataPatch(args)
	if err != nil || p == nil {
		return nil, err
	}
	return domain.Metadata(p).Clean()
}

// effectiveMetadata is an item's metadata on top of what it inherits.
func (s *Server) effectiveMetadata(ctx context.Context, kind string, id int, own domain.Metadata) (domain.Metadata, error) {
	inherited, err := s.repo.InheritedMetadata(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	return inherited.Merge(own), nil
}

func (s *Server) findByMetadata(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	raw, _ := args["match"].(map[string]interface{})
	found, err := s.svc.FindByMetadata(ctx, caller(ctx).p, optStr(args, "kind", ""), raw, optInt(args, "project_id", 0), optInt(args, "limit", 50))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"results": found, "count": len(found)})
}

// ---- Daily reports (rules in internal/service/dayreports.go) ----

func (s *Server) dayReport(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	r, err := s.svc.DayReportDraft(ctx, caller(ctx).p, optStr(args, "date", ""), optStr(args, "tz", ""))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{
		"report": r,
		"next": "Review this with your user, write a short summary in their voice (highlights, blockers, what's next), " +
			"then save it with amp_save_day_report (same date and tz). Saving again replaces it.",
	})
}

func (s *Server) saveDayReport(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	summary, err := reqStr(args, "summary")
	if err != nil {
		return nil, err
	}
	r, err := s.svc.SaveDayReport(ctx, caller(ctx).p, optStr(args, "date", ""), optStr(args, "tz", ""), summary)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"report": r})
}

func (s *Server) listDayReports(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	reports, err := s.svc.DayReports(ctx, caller(ctx).p, optStr(args, "user", ""), optStr(args, "from", ""), optStr(args, "to", ""), optStr(args, "tz", ""))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"reports": reports, "count": len(reports)})
}

func (s *Server) humanImpact(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	days := optInt(args, "days", 30)
	if days < 1 || days > 366 {
		return nil, fmt.Errorf("days must be between 1 and 366")
	}
	until := time.Now()
	rep, err := s.svc.ProjectReport(ctx, caller(ctx).p, projectID, until.AddDate(0, 0, -days), until, "")
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{
		"since": rep.Since, "until": rep.Until, "work": rep.Work,
		"note": "wait = ready → started; cycle = started → done; hours_worked is what people reported. holding_up counts open tickets downstream of a human ticket now.",
	})
}

func (s *Server) velocity(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	days, err := s.svc.Velocity(ctx, caller(ctx).p, optStr(args, "user", ""), optStr(args, "from", ""), optStr(args, "to", ""), optStr(args, "tz", ""))
	if err != nil {
		return nil, err
	}
	total := 0
	for _, d := range days {
		total += d.Completed
	}
	return jsonResult(map[string]interface{}{"days": days, "total_completed": total})
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
	if use != kbRead && k.Kind == kb.ScopeGlobal {
		// Annotations would be seen by every org, so the guide takes none.
		return nil, k.RequireWrite()
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
	if strings.HasPrefix(strings.TrimPrefix(path, "/"), domain.DesignSystemPrefix) {
		return nil, fmt.Errorf("design-system/ docs are only written by publishing a design system version (amp_design_system_publish), " +
			"so they always come from phases a person approved; to change them, start a new version with amp_design_system_start")
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
	limit := kbSearchLimit(args)
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

// kbSearchDefaultLimit and kbSearchMaxLimit are amp_kb_search's documented
// contract ("default 3, max 10"). The REST search the UI uses has its own limit.
const (
	kbSearchDefaultLimit = 3
	kbSearchMaxLimit     = 10
)

// kbSearchLimit reads amp_kb_search's limit: the default when absent or not
// positive, clamped to the max otherwise.
func kbSearchLimit(args map[string]interface{}) int {
	limit := optInt(args, "limit", kbSearchDefaultLimit)
	if limit <= 0 {
		return kbSearchDefaultLimit
	}
	if limit > kbSearchMaxLimit {
		return kbSearchMaxLimit
	}
	return limit
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
	limit := optInt(args, "limit", kbListDefault)
	if limit <= 0 {
		limit = kbListDefault
	}
	limit = min(limit, kbListMax)
	offset := max(optInt(args, "offset", 0), 0)
	docs, total, err := s.kb.ListDocsPage(ctx, k.Scope, optStr(args, "tag", ""), limit, offset)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"kb": k, "docs": docs, "count": len(docs), "total": total}
	if next := offset + len(docs); len(docs) > 0 && next < total {
		out["next_offset"] = next
	}
	return jsonResult(out)
}

// amp_kb_list's page size: the default is the cap it had before KB reads
// moved to Postgres (#1989); a whole big KB in one tool result is too large
// for an agent's context.
const (
	kbListDefault = 250
	kbListMax     = 1000
)

func (s *Server) kbDelete(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbChange)
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimPrefix(path, "/"), domain.DesignSystemPrefix) {
		return nil, fmt.Errorf("design-system/ docs are only written by publishing a design system version (amp_design_system_publish), " +
			"so they always come from phases a person approved; to change them, start a new version with amp_design_system_start")
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	if err := s.kb.DeleteDoc(ctx, k.Scope, path, authorOf(ctx, nil)); err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"deleted": true, "path": path, "kb": k})
}

func (s *Server) kbListDeleted(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbChange)
	if err != nil {
		return nil, err
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	docs, err := s.kb.ListDeletedDocs(ctx, k.Scope)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"kb": k, "docs": docs, "count": len(docs)})
}

func (s *Server) kbRestore(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbChange)
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimPrefix(path, "/"), domain.DesignSystemPrefix) {
		return nil, fmt.Errorf("design-system/ docs are only written by publishing a design system version (amp_design_system_publish), " +
			"so they always come from phases a person approved; to change them, start a new version with amp_design_system_start")
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	doc, err := s.kb.RestoreDoc(ctx, k.Scope, path, authorOf(ctx, nil))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"restored": true, "kb": k, "doc": doc})
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
	// Single-flight per KB: a second call while one runs (here or on another
	// replica) starts nothing, since overlapping runs can lose docs.
	if err := s.kb.StartReindex(ctx, k.Scope); err != nil {
		if errors.Is(err, kb.ErrReindexRunning) {
			return jsonResult(map[string]interface{}{"reindex": "already running", "kb": k})
		}
		return nil, err
	}
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

func (s *Server) kbResolveAnnotation(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	k, err := s.kbOf(ctx, args, kbChange)
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	index, err := reqInt(args, "index")
	if err != nil {
		return nil, err
	}
	resolved := true
	if v, ok := args["resolved"].(bool); ok {
		resolved = v
	}
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	doc, err := s.kb.ResolveAnnotation(ctx, k.Scope, path, index, resolved, authorOf(ctx, map[string]interface{}{}))
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"path": doc.Path, "annotations": doc.Annotations})
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
	case "set_org":
		orgID, oerr := reqInt(args, "org_id")
		if oerr != nil {
			return nil, oerr
		}
		proj, removed, merr := s.svc.MoveProjectToOrg(rctx, p, projectID, orgID, optInt(args, "team_id", 0))
		if merr != nil {
			return nil, merr
		}
		teams, err = s.svc.ListProjectTeams(rctx, p, projectID)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{"project": proj, "teams": teams, "unlinked": removed})
	default:
		return nil, badAction(a, "list, link, unlink, set_org")
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

const patchMetadataDesc = "Optional. Metadata changes: keys with a value are set (string, number, or boolean), keys set to null are removed, other keys are kept."

func obj(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"description":          desc,
		"additionalProperties": map[string]interface{}{"type": []string{"string", "number", "boolean", "null"}},
	}
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
	obj := map[string]interface{}{
		"type":       "object",
		"properties": schema,
	}
	// Parameters documented as "REQUIRED." are required in the schema too:
	// models calling tools directly go by the schema, not the prose.
	// ("REQUIRED for …" / "REQUIRED when …" are conditional and stay optional.)
	var required []string
	for k, v := range schema {
		if m, ok := v.(map[string]interface{}); ok {
			if d, _ := m["description"].(string); strings.HasPrefix(d, "REQUIRED.") {
				required = append(required, k)
			}
		}
	}
	if len(required) > 0 {
		sort.Strings(required)
		obj["required"] = required
	}
	raw, _ := json.Marshal(obj)
	return presentTool(mcpgo.NewToolWithRawSchema(name, desc, raw))
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
	// Say what's missing in the caller's terms: most tools take a ticket.
	return 0, fmt.Errorf("missing which ticket or project this is for: pass task_id (or project_id, story_id or epic_id, as the tool's REQUIRED parameters say)")
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

// guideStart is the guide page amp_guide returns with no topic.
const guideStart = "start-here.md"

func (s *Server) guide(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	if err := s.requireKB(); err != nil {
		return nil, err
	}
	sc := kb.Global()
	docs, err := s.kb.ListDocs(ctx, sc, "")
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("the AMP guide isn't loaded yet (it syncs in the background after AMP starts); try again shortly")
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	pages := make([]map[string]interface{}, 0, len(docs))
	for _, d := range docs {
		pages = append(pages, map[string]interface{}{"path": d.Path, "title": d.Title, "tags": d.Tags})
	}
	page := func(path string) (map[string]interface{}, error) {
		doc, err := s.kb.GetDoc(ctx, sc, path)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"path": doc.Path, "title": doc.Title, "content": doc.Content, "pages": pages,
			"next": "Teach from this page in your own words and show it in your user's real projects. " +
				"For another page call amp_guide with its path.",
		}, nil
	}

	topic := strings.TrimSpace(optStr(args, "topic", ""))
	match := ""
	switch {
	case topic == "":
		match = guideStart
	default:
		for _, d := range docs {
			if d.Path == topic || d.Path == topic+".md" || strings.EqualFold(d.Title, topic) {
				match = d.Path
				break
			}
		}
	}
	if match != "" {
		out, err := page(match)
		if err != nil {
			return nil, err
		}
		return jsonResult(out)
	}

	hits, err := s.kb.Search(ctx, sc, topic, nil, 8, 0, 0)
	if err != nil {
		return nil, err
	}

	// Rank every page: first by how many of the topic's words its title and
	// tags name (so a page about the topic beats one that mentions it, and
	// start-here mentions everything), then by search order. Pages neither
	// named nor found are left out.
	pos := map[string]int{}
	excerpt := map[string]string{}
	for i, h := range hits {
		if _, ok := pos[h.Path]; !ok {
			pos[h.Path], excerpt[h.Path] = i, h.Excerpt
		}
	}
	words := guideWords(topic)
	type cand struct {
		doc   kb.DocSummary
		score int
		pos   int
	}
	var ranked []cand
	for _, d := range docs {
		c := cand{doc: d, score: guideMatch(words, d.Title, d.Tags), pos: len(hits)}
		if p, ok := pos[d.Path]; ok {
			c.pos = p
		}
		if c.score > 0 || c.pos < len(hits) {
			ranked = append(ranked, c)
		}
	}
	if len(ranked) == 0 {
		return jsonResult(map[string]interface{}{
			"found": false, "pages": pages,
			"next": "No guide page matches; pick one from pages, or tell your user the guide doesn't cover it rather than guessing how AMP works.",
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].pos < ranked[j].pos
	})
	out, err := page(ranked[0].doc.Path)
	if err != nil {
		return nil, err
	}
	also := []map[string]string{}
	for _, c := range ranked[1:] {
		if len(also) == 3 {
			break
		}
		also = append(also, map[string]string{"path": c.doc.Path, "title": c.doc.Title, "excerpt": excerpt[c.doc.Path]})
	}
	out["also"] = also
	return jsonResult(out)
}

// guideWords are a topic's content words, lowercased.
func guideWords(topic string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(topic), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) >= 4 && !guideStopWords[w] {
			out = append(out, w)
		}
	}
	return out
}

var guideStopWords = map[string]bool{"what": true, "when": true, "with": true, "does": true, "have": true,
	"that": true, "this": true, "from": true, "into": true, "about": true, "their": true, "there": true,
	"should": true, "would": true, "could": true, "seems": true, "someone": true, "something": true}

// guideMatch counts topic words naming the page in its title or tags,
// matching on a shared stem (the first five letters: "capitalize" and
// "capitalization", "hand" and "handover").
func guideMatch(words []string, title string, tags []string) int {
	names := guideWords(title + " " + strings.Join(tags, " "))
	n := 0
	for _, w := range words {
		for _, name := range names {
			if strings.HasPrefix(name, stem(w)) || strings.HasPrefix(w, stem(name)) {
				n++
				break
			}
		}
	}
	return n
}

func stem(w string) string {
	if len(w) > 5 {
		return w[:5]
	}
	return w
}

// getPlan is a project's plan as one tree: epics → stories, each with its
// ticket counts by state. It replaces walking list_epics / list_stories.
func (s *Server) getPlan(ctx context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	onlyEpic := optInt(args, "epic_id", 0)
	epics, err := s.repo.ListEpics(ctx, projectID)
	if err != nil {
		return nil, err
	}
	stories, err := s.repo.ListStoriesByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tasks, err := s.repo.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, err
	}
	counts := map[int]map[domain.TaskState]int{} // story → state → n
	epicCounts := map[int]map[domain.TaskState]int{}
	bump := func(m map[int]map[domain.TaskState]int, id int, st domain.TaskState) {
		if m[id] == nil {
			m[id] = map[domain.TaskState]int{}
		}
		m[id][st]++
	}
	for _, t := range tasks {
		bump(counts, t.StoryID, t.State)
		bump(epicCounts, t.EpicID, t.State)
	}
	type storyNode struct {
		ID       int                      `json:"id"`
		Name     string                   `json:"name"`
		State    domain.StoryState        `json:"state"`
		OwnerIDs []int                    `json:"owner_ids,omitempty"`
		Tickets  map[domain.TaskState]int `json:"tickets"`
	}
	type epicNode struct {
		ID          int                      `json:"id"`
		Name        string                   `json:"name"`
		Description string                   `json:"description,omitempty"`
		State       domain.EpicState         `json:"state"`
		Priority    string                   `json:"priority,omitempty"`
		OwnerIDs    []int                    `json:"owner_ids,omitempty"`
		Metadata    domain.Metadata          `json:"metadata,omitempty"`
		Tickets     map[domain.TaskState]int `json:"tickets"`
		Stories     []storyNode              `json:"stories"`
	}
	byEpic := map[int][]storyNode{}
	for _, st := range stories {
		byEpic[st.EpicID] = append(byEpic[st.EpicID], storyNode{st.ID, st.Name, st.State, st.OwnerIDs, orEmpty(counts[st.ID])})
	}
	out := []epicNode{}
	for _, e := range epics {
		if onlyEpic != 0 && e.ID != onlyEpic {
			continue
		}
		desc := e.Description
		if r := []rune(desc); onlyEpic == 0 && len(r) > 200 {
			desc = string(r[:200]) + "…"
		}
		out = append(out, epicNode{e.ID, e.Name, desc, e.State, e.Priority, e.OwnerIDs, e.Metadata, orEmpty(epicCounts[e.ID]), append([]storyNode{}, byEpic[e.ID]...)})
	}
	if onlyEpic != 0 && len(out) == 0 {
		return nil, fmt.Errorf("epic %d isn't in project %d", onlyEpic, projectID)
	}
	return jsonResult(map[string]interface{}{"project_id": projectID, "epics": out, "tickets": len(tasks)})
}

func orEmpty(m map[domain.TaskState]int) map[domain.TaskState]int {
	if m == nil {
		return map[domain.TaskState]int{}
	}
	return m
}
