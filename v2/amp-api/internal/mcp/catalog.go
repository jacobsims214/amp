package mcp

import (
	"context"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// The tool catalog: how each tool presents itself and who sees it listed.
//
// Clients find tools by keyword (opencode's Code Mode search scores words in
// the name, then the description), and models pick tools by title and first
// line, so every tool says plainly what it's for, in the words agents and
// people use ("ticket" as well as "task"). Superseded tools stay callable as
// hidden aliases, so prompts and clients that name them keep working, but
// aren't listed. Admin-only and person-only tools are listed only for those
// who can use them.

type listing int

const (
	listed      listing = iota
	hiddenAlias         // callable, not listed: superseded by another tool
	adminOnly           // listed for org owners/admins and global admins
	personOnly          // not listed for agent accounts (they can't use them)
)

type toolInfo struct {
	title       string
	keywords    string // appended to the description as "Use for: …"
	readOnly    bool
	destructive bool
	idempotent  bool
	listing     listing
}

var toolCatalog = map[string]toolInfo{
	// Who and where
	"amp_whoami":     {title: "Who am I", keywords: "my identity, my orgs and teams, check the AMP connection", readOnly: true},
	"amp_guide":      {title: "AMP guide", keywords: "how to use AMP, onboarding, help, docs about AMP itself", readOnly: true},
	"amp_list_orgs":  {title: "List orgs", keywords: "my organizations and my role", readOnly: true},
	"amp_create_org": {title: "Create org", listing: hiddenAlias},
	"amp_audit_log":  {title: "Audit log", keywords: "who changed access, roles, tokens", readOnly: true, listing: adminOnly},

	// Projects
	"amp_list_projects":           {title: "List projects", keywords: "my projects", readOnly: true},
	"amp_get_project":             {title: "Get project", keywords: "project details, settings, metadata", readOnly: true},
	"amp_create_project":          {title: "Create project", keywords: "new project"},
	"amp_update_project_settings": {title: "Update project settings", keywords: "rename project, require plan approval, project metadata", idempotent: true},
	"amp_archive_project":         {title: "Archive project", listing: hiddenAlias},
	"amp_restore_project":         {title: "Restore project", listing: hiddenAlias},
	"amp_reset_project":           {title: "Reset project", destructive: true, listing: hiddenAlias},
	"amp_export_project":          {title: "Export project", readOnly: true, listing: hiddenAlias},
	"amp_import_project":          {title: "Import project", listing: hiddenAlias},
	"amp_list_project_members":    {title: "List project members", keywords: "people on the project, who can own a ticket", readOnly: true},
	"amp_manage_project_teams":    {title: "Manage project teams", keywords: "link a team to a project, move a project to another org"},

	// The plan
	"amp_get_plan":              {title: "Get plan", keywords: "see the plan, list epics and stories with ticket counts, find an epic or story id", readOnly: true},
	"amp_create_epic":           {title: "Create epic", keywords: "new epic, group work"},
	"amp_create_story":          {title: "Create story", keywords: "new story in an epic"},
	"amp_update_epic":           {title: "Update epic", keywords: "rename or edit an epic, epic owners", idempotent: true},
	"amp_update_story":          {title: "Update story", keywords: "rename or edit a story, story owners", idempotent: true},
	"amp_move_story":            {title: "Move story", keywords: "move a story to another epic", idempotent: true},
	"amp_delete_epic":           {title: "Delete epic", keywords: "remove an epic (to the trash)", destructive: true},
	"amp_delete_story":          {title: "Delete story", keywords: "remove a story (to the trash)", destructive: true},
	"amp_list_epics":            {title: "List epics", keywords: "a project's epics; amp_get_plan gives the whole tree with stories and ticket counts", readOnly: true},
	"amp_list_stories":          {title: "List stories", readOnly: true, listing: hiddenAlias},
	"amp_list_project_stories":  {title: "List project stories", readOnly: true, listing: hiddenAlias},
	"amp_get_epic":              {title: "Get epic", readOnly: true, listing: hiddenAlias},
	"amp_get_story":             {title: "Get story", readOnly: true, listing: hiddenAlias},
	"amp_check_plan":            {title: "Check plan", keywords: "validate the plan before dispatching, find unconnected tickets", readOnly: true},
	"amp_approve_plan":          {title: "Approve plan", keywords: "record that the user approved the plan", listing: personOnly},
	"amp_find_by_metadata":      {title: "Find by metadata", keywords: "find projects, epics, stories or tickets by metadata (jira key, cost center)", readOnly: true},
	"amp_set_task_dependencies": {title: "Set ticket dependencies", keywords: "add or remove a dependency, what a ticket waits on, fix ticket order", idempotent: true},

	// Tickets
	"amp_create_tasks":       {title: "Create tickets", keywords: "create one or many tickets, plan a story's tickets with dependencies, new ticket"},
	"amp_create_task":        {title: "Create ticket", listing: hiddenAlias},
	"amp_list_tasks":         {title: "List tickets", keywords: "my tickets, what should I work on, the board, ready to dispatch, questions, blocked, in progress", readOnly: true},
	"amp_get_task":           {title: "Get ticket", keywords: "read a ticket with its comments, history and runs", readOnly: true},
	"amp_get_task_comments":  {title: "Get ticket comments", readOnly: true, listing: hiddenAlias},
	"amp_get_ticket_history": {title: "Get ticket history", readOnly: true, listing: hiddenAlias},
	"amp_get_task_runs":      {title: "Get ticket runs", readOnly: true, listing: hiddenAlias},
	"amp_update_task":        {title: "Update ticket", keywords: "edit a ticket's text, acceptance criteria, priority, agent", idempotent: true},
	"amp_move_task":          {title: "Move ticket", keywords: "move a ticket to another story", idempotent: true},
	"amp_delete_task":        {title: "Delete ticket", keywords: "remove a ticket (to the trash)", destructive: true},
	"amp_assign_task":        {title: "Assign ticket", keywords: "assign, reassign or unassign a ticket, take one over (user='me')", idempotent: true},
	"amp_claim_task":         {title: "Claim ticket", keywords: "take an unassigned ticket (never someone else's)", idempotent: true},
	"amp_handover_tasks":     {title: "Hand over tickets", keywords: "move someone's open tickets to another person"},
	"amp_dispatch_task":      {title: "Dispatch ticket", keywords: "start work on a ticket, hand a ticket to a worker agent, begin a ticket"},
	"amp_add_task_comment":   {title: "Comment on ticket", keywords: "comment on a ticket, post a progress update, log a note or finding"},
	"amp_block_task":         {title: "Block ticket with a question", keywords: "ask the user a question, stop for a decision, flag a blocker, request review"},
	"amp_answer_question":    {title: "Answer ticket question", keywords: "record the user's answer to a blocked ticket, unblock"},
	"amp_complete_task":      {title: "Complete ticket", keywords: "finish, close or mark a ticket done, with the run's tokens and commit"},
	"amp_report_run":         {title: "Report run", keywords: "record a run's model, token usage, cost, branch, PR, merge, or an abandoned run"},
	"amp_set_task_state":     {title: "Set ticket state", keywords: "escape hatch: reopen or force a ticket's state", idempotent: true},
	"amp_set_task_start_at":  {title: "Schedule ticket", keywords: "schedule a ticket to start later", idempotent: true},
	"amp_list_unmerged":      {title: "List unmerged work", keywords: "done tickets whose PR or branch isn't merged", readOnly: true},
	"amp_trace":              {title: "Trace code to tickets", keywords: "which ticket made this commit, branch or PR", readOnly: true},
	"amp_report_merged":      {title: "Report merged", keywords: "CI merge report, mark a branch, PR or commit merged, record the merge commit"},
	"amp_list_deleted_tasks": {title: "List trash", keywords: "deleted tickets, epics and stories", readOnly: true},
	"amp_restore_tasks":      {title: "Restore from trash", keywords: "undelete tickets, epics or stories"},

	// Reports
	"amp_day_report":          {title: "Draft day report", keywords: "end of day, wrap up today, standup draft", readOnly: true},
	"amp_save_day_report":     {title: "Save day report", keywords: "save the end-of-day report", idempotent: true},
	"amp_list_day_reports":    {title: "List day reports", keywords: "read saved day reports, status update", readOnly: true},
	"amp_project_day_reports": {title: "Project standup", keywords: "standup sheet for a project, who did what today", readOnly: true},
	"amp_velocity":            {title: "Velocity", keywords: "tickets completed per day", readOnly: true},
	"amp_human_impact":        {title: "Human impact", keywords: "people vs agents, cycle time, what people did, bottlenecks waiting on people", readOnly: true},

	// Knowledge base
	"amp_kb_search":             {title: "Search knowledge base", keywords: "find docs, search the KB, prior work, notes", readOnly: true},
	"amp_kb_get":                {title: "Read KB doc", keywords: "read a knowledge base document by path", readOnly: true},
	"amp_kb_list":               {title: "List KB docs", keywords: "list knowledge base documents", readOnly: true},
	"amp_kb_write":              {title: "Write KB doc", keywords: "create or update a knowledge base document, save a finding", idempotent: true},
	"amp_kb_delete":             {title: "Delete KB doc", destructive: true},
	"amp_kb_annotate":           {title: "Annotate KB doc", keywords: "add a correction or note to a doc"},
	"amp_kb_resolve_annotation": {title: "Resolve KB annotation", idempotent: true},
	"amp_kb_status":             {title: "KB health", keywords: "stale docs, annotations, tag counts", readOnly: true},
	"amp_kb_sources":            {title: "List KBs for a project", keywords: "which knowledge bases: project, team, org", readOnly: true},
	"amp_kb_tags":               {title: "KB tags", readOnly: true, listing: hiddenAlias},
	"amp_kb_reindex":            {title: "Reindex KB", keywords: "rebuild or re-embed a KB, search scores look huge or wrong, results look keyword-only, a doc is missing from search", idempotent: true},

	// Artifacts and design
	"amp_publish_artifact":      {title: "Publish artifact", keywords: "publish or upload a mockup, prototype, diagram, image, PDF or document for review"},
	"amp_get_artifact":          {title: "Get artifact", keywords: "read a published artifact and its versions", readOnly: true},
	"amp_list_artifacts":        {title: "List artifacts", keywords: "a project's or ticket's published artifacts", readOnly: true},
	"amp_review_artifact":       {title: "Review artifact", keywords: "approve a mockup or design, request changes, the user's verdict", listing: personOnly},
	"amp_keep_artifact":         {title: "Keep artifact", keywords: "keep a mockup so it never expires", idempotent: true, listing: personOnly},
	"amp_design_context":        {title: "Design context", keywords: "design system layers before any UI work, tokens, components", readOnly: true},
	"amp_design_system_start":   {title: "Start design system version", keywords: "build or update an org or project design system"},
	"amp_design_system_publish": {title: "Publish design system", keywords: "publish the approved design system docs"},

	// Admin
	"amp_manage_invites":      {title: "Manage invites", listing: adminOnly},
	"amp_manage_org_members":  {title: "Manage org members", listing: adminOnly},
	"amp_manage_teams":        {title: "Manage teams", listing: adminOnly},
	"amp_manage_team_members": {title: "Manage team members", keywords: "add or remove people on a team (team leads too)"},
}

// presentTool applies the catalog to a tool as it's registered: title,
// annotations, and the "Use for" keywords on the description.
func presentTool(t mcpgo.Tool) mcpgo.Tool {
	info, ok := toolCatalog[t.Name]
	if !ok {
		return t
	}
	t.Title = info.title
	if info.keywords != "" && !strings.Contains(t.Description, "Use for:") {
		t.Description = strings.TrimRight(t.Description, " ") + " Use for: " + info.keywords + "."
	}
	no := false
	ro, de, id := info.readOnly, info.destructive, info.idempotent || info.readOnly
	t.Annotations = mcpgo.ToolAnnotation{Title: info.title, ReadOnlyHint: &ro, DestructiveHint: &de, IdempotentHint: &id, OpenWorldHint: &no}
	return t
}

// listFilter decides which tools a caller sees in tools/list. Hidden aliases
// stay callable; they just aren't advertised.
func (s *Server) listFilter(ctx context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
	p, err := authz.FromContext(ctx)
	admin := p.Super
	if err == nil && !admin && p.HasUser() {
		if orgs, err := s.repo.ListOrgs(ctx, p.UserRef()); err == nil {
			for _, o := range orgs {
				if o.MyRole == domain.OrgRoleOwner || o.MyRole == domain.OrgRoleAdmin {
					admin = true
					break
				}
			}
		}
	}
	out := tools[:0:0]
	for _, t := range tools {
		switch toolCatalog[t.Name].listing {
		case hiddenAlias:
			continue
		case adminOnly:
			if !admin {
				continue
			}
		case personOnly:
			if err == nil && p.Agent {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// ServerOptions are the MCP server options AMP's tools need: the deadline
// middleware and the per-caller tool list. Production and tests both use them.
//
// The list is trimmed in an after-list hook rather than mcp-go's ToolFilter,
// which also refuses calls to filtered tools: hidden aliases must stay
// callable, and admin and person-only tools already check who's calling.
func (s *Server) ServerOptions() []server.ServerOption {
	hooks := &server.Hooks{}
	hooks.AddAfterListTools(func(ctx context.Context, _ any, _ *mcpgo.ListToolsRequest, res *mcpgo.ListToolsResult) {
		res.Tools = s.listFilter(ctx, res.Tools)
	})
	return []server.ServerOption{
		server.WithToolHandlerMiddleware(Middleware()),
		server.WithHooks(hooks),
	}
}

// humanReasonList renders the human reasons for tool descriptions.
func humanReasonList() string {
	parts := make([]string, 0, len(domain.HumanReasons))
	for _, r := range domain.HumanReasons {
		parts = append(parts, "'"+r+"' ("+domain.HumanReasonText[r]+")")
	}
	return "One of " + strings.Join(parts, ", ")
}
