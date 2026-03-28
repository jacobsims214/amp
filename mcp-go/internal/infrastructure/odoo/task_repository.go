package odoo

import (
	"context"
	"fmt"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// taskRepository implements repository.TaskRepository
type taskRepository struct {
	client *Client
}

// NewTaskRepository creates a new task repository
func NewTaskRepository(client *Client) repository.TaskRepository {
	return &taskRepository{client: client}
}

func (r *taskRepository) Create(ctx context.Context, req models.CreateTaskRequest) (*models.Task, error) {
	vals := map[string]interface{}{
		"name":                req.Name,
		"project_id":          req.ProjectID,
		"description":         req.Description,
		"acceptance_criteria": req.AcceptanceCriteria,
		"priority":            req.Priority,
		"planned_hours":       req.PlannedHours,
		"dag_level":           req.DAGLevel,
		"state":               "backlog",
	}

	if req.EpicID != nil {
		vals["epic_id"] = *req.EpicID
	}
	if req.StoryID != nil {
		vals["story_id"] = *req.StoryID
	}
	if len(req.DependencyIDs) > 0 {
		deps := make([]interface{}, len(req.DependencyIDs))
		for i, id := range req.DependencyIDs {
			deps[i] = id
		}
		// Odoo many2many command: [[6, 0, [ids...]]] replaces the entire set
		vals["dependency_ids"] = []interface{}{[]interface{}{6, 0, deps}}
	}

	result, err := r.client.Execute(ctx, "amp.task", "create", vals)
	if err != nil {
		return nil, fmt.Errorf("failed to create task: %w", err)
	}

	taskID := int(result.(int64))
	return r.GetByID(ctx, taskID)
}

func (r *taskRepository) GetByID(ctx context.Context, id int) (*models.Task, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.task", "read",
		[]interface{}{[]int{id}},
		map[string]interface{}{
			"fields": []string{
				"name", "description", "description_text", "acceptance_criteria",
				"project_id", "epic_id", "story_id", "state", "is_ready",
				"dag_level", "dag_critical_path", "priority", "planned_hours",
				"actual_hours", "agent_id", "agent_session", "dispatch_time",
				"completion_time", "context_data", "dependency_ids", "blocked_ids",
				"dependency_count", "blocked_count",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get task: %w", err)
	}

	tasks := result.([]interface{})
	if len(tasks) == 0 {
		return nil, fmt.Errorf("task not found")
	}

	return mapToTask(tasks[0].(map[string]interface{})), nil
}

func (r *taskRepository) Update(ctx context.Context, req models.UpdateTaskRequest) error {
	vals := map[string]interface{}{}
	if req.Name != "" {
		vals["name"] = req.Name
	}
	if req.Description != "" {
		vals["description"] = req.Description
	}
	if req.State != "" {
		vals["state"] = req.State
	}
	if req.AgentID != "" {
		vals["agent_id"] = req.AgentID
	}
	if req.ContextData != "" {
		vals["context_data"] = req.ContextData
	}

	if len(vals) == 0 {
		return nil
	}

	_, err := r.client.Execute(ctx, "amp.task", "write", []int{req.TaskID}, vals)
	if err != nil {
		return fmt.Errorf("failed to update task: %w", err)
	}
	return nil
}

func (r *taskRepository) Dispatch(ctx context.Context, taskID int, agentID string) error {
	// Pack task_id and agent_id into a single positional args list, matching Python: execute("amp.task", "action_dispatch", [task_id, agent_id])
	_, err := r.client.Execute(ctx, "amp.task", "action_dispatch", []interface{}{taskID, agentID})
	if err != nil {
		return fmt.Errorf("failed to dispatch task: %w", err)
	}
	return nil
}

func (r *taskRepository) Complete(ctx context.Context, taskID int) error {
	_, err := r.client.Execute(ctx, "amp.task", "action_complete", []int{taskID})
	if err != nil {
		return fmt.Errorf("failed to complete task: %w", err)
	}
	return nil
}

func (r *taskRepository) Block(ctx context.Context, taskID int, reason string) error {
	// Pack task_id and reason into a single positional args list, matching Python: execute("amp.task", "action_block", [task_id, reason])
	_, err := r.client.Execute(ctx, "amp.task", "action_block", []interface{}{taskID, reason})
	if err != nil {
		return fmt.Errorf("failed to block task: %w", err)
	}
	return nil
}

func (r *taskRepository) ListByStory(ctx context.Context, storyID int) ([]models.Task, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.task", "search_read",
		[]interface{}{[][]interface{}{{"story_id", "=", storyID}}},
		map[string]interface{}{
			"fields": []string{
				"name", "state", "is_ready", "dag_level", "dag_critical_path",
				"agent_id", "priority",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks by story: %w", err)
	}

	tasks := result.([]interface{})
	resultList := make([]models.Task, len(tasks))
	for i, t := range tasks {
		resultList[i] = *mapToTask(t.(map[string]interface{}))
	}

	return resultList, nil
}

func (r *taskRepository) ListTasks(ctx context.Context, projectID, epicID, storyID int, state string) ([]models.Task, error) {
	domain := [][]interface{}{}
	if projectID != 0 {
		domain = append(domain, []interface{}{"project_id", "=", projectID})
	}
	if epicID != 0 {
		domain = append(domain, []interface{}{"epic_id", "=", epicID})
	}
	if storyID != 0 {
		domain = append(domain, []interface{}{"story_id", "=", storyID})
	}
	if state != "" {
		domain = append(domain, []interface{}{"state", "=", state})
	}
	if len(domain) == 0 {
		domain = append(domain, []interface{}{"id", ">", 0})
	}

	result, err := r.client.ExecuteKw(ctx, "amp.task", "search_read",
		[]interface{}{domain},
		map[string]interface{}{
			"fields": []string{
				"name", "description_text", "acceptance_criteria",
				"state", "is_ready", "priority",
				"project_id", "epic_id", "story_id",
				"agent_id", "dispatch_time", "completion_time",
				"dag_level", "dag_critical_path",
				"dependency_count", "blocked_count",
			},
			"order": "story_id, dag_level, priority desc, id",
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}

	tasks := result.([]interface{})
	resultList := make([]models.Task, len(tasks))
	for i, t := range tasks {
		resultList[i] = *mapToTask(t.(map[string]interface{}))
	}
	return resultList, nil
}

func (r *taskRepository) ListByProject(ctx context.Context, projectID int, state string) ([]models.Task, error) {
	domain := [][]interface{}{{"project_id", "=", projectID}}
	if state != "" {
		domain = append(domain, []interface{}{"state", "=", state})
	}

	result, err := r.client.ExecuteKw(ctx, "amp.task", "search_read",
		[]interface{}{domain},
		map[string]interface{}{
			"fields": []string{
				"name", "state", "is_ready", "story_id", "agent_id",
				"dag_level", "dag_critical_path", "priority",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks by project: %w", err)
	}

	tasks := result.([]interface{})
	resultList := make([]models.Task, len(tasks))
	for i, t := range tasks {
		resultList[i] = *mapToTask(t.(map[string]interface{}))
	}

	return resultList, nil
}

func (r *taskRepository) ListReady(ctx context.Context, projectID int) ([]models.Task, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.task", "search_read",
		[]interface{}{[][]interface{}{
			{"project_id", "=", projectID},
			{"state", "=", "ready"},
			{"is_ready", "=", true},
		}},
		map[string]interface{}{
			"fields": []string{
				"name", "description_text", "acceptance_criteria",
				"story_id", "dag_level", "planned_hours",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list ready tasks: %w", err)
	}

	tasks := result.([]interface{})
	resultList := make([]models.Task, len(tasks))
	for i, t := range tasks {
		resultList[i] = *mapToTask(t.(map[string]interface{}))
	}

	return resultList, nil
}

func (r *taskRepository) AddComment(ctx context.Context, req models.AddCommentRequest) error {
	_, err := r.client.ExecuteKw(ctx, "amp.task", "message_post",
		[]interface{}{[]int{req.TaskID}},
		map[string]interface{}{
			"body":         req.Body,
			"message_type": "comment",
		})
	if err != nil {
		return fmt.Errorf("failed to add comment: %w", err)
	}
	return nil
}

func mapToTask(data map[string]interface{}) *models.Task {
	t := &models.Task{
		ID:                 int(data["id"].(int64)),
		Name:               getString(data, "name"),
		Description:        getString(data, "description"),
		DescriptionText:    getString(data, "description_text"),
		AcceptanceCriteria: getString(data, "acceptance_criteria"),
		State:              getString(data, "state"),
		AgentID:            getString(data, "agent_id"),
		AgentSession:       getString(data, "agent_session"),
		DispatchTime:       getString(data, "dispatch_time"),
		CompletionTime:     getString(data, "completion_time"),
		ContextData:        getString(data, "context_data"),
		Priority:           getString(data, "priority"),
	}

	if v, ok := data["project_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			t.ProjectID = int(arr[0].(int64))
		}
	}
	if v, ok := data["epic_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			t.EpicID = int(arr[0].(int64))
		}
	}
	if v, ok := data["story_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			t.StoryID = int(arr[0].(int64))
		}
	}
	if v, ok := data["is_ready"]; ok && v != false {
		t.IsReady = v.(bool)
	}
	if v, ok := data["dag_critical_path"]; ok && v != false {
		t.DAGCriticalPath = v.(bool)
	}
	if v, ok := data["dag_level"]; ok && v != false {
		t.DAGLevel = int(v.(int64))
	}
	if v, ok := data["planned_hours"]; ok && v != false {
		t.PlannedHours = v.(float64)
	}
	if v, ok := data["actual_hours"]; ok && v != false {
		t.ActualHours = v.(float64)
	}
	if v, ok := data["dependency_count"]; ok && v != false {
		t.DependencyCount = int(v.(int64))
	}
	if v, ok := data["blocked_count"]; ok && v != false {
		t.BlockedCount = int(v.(int64))
	}

	return t
}
