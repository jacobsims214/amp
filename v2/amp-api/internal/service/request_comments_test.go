package service

// Tests for the request's discussion read (AMP #4270, epic 202 story 549).
//
// The detail read's own tests (request_views_test.go) prove who may see a
// request; this file asks only the questions the THREAD adds, and it asks them
// against the same fixture so a thread case cannot pass by having a different
// world from the detail case it is supposed to match:
//
//  1. THE TWO ARMS ARE THE TWO VIEWS. An inbox member reads every row of the
//     thread — the internal remarks and the system notes included, because
//     triage works from the record — while the requester reads only the shared
//     kinds, attributed "you"/"the desk", in the projection's own type (no id,
//     no author name, no user id). The requester's arm is
//     requesterSharedComments + requesterComments, so it cannot show a row the
//     detail page would hide (TestListRequestComments_TeamAndRequesterArms,
//     TestListRequestComments_RequesterArmCannotWiden).
//  2. THE AUTHORIZATION IS THE DETAIL READ'S. Every way of not reading a request
//     is the same not-found — another requester's request, a sibling team's
//     request, another org's request, a stranger naming the inbox, an id that
//     never existed — and a submit-only credential is refused before a request
//     is loaded (TestListRequestComments_RefusalsAreTheDetailReads).
//  3. THE PAGE IS THE REPOSITORY'S, FORWARD. after_id and limit are passed
//     through, the page walks the thread exactly once in append order, and the
//     cursor appears only when another page exists
//     (TestListRequestComments_PagesForward).
//
// The database-backed cases run against a real Postgres with every migration
// applied, when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'RequestComment' -count=1

import (
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// TestListRequestComments_TeamAndRequesterArms is criteria 1 and 2's positive
// half: the same request, read by the desk and by its requester, answers two
// different lists — and the difference is exactly the sharing rule the detail
// page's projection already applies.
func TestListRequestComments_TeamAndRequesterArms(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusNeedsInformation, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
		// 053's CHECK: needs_information carries the moment it started.
		r.NeedsInfoSince = timePtr(time.Now().UTC().Truncate(time.Microsecond))
	})
	// Four rows, one per kind, plus one internal remark by a team member — the
	// case the sharing rule exists for.
	f.comment(reqID, domain.RequestCommentDiscussion, "Internal triage note: route this to the platform team.", intRef(f.member))
	f.comment(reqID, domain.RequestCommentQuestion, "Which quarter is this for?", intRef(f.member))
	f.comment(reqID, domain.RequestCommentAnswer, "Q3, for the September release.", intRef(f.customer))
	f.comment(reqID, domain.RequestCommentSystem, "Internal system note: state changed to needs_information by the desk.", nil)
	f.comment(reqID, domain.RequestCommentDiscussion, "A remark of my own the desk should see.", intRef(f.customer))
	const total = 5

	// The team's arm: the thread, every row, in append order.
	team, err := f.svc.ListRequestComments(f.ctx, f.principal(f.member, ""), RequestThreadInput{
		RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: reqID},
	})
	if err != nil {
		t.Fatalf("team thread: %v", err)
	}
	if !team.View.Full() {
		t.Fatalf("an inbox member's thread view = %s, want the team's", team.View)
	}
	if len(team.Comments) != total {
		t.Fatalf("the team's thread holds %d rows, want all %d (every kind, internal ones included)", len(team.Comments), total)
	}
	if team.Count != total || team.NextAfterID != 0 {
		t.Fatalf("team page = count %d, cursor %d; want %d and no cursor", team.Count, team.NextAfterID, total)
	}
	if team.Shared != nil {
		t.Fatalf("the team's answer also carries a requester projection: %+v", team.Shared)
	}
	for i := 1; i < len(team.Comments); i++ {
		if team.Comments[i].ID <= team.Comments[i-1].ID {
			t.Fatalf("the team's thread is not in append order: %d then %d", team.Comments[i-1].ID, team.Comments[i].ID)
		}
	}
	// The rows are the stored rows: the id, the request and the attribution are
	// all there, which is what the UI's RequestComment type expects.
	first := team.Comments[0]
	if first.RequestID != reqID || first.Kind != domain.RequestCommentDiscussion || first.Author == "" || first.ID == 0 {
		t.Fatalf("a team row lost its own fields: %+v", first)
	}

	// The requester's arm: the shared kinds only, in the projection's type.
	own, err := f.svc.ListRequestComments(f.ctx, f.principal(f.customer, ""), RequestThreadInput{
		RequestReadInput: RequestReadInput{RequestID: reqID},
	})
	if err != nil {
		t.Fatalf("requester thread: %v", err)
	}
	if !own.View.Own() {
		t.Fatalf("the requester's thread view = %s, want \"requester\"", own.View)
	}
	if own.Comments != nil {
		t.Fatalf("the requester's answer also carries the team's rows: %+v", own.Comments)
	}
	// question + answer + their own remark. The member's internal note and the
	// system row are not shared.
	if len(own.Shared) != 3 {
		t.Fatalf("the requester's thread holds %d rows, want 3 (a question, an answer, their own remark): %+v", len(own.Shared), own.Shared)
	}
	var bodies []string
	for _, c := range own.Shared {
		bodies = append(bodies, c.Body)
		if c.Kind == domain.RequestCommentSystem {
			t.Fatalf("a system row reached the requester: %+v", c)
		}
		if strings.Contains(c.Body, "Internal") {
			t.Fatalf("internal text reached the requester: %q", c.Body)
		}
	}
	joined := strings.Join(bodies, " | ")
	for _, want := range []string{"Which quarter", "Q3, for the September", "A remark of my own"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the requester's thread is missing %q: %s", want, joined)
		}
	}
	// Attribution is the projection's two words: the desk asked and answered,
	// the requester wrote their own remark.
	wantFrom := []domain.RequesterAuthor{
		domain.RequesterAuthorDesk, // the question
		domain.RequesterAuthorYou,  // the answer
		domain.RequesterAuthorYou,  // their own remark
	}
	for i, c := range own.Shared {
		if c.From != wantFrom[i] {
			t.Fatalf("shared row %d (%q) from = %q, want %q", i, c.Body, c.From, wantFrom[i])
		}
	}
	// A requester's rows carry no cursor: their type has no id to page with.
	if own.NextAfterID != 0 {
		t.Fatalf("the requester's answer carries cursor %d, but their rows have no id", own.NextAfterID)
	}
}

