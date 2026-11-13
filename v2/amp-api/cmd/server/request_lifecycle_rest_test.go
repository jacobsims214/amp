package main

// Team request lifecycle and requester acceptance over REST (epic 202, story 549,
// AMP #3973; build slices F3 and F4; approved contract artifact 36 v2 §1/§2/§4/§5).
//
// internal/service/request_transitions_test.go already proves the whole graph as a
// pure decision (243 cases) and internal/service/request_lifecycle_test.go proves
// the write path (the requester-only acts refused before loading, typed evidence,
// atomicity). internal/api's mutation suite (#3971) proves the write envelope, the
// If-Match spelling and the coded refusals on this surface. So this file asks only
// what is about *these routes*:
//
//	Criterion 1 — lifecycle refusal/success table covered:
//	  TestRESTRequestLifecycle_WorkingChainAndRefusals (a table over the working
//	  chain and every evidence-carrying move: each legal act answers 200 with the
//	  new state and one history row; each illegal edge answers 400 request.invalid
//	  naming what the caller may do; each capability gap answers 403; a stranger,
//	  a sibling team and an unknown id are one 404 — and no refusal writes)
//	  TestRESTRequestLifecycle_DeliveryGate (a delivery with no brief, with no
//	  evidence, and with both no-artifact and links is refused 400 and writes
//	  nothing; the evidenced one lands)
//	  TestRESTRequestLifecycle_ConflictsCarryRev (a stale expected_rev is 409
//	  request.conflict carrying the row's actual rev, and the retry with that rev
//	  succeeds)
//	Criterion 2 — requester-only acceptance tested:
//	  TestRESTRequesterActs_AcceptIsTheRequestersAlone (the requester accepts from
//	  delivered and reopens after; a team member, a team lead, an org owner, an
//	  agent account, an API token and a submit-only credential are each refused
//	  and mutate nothing; the credential refusals are byte-identical across a
//	  real, a foreign and an unknown reference)
//	  TestRESTRequesterActs_AddressingAndAnswers (the reference the customer holds
//	  addresses answer/accept/reopen; a foreign reference and an unknown one are
//	  the same 404; an answer returns the request to the state the question
//	  interrupted and cannot be aimed at delivered)
//	Criterion 3 — no endpoint bypasses service/audit:
//	  TestRESTRequestLifecycle_NoRouteBypassesTheService (the handler file writes
//	  no repository row and no audit entry of its own, the submit-only gate
//	  refuses every lifecycle route, and a refused act writes no request.* audit
//	  row while an accepted one writes exactly one)
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit sit in front of every handler), on
// a scratch database per test. The tenant, people and requests are the reads
// fixture's world (newReadsWorld, request_reads_rest_test.go), extended here with
// the two rows these routes need: a project with one ticket that can evidence a
// delivery, and a way to put a request into a working state.
//
// Run against a disposable Postgres (never a live stack):
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequestLifecycle|TestRESTRequesterActs' -count=1 -race -v

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// ---- the routes' paths ----

// lifecyclePath names one of the team lifecycle routes.
func (w *readsWorld) lifecyclePath(teamID int, request any, act string) string {
	return fmt.Sprintf("/api/teams/%d/requests/%v/%s", teamID, request, act)
}

// requesterPath names one of the requester's own acts.
func (w *readsWorld) requesterPath(request any, act string) string {
	return fmt.Sprintf("/api/requests/%v/%s", request, act)
}

// ---- the rows these routes need ----

// lifecycleTasks is where each world's evidenced-delivery ticket id is remembered:
// the reads fixture's world type is another ticket's file, so this suite keeps its
// own state beside it rather than adding a field there.
var lifecycleTasks = map[*readsWorld]int{}

// lifecycleProject returns a ticket in a project in the world's org, so a delivery
// has something real to be evidenced by. It is created once per world, by the
// inbox's org owner (who is a project member by virtue of owning the org).
func (w *readsWorld) lifecycleProject() int {
	w.t.Helper()
	if id := lifecycleTasks[w]; id != 0 {
		return id
	}
	var projectID int
	if err := w.db.QueryRow(w.ctx,
		`INSERT INTO projects (name, code, org_id) VALUES ('Lifecycle work', $1, $2) RETURNING id`,
		"LC"+strconv.Itoa(w.org)+strconv.Itoa(w.team), w.org).Scan(&projectID); err != nil {
		w.t.Fatalf("create the project: %v", err)
	}
	var epicID, storyID, taskID int
	if err := w.db.QueryRow(w.ctx, `INSERT INTO epics (project_id, name) VALUES ($1, 'Inbox work') RETURNING id`, projectID).Scan(&epicID); err != nil {
		w.t.Fatalf("create the epic: %v", err)
	}
	if err := w.db.QueryRow(w.ctx, `INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'Slice') RETURNING id`, projectID, epicID).Scan(&storyID); err != nil {
		w.t.Fatalf("create the story: %v", err)
	}
	if err := w.db.QueryRow(w.ctx,
		`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'Deliver it') RETURNING id`,
		projectID, epicID, storyID).Scan(&taskID); err != nil {
		w.t.Fatalf("create the ticket: %v", err)
	}
	lifecycleTasks[w] = taskID
	return taskID
}

// evidenceDelivery links the world's ticket to a request as delivered_by, through
// the authorized attach path (so the row is a real relation the delivery gate can
// count, not a hand-written one).
func (w *readsWorld) evidenceDelivery(requestID int) {
	w.t.Helper()
	taskID := w.lifecycleProject()
	var orgID, teamID int
	if err := w.db.QueryRow(w.ctx, `SELECT org_id, team_id FROM team_requests WHERE id = $1`, requestID).
		Scan(&orgID, &teamID); err != nil {
		w.t.Fatalf("read request %d: %v", requestID, err)
	}
	if _, err := w.svc.AttachRequest(w.ctx, w.principal(w.admin.User.ID), service.AttachRequestInput{
		RequestID: requestID, TeamID: teamID,
		LinkKind: domain.RequestLinkDeliveredBy, TargetKind: domain.RequestTargetTask, TargetID: taskID,
	}); err != nil {
		w.t.Fatalf("evidence request %d's delivery: %v", requestID, err)
	}
}

