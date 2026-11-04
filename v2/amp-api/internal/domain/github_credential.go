package domain

import "time"

// GitHubCredential is a project's GitHub API credential as anything outside
// the storage layer sees it (AMP #3154): which host it is for, its label, who
// attached it and when it was attached or rotated.
//
// The token is deliberately not a field here. It is stored sealed
// (internal/repository/github_credentials.go) and opened only by the facts
// reader; there is no struct that carries it back to a caller, so "don't
// return the token" can't be forgotten in a handler.
type GitHubCredential struct {
	ID        int        `json:"id"`
	ProjectID int        `json:"project_id"`
	Host      string     `json:"host"`
	Label     string     `json:"label"`
	CreatedBy *int       `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at,omitempty"`
}

// GitHubCredentialInfo is a project's credential state as a settings surface
// would show it: whether one is attached, its metadata, and whether this
// server can hold one at all (it needs an encryption key). Attaching is the
// only way a token enters AMP; it is never shown again.
type GitHubCredentialInfo struct {
	Configured bool              `json:"configured"`
	Host       string            `json:"host"`
	Credential *GitHubCredential `json:"credential,omitempty"`
	// Encrypted says whether this server has a key to seal a token with.
	// Without one, attaching and rotating are refused rather than stored in
	// plaintext.
	Encrypted bool `json:"encrypted"`
}
