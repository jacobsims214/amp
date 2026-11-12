package repository

// Team requests: the store for attachment download grants (epic 202, story 549,
// AMP #3963; approved product contract artifact 36 v2 §1 and decision D3; build
// slice G3 of discoveries/team-inbox-approved-build-slices.md).
//
// Migration 063 is the table and domain/request_download_grant.go is the record.
// This file is the only read and write path to request_download_grants, and it
// implements what the schema left to Go:
//
//  1. **Single use is an atomic claim, not a read then a write.** ConsumeRequest-
//     DownloadGrant is one UPDATE whose WHERE clause re-states every condition the
//     decide step saw (`used_at IS NULL`, `expires_at > NOW()`), so two browsers
//     following the same URL at the same instant produce exactly one body: the
//     loser's UPDATE matches no row and it is told the grant is gone. A read-then-
//     write here would be the classic double-spend, and the window would be as wide
//     as the handler's slowest step (loading 25 MiB of bytes).
//
//  2. **A grant is looked up by the hash of its token, never by its id.** The id is
//     an internal sequence; the token is 32 random bytes. A URL therefore carries
//     nothing a client could walk, and the database never holds the token itself —
//     only its sha256 (the api_tokens posture).
//
//  3. **Nothing here decides whether the bytes may be served.** The scan gate, the
//     hold, the expiry and the tenant check are the service's (G3): this file returns
//     the grant row and says nothing about it being usable. A layer that could answer
//     "serve it" would be a second gate.
//
// What this file deliberately does NOT do: mint tokens (the service does — this
// layer never sees one, only its hash), log, audit, or return a URL fragment. There
// is no `net/http` here either (a guard test asserts the import list), so no
// disposition header can be set three layers below the handler that owes it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// requestDownloadGrantCols is the full row, in 063's order.
const requestDownloadGrantCols = `id, grant_hash, attachment_id, request_id, org_id, team_id,
	user_id, issued_by, issued_at, expires_at, used_at`

// scanRequestDownloadGrant scans one row into g.
//
// 063's user_id is nullable only because a user deletion nulls it; the record keeps
// it as a plain int and reads "no live person" as 0, which matches no authenticated
// principal (the serve path compares the caller's user id against it). A sentinel
// like -1 would be a value the comparisons would then have to know about.
func scanRequestDownloadGrant(row rowScanner, g *domain.RequestDownloadGrant) error {
	var userID *int
	if err := row.Scan(&g.ID, &g.GrantHash, &g.AttachmentID, &g.RequestID, &g.OrgID, &g.TeamID,
		&userID, &g.IssuedBy, &g.IssuedAt, &g.ExpiresAt, &g.UsedAt); err != nil {
		return err
	}
	if userID != nil {
		g.UserID = *userID
	} else {
		g.UserID = 0
	}
	return nil
}

// InsertRequestDownloadGrant stores one minted grant and returns the row it wrote
// (which is the authority for its expiry: the caller's copy is advisory).
//
// The insert refuses the shapes that would make a grant broader than the contract
// allows — no person, no attachment, no request — as a caller bug rather than
// letting the table's NOT NULL answer with a 23502 that reads like an outage.
func (r *Repo) InsertRequestDownloadGrant(ctx context.Context, g domain.RequestDownloadGrant) (*domain.RequestDownloadGrant, error) {
	if g.UserID <= 0 {
		return nil, fmt.Errorf("a download grant needs the person it was issued to (user_id), never a public URL")
	}
	if g.AttachmentID <= 0 || g.RequestID <= 0 {
		return nil, fmt.Errorf("a download grant names exactly one attachment (%d) on one request (%d)", g.AttachmentID, g.RequestID)
	}
	if g.OrgID <= 0 || g.TeamID <= 0 {
		return nil, ErrRequestTenantScope
	}
	if !validSHA256Hex(g.GrantHash) {
		return nil, fmt.Errorf("a download grant's stored form is its sha256 (64 lower-case hex characters)")
	}
	if !g.ExpiresAt.After(g.IssuedAt) {
		return nil, fmt.Errorf("a download grant expires after it is issued (issued %s, expires %s)", g.IssuedAt, g.ExpiresAt)
	}
	out := &domain.RequestDownloadGrant{}
	err := scanRequestDownloadGrant(r.db.QueryRow(ctx, `INSERT INTO request_download_grants
		    (grant_hash, attachment_id, request_id, org_id, team_id, user_id, issued_by, issued_at, expires_at)
		  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		  RETURNING `+requestDownloadGrantCols,
		g.GrantHash, g.AttachmentID, g.RequestID, g.OrgID, g.TeamID, g.UserID, g.IssuedBy, g.IssuedAt, g.ExpiresAt), out)
	if err != nil {
		return nil, fmt.Errorf("store download grant for attachment %d: %w", g.AttachmentID, err)
	}
	return out, nil
}