// moveRequest puts a request into state through the repository's own transition
// write, so a case can start where it needs to be without replaying the whole
// chain over HTTP (the chain itself is one of the cases). It writes with the real
// write path, not an UPDATE: the state-entry columns and the history row a later
// act reads (an answer's destination) are the product's.
func (w *readsWorld) moveRequest(requestID int, to domain.RequestStatus) {
	w.t.Helper()
	var orgID, teamID int
	var state string
	if err := w.db.QueryRow(w.ctx, `SELECT org_id, team_id, state FROM team_requests WHERE id = $1`, requestID).
		Scan(&orgID, &teamID, &state); err != nil {
		w.t.Fatalf("read request %d: %v", requestID, err)
	}
	if domain.RequestStatus(state) == to {
		return
	}
	write := repository.RequestTransitionWrite{
		RequestID: requestID, OrgID: orgID, TeamID: teamID,
		From: domain.RequestStatus(state), To: to,
		ExpectRev: w.revOf(requestID),
		Actor:     "fixture@lifecycle.test",
	}
	// The state-entry columns 053 requires, stated the way the policy would name
	// them: a delivered request needs its brief and its time, an asking one needs a
	// question, a rejected one needs a reason, a duplicate one needs a canonical.
	switch to {
	case domain.RequestStatusDelivered:
		write.OutcomeBrief = "The CSV export is live; the scheduled variant is not included."
		write.Set = []string{"delivered_at"}
		write.NoArtifact = true
	case domain.RequestStatusRequesterAccepted:
		write.Set = []string{"accepted_at"}
	case domain.RequestStatusRejected:
		write.RejectReason = "We do not map IdP groups per office in the hosted tier."
		write.Set = []string{"closed_at"}
	case domain.RequestStatusDuplicate:
		// A fold is MarkDuplicate's write, not the transition write (it owns the
		// duplicate relation row and the chain flattening), so this case goes
		// through the same path the service uses.
		var canonical int
		if err := w.db.QueryRow(w.ctx,
			`SELECT id FROM team_requests WHERE team_id = $1 AND id <> $2 AND state <> 'duplicate' ORDER BY id LIMIT 1`,
			teamID, requestID).Scan(&canonical); err != nil {
			w.t.Fatalf("pick a canonical for %d: %v", requestID, err)
		}
		if _, _, err := w.svc.Repo.MarkDuplicate(w.ctx, repository.RequestDuplicateWrite{
			RequestID: requestID, OrgID: orgID, TeamID: teamID,
			CanonicalID: canonical, ExpectRev: w.revOf(requestID),
			Actor: "fixture@lifecycle.test",
		}); err != nil {
			w.t.Fatalf("fold request %d onto %d: %v", requestID, canonical, err)
		}
		return
	case domain.RequestStatusNeedsInformation:
		write.Set = []string{"needs_info_since"}
		write.Comment = &repository.RequestTransitionComment{
			Kind: domain.RequestCommentQuestion, Body: "Which group should be the default?",
			Author: "fixture@lifecycle.test",
		}
	}
	if _, _, err := w.svc.Repo.ApplyRequestTransition(w.ctx, write); err != nil {
		w.t.Fatalf("move request %d to %s: %v", requestID, to, err)
	}
}

// stateOf reads a request's stored state.
func (w *readsWorld) stateOf(requestID int) domain.RequestStatus {
	w.t.Helper()
	var state string
	if err := w.db.QueryRow(w.ctx, `SELECT state FROM team_requests WHERE id = $1`, requestID).Scan(&state); err != nil {
		w.t.Fatalf("state of %d: %v", requestID, err)
	}
	return domain.RequestStatus(state)
}

// transitions counts a request's transition history rows, so "nothing was
// written" is asserted against the table rather than against an answer.
func (w *readsWorld) transitions(requestID int) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx,
		`SELECT count(*) FROM request_events WHERE request_id = $1 AND action = 'transition'`, requestID).Scan(&n); err != nil {
		w.t.Fatalf("count history of %d: %v", requestID, err)
	}
	return n
}

// serviceRequestRef is a legal opaque reference for the hand-written rows this
// suite needs (a service submission with no requester user): 16–64 characters, at
// least one letter, no padding.
func serviceRequestRef() string {
	sfx := strings.TrimPrefix(uniq("svc"), "svc-")
	for len(sfx) < 20 {
		sfx += "0"
	}
	return "svc" + sfx
}

// freshRequest submits one request in the inbox as the given person, through the
// real submission service, so every case's target is a product row.
func (w *readsWorld) freshRequest(who *auth.Identity, label string) *domain.TeamRequest {
	w.t.Helper()
	res, err := w.svc.SubmitRequest(w.ctx, w.principal(who.User.ID), service.SubmitRequestInput{
		TeamID: w.team, Source: domain.RequestSourceWeb,
		Subject: "A lifecycle case " + label + " " + uniq("lc"),
	})
	if err != nil {
		w.t.Fatalf("submit %q: %v", label, err)
	}
	return res.Request
}

// ---- acting as a person in the web UI ----

// uiServer is a second listener over the same database whose only difference from
// the world's is the authentication step: the caller is the AMP user the request
// names, arriving as a web-UI session. That is what the requester's acts need — the
// reads fixture's server verifies AMP API tokens, and a token is a token channel,
// while acceptance is a person's act in the browser (I1, D5).
//
// Everything else is the production wiring: the same repository, the same
// api.RestHandler, and Register, so deniedMux, the submit-scope gate and the
// denied() audit are all in front of every route.
var uiServers = map[*readsWorld]*httptest.Server{}

func (w *readsWorld) uiServer() *httptest.Server {
	w.t.Helper()
	if srv := uiServers[w]; srv != nil {
		return srv
	}
	mux := http.NewServeMux()
	h := api.NewRestHandler(nil, w.svc.Repo, nil, nil)
	h.Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.Header.Get("X-Test-User"))
		if err != nil {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		u, err := w.svc.Repo.GetUserByID(r.Context(), id)
		if err != nil {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		ident := &auth.Identity{
			Subject: u.Subject, Email: u.Email, Name: u.DisplayName, User: u,
			Listener: "rest", Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession,
		}
		mux.ServeHTTP(rw, r.WithContext(auth.WithIdentity(r.Context(), ident)))
	}))
	w.t.Cleanup(srv.Close)
	uiServers[w] = srv
	return srv
}

