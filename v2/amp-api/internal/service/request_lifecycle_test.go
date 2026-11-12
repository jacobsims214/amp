package service

// Tests for request_lifecycle.go, the write half of the request state machine
// (AMP #3960, epic 202 story 548; build slices S5/S6). The pure decision is
// already covered by request_transitions_test.go (#3953); these tests own what
// the service adds on top of it:
//
//  1. the capability it resolves is the policy's input, and the two requester
//     acts (accept, answer) are refused BEFORE the request is loaded for any
//     caller that is not a person in the web UI — an agent account, an API token,
//     a team member, a team lead — so a credential can neither accept an outcome
//     nor learn whether an id exists;
//  2. the evidence it hands the policy is typed: a delivery's count comes from the
//     request's stored delivered_by relations (a link to a task, artifact or
//     receipt), never from the caller; a canonical is READ, not asserted; the
//     answer's destination comes from the request's own history;
//  3. every legal and illegal move the policy defines behaves the same way end to
//     end: the legal ones write state + rev + exactly one history row (+ the
//     question/answer comment), the illegal ones are the policy's coded refusal
//     and write nothing at all.
//
// The database-backed tests run against a real Postgres with every migration
// applied, when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'TransitionRequest|AcceptRequest|AnswerRequest|ReopenRequest|RequestDeliveryEvidence' \
//	    -count=1 -race
//
// Every fixture row carries a run-unique suffix (org, teams, users, projects,
// tickets and the requests' public refs), so repeated runs and other tests in the
// same database never see each other's rows.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// lifecycleFixture is one test's tenant: an inbox team in an org with a project
// whose tickets a delivery can link, the people around it, and the places the
// requests live.
type lifecycleFixture struct {
	t   *testing.T
	svc *Service
	db  *pgx.Conn
	ctx context.Context

	org       int
	team      int
	otherTeam int // a sibling team in the same org
	otherOrg  int
	foreign   int // a team in otherOrg

	project   int
	epic      int
	story     int
	task      int
	task2     int
	otherTask int // a ticket in the other org

	member   int // team member
	lead     int // team lead
	admin    int // org owner
	customer int // the requester, no team role
	agent    int // an agent account
	stranger int // in otherOrg only
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	repo := submitTestRepo(t)
	ctx := context.Background()
	db, err := pgx.Connect(ctx, lifecycleDSN(t))
	if err != nil {
		t.Fatalf("connect (fixture): %v", err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	suffix := rand.IntN(1_000_000_000)
	f := &lifecycleFixture{t: t, db: db, ctx: ctx}
	q := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := db.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	user := func(label, kind string) int {
		return q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("life-%s-%d", label, suffix),
			fmt.Sprintf("life-%s-%d@life.test", label, suffix), kind)
	}

	f.org = q(`INSERT INTO orgs (name, slug) VALUES ('Lifecycle org', $1) RETURNING id`, fmt.Sprintf("life-%d", suffix))
	f.team = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, f.org)
	f.otherTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other desk') RETURNING id`, f.org)
	f.otherOrg = q(`INSERT INTO orgs (name, slug) VALUES ('Lifecycle other org', $1) RETURNING id`, fmt.Sprintf("life-other-%d", suffix))
	f.foreign = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign inbox') RETURNING id`, f.otherOrg)

	f.project = q(`INSERT INTO projects (name, code, org_id) VALUES ('Delivered work', $1, $2) RETURNING id`,
		fmt.Sprintf("LIF%d", suffix%100000), f.org)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.project, f.team)
	f.epic = q(`INSERT INTO epics (project_id, name) VALUES ($1, 'Inbox work') RETURNING id`, f.project)
	f.story = q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'Slice') RETURNING id`, f.project, f.epic)
	f.task = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'Deliver it') RETURNING id`,
		f.project, f.epic, f.story)
	f.task2 = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'Verify it') RETURNING id`,
		f.project, f.epic, f.story)
	foreignProject := q(`INSERT INTO projects (name, code, org_id) VALUES ('Elsewhere', $1, $2) RETURNING id`,
		fmt.Sprintf("LIFX%d", suffix%100000), f.otherOrg)
	foreignEpic := q(`INSERT INTO epics (project_id, name) VALUES ($1, 'Foreign epic') RETURNING id`, foreignProject)
	foreignStory := q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'Foreign story') RETURNING id`,
		foreignProject, foreignEpic)
	f.otherTask = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'Foreign task') RETURNING id`,
		foreignProject, foreignEpic, foreignStory)

	f.member = user("member", domain.UserKindHuman)
	f.lead = user("lead", domain.UserKindHuman)
	f.admin = user("admin", domain.UserKindHuman)
	f.customer = user("customer", domain.UserKindCustomer)
	f.agent = user("agent", domain.UserKindAgent)
	f.stranger = user("stranger", domain.UserKindHuman)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.org, f.admin)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, f.member)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, f.lead)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.team, f.member)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, f.team, f.lead)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.otherOrg, f.stranger)

	f.svc = New(repo, nil, nil)
	return f
}

// lifecycleDSN is the one DSN this suite reads. submitTestRepo (the submission
// tests' fixture) already owns the "read TEST_DATABASE_URL, migrate, skip without
// it" contract, so this file reuses that repo rather than starting a second
// connection policy — and the tests connect a plain pgx.Conn for their own reads
// and writes.
func lifecycleDSN(t *testing.T) string {
	t.Helper()
	repo := submitTestRepo(t) // skips when TEST_DATABASE_URL is unset
	_ = repo
	return os.Getenv("TEST_DATABASE_URL")
}

// principal builds a signed-in caller for userID: a person in the web UI unless
// the test says otherwise (agentPrincipal and tokenPrincipal are the two other
// shapes the acceptance rule turns on).
func (f *lifecycleFixture) principal(userID int) authz.Principal {
	f.t.Helper()
	var email, kind string
	if err := f.db.QueryRow(f.ctx, `SELECT email, kind FROM users WHERE id = $1`, userID).Scan(&email, &kind); err != nil {
		f.t.Fatalf("user %d: %v", userID, err)
	}
	return authz.Principal{
		UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession,
		Agent: kind == domain.UserKindAgent,
	}
}

// tokenPrincipal is the same person's account reached through an API token: the
// channel says token and ViaToken says so structurally, which is what InUI reads.
func (f *lifecycleFixture) tokenPrincipal(userID int) authz.Principal {
	p := f.principal(userID)
	p.Channel = auth.ChannelToken
	p.ViaToken = true
	p.AuthMethod = auth.AuthAPIToken
	return p
}

// agentPrincipal is an agent account in the web UI's channel: it is still not a
// person (Principal.InUI excludes agents), which is the case the acceptance rule
// has to catch even when the channel looks like a browser.
func (f *lifecycleFixture) agentPrincipal() authz.Principal {
	p := f.principal(f.agent)
	p.Channel = auth.ChannelUI
	p.Agent = true
	return p
}

// newRequest writes one request in the fixture's inbox in the given state and
// returns the stored row.
func (f *lifecycleFixture) newRequest(label string, state domain.RequestStatus, requester *int) *domain.TeamRequest {
	f.t.Helper()
	return f.newRequestIn(f.team, label, state, requester)
}

// newRequestIn writes one request in teamID.
func (f *lifecycleFixture) newRequestIn(teamID int, label string, state domain.RequestStatus, requester *int) *domain.TeamRequest {
	f.t.Helper()
	req, _, err := f.svc.Repo.CreateRequest(f.ctx, repository.CreateRequestInput{
		OrgID:            f.org,
		TeamID:           teamID,
		PublicRef:        fmt.Sprintf("%s%09d", label, rand.IntN(1_000_000_000)),
		RequesterUserID:  requester,
		RequesterContact: "requester@life.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Please add a CSV export",
		InputBrief:       "We need the queue as CSV for the weekly review.",
		Actor:            "requester@life.test",
	})
	if err != nil {
		f.t.Fatalf("create request %q: %v", label, err)
	}
	if state != domain.RequestStatusNew {
		// Set the starting state directly: a fixture is not exercising the graph,
		// and driving nine moves through the service would make each test's set-up
		// depend on the code it is testing.
		f.setState(req, state)
	}
	return f.get(req.ID, teamID)
}

// setState forces a request's state through SQL, with the columns 053 requires for
// that state, so a fixture can start where a test needs it.
func (f *lifecycleFixture) setState(req *domain.TeamRequest, state domain.RequestStatus) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, `UPDATE team_requests SET
			state = $2,
			needs_info_since = CASE WHEN $2 = 'needs_information' THEN COALESCE(needs_info_since, NOW()) ELSE needs_info_since END,
			delivered_at = CASE WHEN $2 IN ('delivered','requester_accepted') THEN COALESCE(delivered_at, NOW()) ELSE delivered_at END,
			outcome_brief = CASE WHEN $2 IN ('delivered','requester_accepted') THEN 'Delivered and written down.' ELSE outcome_brief END,
			accepted_at = CASE WHEN $2 = 'requester_accepted' THEN COALESCE(accepted_at, NOW()) ELSE accepted_at END,
			accepted_by = CASE WHEN $2 = 'requester_accepted' THEN COALESCE(accepted_by, requester_user_id) ELSE accepted_by END,
			closed_at = CASE WHEN $2 IN ('rejected','duplicate') THEN COALESCE(closed_at, NOW()) ELSE closed_at END,
			reject_reason = CASE WHEN $2 = 'rejected' THEN 'Not something this desk does.' ELSE reject_reason END,
			rev = 1
		WHERE id = $1`, req.ID, string(state)); err != nil {
		f.t.Fatalf("set request %d to %s: %v", req.ID, state, err)
	}
}

// get reloads a request through the tenant-scoped read.
func (f *lifecycleFixture) get(id, teamID int) *domain.TeamRequest {
	f.t.Helper()
	req, err := f.svc.Repo.GetRequest(f.ctx, id, f.org, teamID)
	if err != nil {
		f.t.Fatalf("GetRequest(%d): %v", id, err)
	}
	return req
}

// events and comments read the request's history and thread straight from the
// tables, so assertions see what was stored rather than what a method returned.
func (f *lifecycleFixture) events(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count events for %d: %v", requestID, err)
	}
	return n
}

func (f *lifecycleFixture) transitionEvents(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1 AND action = 'transition'`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count transition events for %d: %v", requestID, err)
	}
	return n
}

