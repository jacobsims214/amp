package repository

// Team requests: the queue and personal-work aggregates a team inbox and My
// work read (epic 202, story 548, AMP #3956; approved product contract
// artifact 36 v2 §2/§5, triage mockup artifact 37 v1 view 1 and view 3; build
// slices S2 (per-state counts for queue pills and My work summaries) and J3
// (the "Team inboxes" and "Claimed by me · team requests" panels) of
// discoveries/team-inbox-approved-build-slices.md).
//
// Two questions, two functions, one predicate:
//
//	CountRequestsByState   how many requests is the queue holding, per state?
//	RequestsOwnedBy        which requests has this one person claimed or been
//	                       assigned, inside the same filtered queue?
//
// Both are READS of the queue filter, not new filters. They are built by
// calling request_reads.go's requestListWhere — the single predicate
// ListRequests pages with and CountRequests counts with — so a filter added
// there is honoured here without this file being edited, and no aggregate can
// quietly start counting a different queue than the one the page shows. Nothing
// in this file writes, and nothing here edits request_reads.go, request_rows.go
// or the tables they read: a column or a filter term belongs there, once.
//
// Why these two are separate from ListRequests/CountRequests. A queue page
// answers "the next N rows"; these answer "what shape is the whole queue in".
// The distinction is the paging fields. A queue pill's number must not shrink
// as someone scrolls, and a "Claimed by me" header must not count only the page
// it can see, so every total here is computed with the cursor and limit
// cleared. The one place a cursor still means something — the ownership list,
// which is a list of ids and can legitimately be paged — reports its total
// cursor-free and its ids cursor-scoped, and says so in its type comment.
//
// The anti-fan-out rule is structural here, not a convention (mockup 37
// decision: "a request becomes yours only via an explicit claim"). A shared
// queue is not a set of personal tasks: RequestsOwnedBy reads ownership off the
// request row itself (claim_user_id / owner_user_id, 053's two distinct fields)
// and never expands a team's queue into per-member rows. One request yields at
// most one row in its result, and that row is its own claim or its own
// assignment — never both, because the WHERE is one disjunction over one table
// rather than a union of two selects, and the ids are derived from the rows
// returned.

import (
	"context"
	"errors"
	"fmt"

	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestOwnershipUser marks an ownership read made without the user whose
// slice it is. Like ErrRequestClaimant it reports a caller bug rather than a
// refusal worth showing anyone: "which requests has somebody claimed" is not a
// question a queue summary can answer, and answering it with an empty result
// would read as "nobody owns anything" instead of as the missing argument.
// It is deliberately not ErrNotFound and never ErrConflict — nothing was looked
// up by id, and nothing collided.
var ErrRequestOwnershipUser = errors.New("an ownership summary names its user: pass the user whose claimed and assigned requests to list")

// RequestStateTotals is how many requests one queue filter matches in each of
// the nine lifecycle states, plus their sum.
//
// States always carries every state in domain.RequestStatuses, zeros included:
// a pill row renders all of them, and a caller should never have to distinguish
// "no requests in that state" from "the key is missing".
//
// What the numbers mean. The filter is applied exactly as ListRequests applies
// it (the same requestListWhere predicate, every dimension honoured), and the
// matching rows are grouped by state — so the breakdown partitions the filter
// rather than describing a different queue. Two invariants follow, and the
// tests pin both:
//
//	totals.State(s) == CountRequests(f with States = [s])
//	totals.Total    == CountRequests(f) == sum over all nine states
//
// A filter that names states gets the honest breakdown for it: the states it
// excluded read 0 and Total is the count of the states it named. A pill row that
// shows every state at once therefore passes a filter with States empty — which
// is also the filter its list is fetched with — and a caller that wants one
// pill's number can read it here or ask CountRequests directly. Nothing is
// silently ignored: a filter term given is a filter term applied.
//
// Paging never reaches these numbers. BeforeID and Limit are cleared before the
// predicate is built (the CountRequests rule from AMP #3897, applied to a
// group-by): a pill counts the queue, not the remainder below somebody's scroll
// position, and not one page of it.
type RequestStateTotals struct {
	// Total is the sum of States — the filter's whole queue.
	Total int `json:"total"`
	// States is the per-state count, one entry per lifecycle state.
	States map[domain.RequestStatus]int `json:"states"`
}

// State is the count for one state, 0 for anything outside the nine (callers
// pass states they already validated; this never panics on a typo).
func (t RequestStateTotals) State(state domain.RequestStatus) int { return t.States[state] }

// CountRequestsByState is the queue-pill aggregate: how many requests this
// filter matches in each state. It is tenant-scoped by the same predicate as
// every request read — a filter without its org and team is refused with
// ErrRequestTenantScope, never answered with every tenant's queue — and it
// honours every non-state filter (requester, source, dates, attached, q) so the
// pills describe the queue the caller is looking at.
//
// It runs one grouped count over team_requests. No link or ticket table is
// joined: the attached filter is the EXISTS predicate requestListWhere already
// owns, and a queue aggregate carries no project internals any more than a queue
// row does (contract §2).
//
// Several teams. The filter names one team, so a caller summarizing a person's
// several inboxes calls this once per team. That is deliberate rather than a
// multi-team variant: requestListWhere scopes by exactly one team/organization
// pair, and widening it here would be a second, divergent tenant predicate —
// the one thing this file is not allowed to grow.
func (r *Repo) CountRequestsByState(ctx context.Context, f RequestFilter) (RequestStateTotals, error) {
	// Cleared on the value copy so the caller's filter is untouched. Limit never
	// reaches the predicate, but clearing it keeps "a total ignores paging" true
	// by inspection (see the type comment).
	f.BeforeID = 0
	f.Limit = 0

	where, args, err := requestListWhere(f)
	if err != nil {
		return RequestStateTotals{}, err
	}
	rows, err := r.db.Query(ctx, `SELECT state, count(*) FROM team_requests WHERE `+where+` GROUP BY state`, args...)
	if err != nil {
		return RequestStateTotals{}, fmt.Errorf("count requests by state: %w", err)
	}
	defer rows.Close()

	totals := RequestStateTotals{States: make(map[domain.RequestStatus]int, len(domain.RequestStatuses))}
	for _, st := range domain.RequestStatuses {
		totals.States[st] = 0
	}
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return RequestStateTotals{}, fmt.Errorf("count requests by state: %w", err)
		}
		st := domain.RequestStatus(state)
		if _, known := totals.States[st]; !known {
			// 053's CHECK pins the nine values, so this is not a state a row
			// can hold: it is the domain and the database disagreeing about the
			// vocabulary (#3754's rule). Refusing beats returning a total whose
			// per-state parts do not add up to it.
			return RequestStateTotals{}, fmt.Errorf("count requests by state: the database holds state %q, which domain.RequestStatuses does not name", state)
		}
		totals.States[st] = n
		totals.Total += n
	}
	if err := rows.Err(); err != nil {
		return RequestStateTotals{}, fmt.Errorf("count requests by state: %w", err)
	}
	return totals, nil
}

