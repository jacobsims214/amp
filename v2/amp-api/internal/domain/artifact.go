package domain

import "time"

// Artifact kinds: what the content is, which decides how AMP shows it.
const (
	ArtifactHTML     = "html"
	ArtifactSVG      = "svg"
	ArtifactImage    = "image"
	ArtifactMarkdown = "markdown"
	ArtifactPDF      = "pdf"
	ArtifactText     = "text"
)

// Artifact review states. A new artifact, and every new version, is in
// review until a person approves it or asks for changes.
const (
	ArtifactInReview         = "in_review"
	ArtifactApproved         = "approved"
	ArtifactChangesRequested = "changes_requested"
)

// MaxArtifactBytes caps one version's content.
const MaxArtifactBytes = 10 << 20

// DefaultArtifactRetentionDays is how long an artifact lives after its last
// version unless someone keeps it.
const DefaultArtifactRetentionDays = 45

// Artifact is something an agent made for people to look at: a mockup, a
// prototype, a diagram, an image, a document. Versions hold the content.
type Artifact struct {
	ID              int        `json:"id"`
	ProjectID       int        `json:"project_id"`
	TaskID          *int       `json:"task_id"`
	TaskRef         string     `json:"task_ref,omitempty"`
	TaskName        string     `json:"task_name,omitempty"`
	Title           string     `json:"title"`
	Kind            string     `json:"kind"`
	Status          string     `json:"status"`
	CurrentVersion  int        `json:"current_version"`
	ApprovedVersion *int       `json:"approved_version"`
	ReviewedBy      *int       `json:"reviewed_by"`
	ReviewedByEmail string     `json:"reviewed_by_email,omitempty"`
	ReviewedAt      *time.Time `json:"reviewed_at"`
	CreatedBy       *int       `json:"created_by"`
	CreatedByEmail  string     `json:"created_by_email,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	// Keep is set when a person chose to keep it; otherwise it's deleted at
	// ExpiresAt (pushed back by every new version).
	Keep              bool       `json:"keep"`
	KeptBy            *int       `json:"kept_by"`
	KeptAt            *time.Time `json:"kept_at"`
	ExpiresAt         *time.Time `json:"expires_at"`
	ExpiryDismissedAt *time.Time `json:"expiry_dismissed_at"`
	// URL is where people view it in AMP (set when the server knows its address).
	URL string `json:"url,omitempty"`
}

// ArtifactVersion is one version's metadata; Content is loaded only on request.
type ArtifactVersion struct {
	ArtifactID     int       `json:"artifact_id"`
	Version        int       `json:"version"`
	ContentType    string    `json:"content_type"`
	SizeBytes      int       `json:"size_bytes"`
	SHA256         string    `json:"sha256"`
	Note           string    `json:"note"`
	CreatedBy      *int      `json:"created_by"`
	CreatedByEmail string    `json:"created_by_email,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Content        []byte    `json:"-"`
}