func (f *lifecycleFixture) comments(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count comments for %d: %v", requestID, err)
	}
	return n
}

// audits is every `request.transition` audit row targeting requestID.
func (f *lifecycleFixture) audits(requestID int) []domain.AuditEntry {
	f.t.Helper()
	entries, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: "request.transition", TargetID: idStr(requestID)})
	if err != nil {
		f.t.Fatalf("list audit for %d: %v", requestID, err)
	}
	return entries
}

// assertRefused proves a refused act wrote nothing: same state, same revision, no
// new history row, no new comment and no new audit row. The counts are what the
// request legitimately had before the refused act (a submit row, an earlier
// question and its audit), so "wrote nothing" is "wrote nothing MORE".
func (f *lifecycleFixture) assertRefused(req *domain.TeamRequest, wantEvents, wantComments, wantAudits int) {
	f.t.Helper()
	stored := f.get(req.ID, req.TeamID)
	if stored.State != req.State || stored.Rev != req.Rev {
		f.t.Errorf("a refused act moved the row: %s@%d, want %s@%d", stored.State, stored.Rev, req.State, req.Rev)
	}
	if got := f.events(req.ID); got != wantEvents {
		f.t.Errorf("a refused act left %d history rows, want %d", got, wantEvents)
	}
	if got := f.comments(req.ID); got != wantComments {
		f.t.Errorf("a refused act left %d comments, want %d", got, wantComments)
	}
	if got := len(f.audits(req.ID)); got != wantAudits {
		f.t.Errorf("a refused act left %d audit rows, want %d", got, wantAudits)
	}
}

