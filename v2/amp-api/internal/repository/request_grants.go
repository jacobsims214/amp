package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Team submit grants ----
//
// A submit grant lets one person submit requests to one team's inbox without
// joining that team (and therefore without reaching any of the team's
// projects). Migration 059 (team_submit_grants) holds the rows; the type is
// domain.TeamSubmitGrant.
//
// The three properties the storage layer is responsible for, and that these
// functions are written to keep:
//
//  1. Granting never creates membership. No function here touches
//     org_members or team_members. A customer with a grant is still exactly
//     a customer, with zero project visibility — the contract's separation of
//     "may submit" from "may see the project".
//  2. A duplicate grant is idempotent. The primary key makes a second row for
//     one (team, user) unstorable, and GrantTeamSubmit keeps the original
//     grantor and time when the grant is already in force, so a replayed grant
//     changes nothing observable. Reviving a revoked grant records the new
//     grantor and time, because that is the access now in force.
//  3. Revocation is effective on the next read. RevokeTeamSubmit is one
//     UPDATE over at most one row, and every read filters revoked_at IS NULL,
//     so there is no cache to invalidate and no delete-then-insert window in
//     which a stale allow-set and the table could disagree.
//
// Who may call these is not decided here: the authorization gate
// (RequireTeamManager: a team lead or an org owner/admin, D14) and the audit
// rows belong to the service that wraps them (AMP #3769). This layer refuses
// what the database refuses — a missing team, a team from another org, a
// missing user — so a caller can't write a grant that names a nonexistent or
// cross-tenant row.

const teamSubmitGrantCols = `team_id, org_id, user_id, granted_by, granted_at, revoked_by, revoked_at`

func scanTeamSubmitGrant(row rowScanner, g *domain.TeamSubmitGrant) error {
	return row.Scan(&g.TeamID, &g.OrgID, &g.UserID, &g.GrantedBy, &g.GrantedAt, &g.RevokedBy, &g.RevokedAt)
}

// GrantTeamSubmit gives userID submit access to teamID, granted by grantedBy
// (nil when there is no attributable actor, e.g. a local-dev session).
//
// It is an upsert so a replay is harmless, and the DO UPDATE is deliberately
// conditional: while the grant is already in force the original granted_by and
// granted_at are kept (a replayed grant is not a new grant), and a revoked
// grant is revived with the new grantor and time (the access now in force is
// the one this call created). The row is never deleted, so a reader never sees
// the pair absent in between.
//
// The team must exist and belong to orgID; both are checked by the composite
// foreign key, and a violation comes back as ErrNotFound (there is no such
// team in this org) or ErrConflict (the team exists in another org). Nothing
// is written when the team is wrong, so a cross-tenant grant is impossible
// rather than merely unusual.
func (r *Repo) GrantTeamSubmit(ctx context.Context, teamID, orgID, userID int, grantedBy *int) (*domain.TeamSubmitGrant, error) {
	// Resolve the team first so a cross-org attempt is a clear refusal rather
	// than a foreign-key message, and so a caller can't create a grant on a
	// team that isn't there. One indexed lookup by primary key.
	var teamOrg int
	err := r.db.QueryRow(ctx, `SELECT org_id FROM teams WHERE id = $1`, teamID).Scan(&teamOrg)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("team %d: %w", teamID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if teamOrg != orgID {
		return nil, conflictf("team %d belongs to org %d, not %d", teamID, teamOrg, orgID)
	}

	g := &domain.TeamSubmitGrant{TeamID: teamID, OrgID: orgID, UserID: userID, GrantedBy: grantedBy}
	err = r.db.QueryRow(ctx, `
		INSERT INTO team_submit_grants (team_id, org_id, user_id, granted_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (team_id, user_id) DO UPDATE SET
		  org_id     = EXCLUDED.org_id,
		  granted_by = CASE WHEN team_submit_grants.revoked_at IS NULL
		                    THEN team_submit_grants.granted_by ELSE EXCLUDED.granted_by END,
		  granted_at = CASE WHEN team_submit_grants.revoked_at IS NULL
		                    THEN team_submit_grants.granted_at ELSE NOW() END,
		  revoked_by = NULL,
		  revoked_at = NULL
		RETURNING `+teamSubmitGrantCols,
		teamID, orgID, userID, grantedBy).Scan(
		&g.TeamID, &g.OrgID, &g.UserID, &g.GrantedBy, &g.GrantedAt, &g.RevokedBy, &g.RevokedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return nil, fmt.Errorf("user %d: %w", userID, ErrNotFound)
		}
		return nil, err
	}
	return g, nil
}

