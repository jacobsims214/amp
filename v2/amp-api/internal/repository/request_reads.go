package repository

// Team requests: the keyset list a team's inbox reads (epic 202, story 535,
// AMP #3874; approved product contract artifact 36 v2 §2 and §5; build slice C5
// of discoveries/team-inbox-approved-build-slices.md).
//
// request_rows.go owns the row contract — requestCols, requestSelect,
// scanRequest and the tenant predicate — and this file USES it: every row here
// is selected with requestSelect and scanned by scanRequest, so a column added
// by a later migration reaches the list without this file changing. Nothing in
// this file may edit that contract; a new column belongs there, once.
//
// Why keyset and not OFFSET. A team inbox is a live queue: rows arrive while
// someone pages through it. `OFFSET n` counts rows, so an insert above the
// window pushes one row past the boundary and the next page repeats a row the
// reader already saw (or, with a delete, skips one). A keyset cursor names the
// last row instead — "everything below id 812" — which is stable no matter how
// many rows land above it. Ordering is `id DESC` and the cursor is that id:
// `id` is the serial primary key, so it is unique (no ties, so no "same
// timestamp" ambiguity), monotonic with insert order, and already the shape the
// queue index has (053's idx_team_requests_queue is (team_id, state, id DESC)).
// Never order by created_at: two rows written in one transaction share NOW(),
// and 054's thread read makes the same point for comments.
//
// Why list and count share one WHERE. The queue shows a page of rows and a
// number on each state pill, and the two must never disagree — a pill that says
// "23 in triage" above a page that cannot reach 23 rows is the bug this shape
// prevents. requestListWhere builds the predicate once; ListRequests adds
// ORDER BY/LIMIT and CountRequests wraps it in count(*). A filter added to one
// is therefore added to both. The one term they do not share is the paging
// cursor: CountRequests zeroes BeforeID, because a count is the filter's total
// and never the remainder of a page.
//
// No project data. The list returns domain.TeamRequest rows and nothing else:
// no epic/story/task names, no project codes, no agent identities. The one
// relation the filters touch — "is this request attached to work?" — is an
// EXISTS over request_links that reads no target data at all (see
// filterAttached), because a queue row must never carry project internals to a
// caller whose rights are request-scoped (contract §2, slice B4).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// Paging bounds. The defaults and the ceiling are the contract's (§5 /
// discovery #3657 "pagination and search": default 50, max 200). A limit above
// the ceiling is clamped rather than refused, the same way ListAudit clamps: a
// caller that asked for 1000 gets a full page and a cursor, and pages on,
// instead of an error it cannot act on.
const (
	requestListDefaultLimit = 50
	requestListMaxLimit     = 200
)

// RequestFilter is one inbox read: the tenant it is scoped to, the queue's
// filters, and the keyset cursor. The zero value is not a valid filter — OrgID
// and TeamID are required (see ListRequests) — and every other field is
// optional, where "empty" means "do not filter on this".
//
// It lives in the repository rather than in domain because it describes this
// query, not the product: the service layer (slice S2) turns transport
// arguments into one of these, and nothing above the repository depends on its
// shape.
type RequestFilter struct {
	// OrgID and TeamID are the request's tenant. Both are required, and a
	// filter without them is refused (ErrRequestTenantScope) rather than
	// silently returning every tenant's queue.
	OrgID  int
	TeamID int

	// States matches any of the listed lifecycle states ("all of them" is an
	// empty list, not a list of nine). Values are normalized the way every
	// request write normalizes them, so " Triaging " is triaging; an unknown
	// or blank entry is refused rather than matched against nothing.
	States []domain.RequestStatus
	// RequesterUserID restricts to one requester's own requests (the
	// requester-facing list, slice S2). Requests submitted by a service or a
	// connector have no requester user and are never matched by it.
	RequesterUserID *int
	// SourceKind restricts to one intake surface (web/mcp/api/connector).
	SourceKind domain.RequestSourceKind
	// CreatedAfter and CreatedBefore are a half-open window on created_at:
	// after is inclusive (>=), before is exclusive (<), the ListAudit
	// convention.
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	// Attached, when set, splits the queue into "has work attached" (true) and
	// "nothing attached yet" (false) — the triage question "what have we not
	// acted on?".
	Attached *bool
	// Query is a free-text substring match over the request's subject, its
	// public reference and its input brief, case-insensitive. LIKE's wildcards
	// in it are literal: searching "100%" finds the request that says 100% and
	// not every request.
	Query string

	// BeforeID pages back: only rows with id < BeforeID are returned. 0 means
	// the first page, and RequestPage.NextBeforeID carries the value for the
	// next one.
	BeforeID int
	// Limit is the page size; 0 means requestListDefaultLimit and anything
	// above requestListMaxLimit is clamped to it.
	Limit int
}

