package repository

// Team requests: the requests one requester already holds, looked up and listed
// by their requester column (epic 202, story 549, AMP #3958; approved product
// contract artifact 36 v2 §2 and §4; build slice S2 of
// discoveries/team-inbox-approved-build-slices.md).
//
// request_rows.go is the record's read contract and this file USES it:
// requestSelect, scanRequest and requestFields are the same ones every other
// request read uses, so a column added by a later migration reaches these reads
// without the file changing. Nothing here may edit that contract.
//
// Why these reads exist at all, and why they are not the "unscoped lookup" that
// file refuses to provide. Every other read in this package is scoped by the
// request's tenant (org and team), because "load by id" alone would be a
// cross-tenant read primitive. A requester has no tenant to name: they are an
// invited customer whose only relation to AMP is their own request, with no org
// role, no team membership and often no idea which team id they wrote to. The
// route they call therefore cannot carry org and team, and the read that serves
// it must be scoped by something else — the requester column, which migration
// 053 keeps as the durable answer to "who asked".
//
// So every read here is scoped by requester_user_id, and the user id it
// compares comes from the authenticated principal, never from input. That makes
// them strictly narrower than a tenant read rather than wider:
//
//   - a lookup returns at most one row, the one whose requester is the caller,
//     so it cannot enumerate and cannot fan out;
//   - the list is one person's own requests, never "every request in the
//     instance" and never "every request in a team": requestOwnWhere is the
//     only predicate it can be widened with, and it is an equality on one
//     column;
//   - a request belonging to somebody else — in any team, in any org — is
//     ErrNotFound, exactly like an id that does not exist, so an id walker
//     learns nothing (contract §2, slice B4);
//   - they read the record, not a projection: which fields a requester may see
//     is the service's decision (authz.RequestViewFor + the projection), and
//     this file must not decide it. Returning the record is also what lets one
//     call serve a caller who is both the requester and a team member: their
//     view is the team's, and the team's view is the record.
//
// A request submitted by a service client or a connector has no requester user
// (nil column), so it is never returned here. That is deliberate: the requester
// surface is a signed-in customer surface, and a service identity reads through
// its team's queue instead.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestRequester is the caller-bug answer for a requester read naming no
// requester. It is deliberately not ErrNotFound: a read without the user it is
// scoped by would otherwise match nothing in the schema and read as "that
// request is not yours", hiding the missing argument behind a legitimate 404.
// It is the same discipline as ErrRequestOwnershipUser and ErrRequestClaimant.
var ErrRequestRequester = errors.New("a requester read names its requester: pass the authenticated user whose requests to read")

// requestOwnWhere is the one predicate behind every read in this file: the row
// belongs to this requester and to nobody else. Written once so the lookup, the
// page and the total cannot describe three different sets of "my requests".
//
// requestOwnWhere builds it for a given argument position, the way
// requestListWhere binds its own arguments: the caller passes the $n it already
// assigned to the requester id, and no read can spell the column differently.
func requestOwnWhere(argN string) string { return "requester_user_id = " + argN }

