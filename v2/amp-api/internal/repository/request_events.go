package repository

// Team requests: the history read (epic 202, story 549, AMP #4271; approved
// product contract artifact 36 v2 §1, invariants I6 and I8; the detail design
// artifact 37 v1's history rail).
//
// Migration 054 (architecture/team-request-history-schema.md) added
// request_events and wrote every act into it — the submit, a transition, a
// claim, a release, a transfer, a link, an unlink, an edit (062) — but until
// this file there was no read that returned one. The table was write-only, so
// the detail page could not render a request's history at all and said so
// (the temporary seam in v2/ui/src/pages/RequestDetail.tsx).
//
// # Append-only, so id order is history order
//
// Rows are only ever inserted (054's header: append-only is an application
// contract, not a SQL guarantee). `id` is the sequence the insert took, so
// ORDER BY id is the order the acts happened — and unlike created_at it is
// still a total order for several rows written in one transaction, which share
// NOW(). That is the same rule the thread read follows, and it is what makes
// the cursor below a stable keyset: a row appended while a reader pages sorts
// after the cursor, never before it.
//
// # The tenant is the request's, and it is asked first
//
// Every other request read answers ErrNotFound for a request in another
// tenant, and this one must answer the same thing rather than an empty list: an
// empty page and "not yours" would otherwise be one answer, and a caller could
// not tell a request with no acts from a request it may not see. So the request
// is loaded tenant-scoped first (getRequest, the one lookup every request read
// uses), and only then are its rows read. The row's own org_id/team_id are
// restated in the WHERE clause as well: 054 denormalizes them onto every row
// precisely so a history read is tenant-scoped without trusting the parent, and
// restating them means a row that somehow disagreed with its request could not
// be served through this read.
//
// The cost is one extra round trip, and it buys the distinction. RequestLinks
// pays the same price for the same reason; the thread read does not, because
// "does this request exist?" is deliberately not its question.

import (
	"context"
	"fmt"

	"github.com/simstech/amp-api/internal/domain"
)

// requestEventCols is the column list of request_events and
// domain.RequestEvent, in 054's order. Explicit (no SELECT *), and positional
// against the scan targets below, the way requestCols and requestCommentCols
// are — a column added by a later migration has to be added here deliberately,
// and a test asserts the list still matches the table.
//
// There is no alias parameter because this read does not join: the tenant
// question is the request's and is asked first, so no second table's `id` or
// `created_at` can collide with these.
const requestEventCols = `id, request_id, org_id, team_id, action, from_state, to_state,
	actor, actor_user_id, detail, created_at`

func requestEventFields(e *domain.RequestEvent) []any {
	return []any{&e.ID, &e.RequestID, &e.OrgID, &e.TeamID, &e.Action,
		&e.FromStatus, &e.ToStatus, &e.Actor, &e.ActorUserID, &e.Detail, &e.CreatedAt}
}

func scanRequestEvent(row rowScanner, e *domain.RequestEvent) error {
	return row.Scan(requestEventFields(e)...)
}

// RequestEventPage is one forward page of a request's history: the rows and the
// cursor for the next page, in the {items, count, next_after_id} shape the
// thread read already answers with — the same direction, because a history is
// read from where it began exactly as a thread is.
//
// NextAfterID is set only when there really is another page (the limit+1
// probe), so a reader never has to ask for an empty page to learn it is done.
// The cursor is an event id, which is the row's own field: a caller that pages
// by hand can use the last row's id instead, and the two agree.
type RequestEventPage struct {
	// Items is the page, in history order (by id).
	Items []domain.RequestEvent `json:"items"`
	// Count is how many rows this page holds.
	Count int `json:"count"`
	// NextAfterID is the cursor for the next page, 0 on the last one.
	NextAfterID int64 `json:"next_after_id,omitempty"`
}

// ListRequestEventPage returns one forward page of one request's history:
// append order (by id, never created_at, which several rows in one transaction
// share), which is the order idx_request_events_request (request_id, id)
// serves.
//
// afterID pages forward: 0 starts at the request's first act, because a history
// is read from where it began rather than from its newest row. limit bounds a
// page (0 means the default, above the ceiling is clamped) — and the clamp
// lives here rather than in the caller, which is why this read answers the
// cursor: a caller re-deriving requestListMaxLimit above the repository would be
// a second copy of the bound, and a caller that did not would silently truncate
// a long history with no way for its reader to ask for the rest.
//
// The tenant is required and is the request's (see the file header): a request
// in another org or team, and an id that was never issued, both answer
// ErrNotFound — the same answer every other request read gives, so a caller
// cannot tell "not yours" from "not there".
func (r *Repo) ListRequestEventPage(ctx context.Context, requestID, orgID, teamID int, afterID int64, limit int) (*RequestEventPage, error) {
	if requestID <= 0 {
		// A non-positive id names no row (the serial starts at 1), so it is an
		// ordinary missing request, answered like a wrong tenant
		// (request_rows.go's rule).
		return nil, fmt.Errorf("history of request %d: %w", requestID, ErrNotFound)
	}
	if orgID <= 0 || teamID <= 0 {
		return nil, fmt.Errorf("history of request %d: %w", requestID, ErrRequestTenantScope)
	}
	// The tenant question is the request's, and it is asked before any row is
	// read: this is what makes a foreign request ErrNotFound rather than an
	// empty history. A real load failure is returned as itself, never as a
	// missing row (getRequest's own rule).
	if _, err := getRequest(ctx, r.db, requestID, orgID, teamID); err != nil {
		return nil, err
	}
	if afterID < 0 {
		afterID = 0
	}
	limit = requestListLimit(limit)
	query := `SELECT ` + requestEventCols + `
		  FROM request_events
		 WHERE request_id = $1 AND org_id = $2 AND team_id = $3 AND id > $4
		 ORDER BY id
		 LIMIT $5`
	// One row more than the page: that extra row is how the caller learns there
	// is another page without a second count query, and it is never returned.
	rows, err := r.db.Query(ctx, query, requestID, orgID, teamID, afterID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list history of request %d: %w", requestID, err)
	}
	items, err := collect(rows, scanRequestEvent)
	if err != nil {
		return nil, fmt.Errorf("list history of request %d: %w", requestID, err)
	}
	page := &RequestEventPage{Items: items, Count: len(items)}
	if len(items) > limit {
		page.Items = items[:limit]
		page.Count = limit
		page.NextAfterID = page.Items[limit-1].ID
	}
	if page.Items == nil {
		// An empty page is "this request has no acts after this cursor", which
		// is an answer; nil would encode as null and read as "no read happened".
		page.Items = []domain.RequestEvent{}
	}
	return page, nil
}

// ListRequestEvents returns one request's history as rows, without the page
// envelope: the shape this read was asked for, and what a caller that wants the
// record rather than a cursor uses.
//
// It is ListRequestEventPage with the envelope dropped, so there is exactly one
// statement, one clamp and one tenant check behind both — a second query here
// would be a second place to forget that the tenant question is asked first.
func (r *Repo) ListRequestEvents(ctx context.Context, requestID, orgID, teamID int, afterID int64, limit int) ([]domain.RequestEvent, error) {
	page, err := r.ListRequestEventPage(ctx, requestID, orgID, teamID, afterID, limit)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}
