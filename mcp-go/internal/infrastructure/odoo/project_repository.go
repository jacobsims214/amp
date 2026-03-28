package odoo

import (
	"context"
	"fmt"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// projectRepository implements repository.ProjectRepository
type projectRepository struct {
	client *Client
}

// NewProjectRepository creates a new project repository
func NewProjectRepository(client *Client) repository.ProjectRepository {
	return &projectRepository{client: client}
}

func (r *projectRepository) Create(ctx context.Context, req models.CreateProjectRequest) (*models.Project, error) {
	vals := map[string]interface{}{
		"name":        req.Name,
		"description": req.Description,
		"state":       "active",
	}
	if req.Code != "" {
		vals["code"] = req.Code
	}

	result, err := r.client.Execute(ctx, "amp.project", "create", vals)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	projectID := int(result.(int64))
	return r.GetByID(ctx, projectID)
}

func (r *projectRepository) GetByID(ctx context.Context, id int) (*models.Project, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.project", "read",
		[]interface{}{[]int{id}},
		map[string]interface{}{
			"fields": []string{
				"name", "code", "description", "state", "epic_count", "story_count",
				"task_count", "completed_task_count", "blocked_task_count",
				"progress_percentage", "active_agent_count", "last_session", "epic_ids",
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	projects := result.([]interface{})
	if len(projects) == 0 {
		return nil, fmt.Errorf("project not found")
	}

	return mapToProject(projects[0].(map[string]interface{})), nil
}

func (r *projectRepository) GetByCode(ctx context.Context, code string) (*models.Project, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.project", "search_read",
		[]interface{}{[][]interface{}{{"code", "=", code}}},
		map[string]interface{}{
			"fields": []string{
				"name", "code", "description", "state", "epic_count", "story_count",
				"task_count", "completed_task_count", "blocked_task_count",
				"progress_percentage", "active_agent_count", "last_session", "epic_ids",
			},
			"limit": 1,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get project by code: %w", err)
	}

	projects := result.([]interface{})
	if len(projects) == 0 {
		return nil, fmt.Errorf("project not found")
	}

	return mapToProject(projects[0].(map[string]interface{})), nil
}

func (r *projectRepository) List(ctx context.Context, limit, offset int) ([]models.Project, error) {
	result, err := r.client.ExecuteKw(ctx, "amp.project", "search_read",
		[]interface{}{[][]interface{}{{"state", "in", []interface{}{"draft", "active"}}}},
		map[string]interface{}{
			"fields": []string{
				"name", "code", "state", "epic_count", "task_count", "progress_percentage",
			},
			"limit":  limit,
			"offset": offset,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}

	projects := result.([]interface{})
	resultList := make([]models.Project, len(projects))
	for i, p := range projects {
		resultList[i] = *mapToProject(p.(map[string]interface{}))
	}

	return resultList, nil
}

func (r *projectRepository) Update(ctx context.Context, req models.UpdateProjectRequest) error {
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

	if len(vals) == 0 {
		return nil
	}

	_, err := r.client.Execute(ctx, "amp.project", "write", []int{req.ProjectID}, vals)
	if err != nil {
		return fmt.Errorf("failed to update project: %w", err)
	}

	return nil
}

func mapToProject(data map[string]interface{}) *models.Project {
	p := &models.Project{
		ID:          int(data["id"].(int64)),
		Name:        getString(data, "name"),
		Code:        getString(data, "code"),
		Description: getString(data, "description"),
		State:       getString(data, "state"),
	}

	if v, ok := data["epic_count"]; ok && v != false {
		p.EpicCount = int(v.(int64))
	}
	if v, ok := data["story_count"]; ok && v != false {
		p.StoryCount = int(v.(int64))
	}
	if v, ok := data["task_count"]; ok && v != false {
		p.TaskCount = int(v.(int64))
	}
	if v, ok := data["completed_task_count"]; ok && v != false {
		p.CompletedTaskCount = int(v.(int64))
	}
	if v, ok := data["blocked_task_count"]; ok && v != false {
		p.BlockedTaskCount = int(v.(int64))
	}
	if v, ok := data["progress_percentage"]; ok && v != false {
		p.ProgressPercentage = v.(float64)
	}
	if v, ok := data["active_agent_count"]; ok && v != false {
		p.ActiveAgentCount = int(v.(int64))
	}
	if v, ok := data["last_session"]; ok && v != false {
		p.LastSession = v.(string)
	}

	return p
}