// GetRequestByRequester loads requestID when its requester is requesterUserID,
// and answers ErrNotFound otherwise — including when the request exists but
// belongs to somebody else, and including when the request has no requester
// user at all.
//
// There is no other filter and no other scope, on purpose: the caller's own
// column IS the authorization for this read (authz.RequestViewFor's
// OwnRequestView), so a wider query here would be a wider grant. Which of the
// two views the answer is rendered as stays the service's decision; this
// function only says "this request is yours, or it is not".
//
// A real load failure (the pool is closing, the statement was cancelled, the
// connection died) is returned as itself, so a caller that turned it into a 404
// would report a database problem as "no such request".
func (r *Repo) GetRequestByRequester(ctx context.Context, requestID, requesterUserID int) (*domain.TeamRequest, error) {
	if requestID <= 0 {
		// The serial starts at 1, so a non-positive id cannot name a row: an
		// ordinary missing request rather than a caller bug, so a route
		// parameter that failed to parse lands on the same 404.
		return nil, fmt.Errorf("request %d for requester: %w", requestID, ErrNotFound)
	}
	if requesterUserID <= 0 {
		return nil, fmt.Errorf("request %d: %w", requestID, ErrRequestRequester)
	}
	req := &domain.TeamRequest{}
	err := scanRequest(r.db.QueryRow(ctx,
		requestSelect+` WHERE id = $1 AND `+requestOwnWhere("$2"),
		requestID, requesterUserID), req)
	if errors.Is(err, pgx.ErrNoRows) {
		// One message for every miss: an unknown id, another requester's
		// request, and a service- or connector-submitted row with no requester
		// all read the same, so nothing about existence or ownership leaks. It
		// names the id only — never the user it was scoped by, which is the
		// caller's own and has no business in a log line about a request they
		// may not see.
		return nil, fmt.Errorf("request %d for requester: %w", requestID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get request %d for requester: %w", requestID, err)
	}
	return req, nil
}

// GetRequestByRequesterRef is GetRequestByRequester for the way a requester
// actually addresses their request: the opaque public reference. The reference
// is unique across the whole table (053), so there is no tenant to add — and
// the requester column is the access control, exactly as in the id lookup.
//
// Another requester's reference, a reference a service client's request carries
// (those rows have no requester user), and a reference that was never minted are
// all ErrNotFound, so the surface's 404 is the same bytes for all three and a
// guessed reference confirms nothing. An empty reference is the caller-bug
// answer, never "everything".
//
// The reference is compared as stored: the caller normalizes it through
// domain.NormalizeRequestPublicRef (which strips the display prefix), so a value
// this function sees was already a legal token shape.
func (r *Repo) GetRequestByRequesterRef(ctx context.Context, publicRef string, requesterUserID int) (*domain.TeamRequest, error) {
	ref := publicRef
	if ref == "" {
		return nil, fmt.Errorf("request by reference: %w", ErrNotFound)
	}
	if requesterUserID <= 0 {
		return nil, fmt.Errorf("request by reference: %w", ErrRequestRequester)
	}
	req := &domain.TeamRequest{}
	err := scanRequest(r.db.QueryRow(ctx,
		requestSelect+` WHERE public_ref = $1 AND `+requestOwnWhere("$2"),
		ref, requesterUserID), req)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("request by reference: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get request by reference: %w", err)
	}
	return req, nil
}

// GetRequestByRef loads publicRef inside orgID's teamID — the team surface
// addressing a request by the reference the requester quotes, which is what a
// triager pastes from an email.
//
// It is tenant-scoped exactly like GetRequest: a reference in another team (a
// sibling team of the same org included) and a reference that was never minted
// are both ErrNotFound, so a paste error and another team's request are the same
// answer. The reference is unique across the whole table (053), so this is not a
// scan; the org/team terms are what make the read a tenant read rather than a
// table-wide lookup.
//
// Without this, a caller that names both a team and a reference would have to
// either search the whole table (the unscoped enumeration this package refuses)
// or be silently scoped by something it did not name.
func (r *Repo) GetRequestByRef(ctx context.Context, publicRef string, orgID, teamID int) (*domain.TeamRequest, error) {
	if publicRef == "" {
		return nil, fmt.Errorf("request by reference: %w", ErrNotFound)
	}
	if orgID <= 0 || teamID <= 0 {
		return nil, fmt.Errorf("request by reference: %w", ErrRequestTenantScope)
	}
	req := &domain.TeamRequest{}
	err := scanRequest(r.db.QueryRow(ctx,
		requestSelect+` WHERE public_ref = $1 AND org_id = $2 AND team_id = $3`,
		publicRef, orgID, teamID), req)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("request by reference in team %d: %w", teamID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get request by reference in team %d: %w", teamID, err)
	}
	return req, nil
}

// OwnRequestFilter is one requester's list: whose requests, and how to page and
// narrow them. The zero value is NOT a valid filter — RequesterUserID is the
// whole scope and is required — and "empty" on the optional fields means "do
// not filter on this", the RequestFilter convention.
//
// It is deliberately smaller than RequestFilter. A requester has no tenant to
// name, no claim or assignment to filter by, no "attached" question to ask and
// no free-text search across other people's words; every term this type does
// not have is a term that cannot leak into a requester's read. States is here
// because the requester mockup (artifact 38 v1) filters the list into open and
// closed, and that is a filter over their own rows.
type OwnRequestFilter struct {
	// RequesterUserID is the caller: the one user whose requests these are.
	// Required, and refused with ErrRequestRequester rather than answered with
	// nothing.
	RequesterUserID int
	// States matches any of the listed lifecycle states; empty matches all.
	// Values are normalized and refused when unknown, the RequestFilter rule.
	States []domain.RequestStatus
	// BeforeID pages back: only rows with id < BeforeID. 0 means the first
	// page.
	BeforeID int
	// Limit is the page size; 0 means the ListRequests default (50) and
	// anything above its ceiling is clamped.
	Limit int
}

