package models

// Epic represents an AMP epic
type Epic struct {
	ID                 int     `json:"id"`
	Name               string  `json:"name"`
	Description        string  `json:"description"`
	ProjectID          int     `json:"project_id"`
	State              string  `json:"state"`
	StoryCount         int     `json:"story_count"`
	TaskCount          int     `json:"task_count"`
	ProgressPercentage float64 `json:"progress_percentage"`
	StoryIDs           []int   `json:"story_ids"`
	Priority           string  `json:"priority"`
}

// CreateEpicRequest represents the request to create an epic
type CreateEpicRequest struct {
	Name        string `json:"name"`
	ProjectID   int    `json:"project_id"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty"`
}

// UpdateDAGRequest represents the request to update epic DAG
type UpdateDAGRequest struct {
	EpicID  int    `json:"epic_id"`
}
