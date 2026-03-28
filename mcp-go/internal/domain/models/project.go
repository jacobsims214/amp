package models

// Project represents an AMP project
type Project struct {
	ID                 int     `json:"id"`
	Name               string  `json:"name"`
	Code               string  `json:"code"`
	Description        string  `json:"description"`
	State              string  `json:"state"`
	EpicCount          int     `json:"epic_count"`
	StoryCount         int     `json:"story_count"`
	TaskCount          int     `json:"task_count"`
	CompletedTaskCount int     `json:"completed_task_count"`
	BlockedTaskCount   int     `json:"blocked_task_count"`
	ProgressPercentage float64 `json:"progress_percentage"`
	ActiveAgentCount   int     `json:"active_agent_count"`
	LastSession        string  `json:"last_session"`
	EpicIDs            []int   `json:"epic_ids"`
}

// CreateProjectRequest represents the request to create a project
type CreateProjectRequest struct {
	Name        string `json:"name"`
	Code        string `json:"code,omitempty"`
	Description string `json:"description,omitempty"`
}

// UpdateProjectRequest represents the request to update a project
type UpdateProjectRequest struct {
	ProjectID   int    `json:"project_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
}
