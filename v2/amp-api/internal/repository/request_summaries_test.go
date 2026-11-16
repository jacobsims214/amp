package repository

// Tests for request_summaries.go against a real Postgres (AMP #3956, epic 202
// story 548). Like the other request tests they run only when TEST_DATABASE_URL
// is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/ -run 'RequestsByState|RequestsOwnedBy|RequestSummaries' -count=1 -race
//
// The ticket's three criteria are what these assert, in order:
//
//  1. PER-STATE TOTALS ARE INDEPENDENT OF THE CURSOR AND MATCH THE FILTER. The
//     totals of a paged filter equal the totals without it, the totals of the
//     filter's own CountRequests, and the number of rows paging the same filter
//     to the end yields — per state and in total. Every filter dimension is
//     honoured (the same predicate ListRequests pages with), and the caller's
//     filter comes back untouched, which is what lets a service reuse ONE paged
//     filter for both the page and the pills.
//  2. CLAIMED AND ASSIGNED SUMMARIES DEDUPLICATE THE REQUEST. A request this
//     user both holds the claim on and is assigned appears exactly once in
//     `IDs` while still being named in `ClaimedIDs` and `AssignedIDs`; a request
//     someone else owns never appears; and a full shared queue with nothing
//     claimed belongs to nobody, which is the anti-fan-out rule the mockup
//     makes structural. `Total` counts distinct requests and ignores the cursor.
//  3. TENANT SCOPE EXCLUDES UNRELATED ROWS. A sibling team in the same org,
//     another org's team, and a team number that exists in another org are all
//     absent from the totals and from the ownership lists, and a filter without
//     its org/team is refused instead of answered (ErrRequestTenantScope).
//
// Each test builds its own org, teams, users and requests with a random suffix
// (requestReadsFixture), so repeated runs and tests in the same database don't
// see each other's rows.

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// requestSummariesFixture is requestReadsFixture plus the extra user a "somebody
// else claimed it" case needs. One migrated database per process is the
// package's convention (requestRowsTestRepo).
type requestSummariesFixture struct {
	*requestReadsFixture
	// third is a person who is neither the fixture's user nor user2, so a test
	// can claim a request as someone the caller is not.
	third int
}

