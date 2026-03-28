package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/amp/mcp-go/internal/application/usecases"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Handler handles MCP protocol requests
type Handler struct {
	projectUC   *usecases.ProjectUseCase
	epicUC      *usecases.EpicUseCase
	storyUC     *usecases.StoryUseCase
	taskUC      *usecases.TaskUseCase
	kbUC        *usecases.KBUseCase
	dashboardUC *usecases.DashboardUseCase
	odooClient  interface{}
}

// NewHandler creates a new MCP handler
func NewHandler(
	projectUC *usecases.ProjectUseCase,
	epicUC *usecases.EpicUseCase,
	storyUC *usecases.StoryUseCase,
	taskUC *usecases.TaskUseCase,
	kbUC *usecases.KBUseCase,
	dashboardUC *usecases.DashboardUseCase,
	odooClient interface{},
) *Handler {
	return &Handler{
		projectUC:   projectUC,
		epicUC:      epicUC,
		storyUC:     storyUC,
		taskUC:      taskUC,
		kbUC:        kbUC,
		dashboardUC: dashboardUC,
		odooClient:  odooClient,
	}
}

// prop is a shorthand for building a JSON Schema property descriptor.
func prop(typ, description string) map[string]interface{} {
	return map[string]interface{}{"type": typ, "description": description}
}

