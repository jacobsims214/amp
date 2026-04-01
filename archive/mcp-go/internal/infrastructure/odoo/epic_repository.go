package odoo

import (
	"context"
	"fmt"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// epicRepository implements repository.EpicRepository
type epicRepository struct {
	client *Client
}

// NewEpicRepository creates a new epic repository
func NewEpicRepository(client *Client) repository.EpicRepository {
	return &epicRepository{client: client}
}

func (r *epicRepository) Create(ctx context.Context, req models.CreateEpicRequest) (*models.Epic, error) {
	vals := map[string]interface{}{
		"name":        req.Name,
		"project_id":  req.ProjectID,
		"description": req.Description,
		"priority":    req.Priority,
		"state":       "backlog",
	}

	result, err := r.client.Execute(ctx, "amp.epic", "create", vals)
	if err != nil {
		return nil, fmt.Errorf("failed to create epic: %w", err)
	}

	epicID := int(result.(int64))
	return r.GetByID(ctx, epicID)
}

func (r *epicRepository) GetByID(ctx context.Context, id int) (*models.Epic, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.epic", "read",
		[]interface{}{[]int{id}},
		map[string]interface{}{
			"fields": []string{
				"name", "description", "project_id", "state",
				"story_count", "task_count", "completed_story_count", "completed_task_count",
				"progress_percentage", "story_ids",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get epic: %w", err)
	}

	epics := result.([]interface{})
	if len(epics) == 0 {
		return nil, fmt.Errorf("epic not found")
	}

	return mapToEpic(epics[0].(map[string]interface{})), nil
}

func (r *epicRepository) ListByProject(ctx context.Context, projectID int) ([]models.Epic, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.epic", "search_read",
		[]interface{}{[][]interface{}{{"project_id", "=", projectID}}},
		map[string]interface{}{
			"fields": []string{
				"name", "state", "story_count", "progress_percentage", "priority",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list epics: %w", err)
	}

	epics := result.([]interface{})
	resultList := make([]models.Epic, len(epics))
	for i, e := range epics {
		resultList[i] = *mapToEpic(e.(map[string]interface{}))
	}

	return resultList, nil
}


func (r *epicRepository) SetState(ctx context.Context, epicID int, state string, reason string) error {
	methodMap := map[string]string{
		"in_progress": "action_start",
		"completed":   "action_complete",
		"blocked":     "action_block",
		"backlog":     "action_reset",
	}
	method, ok := methodMap[state]
	if !ok {
		return fmt.Errorf("invalid epic state %q: must be backlog, in_progress, completed, or blocked", state)
	}
	var args []interface{}
	if state == "blocked" && reason != "" {
		args = []interface{}{reason}
	}
	_, err := r.client.Execute(ctx, "amp.epic", method, append([]interface{}{[]int{epicID}}, args...)...)
	if err != nil {
		return fmt.Errorf("failed to set epic state to %q: %w", state, err)
	}
	return nil
}

func (r *epicRepository) Delete(ctx context.Context, epicID int) error {
	// Deleting an epic cascades to stories and tasks in Odoo
	_, err := r.client.Execute(ctx, "amp.epic", "unlink", []int{epicID})
	if err != nil {
		return fmt.Errorf("failed to delete epic: %w", err)
	}
	return nil
}

func (r *epicRepository) DeleteByProject(ctx context.Context, projectID int) (int, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.epic", "search",
		[]interface{}{[][]interface{}{{"project_id", "=", projectID}}},
		map[string]interface{}{})
	if err != nil {
		return 0, fmt.Errorf("failed to search epics: %w", err)
	}
	ids := result.([]interface{})
	if len(ids) == 0 {
		return 0, nil
	}
	intIDs := make([]int, len(ids))
	for i, id := range ids {
		intIDs[i] = int(id.(int64))
	}
	_, err = r.client.Execute(ctx, "amp.epic", "unlink", intIDs)
	if err != nil {
		return 0, fmt.Errorf("failed to delete epics: %w", err)
	}
	return len(intIDs), nil
}

func mapToEpic(data map[string]interface{}) *models.Epic {
	e := &models.Epic{
		ID:          int(data["id"].(int64)),
		Name:        getString(data, "name"),
		Description: getString(data, "description"),
		State:       getString(data, "state"),
		Priority:    getString(data, "priority"),
	}

	if v, ok := data["project_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			e.ProjectID = int(arr[0].(int64))
		}
	}
	if v, ok := data["story_count"]; ok && v != false {
		e.StoryCount = int(v.(int64))
	}
	if v, ok := data["task_count"]; ok && v != false {
		e.TaskCount = int(v.(int64))
	}
	if v, ok := data["progress_percentage"]; ok && v != false {
		e.ProgressPercentage = v.(float64)
	}

	return e
}
