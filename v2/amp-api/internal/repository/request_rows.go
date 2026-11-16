package repository

// Team requests: the one row contract every read of a request shares (epic
// 202, story 535, AMP #3870; approved product contract artifact 36 v2 §1 and §2;
// build slices C1–C5 of discoveries/team-inbox-approved-build-slices.md).
//
// Migrations 053–056 added the inbox's four tables: team_requests (the record),
// request_comments/request_events (its discussion and history), request_links
// (its lineage) and request_attachments (the requester's files). This file
// holds the column list and the scanner for the RECORD itself, plus the one
// tenant-scoped lookup. The other three tables get their own files and their own
// scanners in their own slices (the comments and keyset reads in C5's follow-up,
// links in C4), so nothing here grows sideways into a general-purpose request
// data layer.
//
// Why one list and one scanner. The rest of the repository learned this the
// hard way: projectCols/taskCols exist because a column added to keep one query
// in step was forgotten in the other five, and a forgotten column reads as a
// zero value rather than as an error. So there is exactly one `requestCols`, one
// `scanRequest`, and every request read (the lookup below, the keyset list, the
// claim's SELECT ... FOR UPDATE, the create's RETURNING) is built from them.
// Later concern modules may USE this contract and must not edit it: a column
// added by a future migration is added here, once, and
// TestRequestRowColumnsMatchSchema fails until it is.
//
// No SELECT *. The list is explicit for the same reason the scans are: a `*`
// silently re-orders itself when a migration lands, and a scan target list that
// no longer matches the query then fails with "number of field descriptions
// must match number of destinations" — or, worse, succeeds with every value in
// the wrong field when the types happen to line up. Listing the columns makes
// the contract readable and lets the test compare it to the live table.
//
// Tenancy is part of the lookup, not a filter a caller remembers. Every request
// read in this package is scoped by org AND team, because a request belongs to
// exactly one of each (053's composite foreign key) and "load by id" alone is a
// cross-tenant read primitive. A row in another team — even in the same org — is
// answered exactly like a row that does not exist: repository.ErrNotFound, the
// one error a caller turns into the request surface's single 404
// (authz.RequestNotFound), so existence never leaks (contract §2, slice B4).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestTenantScope marks a request lookup made without the request's
// tenant. It is deliberately not ErrNotFound: a caller that forgot the org/team
// has a bug, and answering "not found" would hide it behind a legitimate 404
// while a caller that passed ids that simply do not match has an ordinary
// missing row. Keeping the two apart is what makes the 404 indistinguishable
// (wrong tenant, unknown id) without making a programming error invisible.
var ErrRequestTenantScope = errors.New("a request lookup needs its org and team context: pass the request's tenant, never a bare id")

// requestCols is the column list of domain.TeamRequest, in migration 053's
// order. Keep it in step with the table: TestRequestRowColumnsMatchSchema reads
// information_schema and fails when the two drift apart.
const requestCols = `id, public_ref, org_id, team_id, requester_user_id, requester_contact,
	source_kind, source_external_id, subject, input_brief, outcome_brief, state,
	claim_user_id, claimed_at, owner_user_id, duplicate_of, reject_reason,
	needs_info_since, planned_at, delivered_at, accepted_at, accepted_by,
	closed_at, reopened_at, reopened_count, rev, metadata, created_at, updated_at`

// requestSelect is what every read of a request row starts from, so no query
// can quietly use a different column set. A caller appends its own WHERE / ORDER
// BY / LIMIT (and FOR UPDATE, inside a transaction).
const requestSelect = `SELECT ` + requestCols + ` FROM team_requests`

// requestTenantWhere is the tenant predicate, in the argument order
// (requestID, orgID, teamID). It is a constant for the same reason the columns
// are: the one read of a request row that must take a lock — a later concern's
// claim, which runs `requestSelect + requestTenantWhere + " FOR UPDATE"` inside
// its transaction — reuses the predicate instead of writing it again from
// memory, so no read in the package can be scoped by only one half of the
// tenant.
const requestTenantWhere = ` WHERE id = $1 AND org_id = $2 AND team_id = $3`

