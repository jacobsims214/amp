package domain

// Private attachment download grants (epic 202, story 549, AMP #3963; approved
// product contract artifact 36 v2 §1 and decision D3; build slice G3 of
// discoveries/team-inbox-approved-build-slices.md — the serving half; the
// storage half is #3955 and the upload half is #3961).
//
// The contract's rule is one sentence: an attachment's bytes are reached "only
// via short-lived signed URLs, always as an attachment". This file is the
// vocabulary that makes it true, and it is deliberately just that — a record
// and a format, not a signer and not a store. Where the signature is computed
// is the service's (internal/service/request_downloads.go) and how a grant row
// is read and consumed is the repository's
// (internal/repository/request_download_grants.go).
//
// Two properties are worth stating here because they are the reason the shape
// is what it is:
//
//  1. **A grant names exactly one attachment and one request, in one tenant.**
//     It is a capability for one file, not for a request's files, not for a
//     team's requests and never a prefix or a wildcard: there is no field that
//     could express "everything on request 7" and no way to derive one
//     attachment's grant from another's. A caller that holds grant A learns
//     nothing that lets it name B.
//  2. **The grant is a bearer credential, so it is stored as a hash and its
//     value never appears twice.** The token that goes in the URL is random and
//     only its sha256 reaches the database (the api_tokens and receipt-upload
//     posture); it is never put in an audit row, a log line, an event payload
//     or a summary. A grant is also *bound to a person* by the stored row, so
//     possession alone is not enough: the serve path re-checks the person on
//     every use (the ticket's "auth recheck/revocation").

import "time"

// RequestDownloadGrant is one issued download grant: the row behind a signed
// URL. It is the unit of expiry, of single-use and of revocation.
//
// The grant is *not* an authorization decision: it records that some caller
// was allowed to read one attachment at one moment, and the serve path decides
// again whether they still are (service.RequestDownloadGrantView). A grant can
// therefore go stale safely — losing team membership, a request moving to a
// different requester or a file becoming held all refuse the download without
// touching this row.
type RequestDownloadGrant struct {
	ID int64 `json:"id"`
	// GrantHash is the sha256 (lower-case hex) of the token the caller holds.
	// It is the row's identity: the token itself is never stored, so a database
	// dump cannot be replayed as a download.
	GrantHash string `json:"-"`
	// The one file, its request and their tenant. All three are stored so the
	// serve path can refuse a mismatched pair without trusting the URL's own
	// numbers, and so a tenant's grants can be found and revoked.
	AttachmentID int64 `json:"attachment_id"`
	RequestID    int   `json:"request_id"`
	OrgID        int   `json:"org_id"`
	TeamID       int   `json:"team_id"`
	// UserID is who the grant was issued to. It is required: a download without
	// a person is a public URL, which is exactly what the contract forbids
	// ("do not bypass the auth proxy by inventing public URLs").
	UserID int `json:"user_id"`
	// IssuedBy is the human who minted it, when that is not the same person
	// (a team member minting for the requester). Nullable for a departed user.
	IssuedBy  *int      `json:"issued_by,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// UsedAt is when the grant was spent. A spent grant is refused even before
	// it expires: single-use keeps a URL that leaked through a referer, a log or
	// a chat message to one download.
	UsedAt *time.Time `json:"used_at,omitempty"`
}

// Active reports whether the grant may still be spent at now: unspent and
// within its window. The serve path calls this *in addition to* the database's
// atomic consume, never instead of it — the atomic check is what makes two
// concurrent downloads of one URL yield one body.
func (g RequestDownloadGrant) Active(now time.Time) bool {
	return g.UsedAt == nil && now.Before(g.ExpiresAt)
}

// Grant fields. The token is minted by the service; these are the bounds the
// surface quotes and the audit row records.
const (
	// RequestDownloadGrantTTL is how long a signed download URL is valid. Five
	// minutes: long enough for a browser to follow a redirect and a person to
	// click, short enough that a URL pasted into a chat is dead before it is
	// useful. It is deliberately not configurable per request — a longer grant
	// is not a feature, it is a leak waiting to happen.
	RequestDownloadGrantTTL = 5 * time.Minute
)

// RequestDownloadGrantRef names one grant's identity for a revoke: the person,
// the attachment, or the request. Callers pass at most one; the zero value
// revokes nothing (rather than everything), so a forgotten filter cannot empty
// the table.
type RequestDownloadGrantRef struct {
	UserID       int
	AttachmentID int64
	RequestID    int
	// OrgID and TeamID scope the narrowing forms to one tenant, so a revoke can
	// never reach another org's grants even with a guessed attachment id.
	OrgID  int
	TeamID int
}

// Valid reports whether the reference names something to revoke. A reference
// with no selector is refused by the service rather than answered with a
// full-table delete.
func (ref RequestDownloadGrantRef) Valid() bool {
	return ref.UserID > 0 || ref.AttachmentID > 0 || ref.RequestID > 0 || ref.OrgID > 0
}
