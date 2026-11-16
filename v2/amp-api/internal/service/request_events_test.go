package service

// Tests for the request's history read (AMP #4271, epic 202 story 549).
//
// The detail read's own tests (request_views_test.go) prove who may see a
// request, and request_comments_test.go proves the thread's two arms. This file
// asks only the questions the HISTORY read adds, against the same fixture so a
// history case cannot pass by having a different world from the detail case it
// is supposed to match:
//
//  1. THE TEAM READS THE ROWS, IN HISTORY ORDER. An inbox member gets every act
//     the request has, in append order, with each row's own fields — the actor,
//     the action, the detail — because triage works from the record
//     (TestListRequestEvents_TeamReadsTheRows).
//  2. THE REQUESTER IS REFUSED, NOT FILTERED. A history row names internal
//     actors, internal state names and id-only details, and the projection has
//     no field for any of them, so a requester's read is the surface's one 404 —
//     the same bytes the detail read gives for something they may not see, never
//     a filtered list and never a 403 (TestListRequestEvents_RequesterIsRefused).
//  3. THE AUTHORIZATION IS THE DETAIL READ'S. Every way of not reading a request
//     is the same not-found — another requester's request, a sibling team's
//     request, another org's request, a stranger naming the inbox, an id that
//     never existed, an unissued team — and a submit-only credential is refused
//     before a request is loaded (TestListRequestEvents_RefusalsAreTheDetailReads).
//  4. THE PAGE IS THE REPOSITORY'S, FORWARD. after_id and limit are passed
//     through, the page walks the history exactly once in append order, and the
//     cursor appears only when another page exists
//     (TestListRequestEvents_PagesForward).
//
// The database-backed cases run against a real Postgres with every migration
// applied, when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'RequestEvent' -count=1