// sendWithIdentity issues one request as a signed-in person.
func (w *readsWorld) sendWithIdentity(ident *auth.Identity, method, path string, body any) writeResult {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, w.uiServer().URL+path, reader)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ident != nil && ident.User != nil {
		req.Header.Set("X-Test-User", strconv.Itoa(ident.User.ID))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := writeResult{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// uiSend issues one request with ident on the context exactly as the auth
// middleware would, so the mux, deniedMux, the submit-scope gate and every
// handler behave as they do in production and only the authentication step is
// stood in for.
//
// It is needed because the reads fixture's server verifies AMP API tokens, and a
// token is a token channel: a requester's acceptance is a person's act in the web
// UI (I1, D5), so the accept/reopen cases must arrive as a signed-in person.
func (w *readsWorld) uiSend(ident *auth.Identity, method, path string, body any) writeResult {
	w.t.Helper()
	c := *ident
	c.Listener, c.Channel, c.AuthMethod = "rest", auth.ChannelUI, auth.AuthDexSession
	return w.sendWithIdentity(&c, method, path, body)
}

// uiIdentity is the *auth.Identity a signed-in person's request carries.
func (w *readsWorld) uiIdentity(userID int) *auth.Identity {
	w.t.Helper()
	u, err := w.svc.Repo.GetUserByID(w.ctx, userID)
	if err != nil {
		w.t.Fatalf("user %d: %v", userID, err)
	}
	return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: u.DisplayName, User: u}
}

// asToken is the same person reached through an AMP API token: the channel and
// ViaToken are what Principal.InUI reads, and setting them is how the middleware
// labels a token.
func (w *readsWorld) asToken(ident *auth.Identity, tokenID int64) *auth.Identity {
	c := *ident
	c.Channel, c.APIToken, c.AuthMethod, c.TokenID = auth.ChannelToken, true, auth.AuthAPIToken, tokenID
	return &c
}

// relaxSubmissionLimit raises this world's submission quota.
//
// The limiter is package-shared by default (service.defaultRequestLimits, so the
// REST and MCP listeners count one budget), and this suite submits a dozen
// requests per case — more than a window allows. Every other cmd/server suite that
// submits raises it the same way; without that, these tests would fail on the shared
// database as soon as another suite had spent the budget, which is a property of the
// fixture and not of the routes under test. The limiter's own behaviour is covered
// where it belongs (internal/service/request_limits_test.go, and the submission
// routes' 429 test).
func (w *readsWorld) relaxSubmissionLimit() {
	w.t.Helper()
	w.svc.RequestLimits = service.NewRequestLimiter(service.RequestLimitConfig{Window: time.Minute, Max: 10000})
}

// ---- criterion 1: the refusal and success table ----

// TestRESTRequestLifecycle_WorkingChainAndRefusals walks the lifecycle over HTTP:
// the plain working steps, the three evidence-carrying closes and the question,
// each in the state it is legal from and in a state it is not. It pins the three
// answers a client branches on — 200 with the new state and one history row, 400
// request.invalid naming what the caller may do instead, 403 for an edge the
// caller can see but may not take — and that every refusal wrote nothing.
func TestRESTRequestLifecycle_WorkingChainAndRefusals(t *testing.T) {
	w := newReadsWorld(t)
	w.relaxSubmissionLimit()
	admin := w.uiIdentity(w.admin.User.ID) // org owner: a manager on this inbox

	// The working chain, one request end to end: new → triaging → accepted →
	// in_progress. Each step answers the new state and appends exactly one history
	// row, which is I6 on the surface a client sees.
	chain := w.freshRequest(w.requester, "chain")
	base := w.transitions(chain.ID)
	for _, step := range []domain.RequestStatus{
		domain.RequestStatusTriaging, domain.RequestStatusAccepted, domain.RequestStatusInProgress,
	} {
		res := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, chain.ID, "transition"),
			map[string]any{"to": string(step)})
		if res.status != 200 {
			t.Fatalf("%s = %d %v, want 200", step, res.status, res.body)
		}
		_, _, rev, changed := w.answerFacts(res)
		if !changed {
			t.Fatalf("%s reported changed=false", step)
		}
		if got := w.stateOf(chain.ID); got != step {
			t.Fatalf("state = %q, want %q", got, step)
		}
		if got := w.revOf(chain.ID); got != rev {
			t.Fatalf("the answer's rev %d is not the row's %d", rev, got)
		}
		if got := w.transitions(chain.ID); got != base+1 {
			t.Fatalf("%s appended %d history rows, want exactly 1", step, got-base)
		}
		base = w.transitions(chain.ID)
	}

	// An illegal edge is 400 request.invalid, and the message names what the
	// caller may do instead (I7) — so a UI's action list and a refusal's words are
	// the same answer.
	illegal := w.freshRequest(w.requester, "illegal")
	illegalBase := w.transitions(illegal.ID)
	res := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, illegal.ID, "transition"),
		map[string]any{"to": string(domain.RequestStatusDelivered)})
	w.wantWriteCode(res, 400, authz.CodeRequestInvalid)
	if msg, _ := res.body["error"].(string); !strings.Contains(msg, string(domain.RequestStatusTriaging)) {
		t.Fatalf("an illegal edge does not name what the caller may do instead: %q", msg)
	}
	if got := w.stateOf(illegal.ID); got != domain.RequestStatusNew {
		t.Fatalf("a refused transition moved the request to %q", got)
	}
	if got := w.transitions(illegal.ID); got != illegalBase {
		t.Fatalf("a refused transition appended %d history rows", got-illegalBase)
	}

	// A generic transition naming no destination says so rather than guessing.
	noDest := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, illegal.ID, "transition"), map[string]any{})
	w.wantWriteCode(noDest, 400, authz.CodeRequestInvalid)

	// A body that names a destination on a route whose destination is fixed is
	// answered, not silently ignored: a caller that sent `to` believes it chose.
	conflicting := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, illegal.ID, "reject"),
		map[string]any{"to": "triaging", "reject_reason": "no"})
	w.wantWriteCode(conflicting, 400, authz.CodeRequestInvalid)

	// The question (I5): entering needs_information carries the question, and the
	// comment the move appended comes back with the answer — with no author.
	asking := w.freshRequest(w.requester, "ask")
	w.moveRequest(asking.ID, domain.RequestStatusTriaging)
	asked := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, asking.ID, "needs-info"),
		map[string]any{"question": "Which group should be the default for Berlin users?"})
	if asked.status != 200 {
		t.Fatalf("needs-info = %d %v, want 200", asked.status, asked.body)
	}
	row, ok := asked.body["comment"].(map[string]any)
	if !ok {
		t.Fatalf("the question answer carries no comment: %v", asked.body)
	}
	if kind, _ := row["kind"].(string); kind != string(domain.RequestCommentQuestion) {
		t.Fatalf("the appended comment kind = %v, want question", row["kind"])
	}
	for _, key := range []string{"author", "author_user_id"} {
		if _, leaked := row[key]; leaked {
			t.Fatalf("the question answer carries %q: %v", key, row)
		}
	}
	if got := w.stateOf(asking.ID); got != domain.RequestStatusNeedsInformation {
		t.Fatalf("state = %q, want needs_information", got)
	}
	// A question with no words is refused: asking obliges an answer (I5).
	emptyQuestion := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, asking.ID, "needs-info"),
		map[string]any{"question": "   "})
	w.wantWriteCode(emptyQuestion, 400, authz.CodeRequestInvalid)

	// A reject carries its reason (I3) and is a manager's act.
	rejecting := w.freshRequest(w.requester, "reject")
	w.moveRequest(rejecting.ID, domain.RequestStatusTriaging)
	noReason := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, rejecting.ID, "reject"), map[string]any{})
	w.wantWriteCode(noReason, 400, authz.CodeRequestInvalid)
	// A plain member triages and delivers but does not close: the contract's actor
	// table gives reject and duplicate to the lead/org-admin row alone, so the
	// refusal is a 403 naming the capability, not a 404 and not a silent success.
	member := w.uiIdentity(w.member.User.ID)
	memberReject := w.uiSend(member, http.MethodPost, w.lifecyclePath(w.team, rejecting.ID, "reject"),
		map[string]any{"reject_reason": "not ours to do"})
	w.wantWriteCode(memberReject, 403, "")
	if msg, _ := memberReject.body["error"].(string); !strings.Contains(msg, "manager") {
		t.Fatalf("a member's reject does not name the capability it needs: %q", msg)
	}
	if got := w.stateOf(rejecting.ID); got != domain.RequestStatusTriaging {
		t.Fatalf("a refused reject moved the request to %q", got)
	}
	rejected := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, rejecting.ID, "reject"),
		map[string]any{"reject_reason": "We do not map IdP groups per office in the hosted tier."})
	if rejected.status != 200 {
		t.Fatalf("reject = %d %v, want 200", rejected.status, rejected.body)
	}
	if got := w.stateOf(rejecting.ID); got != domain.RequestStatusRejected {
		t.Fatalf("state = %q, want rejected", got)
	}

	// The team reopens its own close (D6), and the reopen is recorded as one.
	reopened := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, rejecting.ID, "reopen"), nil)
	if reopened.status != 200 {
		t.Fatalf("reopen = %d %v, want 200", reopened.status, reopened.body)
	}
	if got := w.stateOf(rejecting.ID); got != domain.RequestStatusTriaging {
		t.Fatalf("a reopen landed in %q, want triaging", got)
	}
	var reopenedCount int
	if err := w.db.QueryRow(w.ctx, `SELECT reopened_count FROM team_requests WHERE id = $1`, rejecting.ID).Scan(&reopenedCount); err != nil {
		t.Fatal(err)
	}
	if reopenedCount != 1 {
		t.Fatalf("reopened_count = %d, want 1", reopenedCount)
	}

	// A duplicate names its canonical (D7), and a canonical that is not a request
	// in this team is refused with the policy's own words.
	folding := w.freshRequest(w.requester, "duplicate")
	w.moveRequest(folding.ID, domain.RequestStatusTriaging)
	noCanonical := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, folding.ID, "duplicate"), map[string]any{})
	w.wantWriteCode(noCanonical, 400, authz.CodeRequestInvalid)
	selfFold := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, folding.ID, "duplicate"),
		map[string]any{"canonical_request_id": folding.ID})
	w.wantWriteCode(selfFold, 400, authz.CodeRequestInvalid)
	if got := w.stateOf(folding.ID); got != domain.RequestStatusTriaging {
		t.Fatalf("a refused fold moved the request to %q", got)
	}
	folded := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, folding.ID, "duplicate"),
		map[string]any{"canonical_request_id": w.mined[0].ID})
	if folded.status != 200 {
		t.Fatalf("duplicate = %d %v, want 200", folded.status, folded.body)
	}
	if got := w.stateOf(folding.ID); got != domain.RequestStatusDuplicate {
		t.Fatalf("state = %q, want duplicate", got)
	}

	// A stranger, a sibling team's request through this team's path, and an id
	// that never existed are one answer, byte for byte.
	stranger := w.send(w.strangerTok, http.MethodPost, w.lifecyclePath(w.team, chain.ID, "transition"),
		map[string]any{"to": "triaging"}, nil)
	w.wantWriteCode(stranger, 404, authz.CodeRequestNotFound)
	sibling := w.send(w.memberTok, http.MethodPost, w.lifecyclePath(w.team, w.sibling.ID, "transition"),
		map[string]any{"to": "triaging"}, nil)
	w.wantWriteCode(sibling, 404, authz.CodeRequestNotFound)
	unknown := w.send(w.memberTok, http.MethodPost, w.lifecyclePath(w.team, "999999", "transition"),
		map[string]any{"to": "triaging"}, nil)
	w.wantWriteCode(unknown, 404, authz.CodeRequestNotFound)
	if stranger.raw != unknown.raw || sibling.raw != unknown.raw {
		t.Fatalf("404 bodies differ:\n stranger=%s sibling=%s unknown=%s", stranger.raw, sibling.raw, unknown.raw)
	}

	// The team routes address a request by its serial. A public reference is
	// refused by name rather than searched for (a second tenant-scoped lookup in an
	// adapter is what #3971 declined to add, and the requester surface is where a
	// reference belongs).
	byRef := w.send(w.memberTok, http.MethodPost, w.lifecyclePath(w.team, chain.PublicRef, "transition"),
		map[string]any{"to": "triaging"}, nil)
	w.wantWriteCode(byRef, 400, authz.CodeRequestInvalid)
}