// RequestOwnership is one person's slice of one filtered team queue: the
// requests whose triage claim they hold, the requests they are the assignee of,
// and both together, newest first.
//
// A request that this person both holds the claim on and owns (053 deliberately
// keeps claim and assignee apart) appears in ClaimedIDs, in AssignedIDs and
// exactly once in IDs: the two lists answer different questions ("triaging" vs
// "owns the outcome") about the same row, while IDs is the person's work list
// and may not repeat a request. That is what "deduplicate the request" means
// here, and it is structural: one row per matching request is read and the
// lists are partitioned from it, so no request can be counted twice no matter
// how many ownership fields match.
//
// Total counts distinct requests (cursor-free, see below) and is therefore the
// number a "Claimed by me" panel header shows. Count and NextBeforeID describe
// the page of IDs in this value.
type RequestOwnership struct {
	// UserID is whose slice this is, echoed so a caller with several panels
	// cannot mix two people's answers up.
	UserID int `json:"user_id"`
	// ClaimedIDs are the page's rows this user is triaging: claim_user_id set
	// to them. Never expired or stolen by anything here (D4).
	ClaimedIDs []int `json:"claimed_ids"`
	// AssignedIDs are the page's rows this user owns the outcome of
	// (owner_user_id). Assignment is the durable half; a transfer clears a
	// claim, so in practice these rarely overlap — but the summary does not
	// assume it, and a row can be in both lists.
	AssignedIDs []int `json:"assigned_ids"`
	// IDs are the page's rows claimed OR assigned to this user, each id once,
	// newest first. This is the list a personal panel renders.
	IDs []int `json:"ids"`
	// Count is how many ids this page holds (len(IDs)).
	Count int `json:"count"`
	// NextBeforeID is the cursor for the next page, and 0 when this page is the
	// last one. Only set when the reader actually has more rows to fetch.
	NextBeforeID int `json:"next_before_id,omitempty"`
	// Total is how many distinct requests the ownership + filter match with the
	// paging fields cleared. It never shrinks because someone paged:
	// exactly the CountRequests rule, applied to a personal list.
	Total int `json:"total"`
}