// RegisterTools registers all MCP tools with the server
func (h *Handler) RegisterTools(s *server.MCPServer) {
	ctx := context.Background()

	// ---- Project tools ----
	s.AddTool(mcp.NewTool("amp_create_project",
		"Create a new AMP project in Odoo. Returns the created project object with its id.",
		map[string]interface{}{
			"name":        prop("string", "REQUIRED. Project name."),
			"code":        prop("string", "REQUIRED. Short unique project code, e.g. 'amp-platform'."),
			"description": prop("string", "Optional project description."),
		}), h.makeHandler(ctx, h.handleCreateProject))

	s.AddTool(mcp.NewTool("amp_list_projects",
		"List all active AMP projects. Returns array of projects under key 'projects'.",
		map[string]interface{}{
			"limit":  prop("number", "Max results to return. Default 50."),
			"offset": prop("number", "Pagination offset. Default 0."),
		}), h.makeHandler(ctx, h.handleListProjects))

	s.AddTool(mcp.NewTool("amp_get_project",
		"Get full project details by its numeric Odoo ID. Returns project under key 'project'.",
		map[string]interface{}{
			"project_id": prop("number", "REQUIRED. Numeric Odoo project ID."),
		}), h.makeHandler(ctx, h.handleGetProject))

	s.AddTool(mcp.NewTool("amp_get_project_by_code",
		"Find a project by its short code string (e.g. 'amp-platform'). Returns project under key 'project'.",
		map[string]interface{}{
			"code": prop("string", "REQUIRED. Project code string."),
		}), h.makeHandler(ctx, h.handleGetProjectByCode))

	s.AddTool(mcp.NewTool("amp_update_project",
		"Update fields on an existing project. Only provided fields are changed.",
		map[string]interface{}{
			"project_id":  prop("number", "REQUIRED. Numeric Odoo project ID."),
			"name":        prop("string", "New project name."),
			"description": prop("string", "New project description."),
			"state":       prop("string", "New state: 'draft', 'active', or 'done'."),
		}), h.makeHandler(ctx, h.handleUpdateProject))

	// ---- Epic tools ----
	s.AddTool(mcp.NewTool("amp_create_epic",
		"Create a new epic under a project. Returns the created epic object with its id.",
		map[string]interface{}{
			"name":        prop("string", "REQUIRED. Epic name."),
			"project_id":  prop("number", "REQUIRED. Numeric Odoo project ID."),
			"description": prop("string", "Optional epic description."),
			"priority":    prop("string", "Priority: '0'=low, '1'=normal (default), '2'=high, '3'=urgent."),
		}), h.makeHandler(ctx, h.handleCreateEpic))

	s.AddTool(mcp.NewTool("amp_get_epic",
		"Get epic details by its numeric ID. Returns epic under key 'epic'.",
		map[string]interface{}{
			"epic_id": prop("number", "REQUIRED. Numeric Odoo epic ID."),
		}), h.makeHandler(ctx, h.handleGetEpic))

	s.AddTool(mcp.NewTool("amp_list_epics",
		"List all epics for a project. Returns array under key 'epics'.",
		map[string]interface{}{
			"project_id": prop("number", "REQUIRED. Numeric Odoo project ID."),
		}), h.makeHandler(ctx, h.handleListEpics))

	s.AddTool(mcp.NewTool("amp_update_epic_dag",
		"Store a DAG (directed acyclic graph) JSON string on an epic to track story/task dependencies.",
		map[string]interface{}{
			"epic_id":  prop("number", "REQUIRED. Numeric Odoo epic ID."),
			"dag_json": prop("string", "REQUIRED. JSON string representing the DAG structure."),
		}), h.makeHandler(ctx, h.handleUpdateEpicDAG))

	// ---- Story tools ----
	s.AddTool(mcp.NewTool("amp_create_story",
		"Create a new story under an epic. Returns the created story object with its id.",
		map[string]interface{}{
			"name":                prop("string", "REQUIRED. Story name."),
			"project_id":          prop("number", "REQUIRED. Numeric Odoo project ID."),
			"epic_id":             prop("number", "REQUIRED. Numeric Odoo epic ID."),
			"description":         prop("string", "Optional story description."),
			"acceptance_criteria": prop("string", "Optional acceptance criteria."),
			"priority":            prop("string", "Priority: '0'=low, '1'=normal (default), '2'=high, '3'=urgent."),
			"planned_hours":       prop("number", "Estimated hours. Default 0."),
		}), h.makeHandler(ctx, h.handleCreateStory))

	s.AddTool(mcp.NewTool("amp_get_story",
		"Get story details by its numeric ID. Returns story under key 'story'.",
		map[string]interface{}{
			"story_id": prop("number", "REQUIRED. Numeric Odoo story ID."),
		}), h.makeHandler(ctx, h.handleGetStory))

	s.AddTool(mcp.NewTool("amp_list_stories",
		"List all stories for an epic. Returns array under key 'stories'.",
		map[string]interface{}{
			"epic_id": prop("number", "REQUIRED. Numeric Odoo epic ID."),
		}), h.makeHandler(ctx, h.handleListStories))

	// ---- Task tools ----
	s.AddTool(mcp.NewTool("amp_create_task",
		"Create a new task. Returns the created task object with its id.",
		map[string]interface{}{
			"name":                prop("string", "REQUIRED. Task name."),
			"project_id":          prop("number", "REQUIRED. Numeric Odoo project ID."),
			"epic_id":             prop("number", "Optional. Numeric Odoo epic ID."),
			"story_id":            prop("number", "Optional. Numeric Odoo story ID."),
			"description":         prop("string", "Optional task description (HTML supported)."),
			"acceptance_criteria": prop("string", "Optional acceptance criteria."),
			"priority":            prop("string", "Priority: '0'=low, '1'=normal (default), '2'=high, '3'=urgent."),
			"planned_hours":       prop("number", "Estimated hours. Default 0."),
			"dag_level":           prop("number", "DAG execution level (0=first). Default 0."),
			"dependency_ids":      map[string]interface{}{"type": "array", "description": "Optional array of numeric task IDs this task depends on.", "items": map[string]interface{}{"type": "number"}},
		}), h.makeHandler(ctx, h.handleCreateTask))

	s.AddTool(mcp.NewTool("amp_list_tasks",
		"List tasks filtered by project, epic, or story and optionally by state. Returns array under key 'tasks'. Use this to see all tasks in a story/epic and their current states before deciding what to dispatch.",
		map[string]interface{}{
			"project_id": prop("number", "Filter by project ID. Use alone to see all project tasks."),
			"epic_id":    prop("number", "Filter by epic ID."),
			"story_id":   prop("number", "Filter by story ID."),
			"state":      prop("string", "Optional state filter: 'backlog', 'ready', 'in_progress', 'review', 'completed', 'blocked'. Omit to see all states."),
		}), h.makeHandler(ctx, h.handleListTasks))

	s.AddTool(mcp.NewTool("amp_get_task",
		"Get full task details by its numeric ID. Returns task under key 'task'.",
		map[string]interface{}{
			"task_id": prop("number", "REQUIRED. Numeric Odoo task ID."),
		}), h.makeHandler(ctx, h.handleGetTask))

	s.AddTool(mcp.NewTool("amp_update_task",
		"Update fields on an existing task. Only provided fields are changed.",
		map[string]interface{}{
			"task_id":      prop("number", "REQUIRED. Numeric Odoo task ID."),
			"name":         prop("string", "New task name."),
			"description":  prop("string", "New task description."),
			"state":        prop("string", "New state: 'backlog', 'ready', 'in_progress', 'done', 'blocked'."),
			"agent_id":     prop("string", "Agent identifier string."),
			"context_data": prop("string", "JSON string of agent context/progress notes."),
		}), h.makeHandler(ctx, h.handleUpdateTask))

	s.AddTool(mcp.NewTool("amp_dispatch_task",
		"Dispatch a task to a named agent, setting its state to 'in_progress'.",
		map[string]interface{}{
			"task_id":  prop("number", "REQUIRED. Numeric Odoo task ID."),
			"agent_id": prop("string", "REQUIRED. Agent identifier string (e.g. 'amp-worker')."),
		}), h.makeHandler(ctx, h.handleDispatchTask))

	s.AddTool(mcp.NewTool("amp_complete_task",
		"Mark a task as completed, setting state to 'done'.",
		map[string]interface{}{
			"task_id": prop("number", "REQUIRED. Numeric Odoo task ID."),
		}), h.makeHandler(ctx, h.handleCompleteTask))

	s.AddTool(mcp.NewTool("amp_block_task",
		"Block a task with a reason, setting state to 'blocked'.",
		map[string]interface{}{
			"task_id": prop("number", "REQUIRED. Numeric Odoo task ID."),
			"reason":  prop("string", "REQUIRED. Human-readable reason for blocking."),
		}), h.makeHandler(ctx, h.handleBlockTask))

	s.AddTool(mcp.NewTool("amp_list_ready_tasks",
		"Get all tasks in 'ready' state with all dependencies complete. Returns array under key 'ready_tasks'.",
		map[string]interface{}{
			"project_id": prop("number", "REQUIRED. Numeric Odoo project ID."),
		}), h.makeHandler(ctx, h.handleListReadyTasks))

	s.AddTool(mcp.NewTool("amp_add_task_comment",
		"Post a comment/note to a task's chatter log.",
		map[string]interface{}{
			"task_id": prop("number", "REQUIRED. Numeric Odoo task ID."),
			"body":    prop("string", "REQUIRED. Comment text (HTML supported)."),
		}), h.makeHandler(ctx, h.handleAddTaskComment))

	// ---- Knowledge Base tools ----
	s.AddTool(mcp.NewTool("amp_create_kb_entry",
		"Create a knowledge base entry linked to a project and optionally an epic/story/task.",
		map[string]interface{}{
			"title":            prop("string", "REQUIRED. KB entry title."),
			"content":          prop("string", "REQUIRED. KB entry content (HTML supported)."),
			"project_id":       prop("number", "REQUIRED. Numeric Odoo project ID."),
			"entry_type":       prop("string", "Type: 'context', 'decision', 'bug', 'solution', 'reference'. Default 'context'."),
			"epic_id":          prop("number", "Optional. Numeric Odoo epic ID to link to."),
			"story_id":         prop("number", "Optional. Numeric Odoo story ID to link to."),
			"task_id":          prop("number", "Optional. Numeric Odoo task ID to link to."),
			"tags":             map[string]interface{}{"type": "array", "description": "Optional array of tag strings.", "items": map[string]interface{}{"type": "string"}},
			"created_by_agent": prop("string", "Optional. Agent identifier that created this entry."),
		}), h.makeHandler(ctx, h.handleCreateKBEntry))

	s.AddTool(mcp.NewTool("amp_search_kb",
		"Full-text search across KB entry titles, content, and tags. Returns array under key 'entries'.",
		map[string]interface{}{
			"query":      prop("string", "REQUIRED. Search query string."),
			"project_id": prop("number", "Optional. Limit to a specific project ID."),
			"entry_type": prop("string", "Optional. Filter by type: 'context', 'decision', 'bug', 'solution', 'reference'."),
			"limit":      prop("number", "Max results. Default 20."),
		}), h.makeHandler(ctx, h.handleSearchKB))

	s.AddTool(mcp.NewTool("amp_get_kb_entry",
		"Get a single KB entry by its numeric ID. Returns entry under key 'entry'.",
		map[string]interface{}{
			"entry_id": prop("number", "REQUIRED. Numeric Odoo KB entry ID."),
		}), h.makeHandler(ctx, h.handleGetKBEntry))

	s.AddTool(mcp.NewTool("amp_get_project_kb",
		"Get all KB entries for a project, newest first. Returns array under key 'entries'.",
		map[string]interface{}{
			"project_id": prop("number", "REQUIRED. Numeric Odoo project ID."),
			"limit":      prop("number", "Max results. Default 20."),
		}), h.makeHandler(ctx, h.handleGetProjectKB))

	s.AddTool(mcp.NewTool("amp_get_task_kb",
		"Get KB entries related to a task (matched via task, story, epic, and project). Returns array under key 'entries'.",
		map[string]interface{}{
			"task_id": prop("number", "REQUIRED. Numeric Odoo task ID."),
		}), h.makeHandler(ctx, h.handleGetTaskKB))

	// ---- Dashboard tools ----
	s.AddTool(mcp.NewTool("amp_get_dashboard",
		"Get a real-time project dashboard: counts of epics, stories, tasks by state, active agents, progress percentage.",
		map[string]interface{}{
			"project_id": prop("number", "REQUIRED. Numeric Odoo project ID."),
		}), h.makeHandler(ctx, h.handleGetDashboard))

	s.AddTool(mcp.NewTool("amp_validate_context",
		"Validate a .amp.json context object, confirming the project_id exists in Odoo.",
		map[string]interface{}{
			"context": map[string]interface{}{
				"type":        "object",
				"description": "REQUIRED. The parsed .amp.json context object with fields: project_id (number), project_code (string), project_name (string), version (string).",
			},
		}), h.makeHandler(ctx, h.handleValidateContext))

	s.AddTool(mcp.NewTool("amp_health_check",
		"Check MCP server health and Odoo connection status. No arguments required.",
		map[string]interface{}{}), h.makeHandler(ctx, h.handleHealthCheck))
}

