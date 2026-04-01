// Package mcp exposes AMP task operations as MCP tools over HTTP/SSE.
//
// Tool surface (task API only — scoped to what agents actually need):
//
//	amp_create_project   amp_list_projects   amp_get_project
//	amp_create_epic      amp_list_epics      amp_get_epic
//	amp_create_story     amp_list_stories    amp_get_story
//	amp_create_task      amp_list_tasks      amp_get_task
//	amp_update_task      amp_dispatch_task   amp_complete_task
//	amp_block_task       amp_add_task_comment
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
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
}

func NewServer(registry *actor.Registry, repo *repository.Repo, h *hub.Hub, kbSvc *kb.Service) *Server {
	return &Server{registry: registry, repo: repo, hub: h, kb: kbSvc}
}

// Register wires all MCP tools onto the provided MCPServer.
func (s *Server) Register(mcp *server.MCPServer) {
	ctx := context.Background()

	// ---- Projects (CRUD via repo, no actor needed) ----
	mcp.AddTool(tool("amp_create_project", "Create a new AMP project. Returns the created project with its id.",
		props{
			"name":        str("REQUIRED. Project name."),
			"code":        str("REQUIRED. Short unique code, e.g. 'my-project'."),
			"description": str("Optional description."),
		}), wrap(ctx, s.createProject))

	mcp.AddTool(tool("amp_list_projects", "List all active projects.",
		props{}), wrap(ctx, s.listProjects))

	mcp.AddTool(tool("amp_get_project", "Get a project by its numeric ID.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.getProject))

	// ---- Epics ----
	mcp.AddTool(tool("amp_create_epic", "Create an epic inside a project.",
		props{
			"project_id":  num("REQUIRED. Project ID."),
			"name":        str("REQUIRED. Epic name."),
			"description": str("Epic description."),
			"priority":    str("Priority: 0=low, 1=normal (default), 2=high, 3=critical."),
		}), wrap(ctx, s.createEpic))

	mcp.AddTool(tool("amp_list_epics", "List all epics for a project.",
		props{"project_id": num("REQUIRED. Project ID.")}), wrap(ctx, s.listEpics))

	mcp.AddTool(tool("amp_get_epic", "Get an epic by ID.",
		props{"epic_id": num("REQUIRED. Epic ID.")}), wrap(ctx, s.getEpic))

	// ---- Stories ----
	mcp.AddTool(tool("amp_create_story", "Create a story inside an epic.",
		props{
			"project_id":          num("REQUIRED. Project ID."),
			"epic_id":             num("REQUIRED. Epic ID."),
			"name":                str("REQUIRED. Story name."),
			"description":         str("Story description."),
			"acceptance_criteria": str("Conditions that prove this story is done."),
			"priority":            str("Priority: 0=low, 1=normal, 2=high, 3=critical."),
		}), wrap(ctx, s.createStory))

	mcp.AddTool(tool("amp_list_stories", "List all stories for an epic.",
		props{"epic_id": num("REQUIRED. Epic ID.")}), wrap(ctx, s.listStories))

	mcp.AddTool(tool("amp_get_story", "Get a story by ID.",
		props{"story_id": num("REQUIRED. Story ID.")}), wrap(ctx, s.getStory))

	// ---- Tasks (all mutations go through actors) ----
	mcp.AddTool(tool("amp_create_task",
		"Create a task. REQUIRED hierarchy: every task must belong to a story which must belong to an epic. "+
			"Set assigned_to at planning time so the user can review who will work each ticket before dispatch. "+
			"If dependency_ids are provided, state is set automatically (blocked/backlog). Never set state yourself.",
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
		}), wrap(ctx, s.createTask))

	mcp.AddTool(tool("amp_list_tasks",
		"List tasks for a project. Response includes a ready_to_dispatch array (state=backlog) — dispatch everything in it. Blocked tasks include blocked_by_ids showing exactly which task IDs are in the way.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"state":      str("Optional state filter: backlog, in_progress, completed, blocked."),
		}), wrap(ctx, s.listTasks))

	mcp.AddTool(tool("amp_get_task", "Get full task details by ID.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.getTask))

	mcp.AddTool(tool("amp_update_task", "Update task name, description, assigned_to, or agent_id.",
		props{
			"task_id":     num("REQUIRED. Task ID."),
			"name":        str("New task name."),
			"description": str("New task description."),
			"assigned_to": str("Who should work this task — can be corrected by the user before dispatch."),
			"agent_id":    str("Agent identifier — set at dispatch time, not plan time."),
		}), wrap(ctx, s.updateTask))

	mcp.AddTool(tool("amp_dispatch_task", "Dispatch a task to an agent (sets state to in_progress). Fails if task is blocked.",
		props{
			"task_id":  num("REQUIRED. Task ID."),
			"agent_id": str("REQUIRED. Agent identifier, e.g. 'amp-worker'."),
		}), wrap(ctx, s.dispatchTask))

	mcp.AddTool(tool("amp_complete_task", "Mark a task completed. Auto-unblocks any tasks that were waiting on it.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.completeTask))

	mcp.AddTool(tool("amp_block_task", "Block a task with a reason.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"reason":  str("REQUIRED. Why the task is blocked."),
		}), wrap(ctx, s.blockTask))

	mcp.AddTool(tool("amp_add_task_comment", "Post a comment to a task's log. Used by agents to report progress.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"body":    str("REQUIRED. Comment text."),
			"author":  str("Optional. Who is posting (defaults to 'agent')."),
		}), wrap(ctx, s.addComment))

	mcp.AddTool(tool("amp_get_task_comments", "Get all comments for a task.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.getComments))

	mcp.AddTool(tool("amp_get_ticket_history",
		"Get the full activity log for a task — every state change, dispatch, completion, comment, and unblock, "+
			"in chronological order. Use this to understand what has happened on a ticket and who did it.",
		props{"task_id": num("REQUIRED. Task ID.")}), wrap(ctx, s.getTicketHistory))

	// ---- Management / cleanup tools ----

	mcp.AddTool(tool("amp_reset_project",
		"Delete ALL epics, stories, and tasks for a project but keep the project itself. "+
			"Use this when replanning from scratch. DESTRUCTIVE — cannot be undone.",
		props{
			"project_id": num("REQUIRED. Project ID to reset."),
		}), wrap(ctx, s.resetProject))

	mcp.AddTool(tool("amp_delete_task",
		"Delete a single task. Use this to remove tasks created by mistake during planning.",
		props{
			"task_id": num("REQUIRED. Task ID to delete."),
		}), wrap(ctx, s.deleteTask))

	mcp.AddTool(tool("amp_delete_epic",
		"Delete an epic and ALL its stories and tasks (cascades). Use with care.",
		props{
			"epic_id": num("REQUIRED. Epic ID to delete."),
		}), wrap(ctx, s.deleteEpic))

	mcp.AddTool(tool("amp_set_task_state",
		"Override a task's state directly. Use as a manager escape hatch — e.g. re-open a "+
			"completed task, reset an in_progress task back to backlog if its agent crashed. "+
			"Valid states: backlog, in_progress, completed, blocked.",
		props{
			"task_id": num("REQUIRED. Task ID."),
			"state":   str("REQUIRED. New state: backlog, in_progress, completed, or blocked."),
			"reason":  str("Optional. Why this manual override was needed (logged to activity)."),
		}), wrap(ctx, s.setTaskState))

	mcp.AddTool(tool("amp_list_project_stories",
		"List all stories for a project across all epics. Useful for getting a full picture "+
			"without iterating epics one by one.",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.listProjectStories))

	// ---- Knowledge Base ----
	mcp.AddTool(tool("amp_kb_write",
		"Write (create or update) a document in the project knowledge base. "+
			"Use this to record architectural decisions, how-to guides, discoveries, gotchas. "+
			"Documents are chunked and semantically indexed automatically.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"path":       str("REQUIRED. Document path, e.g. 'architecture/auth.md' or 'decisions/001-actor-model.md'."),
			"title":      str("REQUIRED. Document title."),
			"content":    str("REQUIRED. Full document content in markdown."),
			"tags":       arr("Optional. Tag array, e.g. ['auth','jwt','middleware']. Be specific."),
			"author":     str("Optional. Who wrote this (defaults to 'agent')."),
		}), wrap(ctx, s.kbWrite))

	mcp.AddTool(tool("amp_kb_search",
		"Search the project knowledge base using hybrid keyword+semantic search. "+
			"Call this BEFORE starting any task to check what previous agents already documented. "+
			"Returns the most relevant document chunks with excerpts.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"query":      str("REQUIRED. Natural language search query."),
			"tags":       arr("Optional. Filter results to docs with these tags."),
			"limit":      num("Optional. Max results to return (default 10)."),
		}), wrap(ctx, s.kbSearch))

	mcp.AddTool(tool("amp_kb_get",
		"Get the full content of a knowledge base document by its path.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"path":       str("REQUIRED. Document path, e.g. 'architecture/auth.md'."),
		}), wrap(ctx, s.kbGet))

	mcp.AddTool(tool("amp_kb_list",
		"List all documents in the project knowledge base. Optionally filter by tag.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"tag":        str("Optional. Filter to documents with this tag."),
		}), wrap(ctx, s.kbList))

	mcp.AddTool(tool("amp_kb_delete",
		"Delete a document from the knowledge base by its path.",
		props{
			"project_id": num("REQUIRED. Project ID."),
			"path":       str("REQUIRED. Document path to delete."),
		}), wrap(ctx, s.kbDelete))

	mcp.AddTool(tool("amp_kb_tags",
		"List all tags used in the project knowledge base with document counts.",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.kbTags))

	mcp.AddTool(tool("amp_kb_reindex",
		"Re-embed all documents in the project knowledge base. "+
			"Use after changing the embedding model or if search quality degrades.",
		props{
			"project_id": num("REQUIRED. Project ID."),
		}), wrap(ctx, s.kbReindex))
}

