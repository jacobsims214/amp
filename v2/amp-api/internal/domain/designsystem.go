package domain

import "time"

// Design system scopes.
const (
	DesignSystemOrg     = "org"
	DesignSystemProject = "project"
)

// DesignSystemPrefix is where a design system's docs live in its KB. Only a
// publish writes there.
const DesignSystemPrefix = "design-system/"

// DesignSystem is an org's design system, or a product project's layer on
// top of it.
type DesignSystem struct {
	ID              int        `json:"id"`
	Scope           string     `json:"scope"`
	OrgID           int        `json:"org_id"`
	ProjectID       *int       `json:"project_id"`        // the product project, for scope project
	SourceProjectID int        `json:"source_project_id"` // where the work happens
	Version         int        `json:"version"`           // 0 until first published
	CurrentEpicID   *int       `json:"current_epic_id"`   // the version being built, if any
	PublishedAt     *time.Time `json:"published_at"`
	PublishedBy     *int       `json:"published_by"`
	CreatedAt       time.Time  `json:"created_at"`
}

// DesignSystemVersion is one publish.
type DesignSystemVersion struct {
	Version     int                    `json:"version"`
	Changelog   string                 `json:"changelog"`
	Artifacts   []DesignSystemArtifact `json:"artifacts"`
	PublishedBy *int                   `json:"published_by"`
	PublishedAt time.Time              `json:"published_at"`
}

// DesignSystemArtifact is an approved artifact a version was published from.
type DesignSystemArtifact struct {
	Phase      string `json:"phase"`
	TaskID     int    `json:"task_id"`
	ArtifactID int    `json:"artifact_id"`
	Version    int    `json:"version"`
	Title      string `json:"title"`
	URL        string `json:"url,omitempty"`
}