// makeHandler wraps our context-based handlers to match MCP's ToolHandlerFunc signature
func (h *Handler) makeHandler(baseCtx context.Context, handler func(context.Context, map[string]interface{}) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(args map[string]interface{}) (*mcp.CallToolResult, error) {
		return handler(baseCtx, args)
	}
}

// ---- Safe argument helpers ----

// argInt extracts an integer from args[key]. JSON numbers arrive as float64;
// also handles int, int64, and string-encoded numbers (e.g. "2") for robustness.
func argInt(args map[string]interface{}, key string) (int, error) {
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
	case string:
		// Some MCP clients send numbers as strings; parse defensively.
		var i int
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i, nil
		}
	}
	return 0, fmt.Errorf("argument %q must be a number, got %T(%v)", key, v, v)
}

// argString extracts a string from args[key]. Returns ("", error) if missing/wrong type.
func argString(args map[string]interface{}, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", fmt.Errorf("missing required argument: %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T", key, v)
	}
	return s, nil
}

// optInt extracts an optional integer, returning defaultVal if absent.
func optInt(args map[string]interface{}, key string, defaultVal int) int {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		case string:
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
				return i
			}
		}
	}
	return defaultVal
}

// optString extracts an optional string, returning defaultVal if absent.
func optString(args map[string]interface{}, key string, defaultVal string) string {
	if v, ok := args[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultVal
}

// optIntPtr extracts an optional integer pointer (nil if absent).
func optIntPtr(args map[string]interface{}, key string) *int {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			i := int(n)
			return &i
		case int:
			return &n
		case int64:
			i := int(n)
			return &i
		case string:
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
				return &i
			}
		}
	}
	return nil
}