// ListOwnRequests returns one page of requesterUserID's requests, newest first,
// with the same {items, count, next_before_id} shape and the same keyset rules
// as ListRequests: ordering is `id DESC` and the cursor is that id, so a page is
// stable while rows land above it, and the limit+1 probe tells the caller there
// is another page without counting first.
//
// The count of a filter and the rows it lists are produced by the same
// predicate (ownRequestListWhere), so CountOwnRequests always agrees with paging
// through this function to the end.
func (r *Repo) ListOwnRequests(ctx context.Context, f OwnRequestFilter) (*RequestPage, error) {
	where, args, err := ownRequestListWhere(f)
	if err != nil {
		return nil, err
	}
	limit := requestListLimit(f.Limit)
	// One row more than the page: that extra row is how the caller learns there
	// is another page without a second count query, and it is never returned.
	args = append(args, limit+1)
	q := requestSelect + " WHERE " + where + " ORDER BY id DESC LIMIT $" + fmt.Sprint(len(args))
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list requests of requester %d: %w", f.RequesterUserID, err)
	}
	items, err := collect(rows, scanRequest)
	if err != nil {
		return nil, fmt.Errorf("list requests of requester %d: %w", f.RequesterUserID, err)
	}
	page := &RequestPage{Items: items, Count: len(items)}
	if len(items) > limit {
		page.Items = items[:limit]
		page.Count = limit
		page.NextBeforeID = page.Items[limit-1].ID
	}
	return page, nil
}

// CountOwnRequests is how many requests the filter matches in total, ignoring
// its paging fields — the number a requester's list header shows. It is
// CountRequests' rule applied to an own-request list: the cursor is cleared
// before the predicate is built, so the number never shrinks as someone pages,
// and the caller's filter is untouched.
func (r *Repo) CountOwnRequests(ctx context.Context, f OwnRequestFilter) (int, error) {
	f.BeforeID = 0
	f.Limit = 0
	where, args, err := ownRequestListWhere(f)
	if err != nil {
		return 0, err
	}
	var n int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM team_requests WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count requests of requester %d: %w", f.RequesterUserID, err)
	}
	return n, nil
}

// ownRequestListWhere builds this file's whole predicate: the requester's own
// column, the optional state filter, and the paging cursor. It is the only
// place the own-request reads spell their SQL, which is what keeps a page and
// its total from drifting apart.
//
// The order is fixed (own column, states, cursor) and the arguments are bound in
// that order, so a caller reading the two functions above can see the same
// $1/$2/... in both. The cursor is included when BeforeID is set; a caller
// whose answer must be the total rather than one page's remainder clears it
// first (CountOwnRequests), exactly as CountRequests does.
func ownRequestListWhere(f OwnRequestFilter) (string, []any, error) {
	if f.RequesterUserID <= 0 {
		return "", nil, fmt.Errorf("list own requests: %w", ErrRequestRequester)
	}
	where := []string{requestOwnWhere("$1")}
	args := []any{f.RequesterUserID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if len(f.States) > 0 {
		// Normalized here, once, and refused on an unknown value: a typo'd state
		// that matched nothing would read as "no requests in that state" instead
		// of as the caller's mistake (the requestListWhere rule).
		states := make([]string, 0, len(f.States))
		for _, raw := range f.States {
			if strings.TrimSpace(string(raw)) == "" {
				return "", nil, errors.New("list own requests: an empty state is not a filter; omit States to list every state")
			}
			st, err := domain.NormalizeRequestStatus(string(raw))
			if err != nil {
				return "", nil, fmt.Errorf("list own requests: %w", err)
			}
			states = append(states, string(st))
		}
		where = append(where, "state = ANY("+arg(states)+")")
	}
	if f.BeforeID != 0 {
		where = append(where, "id < "+arg(f.BeforeID))
	}
	return strings.Join(where, " AND "), args, nil
}