// RequestPage is one page of an inbox read, in the {items, count,
// next_before_id} shape amp_kb_list and ListAudit already answer with.
type RequestPage struct {
	Items []domain.TeamRequest `json:"items"`
	// Count is how many rows this page holds (not the filtered total; that is
	// CountRequests).
	Count int `json:"count"`
	// NextBeforeID is the cursor for the next page, and 0 when this is the
	// last one. It is only set when the reader actually has more rows to
	// fetch, so a caller never asks for an empty page to learn it is done.
	NextBeforeID int `json:"next_before_id,omitempty"`
}

// ListRequests returns one page of teamID's requests, newest first, for the
// caller's org and team only. A request in another team (another team of the
// same org included) is not filtered out of the answer — it is never read:
// the tenant is in the WHERE clause, not in a post-filter, so no caller can
// derive another team's row count from a page.
//
// The count of a filter and the rows that filter lists are produced by the same
// predicate (requestListWhere), so CountRequests always agrees with paging
// through ListRequests to the end.
func (r *Repo) ListRequests(ctx context.Context, f RequestFilter) (*RequestPage, error) {
	where, args, err := requestListWhere(f)
	if err != nil {
		return nil, err
	}
	limit := requestListLimit(f.Limit)
	// One row more than the page: that extra row is how the caller learns
	// there is another page without a second count query, and it is never
	// returned.
	args = append(args, limit+1)
	q := requestSelect + " WHERE " + where + " ORDER BY id DESC LIMIT $" + fmt.Sprint(len(args))
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	items, err := collect(rows, scanRequest)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	page := &RequestPage{Items: items, Count: len(items)}
	if len(items) > limit {
		page.Items = items[:limit]
		page.Count = limit
		page.NextBeforeID = page.Items[limit-1].ID
	}
	return page, nil
}

// CountRequests is how many requests the filter matches in total, ignoring its
// paging fields. It is the number a queue pill shows, computed by the same
// predicate ListRequests pages with.
//
// The cursor is cleared before the predicate is built. It is a paging field
// exactly like Limit — "rows below this one" describes a page, not the filter
// — so honoring it would answer "how many rows are left below the cursor",
// the one number a queue pill must never show: the pill counts the queue (the
// filter's total), not the rest of someone's scroll. Limit never reached the
// predicate; BeforeID does, so it is zeroed here, on the caller's value copy.
// The caller's filter is untouched and ListRequests' cursor semantics are
// unaffected.
func (r *Repo) CountRequests(ctx context.Context, f RequestFilter) (int, error) {
	f.BeforeID = 0
	where, args, err := requestListWhere(f)
	if err != nil {
		return 0, err
	}
	var n int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM team_requests WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count requests: %w", err)
	}
	return n, nil
}

// requestListLimit clamps the asked-for page size into the contract's bounds.
func requestListLimit(limit int) int {
	switch {
	case limit <= 0:
		return requestListDefaultLimit
	case limit > requestListMaxLimit:
		return requestListMaxLimit
	}
	return limit
}