// textResult wraps a JSON-marshalled value as an MCP text content result.
func textResult(v interface{}) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []interface{}{mcp.TextContent{Type: "text", Text: string(data)}},
	}, nil
}

// ---- Tool handlers ----

func (h *Handler) handleCreateProject(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	req := models.CreateProjectRequest{
		Name:        optString(args, "name", ""),
		Code:        optString(args, "code", ""),
		Description: optString(args, "description", ""),
	}

	project, err := h.projectUC.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}
	return textResult(project)
}

func (h *Handler) handleListProjects(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	limit := optInt(args, "limit", 50)
	offset := optInt(args, "offset", 0)

	projects, err := h.projectUC.List(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	return textResult(map[string]interface{}{"projects": projects})
}

func (h *Handler) handleGetProject(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	project, err := h.projectUC.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}
	return textResult(map[string]interface{}{"project": project})
}

func (h *Handler) handleGetProjectByCode(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	code, err := argString(args, "code")
	if err != nil {
		return nil, err
	}

	project, err := h.projectUC.GetByCode(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("failed to get project by code: %w", err)
	}
	return textResult(map[string]interface{}{"project": project})
}

func (h *Handler) handleUpdateProject(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	req := models.UpdateProjectRequest{
		ProjectID:   projectID,
		Name:        optString(args, "name", ""),
		Description: optString(args, "description", ""),
		State:       optString(args, "state", ""),
	}

	if err := h.projectUC.Update(ctx, req); err != nil {
		return nil, fmt.Errorf("failed to update project: %w", err)
	}
	return textResult(map[string]interface{}{"project_id": projectID, "updated": true})
}

