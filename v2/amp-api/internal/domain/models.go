package domain

import "time"

// ---- Enums ----

type TaskState string

const (
	TaskStateBacklog    TaskState = "backlog"
	TaskStateInProgress TaskState = "in_progress"
	TaskStateCompleted  TaskState = "completed"
	TaskStateBlocked    TaskState = "blocked"
)

type ProjectState string

const (
	ProjectStateActive   ProjectState = "active"
	ProjectStateArchived ProjectState = "archived"
)

type EpicState string

const (
	EpicStateBacklog    EpicState = "backlog"
	EpicStateInProgress EpicState = "in_progress"
	EpicStateCompleted  EpicState = "completed"
	EpicStateBlocked    EpicState = "blocked"
)

type StoryState string

const (
	StoryStateBacklog    StoryState = "backlog"
	StoryStateInProgress StoryState = "in_progress"
	StoryStateCompleted  StoryState = "completed"
	StoryStateBlocked    StoryState = "blocked"
)

// ---- Core models ----

type Project struct {
	ID          int          `json:"id"`
	Name        string       `json:"name"`
	Code        string       `json:"code"`
	Description string       `json:"description"`
	State       ProjectState `json:"state"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

type Epic struct {
	ID          int       `json:"id"`
	ProjectID   int       `json:"project_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	State       EpicState `json:"state"`
	Priority    string    `json:"priority"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Story struct {
	ID                 int        `json:"id"`
	ProjectID          int        `json:"project_id"`
	EpicID             int        `json:"epic_id"`
	Name               string     `json:"name"`
	Description        string     `json:"description"`
	AcceptanceCriteria string     `json:"acceptance_criteria"`
	State              StoryState `json:"state"`
	Priority           string     `json:"priority"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type Task struct {
	ID                 int       `json:"id"`
	ProjectID          int       `json:"project_id"`
	EpicID             int       `json:"epic_id"`  // required — every task belongs to an epic
	StoryID            int       `json:"story_id"` // required — every task belongs to a story
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	AcceptanceCriteria string    `json:"acceptance_criteria"`
	State              TaskState `json:"state"`
	Priority           string    `json:"priority"`
	// DependencyIDs are the task IDs that must complete before this task can run.
	// The actor derives state: empty → backlog, any incomplete dep → blocked.
	// Agents never set state directly on create.
	DependencyIDs []int `json:"dependency_ids"`
	// BlockedByIDs is the subset of DependencyIDs not yet completed.
	// Computed on every read — never stored.
	BlockedByIDs []int `json:"blocked_by_ids,omitempty"`
	// AssignedTo is set by the manager at planning time — who should work this ticket.
	// This is free text (e.g. "amp-worker", "amp-worker-frontend").
	// It is shown on the board before dispatch so the user can review and correct it.
	AssignedTo string `json:"assigned_to,omitempty"`
	// AgentID is set at dispatch time by the actor — who is actually working it now.
	AgentID      string     `json:"agent_id,omitempty"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	BlockReason  string     `json:"block_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Comment struct {
	ID        int       `json:"id"`
	TaskID    int       `json:"task_id"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- Request types ----

type CreateProjectRequest struct {
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
}

type CreateEpicRequest struct {
	ProjectID   int    `json:"project_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
}

type CreateStoryRequest struct {
	ProjectID          int    `json:"project_id"`
	EpicID             int    `json:"epic_id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Priority           string `json:"priority"`
}

type CreateTaskRequest struct {
	ProjectID          int    `json:"project_id"`
	EpicID             int    `json:"epic_id"`  // required
	StoryID            int    `json:"story_id"` // required
	Name               string `json:"name"`
	Description        string `json:"description"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Priority           string `json:"priority"`
	// AssignedTo: set at planning time by the manager. Who should work this.
	AssignedTo string `json:"assigned_to,omitempty"`
	// DependencyIDs: task IDs that must complete before this one runs.
	// State (backlog vs blocked) is derived by the actor — agent does not set it.
	DependencyIDs []int `json:"dependency_ids"`
}

type UpdateTaskRequest struct {
	TaskID      int    `json:"task_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	AssignedTo  string `json:"assigned_to,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
}

type DispatchTaskRequest struct {
	TaskID  int    `json:"task_id"`
	AgentID string `json:"agent_id"`
}

type AddCommentRequest struct {
	TaskID int    `json:"task_id"`
	Body   string `json:"body"`
	Author string `json:"author"`
}

// ActivityLog is the immutable audit trail for a task.
// Every state transition and comment produces an entry.
// This is what amp_get_ticket_history returns — the full story of a ticket.
type ActivityLog struct {
	ID        int       `json:"id"`
	TaskID    int       `json:"task_id"`
	ProjectID int       `json:"project_id"`
	Actor     string    `json:"actor"`  // "manager", "amp-worker", "system", agent ID
	Action    string    `json:"action"` // "created", "dispatched", "completed", "blocked", "comment", "state_change"
	FromState string    `json:"from_state,omitempty"`
	ToState   string    `json:"to_state,omitempty"`
	Detail    string    `json:"detail,omitempty"` // human-readable summary or comment body
	CreatedAt time.Time `json:"created_at"`
}

// ---- Events (broadcast to SSE clients) ----

type EventType string

const (
	EventProjectCreated    EventType = "project.created"
	EventEpicCreated       EventType = "epic.created"
	EventEpicStateChanged  EventType = "epic.state_changed"
	EventStoryCreated      EventType = "story.created"
	EventStoryStateChanged EventType = "story.state_changed"
	EventTaskCreated       EventType = "task.created"
	EventTaskUpdated       EventType = "task.updated"
	EventTaskDispatched    EventType = "task.dispatched"
	EventTaskCompleted     EventType = "task.completed"
	EventTaskBlocked       EventType = "task.blocked"
	EventTaskUnblocked     EventType = "task.unblocked"
	EventCommentAdded      EventType = "comment.added"
)

type Event struct {
	Type      EventType   `json:"type"`
	ProjectID int         `json:"project_id"`
	Payload   interface{} `json:"payload"`
	At        time.Time   `json:"at"`
}