// link delivers requestID with one delivered_by link to a ticket, which is what a
// delivery's evidence is (I2, D10).
func (f *lifecycleFixture) attachDelivered(req *domain.TeamRequest, taskID int) {
	f.t.Helper()
	if _, _, err := f.svc.Repo.LinkRequest(f.ctx, repository.RequestLinkWrite{
		RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
		LinkKind: domain.RequestLinkDeliveredBy, TargetKind: domain.RequestTargetTask, TargetID: taskID,
		TargetOrgID: f.org, Actor: "triager",
	}); err != nil {
		f.t.Fatalf("attach delivered_by to task %d: %v", taskID, err)
	}
}

// ---- 1. Acceptance is a person's, and only the requester's ----

// TestAcceptRequestIsAPersonsActRefusedBeforeLoading is I1/D5 as the service
// enforces it: an agent account and an API token are refused BEFORE the request is
// loaded, so their answer is the same bytes for an id that exists, an id that does
// not and another team's id — and nothing at all is written.
func TestAcceptRequestIsAPersonsActRefusedBeforeLoading(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("accept000", domain.RequestStatusDelivered, &requester)

	unknown := 987654321
	foreign := f.newRequestIn(f.otherTeam, "accept0f0", domain.RequestStatusDelivered, &requester)
	for name, p := range map[string]authz.Principal{
		"an agent account": f.agentPrincipal(),
		"an API token":     f.tokenPrincipal(requester),
	} {
		t.Run(name, func(t *testing.T) {
			var messages []string
			for _, target := range []struct{ id, team int }{
				{req.ID, f.team}, {unknown, f.team}, {foreign.ID, f.otherTeam},
			} {
				got, err := f.svc.AcceptRequest(f.ctx, p, target.id, target.team, 0)
				if err == nil {
					t.Fatalf("%s accepted %+v, want a refusal", name, got)
				}
				if authz.StatusOf(err, 500) != 403 {
					t.Errorf("%s: refusal status = %d, want 403 (%v)", name, authz.StatusOf(err, 500), err)
				}
				messages = append(messages, err.Error())
			}
			for _, m := range messages {
				if m != messages[0] {
					t.Errorf("%s: refusals differ (%q vs %q): the answer must not depend on the id", name, m, messages[0])
				}
			}
			f.assertRefused(req, 1, 0, 0) // the fixture's own submit row only
			if stored := f.get(foreign.ID, f.otherTeam); stored.State != domain.RequestStatusDelivered {
				t.Errorf("the foreign request moved to %s", stored.State)
			}
		})
	}

	// A team-side person passes the pre-load check and is refused by the policy
	// instead, in words that name their own role: they can see the request, so a 404
	// would be a lie, and nothing about the id is disclosed either way.
	for name, who := range map[string]int{"a team member": f.member, "a team lead": f.lead, "an org owner": f.admin} {
		got, err := f.svc.AcceptRequest(f.ctx, f.principal(who), req.ID, f.team, 0)
		if err == nil {
			t.Fatalf("%s accepted someone else's request (%+v)", name, got)
		}
		if authz.StatusOf(err, 500) != 403 || !strings.Contains(err.Error(), "requester's alone") {
			t.Errorf("%s: refusal = %d %v, want a 403 naming I1/D5", name, authz.StatusOf(err, 500), err)
		}
		f.assertRefused(req, 1, 0, 0)
	}

	// And the requester themselves does accept, from delivered, once.
	before := f.get(req.ID, f.team)
	res, err := f.svc.AcceptRequest(f.ctx, f.principal(requester), req.ID, f.team, before.Rev)
	if err != nil {
		t.Fatalf("the requester's acceptance: %v", err)
	}
	if res.Request.State != domain.RequestStatusRequesterAccepted {
		t.Fatalf("state = %s, want requester_accepted", res.Request.State)
	}
	if res.Request.AcceptedBy == nil || *res.Request.AcceptedBy != requester {
		t.Errorf("accepted_by = %v, want the requester %d", res.Request.AcceptedBy, requester)
	}
	if res.Request.Rev != before.Rev+1 {
		t.Errorf("rev = %d, want %d", res.Request.Rev, before.Rev+1)
	}
	if n := f.transitionEvents(req.ID); n != 1 {
		t.Errorf("transition history rows = %d, want exactly one", n)
	}
	if n := len(f.audits(req.ID)); n != 1 {
		t.Errorf("audit rows = %d, want exactly one", n)
	}
}

// TestAcceptRequestByANonRequesterIsRefused is the other half of I1: a person in
// the web UI who is not this request's requester is refused by the policy, with
// the policy's own words, and nothing moves.
func TestAcceptRequestByANonRequesterIsRefused(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("accept001", domain.RequestStatusDelivered, &requester)

	// A second customer (no team role) is nobody's requester here.
	otherCustomer := f.newCustomer("othercustomer")
	// A team member, a lead and an org owner can see it and may not accept it: 403,
	// naming what the act belongs to. A customer with no standing is nobody here, so
	// the answer is the surface's single 404 — which is also what an unknown id gets.
	for _, tc := range []struct {
		who      int
		wantCode string
	}{
		{f.member, ""},
		{f.lead, ""},
		{f.admin, ""},
		{otherCustomer, authz.CodeRequestNotFound},
	} {
		got, err := f.svc.AcceptRequest(f.ctx, f.principal(tc.who), req.ID, f.team, 0)
		if err == nil {
			t.Fatalf("user %d accepted someone else's request (%+v)", tc.who, got)
		}
		if tc.wantCode == "" && authz.StatusOf(err, 500) != 403 {
			t.Errorf("user %d: refusal status = %d, want 403 (%v)", tc.who, authz.StatusOf(err, 500), err)
		}
		if tc.wantCode != "" && authz.CodeOf(err) != tc.wantCode {
			t.Errorf("user %d: refusal code = %q, want %q (%v)", tc.who, authz.CodeOf(err), tc.wantCode, err)
		}
		f.assertRefused(req, 1, 0, 0)
	}

	// From a state that is not delivered the requester is refused too: acceptance
	// is from delivered only, never a shortcut past the team's delivery.
	inProgress := f.newRequest("accept002", domain.RequestStatusInProgress, &requester)
	if _, err := f.svc.AcceptRequest(f.ctx, f.principal(requester), inProgress.ID, f.team, 0); err == nil {
		t.Fatal("a request in progress was accepted")
	} else if authz.CodeOf(err) != authz.CodeRequestInvalid {
		t.Errorf("refusal code = %q, want %q (an edge that does not exist)", authz.CodeOf(err), authz.CodeRequestInvalid)
	}
	f.assertRefused(inProgress, 1, 0, 0)
}