// TestRESTRequestLifecycle_DeliveryGate pins I2 on the route: a delivery states
// that the outcome exists and is evidenced — an outcome brief plus at least one
// delivered_by link, or the explicit no-artifact record, never neither and never
// both. The gate is the aggregate's (one definition), so this test also proves the
// route reaches it rather than a second check of its own.
func TestRESTRequestLifecycle_DeliveryGate(t *testing.T) {
	w := newReadsWorld(t)
	w.relaxSubmissionLimit()
	admin := w.uiIdentity(w.admin.User.ID)
	deliver := func(requestID int, body map[string]any) writeResult {
		return w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, requestID, "deliver"), body)
	}

	target := w.freshRequest(w.requester, "deliver")
	w.moveRequest(target.ID, domain.RequestStatusInProgress)
	before := w.transitions(target.ID)

	// No brief: the outcome has to say what is and is not included.
	w.wantWriteCode(deliver(target.ID, map[string]any{"no_artifact": true}), 400, authz.CodeRequestInvalid)
	// A brief and no evidence, with the no-artifact record left off.
	unevidenced := deliver(target.ID, map[string]any{
		"outcome_brief": "Everything you asked for is included.",
	})
	w.wantWriteCode(unevidenced, 400, authz.CodeRequestInvalid)
	if got := w.stateOf(target.ID); got != domain.RequestStatusInProgress {
		t.Fatalf("an unevidenced delivery moved the request to %q", got)
	}
	if got := w.transitions(target.ID); got != before {
		t.Fatalf("an unevidenced delivery appended %d history rows", got-before)
	}

	// Evidenced by a real delivered_by link, the delivery lands.
	w.evidenceDelivery(target.ID)
	delivered := deliver(target.ID, map[string]any{
		"outcome_brief": "The CSV export is live; the scheduled variant is not included.",
	})
	if delivered.status != 200 {
		t.Fatalf("deliver = %d %v, want 200", delivered.status, delivered.body)
	}
	if got := w.stateOf(target.ID); got != domain.RequestStatusDelivered {
		t.Fatalf("state = %q, want delivered", got)
	}
	// And once it has links, claiming there is nothing to link is refused: the two
	// are alternatives, and a delivery says which one it is.
	both := w.freshRequest(w.requester, "both")
	w.moveRequest(both.ID, domain.RequestStatusInProgress)
	w.evidenceDelivery(both.ID)
	bothRes := deliver(both.ID, map[string]any{
		"outcome_brief": "Everything you asked for is included.", "no_artifact": true,
	})
	w.wantWriteCode(bothRes, 400, authz.CodeRequestInvalid)

	// The refusal names the request's own reference, which is the AGGREGATE's wording:
	// the delevery gate asks the outcome aggregate and reports what is missing in the
	// requester's terms. The lifecycle's own evidence check would also refuse this
	// payload (400 request.invalid, "delivered needs at least one delivered_by link"),
	// so asserting the wording is what proves this route goes through DeliverRequest
	// rather than straight to the transition — the same gate the rail's Deliver panel
	// disables its button on, and the one J2 will render.
	if msg, _ := unevidenced.body["error"].(string); !strings.Contains(msg, target.PublicRef) {
		t.Fatalf("the delivery refusal does not name the request, so it did not come from the aggregate's gate: %q", msg)
	}

	// A delivery naming a state, an actor or a capability is refused by the
	// decoder: the destination is the route's and the identity is the principal's.
	unknownField := w.rawSend(w.adminTokOrFail(t), http.MethodPost,
		w.lifecyclePath(w.team, target.ID, "deliver"), `{"outcome_brief":"x","state":"accepted"}`)
	w.wantWriteCode(unknownField, 400, authz.CodeRequestInvalid)

	// The requester cannot deliver their own request: stating the outcome exists is
	// the desk's act (I2). They can see it, so the refusal names the rule (403)
	// rather than pretending it is gone.
	mine := w.freshRequest(w.requester, "mine")
	w.moveRequest(mine.ID, domain.RequestStatusInProgress)
	requesterDeliver := w.uiSend(w.uiIdentity(w.requester.User.ID), http.MethodPost,
		w.lifecyclePath(w.team, mine.ID, "deliver"), map[string]any{"outcome_brief": "I did it myself.", "no_artifact": true})
	w.wantWriteCode(requesterDeliver, 403, "")
}