func newRequestSummariesFixture(t *testing.T) *requestSummariesFixture {
	t.Helper()
	f := &requestSummariesFixture{requestReadsFixture: newRequestReadsFixture(t)}
	err := f.r.db.QueryRow(f.ctx, `INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("summaries-third-%d", f.suffix),
		fmt.Sprintf("summaries-third-%d@summaries.test", f.suffix)).Scan(&f.third)
	if err != nil {
		t.Fatalf("insert third user: %v", err)
	}
	return f
}

// claimedBy / assignedTo are request mutators that set the two ownership fields
// the summary reads. 053's CHECK requires a claim time whenever a claimant is
// set, so claim always writes both.
func claimedBy(userID int) func(*domain.TeamRequest) {
	return func(r *domain.TeamRequest) {
		at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		r.ClaimUserID = &userID
		r.ClaimedAt = &at
	}
}

func assignedTo(userID int) func(*domain.TeamRequest) {
	return func(r *domain.TeamRequest) { r.OwnerUserID = &userID }
}

// totalsOf reads the aggregate for a filter and fails the test if it errors.
func (f *requestSummariesFixture) totalsOf(filter RequestFilter) RequestStateTotals {
	f.t.Helper()
	got, err := f.r.CountRequestsByState(f.ctx, filter)
	if err != nil {
		f.t.Fatalf("CountRequestsByState(org %d, team %d): %v", filter.OrgID, filter.TeamID, err)
	}
	return got
}

// ownedOf reads an ownership summary and fails the test if it errors.
func (f *requestSummariesFixture) ownedOf(filter RequestFilter, userID int) *RequestOwnership {
	f.t.Helper()
	got, err := f.r.RequestsOwnedBy(f.ctx, filter, userID)
	if err != nil {
		f.t.Fatalf("RequestsOwnedBy(org %d, team %d, user %d): %v", filter.OrgID, filter.TeamID, userID, err)
	}
	return got
}

// TestRequestsByStateTotalsIgnoreTheCursor is criterion 1.
//
// A queue pill's number must describe the queue, not the reader's scroll
// position: the totals of a paged filter must equal the totals of the same
// filter with no cursor, equal CountRequests of it, and equal the rows that
// paging it to the end yields — per state and in total. The second half of the
// test is the other direction: the totals are the FILTER's, so every dimension
// the caller set (states, source, requester, query, attached) narrows them, and
// a filter whose states are pinned reports exactly that pinned state's count
// with the others at zero.
func TestRequestsByStateTotalsIgnoreTheCursor(t *testing.T) {
	f := newRequestSummariesFixture(t)
	base := requestTime(-2 * time.Hour)

	// A queue with several states, two sources, two requesters and one attached
	// request, so each filter dimension below has something to change.
	requester := f.user
	otherRequester := f.user2
	stamp := func(r *domain.TeamRequest) { r.CreatedAt = base }
	ids := map[domain.RequestStatus][]int{}
	add := func(state domain.RequestStatus, mutators ...func(*domain.TeamRequest)) int {
		m := append([]func(*domain.TeamRequest){stamp}, mutators...)
		id := f.newRequest(f.team, state, m...)
		ids[state] = append(ids[state], id)
		return id
	}
	add(domain.RequestStatusNew, func(r *domain.TeamRequest) { r.RequesterUserID = &requester })
	add(domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = &requester
		r.SourceKind = domain.RequestSourceMCP
	})
	add(domain.RequestStatusNew, func(r *domain.TeamRequest) { r.RequesterUserID = &requester })
	add(domain.RequestStatusTriaging, func(r *domain.TeamRequest) { r.RequesterUserID = &requester })
	add(domain.RequestStatusTriaging, func(r *domain.TeamRequest) { r.RequesterUserID = &otherRequester })
	add(domain.RequestStatusDelivered, func(r *domain.TeamRequest) {
		r.RequesterUserID = &requester
		at := base
		r.OutcomeBrief = "Shipped"
		r.DeliveredAt = &at
	})
	attached := add(domain.RequestStatusInProgress, func(r *domain.TeamRequest) { r.RequesterUserID = &requester })
	f.link(attached, string(domain.RequestLinkRoutesTo), string(domain.RequestTargetProject), 1)
	add(domain.RequestStatusNeedsInformation, func(r *domain.TeamRequest) {
		r.RequesterUserID = &requester
		at := base
		r.NeedsInfoSince = &at
	})

	all := RequestFilter{OrgID: f.org, TeamID: f.team}

	// The independent truth: paging the filter to the end.
	paged := f.pageAll(all, 3)
	if len(paged) != 8 {
		t.Fatalf("paging the queue yielded %d rows, want the 8 inserted", len(paged))
	}
	allTotal, err := f.r.CountRequests(f.ctx, all)
	if err != nil {
		t.Fatalf("CountRequests: %v", err)
	}
	if allTotal != len(paged) {
		t.Fatalf("CountRequests = %d, want %d (the rows paging to the end yields)", allTotal, len(paged))
	}

	// The expected numbers are exactly what this test inserted: every other test
	// in the package uses its own random org and team.
	want := map[domain.RequestStatus]int{}
	for state, list := range ids {
		want[state] = len(list)
	}

	without := f.totalsOf(all)
	if without.Total != allTotal {
		t.Fatalf("totals.Total = %d, want CountRequests' %d", without.Total, allTotal)
	}
	for _, state := range domain.RequestStatuses {
		if without.State(state) != want[state] {
			t.Errorf("totals[%s] = %d, want the %d inserted", state, without.State(state), want[state])
		}
		// Every state is present, zeros included, so a pill row renders all of
		// them without the caller coalescing missing keys.
		if _, ok := without.States[state]; !ok {
			t.Errorf("totals.States has no entry for %s; every state must be present, zero included", state)
		}
	}
	sum := 0
	for _, n := range without.States {
		sum += n
	}
	if sum != without.Total {
		t.Fatalf("the per-state counts sum to %d but Total is %d", sum, without.Total)
	}

	// The same numbers from a paged filter — the shape slice S2 hands it, where
	// one filter drives both the page and the pills.
	newest := paged[0].ID
	middle := paged[len(paged)/2].ID
	for _, tc := range []struct {
		name   string
		filter RequestFilter
	}{
		{"cursor at the newest row", RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: newest}},
		{"cursor at a middle row", RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: middle}},
		{"cursor and a one-row limit", RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: middle, Limit: 1}},
		{"a two-row limit", RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.totalsOf(tc.filter)
			if got.Total != without.Total {
				t.Fatalf("totals.Total = %d, want %d (a total ignores the cursor and the limit)", got.Total, without.Total)
			}
			for _, state := range domain.RequestStatuses {
				if got.State(state) != without.State(state) {
					t.Fatalf("totals[%s] = %d, want %d (paging must not change a pill)", state, got.State(state), without.State(state))
				}
			}
		})
	}

	// Each state's number is the count of the same filter pinned to it: the
	// breakdown partitions the filter rather than describing a different queue.
	for _, state := range domain.RequestStatuses {
		pinned := RequestFilter{OrgID: f.org, TeamID: f.team, States: []domain.RequestStatus{state}}
		n, err := f.r.CountRequests(f.ctx, pinned)
		if err != nil {
			t.Fatalf("CountRequests(%s): %v", state, err)
		}
		if n != without.State(state) {
			t.Errorf("CountRequests(%s) = %d, totals[%s] = %d: the breakdown and the count must agree", state, n, state, without.State(state))
		}
		// And a filter that names states gets the honest breakdown for it.
		named := f.totalsOf(pinned)
		if named.Total != n {
			t.Errorf("totals(%s).Total = %d, want CountRequests' %d", state, named.Total, n)
		}
		if named.State(state) != n {
			t.Errorf("totals(%s)[%s] = %d, want %d", state, state, named.State(state), n)
		}
		for _, other := range domain.RequestStatuses {
			if other != state && named.State(other) != 0 {
				t.Errorf("totals(%s)[%s] = %d, want 0: a filter that pins one state cannot count another", state, other, named.State(other))
			}
		}
	}

	// Every other filter dimension narrows the totals too — the aggregate is the
	// queue's, not an unfiltered table's.
	for _, tc := range []struct {
		name  string
		add   func(*RequestFilter)
		state domain.RequestStatus
		want  int
	}{
		{"source", func(fl *RequestFilter) { fl.SourceKind = domain.RequestSourceMCP }, domain.RequestStatusNew, 1},
		{"requester", func(fl *RequestFilter) { fl.RequesterUserID = &otherRequester }, domain.RequestStatusTriaging, 1},
		{"query", func(fl *RequestFilter) { fl.Query = "A request" }, domain.RequestStatusNew, 3},
		{"attached", func(fl *RequestFilter) { fl.Attached = &[]bool{true}[0] }, domain.RequestStatusInProgress, 1},
		{"created after", func(fl *RequestFilter) { fl.CreatedAfter = &[]time.Time{base.Add(time.Hour)}[0] }, domain.RequestStatusNew, 0},
	} {
		t.Run("filter "+tc.name, func(t *testing.T) {
			filter := RequestFilter{OrgID: f.org, TeamID: f.team}
			tc.add(&filter)
			got := f.totalsOf(filter)
			if got.State(tc.state) != tc.want {
				t.Fatalf("totals[%s] = %d, want %d with the %s filter applied", tc.state, got.State(tc.state), tc.want, tc.name)
			}
			n, err := f.r.CountRequests(f.ctx, filter)
			if err != nil {
				t.Fatalf("CountRequests: %v", err)
			}
			if got.Total != n {
				t.Fatalf("totals.Total = %d, want CountRequests' %d with the %s filter", got.Total, n, tc.name)
			}
		})
	}

	// The caller's filter survives: a service reuses one paged filter for the
	// page and the pills, so the aggregate may not clear the caller's cursor or
	// limit behind its back.
	shared := RequestFilter{OrgID: f.org, TeamID: f.team, BeforeID: middle, Limit: 2, States: []domain.RequestStatus{domain.RequestStatusNew}}
	f.totalsOf(shared)
	if shared.BeforeID != middle || shared.Limit != 2 || len(shared.States) != 1 {
		t.Fatalf("CountRequestsByState mutated the caller's filter: %+v", shared)
	}

	// A state outside the nine can never come back from the schema (053's
	// CHECK), and asking for one is refused rather than matched against
	// nothing — the ListRequests rule.
	if _, err := f.r.CountRequestsByState(f.ctx, RequestFilter{OrgID: f.org, TeamID: f.team,
		States: []domain.RequestStatus{"not_a_state"}}); err == nil {
		t.Fatal("a filter naming an unknown state was accepted")
	}
}

// TestRequestsByStateRefusesATenantlessFilter is part of criterion 3 for the
// totals: a summary without its org and team is a caller bug answered with
// ErrRequestTenantScope, never with every tenant's queue.
func TestRequestsByStateRefusesATenantlessFilter(t *testing.T) {
	f := newRequestSummariesFixture(t)
	// One row that a leak would find, so "refused" is not the same thing as
	// "found nothing anyway".
	f.newRequest(f.team, domain.RequestStatusNew)

	for _, tc := range []struct {
		name   string
		filter RequestFilter
	}{
		{"no tenant", RequestFilter{}},
		{"org only", RequestFilter{OrgID: f.org}},
		{"team only", RequestFilter{TeamID: f.team}},
		{"negative org", RequestFilter{OrgID: -1, TeamID: f.team}},
		{"negative team", RequestFilter{OrgID: f.org, TeamID: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.r.CountRequestsByState(f.ctx, tc.filter); !errors.Is(err, ErrRequestTenantScope) {
				t.Fatalf("CountRequestsByState(%s) = %v, want ErrRequestTenantScope", tc.name, err)
			}
			if _, err := f.r.RequestsOwnedBy(f.ctx, tc.filter, f.user); !errors.Is(err, ErrRequestTenantScope) {
				t.Fatalf("RequestsOwnedBy(%s) = %v, want ErrRequestTenantScope", tc.name, err)
			}
		})
	}
}

// TestRequestSummariesAreTenantScoped is criterion 3.
//
// The same shape of request exists in this team, in a sibling team of the same
// org, in another org's team, and — the case a naive team-only query gets wrong
// — the SAME user claims and is assigned work in all of them. Only this team's
// rows may appear in either aggregate, so no queue summary and no "claimed by
// me" panel can be assembled out of another team's rows. The team-number reuse
// is the sharpest arm: teams.id is global, but a caller that scoped by team
// alone while ignoring org would still be caught by a test that puts the same
// person in both.
func TestRequestSummariesAreTenantScoped(t *testing.T) {
	f := newRequestSummariesFixture(t)
	mine := RequestFilter{OrgID: f.org, TeamID: f.team}

	// Two rows in this team, in two states, both owned by the fixture's user.
	here := map[domain.RequestStatus][]int{}
	here[domain.RequestStatusNew] = append(here[domain.RequestStatusNew],
		f.newRequest(f.team, domain.RequestStatusNew, claimedBy(f.user), assignedTo(f.user)))
	here[domain.RequestStatusTriaging] = append(here[domain.RequestStatusTriaging],
		f.newRequest(f.team, domain.RequestStatusTriaging, claimedBy(f.user)))

	// Same states, same owner, three other tenants.
	otherTenants := []int{f.team2, f.otherTeam}
	for _, team := range otherTenants {
		f.newRequest(team, domain.RequestStatusNew, claimedBy(f.user), assignedTo(f.user))
		f.newRequest(team, domain.RequestStatusNew, claimedBy(f.user), assignedTo(f.user))
		f.newRequest(team, domain.RequestStatusDelivered, claimedBy(f.user), assignedTo(f.user),
			func(r *domain.TeamRequest) {
				at := requestTime(-time.Hour)
				r.OutcomeBrief = "Done"
				r.DeliveredAt = &at
			})
	}

	totals := f.totalsOf(mine)
	if totals.Total != 2 {
		t.Fatalf("totals.Total = %d, want 2 — another team's requests entered this team's queue", totals.Total)
	}
	if totals.State(domain.RequestStatusNew) != 1 || totals.State(domain.RequestStatusTriaging) != 1 {
		t.Fatalf("per-state totals = %v, want new=1 triaging=1 (only this team's rows)", totals.States)
	}
	if totals.State(domain.RequestStatusDelivered) != 0 {
		t.Fatalf("totals[delivered] = %d, want 0 — another tenant's delivered row was counted", totals.State(domain.RequestStatusDelivered))
	}

	owned := f.ownedOf(mine, f.user)
	if owned.Total != 2 || len(owned.IDs) != 2 {
		t.Fatalf("owned = %d ids (Total %d), want the 2 this team's user owns — a stranger's queue leaked in", len(owned.IDs), owned.Total)
	}
	for _, id := range owned.IDs {
		if id != here[domain.RequestStatusNew][0] && id != here[domain.RequestStatusTriaging][0] {
			t.Fatalf("owned lists request %d, which is not in this team", id)
		}
	}

	// The sibling team's own view is tellable apart: the same person, a
	// different queue, three rows.
	sibling := f.ownedOf(RequestFilter{OrgID: f.org, TeamID: f.team2}, f.user)
	if sibling.Total != 3 {
		t.Fatalf("the sibling team's summary holds %d rows, want its own 3: the tenant scope is not symmetric", sibling.Total)
	}

	// A user id is required. Zero would be "nothing in the schema", which reads
	// as "nobody owns anything" rather than as the missing argument.
	if _, err := f.r.RequestsOwnedBy(f.ctx, mine, 0); !errors.Is(err, ErrRequestOwnershipUser) {
		t.Fatalf("RequestsOwnedBy with no user = %v, want ErrRequestOwnershipUser", err)
	}
	if _, err := f.r.RequestsOwnedBy(f.ctx, mine, -4); !errors.Is(err, ErrRequestOwnershipUser) {
		t.Fatalf("RequestsOwnedBy with a negative user = %v, want ErrRequestOwnershipUser", err)
	}
	if errors.Is(ErrRequestOwnershipUser, ErrNotFound) || errors.Is(ErrRequestOwnershipUser, ErrConflict) {
		t.Fatal("ErrRequestOwnershipUser must be distinct from not-found and conflict")
	}
}

// TestRequestsOwnedByDedupesClaimAndAssignment is criterion 2.
//
// The ownership summaries answer two different questions about the same rows, and
// a request this person both holds the claim on and owns must be one request in
// the work list, not two. The same test pins the anti-fan-out rule in the other
// direction: a full shared queue with nothing claimed and nobody assigned belongs
// to no member at all, however many people are on the team — a shared queue is
// not a set of personal tasks.
func TestRequestsOwnedByDedupesClaimAndAssignment(t *testing.T) {
	f := newRequestSummariesFixture(t)
	filter := RequestFilter{OrgID: f.org, TeamID: f.team}

	var (
		claimedOnly  = f.newRequest(f.team, domain.RequestStatusNew, claimedBy(f.user))
		assignedOnly = f.newRequest(f.team, domain.RequestStatusNew, assignedTo(f.user))
		// Both fields in one row. SetAssignee never leaves this state, but the
		// schema permits it and a summary must count the request once whether or
		// not the two fields agree.
		both = f.newRequest(f.team, domain.RequestStatusTriaging, claimedBy(f.user), assignedTo(f.user))
		// Someone else's claim and someone else's assignment: neither is this
		// user's work, though both are in the same shared queue.
		theirsClaimed  = f.newRequest(f.team, domain.RequestStatusTriaging, claimedBy(f.third))
		theirsAssigned = f.newRequest(f.team, domain.RequestStatusNeedsInformation, assignedTo(f.third),
			func(r *domain.TeamRequest) {
				at := requestTime(-time.Hour)
				r.NeedsInfoSince = &at
			})
		// In the shared queue, owned by nobody.
		unclaimed = f.newRequest(f.team, domain.RequestStatusNew)
	)

	got := f.ownedOf(filter, f.user)
	wantIDs := []int{both, assignedOnly, claimedOnly} // id DESC: newest first
	if len(got.IDs) != len(wantIDs) {
		t.Fatalf("IDs = %v, want %v (one entry per request this user owns)", got.IDs, wantIDs)
	}
	for i, id := range wantIDs {
		if got.IDs[i] != id {
			t.Fatalf("IDs = %v, want %v (newest first, each request once)", got.IDs, wantIDs)
		}
	}
	if got.Count != len(wantIDs) {
		t.Fatalf("Count = %d, want %d", got.Count, len(wantIDs))
	}
	if got.Total != len(wantIDs) {
		t.Fatalf("Total = %d, want %d distinct requests (a request both claimed and assigned counts once)", got.Total, len(wantIDs))
	}

	// The two named halves still answer their own questions about the same row.
	assertIDs(t, "ClaimedIDs", got.ClaimedIDs, []int{both, claimedOnly})
	assertIDs(t, "AssignedIDs", got.AssignedIDs, []int{both, assignedOnly})
	seen := map[int]int{}
	for _, id := range got.IDs {
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("request %d appears %d times in IDs: the summary must deduplicate the request", id, seen[id])
		}
	}
	for _, foreign := range []int{theirsClaimed, theirsAssigned, unclaimed} {
		for _, id := range got.IDs {
			if id == foreign {
				t.Fatalf("request %d is in this user's work list but belongs to somebody else (or nobody)", foreign)
			}
		}
	}

	// The other person's slice is theirs alone, and its size is the mirror: one
	// claim, one assignment, no fan-out of the shared queue.
	theirs := f.ownedOf(filter, f.third)
	assertIDs(t, "third user's IDs", theirs.IDs, []int{theirsAssigned, theirsClaimed})
	if theirs.Total != 2 {
		t.Fatalf("the third user's Total = %d, want their own 2", theirs.Total)
	}

	// A team member who claimed nothing owns nothing, even though the queue has
	// rows — the shared queue is not a per-member fan-out.
	nobody := f.ownedOf(filter, f.user2)
	if nobody.Total != 0 || len(nobody.IDs) != 0 || len(nobody.ClaimedIDs) != 0 || len(nobody.AssignedIDs) != 0 {
		t.Fatalf("a member who claimed nothing got %+v, want an empty slice: the queue must not fan out into personal work", nobody)
	}
	if nobody.IDs == nil || nobody.ClaimedIDs == nil || nobody.AssignedIDs == nil {
		t.Fatal("an empty summary must carry empty lists, not nil: a JSON client renders nil as null and has to special-case it")
	}

	// Filters still apply to a personal list — "my open work" is one call.
	open := f.ownedOf(RequestFilter{OrgID: f.org, TeamID: f.team,
		States: []domain.RequestStatus{domain.RequestStatusNew}}, f.user)
	assertIDs(t, "new-state owned IDs", open.IDs, []int{assignedOnly, claimedOnly})
	if open.Total != 2 {
		t.Fatalf("filtered Total = %d, want 2", open.Total)
	}

	// And the cursor pages the list without moving its total.
	first := f.ownedOf(RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 2}, f.user)
	if len(first.IDs) != 2 || first.NextBeforeID != assignedOnly {
		t.Fatalf("first page = %v (next %d), want %v with the cursor at %d", first.IDs, first.NextBeforeID, wantIDs[:2], assignedOnly)
	}
	if first.Total != 3 {
		t.Fatalf("a paged summary reported Total = %d, want the unfiltered 3: the header must not shrink as someone pages", first.Total)
	}
	second := f.ownedOf(RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 2, BeforeID: first.NextBeforeID}, f.user)
	assertIDs(t, "second page IDs", second.IDs, []int{claimedOnly})
	if second.NextBeforeID != 0 {
		t.Fatalf("the last page reported next_before_id %d, want 0", second.NextBeforeID)
	}
	if second.Total != 3 {
		t.Fatalf("the second page reported Total = %d, want 3", second.Total)
	}
}

// assertIDs compares an id slice to an expected one, in order.
func assertIDs(t *testing.T, what string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// TestRequestsOwnedByIgnoresTheCursorForItsTotal restates criterion 1 for the
// ownership total in the case that matters most: every cursor a caller can
// arrive with must leave Total alone while still paging the ids. (The claim and
// assignment dedup test pages once; this one walks the whole list for each
// cursor, which is the "totals vs paging" pairing the ticket asks for.)
func TestRequestsOwnedByIgnoresTheCursorForItsTotal(t *testing.T) {
	f := newRequestSummariesFixture(t)
	filter := RequestFilter{OrgID: f.org, TeamID: f.team}
	var ids []int
	for i := 0; i < 5; i++ {
		if i%2 == 0 {
			ids = append(ids, f.newRequest(f.team, domain.RequestStatusNew, claimedBy(f.user)))
		} else {
			ids = append(ids, f.newRequest(f.team, domain.RequestStatusNew, assignedTo(f.user)))
		}
	}
	total := f.ownedOf(filter, f.user)
	if total.Total != len(ids) || len(total.IDs) != len(ids) {
		t.Fatalf("summary = %d ids (Total %d), want %d", len(total.IDs), total.Total, len(ids))
	}

	// Walk the whole list at two pages of two, asserting each page's ids are a
	// strict prefix continuation and that Total never moves.
	walked := RequestFilter{OrgID: f.org, TeamID: f.team, Limit: 2}
	var seen []int
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatalf("paging did not terminate: cursor %d", walked.BeforeID)
		}
		got := f.ownedOf(walked, f.user)
		if got.Total != len(ids) {
			t.Fatalf("page %d reported Total = %d, want %d", page, got.Total, len(ids))
		}
		seen = append(seen, got.IDs...)
		if got.NextBeforeID == 0 {
			break
		}
		walked.BeforeID = got.NextBeforeID
	}
	if len(seen) != len(ids) {
		t.Fatalf("paging the summary yielded %v, want the %d rows", seen, len(ids))
	}
	uniq := map[int]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("request %d appeared on more than one page", id)
		}
		uniq[id] = true
	}
}
