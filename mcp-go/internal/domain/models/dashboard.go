package models

// Dashboard represents project dashboard data
type Dashboard struct {
	ProjectID          int           `json:"project_id"`
	ProjectName        string        `json:"project_name"`
	EpicCount          int           `json:"epic_count"`
	StoryCount         int           `json:"story_count"`
	TaskCount          int           `json:"task_count"`
	CompletedTaskCount int           `json:"completed_task_count"`
	BlockedTaskCount   int           `json:"blocked_task_count"`
	ReadyTaskCount     int           `json:"ready_task_count"`
	InProgressCount    int           `json:"in_progress_count"`
	ProgressPercentage float64       `json:"progress_percentage"`
	ActiveAgents       []string      `json:"active_agents"`
	RecentActivity     []Activity    `json:"recent_activity"`
	Epics              []EpicSummary `json:"epics"`
}

// Activity represents a dashboard activity entry
type Activity struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
	AgentID   string `json:"agent_id"`
}

// EpicSummary represents epic summary for dashboard
type EpicSummary struct {
	ID                 int     `json:"id"`
	Name               string  `json:"name"`
	State              string  `json:"state"`
	ProgressPercentage float64 `json:"progress_percentage"`
	StoryCount         int     `json:"story_count"`
	TaskCount          int     `json:"task_count"`
}

// AMPContext represents the .amp.json file structure
type AMPContext struct {
	Version        string                 `json:"version"`
	ProjectID      int                    `json:"project_id"`
	ProjectCode    string                 `json:"project_code"`
	ProjectName    string                 `json:"project_name"`
	CreatedAt      string                 `json:"created_at"`
	LastSession    string                 `json:"last_session"`
	CurrentEpicID  *int                   `json:"current_epic_id,omitempty"`
	CurrentStoryID *int                   `json:"current_story_id,omitempty"`
	ActiveTaskIDs  []int                  `json:"active_task_ids"`
	KBEntryIDs     []int                  `json:"kb_entry_ids"`
	Context        map[string]interface{} `json:"context"`
}

// ValidateContextRequest represents the request to validate context
type ValidateContextRequest struct {
	Context AMPContext `json:"context"`
}