// TestRESTRequestLifecycle_ConflictsCarryRev: a lifecycle write is a
// compare-and-set when the caller sends the revision it read. A stale one is 409
// request.conflict carrying the row's actual rev — the fact a retry needs, so no
// re-read is required — and the retry with that rev succeeds.
func TestRESTRequestLifecycle_ConflictsCarryRev(t *testing.T) {
	w := newReadsWorld(t)
	w.relaxSubmissionLimit()
	admin := w.uiIdentity(w.admin.User.ID)

	target := w.freshRequest(w.requester, "conflict")
	path := w.lifecyclePath(w.team, target.ID, "transition")
	stale := w.revOf(target.ID)

	first := w.uiSend(admin, http.MethodPost, path, map[string]any{"to": "triaging", "expected_rev": stale})
	if first.status != 200 {
		t.Fatalf("first step = %d %v, want 200", first.status, first.body)
	}
	_, _, current, _ := w.answerFacts(first)

	lost := w.uiSend(admin, http.MethodPost, path, map[string]any{"to": "accepted", "expected_rev": stale})
	w.wantWriteCode(lost, 409, authz.CodeRequestConflict)
	if got, _ := lost.body["rev"].(float64); int(got) != current {
		t.Fatalf("the conflict reports rev %v, want the row's %d", lost.body["rev"], current)
	}
	if got := w.stateOf(target.ID); got != domain.RequestStatusTriaging {
		t.Fatalf("a stale write moved the request to %q", got)
	}
	retry := w.uiSend(admin, http.MethodPost, path, map[string]any{"to": "accepted", "expected_rev": current})
	if retry.status != 200 {
		t.Fatalf("retry with the reported rev = %d %v, want 200", retry.status, retry.body)
	}

	// The same precondition in If-Match, on the route that takes no body field for
	// it: one HTTP spelling of the compare-and-set on this surface.
	deliverTarget := w.freshRequest(w.requester, "ifmatch")
	w.moveRequest(deliverTarget.ID, domain.RequestStatusInProgress)
	w.evidenceDelivery(deliverTarget.ID)
	rev := w.revOf(deliverTarget.ID)
	delivered := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, deliverTarget.ID, "deliver"),
		map[string]any{"outcome_brief": "Delivered, as agreed.", "expected_rev": rev})
	if delivered.status != 200 {
		t.Fatalf("deliver = %d %v, want 200", delivered.status, delivered.body)
	}
	// The same revision again is stale — and the conflict reports the row's actual
	// one, so a caller that meant something else can act without re-reading.
	staleDeliver := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, deliverTarget.ID, "deliver"),
		map[string]any{"outcome_brief": "Delivered again.", "expected_rev": rev})
	w.wantWriteCode(staleDeliver, 409, authz.CodeRequestConflict)
	if got, _ := staleDeliver.body["rev"].(float64); int(got) != rev+1 {
		t.Fatalf("the conflict reports rev %v, want the delivered row's %d", staleDeliver.body["rev"], rev+1)
	}

	// `If-Match: *` is refused on these routes too, exactly as on #3971's writes:
	// "any representation" is the lost update the revision exists to prevent.
	starTarget := w.freshRequest(w.requester, "star")
	star := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, starTarget.ID, "transition"),
		map[string]any{"to": "triaging"})
	if star.status != 200 {
		t.Fatalf("step = %d %v", star.status, star.body)
	}
	req, err := http.NewRequest(http.MethodPost, w.srv.URL+w.lifecyclePath(w.team, starTarget.ID, "transition"),
		strings.NewReader(`{"to":"accepted"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.adminTokOrFail(t))
	req.Header.Set("If-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("If-Match: * = %d, want 400", resp.StatusCode)
	}
	if got := w.stateOf(starTarget.ID); got != domain.RequestStatusTriaging {
		t.Fatalf("an If-Match: * write landed (%q)", got)
	}
}

// ---- criterion 2: the requester's acts ----

// TestRESTRequesterActs_AcceptIsTheRequestersAlone is I1/D5 on the surface: the
// request's own requester accepts a delivery and reopens after it, and *nobody
// else can*. Every other shape of caller — a team member, a team lead, the org
// owner, an agent account, an API token, a submit-only credential — is refused
// and mutates nothing, and the credential refusals are byte-identical across a
// real reference, another person's and one that was never minted.
func TestRESTRequesterActs_AcceptIsTheRequestersAlone(t *testing.T) {
	w := newReadsWorld(t)
	w.relaxSubmissionLimit()

	target := w.freshRequest(w.requester, "accept")
	w.moveRequest(target.ID, domain.RequestStatusDelivered)
	path := w.requesterPath(target.PublicRef, "accept")
	beforeRev, beforeState := w.revOf(target.ID), w.stateOf(target.ID)

	// A credential — the reads fixture's AMP API tokens — is refused before the
	// reference is resolved, so the three references answer the same bytes and
	// nothing is learned about which exist.
	real := w.send(w.requesterTok, http.MethodPost, path, nil, nil)
	foreign := w.send(w.requesterTok, http.MethodPost, w.requesterPath(w.mined[2].PublicRef, "accept"), nil, nil)
	unknown := w.send(w.requesterTok, http.MethodPost, w.requesterPath("a"+uniq("ghost"), "accept"), nil, nil)
	for name, res := range map[string]writeResult{"real": real, "foreign": foreign, "unknown": unknown} {
		if res.status != 403 {
			t.Fatalf("a token's accept (%s reference) = %d %v, want 403", name, res.status, res.body)
		}
		if msg, _ := res.body["error"].(string); !strings.Contains(msg, "web UI") {
			t.Fatalf("the token refusal does not say where the act belongs: %q", msg)
		}
	}
	if real.raw != unknown.raw || foreign.raw != unknown.raw {
		t.Fatalf("a token's refusals differ by reference, so they leak existence:\n real=%s foreign=%s unknown=%s",
			real.raw, foreign.raw, unknown.raw)
	}
	// The submit-only credential is stopped one layer earlier, by the gate in front
	// of every route (token.scope_denied), which is the same refusal it gets on the
	// read routes.
	scoped := w.send(w.scopedTok, http.MethodPost, path, nil, nil)
	w.wantWriteCode(scoped, 403, authz.CodeTokenScopeDenied)

	// A person who is not the requester cannot even address the request on this
	// surface: the requester reads and acts are scoped by the caller's own
	// requester column, so another person's reference is the surface's one 404 —
	// the same rule GET /api/requests/{ref} follows. That is stricter than a 403 and
	// tells them nothing; the capability refusal that names I1/D5 is exercised
	// below, where a team member *can* address the request (their own submission)
	// and the policy is what stops them.
	agentUser, err := w.svc.Repo.UpsertUserFromClaims(w.ctx, "life-agent-"+uniq("a"),
		"life-agent-"+uniq("a")+"@lifecycle.test", "agent", nil)
	if err != nil {
		t.Fatalf("create an agent account: %v", err)
	}
	if err := w.svc.Repo.SetUserRole(w.ctx, agentUser.ID, domain.RoleMember, true); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Repo.SetUserRole(w.ctx, agentUser.ID, domain.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(w.ctx, `UPDATE users SET kind = $2 WHERE id = $1`, agentUser.ID, domain.UserKindAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(w.ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, w.team, agentUser.ID); err != nil {
		t.Fatal(err)
	}
	// The team lead: the fixture's admin is an org owner, which RequireTeamManager
	// accepts, so no extra row is needed for that shape.
	for name, ident := range map[string]*auth.Identity{
		"member": w.uiIdentity(w.member.User.ID),
		"lead":   w.uiIdentity(w.admin.User.ID),
	} {
		// Another person's reference on the requester surface is the own-request
		// 404 — the same answer GET /api/requests/{ref} gives them, so the surface
		// never confirms a request exists outside the caller's own list.
		res := w.uiSend(ident, http.MethodPost, path, nil)
		w.wantWriteCode(res, 404, authz.CodeRequestNotFound)
		if got := w.stateOf(target.ID); got != beforeState {
			t.Fatalf("%s accept moved the request to %q", name, got)
		}
		if got := w.revOf(target.ID); got != beforeRev {
			t.Fatalf("%s accept bumped the revision from %d to %d", name, beforeRev, got)
		}
	}
	// An agent account in the web UI's own channel is refused one layer earlier, by
	// the person check that runs before the reference is resolved — so it cannot even
	// learn whether the reference names a request. That is the D5 structural half.
	agentAccept := w.uiSend(w.uiIdentity(agentUser.ID), http.MethodPost, path, nil)
	if agentAccept.status != 403 {
		t.Fatalf("an agent's accept = %d %v, want 403", agentAccept.status, agentAccept.body)
	}
	if msg, _ := agentAccept.body["error"].(string); !strings.Contains(msg, "web UI") {
		t.Fatalf("the agent refusal does not say where the act belongs: %q", msg)
	}
	if got := w.stateOf(target.ID); got != beforeState {
		t.Fatalf("an agent's accept moved the request to %q", got)
	}

	// The structural case: a team member who submitted to their own inbox. They can
	// address their own request and they are an inbox member, so the *capability*
	// resolution is what stops them — RequestCapabilityFor gives the team's standing
	// precedence, and the policy refuses with a sentence naming I1/D5. This is the
	// path an on-behalf acceptance would have to travel, and it does not exist.
	ownSubmission := w.mined[3]
	w.moveRequest(ownSubmission.ID, domain.RequestStatusDelivered)
	ownAsMember := w.uiSend(w.uiIdentity(w.member.User.ID), http.MethodPost,
		w.requesterPath(ownSubmission.PublicRef, "accept"), nil)
	if ownAsMember.status != 403 {
		t.Fatalf("a team member accepting their own submission = %d %v, want 403 (a team member is not the requester)", ownAsMember.status, ownAsMember.body)
	}
	if msg, _ := ownAsMember.body["error"].(string); !strings.Contains(strings.ToLower(msg), "requester") {
		t.Fatalf("the refusal does not name the rule it broke: %q", msg)
	}
	if got := w.stateOf(ownSubmission.ID); got != domain.RequestStatusDelivered {
		t.Fatalf("a team member's accept moved their own request to %q", got)
	}

	// A token minted for the requester *themselves* is still not the requester: a
	// credential is not a person (D5), which is the case that would otherwise look
	// like it should pass.
	requesterToken := w.mintToken(w.requester)
	ownToken := w.send(requesterToken, http.MethodPost, path, nil, nil)
	if ownToken.status != 403 {
		t.Fatalf("the requester's own token accept = %d %v, want 403", ownToken.status, ownToken.body)
	}
	if got := w.stateOf(target.ID); got != beforeState {
		t.Fatalf("a token accept moved the request to %q", got)
	}

	// The requester, signed in, in the browser: the one caller that can.
	requester := w.uiIdentity(w.requester.User.ID)
	accepted := w.uiSend(requester, http.MethodPost, path, nil)
	if accepted.status != 200 {
		t.Fatalf("the requester's accept = %d %v, want 200", accepted.status, accepted.body)
	}
	if got := w.stateOf(target.ID); got != domain.RequestStatusRequesterAccepted {
		t.Fatalf("state = %q, want requester_accepted", got)
	}
	var acceptedBy *int
	if err := w.db.QueryRow(w.ctx, `SELECT accepted_by FROM team_requests WHERE id = $1`, target.ID).Scan(&acceptedBy); err != nil {
		t.Fatal(err)
	}
	if acceptedBy == nil || *acceptedBy != w.requester.User.ID {
		t.Fatalf("accepted_by = %v, want the requester %d", acceptedBy, w.requester.User.ID)
	}
	// The act is audited once, under the lifecycle's action, by the service.
	if got := w.auditRows(w.requester.User.ID, "request.transition"); got != 1 {
		t.Fatalf("request.transition audit rows = %d, want 1", got)
	}

	// The requester reopens after accepting (D6): the same reference, the same
	// surface, and the reopen is recorded.
	reopened := w.uiSend(requester, http.MethodPost, w.requesterPath(target.PublicRef, "reopen"), nil)
	if reopened.status != 200 {
		t.Fatalf("the requester's reopen = %d %v, want 200", reopened.status, reopened.body)
	}
	if got := w.stateOf(target.ID); got != domain.RequestStatusTriaging {
		t.Fatalf("a reopen landed in %q, want triaging", got)
	}

	// A request with no requester user (a service or connector submission) has
	// nobody who can accept it, and says so rather than pretending it is missing.
	serviceReq, _, err := w.svc.Repo.CreateRequest(w.ctx, repository.CreateRequestInput{
		OrgID: w.org, TeamID: w.team, PublicRef: serviceRequestRef(),
		RequesterContact: "connector@vendor.test", SourceKind: domain.RequestSourceAPI,
		Subject: "A service-submitted request", Actor: "connector",
	})
	if err != nil {
		t.Fatalf("create a service request: %v", err)
	}
	w.moveRequest(serviceReq.ID, domain.RequestStatusDelivered)
	noRequester := w.uiSend(requester, http.MethodPost, w.requesterPath(serviceReq.PublicRef, "accept"), nil)
	// The own-request load is scoped by the caller's requester column, so a request
	// with no requester is simply not theirs: the surface's one 404.
	w.wantWriteCode(noRequester, 404, authz.CodeRequestNotFound)
}

// TestRESTRequesterActs_AddressingAndAnswers: the requester surface addresses a
// request by the reference the customer actually holds, and answering a question
// returns the request to the state the question interrupted — never forward to
// delivered, and never to a destination the caller picks.
func TestRESTRequesterActs_AddressingAndAnswers(t *testing.T) {
	w := newReadsWorld(t)
	w.relaxSubmissionLimit()
	requester := w.uiIdentity(w.requester.User.ID)

	// A request in needs_information, asked from triaging: the answer must restore
	// triaging, not delivered.
	target := w.freshRequest(w.requester, "answer")
	w.moveRequest(target.ID, domain.RequestStatusTriaging)
	admin := w.uiIdentity(w.admin.User.ID)
	asked := w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, target.ID, "needs-info"),
		map[string]any{"question": "Which group should be the default for Berlin users?"})
	if asked.status != 200 {
		t.Fatalf("needs-info = %d %v", asked.status, asked.body)
	}
	commentRow, _ := asked.body["comment"].(map[string]any)
	questionID, _ := commentRow["id"].(float64)

	// Answering by reference, naming no question: the open question is the one
	// answered, and the destination is the request's prior state.
	answerPath := w.requesterPath(target.PublicRef, "answer")
	answered := w.uiSend(requester, http.MethodPost, answerPath,
		map[string]any{"answer": "de-ops should be the default, and only new sign-ins."})
	if answered.status != 200 {
		t.Fatalf("answer = %d %v, want 200", answered.status, answered.body)
	}
	if got := w.stateOf(target.ID); got != domain.RequestStatusTriaging {
		t.Fatalf("an answer landed in %q, want the state the question interrupted (triaging)", got)
	}
	row, ok := answered.body["comment"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no comment: %v", answered.body)
	}
	if kind, _ := row["kind"].(string); kind != string(domain.RequestCommentAnswer) {
		t.Fatalf("the appended comment kind = %v, want answer", kind)
	}
	for _, key := range []string{"author", "author_user_id"} {
		if _, leaked := row[key]; leaked {
			t.Fatalf("the answer carries %q: %v", key, row)
		}
	}
	// It answers the question the desk asked, by id.
	if got, _ := row["id"].(float64); int64(got) == int64(questionID) {
		t.Fatalf("the answer reused the question's comment id")
	}

	// An answer cannot be aimed at delivered: the requester does not choose where
	// their request goes next (I5). The body has no field for a destination, so the
	// attempt is refused by the decoder rather than silently ignored.
	w.moveRequest(target.ID, domain.RequestStatusTriaging)
	w.uiSend(admin, http.MethodPost, w.lifecyclePath(w.team, target.ID, "needs-info"),
		map[string]any{"question": "And should existing accounts move too?"})
	aimed := w.rawSend(w.requesterTok, http.MethodPost, w.requesterPath(target.PublicRef, "answer"),
		`{"answer":"yes","to":"delivered"}`)
	w.wantWriteCode(aimed, 400, authz.CodeRequestInvalid)
	if got := w.stateOf(target.ID); got != domain.RequestStatusNeedsInformation {
		t.Fatalf("a refused answer moved the request to %q", got)
	}

	// Another requester's reference, and one that was never minted, are one answer
	// byte for byte — so a guessed reference confirms nothing.
	foreign := w.uiSend(requester, http.MethodPost, w.requesterPath(w.mined[2].PublicRef, "answer"),
		map[string]any{"answer": "not mine"})
	w.wantWriteCode(foreign, 404, authz.CodeRequestNotFound)
	unknown := w.uiSend(requester, http.MethodPost, w.requesterPath("a"+uniq("ghost"), "answer"),
		map[string]any{"answer": "not real"})
	w.wantWriteCode(unknown, 404, authz.CodeRequestNotFound)
	if foreign.raw != unknown.raw {
		t.Fatalf("a foreign reference and an unknown one answer differently:\n foreign=%s unknown=%s", foreign.raw, unknown.raw)
	}
	// A reference the domain could never have minted is the caller's mistake, not a
	// missing request: a coded 400 naming the field, never a 404 claiming a request
	// is gone.
	junk := w.uiSend(requester, http.MethodPost, w.requesterPath("not a reference!", "answer"),
		map[string]any{"answer": "x"})
	w.wantWriteCode(junk, 400, authz.CodeRequestInvalid)

	// A team member who did not submit the request cannot address it on this
	// surface at all: the requester acts load through the own-request read, so
	// someone else's reference is the same 404 a stranger gets. Answering on a
	// requester's behalf is therefore not merely refused, it is unreachable — and
	// the capability refusal that names I1/D5 is the case the accept test pins,
	// where a member can address their own submission and the policy is what stops
	// them.
	member := w.uiIdentity(w.member.User.ID)
	memberAnswer := w.uiSend(member, http.MethodPost, w.requesterPath(target.PublicRef, "answer"),
		map[string]any{"answer": "answered for them"})
	w.wantWriteCode(memberAnswer, 404, authz.CodeRequestNotFound)
	if got := w.stateOf(target.ID); got != domain.RequestStatusNeedsInformation {
		t.Fatalf("a member's answer moved the request to %q", got)
	}

	// The serial addresses the same request on this surface, which is how a team
	// member who submitted to their own inbox reaches it. The reopen route targets
	// triaging and the policy decides whether that move is legal from where the
	// request is: r3 is the member's own submission, still `new`, and new → triaging
	// is a legal team edge — so this is the same move the working-step route makes,
	// reached by serial. Asserting the 200 (rather than guessing at a refusal) is the
	// point: the serial resolved, and the answer is the policy's.
	memberOwn := w.uiIdentity(w.member.User.ID)
	ownSerial := w.uiSend(memberOwn, http.MethodPost, w.requesterPath(w.mined[3].ID, "reopen"), nil)
	if ownSerial.status != 200 {
		t.Fatalf("reopen by serial = %d %v, want 200", ownSerial.status, ownSerial.body)
	}
	if got := w.stateOf(w.mined[3].ID); got != domain.RequestStatusTriaging {
		t.Fatalf("state = %q, want triaging", got)
	}
	// And the serial of a request that is not theirs is the same 404 as a foreign
	// reference: addressing a request you do not own never resolves.
	notTheirs := w.uiSend(memberOwn, http.MethodPost, w.requesterPath(target.ID, "answer"),
		map[string]any{"answer": "x"})
	w.wantWriteCode(notTheirs, 404, authz.CodeRequestNotFound)
}

// goCodeWithoutComments strips a Go file's comments, so a guard that asserts what
// the code does is not fooled by prose. The header of a handler file necessarily
// *names* the things it refuses to do ("nothing here calls PlanRequestTransition"),
// and a scan that counted those words would be a test of the comment rather than
// of the code. It is deliberately simple — block comments and whole-line `//`
// comments — because the files it reads are gofmt'd, so a trailing comment after
// code is the only shape it has to leave alone (and leaving it alone is the safe
// direction: a forbidden call would still be found).
func goCodeWithoutComments(src []byte) string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inBlock:
			if idx := strings.Index(trimmed, "*/"); idx >= 0 {
				inBlock = false
				trimmed = strings.TrimSpace(trimmed[idx+2:])
				if trimmed != "" && !strings.HasPrefix(trimmed, "//") {
					out = append(out, trimmed)
				}
			}
			continue
		case strings.HasPrefix(trimmed, "//"):
			continue
		case strings.HasPrefix(trimmed, "/*"):
			if idx := strings.Index(trimmed, "*/"); idx >= 0 {
				rest := strings.TrimSpace(trimmed[idx+2:])
				if rest != "" && !strings.HasPrefix(rest, "//") {
					out = append(out, rest)
				}
				continue
			}
			inBlock = true
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// ---- criterion 3: no route bypasses the service ----

// TestRESTRequestLifecycle_NoRouteBypassesTheService pins the two things a route
// could get wrong that no refusal table would show: an adapter that writes a row
// or an audit entry itself, and a route that is not behind the submit-scope gate.
func TestRESTRequestLifecycle_NoRouteBypassesTheService(t *testing.T) {
	// The source scan runs first: it needs no database, and its failure is about the
	// code rather than the world.
	src, err := os.ReadFile("../../internal/api/request_lifecycle.go")
	if err != nil {
		t.Fatalf("read request_lifecycle.go: %v", err)
	}
	code := goCodeWithoutComments(src)
	for _, forbidden := range []string{
		// The service owns every write, the audit row and the capability check.
		"Repo.", "s.audit(", "h.svc.Repo", "ApplyRequestTransition", "MarkDuplicate",
		// No second copy of the graph or the capability ladder in an adapter.
		"RequestCapability", "requestMoves", "PlanRequestTransition",
		// No approval or dispatch path, ever, from a request.
		"ApprovePlan(", "Dispatch(",
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("request_lifecycle.go mentions %q: the adapter decodes, calls one service method and encodes, and decides nothing", forbidden)
		}
	}

	w := newReadsWorld(t)
	w.relaxSubmissionLimit()
	target := w.freshRequest(w.requester, "scope")
	w.moveRequest(target.ID, domain.RequestStatusTriaging)

	// Every route this ticket adds is behind the submit-scope gate: a submit-only
	// credential is refused with token.scope_denied before any handler runs, so a
	// route added tomorrow cannot inherit write access by forgetting a check.
	for _, path := range []string{
		w.lifecyclePath(w.team, target.ID, "transition"),
		w.lifecyclePath(w.team, target.ID, "needs-info"),
		w.lifecyclePath(w.team, target.ID, "reject"),
		w.lifecyclePath(w.team, target.ID, "duplicate"),
		w.lifecyclePath(w.team, target.ID, "reopen"),
		w.lifecyclePath(w.team, target.ID, "deliver"),
		w.requesterPath(target.PublicRef, "answer"),
		w.requesterPath(target.PublicRef, "accept"),
		w.requesterPath(target.PublicRef, "reopen"),
	} {
		res := w.send(w.scopedTok, http.MethodPost, path, map[string]any{"to": "triaging"}, nil)
		if res.status != 403 {
			t.Fatalf("submit-only %s = %d %v, want 403", path, res.status, res.body)
		}
		if c, _ := res.body["code"].(string); c != authz.CodeTokenScopeDenied {
			t.Fatalf("submit-only %s code = %v, want %q", path, res.body["code"], authz.CodeTokenScopeDenied)
		}
	}

	// And a refused act writes no request.* audit row: a refusal is not an act.
	before := w.auditRows(w.admin.User.ID, "request.transition")
	refused := w.uiSend(w.uiIdentity(w.admin.User.ID), http.MethodPost,
		w.lifecyclePath(w.team, target.ID, "transition"), map[string]any{"to": "delivered"})
	w.wantWriteCode(refused, 400, authz.CodeRequestInvalid)
	if got := w.auditRows(w.admin.User.ID, "request.transition"); got != before {
		t.Fatalf("a refused act produced %d request.transition audit rows", got-before)
	}

	// Every route this file adds is registered through the deniedMux wrapper, which
	// is what puts the auth verifier, the scope gate and the access.denied audit in
	// front of it. Read from the source so a route added without it fails here.
	rest, err := os.ReadFile("../../internal/api/rest.go")
	if err != nil {
		t.Fatalf("read rest.go: %v", err)
	}
	for _, route := range []string{
		`POST /api/teams/{id}/requests/{request}/transition`,
		`POST /api/teams/{id}/requests/{request}/needs-info`,
		`POST /api/teams/{id}/requests/{request}/reject`,
		`POST /api/teams/{id}/requests/{request}/duplicate`,
		`POST /api/teams/{id}/requests/{request}/reopen`,
		`POST /api/teams/{id}/requests/{request}/deliver`,
		`POST /api/requests/{request}/answer`,
		`POST /api/requests/{request}/accept`,
		`POST /api/requests/{request}/reopen`,
	} {
		if !strings.Contains(string(rest), route) {
			t.Fatalf("route %q is not registered", route)
		}
	}
}
