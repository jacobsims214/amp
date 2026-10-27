package domain

import "time"

// GitHubHook is a project's inbound GitHub webhook (AMP #2479): GitHub
// posts pull_request events to /api/hooks/github/{project_id}, signed with
// the project's secret. The secret is never part of this record; it is
// returned once, when it is generated.
type GitHubHook struct {
	ProjectID      int        `json:"project_id"`
	CreatedBy      *int       `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	RotatedAt      time.Time  `json:"rotated_at"`
	RejectedCount  int64      `json:"rejected_count"`
	LastRejectedAt *time.Time `json:"last_rejected_at,omitempty"`
}

// GitHubHookDelivery is one signed delivery GitHub made to a project's hook.
type GitHubHookDelivery struct {
	DeliveryID string    `json:"delivery_id"`
	Event      string    `json:"event"`
	Action     string    `json:"action,omitempty"`
	Status     int       `json:"status"`
	Result     string    `json:"result"`
	TaskIDs    []int     `json:"task_ids"`
	ReceivedAt time.Time `json:"received_at"`
}
