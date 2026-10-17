package domain

import "time"

// Run outcomes.
const (
	RunCompleted = "completed"
	RunBlocked   = "blocked"
	RunProgress  = "progress"
	RunAbandoned = "abandoned"
)

// Run is one agent run on a ticket: the model it used, what it spent (when
// its harness could report it), and where its work went.
type Run struct {
	ID               int64     `json:"id"`
	TaskID           int       `json:"task_id"`
	ProjectID        int       `json:"project_id"`
	UserID           *int      `json:"user_id,omitempty"` // whose token the agent ran under
	AgentID          string    `json:"agent_id,omitempty"`
	Outcome          string    `json:"outcome"`
	Model            string    `json:"model,omitempty"`
	InputTokens      *int64    `json:"input_tokens,omitempty"`
	OutputTokens     *int64    `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64    `json:"cache_write_tokens,omitempty"`
	CostUSD          *float64  `json:"cost_usd,omitempty"`
	Branch           string    `json:"branch,omitempty"`
	Commit           string    `json:"commit,omitempty"`
	PRURL            string    `json:"pr_url,omitempty"`
	Pushed           *bool     `json:"pushed,omitempty"`
	Note             string    `json:"note,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// HasDetail reports whether a run carries anything beyond its outcome, so
// callers only record runs that tell us something.
func (r Run) HasDetail() bool {
	return r.Model != "" || r.InputTokens != nil || r.OutputTokens != nil || r.CostUSD != nil ||
		r.Branch != "" || r.Commit != "" || r.PRURL != "" || r.Pushed != nil || r.Note != ""
}

// Usage totals runs' token spend.
type Usage struct {
	Runs             int      `json:"runs"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	CostUSD          float64  `json:"cost_usd"`
	Unreported       int      `json:"unreported"` // runs with no token counts
	Models           []string `json:"models,omitempty"`
}

// Add counts one run.
func (u *Usage) Add(r Run) {
	u.Runs++
	if r.InputTokens == nil && r.OutputTokens == nil {
		u.Unreported++
	}
	if r.InputTokens != nil {
		u.InputTokens += *r.InputTokens
	}
	if r.OutputTokens != nil {
		u.OutputTokens += *r.OutputTokens
	}
	if r.CacheReadTokens != nil {
		u.CacheReadTokens += *r.CacheReadTokens
	}
	if r.CacheWriteTokens != nil {
		u.CacheWriteTokens += *r.CacheWriteTokens
	}
	if r.CostUSD != nil {
		u.CostUSD += *r.CostUSD
	}
	if r.Model != "" {
		for _, m := range u.Models {
			if m == r.Model {
				return
			}
		}
		u.Models = append(u.Models, r.Model)
	}
}
