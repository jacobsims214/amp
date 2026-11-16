package service

// The request mutation paths' proving tests (AMP #3959, epic 202 story 549;
// build slices S3 and S4 of discoveries/team-inbox-approved-build-slices.md).
//
// The ticket's three criteria are what these assert, in order:
//
//  1. the edit / claim / comment authorization matrix — who may do what, to
//     whose request, in what state (TestEditRequest_AuthorizationMatrix,
//     TestClaimAndRelease_AuthorizationAndConflicts,
//     TestAssignRequest_AuthorizationAndTransfer,
//     TestCommentOnRequest_AuthorizationAndVoice, and the pure
//     TestRequestCommentVoiceTable that pins the closed table behind them).
//  2. CAS conflicts carry the current revision and a replay audits nothing —
//     one audit row per act, none for a retry, for all four paths
//     (TestEditRequest_StaleRevisionConflicts, TestMutationReplayWritesAndAuditsNothing,
//     and the replay legs of the claim/assign/comment tests).
//  3. no actor or state spoofing: a payload cannot name the author, the actor,
//     the tenant, the claim or the assignee, and no mutation moves the lifecycle
//     (TestRequestMutations_CannotSpoofActorOrState).
//
// The database-backed tests run against a real Postgres with every migration
// applied, when TEST_DATABASE_URL is set, and reuse the submission fixture
// (request_submit_test.go) so the tenant, users and roles are set up once for
// both files:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'RequestMutation|EditRequest|Claim|Assign|CommentOnRequest|RequestCommentVoice|RequestUntriaged' -count=1 -race
//
// Every request these tests create carries the fixture's run-unique tenant, so
// repeated runs and other tests sharing the database never see each other's rows.

import (
	"errors"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- fixture helpers ----

// submitAs creates one request in the fixture's inbox as p and returns it.
func (f *submitFixture) submitAs(p authz.Principal, subject string) *domain.TeamRequest {
	f.t.Helper()
	res, err := f.svc.SubmitRequest(f.ctx, p, SubmitRequestInput{
		TeamID:     f.team,
		Source:     domain.RequestSourceWeb,
		Subject:    subject,
		InputBrief: "The brief that came with " + subject + ".",
	})
	if err != nil {
		f.t.Fatalf("submit %q as user %d: %v", subject, p.UserID, err)
	}
	return res.Request
}

// addTeamMember provisions a user with the given team role in the fixture's inbox
// and returns their id. It is what a second triager needs: a real account with a
// real membership, so authz reads it from the rows like any other caller's.
func (f *submitFixture) addTeamMember(label, role string) int {
	f.t.Helper()
	var id int
	if err := f.db.QueryRow(f.ctx,
		`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		"mut-"+label+"-"+idStr(f.team), "mut-"+label+"-"+idStr(f.team)+"@mut.test", domain.UserKindHuman).Scan(&id); err != nil {
		f.t.Fatalf("create %s: %v", label, err)
	}
	f.sqlExec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, $3)`, f.team, id, role)
	return id
}

// sqlExec runs fixture SQL (state moves, claim clearing) that no service under
// test owns.
func (f *submitFixture) sqlExec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// requestRow reads a request back through the service's own repository, so a
// test asserts what a caller would see rather than what an UPDATE left behind.
func (f *submitFixture) requestRow(requestID int) *domain.TeamRequest {
	f.t.Helper()
	req, err := f.svc.Repo.GetRequest(f.ctx, requestID, f.org, f.team)
	if err != nil {
		f.t.Fatalf("load request %d: %v", requestID, err)
	}
	return req
}

// eventCount is how many history rows of one action requestID has.
func (f *submitFixture) eventCount(requestID int, action domain.RequestEventAction) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx,
		`SELECT count(*) FROM request_events WHERE request_id = $1 AND action = $2`,
		requestID, string(action)).Scan(&n); err != nil {
		f.t.Fatalf("count %s events for %d: %v", action, requestID, err)
	}
	return n
}

// eventDetails returns the detail JSON of requestID's newest event of action.
func (f *submitFixture) eventDetails(requestID int, action domain.RequestEventAction) string {
	f.t.Helper()
	var detail string
	if err := f.db.QueryRow(f.ctx,
		`SELECT detail FROM request_events WHERE request_id = $1 AND action = $2 ORDER BY id DESC LIMIT 1`,
		requestID, string(action)).Scan(&detail); err != nil {
		f.t.Fatalf("read %s event detail for %d: %v", action, requestID, err)
	}
	return detail
}

// auditCount is how many audit rows one action has for requestID.
func (f *submitFixture) auditCount(action string, requestID int) int {
	f.t.Helper()
	entries, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: action, TargetID: idStr(requestID)})
	if err != nil {
		f.t.Fatalf("list %s audit for %d: %v", action, requestID, err)
	}
	return len(entries)
}

// commentCount is how many discussion rows requestID has.
func (f *submitFixture) commentCount(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count comments for %d: %v", requestID, err)
	}
	return n
}

// requireCode asserts err carries the request surface's stable code, status and
// (for the sentinels a caller branches on) the repository identity behind it.
func requireCode(t *testing.T, err error, code string, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s (%d), got nil", code, status)
	}
	if got := authz.CodeOf(err); got != code {
		t.Fatalf("error code = %q (%v), want %q", got, err, code)
	}
	if got := authz.StatusOf(err, 500); got != status {
		t.Fatalf("error status = %d (%v), want %d", got, err, status)
	}
}

func strPtr(s string) *string { return &s }

// ---- criterion 3, pure half: the voice table is closed ----

