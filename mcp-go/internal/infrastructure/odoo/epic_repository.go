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
				"name", "code", "description", "project_id", "state",
				"story_count", "task_count", "progress_percentage", "story_ids", "dag_json",
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

func (r *epicRepository) UpdateDAG(ctx context.Context, epicID int, dagJSON string) error {
	_, err := r.client.Execute(ctx, "amp.epic", "write", []int{epicID}, map[string]interface{}{
		"dag_json": dagJSON,
	})
	if err != nil {
		return fmt.Errorf("failed to update epic DAG: %w", err)
	}
	return nil
}

func mapToEpic(data map[string]interface{}) *models.Epic {
	e := &models.Epic{
		ID:          int(data["id"].(int64)),
		Name:        getString(data, "name"),
		Code:        getString(data, "code"),
		Description: getString(data, "description"),
		State:       getString(data, "state"),
		DAGJSON:     getString(data, "dag_json"),
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