// RevokeTeamSubmit removes userID's submit access to teamID, attributed to
// revokedBy. It returns the revoked grant, or ErrNotFound when the team/user
// pair has no grant at all.
//
// Revoking an already-revoked grant is a no-op that still returns the row: the
// WHERE clause matches nothing, so the original revoker and time are kept
// rather than being overwritten by whoever revoked second. One statement, so
// two concurrent revocations can't interleave between a read and a write.
func (r *Repo) RevokeTeamSubmit(ctx context.Context, teamID, userID int, revokedBy *int) (*domain.TeamSubmitGrant, error) {
	g := &domain.TeamSubmitGrant{}
	err := r.db.QueryRow(ctx, `
		UPDATE team_submit_grants SET revoked_by = $3, revoked_at = NOW()
		WHERE team_id = $1 AND user_id = $2 AND revoked_at IS NULL
		RETURNING `+teamSubmitGrantCols,
		teamID, userID, revokedBy).Scan(
		&g.TeamID, &g.OrgID, &g.UserID, &g.GrantedBy, &g.GrantedAt, &g.RevokedBy, &g.RevokedAt)
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// Nothing was in force. Distinguish "no grant at all" from "already
	// revoked", so the service can 404 the first and treat the second as the
	// no-op it is.
	existing, getErr := r.GetTeamSubmitGrant(ctx, teamID, userID)
	if getErr != nil {
		return nil, getErr
	}
	return existing, nil
}

// GetTeamSubmitGrant returns the team/user grant whether or not it is in force.
// A pair with no row is ErrNotFound.
func (r *Repo) GetTeamSubmitGrant(ctx context.Context, teamID, userID int) (*domain.TeamSubmitGrant, error) {
	g := &domain.TeamSubmitGrant{}
	err := scanTeamSubmitGrant(r.db.QueryRow(ctx,
		`SELECT `+teamSubmitGrantCols+` FROM team_submit_grants WHERE team_id = $1 AND user_id = $2`,
		teamID, userID), g)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("team %d submit grant for user %d: %w", teamID, userID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// HasTeamSubmitGrant reports whether userID may submit to teamID right now.
//
// This is the read the submit capability makes on every call (slice B3), and
// it answers from the table, never from a cached allow-set: a revoked grant
// fails here on the very next call with nothing to reset. It is a single
// indexed lookup on the primary key.
func (r *Repo) HasTeamSubmitGrant(ctx context.Context, teamID, userID int) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM team_submit_grants
		               WHERE team_id = $1 AND user_id = $2 AND revoked_at IS NULL)`,
		teamID, userID).Scan(&ok)
	return ok, err
}

// TeamSubmitGrantTeams returns the ids of every team userID may submit to
// right now. Used to open the requester's inbox list; revoked grants are never
// in the answer.
func (r *Repo) TeamSubmitGrantTeams(ctx context.Context, userID int) ([]int, error) {
	rows, err := r.db.Query(ctx, `
		SELECT team_id FROM team_submit_grants
		WHERE user_id = $1 AND revoked_at IS NULL ORDER BY team_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListTeamSubmitGrants returns a team's submit grants, in force or revoked,
// with the recipient's identity for the administration view. Revoked grants
// are included on purpose: "who may submit" and "who was taken away" are the
// same listing to a manager, and the row is what says which is which.
func (r *Repo) ListTeamSubmitGrants(ctx context.Context, teamID int) ([]domain.TeamSubmitGrantee, error) {
	rows, err := r.db.Query(ctx, `
		SELECT g.team_id, g.org_id, g.user_id, g.granted_by, g.granted_at, g.revoked_by, g.revoked_at,
		       u.email, u.display_name, u.kind
		FROM team_submit_grants g JOIN users u ON u.id = g.user_id
		WHERE g.team_id = $1
		ORDER BY (g.revoked_at IS NOT NULL), u.email`, teamID)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, g *domain.TeamSubmitGrantee) error {
		return row.Scan(&g.TeamID, &g.OrgID, &g.UserID, &g.GrantedBy, &g.GrantedAt, &g.RevokedBy, &g.RevokedAt,
			&g.Email, &g.DisplayName, &g.Kind)
	})
}