// TestRequestCommentVoiceTable pins the whole mapping: which kind belongs to which
// voice, that a plain comment belongs to both, and that `system` belongs to
// neither caller. It is exhaustive over the domain's four kinds, so a fifth kind
// added without a decision here fails rather than defaulting to somebody's voice.
func TestRequestCommentVoiceTable(t *testing.T) {
	cases := []struct {
		kind             domain.RequestCommentKind
		teamOK, reqOK    bool
		wantPostableKind bool
	}{
		{domain.RequestCommentDiscussion, true, true, true},
		{domain.RequestCommentQuestion, true, false, true},
		{domain.RequestCommentAnswer, false, true, true},
		{domain.RequestCommentSystem, false, false, false},
	}
	if len(cases) != len(domain.RequestCommentKinds) {
		t.Fatalf("%d cases for %d domain kinds: every kind needs a decision here", len(cases), len(domain.RequestCommentKinds))
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			if _, ok := requestCommentVoice(c.kind); ok != c.wantPostableKind {
				t.Fatalf("requestCommentVoice(%q) postable = %v, want %v", c.kind, ok, c.wantPostableKind)
			}
			if got := requestCommentVoiceAllowed(c.kind, requestVoiceTeam); got != c.teamOK {
				t.Errorf("%q from the team = %v, want %v", c.kind, got, c.teamOK)
			}
			if got := requestCommentVoiceAllowed(c.kind, requestVoiceRequester); got != c.reqOK {
				t.Errorf("%q from the requester = %v, want %v", c.kind, got, c.reqOK)
			}
		})
	}
	// A kind nobody declared is not silently attributed to the caller.
	if _, ok := requestCommentVoice(domain.RequestCommentKind("announcement")); ok {
		t.Fatal("an unknown comment kind was accepted by the voice table")
	}
	if requestCommentVoiceAllowed(domain.RequestCommentKind("announcement"), requestVoiceTeam) {
		t.Fatal("an unknown comment kind was allowed from the team's voice")
	}
}