func (h *Handler) handleCreateEpic(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	name, err := argString(args, "name")
	if err != nil {
		return nil, err
	}
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	priority := optString(args, "priority", "1")
	if priority == "" {
		priority = "1"
	}

	req := models.CreateEpicRequest{
		Name:        name,
		ProjectID:   projectID,
		Description: optString(args, "description", ""),
		Priority:    priority,
	}

	epic, err := h.epicUC.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create epic: %w", err)
	}
	return textResult(epic)
}

func (h *Handler) handleGetEpic(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	epicID, err := argInt(args, "epic_id")
	if err != nil {
		return nil, err
	}

	epic, err := h.epicUC.GetByID(ctx, epicID)
	if err != nil {
		return nil, fmt.Errorf("failed to get epic: %w", err)
	}
	return textResult(map[string]interface{}{"epic": epic})
}

func (h *Handler) handleListEpics(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	epics, err := h.epicUC.ListByProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list epics: %w", err)
	}
	return textResult(map[string]interface{}{"epics": epics})
}

func (h *Handler) handleUpdateEpicDAG(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	epicID, err := argInt(args, "epic_id")
	if err != nil {
		return nil, err
	}
	dagJSON, err := argString(args, "dag_json")
	if err != nil {
		return nil, err
	}

	if err := h.epicUC.UpdateDAG(ctx, epicID, dagJSON); err != nil {
		return nil, fmt.Errorf("failed to update epic DAG: %w", err)
	}
	return textResult(map[string]interface{}{"epic_id": epicID, "dag_updated": true})
}

func (h *Handler) handleCreateStory(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	name, err := argString(args, "name")
	if err != nil {
		return nil, err
	}
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	epicID, err := argInt(args, "epic_id")
	if err != nil {
		return nil, err
	}

	priority := optString(args, "priority", "1")
	if priority == "" {
		priority = "1"
	}

	req := models.CreateStoryRequest{
		Name:               name,
		ProjectID:          projectID,
		EpicID:             epicID,
		Description:        optString(args, "description", ""),
		AcceptanceCriteria: optString(args, "acceptance_criteria", ""),
		Priority:           priority,
		PlannedHours:       optFloat64(args, "planned_hours", 0.0),
	}

	story, err := h.storyUC.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create story: %w", err)
	}
	return textResult(story)
}

func (h *Handler) handleGetStory(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	storyID, err := argInt(args, "story_id")
	if err != nil {
		return nil, err
	}

	story, err := h.storyUC.GetByID(ctx, storyID)
	if err != nil {
		return nil, fmt.Errorf("failed to get story: %w", err)
	}
	return textResult(map[string]interface{}{"story": story})
}

func (h *Handler) handleListStories(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	epicID, err := argInt(args, "epic_id")
	if err != nil {
		return nil, err
	}

	stories, err := h.storyUC.ListByEpic(ctx, epicID)
	if err != nil {
		return nil, fmt.Errorf("failed to list stories: %w", err)
	}
	return textResult(map[string]interface{}{"stories": stories})
}