import (
	"fmt"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// event appends one history row to requestID in the fixture's own tenant and
// returns its id.
//
// It writes the table rather than driving an act, because this file is about the
// READ: "seven rows, in this order" would otherwise be seven lifecycle steps the
// test would have to keep legal, and the act paths' own tests already prove they
// write these rows. The columns are 054's; from_state/to_state stay empty
// because only a transition fills them (the CHECK would refuse anything else).
func (f *viewsFixture) event(requestID int, action domain.RequestEventAction, actor string, actorUserID *int, detail string) int64 {
	f.t.Helper()
	var id int64
	var orgID, teamID int
	if err := f.db.QueryRow(f.ctx, `SELECT org_id, team_id FROM team_requests WHERE id = $1`, requestID).
		Scan(&orgID, &teamID); err != nil {
		f.t.Fatalf("tenant of request %d: %v", requestID, err)
	}
	if err := f.db.QueryRow(f.ctx, `
		INSERT INTO request_events (request_id, org_id, team_id, action, actor, actor_user_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		requestID, orgID, teamID, string(action), actor, actorUserID, detail).Scan(&id); err != nil {
		f.t.Fatalf("insert event on request %d: %v", requestID, err)
	}
	return id
}

// TestListRequestEvents_TeamReadsTheRows is criteria 1's positive half: an inbox
// member gets every row, in append order, with the fields the UI's RequestEvent
// is typed on.
func TestListRequestEvents_TeamReadsTheRows(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	// Four acts of four different kinds, one per verb the rail colours
	// differently: a submit, a claim, a transfer and a link.
	submit := f.event(reqID, domain.RequestActionSubmit, "member@views.test", intRef(f.member), `{"source_kind":"web"}`)
	claim := f.event(reqID, domain.RequestActionClaim, "member@views.test", intRef(f.member), "")
	assign := f.event(reqID, domain.RequestActionAssign, "lead@views.test", intRef(f.lead), `{"previous_owner_user_id":null}`)
	link := f.event(reqID, domain.RequestActionLink, "lead@views.test", intRef(f.lead), `{"link_kind":"planned_as","target_kind":"task","target_id":42}`)
	want := []int64{submit, claim, assign, link}

	history, err := f.svc.ListRequestEvents(f.ctx, f.principal(f.member, ""), RequestHistoryInput{
		RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: reqID},
	})
	if err != nil {
		t.Fatalf("team history: %v", err)
	}
	if !history.View.Full() {
		t.Fatalf("an inbox member's history view = %s, want the team's", history.View)
	}
	if history.RequestID != reqID {
		t.Fatalf("history request_id = %d, want %d", history.RequestID, reqID)
	}
	if history.Count != len(want) || len(history.Events) != len(want) {
		t.Fatalf("history holds %d rows (count %d), want %d", len(history.Events), history.Count, len(want))
	}
	if history.NextAfterID != 0 {
		t.Fatalf("a page holding the whole history carries cursor %d", history.NextAfterID)
	}
	for i, id := range want {
		if history.Events[i].ID != id {
			t.Fatalf("row %d is event %d, want %d (append order, by id)", i, history.Events[i].ID, id)
		}
	}
	// The rows are the stored rows: the request, the tenant, the action, the
	// actor and the detail are all there, which is what the UI's RequestEvent
	// type is built from.
	first := history.Events[0]
	if first.RequestID != reqID || first.OrgID != f.org || first.TeamID != f.team {
		t.Fatalf("a team row lost its identity: %+v", first)
	}
	if first.Action != domain.RequestActionSubmit || first.Actor == "" || first.ActorUserID == nil || *first.ActorUserID != f.member {
		t.Fatalf("a team row lost its attribution: %+v", first)
	}
	if first.Detail != `{"source_kind":"web"}` || first.CreatedAt.IsZero() {
		t.Fatalf("a team row lost its detail or its time: %+v", first)
	}

	// A requester who is also an inbox member gets the team's view and the
	// history with it — the view decides, not the surface they named.
	own := f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.member)
	})
	f.event(own, domain.RequestActionSubmit, "member@views.test", intRef(f.member), "")
	if got, err := f.svc.ListRequestEvents(f.ctx, f.principal(f.member, ""), RequestHistoryInput{
		RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: own},
	}); err != nil || got.Count != 1 {
		t.Fatalf("a member's own request = %d rows, %v; want 1", got.Count, err)
	}

	// A request with no acts is an empty list, not nil: the rail renders
	// "nothing has happened yet" from it.
	quiet := f.request(f.team, &seq, domain.RequestStatusNew)
	silent, err := f.svc.ListRequestEvents(f.ctx, f.principal(f.member, ""), RequestHistoryInput{
		RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: quiet},
	})
	if err != nil {
		t.Fatalf("history of a request with no acts: %v", err)
	}
	if silent.Events == nil || silent.Count != 0 {
		t.Fatalf("a request with no acts = %+v, want an empty non-nil list", silent)
	}
}

// TestListRequestEvents_RequesterIsRefused is criterion 1's negative half: a
// requester's projecton has no field for a history row, so the read refuses
// rather than filtering — with the surface's one 404, which is the same answer
// the detail read gives for anything a caller may not see.
func TestListRequestEvents_RequesterIsRefused(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	own := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	f.event(own, domain.RequestActionSubmit, "customer@views.test", intRef(f.customer), `{"requester_user_id":1}`)
	f.event(own, domain.RequestActionClaim, "member@views.test", intRef(f.member), "")
	// Another requester's request, which the detail read refuses: the history
	// read's refusal for the caller's OWN request must be the same bytes.
	anothers := f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.plain)
	})

	customer := f.principal(f.customer, "")
	// The detail read answers this requester's own request (200): they may read
	// it, and the projection is what they get.
	if read, err := f.svc.GetRequest(f.ctx, customer, RequestReadInput{RequestID: own}); err != nil || !read.View.Own() {
		t.Fatalf("the detail read refused the requester their own request: %v", err)
	}
	// The history read refuses it, whichever way the request is named.
	for _, tc := range []struct {
		name string
		in   RequestHistoryInput
	}{
		{"by serial", RequestHistoryInput{RequestReadInput: RequestReadInput{RequestID: own}}},
		{"naming the inbox", RequestHistoryInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: own}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.svc.ListRequestEvents(f.ctx, customer, tc.in)
			if err == nil {
				t.Fatalf("%s: a requester read the history: %+v", tc.name, got)
			}
			if !isRequestNotFound(err) {
				t.Fatalf("%s: err = %v, want the surface's not-found (404 request.not_found)", tc.name, err)
			}
			if got != nil {
				t.Fatalf("%s: a refusal also carried an answer: %+v", tc.name, got)
			}
		})
	}
	// And that refusal is the detail read's own message, not a second opinion:
	// the same bytes the detail read gives the same person for a request that is
	// not theirs.
	_, detailErr := f.svc.GetRequest(f.ctx, customer, RequestReadInput{RequestID: anothers})
	_, historyErr := f.svc.ListRequestEvents(f.ctx, customer, RequestHistoryInput{
		RequestReadInput: RequestReadInput{RequestID: own},
	})
	if !isRequestNotFound(detailErr) {
		t.Fatalf("the detail read = %v for another requester's request, want a not-found", detailErr)
	}
	if historyErr.Error() != detailErr.Error() {
		t.Fatalf("the history read refuses with %q, the detail read with %q: they must be one answer", historyErr, detailErr)
	}
}

// TestListRequestEvents_RefusalsAreTheDetailReads is criterion 2's negative
// half: every way of not being allowed to read a request is the detail read's
// own not-found, message for message, and a submit-only credential is refused
// before a request is loaded.
func TestListRequestEvents_RefusalsAreTheDetailReads(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	own := f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	sibling := f.request(f.otherTeam, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.plain)
	})
	foreign := f.request(f.foreign, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.stranger)
	})
	f.event(own, domain.RequestActionSubmit, "customer@views.test", intRef(f.customer), "")

	customer := f.principal(f.customer, "")
	stranger := f.principal(f.stranger, "")
	member := f.principal(f.member, "")

	probes := []struct {
		name string
		p    authz.Principal
		in   RequestHistoryInput
	}{
		{"a sibling team's request named through this inbox", member, RequestHistoryInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: sibling}}},
		{"another org's request named through this inbox", member, RequestHistoryInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: foreign}}},
		{"a stranger naming the inbox", stranger, RequestHistoryInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: own}}},
		{"an id that never existed", member, RequestHistoryInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: 2_000_000_000}}},
		{"an unissued team", member, RequestHistoryInput{RequestReadInput: RequestReadInput{TeamID: 2_000_000_000, RequestID: own}}},
		{"a caller with no name at all", customer, RequestHistoryInput{}},
	}
	var want string
	for _, pr := range probes {
		_, err := f.svc.ListRequestEvents(f.ctx, pr.p, pr.in)
		if pr.name == "a caller with no name at all" {
			// Naming neither a serial nor a reference is a caller bug, and the
			// refusal says so rather than hiding it behind a legitimate 404
			// (normalizeReadRef's rule, unchanged here).
			if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
				t.Fatalf("%s: err = %v (code %q), want %q", pr.name, err, code, authz.CodeRequestInvalid)
			}
			continue
		}
		if !isRequestNotFound(err) {
			t.Fatalf("%s: err = %v, want the surface's not-found", pr.name, err)
		}
		if want == "" {
			want = err.Error()
			continue
		}
		if err.Error() != want {
			t.Fatalf("%s answered %q, want the identical message %q", pr.name, err, want)
		}
	}

	// A submit-only credential reads no history either, on either surface, and
	// is refused before the request is loaded (so it cannot probe ids).
	scoped := f.principal(f.customer, auth.ChannelMCP)
	scoped.Scope = domain.TokenScopeSubmit
	scoped.ScopeTeamID = f.team
	for _, in := range []RequestHistoryInput{
		{RequestReadInput: RequestReadInput{RequestID: own}},
		{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: own}},
	} {
		_, err := f.svc.ListRequestEvents(f.ctx, scoped, in)
		if authz.CodeOf(err) != authz.CodeTokenScopeDenied {
			t.Fatalf("submit-only history read = %v (code %q), want %q", err, authz.CodeOf(err), authz.CodeTokenScopeDenied)
		}
	}
}

// TestListRequestEvents_PagesForward is criterion 3: after_id and limit reach
// the repository, the pages add up to the history exactly once, and the cursor
// is present only when there is another page.
func TestListRequestEvents_PagesForward(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	const total = 7
	actions := []domain.RequestEventAction{
		domain.RequestActionSubmit, domain.RequestActionClaim, domain.RequestActionRelease,
		domain.RequestActionAssign, domain.RequestActionLink, domain.RequestActionUnlink,
		domain.RequestActionEdit,
	}
	var stored []int64
	for i := 0; i < total; i++ {
		stored = append(stored, f.event(reqID, actions[i], fmt.Sprintf("actor-%d@views.test", i), intRef(f.lead), ""))
	}
	member := f.principal(f.lead, "")

	var ids []int64
	var cursor int64
	for page := 0; page < 20; page++ {
		history, err := f.svc.ListRequestEvents(f.ctx, member, RequestHistoryInput{
			RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: reqID},
			AfterID:          cursor,
			Limit:            3,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if history.Count != len(history.Events) {
			t.Fatalf("page %d: count %d != %d rows", page, history.Count, len(history.Events))
		}
		if len(history.Events) > 3 {
			t.Fatalf("page %d holds %d rows for a limit of 3", page, len(history.Events))
		}
		for _, e := range history.Events {
			ids = append(ids, e.ID)
		}
		if history.NextAfterID == 0 {
			break
		}
		cursor = history.NextAfterID
	}
	if len(ids) != total {
		t.Fatalf("paging yielded %d rows, want %d: %v", len(ids), total, ids)
	}
	for i := range stored {
		if ids[i] != stored[i] {
			t.Fatalf("paged row %d is %d, want %d: paging is not stable", i, ids[i], stored[i])
		}
	}

	// A page that lands on the last row has no cursor; one row short, it does.
	last, err := f.svc.ListRequestEvents(f.ctx, member, RequestHistoryInput{
		RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: reqID},
		AfterID:          ids[total-2],
		Limit:            5,
	})
	if err != nil {
		t.Fatalf("last page: %v", err)
	}
	if last.Count != 1 || last.NextAfterID != 0 {
		t.Fatalf("the last page = %d rows, cursor %d; want 1 and none", last.Count, last.NextAfterID)
	}
	// A limit above the repository's ceiling is clamped, never refused, and the
	// whole short history still arrives with no cursor.
	big, err := f.svc.ListRequestEvents(f.ctx, member, RequestHistoryInput{
		RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: reqID},
		Limit:            10_000,
	})
	if err != nil {
		t.Fatalf("clamped limit: %v", err)
	}
	if big.Count != total || big.NextAfterID != 0 {
		t.Fatalf("a clamped limit = %d rows, cursor %d; want %d and none", big.Count, big.NextAfterID, total)
	}
}