// TestRequestUntriaged pins the window an edit is allowed in: intake's state, no
// claim, no assignee — each half on its own.
func TestRequestUntriaged(t *testing.T) {
	claimer, owner := 7, 8
	cases := []struct {
		name string
		req  domain.TeamRequest
		want bool
	}{
		{"fresh", domain.TeamRequest{State: domain.RequestStatusNew}, true},
		{"triaging", domain.TeamRequest{State: domain.RequestStatusTriaging}, false},
		{"claimed", domain.TeamRequest{State: domain.RequestStatusNew, ClaimUserID: &claimer}, false},
		{"assigned", domain.TeamRequest{State: domain.RequestStatusNew, OwnerUserID: &owner}, false},
		{"delivered", domain.TeamRequest{State: domain.RequestStatusDelivered}, false},
	}
	for _, c := range cases {
		if got := requestUntriaged(&c.req); got != c.want {
			t.Errorf("%s: untriaged = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---- criterion 1: the edit matrix ----

// TestEditRequest_AuthorizationMatrix walks who may edit what, and proves the two
// refusals the contract cares about: nobody but the submitter, and nobody at all
// once triage has started (a claim, an assignment or a state move).
func TestEditRequest_AuthorizationMatrix(t *testing.T) {
	f := newSubmitFixture(t)
	member := f.principal(f.member)
	admin := f.principal(f.admin)
	customer := f.principal(f.customer)
	stranger := f.principal(f.stranger)

	t.Run("submitter edits an untriaged request", func(t *testing.T) {
		req := f.submitAs(customer, "Export totals don't match")
		got, changed, err := f.svc.EditRequest(f.ctx, customer, f.team, req.ID, req.Rev, RequestEditInput{
			Subject:    strPtr("Export totals don't match the ledger"),
			InputBrief: strPtr("Short by about 4,300 across 11 rows."),
		})
		if err != nil {
			t.Fatalf("submitter edit: %v", err)
		}
		if !changed {
			t.Fatal("a real edit reported no change")
		}
		if got.Subject != "Export totals don't match the ledger" || !strings.Contains(got.InputBrief, "4,300") {
			t.Fatalf("stored subject/brief = %q / %q, want the edit", got.Subject, got.InputBrief)
		}
		if got.Rev != req.Rev+1 {
			t.Fatalf("rev after an edit = %d, want %d", got.Rev, req.Rev+1)
		}
		if n := f.eventCount(req.ID, domain.RequestActionEdit); n != 1 {
			t.Fatalf("%d edit history rows, want exactly 1", n)
		}
		if detail := f.eventDetails(req.ID, domain.RequestActionEdit); !strings.Contains(detail, "subject") || !strings.Contains(detail, "input_brief") {
			t.Fatalf("edit event detail = %q, want the changed field names and no content", detail)
		}
		if n := f.auditCount("request.update", req.ID); n != 1 {
			t.Fatalf("%d request.update audit rows, want exactly 1", n)
		}
	})

	t.Run("a loaded-but-not-submitted request is refused", func(t *testing.T) {
		req := f.submitAs(customer, "Filter requests by source")
		for name, p := range map[string]authz.Principal{"team member": member, "org owner": admin} {
			_, _, err := f.svc.EditRequest(f.ctx, p, f.team, req.ID, req.Rev, RequestEditInput{Subject: strPtr("rewritten")})
			requireCode(t, err, "", 403)
			if errors.Is(err, repository.ErrConflict) {
				t.Fatalf("%s: a not-yours edit reported a conflict", name)
			}
		}
		if row := f.requestRow(req.ID); row.Subject != "Filter requests by source" || row.Rev != req.Rev {
			t.Fatalf("a refused edit wrote: subject %q rev %d", row.Subject, row.Rev)
		}
		if n := f.auditCount("request.update", req.ID); n != 0 {
			t.Fatalf("%d audit rows after refused edits, want 0", n)
		}
	})

	t.Run("a team member who submitted may edit their own", func(t *testing.T) {
		req := f.submitAs(member, "Please export the queue")
		// The team's read view wins for a member (authz.RequestViewFor), but the
		// durable requester column is still theirs, and the request is untriaged.
		if _, changed, err := f.svc.EditRequest(f.ctx, member, f.team, req.ID, req.Rev, RequestEditInput{Subject: strPtr("Please export the queue as CSV")}); err != nil || !changed {
			t.Fatalf("member editing their own submission: changed=%v err=%v", changed, err)
		}
	})

	t.Run("triage closes the window", func(t *testing.T) {
		// A claim: somebody is looking at it now.
		claimed := f.submitAs(customer, "Claimed fixture")
		if _, _, err := f.svc.ClaimRequest(f.ctx, member, f.team, claimed.ID, 0); err != nil {
			t.Fatalf("claim: %v", err)
		}
		_, _, err := f.svc.EditRequest(f.ctx, customer, f.team, claimed.ID, f.requestRow(claimed.ID).Rev,
			RequestEditInput{Subject: strPtr("too late")})
		requireCode(t, err, "", 403)

		// An assignment.
		assigned := f.submitAs(customer, "Assigned fixture")
		if _, _, err := f.svc.AssignRequest(f.ctx, admin, f.team, assigned.ID, "me", 0); err != nil {
			t.Fatalf("assign: %v", err)
		}
		_, _, err = f.svc.EditRequest(f.ctx, customer, f.team, assigned.ID, f.requestRow(assigned.ID).Rev,
			RequestEditInput{Subject: strPtr("too late")})
		requireCode(t, err, "", 403)

		// A state move (#3960's act, simulated through the row).
		moved := f.submitAs(customer, "Moved fixture")
		f.sqlExec(`UPDATE team_requests SET state = 'triaging' WHERE id = $1`, moved.ID)
		_, _, err = f.svc.EditRequest(f.ctx, customer, f.team, moved.ID, f.requestRow(moved.ID).Rev,
			RequestEditInput{Subject: strPtr("too late")})
		requireCode(t, err, "", 403)

		for _, id := range []int{claimed.ID, assigned.ID, moved.ID} {
			if n := f.auditCount("request.update", id); n != 0 {
				t.Fatalf("request %d: %d request.update audit rows after refused edits, want 0", id, n)
			}
			if n := f.eventCount(id, domain.RequestActionEdit); n != 0 {
				t.Fatalf("request %d: %d edit events after refused edits, want 0", id, n)
			}
		}
	})

	t.Run("someone else's request is not found, not forbidden", func(t *testing.T) {
		mine := f.submitAs(customer, "My request")
		// A second requester, granted submit access to the same inbox (a customer
		// who is on no team and in no org).
		f.sqlExec(`INSERT INTO team_submit_grants (team_id, org_id, user_id) VALUES ($1, $2, $3)`, f.team, f.org, f.otherCust)
		theirs := f.submitAs(f.principal(f.otherCust), "Their request")
		_, _, err := f.svc.EditRequest(f.ctx, customer, f.team, theirs.ID, theirs.Rev, RequestEditInput{Subject: strPtr("hijack")})
		requireCode(t, err, authz.CodeRequestNotFound, 404)
		if n := f.auditCount("request.update", theirs.ID); n != 0 {
			t.Fatalf("%d audit rows on a foreign request, want 0", n)
		}
		// A stranger to the org sees the same bytes.
		_, _, err = f.svc.EditRequest(f.ctx, stranger, f.team, mine.ID, mine.Rev, RequestEditInput{Subject: strPtr("hijack")})
		requireCode(t, err, authz.CodeRequestNotFound, 404)
		// An unknown team and an unknown request are the same answer.
		_, _, err = f.svc.EditRequest(f.ctx, customer, f.foreign, mine.ID, mine.Rev, RequestEditInput{Subject: strPtr("hijack")})
		requireCode(t, err, authz.CodeRequestNotFound, 404)
	})

	t.Run("payloads the submitter may not write are refused, not dropped", func(t *testing.T) {
		req := f.submitAs(customer, "Payload fixture")
		rev := req.Rev
		for _, c := range []struct {
			name  string
			in    RequestEditInput
			exrev int
			want  string
		}{
			{"outcome_brief", RequestEditInput{OutcomeBrief: strPtr("Delivered: everything")}, rev, "outcome_brief"},
			{"metadata", RequestEditInput{Metadata: domain.MetadataPatch{"jira_key": "OPS-1"}}, rev, "metadata"},
			// The precondition is required here, not merely enforced by the
			// repository's rev floor: a missing one must say so, so a caller knows to
			// send the rev it read rather than reuse the repository's wording.
			{"missing expected_rev", RequestEditInput{Subject: strPtr("x")}, 0, "expected_rev"},
		} {
			_, _, err := f.svc.EditRequest(f.ctx, customer, f.team, req.ID, c.exrev, c.in)
			requireCode(t, err, authz.CodeRequestInvalid, 400)
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s refusal %q does not name %q", c.name, err, c.want)
			}
		}
		// Domain bounds are named as request.invalid, not surfaced as a fault.
		if _, _, err := f.svc.EditRequest(f.ctx, customer, f.team, req.ID, rev, RequestEditInput{Subject: strPtr("   ")}); err == nil {
			t.Fatal("a blank subject was accepted")
		} else {
			requireCode(t, err, authz.CodeRequestInvalid, 400)
		}
		if row := f.requestRow(req.ID); row.Rev != rev || row.OutcomeBrief != "" || len(row.Metadata) != 0 {
			t.Fatalf("a refused payload wrote: rev %d outcome %q metadata %v", row.Rev, row.OutcomeBrief, row.Metadata)
		}
	})
}

// TestEditRequest_StaleRevisionConflicts is criterion 2's CAS half: a stale rev is
// the surface's 409 carrying the revision the row is actually at, nothing is
// written, and the repository's ErrConflict identity survives so a caller that
// only branches on "conflict" needs no special case.
func TestEditRequest_StaleRevisionConflicts(t *testing.T) {
	f := newSubmitFixture(t)
	customer := f.principal(f.customer)
	req := f.submitAs(customer, "Concurrent editors")

	// Someone else moves the row first (a second edit, from the same submitter's
	// other device).
	first, changed, err := f.svc.EditRequest(f.ctx, customer, f.team, req.ID, req.Rev, RequestEditInput{Subject: strPtr("Concurrent editors, take two")})
	if err != nil || !changed {
		t.Fatalf("first edit: changed=%v err=%v", changed, err)
	}

	_, _, err = f.svc.EditRequest(f.ctx, customer, f.team, req.ID, req.Rev, RequestEditInput{Subject: strPtr("stale")})
	requireCode(t, err, authz.CodeRequestConflict, 409)
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("stale edit error %v does not match repository.ErrConflict", err)
	}
	var conflict *repository.RequestRevConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("stale edit error %v does not carry the current revision", err)
	}
	if conflict.Current != first.Rev || conflict.Expected != req.Rev {
		t.Fatalf("conflict says expected %d current %d, want expected %d current %d", conflict.Expected, conflict.Current, req.Rev, first.Rev)
	}
	if !strings.Contains(err.Error(), idStr(first.Rev)) {
		t.Fatalf("conflict message %q does not name the current revision %d", err, first.Rev)
	}
	if row := f.requestRow(req.ID); row.Subject != "Concurrent editors, take two" || row.Rev != first.Rev {
		t.Fatalf("the stale edit wrote: subject %q rev %d", row.Subject, row.Rev)
	}
	if n := f.auditCount("request.update", req.ID); n != 1 {
		t.Fatalf("%d request.update audit rows after one edit and one conflict, want 1", n)
	}
	if n := f.eventCount(req.ID, domain.RequestActionEdit); n != 1 {
		t.Fatalf("%d edit events after one edit and one conflict, want 1", n)
	}

	// Retrying with the revision the conflict named succeeds.
	if _, changed, err := f.svc.EditRequest(f.ctx, customer, f.team, req.ID, conflict.Current, RequestEditInput{Subject: strPtr("concurrent editors, take three")}); err != nil || !changed {
		t.Fatalf("retry with the current rev: changed=%v err=%v", changed, err)
	}
}

// TestMutationReplayWritesAndAuditsNothing is criterion 2's other half, for the
// edit (a no-op save) and the claim (a retry): nothing written, no revision moved,
// no second history row, no second audit row.
func TestMutationReplayWritesAndAuditsNothing(t *testing.T) {
	f := newSubmitFixture(t)
	member := f.principal(f.member)
	customer := f.principal(f.customer)
	req := f.submitAs(customer, "Replay fixture")

	edited, changed, err := f.svc.EditRequest(f.ctx, customer, f.team, req.ID, req.Rev, RequestEditInput{Subject: strPtr("Replay fixture, renamed")})
	if err != nil || !changed {
		t.Fatalf("first edit: changed=%v err=%v", changed, err)
	}
	// The same save, sent again: every field already has that value.
	again, changed, err := f.svc.EditRequest(f.ctx, customer, f.team, req.ID, edited.Rev, RequestEditInput{
		Subject:    strPtr("Replay fixture, renamed"),
		InputBrief: strPtr(edited.InputBrief),
	})
	if err != nil {
		t.Fatalf("replayed edit: %v", err)
	}
	if changed {
		t.Fatal("a no-op edit reported a change")
	}
	if again.Rev != edited.Rev {
		t.Fatalf("a no-op edit moved rev from %d to %d", edited.Rev, again.Rev)
	}
	if n := f.auditCount("request.update", req.ID); n != 1 {
		t.Fatalf("%d request.update audit rows after one edit and one no-op, want 1", n)
	}
	if n := f.eventCount(req.ID, domain.RequestActionEdit); n != 1 {
		t.Fatalf("%d edit events after one edit and one no-op, want 1", n)
	}

	claimed, changed, err := f.svc.ClaimRequest(f.ctx, member, f.team, req.ID, 0)
	if err != nil || !changed {
		t.Fatalf("claim: changed=%v err=%v", changed, err)
	}
	claimedAt := claimed.ClaimedAt
	replayed, changed, err := f.svc.ClaimRequest(f.ctx, member, f.team, req.ID, 0)
	if err != nil {
		t.Fatalf("replayed claim: %v", err)
	}
	if changed {
		t.Fatal("a repeated claim reported a change")
	}
	if replayed.Rev != claimed.Rev {
		t.Fatalf("a repeated claim moved rev from %d to %d", claimed.Rev, replayed.Rev)
	}
	if replayed.ClaimedAt == nil || claimedAt == nil || !replayed.ClaimedAt.Equal(*claimedAt) {
		t.Fatalf("a repeated claim moved claimed_at: %v -> %v (D4 keeps the claim's age)", claimedAt, replayed.ClaimedAt)
	}
	if n := f.auditCount("request.claim", req.ID); n != 1 {
		t.Fatalf("%d request.claim audit rows after one claim and one replay, want 1", n)
	}
	if n := f.eventCount(req.ID, domain.RequestActionClaim); n != 1 {
		t.Fatalf("%d claim events after one claim and one replay, want 1", n)
	}
}

// ---- criterion 1: claim and release ----

// TestClaimAndRelease_AuthorizationAndConflicts proves exactly one claim holder
// (I4) with no steal, no expiry, and a 409 that names the holder.
func TestClaimAndRelease_AuthorizationAndConflicts(t *testing.T) {
	f := newSubmitFixture(t)
	member := f.principal(f.member)
	admin := f.principal(f.admin)
	customer := f.principal(f.customer)
	stranger := f.principal(f.stranger)
	req := f.submitAs(customer, "Claim race")

	// A requester cannot claim: they hold no claim and take no triage.
	_, _, err := f.svc.ClaimRequest(f.ctx, customer, f.team, req.ID, 0)
	requireCode(t, err, "", 403)
	// A stranger to the org does not even see it.
	_, _, err = f.svc.ClaimRequest(f.ctx, stranger, f.team, req.ID, 0)
	requireCode(t, err, authz.CodeRequestNotFound, 404)
	if row := f.requestRow(req.ID); row.ClaimUserID != nil {
		t.Fatalf("a refused claim wrote holder %v", row.ClaimUserID)
	}

	claimed, changed, err := f.svc.ClaimRequest(f.ctx, member, f.team, req.ID, req.Rev)
	if err != nil || !changed {
		t.Fatalf("member claim: changed=%v err=%v", changed, err)
	}
	if claimed.ClaimUserID == nil || *claimed.ClaimUserID != f.member {
		t.Fatalf("claim holder = %v, want %d", claimed.ClaimUserID, f.member)
	}
	if claimed.ClaimedAt == nil {
		t.Fatal("a claim records when it was taken (claimed_at)")
	}
	if claimed.Rev != req.Rev+1 {
		t.Fatalf("rev after a claim = %d, want %d", claimed.Rev, req.Rev+1)
	}
	if detail := f.eventDetails(req.ID, domain.RequestActionClaim); !strings.Contains(detail, idStr(f.member)) {
		t.Fatalf("claim event detail = %q, want the claimant", detail)
	}
	if n := f.auditCount("request.claim", req.ID); n != 1 {
		t.Fatalf("%d request.claim audit rows, want 1", n)
	}

	// A second triager — an org owner, no less — cannot take it.
	_, _, err = f.svc.ClaimRequest(f.ctx, admin, f.team, req.ID, 0)
	requireCode(t, err, authz.CodeRequestConflict, 409)
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("lost claim %v does not match repository.ErrConflict", err)
	}
	var held *repository.RequestClaimHeld
	if !errors.As(err, &held) || held.Holder != f.member {
		t.Fatalf("lost claim error %v does not name holder %d", err, f.member)
	}
	if !strings.Contains(err.Error(), idStr(f.member)) {
		t.Fatalf("lost claim message %q does not name the holder", err)
	}
	if n := f.auditCount("request.claim", req.ID); n != 1 {
		t.Fatalf("%d request.claim audit rows after a refused steal, want 1", n)
	}

	// Releasing someone else's claim is refused the same way.
	_, _, err = f.svc.ReleaseRequestClaim(f.ctx, admin, f.team, req.ID, 0)
	requireCode(t, err, authz.CodeRequestConflict, 409)

	released, changed, err := f.svc.ReleaseRequestClaim(f.ctx, member, f.team, req.ID, 0)
	if err != nil || !changed {
		t.Fatalf("release: changed=%v err=%v", changed, err)
	}
	if released.ClaimUserID != nil || released.ClaimedAt != nil {
		t.Fatalf("release left claim %v / %v", released.ClaimUserID, released.ClaimedAt)
	}
	if detail := f.eventDetails(req.ID, domain.RequestActionRelease); !strings.Contains(detail, idStr(f.member)) {
		t.Fatalf("release event detail = %q, want the released holder", detail)
	}
	if n := f.auditCount("request.release", req.ID); n != 1 {
		t.Fatalf("%d request.release audit rows, want 1", n)
	}

	// A release with no claim in force is the no-op it is: success, nothing
	// written, nothing audited.
	if _, changed, err := f.svc.ReleaseRequestClaim(f.ctx, member, f.team, req.ID, 0); err != nil || changed {
		t.Fatalf("repeat release: changed=%v err=%v", changed, err)
	}
	if n := f.auditCount("request.release", req.ID); n != 1 {
		t.Fatalf("%d request.release audit rows after a repeated release, want 1", n)
	}
}

