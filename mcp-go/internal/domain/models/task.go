package models

// Task represents an AMP task
type Task struct {
	ID                 int     `json:"id"`
	Name               string  `json:"name"`
	Description        string  `json:"description"`
	DescriptionText    string  `json:"description_text"`
	AcceptanceCriteria string  `json:"acceptance_criteria"`
	ProjectID          int     `json:"project_id"`
	EpicID             int     `json:"epic_id"`
	StoryID            int     `json:"story_id"`
	State              string  `json:"state"`
	IsReady            bool    `json:"is_ready"`
	DAGLevel           int     `json:"dag_level"`
	DAGCriticalPath    bool    `json:"dag_critical_path"`
	Priority           string  `json:"priority"`
	PlannedHours       float64 `json:"planned_hours"`
	ActualHours        float64 `json:"actual_hours"`
	AgentID            string  `json:"agent_id"`
	AgentSession       string  `json:"agent_session"`
	DispatchTime       string  `json:"dispatch_time"`
	CompletionTime     string  `json:"completion_time"`
	ContextData        string  `json:"context_data"`
	DependencyIDs      []int   `json:"dependency_ids"`
	BlockedIDs         []int   `json:"blocked_ids"`
	DependencyCount    int     `json:"dependency_count"`
	BlockedCount       int     `json:"blocked_count"`
}

// CreateTaskRequest represents the request to create a task
type CreateTaskRequest struct {
	Name               string  `json:"name"`
	ProjectID          int     `json:"project_id"`
	EpicID             *int    `json:"epic_id,omitempty"`
	StoryID            *int    `json:"story_id,omitempty"`
	Description        string  `json:"description,omitempty"`
	AcceptanceCriteria string  `json:"acceptance_criteria,omitempty"`
	Priority           string  `json:"priority,omitempty"`
	PlannedHours       float64 `json:"planned_hours,omitempty"`
	DependencyIDs      []int   `json:"dependency_ids,omitempty"`
	DAGLevel           int     `json:"dag_level,omitempty"`
}

// UpdateTaskRequest represents the request to update a task
type UpdateTaskRequest struct {
	TaskID      int    `json:"task_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	ContextData string `json:"context_data,omitempty"`
}

// DispatchTaskRequest represents the request to dispatch a task
type DispatchTaskRequest struct {
	TaskID  int    `json:"task_id"`
	AgentID string `json:"agent_id"`
}

// AddCommentRequest represents the request to add a comment
type AddCommentRequest struct {
	TaskID int    `json:"task_id"`
	Body   string `json:"body"`
}
