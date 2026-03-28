package odoo

import (
	"context"
	"fmt"
	"github.com/amp/mcp-go/internal/domain/models"
	"github.com/amp/mcp-go/internal/domain/repository"
)

// dashboardRepository implements repository.DashboardRepository
type dashboardRepository struct {
	client *Client
}

// NewDashboardRepository creates a new dashboard repository
func NewDashboardRepository(client *Client) repository.DashboardRepository {
	return &dashboardRepository{client: client}
}

func (r *dashboardRepository) GetProjectDashboard(ctx context.Context, projectID int) (*models.Dashboard, error) {
	result, err := r.client.Execute(ctx, "amp.dashboard", "get_project_dashboard", []int{projectID})
	if err != nil {
		return nil, fmt.Errorf("failed to get dashboard: %w", err)
	}

	data := result.(map[string]interface{})
	dashboard := &models.Dashboard{
		ProjectID:          projectID,
		ProjectName:        getString(data, "project_name"),
		EpicCount:          getInt(data, "epic_count"),
		StoryCount:         getInt(data, "story_count"),
		TaskCount:          getInt(data, "task_count"),
		CompletedTaskCount: getInt(data, "completed_task_count"),
		BlockedTaskCount:   getInt(data, "blocked_task_count"),
		ReadyTaskCount:     getInt(data, "ready_task_count"),
		InProgressCount:    getInt(data, "in_progress_count"),
		ProgressPercentage: getFloat64(data, "progress_percentage"),
	}

	if v, ok := data["active_agents"]; ok && v != false {
		if arr, ok := v.([]interface{}); ok {
			agents := make([]string, len(arr))
			for i, agent := range arr {
				agents[i] = agent.(string)
			}
			dashboard.ActiveAgents = agents
		}
	}

	return dashboard, nil
}

func (r *dashboardRepository) ValidateContext(ctx context.Context, req models.ValidateContextRequest) (bool, string, error) {
	// Check required fields
	if req.Context.ProjectID == 0 {
		return false, "", fmt.Errorf("missing required field: project_id")
	}
	if req.Context.ProjectCode == "" {
		return false, "", fmt.Errorf("missing required field: project_code")
	}

	// Verify project exists
	result, err := r.client.ExecuteKw(ctx, "amp.project", "read",
		[]interface{}{[]int{req.Context.ProjectID}},
		map[string]interface{}{
			"fields": []string{"name"},
		})
	if err != nil {
		return false, "", fmt.Errorf("failed to validate project: %w", err)
	}

	projects := result.([]interface{})
	if len(projects) == 0 {
		return false, "", fmt.Errorf("project not found in Odoo")
	}

	project := projects[0].(map[string]interface{})
	projectName := getString(project, "name")

	return true, projectName, nil
}

// Helper functions
func getString(data map[string]interface{}, key string) string {
	if v, ok := data[key]; ok && v != nil && v != false {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(data map[string]interface{}, key string) int {
	if v, ok := data[key]; ok && v != nil && v != false {
		if i, ok := v.(int64); ok {
			return int(i)
		}
	}
	return 0
}

func getFloat64(data map[string]interface{}, key string) float64 {
	if v, ok := data[key]; ok && v != nil && v != false {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}