func (h *Handler) handleCreateTask(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	name, err := argString(args, "name")
	if err != nil {
		return nil, err
	}
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	priority := optString(args, "priority", "1")
	if priority == "" {
		priority = "1"
	}

	req := models.CreateTaskRequest{
		Name:               name,
		ProjectID:          projectID,
		Description:        optString(args, "description", ""),
		AcceptanceCriteria: optString(args, "acceptance_criteria", ""),
		Priority:           priority,
		PlannedHours:       optFloat64(args, "planned_hours", 0.0),
		DAGLevel:           optInt(args, "dag_level", 0),
		EpicID:             optIntPtr(args, "epic_id"),
		StoryID:            optIntPtr(args, "story_id"),
	}

	if deps, ok := args["dependency_ids"].([]interface{}); ok {
		req.DependencyIDs = make([]int, 0, len(deps))
		for _, d := range deps {
			switch n := d.(type) {
			case float64:
				req.DependencyIDs = append(req.DependencyIDs, int(n))
			case int:
				req.DependencyIDs = append(req.DependencyIDs, n)
			case int64:
				req.DependencyIDs = append(req.DependencyIDs, int(n))
			}
		}
	}

	task, err := h.taskUC.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create task: %w", err)
	}
	return textResult(task)
}

func (h *Handler) handleListTasks(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	projectID := optInt(args, "project_id", 0)
	epicID := optInt(args, "epic_id", 0)
	storyID := optInt(args, "story_id", 0)
	state := optString(args, "state", "")

	tasks, err := h.taskUC.ListTasks(ctx, projectID, epicID, storyID, state)
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	return textResult(map[string]interface{}{
		"tasks": tasks,
		"count": len(tasks),
	})
}

func (h *Handler) handleGetTask(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	taskID, err := argInt(args, "task_id")
	if err != nil {
		return nil, err
	}

	task, err := h.taskUC.GetByID(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to get task: %w", err)
	}
	return textResult(map[string]interface{}{"task": task})
}

func (h *Handler) handleUpdateTask(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	taskID, err := argInt(args, "task_id")
	if err != nil {
		return nil, err
	}

	req := models.UpdateTaskRequest{
		TaskID:      taskID,
		Name:        optString(args, "name", ""),
		Description: optString(args, "description", ""),
		State:       optString(args, "state", ""),
		AgentID:     optString(args, "agent_id", ""),
		ContextData: optString(args, "context_data", ""),
	}

	if err := h.taskUC.Update(ctx, req); err != nil {
		return nil, fmt.Errorf("failed to update task: %w", err)
	}
	return textResult(map[string]interface{}{"task_id": taskID, "updated": true})
}

func (h *Handler) handleDispatchTask(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	taskID, err := argInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	agentID, err := argString(args, "agent_id")
	if err != nil {
		return nil, err
	}

	if err := h.taskUC.Dispatch(ctx, taskID, agentID); err != nil {
		return nil, fmt.Errorf("failed to dispatch task: %w", err)
	}
	return textResult(map[string]interface{}{"task_id": taskID, "dispatched_to": agentID})
}

func (h *Handler) handleCompleteTask(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	taskID, err := argInt(args, "task_id")
	if err != nil {
		return nil, err
	}

	if err := h.taskUC.Complete(ctx, taskID); err != nil {
		return nil, fmt.Errorf("failed to complete task: %w", err)
	}
	return textResult(map[string]interface{}{"task_id": taskID, "state": "completed"})
}

func (h *Handler) handleBlockTask(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	taskID, err := argInt(args, "task_id")
	if err != nil {
		return nil, err
	}

	if err := h.taskUC.Block(ctx, taskID, optString(args, "reason", "")); err != nil {
		return nil, fmt.Errorf("failed to block task: %w", err)
	}
	return textResult(map[string]interface{}{"task_id": taskID, "state": "blocked"})
}

func (h *Handler) handleListReadyTasks(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	tasks, err := h.taskUC.ListReady(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list ready tasks: %w", err)
	}
	return textResult(map[string]interface{}{"ready_tasks": tasks})
}

func (h *Handler) handleAddTaskComment(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	taskID, err := argInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	body, err := argString(args, "body")
	if err != nil {
		return nil, err
	}

	req := models.AddCommentRequest{TaskID: taskID, Body: body}
	if err := h.taskUC.AddComment(ctx, req); err != nil {
		return nil, fmt.Errorf("failed to add comment: %w", err)
	}
	return textResult(map[string]interface{}{"task_id": taskID, "comment_added": true})
}