// newCustomer inserts one more customer account (a person with no team role) in
// the fixture's org and returns its id.
func (f *lifecycleFixture) newCustomer(label string) int {
	f.t.Helper()
	var id int
	err := f.db.QueryRow(f.ctx, `INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("life-%s-%d-%d", label, rand.IntN(1_000_000_000), f.org),
		fmt.Sprintf("life-%s-%d-%d@life.test", label, rand.IntN(1_000_000_000), f.org),
		domain.UserKindCustomer).Scan(&id)
	if err != nil {
		f.t.Fatalf("insert customer %s: %v", label, err)
	}
	return id
}

// TestAnswerRequestIsTheRequestersAndRestoresThePriorState is I5 end to end: the
// team asks, the requester answers, the request returns to the state the question
// interrupted, and the answer's comment and the state move land together.
func TestAnswerRequestIsTheRequestersAndRestoresThePriorState(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("answer000", domain.RequestStatusAccepted, &requester)
	member := f.principal(f.member)

	asked, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team,
		To:       domain.RequestStatusNeedsInformation,
		Question: "Do you need the 14 legacy accounts moved too?",
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if asked.Comment == nil || asked.Comment.Kind != domain.RequestCommentQuestion {
		t.Fatalf("the question move stored comment %+v, want a question", asked.Comment)
	}
	if asked.Request.State != domain.RequestStatusNeedsInformation || asked.Request.NeedsInfoSince == nil {
		t.Fatalf("state = %s needs_info_since = %v", asked.Request.State, asked.Request.NeedsInfoSince)
	}

	// The team cannot answer on the requester's behalf: the policy refuses a team
	// capability on the answer path, and nothing moves.
	if _, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, Answer: "No, only the new ones.",
	}); err == nil {
		t.Fatal("a team member answered the requester's question")
	}
	f.assertRefused(asked.Request, 2, 1, 1)

	// Neither can an agent or a token, and the refusal happens before loading.
	for _, p := range []authz.Principal{f.agentPrincipal(), f.tokenPrincipal(requester)} {
		if _, err := f.svc.AnswerRequest(f.ctx, p, TransitionRequestInput{
			RequestID: req.ID, TeamID: f.team, Answer: "No, only the new ones.",
		}); err == nil {
			t.Fatalf("a non-person principal answered (%s)", p.Channel)
		}
	}

	// The requester's answer goes back to accepted, with one answer comment.
	answered, err := f.svc.AnswerRequest(f.ctx, f.principal(requester), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, Answer: "No, only the new ones.",
	})
	if err != nil {
		t.Fatalf("the requester's answer: %v", err)
	}
	if answered.Request.State != domain.RequestStatusAccepted {
		t.Errorf("state = %s, want accepted (the state the question interrupted)", answered.Request.State)
	}
	if answered.Comment == nil || answered.Comment.Kind != domain.RequestCommentAnswer {
		t.Fatalf("answer stored comment %+v, want an answer", answered.Comment)
	}
	if answered.Comment.ID <= asked.Comment.ID {
		t.Errorf("the answer comment (%d) does not follow the question (%d) in the thread", answered.Comment.ID, asked.Comment.ID)
	}
	if got := f.comments(req.ID); got != 2 {
		t.Errorf("thread rows = %d, want the question and the answer", got)
	}
	// D8: the answer does not clear the wait, so a later clock can exclude it.
	if answered.Request.NeedsInfoSince == nil {
		t.Error("the answer cleared needs_info_since")
	}
}

// TestAnswerRequestWithoutAQuestionIsRefused is I5's prerequisite: there is no
// answer to give when nothing was asked, and the refusal names that rather than
// moving the request to an invented destination.
func TestAnswerRequestWithoutAQuestionIsRefused(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	// A request hand-placed in needs_information with no question and no history:
	// the case the schema permits and the service must refuse.
	req := f.newRequest("answer001", domain.RequestStatusNeedsInformation, &requester)

	got, err := f.svc.AnswerRequest(f.ctx, f.principal(requester), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, Answer: "Here you go.",
	})
	if err == nil {
		t.Fatalf("an answer to no question succeeded (%+v)", got)
	}
	if authz.CodeOf(err) != authz.CodeRequestInvalid {
		t.Errorf("refusal code = %q, want %q: %v", authz.CodeOf(err), authz.CodeRequestInvalid, err)
	}
	f.assertRefused(req, 1, 0, 0)
}

// ---- 2. Evidence is typed, never asserted ----

// TestRequestDeliveryEvidenceMustBeAStoredVerifiedLink is I2 as the service
// enforces it: a delivery is evidenced by the request's own delivered_by
// relations, the count is read (not reported), and a row that is not one of the
// three delivery targets is refused rather than counted.
func TestRequestDeliveryEvidenceMustBeAStoredVerifiedLink(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	member := f.principal(f.member)

	// No link, no no-artifact record: refused.
	bare := f.newRequest("deliv000", domain.RequestStatusInProgress, &requester)
	if _, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: bare.ID, TeamID: f.team,
		To:           domain.RequestStatusDelivered,
		OutcomeBrief: "Shipped.",
	}); err == nil {
		t.Fatal("a delivery with no evidence succeeded")
	} else if authz.CodeOf(err) != authz.CodeRequestInvalid {
		t.Errorf("refusal code = %q, want %q: %v", authz.CodeOf(err), authz.CodeRequestInvalid, err)
	}
	f.assertRefused(bare, 1, 0, 0)

	// The explicit no-artifact record is the other half of I2.
	noArtifact, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: bare.ID, TeamID: f.team,
		To:           domain.RequestStatusDelivered,
		OutcomeBrief: "Shipped; nothing to link.",
		NoArtifact:   true,
	})
	if err != nil {
		t.Fatalf("a no-artifact delivery: %v", err)
	}
	if noArtifact.Request.State != domain.RequestStatusDelivered || noArtifact.Request.DeliveredAt == nil {
		t.Fatalf("delivered stored %s at %v", noArtifact.Request.State, noArtifact.Request.DeliveredAt)
	}

	// A stored delivered_by link is the other way.
	linked := f.newRequest("deliv001", domain.RequestStatusInProgress, &requester)
	f.attachDelivered(linked, f.task)
	delivered, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: linked.ID, TeamID: f.team,
		To:           domain.RequestStatusDelivered,
		OutcomeBrief: "Live for the three Berlin workspaces; the 14 legacy accounts are untouched.",
	})
	if err != nil {
		t.Fatalf("a delivery with a stored link: %v", err)
	}
	if delivered.Request.OutcomeBrief == "" || delivered.Request.State != domain.RequestStatusDelivered {
		t.Fatalf("delivered stored %s with brief %q", delivered.Request.State, delivered.Request.OutcomeBrief)
	}

	// A row that is not delivery evidence is refused, not counted: an injected
	// source that answers with a delivered_by relation to a PROJECT (a target D10
	// does not allow) is a bug the service catches rather than a delivery it
	// permits.
	f.svc.RequestEvidence = fakeRequestEvidence{links: []domain.RequestLink{{
		RequestID: linked.ID, LinkKind: domain.RequestLinkDeliveredBy, TargetKind: domain.RequestTargetProject, TargetID: f.project,
	}}}
	second := f.newRequest("deliv002", domain.RequestStatusInProgress, &requester)
	if _, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: second.ID, TeamID: f.team,
		To:           domain.RequestStatusDelivered,
		OutcomeBrief: "Shipped.",
	}); err == nil {
		t.Fatal("a delivered_by link to a project counted as delivery evidence")
	} else if !errors.Is(err, repository.ErrRequestTransitionShape) && authz.CodeOf(err) != authz.CodeRequestInvalid {
		t.Errorf("refusal = %v, want the shape/evidence refusal", err)
	}
	f.assertRefused(second, 1, 0, 0)

	// The caller cannot set the record either: an injected source that reports a
	// count with no relation has nothing to count, because the policy's input is
	// derived from the rows rather than from the caller.
	f.svc.RequestEvidence = fakeRequestEvidence{}
	third := f.newRequest("deliv003", domain.RequestStatusInProgress, &requester)
	if _, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: third.ID, TeamID: f.team,
		To:           domain.RequestStatusDelivered,
		OutcomeBrief: "Shipped.",
	}); err == nil {
		t.Fatal("a delivery with no relation was accepted from a source that reports nothing")
	}
}

// fakeRequestEvidence is a delivery-evidence source with exactly what a test says
// it has. It is the only injection point in this path, and it exists so the tests
// can state "these are the stored relations" (or a row that is not one) without
// writing request_links rows through LinkRequest.
type fakeRequestEvidence struct{ links []domain.RequestLink }

func (e fakeRequestEvidence) DeliveredByLinks(_ context.Context, requestID, _, _ int) ([]domain.RequestLink, error) {
	out := make([]domain.RequestLink, 0, len(e.links))
	for _, l := range e.links {
		if l.RequestID == requestID {
			out = append(out, l)
		}
	}
	return out, nil
}

// ---- 3. Legal and illegal moves end to end ----

// TestTransitionRequestLegalChainWritesOncePerAct walks the approved working chain
// on one request and asserts each step wrote exactly its own state, revision and
// one transition history row, plus one audit row
// — and that the rev the caller read is what the write applies to.
func TestTransitionRequestLegalChainWritesOncePerAct(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("chain0000", domain.RequestStatusNew, &requester)
	member := f.principal(f.member)

	chain := []domain.RequestStatus{
		domain.RequestStatusTriaging,
		domain.RequestStatusAccepted,
		domain.RequestStatusInProgress,
	}
	current := req
	for i, to := range chain {
		res, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
			RequestID: req.ID, TeamID: f.team, To: to, ExpectedRev: current.Rev,
		})
		if err != nil {
			t.Fatalf("move %d to %s: %v", i, to, err)
		}
		if res.Request.State != to {
			t.Fatalf("move %d landed in %s, want %s", i, res.Request.State, to)
		}
		if res.Request.Rev != current.Rev+1 {
			t.Fatalf("move %d rev = %d, want %d", i, res.Request.Rev, current.Rev+1)
		}
		if got := f.transitionEvents(req.ID); got != i+1 {
			t.Fatalf("after move %d there are %d transition rows, want %d", i, got, i+1)
		}
		if got := len(f.audits(req.ID)); got != i+1 {
			t.Fatalf("after move %d there are %d audit rows, want %d", i, got, i+1)
		}
		current = res.Request
	}

	// Deliver with a real link, then the requester accepts, then reopens, then the
	// team rejects with a reason, then reopens: every remaining edge with an
	// evidence requirement, each one writing once.
	f.attachDelivered(current, f.task)
	delivered, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusDelivered,
		OutcomeBrief: "Live in three workspaces.",
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	accepted, err := f.svc.AcceptRequest(f.ctx, f.principal(requester), req.ID, f.team, delivered.Request.Rev)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Plan.AcceptedBy == nil || *accepted.Plan.AcceptedBy != requester {
		t.Errorf("plan accepted_by = %v, want %d", accepted.Plan.AcceptedBy, requester)
	}
	reopened, err := f.svc.ReopenRequest(f.ctx, f.principal(requester), req.ID, f.team, accepted.Request.Rev)
	if err != nil {
		t.Fatalf("requester reopen: %v", err)
	}
	if reopened.Request.State != domain.RequestStatusTriaging || reopened.Request.ReopenedCount != 1 {
		t.Fatalf("reopen stored %s count %d", reopened.Request.State, reopened.Request.ReopenedCount)
	}
	rejected, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusRejected,
		RejectReason: "Out of scope for this desk.",
	})
	if err != nil {
		t.Fatalf("reject by the lead: %v", err)
	}
	if rejected.Request.RejectReason != "Out of scope for this desk." {
		t.Errorf("reject_reason = %q", rejected.Request.RejectReason)
	}
	if _, err := f.svc.ReopenRequest(f.ctx, f.principal(f.lead), req.ID, f.team, rejected.Request.Rev); err != nil {
		t.Fatalf("team reopen from rejected: %v", err)
	}

	// Eight acts, eight transition rows, eight audit rows: history is one row per
	// act (I6), never an amended one.
	if got := f.transitionEvents(req.ID); got != 8 {
		t.Errorf("transition rows = %d, want 8", got)
	}
	if got := len(f.audits(req.ID)); got != 8 {
		t.Errorf("audit rows = %d, want 8", got)
	}
}

// TestTransitionRequestIllegalMovesAreThePolicysRefusal is the illegal half: each
// of these is refused with the surface's coded answer and writes nothing. The
// policy's own matrix is #3953's test; what this proves is that the service
// routes every refusal through it rather than deciding anything itself.
func TestTransitionRequestIllegalMovesAreThePolicysRefusal(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	member := f.principal(f.member)
	lead := f.principal(f.lead)

	cases := []struct {
		name     string
		state    domain.RequestStatus
		p        authz.Principal
		in       TransitionRequestInput
		wantCode string
	}{
		{"new straight to accepted", domain.RequestStatusNew, member,
			TransitionRequestInput{To: domain.RequestStatusAccepted}, authz.CodeRequestInvalid},
		{"delivered straight to accepted by the team", domain.RequestStatusDelivered, member,
			TransitionRequestInput{To: domain.RequestStatusRequesterAccepted}, ""}, // 403, no code
		{"asking from new", domain.RequestStatusNew, member,
			TransitionRequestInput{To: domain.RequestStatusNeedsInformation, Question: "Which group?"}, authz.CodeRequestInvalid},
		{"entering needs_information with no question", domain.RequestStatusTriaging, member,
			TransitionRequestInput{To: domain.RequestStatusNeedsInformation}, authz.CodeRequestInvalid},
		{"a plain member rejecting", domain.RequestStatusTriaging, member,
			TransitionRequestInput{To: domain.RequestStatusRejected, RejectReason: "No."}, ""}, // 403
		{"a reject with no reason", domain.RequestStatusTriaging, lead,
			TransitionRequestInput{To: domain.RequestStatusRejected}, authz.CodeRequestInvalid},
		{"a plain member folding a duplicate", domain.RequestStatusTriaging, member,
			TransitionRequestInput{To: domain.RequestStatusDuplicate, CanonicalRequestID: 1}, ""}, // 403
		{"delivered → rejected", domain.RequestStatusDelivered, lead,
			TransitionRequestInput{To: domain.RequestStatusRejected, RejectReason: "No."}, authz.CodeRequestInvalid},
		{"a stranger", domain.RequestStatusTriaging, f.principal(f.stranger),
			TransitionRequestInput{To: domain.RequestStatusAccepted}, authz.CodeRequestNotFound},
		{"an agent account on the team's behalf", domain.RequestStatusTriaging, f.agentPrincipal(),
			TransitionRequestInput{To: domain.RequestStatusAccepted}, authz.CodeRequestNotFound},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := f.newRequest(fmt.Sprintf("illegal%03d", i), tc.state, &requester)
			tc.in.RequestID, tc.in.TeamID = req.ID, f.team
			got, err := f.svc.TransitionRequest(f.ctx, tc.p, tc.in)
			if err == nil {
				t.Fatalf("%s succeeded (%+v), want a refusal", tc.name, got)
			}
			if tc.wantCode != "" && authz.CodeOf(err) != tc.wantCode {
				t.Errorf("refusal code = %q, want %q (%v)", authz.CodeOf(err), tc.wantCode, err)
			}
			if tc.wantCode == "" && authz.StatusOf(err, 500) != 403 {
				t.Errorf("refusal status = %d, want 403 (%v)", authz.StatusOf(err, 500), err)
			}
			f.assertRefused(req, 1, 0, 0)
		})
	}
}

// TestTransitionRequestReopenFromDuplicateDetaches is D7's reopen: the team
// reopens a duplicate, which detaches it in the same write — the pointer, the live
// relation row and the state move together — while the fold stays in the history.
func TestTransitionRequestReopenFromDuplicateDetaches(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	canonical := f.newRequest("dup00000", domain.RequestStatusTriaging, &requester)
	folded := f.newRequest("dup00001", domain.RequestStatusTriaging, &requester)
	lead := f.principal(f.lead)

	res, err := f.svc.TransitionRequest(f.ctx, lead, TransitionRequestInput{
		RequestID: folded.ID, TeamID: f.team, To: domain.RequestStatusDuplicate, CanonicalRequestID: canonical.ID,
	})
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if res.Request.State != domain.RequestStatusDuplicate || res.Request.DuplicateOf == nil || *res.Request.DuplicateOf != canonical.ID {
		t.Fatalf("fold stored %s canonical %v", res.Request.State, res.Request.DuplicateOf)
	}
	if res.Plan.DuplicateOf == nil || *res.Plan.DuplicateOf != canonical.ID {
		t.Errorf("fold plan named canonical %v, want %d", res.Plan.DuplicateOf, canonical.ID)
	}

	reopened, err := f.svc.ReopenRequest(f.ctx, lead, folded.ID, f.team, res.Request.Rev)
	if err != nil {
		t.Fatalf("reopen from duplicate: %v", err)
	}
	if reopened.Request.DuplicateOf != nil {
		t.Errorf("reopened duplicate still points at %d", *reopened.Request.DuplicateOf)
	}
	if reopened.Request.ReopenedCount != 1 || reopened.Request.State != domain.RequestStatusTriaging {
		t.Errorf("reopen stored %s count %d", reopened.Request.State, reopened.Request.ReopenedCount)
	}
	var links int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, folded.ID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Errorf("%d link rows survive the detach, want none", links)
	}
}

// TestTransitionRequestCanonicalIsReadNotAsserted proves the duplicate's canonical
// is observed: a canonical in another team, one that is itself a duplicate, and
// the request itself are each refused by the policy, and a canonical the caller
// cannot see is never named in a way that leaks it.
func TestTransitionRequestCanonicalIsReadNotAsserted(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	lead := f.principal(f.lead)
	root := f.newRequest("canon000", domain.RequestStatusTriaging, &requester)
	child := f.newRequest("canon001", domain.RequestStatusTriaging, &requester)
	// child folds into root, so child is itself a duplicate.
	if _, err := f.svc.TransitionRequest(f.ctx, lead, TransitionRequestInput{
		RequestID: child.ID, TeamID: f.team, To: domain.RequestStatusDuplicate, CanonicalRequestID: root.ID,
	}); err != nil {
		t.Fatalf("fold child into root: %v", err)
	}

	cases := []struct {
		name      string
		requestID int
		teamID    int
		canonical int
	}{
		// A request in the caller's team whose canonical is another team's: the
		// canonical is not in this tenant, so it is not there to fold into.
		{"a canonical in another team", f.newRequest("canon002", domain.RequestStatusTriaging, &requester).ID, f.team,
			f.newRequestIn(f.otherTeam, "canon005", domain.RequestStatusTriaging, &requester).ID},
		{"a canonical that is itself a duplicate", f.newRequest("canon003", domain.RequestStatusTriaging, &requester).ID, f.team, child.ID},
		{"the request itself", 0, 0, 0}, // filled in below: a request folding into itself
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, canonicalID, teamID := tc.requestID, tc.canonical, tc.teamID
			if id == 0 {
				r := f.newRequest("canon004", domain.RequestStatusTriaging, &requester)
				id, canonicalID, teamID = r.ID, r.ID, f.team
			}
			got, err := f.svc.TransitionRequest(f.ctx, lead, TransitionRequestInput{
				RequestID: id, TeamID: teamID, To: domain.RequestStatusDuplicate, CanonicalRequestID: canonicalID,
			})
			if err == nil {
				t.Fatalf("%s folded (%+v), want a refusal", tc.name, got)
			}
			if authz.CodeOf(err) != authz.CodeRequestInvalid && authz.CodeOf(err) != authz.CodeRequestNotFound {
				t.Errorf("refusal = %q (%v), want request.invalid or request.not_found", authz.CodeOf(err), err)
			}
			stored := f.get(id, teamID)
			if stored.State != domain.RequestStatusTriaging || stored.Rev != 1 {
				t.Errorf("a refused fold moved the row to %s@%d", stored.State, stored.Rev)
			}
		})
	}
	_ = requester
}

// TestTransitionRequestStaleRevisionIsAConflict is the compare-and-set: a caller
// that read an old revision is told the current one and nothing moves, while a
// caller whose copy is current proceeds.
func TestTransitionRequestStaleRevisionIsAConflict(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("rev00000", domain.RequestStatusNew, &requester)
	member := f.principal(f.member)

	if _, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusTriaging, ExpectedRev: req.Rev,
	}); err != nil {
		t.Fatalf("first move: %v", err)
	}
	_, err := f.svc.TransitionRequest(f.ctx, member, TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusAccepted, ExpectedRev: req.Rev,
	})
	if err == nil {
		t.Fatal("a move at a stale revision succeeded")
	}
	if authz.CodeOf(err) != authz.CodeRequestConflict || authz.StatusOf(err, 500) != 409 {
		t.Fatalf("refusal = %d/%q (%v), want 409 request.conflict", authz.StatusOf(err, 500), authz.CodeOf(err), err)
	}
	if !errors.Is(err, repository.ErrConflict) {
		t.Error("a stale revision must keep ErrConflict in its chain so a handler maps it once")
	}
	stored := f.get(req.ID, f.team)
	if stored.State != domain.RequestStatusTriaging || stored.Rev != 2 {
		t.Errorf("after the refused move the row is %s@%d, want triaging@2", stored.State, stored.Rev)
	}
	if got := f.transitionEvents(req.ID); got != 1 {
		t.Errorf("transition rows = %d, want 1", got)
	}
}

// TestTransitionRequestUnknownAndForeignRequestsAreNotFound is the surface's one
// 404: an unknown id and another team's id are the same answer, and neither writes
// anything (there is nothing to write).
func TestTransitionRequestUnknownAndForeignRequestsAreNotFound(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	lead := f.principal(f.lead)
	foreign := f.newRequestIn(f.otherTeam, "foreign0", domain.RequestStatusTriaging, &requester)

	for _, id := range []int{987654321, foreign.ID} {
		got, err := f.svc.TransitionRequest(f.ctx, lead, TransitionRequestInput{
			RequestID: id, TeamID: f.team, To: domain.RequestStatusAccepted,
		})
		if err == nil {
			t.Fatalf("request %d was moved (%+v), want not-found", id, got)
		}
		if authz.CodeOf(err) != authz.CodeRequestNotFound {
			t.Errorf("request %d: code = %q, want %q", id, authz.CodeOf(err), authz.CodeRequestNotFound)
		}
	}
	// A team that does not exist is the same answer, not a different one.
	if _, err := f.svc.TransitionRequest(f.ctx, lead, TransitionRequestInput{
		RequestID: foreign.ID, TeamID: 987654321, To: domain.RequestStatusAccepted,
	}); authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Errorf("an unknown team answered %q, want request.not_found", authz.CodeOf(err))
	}
	if stored := f.get(foreign.ID, f.otherTeam); stored.State != domain.RequestStatusTriaging {
		t.Errorf("the foreign request moved to %s", stored.State)
	}
}

// TestTransitionRequestNeverAdvancesFromLinkedWork is I8 at the service's door:
// completing, merging or closing a ticket that is linked to a request moves
// nothing, and there is no input by which a caller can make one. The check is
// structural — the trigger this service can produce is only "explicit" — so the
// test states the fact it can observe: a linked ticket's state changes leave the
// request exactly where it was.
func TestTransitionRequestNeverAdvancesFromLinkedWork(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("linked00", domain.RequestStatusInProgress, &requester)
	f.attachDelivered(req, f.task)

	before := f.get(req.ID, f.team)
	// The linked ticket is completed and merged, which in every other AMP surface
	// is what "the work is done" looks like.
	if _, err := f.db.Exec(f.ctx, `UPDATE tasks SET state = 'completed', completed_at = NOW() WHERE id = $1`, f.task); err != nil {
		t.Fatalf("complete the linked ticket: %v", err)
	}
	after := f.get(req.ID, f.team)
	if after.State != before.State || after.Rev != before.Rev {
		t.Fatalf("a completed linked ticket moved request %d from %s@%d to %s@%d",
			req.ID, before.State, before.Rev, after.State, after.Rev)
	}
	if got := f.transitionEvents(req.ID); got != 0 {
		t.Errorf("transition rows = %d after a ticket was completed, want 0 (a submit is not a transition)", got)
	}

	// And the delivery that follows is an explicit act, with its own record: the
	// ticket's completion is not one.
	res, err := f.svc.TransitionRequest(f.ctx, f.principal(f.member), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusDelivered,
		OutcomeBrief: "Exported live.",
	})
	if err != nil {
		t.Fatalf("explicit delivery: %v", err)
	}
	if res.Plan.From != domain.RequestStatusInProgress || res.Plan.To != domain.RequestStatusDelivered {
		t.Errorf("plan %s → %s", res.Plan.From, res.Plan.To)
	}
}

// TestRequestTransitionColumnsAreWritable is the cross-package half of the column
// guard: every column the policy can name in a plan is one the repository's write
// can actually set. A plan that names a column nothing writes would be a move
// whose state-entry stamp silently never lands, which is exactly the kind of gap
// a schema CHECK cannot see (the column is nullable).
func TestRequestTransitionColumnsAreWritable(t *testing.T) {
	for _, c := range []RequestTransitionColumn{
		RequestSetNeedsInfoSince, RequestSetDeliveredAt, RequestSetAcceptedAt,
		RequestSetClosedAt, RequestSetReopenedAt, RequestSetReopenedCount,
	} {
		if !repository.RequestTransitionColumnSupported(string(c)) {
			t.Errorf("the policy can name column %q but the repository cannot set it", c)
		}
	}
	// The other direction is a repository capability the policy has no word for, so
	// it is a policy change to introduce, not a silent extra write.
	if repository.RequestTransitionColumnSupported("subject") {
		t.Error("the repository's transition write can set a column no plan may name")
	}
}

// TestRequestTransitionCapabilityFor drives the one mapping from authz rows to the
// policy's vocabulary the service relies on, through the service's own resolution
// (authz.InboxRole + RequireTeamManager), so a change to either side shows up here:
// a team member and a lead triage, only a lead/org-owner is a manager, and the
// requester is only the requester when they are a person in the web UI.
func TestRequestTransitionCapabilityFor(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("capmap00", domain.RequestStatusTriaging, &requester)

	cases := []struct {
		name string
		p    authz.Principal
		want RequestCapability
	}{
		{"the requester, in the web UI", f.principal(requester), RequestCapabilityRequester},
		{"the requester, over a token", f.tokenPrincipal(requester), RequestCapabilityNone},
		{"the requester, as an agent account", f.agentPrincipal(), RequestCapabilityNone},
		{"a team member", f.principal(f.member), RequestCapabilityTeam},
		{"a team lead", f.principal(f.lead), RequestCapabilityManager},
		{"an org owner", f.principal(f.admin), RequestCapabilityManager},
		{"a stranger", f.principal(f.stranger), RequestCapabilityNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, err := f.svc.Authz.InboxRole(f.ctx, tc.p, f.team)
			if err != nil {
				t.Fatalf("InboxRole: %v", err)
			}
			team, err := f.svc.Repo.GetTeam(f.ctx, f.team)
			if err != nil {
				t.Fatalf("GetTeam: %v", err)
			}
			manager, err := isTeamManager(f.ctx, f.svc.Authz, tc.p, team)
			if err != nil {
				t.Fatalf("isTeamManager: %v", err)
			}
			if got := RequestCapabilityFor(req, tc.p, role, manager); got != tc.want {
				t.Errorf("capability = %s, want %s", got, tc.want)
			}
		})
	}

	// A team member who submitted to their own inbox is a TEAM member, not the
	// requester: the team's standing wins (authz.RequestViewFor's order), which is
	// what stops a triager accepting their own request.
	own := f.newRequest("capmap01", domain.RequestStatusDelivered, &f.member)
	role, _ := f.svc.Authz.InboxRole(f.ctx, f.principal(f.member), f.team)
	team, _ := f.svc.Repo.GetTeam(f.ctx, f.team)
	manager, _ := isTeamManager(f.ctx, f.svc.Authz, f.principal(f.member), team)
	if got := RequestCapabilityFor(own, f.principal(f.member), role, manager); got != RequestCapabilityTeam {
		t.Errorf("a team member's own request gave capability %s, want team", got)
	}
}