// RequestsOwnedBy lists the request ids in f's queue that userID has claimed or
// is assigned, newest first, with the tenant taken from f exactly as every other
// request read takes it.
//
// Ownership is read off the request row — claim_user_id or owner_user_id — and
// never off request_events or a team's membership: a request is "yours" only
// because you claimed it or were made its assignee, which is precisely the
// anti-fan-out rule the mockup makes structural ("shared queues, not your
// tickets"). An unclaimed, unassigned queue therefore yields nothing for
// anybody, however large it is.
//
// Filters and paging. Every non-paging filter in f is honoured, States
// included: "my open requests" is one call, not a client-side filter. The list
// itself is paged by f.BeforeID/f.Limit with the ListRequests clamp (50 default,
// 200 ceiling) and its own limit+1 probe, so a caller pages a personal panel the
// same way it pages the queue. Total is computed by the same predicate with the
// paging fields cleared, so a header count never equals "how far I scrolled".
//
// A user id is required (ErrRequestOwnershipUser); a missing org or team is
// ErrRequestTenantScope, the same caller-bug answer every request read gives. A
// row in another team — a sibling team of the same org included — and a row in
// another org are never read, so a stranger's claimed work cannot enter the
// answer by any id a caller can pass.
func (r *Repo) RequestsOwnedBy(ctx context.Context, f RequestFilter, userID int) (*RequestOwnership, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("requests owned by: %w", ErrRequestOwnershipUser)
	}
	where, args, owned, err := requestOwnedWhere(f, userID)
	if err != nil {
		return nil, err
	}

	limit := requestListLimit(f.Limit)
	// One row more than the page is the "is there another page?" probe: it is
	// never returned, and the caller does not have to ask for an empty page to
	// learn it is done (the ListRequests shape).
	pageArgs := append(append([]any{}, args...), limit+1)
	owner := fmt.Sprintf("$%d", len(args))
	rows, err := r.db.Query(ctx, `SELECT id,
			COALESCE(claim_user_id = `+owner+`, false),
			COALESCE(owner_user_id = `+owner+`, false)
		FROM team_requests WHERE `+where+` AND `+owned+`
		ORDER BY id DESC LIMIT $`+fmt.Sprint(len(pageArgs)), pageArgs...)
	if err != nil {
		return nil, fmt.Errorf("requests owned by %d: %w", userID, err)
	}
	// One record per row, so the probe row can be dropped before any list is
	// built and the three lists can never hold a row the page does not.
	type ownedRow struct {
		id                int
		claimed, assigned bool
	}
	var got []ownedRow
	for rows.Next() {
		var rec ownedRow
		if err := rows.Scan(&rec.id, &rec.claimed, &rec.assigned); err != nil {
			rows.Close()
			return nil, fmt.Errorf("requests owned by %d: %w", userID, err)
		}
		got = append(got, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("requests owned by %d: %w", userID, err)
	}
	rows.Close()

	out := &RequestOwnership{UserID: userID, ClaimedIDs: []int{}, AssignedIDs: []int{}, IDs: []int{}}
	if len(got) > limit {
		got = got[:limit]
		out.NextBeforeID = got[limit-1].id
	}
	for _, rec := range got {
		out.IDs = append(out.IDs, rec.id)
		if rec.claimed {
			out.ClaimedIDs = append(out.ClaimedIDs, rec.id)
		}
		if rec.assigned {
			out.AssignedIDs = append(out.AssignedIDs, rec.id)
		}
	}
	out.Count = len(out.IDs)

	// The total is the same filter with the paging fields cleared — the same
	// predicate and the same ownership clause built by the same helper, so the
	// header and the pages cannot describe different queues. Cleared on a value
	// copy: the caller's filter is untouched.
	total := f
	total.BeforeID = 0
	total.Limit = 0
	totalWhere, totalArgs, totalOwned, err := requestOwnedWhere(total, userID)
	if err != nil {
		return nil, err
	}
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM team_requests WHERE `+totalWhere+` AND `+totalOwned,
		totalArgs...).Scan(&out.Total); err != nil {
		return nil, fmt.Errorf("count requests owned by %d: %w", userID, err)
	}
	return out, nil
}

// requestOwnedWhere turns a queue filter and a user into the WHERE predicate and
// its arguments for the ownership reads, and returns the ownership disjunction
// itself separately because the list query also selects its two halves.
//
// The user is appended as one parameter referenced by both column comparisons, so
// the disjunction is over one row rather than over a union of two selects: a
// request that is both claimed by and assigned to the caller matches once, which
// is the structural half of "summaries deduplicate request".
//
// requestListWhere owns the tenant and every filter; this adds exactly the one
// term it is about and never re-states the tenant, so a filter that would be
// refused (no org/team, unknown state or source) is refused here with the same
// error.
func requestOwnedWhere(f RequestFilter, userID int) (where string, args []any, owned string, err error) {
	where, args, err = requestListWhere(f)
	if err != nil {
		return "", nil, "", err
	}
	args = append(args, userID)
	param := fmt.Sprintf("$%d", len(args))
	owned = "(claim_user_id = " + param + " OR owner_user_id = " + param + ")"
	return where, args, owned, nil
}