// ---- Tool handlers ----

func (s *Server) createProject(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	req := domain.CreateProjectRequest{
		Name:        optStr(args, "name", ""),
		Code:        optStr(args, "code", ""),
		Description: optStr(args, "description", ""),
	}
	if req.Name == "" || req.Code == "" {
		return nil, fmt.Errorf("name and code are required")
	}
	p, err := s.repo.CreateProject(context.Background(), req)
	if err != nil {
		return nil, err
	}
	s.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: p.ID, Payload: p, At: time.Now()})
	return jsonResult(p)
}

func (s *Server) listProjects(_ context.Context, _ map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projects, err := s.repo.ListProjects(context.Background())
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"projects": projects})
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

func (s *Server) createEpic(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	req := domain.CreateEpicRequest{
		ProjectID:   projectID,
		Name:        optStr(args, "name", ""),
		Description: optStr(args, "description", ""),
		Priority:    optStr(args, "priority", "1"),
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

func (s *Server) createStory(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
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

func (s *Server) createTask(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
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

func (s *Server) listTasks(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	state := optStr(args, "state", "")

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
	completed := make([]domain.Task, 0)
	for _, t := range reply.Tasks {
		switch t.State {
		case domain.TaskStateBacklog:
			readyToDispatch = append(readyToDispatch, t)
		case domain.TaskStateInProgress:
			inProgress = append(inProgress, t)
		case domain.TaskStateBlocked:
			blocked = append(blocked, t)
		case domain.TaskStateCompleted:
			completed = append(completed, t)
		}
	}

	return jsonResult(map[string]interface{}{
		"ready_to_dispatch": readyToDispatch, // dispatch ALL of these immediately
		"in_progress":       inProgress,
		"blocked":           blocked, // each has blocked_by_ids showing what's in the way
		"completed":         completed,
		"count":             len(reply.Tasks),
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

func (s *Server) updateTask(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	// Route via repo lookup then actor.
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
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

func (s *Server) dispatchTask(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
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
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgDispatchTask{TaskID: taskID, AgentID: agentID, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "dispatched_to": agentID})
}

func (s *Server) completeTask(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgCompleteTask{TaskID: taskID, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "state": "completed"})
}

func (s *Server) blockTask(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	reason := optStr(args, "reason", "")
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan ReplySimple, 1)
	pid, err := s.registry.Get(task.ProjectID)
	if err != nil {
		return nil, err
	}
	s.registry.System().Root.Send(pid, &actor.MsgBlockTask{TaskID: taskID, Reason: reason, ReplyCh: replyCh})
	reply := <-replyCh
	if reply.Err != nil {
		return nil, reply.Err
	}
	return jsonResult(map[string]interface{}{"task_id": taskID, "state": "blocked"})
}

func (s *Server) addComment(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
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
	req := domain.AddCommentRequest{
		TaskID: taskID,
		Body:   body,
		Author: optStr(args, "author", "agent"),
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
	if _, err := s.repo.GetProject(context.Background(), projectID); err != nil {
		return nil, fmt.Errorf("project %d not found", projectID)
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

func (s *Server) deleteTask(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
		return nil, err
	}
	task, err := s.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("task %d not found", taskID)
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

func (s *Server) deleteEpic(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	epicID, err := reqInt(args, "epic_id")
	if err != nil {
		return nil, err
	}
	epic, err := s.repo.GetEpic(context.Background(), epicID)
	if err != nil {
		return nil, fmt.Errorf("epic %d not found", epicID)
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

func (s *Server) setTaskState(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	taskID, err := reqInt(args, "task_id")
	if err != nil {
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

// ---- KB handlers ----

func (s *Server) kbWrite(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
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
	author := optStr(args, "author", "agent")

	doc, err := s.kb.WriteDoc(context.Background(), projectID, path, title, content, author, tags)
	if err != nil {
		return nil, err
	}
	return jsonResult(doc)
}

func (s *Server) kbSearch(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	query, err := reqStr(args, "query")
	if err != nil {
		return nil, err
	}
	tags := optStrSlice(args, "tags")
	limit := optInt(args, "limit", 10)

	results, err := s.kb.Search(context.Background(), projectID, query, tags, limit)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"results": results, "count": len(results)})
}

func (s *Server) kbGet(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	doc, err := s.kb.GetDoc(context.Background(), projectID, path)
	if err != nil {
		return nil, err
	}
	return jsonResult(doc)
}

func (s *Server) kbList(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	tag := optStr(args, "tag", "")
	docs, err := s.kb.ListDocs(context.Background(), projectID, tag)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"docs": docs, "count": len(docs)})
}

func (s *Server) kbDelete(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	path, err := reqStr(args, "path")
	if err != nil {
		return nil, err
	}
	if err := s.kb.DeleteDoc(context.Background(), projectID, path); err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"deleted": true, "path": path})
}

func (s *Server) kbTags(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	tags, err := s.kb.ListTags(context.Background(), projectID)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]interface{}{"tags": tags})
}

func (s *Server) kbReindex(_ context.Context, args map[string]interface{}) (*mcpgo.CallToolResult, error) {
	projectID, err := reqInt(args, "project_id")
	if err != nil {
		return nil, err
	}
	go func() {
		if err := s.kb.Reindex(context.Background(), projectID); err != nil {
			slog.Default().Error("kb reindex failed", "project_id", projectID, "err", err)
		}
	}()
	return jsonResult(map[string]interface{}{"reindex": "started", "project_id": projectID})
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

func arr(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":        "array",
		"description": desc,
		"items":       map[string]interface{}{"type": "number"},
	}
}

func tool(name, desc string, schema props) mcpgo.Tool {
	return mcpgo.NewTool(name, desc, schema)
}

func wrap(baseCtx context.Context, fn func(context.Context, map[string]interface{}) (*mcpgo.CallToolResult, error)) server.ToolHandlerFunc {
	return func(args map[string]interface{}) (*mcpgo.CallToolResult, error) {
		return fn(baseCtx, args)
	}
}

func jsonResult(v interface{}) (*mcpgo.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcpgo.CallToolResult{
		Content: []interface{}{mcpgo.TextContent{Type: "text", Text: string(data)}},
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