// TestClaimRequest_ConcurrentTriagers is criterion 1's race: two triagers claiming
// at the same instant, neither knowing about the other, end with exactly one
// holder (I4) — the loser gets the 409 naming the winner, and the request has one
// claim event and one audit row, not two.
//
// The race is real, not simulated: both callers go through the service with no
// expectation set, so the answer is decided by the repository's row lock. Whichever
// order the scheduler picks, the invariants below are the same.
func TestClaimRequest_ConcurrentTriagers(t *testing.T) {
	f := newSubmitFixture(t)
	customer := f.principal(f.customer)
	req := f.submitAs(customer, "Concurrent claim")

	// Two distinct triagers: the fixture's member, and the org owner added to the
	// team as a second member.
	second := f.addTeamMember("second", domain.TeamRoleMember)
	claimers := []authz.Principal{f.principal(f.member), f.principal(second)}

	type result struct {
		who     int
		changed bool
		err     error
		holder  int
	}
	start := make(chan struct{})
	out := make(chan result, len(claimers))
	for _, p := range claimers {
		go func(p authz.Principal) {
			<-start
			_, changed, err := f.svc.ClaimRequest(f.ctx, p, f.team, req.ID, 0)
			holder := -1
			var held *repository.RequestClaimHeld
			if errors.As(err, &held) {
				holder = held.Holder
			}
			out <- result{who: p.UserID, changed: changed, err: err, holder: holder}
		}(p)
	}
	close(start)

	var winners, losers []result
	for range claimers {
		r := <-out
		switch {
		case r.err == nil && r.changed:
			winners = append(winners, r)
		case r.err != nil:
			losers = append(losers, r)
		default:
			t.Fatalf("user %d: a lost race reported success with no change (err %v)", r.who, r.err)
		}
	}
	if len(winners) != 1 || len(losers) != 1 {
		t.Fatalf("%d winners and %d losers, want exactly one of each", len(winners), len(losers))
	}
	requireCode(t, losers[0].err, authz.CodeRequestConflict, 409)
	if losers[0].holder != winners[0].who {
		t.Fatalf("the loser was told holder %d, but %d won", losers[0].holder, winners[0].who)
	}

	row := f.requestRow(req.ID)
	if row.ClaimUserID == nil || *row.ClaimUserID != winners[0].who {
		t.Fatalf("stored holder = %v, want the winner %d", row.ClaimUserID, winners[0].who)
	}
	if n := f.eventCount(req.ID, domain.RequestActionClaim); n != 1 {
		t.Fatalf("%d claim events after a race, want exactly 1", n)
	}
	if n := f.auditCount("request.claim", req.ID); n != 1 {
		t.Fatalf("%d request.claim audit rows after a race, want exactly 1 (a lost race is not an act)", n)
	}

	// And the holder stays the holder: the loser trying again (which is what a
	// retrying client does) is still refused, so a race cannot end with both
	// callers believing they own the triage.
	if _, _, err := f.svc.ClaimRequest(f.ctx, f.principal(losers[0].who), f.team, req.ID, 0); err == nil {
		t.Fatal("the loser's retry succeeded: the race left the claim takeable")
	} else {
		requireCode(t, err, authz.CodeRequestConflict, 409)
	}
}