// TestListRequestComments_RequesterArmCannotWiden pins the sharing rule against
// the one thing a caller could try: naming the request through the team surface.
// A requester who is not on the inbox gets the same answer either way — the
// projection's shared kinds — because the view, not the surface, decides.
func TestListRequestComments_RequesterArmCannotWiden(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	f.comment(reqID, domain.RequestCommentDiscussion, "Internal note nobody outside the desk may read.", intRef(f.member))
	f.comment(reqID, domain.RequestCommentQuestion, "Could you confirm the scope?", intRef(f.member))
	f.comment(reqID, domain.RequestCommentSystem, "System note: claimed by the desk.", intRef(f.lead))

	customer := f.principal(f.customer, "")
	for _, tc := range []struct {
		name string
		in   RequestThreadInput
	}{
		{"by serial", RequestThreadInput{RequestReadInput: RequestReadInput{RequestID: reqID}}},
		{"naming the inbox", RequestThreadInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: reqID}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread, err := f.svc.ListRequestComments(f.ctx, customer, tc.in)
			if err != nil {
				t.Fatalf("requester thread: %v", err)
			}
			if thread.Comments != nil {
				t.Fatalf("%s: the requester received the team's rows: %+v", tc.name, thread.Comments)
			}
			if len(thread.Shared) != 1 || thread.Shared[0].Kind != domain.RequestCommentQuestion {
				t.Fatalf("%s: shared = %+v, want only the question", tc.name, thread.Shared)
			}
		})
	}
}