// requestListWhere builds the tenant predicate and every filter, returning the
// SQL fragment and its arguments in order. Both readers call it, which is what
// keeps a count and the pages it counts from drifting apart. It includes the
// paging cursor when BeforeID is set; a caller whose answer must be the
// filter's total rather than one page's remainder (CountRequests) clears
// BeforeID before calling.
//
// The first two terms are the org/team halves of request_rows.go's
// requestTenantWhere, in the same order, so a caller reading either file sees
// the same tenancy. The id half is absent here because a list is scoped to a
// tenant, not to one row.
func requestListWhere(f RequestFilter) (string, []any, error) {
	if f.OrgID <= 0 || f.TeamID <= 0 {
		return "", nil, fmt.Errorf("list requests: %w", ErrRequestTenantScope)
	}
	where := []string{"org_id = $1", "team_id = $2"}
	args := []any{f.OrgID, f.TeamID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if len(f.States) > 0 {
		// Normalized here, once, and refused on an unknown value: a typo'd
		// state that matched nothing would read as "no work in that state"
		// instead of as the caller's mistake.
		states := make([]string, 0, len(f.States))
		for _, raw := range f.States {
			if strings.TrimSpace(string(raw)) == "" {
				return "", nil, fmt.Errorf("list requests: an empty state is not a filter; omit States to list every state")
			}
			st, err := domain.NormalizeRequestStatus(string(raw))
			if err != nil {
				return "", nil, fmt.Errorf("list requests: %w", err)
			}
			states = append(states, string(st))
		}
		where = append(where, "state = ANY("+arg(states)+")")
	}
	if f.RequesterUserID != nil {
		where = append(where, "requester_user_id = "+arg(*f.RequesterUserID))
	}
	if f.SourceKind != "" {
		// RequestSourceKinds is the closed set migration 053's CHECK pins.
		// NormalizeRequestSource is deliberately not used: it also rules on
		// source_external_id (a connector must carry one, a web request must
		// not), which is a write rule and has no meaning in a filter.
		kind := domain.RequestSourceKind(strings.ToLower(strings.TrimSpace(string(f.SourceKind))))
		if !slices.Contains(domain.RequestSourceKinds, kind) {
			return "", nil, fmt.Errorf("list requests: source_kind must be one of %v, not %q", domain.RequestSourceKinds, f.SourceKind)
		}
		where = append(where, "source_kind = "+arg(string(kind)))
	}
	if f.CreatedAfter != nil {
		where = append(where, "created_at >= "+arg(*f.CreatedAfter))
	}
	if f.CreatedBefore != nil {
		where = append(where, "created_at < "+arg(*f.CreatedBefore))
	}
	if f.Attached != nil {
		if *f.Attached {
			where = append(where, attachedWhere)
		} else {
			where = append(where, notAttachedWhere)
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// The wildcards in the needle are literal, the same escaping
		// ListAudit uses for its actor substring: a request whose subject says
		// "100% done" is found by "100%", and a bare "%" finds that subject
		// instead of every row in the queue.
		needle := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
		ref := arg(needle)
		where = append(where,
			`(subject ILIKE '%' || `+ref+` || '%' OR public_ref ILIKE '%' || `+ref+` || '%' OR input_brief ILIKE '%' || `+ref+` || '%')`)
	}
	if f.BeforeID != 0 {
		where = append(where, "id < "+arg(f.BeforeID))
	}
	return strings.Join(where, " AND "), args, nil
}

// attachedWhere / notAttachedWhere are the "is there work attached to this
// request?" predicates behind RequestFilter.Attached.
//
// Attached means a lineage row exists: the request was routed to a project
// (routes_to) or plan items were created for it (planned_as) — 055's two
// link kinds that describe the request having been acted on. The EXISTS reads
// no target data (no join to projects, epics, stories or tasks), so a queue row
// still carries nothing but the request (contract §2: a request-scoped caller
// must never see project internals), and a link whose target was later deleted
// still counts: the row is lineage, and its survival is the point of 055's
// absent foreign key.
//
// planned_at (053: "first planned_as link") is the request-row mirror of the
// planned_as half. It is deliberately not consulted: two sources of truth for
// one question is how they drift, and the link table is the durable one.
//
// The subquery assumes the outer query is over team_requests with no alias, so
// the correlated column is unqualified `id`; requestSelect selects exactly that
// table and nothing joins it.
const attachedWhere = `EXISTS (SELECT 1 FROM request_links l
	WHERE l.request_id = team_requests.id AND l.link_kind IN ('routes_to', 'planned_as'))`
const notAttachedWhere = `NOT ` + attachedWhere