// ---- criterion 1: assignment ----

// TestAssignRequest_AuthorizationAndTransfer proves assignment is a manager's act,
// that a transfer clears the claim in the same write, that a non-triager cannot
// be handed a request they could not open, and that a replay writes nothing.
func TestAssignRequest_AuthorizationAndTransfer(t *testing.T) {
	f := newSubmitFixture(t)
	member := f.principal(f.member)
	admin := f.principal(f.admin)
	customer := f.principal(f.customer)
	stranger := f.principal(f.stranger)
	req := f.submitAs(customer, "Assign fixture")

	// A plain member is not a manager (contract §2 gives "assign" to the lead row).
	_, _, err := f.svc.AssignRequest(f.ctx, member, f.team, req.ID, "me", 0)
	requireCode(t, err, "", 403)
	// A requester cannot hand their own request to anyone.
	_, _, err = f.svc.AssignRequest(f.ctx, customer, f.team, req.ID, "me", 0)
	requireCode(t, err, "", 403)
	// A stranger cannot even see it.
	_, _, err = f.svc.AssignRequest(f.ctx, stranger, f.team, req.ID, "me", 0)
	requireCode(t, err, authz.CodeRequestNotFound, 404)

	// Someone claims first, so the transfer has a claim to clear.
	if _, _, err := f.svc.ClaimRequest(f.ctx, member, f.team, req.ID, 0); err != nil {
		t.Fatalf("claim before assign: %v", err)
	}
	assigned, changed, err := f.svc.AssignRequest(f.ctx, admin, f.team, req.ID, idStr(f.member), 0)
	if err != nil || !changed {
		t.Fatalf("assign: changed=%v err=%v", changed, err)
	}
	if assigned.OwnerUserID == nil || *assigned.OwnerUserID != f.member {
		t.Fatalf("owner = %v, want %d", assigned.OwnerUserID, f.member)
	}
	if assigned.ClaimUserID != nil {
		t.Fatalf("a transfer left the claim in place: %v", assigned.ClaimUserID)
	}
	detail := f.eventDetails(req.ID, domain.RequestActionAssign)
	if !strings.Contains(detail, "cleared_claim_user_id") || !strings.Contains(detail, idStr(f.member)) {
		t.Fatalf("assign event detail = %q, want the cleared claim named", detail)
	}
	if n := f.auditCount("request.assign", req.ID); n != 1 {
		t.Fatalf("%d request.assign audit rows, want 1", n)
	}

	// The same transfer again is a replay: nothing written, nothing audited.
	if _, changed, err := f.svc.AssignRequest(f.ctx, admin, f.team, req.ID, strPtrInt(f.member), 0); err != nil || changed {
		t.Fatalf("repeated assign: changed=%v err=%v", changed, err)
	}
	if n := f.auditCount("request.assign", req.ID); n != 1 {
		t.Fatalf("%d request.assign audit rows after a replay, want 1", n)
	}
	if n := f.eventCount(req.ID, domain.RequestActionAssign); n != 1 {
		t.Fatalf("%d assign events after a replay, want 1", n)
	}

	// A customer with only a submit grant could not open the request they were
	// given; the assignment is refused and writes nothing.
	_, _, err = f.svc.AssignRequest(f.ctx, admin, f.team, req.ID, idStr(f.customer), 0)
	requireCode(t, err, authz.CodeRequestInvalid, 400)
	// An unknown user is a not-found for that user (authz.ResolveUser's answer,
	// which names the user it could not resolve; the request surface's own 404
	// code is not the right one for it).
	_, _, err = f.svc.AssignRequest(f.ctx, admin, f.team, req.ID, "999999999", 0)
	requireCode(t, err, "", 404)
	// A blank assignee names nothing.
	_, _, err = f.svc.AssignRequest(f.ctx, admin, f.team, req.ID, "", 0)
	requireCode(t, err, authz.CodeRequestInvalid, 400)
	if row := f.requestRow(req.ID); row.OwnerUserID == nil || *row.OwnerUserID != f.member {
		t.Fatalf("a refused assignment changed the owner to %v", row.OwnerUserID)
	}

	// Unassigning is a transfer too, and leaves the request unowned.
	unassigned, changed, err := f.svc.AssignRequest(f.ctx, admin, f.team, req.ID, "none", 0)
	if err != nil || !changed {
		t.Fatalf("unassign: changed=%v err=%v", changed, err)
	}
	if unassigned.OwnerUserID != nil {
		t.Fatalf("unassign left owner %v", unassigned.OwnerUserID)
	}
	if n := f.auditCount("request.assign", req.ID); n != 2 {
		t.Fatalf("%d request.assign audit rows after assign + unassign, want 2", n)
	}
}

