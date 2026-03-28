package odoo

import (
	"context"
	"fmt"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// storyRepository implements repository.StoryRepository
type storyRepository struct {
	client *Client
}

// NewStoryRepository creates a new story repository
func NewStoryRepository(client *Client) repository.StoryRepository {
	return &storyRepository{client: client}
}

func (r *storyRepository) Create(ctx context.Context, req models.CreateStoryRequest) (*models.Story, error) {
	vals := map[string]interface{}{
		"name":                req.Name,
		"project_id":          req.ProjectID,
		"epic_id":             req.EpicID,
		"description":         req.Description,
		"acceptance_criteria": req.AcceptanceCriteria,
		"priority":            req.Priority,
		"planned_hours":       req.PlannedHours,
		"state":               "backlog",
	}

	result, err := r.client.Execute(ctx, "amp.story", "create", vals)
	if err != nil {
		return nil, fmt.Errorf("failed to create story: %w", err)
	}

	storyID := int(result.(int64))
	return r.GetByID(ctx, storyID)
}

func (r *storyRepository) GetByID(ctx context.Context, id int) (*models.Story, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.story", "read",
		[]interface{}{[]int{id}},
		map[string]interface{}{
			"fields": []string{
				"name", "description", "acceptance_criteria", "project_id", "epic_id",
				"state", "is_ready", "task_count", "progress_percentage", "task_ids",
				"dependency_ids", "blocked_ids", "priority", "planned_hours",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get story: %w", err)
	}

	stories := result.([]interface{})
	if len(stories) == 0 {
		return nil, fmt.Errorf("story not found")
	}

	return mapToStory(stories[0].(map[string]interface{})), nil
}

func (r *storyRepository) ListByEpic(ctx context.Context, epicID int) ([]models.Story, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.story", "search_read",
		[]interface{}{[][]interface{}{{"epic_id", "=", epicID}}},
		map[string]interface{}{
			"fields": []string{
				"name", "state", "is_ready", "task_count", "progress_percentage",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list stories: %w", err)
	}

	stories := result.([]interface{})
	resultList := make([]models.Story, len(stories))
	for i, s := range stories {
		resultList[i] = *mapToStory(s.(map[string]interface{}))
	}

	return resultList, nil
}

func mapToStory(data map[string]interface{}) *models.Story {
	s := &models.Story{
		ID:                 int(data["id"].(int64)),
		Name:               getString(data, "name"),
		Description:        getString(data, "description"),
		AcceptanceCriteria: getString(data, "acceptance_criteria"),
		State:              getString(data, "state"),
		Priority:           getString(data, "priority"),
	}

	if v, ok := data["project_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			s.ProjectID = int(arr[0].(int64))
		}
	}
	if v, ok := data["epic_id"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			s.EpicID = int(arr[0].(int64))
		}
	}
	if v, ok := data["is_ready"]; ok && v != false {
		s.IsReady = v.(bool)
	}
	if v, ok := data["task_count"]; ok && v != false {
		s.TaskCount = int(v.(int64))
	}
	if v, ok := data["progress_percentage"]; ok && v != false {
		s.ProgressPercentage = v.(float64)
	}
	if v, ok := data["planned_hours"]; ok && v != false {
		s.PlannedHours = v.(float64)
	}

	return s
}
