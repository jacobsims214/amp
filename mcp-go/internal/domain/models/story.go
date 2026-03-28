package models

// Story represents an AMP story
type Story struct {
	ID                 int     `json:"id"`
	Name               string  `json:"name"`
	Description        string  `json:"description"`
	AcceptanceCriteria string  `json:"acceptance_criteria"`
	ProjectID          int     `json:"project_id"`
	EpicID             int     `json:"epic_id"`
	State              string  `json:"state"`
	IsReady            bool    `json:"is_ready"`
	TaskCount          int     `json:"task_count"`
	ProgressPercentage float64 `json:"progress_percentage"`
	TaskIDs            []int   `json:"task_ids"`
	DependencyIDs      []int   `json:"dependency_ids"`
	BlockedIDs         []int   `json:"blocked_ids"`
	Priority           string  `json:"priority"`
	PlannedHours       float64 `json:"planned_hours"`
}

// CreateStoryRequest represents the request to create a story
type CreateStoryRequest struct {
	Name               string  `json:"name"`
	ProjectID          int     `json:"project_id"`
	EpicID             int     `json:"epic_id"`
	Description        string  `json:"description,omitempty"`
	AcceptanceCriteria string  `json:"acceptance_criteria,omitempty"`
	Priority           string  `json:"priority,omitempty"`
	PlannedHours       float64 `json:"planned_hours,omitempty"`
}