// strPtrInt renders a user id the way a surface can pass it: an id or an email.
func strPtrInt(id int) string { return idStr(id) }

// ---- criterion 1 + 3: discussion ----

// TestCommentOnRequest_AuthorizationAndVoice proves the kind/voice split, that the
// author is the authenticated principal and cannot be named by the payload, that a
// comment changes nothing but itself, and that a retried post answers with the row
// it already wrote instead of a second one.
func TestCommentOnRequest_AuthorizationAndVoice(t *testing.T) {
	f := newSubmitFixture(t)
	member := f.principal(f.member)
	customer := f.principal(f.customer)
	stranger := f.principal(f.stranger)
	req := f.submitAs(customer, "Discussion fixture")
	before := f.requestRow(req.ID)

	say := func(p authz.Principal, kind domain.RequestCommentKind, body string) (*domain.RequestComment, bool, error) {
		t.Helper()
		return f.svc.CommentOnRequest(f.ctx, p, f.team, req.ID, RequestDiscussionInput{Kind: kind, Body: body})
	}

	// A triager's plain comment: attributed to the authenticated principal.
	c, changed, err := say(member, domain.RequestCommentDiscussion, "Thanks — we are on it.\n\nSender: admin@example.test")
	if err != nil || !changed {
		t.Fatalf("team comment: changed=%v err=%v", changed, err)
	}
	if c.Author != member.Email || c.AuthorUserID == nil || *c.AuthorUserID != f.member {
		t.Fatalf("team comment attributed to %q (user %v), want %q (%d): text in the body cannot rename the author",
			c.Author, c.AuthorUserID, member.Email, f.member)
	}
	if c.Kind != domain.RequestCommentDiscussion {
		t.Fatalf("empty kind stored as %q, want the plain comment", c.Kind)
	}

	// The requester's own comment, and their answer to the desk's question.
	if _, _, err := say(customer, domain.RequestCommentDiscussion, "One more thing: the November export too."); err != nil {
		t.Fatalf("requester comment: %v", err)
	}
	q, _, err := say(member, domain.RequestCommentQuestion, "Which ledger are you comparing against?")
	if err != nil {
		t.Fatalf("team question: %v", err)
	}
	if q.Kind != domain.RequestCommentQuestion {
		t.Fatalf("question stored as %q", q.Kind)
	}
	a, _, err := say(customer, domain.RequestCommentAnswer, "The one in the October close.")
	if err != nil {
		t.Fatalf("requester answer: %v", err)
	}
	if a.Author != customer.Email || a.AuthorUserID == nil || *a.AuthorUserID != f.customer {
		t.Fatalf("answer attributed to %q (user %v), want the requester", a.Author, a.AuthorUserID)
	}

	// Wrong voice, both directions, and the system's own rows.
	for _, c := range []struct {
		name string
		p    authz.Principal
		kind domain.RequestCommentKind
	}{
		{"requester asks a question", customer, domain.RequestCommentQuestion},
		{"team member answers", member, domain.RequestCommentAnswer},
		{"team member posts system", member, domain.RequestCommentSystem},
		{"requester posts system", customer, domain.RequestCommentSystem},
	} {
		if _, _, err := say(c.p, c.kind, "should not land"); err == nil {
			t.Fatalf("%s: accepted", c.name)
		} else {
			requireCode(t, err, "", 403)
		}
	}
	// An unknown kind is request.invalid, not a 403 dressed up as a permission.
	if _, _, err := say(member, domain.RequestCommentKind("announcement"), "x"); err == nil {
		t.Fatal("an unknown comment kind was accepted")
	} else {
		requireCode(t, err, authz.CodeRequestInvalid, 400)
	}
	// An empty body says nothing.
	if _, _, err := say(member, domain.RequestCommentDiscussion, "   "); err == nil {
		t.Fatal("a blank comment was accepted")
	} else {
		requireCode(t, err, authz.CodeRequestInvalid, 400)
	}
	// A stranger is answered not-found, and writes nothing.
	if _, _, err := say(stranger, domain.RequestCommentDiscussion, "hi"); err == nil {
		t.Fatal("a stranger commented")
	} else {
		requireCode(t, err, authz.CodeRequestNotFound, 404)
	}

	if n := f.commentCount(req.ID); n != 4 {
		t.Fatalf("%d comments stored, want the 4 accepted rows", n)
	}
	if n := f.auditCount("request.comment", req.ID); n != 4 {
		t.Fatalf("%d request.comment audit rows, want one per accepted row", n)
	}

	// A comment changes nothing but itself (I8): same state, same rev, no event.
	after := f.requestRow(req.ID)
	if after.State != before.State || after.Rev != before.Rev || after.ClaimUserID != nil || after.OwnerUserID != nil {
		t.Fatalf("a comment moved the request: state %q->%q rev %d->%d claim %v owner %v",
			before.State, after.State, before.Rev, after.Rev, after.ClaimUserID, after.OwnerUserID)
	}
	var events int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1 AND action <> 'submit'`, req.ID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("%d non-submit history rows after comments, want 0: a comment is not a transition", events)
	}

	// A retried post answers with the row it already wrote, unchanged, and audits
	// nothing a second time.
	first, _, err := say(member, domain.RequestCommentDiscussion, "Retried reply")
	if err != nil {
		t.Fatalf("comment to retry: %v", err)
	}
	got, changed, err := f.svc.CommentOnRequest(f.ctx, member, f.team, req.ID, RequestDiscussionInput{
		Kind: domain.RequestCommentDiscussion, Body: "Retried reply", RetryOfCommentID: first.ID,
	})
	if err != nil {
		t.Fatalf("retried comment: %v", err)
	}
	if changed {
		t.Fatal("a retried comment reported a change")
	}
	if got.ID != first.ID || got.Body != first.Body || got.Author != first.Author {
		t.Fatalf("the retry answered with a different row: %+v vs %+v", got, first)
	}
	if n := f.commentCount(req.ID); n != 5 {
		t.Fatalf("%d comments after a retry, want 5 (the retry writes no second row)", n)
	}
	if n := f.auditCount("request.comment", req.ID); n != 5 {
		t.Fatalf("%d request.comment audit rows after a retry, want 5", n)
	}
}

// ---- criterion 3: no lifecycle or identity spoofing, end to end ----

// TestRequestMutations_CannotSpoofActorOrState runs every mutation path against a
// record whose lifecycle columns are all set, and asserts that none of them moved.
// It is the structural half of "no actor/state spoofing": the input types have no
// field for these columns, and this proves the writes do not touch them either.
func TestRequestMutations_CannotSpoofActorOrState(t *testing.T) {
	f := newSubmitFixture(t)
	admin := f.principal(f.admin)
	member := f.principal(f.member)
	customer := f.principal(f.customer)

	// A request in the middle of the lifecycle, with every lifecycle column
	// populated the way #3960's transitions will leave them.
	req := f.submitAs(customer, "Lifecycle fixture")
	f.sqlExec(`UPDATE team_requests SET state = 'accepted', outcome_brief = 'Included: the CSV.', delivered_at = NOW(),
		needs_info_since = NOW(), closed_at = NULL, rev = 5 WHERE id = $1`, req.ID)
	before := f.requestRow(req.ID)

	if _, _, err := f.svc.ClaimRequest(f.ctx, member, f.team, req.ID, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, _, err := f.svc.AssignRequest(f.ctx, admin, f.team, req.ID, idStr(f.member), 0); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, err := f.svc.CommentOnRequest(f.ctx, member, f.team, req.ID, RequestDiscussionInput{Body: "note"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if _, _, err := f.svc.ReleaseRequestClaim(f.ctx, member, f.team, req.ID, 0); err != nil {
		t.Fatalf("release: %v", err)
	}

	after := f.requestRow(req.ID)
	if after.State != before.State {
		t.Fatalf("a mutation moved the state %q -> %q", before.State, after.State)
	}
	if after.DuplicateOf != nil || after.RejectReason != "" {
		t.Fatalf("a mutation wrote duplicate_of %v / reject_reason %q", after.DuplicateOf, after.RejectReason)
	}
	if after.OutcomeBrief != before.OutcomeBrief {
		t.Fatalf("a mutation rewrote the outcome brief %q -> %q", before.OutcomeBrief, after.OutcomeBrief)
	}
	if (after.DeliveredAt == nil) != (before.DeliveredAt == nil) || (after.NeedsInfoSince == nil) != (before.NeedsInfoSince == nil) ||
		(after.AcceptedAt == nil) != (before.AcceptedAt == nil) || (after.ClosedAt == nil) != (before.ClosedAt == nil) ||
		after.ReopenedCount != before.ReopenedCount {
		t.Fatal("a mutation moved a lifecycle timestamp or the reopen count")
	}
	if after.RequesterUserID == nil || *after.RequesterUserID != f.customer || after.RequesterContact != before.RequesterContact {
		t.Fatalf("a mutation rewrote the requester (%v / %q)", after.RequesterUserID, after.RequesterContact)
	}
	if after.PublicRef != before.PublicRef || after.OrgID != before.OrgID || after.TeamID != before.TeamID {
		t.Fatal("a mutation rewrote the request's identity or tenant")
	}
}
