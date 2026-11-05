package domain

import "time"

// UpstreamDecision is what a person decided on a completed human ticket that
// another ticket depends on. It reaches the dependent's agent (MCP
// upstream_decisions) and the dependent's drawer, with no extra call. See
// docs/specs/human-decision-docs.md.
type UpstreamDecision struct {
	TaskID   int       `json:"task_id"`
	Name     string    `json:"name"`
	Reason   string    `json:"human_reason"`
	Decision string    `json:"decision"`         // the note, or option + decided text
	Option   string    `json:"option,omitempty"` // chosen option label (doc tickets)
	DocURL   string    `json:"doc_url,omitempty"`
	Version  int       `json:"version,omitempty"`
	By       *int      `json:"decided_by"`
	ByName   string    `json:"decided_by_name"`
	At       time.Time `json:"decided_at"`
}