// requestFields returns the scan targets for requestCols, in order. It is a
// function rather than an inline list so a second read of the same shape cannot
// drift from the first: the number of targets and the number of columns are
// checked by pgx on every scan.
func requestFields(r *domain.TeamRequest) []any {
	return []any{
		&r.ID, &r.PublicRef, &r.OrgID, &r.TeamID,
		&r.RequesterUserID, &r.RequesterContact,
		&r.SourceKind, &r.SourceExternalID,
		&r.Subject, &r.InputBrief, &r.OutcomeBrief, &r.State,
		&r.ClaimUserID, &r.ClaimedAt, &r.OwnerUserID, &r.DuplicateOf, &r.RejectReason,
		&r.NeedsInfoSince, &r.PlannedAt, &r.DeliveredAt, &r.AcceptedAt, &r.AcceptedBy,
		&r.ClosedAt, &r.ReopenedAt, &r.ReopenedCount, &r.Rev, &r.Metadata,
		&r.CreatedAt, &r.UpdatedAt,
	}
}

// scanRequest scans one row of requestSelect into r. Every column is stored,
// including the nullable ones: a NULL becomes a nil pointer rather than a zero
// value, so "no claim" and "claimed by user 0" can never be confused.
func scanRequest(row rowScanner, r *domain.TeamRequest) error {
	return row.Scan(requestFields(r)...)
}

// GetRequest loads requestID from orgID's teamID. The tenant is required: a
// request in another team — another team in the same org included — and one that
// does not exist both come back as ErrNotFound, so a handler answers one 404 for
// both and an id walker learns nothing.
//
// The returned value is the row as stored; no domain validation runs here. Reads
// are not the place to refuse a row the schema already accepted (a CHECK violation
// could only be historical corruption), and a read that normalized would hide
// that from the caller it matters to.
func (r *Repo) GetRequest(ctx context.Context, requestID, orgID, teamID int) (*domain.TeamRequest, error) {
	return getRequest(ctx, r.db, requestID, orgID, teamID)
}

// getRequest is GetRequest's body, taking the one read surface it needs.
// queryRower is the repository's existing pattern for that (auditchain.go): both
// *pgxpool.Pool and pgx.Tx satisfy it, so a caller inside a transaction — the
// claim, the versioned update and the link write all load the row they are about
// to change after locking it — reuses this function and the scanner instead of
// copying the column list into its own file.
//
// The row is selected by id AND its tenant in one statement: there is no
// unscoped lookup in this package to reach for by mistake, and no read-then-check
// window between fetching a row and deciding whether the caller may see it.
func getRequest(ctx context.Context, q queryRower, requestID, orgID, teamID int) (*domain.TeamRequest, error) {
	if requestID <= 0 {
		// A non-positive id cannot name a row (the serial starts at 1), so it is
		// an ordinary missing request — not a tenant-scope bug — and answering
		// ErrNotFound keeps a route parameter that failed to parse on the same
		// 404 path as an id that simply does not exist.
		return nil, fmt.Errorf("request %d: %w", requestID, ErrNotFound)
	}
	if orgID <= 0 || teamID <= 0 {
		return nil, fmt.Errorf("request %d: %w", requestID, ErrRequestTenantScope)
	}
	req := &domain.TeamRequest{}
	err := scanRequest(q.QueryRow(ctx, requestSelect+requestTenantWhere,
		requestID, orgID, teamID), req)
	if errors.Is(err, pgx.ErrNoRows) {
		// The message names the id only. Naming the tenant here would put the
		// request's team into a log line (or, if a handler ever passed the error
		// through, into a response) for a caller who is not entitled to it.
		return nil, fmt.Errorf("request %d: %w", requestID, ErrNotFound)
	}
	if err != nil {
		// A real load failure (the pool is closing, the statement was cancelled,
		// the connection died) is NOT a missing row: it is wrapped as itself so
		// errors.Is still finds context.Canceled, pgx.ErrTxClosed and friends,
		// and errors.Is(err, ErrNotFound) stays false. A caller that turned this
		// into a 404 would report a database problem as "no such request".
		return nil, fmt.Errorf("get request %d: %w", requestID, err)
	}
	return req, nil
}
