package domain

import "time"

// Team submit grants (epic 202; approved contract artifact 36 v2 §2 and
// decision D14, build slice B2). A submit grant is how a person who is not a
// member of a team may submit a request to that team's inbox — and only that.
//
// It is deliberately a relation of its own rather than a team role or a
// project level. Every project capability in AMP starts at `Level >= Member`,
// and a person reaches a project through team membership, so expressing
// "may submit" as membership would hand the requester `TaskRead` on every
// ticket in the project. The contract forbids that ("submitting is a
// capability outside the project level ladder"), so the grant is keyed to a
// team, revocable, and checked on its own by the submit capability (slice B3).
//
// Granted then revoked is a state, not a delete: the row survives so a
// re-grant can revive it and so the pair is never absent-then-present in a
// way a reader could race. Who granted and who revoked it is recorded on the
// row for the access listing; the durable history of those acts is the audit
// log, written by the service (slice B2's service half, AMP #3769).

// TeamSubmitGrant is one row of team_submit_grants: one person's submit
// access to one team, in force or (with RevokedAt set) revoked.
type TeamSubmitGrant struct {
	TeamID    int        `json:"team_id"`
	OrgID     int        `json:"org_id"`
	UserID    int        `json:"user_id"`
	GrantedBy *int       `json:"granted_by,omitempty"`
	GrantedAt time.Time  `json:"granted_at"`
	RevokedBy *int       `json:"revoked_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether the grant is in force. Every read the submit
// capability makes filters on this, which is what makes revocation effective
// on the next check with no cache to invalidate.
func (g TeamSubmitGrant) Active() bool { return g.RevokedAt == nil }

// TeamSubmitGrantee is a grant with the recipient's identity, for the
// administration listing ("who may submit to this team"). The grant's own
// team/org/user ids and attribution are unchanged; the extra fields are a
// display projection, never used to authorize anything.
type TeamSubmitGrantee struct {
	TeamSubmitGrant
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Kind        string `json:"kind,omitempty"`
}
