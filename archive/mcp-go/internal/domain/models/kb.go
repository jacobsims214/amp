package models

// KBEntry represents a knowledge base entry
type KBEntry struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	Content        string `json:"content"`
	ContentText    string `json:"content_text"`
	EntryType      string `json:"entry_type"`
	ProjectID      int    `json:"project_id"`
	EpicID         int    `json:"epic_id"`
	StoryID        int    `json:"story_id"`
	TaskID         int    `json:"task_id"`
	CreatedByAgent string `json:"created_by_agent"`
	CreateDate     string `json:"create_date"`
	Tags           string `json:"tags"`
}

// CreateKBEntryRequest represents the request to create a KB entry
type CreateKBEntryRequest struct {
	Title          string   `json:"title"`
	Content        string   `json:"content"`
	ProjectID      int      `json:"project_id"`
	EpicID         *int     `json:"epic_id,omitempty"`
	StoryID        *int     `json:"story_id,omitempty"`
	TaskID         *int     `json:"task_id,omitempty"`
	EntryType      string   `json:"entry_type,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	CreatedByAgent string   `json:"created_by_agent,omitempty"`
}

// SearchKBRequest represents the request to search KB
type SearchKBRequest struct {
	Query     string `json:"query"`
	ProjectID *int   `json:"project_id,omitempty"`
	EntryType string `json:"entry_type,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