func (h *Handler) handleCreateKBEntry(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	title, err := argString(args, "title")
	if err != nil {
		return nil, err
	}
	content, err := argString(args, "content")
	if err != nil {
		return nil, err
	}
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	entryType := optString(args, "entry_type", "context")
	if entryType == "" {
		entryType = "context"
	}

	req := models.CreateKBEntryRequest{
		Title:          title,
		Content:        content,
		ProjectID:      projectID,
		EntryType:      entryType,
		EpicID:         optIntPtr(args, "epic_id"),
		StoryID:        optIntPtr(args, "story_id"),
		TaskID:         optIntPtr(args, "task_id"),
		CreatedByAgent: optString(args, "created_by_agent", ""),
	}

	if tags, ok := args["tags"].([]interface{}); ok {
		req.Tags = make([]string, 0, len(tags))
		for _, t := range tags {
			if s, ok := t.(string); ok {
				req.Tags = append(req.Tags, s)
			}
		}
	}

	entry, err := h.kbUC.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create KB entry: %w", err)
	}
	return textResult(entry)
}

func (h *Handler) handleSearchKB(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	query, err := argString(args, "query")
	if err != nil {
		return nil, err
	}

	req := models.SearchKBRequest{
		Query:     query,
		ProjectID: optIntPtr(args, "project_id"),
		EntryType: optString(args, "entry_type", ""),
		Limit:     optInt(args, "limit", 20),
	}

	entries, err := h.kbUC.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to search KB: %w", err)
	}
	return textResult(map[string]interface{}{"entries": entries})
}

func (h *Handler) handleGetKBEntry(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	entryID, err := argInt(args, "entry_id")
	if err != nil {
		return nil, err
	}

	entry, err := h.kbUC.GetByID(ctx, entryID)
	if err != nil {
		return nil, fmt.Errorf("failed to get KB entry: %w", err)
	}
	return textResult(map[string]interface{}{"entry": entry})
}

func (h *Handler) handleGetProjectKB(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	entries, err := h.kbUC.ListByProject(ctx, projectID, optInt(args, "limit", 20))
	if err != nil {
		return nil, fmt.Errorf("failed to get project KB: %w", err)
	}
	return textResult(map[string]interface{}{"entries": entries})
}

func (h *Handler) handleGetTaskKB(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	taskID, err := argInt(args, "task_id")
	if err != nil {
		return nil, err
	}

	entries, err := h.kbUC.ListByTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to get task KB: %w", err)
	}
	return textResult(map[string]interface{}{"entries": entries})
}

func (h *Handler) handleGetDashboard(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	projectID, err := argInt(args, "project_id")
	if err != nil {
		return nil, err
	}

	dashboard, err := h.dashboardUC.GetProjectDashboard(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to get dashboard: %w", err)
	}
	return textResult(dashboard)
}

func (h *Handler) handleValidateContext(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	contextData, _ := args["context"].(map[string]interface{})
	if contextData == nil {
		contextData = map[string]interface{}{}
	}

	ampContext := models.AMPContext{
		Version:     getStringFromMap(contextData, "version"),
		ProjectCode: getStringFromMap(contextData, "project_code"),
		ProjectName: getStringFromMap(contextData, "project_name"),
		Context:     make(map[string]interface{}),
	}
	if pid, ok := contextData["project_id"]; ok && pid != nil {
		switch n := pid.(type) {
		case float64:
			ampContext.ProjectID = int(n)
		case int:
			ampContext.ProjectID = n
		case int64:
			ampContext.ProjectID = int(n)
		}
	}

	req := models.ValidateContextRequest{Context: ampContext}
	valid, projectName, err := h.dashboardUC.ValidateContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to validate context: %w", err)
	}

	result := map[string]interface{}{"valid": valid}
	if valid {
		result["project_name"] = projectName
	} else {
		result["error"] = "context validation failed"
	}
	return textResult(result)
}

func (h *Handler) handleHealthCheck(ctx context.Context, args map[string]interface{}) (*mcp.CallToolResult, error) {
	result := map[string]interface{}{
		"status":         "healthy",
		"odoo_connected": h.odooClient != nil,
		"odoo_version":   "unknown",
		"amp_module":     "amp.project",
	}
	return textResult(result)
}

// ---- String/float helpers ----

func optFloat64(args map[string]interface{}, key string, defaultVal float64) float64 {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		case string:
			var f float64
			if _, err := fmt.Sscanf(n, "%f", &f); err == nil {
				return f
			}
		}
	}
	return defaultVal
}

func getStringFromMap(data map[string]interface{}, key string) string {
	if v, ok := data[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