// GetRequestDownloadGrant finds one grant by its token's hash. An unknown hash, a
// spent grant and an expired one are all returned as the row (with UsedAt/ExpiresAt
// set) rather than as three errors: the serve path's answer for all three is the
// same 404, and telling them apart here would invite a caller to.
//
// A malformed hash is an ordinary not-found, not a bug: it can only come from a URL
// somebody typed.
func (r *Repo) GetRequestDownloadGrant(ctx context.Context, grantHash string) (*domain.RequestDownloadGrant, error) {
	if !validSHA256Hex(grantHash) {
		return nil, ErrNotFound
	}
	g := &domain.RequestDownloadGrant{}
	err := scanRequestDownloadGrant(r.db.QueryRow(ctx, `SELECT `+requestDownloadGrantCols+`
		  FROM request_download_grants WHERE grant_hash = $1`, grantHash), g)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load download grant: %w", err)
	}
	return g, nil
}

// ConsumeRequestDownloadGrant spends a grant atomically and returns the row it
// spent. `now` is the caller's clock, not the database's: the gate decides expiry with
// the service's clock (so a test can drive it, and one instant decides the whole call),
// and an atomic guard measured against a different clock would be able to disagree with
// the decision that let the request this far. Passing the instant in keeps exactly one
// answer to "has this expired". Two callers racing on one URL: exactly one gets the row back, the other
// gets ErrNotFound (the same answer as an unknown, spent or expired grant — there
// is nothing useful to distinguish, and a distinct "already used" would tell a
// stranger their guess was once valid).
//
// The WHERE clause repeats the conditions rather than trusting the decide step's
// read: between that read and this UPDATE the row could have been spent, and
// checking atomically is what makes single-use true under concurrency rather than
// true when nothing else happens.
func (r *Repo) ConsumeRequestDownloadGrant(ctx context.Context, grantHash string, now time.Time) (*domain.RequestDownloadGrant, error) {
	if !validSHA256Hex(grantHash) {
		return nil, ErrNotFound
	}
	g := &domain.RequestDownloadGrant{}
	err := scanRequestDownloadGrant(r.db.QueryRow(ctx, `UPDATE request_download_grants
		   SET used_at = $2
		 WHERE grant_hash = $1
		   AND used_at IS NULL
		   AND expires_at > $2
		RETURNING `+requestDownloadGrantCols, grantHash, now), g)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("consume download grant: %w", err)
	}
	return g, nil
}

// RevokeRequestDownloadGrants deletes the unspent grants a reference names and
// reports how many it removed. Deleting is the revocation: the serve path looks the
// hash up on every use, so a row that is not there is a 404 on the very next click
// with no cache to invalidate.
//
// A narrow reference (a person, an attachment, a request) must carry its tenant, so
// a revoke can never reach another org's grants with a guessed id. A bare org-wide
// revoke is the deliberate act of taking away an org's live URLs and names only
// OrgID.
func (r *Repo) RevokeRequestDownloadGrants(ctx context.Context, ref domain.RequestDownloadGrantRef) (int, error) {
	if !ref.Valid() {
		return 0, fmt.Errorf("a grant revoke must name a person, an attachment, a request or an org")
	}
	if (ref.AttachmentID > 0 || ref.RequestID > 0 || ref.UserID > 0) && (ref.OrgID <= 0 || ref.TeamID <= 0) {
		return 0, ErrRequestTenantScope
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM request_download_grants
		 WHERE used_at IS NULL
		   AND ($1 = 0 OR user_id = $1)
		   AND ($2 = 0 OR attachment_id = $2)
		   AND ($3 = 0 OR request_id = $3)
		   AND ($4 = 0 OR org_id = $4)
		   AND ($5 = 0 OR team_id = $5)`,
		ref.UserID, ref.AttachmentID, ref.RequestID, ref.OrgID, ref.TeamID)
	if err != nil {
		return 0, fmt.Errorf("revoke download grants: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DeleteExpiredRequestDownloadGrants removes grants past their window, spent or not.
// It is the ledger's only housekeeping: a grant that cannot be found is exactly as
// refused as one that has expired, so deleting them is a tidiness job and never a
// security step. The attachment purge (#3964) can run it alongside its own sweep.
func (r *Repo) DeleteExpiredRequestDownloadGrants(ctx context.Context, before time.Time) (int, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM request_download_grants WHERE expires_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired download grants: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// validSHA256Hex reports whether s is a lower-case hex sha256 — the only form a grant
// hash is stored in. Anything else cannot name a row, so it is refused before a query.
func validSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