// TestListRequestComments_RefusalsAreTheDetailReads is criterion 2's negative
// half: every way of not being allowed to read a request is the detail read's
// own not-found, message for message, and a submit-only credential is refused
// before a request is loaded.
func TestListRequestComments_RefusalsAreTheDetailReads(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	own := f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	anothers := f.request(f.team, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.plain)
	})
	sibling := f.request(f.otherTeam, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.plain)
	})
	foreign := f.request(f.foreign, &seq, domain.RequestStatusNew, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.stranger)
	})
	f.comment(own, domain.RequestCommentQuestion, "Which quarter?", intRef(f.member))

	customer := f.principal(f.customer, "")
	stranger := f.principal(f.stranger, "")
	member := f.principal(f.member, "")

	probes := []struct {
		name string
		p    authz.Principal
		in   RequestThreadInput
	}{
		{"another requester's request", customer, RequestThreadInput{RequestReadInput: RequestReadInput{RequestID: anothers}}},
		{"a sibling team's request named through this inbox", member, RequestThreadInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: sibling}}},
		{"another org's request named through this inbox", member, RequestThreadInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: foreign}}},
		{"a stranger naming the inbox", stranger, RequestThreadInput{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: own}}},
		{"an id that never existed", customer, RequestThreadInput{RequestReadInput: RequestReadInput{RequestID: 2_000_000_000}}},
		{"an unissued team", member, RequestThreadInput{RequestReadInput: RequestReadInput{TeamID: 2_000_000_000, RequestID: own}}},
	}
	var want string
	for _, pr := range probes {
		_, err := f.svc.ListRequestComments(f.ctx, pr.p, pr.in)
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

	// The same refusal the detail read gives, for the same request: the thread
	// read is the detail read's decision, so the two messages must agree.
	_, detailErr := f.svc.GetRequest(f.ctx, customer, RequestReadInput{RequestID: anothers})
	if !isRequestNotFound(detailErr) || detailErr.Error() != want {
		t.Fatalf("the detail read refuses with %q, the thread with %q: they must be one answer", detailErr, want)
	}

	// A submit-only credential reads no thread either, on either surface.
	scoped := f.principal(f.customer, auth.ChannelMCP)
	scoped.Scope = domain.TokenScopeSubmit
	scoped.ScopeTeamID = f.team
	for _, in := range []RequestThreadInput{
		{RequestReadInput: RequestReadInput{RequestID: own}},
		{RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: own}},
	} {
		_, err := f.svc.ListRequestComments(f.ctx, scoped, in)
		if authz.CodeOf(err) != authz.CodeTokenScopeDenied {
			t.Fatalf("submit-only thread read = %v (code %q), want %q", err, authz.CodeOf(err), authz.CodeTokenScopeDenied)
		}
	}
}

// TestListRequestComments_PagesForward is criterion 3: after_id and limit reach
// the repository, the pages add up to the thread exactly once, and the cursor
// is present only when there is another page.
func TestListRequestComments_PagesForward(t *testing.T) {
	f := newViewsFixture(t)
	seq := 0
	reqID := f.request(f.team, &seq, domain.RequestStatusTriaging, func(r *domain.TeamRequest) {
		r.RequesterUserID = intRef(f.customer)
	})
	const total = 7
	for i := 0; i < total; i++ {
		f.comment(reqID, domain.RequestCommentDiscussion, "row "+string(rune('a'+i)), intRef(f.lead))
	}
	member := f.principal(f.lead, "")

	var ids []int64
	var cursor int64
	for page := 0; page < 20; page++ {
		thread, err := f.svc.ListRequestComments(f.ctx, member, RequestThreadInput{
			RequestReadInput: RequestReadInput{TeamID: f.team, RequestID: reqID},
			AfterID:          cursor,
			Limit:            3,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if thread.Count != len(thread.Comments) {
			t.Fatalf("page %d: count %d != %d rows", page, thread.Count, len(thread.Comments))
		}
		if len(thread.Comments) > 3 {
			t.Fatalf("page %d holds %d rows for a limit of 3", page, len(thread.Comments))
		}
		for _, c := range thread.Comments {
			ids = append(ids, c.ID)
		}
		if thread.NextAfterID == 0 {
			break
		}
		cursor = thread.NextAfterID
	}
	if len(ids) != total {
		t.Fatalf("paging yielded %d rows, want %d: %v", len(ids), total, ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("paging is not in strictly increasing id order: %v", ids)
		}
	}

	// A page that lands on the last row has no cursor; one row short, it does.
	last, err := f.svc.ListRequestComments(f.ctx, member, RequestThreadInput{
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

	// The requester's arm ignores the cursor it was handed, because its rows have
	// no id to continue from: a requester asking "after 999" still gets the
	// shared rows they may read, never a page keyed on somebody else's comment.
	own, err := f.svc.ListRequestComments(f.ctx, f.principal(f.customer, ""), RequestThreadInput{
		RequestReadInput: RequestReadInput{RequestID: reqID},
		AfterID:          999,
		Limit:            3,
	})
	if err != nil {
		t.Fatalf("requester page: %v", err)
	}
	if own.NextAfterID != 0 {
		t.Fatalf("the requester's answer carries cursor %d", own.NextAfterID)
	}
	if own.Comments != nil {
		t.Fatalf("the requester received team rows: %+v", own.Comments)
	}
}
