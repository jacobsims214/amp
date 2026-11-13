package main

// Team request lineage and attachments over REST (epic 202, story 549, AMP
// #3974; build slice F5; approved contract artifact 36 v2 §1/§2/§3 and decisions
// D3/D9/D11).
//
// internal/service/request_links_test.go, request_handoff_test.go,
// request_uploads_test.go, request_retention_test.go and request_downloads_test.go
// already prove the services and their repositories as boundaries.
// internal/mcp/request_planning_test.go proves the same three service methods
// through the MCP tools. So this file asks only what is about *these routes*:
//
//	Criterion 1 — link/handoff retry and approval boundaries proven:
//	  TestRESTRequestLinks_AttachIsRetrySafeAndAuthorized (one relation: 201 the
//	  first time, 200 changed=false on a replay with no second row, history entry
//	  or audit row; a second different route is 409 naming the current project and
//	  supersede_route moves it; a planned_as to a story and a delivered_by to a
//	  ticket land; a target in another org and a project the caller cannot see are
//	  the surface's one 404; a requester, a sibling team and an unknown request
//	  write nothing; attaching approves and dispatches nothing)
//	  TestRESTRequestLinks_HandoffIsRetrySafeAndApprovesNothing (a batch: 200 with
//	  two tickets, their dependency and their untrusted quotes; every created
//	  ticket is unapproved and the real dispatch gate refuses it; the same
//	  handoff_key replans nothing and audits nothing; a new key is a deliberate
//	  re-plan; an unrouted request is 400 naming the route; a decision-shaped
//	  payload is 400 naming the rule; 101 items is 400 naming the bound)
//	Criterion 2 — upload/private download matrix covered:
//	  TestRESTRequestAttachments_UploadPanelAndGate (the requester uploads their
//	  own file and reads the panel; the sniffed type is stored, not the name's; a
//	  team member uploads to the same request; the mint + serve path still returns
//	  exactly the bytes once; a stranger, a sibling team and a foreign org are one
//	  404 on every route; the sixth file and a held binary are refused or held as
//	  the domain says; a requester may delete their own file and may not keep it)
//	Criterion 3 — malformed/oversized routes write nothing:
//	  TestRESTRequestLinks_CodedRefusalsWriteNothing (a URL field on an upload, a
//	  content_type claim, an unreadable base64 payload, a payload over the
//	  per-file bound, a body over the transport's bound, a keep without its flag,
//	  a malformed attachment id, a reference instead of a serial, an unknown
//	  field, and two JSON values are each 400 request.invalid naming the mistake —
//	  and none of them writes a row or an audit entry)
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit sit in front of every handler),
// the real auth.NewAPITokenVerifier, a real actor registry (the handoff creates
// tickets through Service.CreateTasks, which needs one) and a scratch database
// per test.
//
// # Why this suite has its own world instead of borrowing the reads fixture's
//
// `newReadsWorld` submits six requests in its constructor, and it does so before
// any caller can replace the service's limiter. The submission limiter is
// **package-shared** by default (`service.defaultRequestLimits`, so the REST and
// MCP listeners count one budget) and its key is `(org, team, submitter, source)`
// — three ids and a constant. Ids restart at 1 in every scratch database, so
// every suite's first org, first team and first customer share one bucket. Six
// constructor submissions per test, times the tests in this file, is enough to
// spend a minute's budget and make *unrelated* suites answer 429 in the same run
// — a defect in the shared fixture, not in the routes under test, and one this
// ticket must not add to.
//
// So this file builds its own tenant: its own orgs, teams, users and tokens, and
// its own Service with its own limiter (`service.NewRequestLimiter` with a
// budget no test here can reach). The shapes it does share with the other suites
// — `freshDatabase`, `uniq`, `projectCode`, and the response type `writeResult` —
// are the helpers, not the world.
//
// Run against a disposable Postgres (never a live stack):
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequestLinks|TestRESTRequestAttachments' -count=1 -race -v

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// linksWorld is one tenant with an inbox team, a sibling team in the same org, a
// second org, the people a triage act's caller can be, and the three projects
// these routes need.
//
// It is deliberately self-contained (see the file header): its own limiter, its
// own users and its own requests, so it neither borrows nor spends any other
// suite's shared state.
type linksWorld struct {
	t         *testing.T
	ctx       context.Context
	repo      *repository.Repo
	db        *pgx.Conn
	svc       *service.Service
	srv       *httptest.Server
	uiSrv     *httptest.Server
	org       int
	team      int
	otherTeam int
	otherOrg  int
	foreign   int

	// The people a caller can be.
	requester *auth.Identity // a customer with a submit grant on the inbox
	other     *auth.Identity // another customer, granted on the same inbox
	member    *auth.Identity // a member of the inbox team
	admin     *auth.Identity // an owner of the inbox's org (a manager on the inbox)
	stranger  *auth.Identity // an owner of another org

	requesterTok, otherTok, memberTok, strangerTok string
	// scopedTok is a submit-only credential bound to the inbox: it may POST the
	// submission and nothing else, every route in this file included.
	scopedTok string

	// The inbox's requests, submitted through the real submission service:
	//   [0] the requester's   [1] the requester's (claimed by the member)
	//   [2] the other customer's   [3] the member's own
	mined []*domain.TeamRequest

	// project is the routed project (owned by the inbox team), project2 a second
	// one in the same org the inbox team owns, hidden a project in the same org the
	// inbox team is NOT on, foreign a project in another org.
	project, project2, hidden, foreignProject int
	epic, story                               int
	hiddenTask                                int
	// deliverable is a ticket in the routed project: the inbox team reaches it
	// through the project's owner link, so it is work the team may attach as
	// delivered_by evidence.
	deliverable int
}

func newLinksWorld(t *testing.T) *linksWorld {
	t.Helper()
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	w := &linksWorld{t: t, ctx: ctx, repo: repo, db: db}
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
	sfx := uniq("linksrest")
	// The first sign-in in a scratch database is a global admin, and a global admin
	// reaches every inbox by design (authz.InboxRole) — so a throwaway bootstrap
	// account takes that slot and every person tested with is pinned.
	if _, err := repo.UpsertUserFromClaims(ctx, sfx+"-bootstrap",
		"bootstrap-"+sfx+"@links-rest.test", "bootstrap", nil); err != nil {
		t.Fatalf("bootstrap user: %v", err)
	}
	user := func(label, kind string) *auth.Identity {
		u, err := repo.UpsertUserFromClaims(ctx, sfx+"-"+label,
			fmt.Sprintf("%s-%s@links-rest.test", label, sfx), label, nil)
		if err != nil {
			t.Fatalf("user %s: %v", label, err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleMember, true); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, false); err != nil {
			t.Fatalf("pin %s as a non-admin: %v", label, err)
		}
		if kind == domain.UserKindCustomer {
			exec(`UPDATE users SET kind = $2 WHERE id = $1`, u.ID, kind)
			u.Kind = kind
		}
		return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: label, User: u}
	}

	w.org = q(`INSERT INTO orgs (name, slug) VALUES ('Links org', $1) RETURNING id`, sfx)
	w.team = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Links desk') RETURNING id`, w.org)
	w.otherTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Links sibling desk') RETURNING id`, w.org)
	w.otherOrg = q(`INSERT INTO orgs (name, slug) VALUES ('Links other org', $1) RETURNING id`, sfx+"-o")
	w.foreign = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign desk') RETURNING id`, w.otherOrg)

	w.stranger = user("stranger", domain.UserKindHuman)
	w.member = user("member", domain.UserKindHuman)
	w.admin = user("admin", domain.UserKindHuman)
	w.requester = user("requester", domain.UserKindCustomer)
	w.other = user("othercustomer", domain.UserKindCustomer)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.org, w.member.User.ID)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, w.team, w.member.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.org, w.admin.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.otherOrg, w.other.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.otherOrg, w.stranger.User.ID)
	if _, err := repo.GrantTeamSubmit(ctx, w.team, w.org, w.requester.User.ID, nil); err != nil {
		t.Fatalf("grant the requester: %v", err)
	}
	if _, err := repo.GrantTeamSubmit(ctx, w.team, w.org, w.other.User.ID, nil); err != nil {
		t.Fatalf("grant the second requester: %v", err)
	}

	// The handler's own service behind the real REST mux and token verifier. A real
	// actor registry, because a handoff creates tickets through Service.CreateTasks,
	// which sends to the project actor.
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	reg := actor.NewRegistry(system, repo, hub.New())
	mux := http.NewServeMux()
	h := api.NewRestHandler(reg, repo, hub.New(), nil)
	w.svc = h.Service()
	// This world's own submission budget: no test in this file comes near it, and it
	// is not the package-shared limiter every other suite counts against.
	w.svc.RequestLimits = service.NewRequestLimiter(service.RequestLimitConfig{Window: time.Minute, Max: 10000})
	h.Register(mux)
	w.srv = httptest.NewServer(auth.NewAPITokenVerifier(repo).Middleware(mux))
	t.Cleanup(w.srv.Close)

	w.memberTok = w.mintToken(w.member)
	w.requesterTok = w.mintToken(w.requester)
	w.otherTok = w.mintToken(w.other)
	w.strangerTok = w.mintToken(w.stranger)
	ui := authz.Principal{UserID: w.member.User.ID, Email: w.member.Email,
		Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	scoped, err := w.svc.CreateScopedToken(ctx, ui, w.member.User.ID, "links-scoped", 0,
		service.TokenScopeRequest{Scope: "submit", TeamID: w.team})
	if err != nil {
		t.Fatalf("mint submit-only token: %v", err)
	}
	w.scopedTok = scoped.Token

	// The four inbox requests, through the real submission path.
	submit := func(who *auth.Identity, teamID int, subject string) *domain.TeamRequest {
		t.Helper()
		res, err := w.svc.SubmitRequest(ctx, w.principalOf(who.User.ID), service.SubmitRequestInput{
			TeamID: teamID, Source: domain.RequestSourceWeb,
			Subject: subject, InputBrief: "The details are in the request body.",
		})
		if err != nil {
			t.Fatalf("submit %q: %v", subject, err)
		}
		return res.Request
	}
	w.mined = []*domain.TeamRequest{
		submit(w.requester, w.team, "Export the queue as CSV"),
		submit(w.requester, w.team, "The dashboard total looks wrong"),
		submit(w.other, w.team, "Please add a dark theme"),
		submit(w.member, w.team, "A team member's own request"),
	}
	// r1 is claimed by the member: a live claim, which is ordinary triage state and
	// the row the handoff cases plan from.
	exec(`UPDATE team_requests SET claim_user_id = $2, claimed_at = NOW(), state = 'triaging' WHERE id = $1`,
		w.mined[1].ID, w.member.User.ID)

	// The routed project: created with this org and the inbox team as its owner, so
	// the team's member reaches it through the team link and nobody else does.
	proj, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: "Delivered work", Code: projectCode("LW", sfx), OrgID: &w.org, TeamID: &w.team,
	})
	if err != nil {
		t.Fatalf("create the routed project: %v", err)
	}
	w.project = proj.ID
	epic, err := repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: w.project, Name: "the-plan"})
	if err != nil {
		t.Fatalf("create the routed epic: %v", err)
	}
	w.epic = epic.ID
	story, err := repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: w.project, EpicID: w.epic, Name: "the-story"})
	if err != nil {
		t.Fatalf("create the routed story: %v", err)
	}
	w.story = story.ID
	w.deliverable = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'deliver it') RETURNING id`,
		w.project, w.epic, w.story)

	proj2, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: "Second delivered work", Code: projectCode("LX", sfx), OrgID: &w.org, TeamID: &w.team,
	})
	if err != nil {
		t.Fatalf("create the second project: %v", err)
	}
	w.project2 = proj2.ID

	// A project in the same org the inbox team is not on. Being in the org is not
	// enough to attach into it (E1's tenant/access pair), so the member must be
	// refused exactly as for a project that is not there.
	w.hidden = q(`INSERT INTO projects (name, code, org_id) VALUES ('Hidden work', $1, $2) RETURNING id`,
		projectCode("PH", sfx), w.org)
	hiddenTeam := q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Hidden team') RETURNING id`, w.org)
	outsider := q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		sfx+"-outsider", sfx+"-outsider@links-rest.test", domain.UserKindHuman)
	q(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member') RETURNING user_id`, hiddenTeam, outsider)
	q(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'owner') RETURNING project_id`, w.hidden, hiddenTeam)
	hiddenEpic := q(`INSERT INTO epics (project_id, name) VALUES ($1, 'hidden-epic') RETURNING id`, w.hidden)
	hiddenStory := q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'hidden-story') RETURNING id`, w.hidden, hiddenEpic)
	w.hiddenTask = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'hidden-task') RETURNING id`,
		w.hidden, hiddenEpic, hiddenStory)

	// A project in another org entirely, with a ticket in it.
	foreignProj := q(`INSERT INTO projects (name, code, org_id) VALUES ('Foreign work', $1, $2) RETURNING id`,
		projectCode("LF", sfx), w.otherOrg)
	fEpic := q(`INSERT INTO epics (project_id, name) VALUES ($1, 'foreign-epic') RETURNING id`, foreignProj)
	fStory := q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'foreign-story') RETURNING id`, foreignProj, fEpic)
	q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'foreign-task') RETURNING id`,
		foreignProj, fEpic, fStory)
	w.foreignProject = foreignProj
	return w
}

// principalOf builds the principal a signed-in person's request carries, the same
// way the auth middleware does.
func (w *linksWorld) principalOf(userID int) authz.Principal {
	w.t.Helper()
	var email string
	if err := w.db.QueryRow(w.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		w.t.Fatalf("email of %d: %v", userID, err)
	}
	return authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
}

// mintToken mints a full-scope API token for one person.
func (w *linksWorld) mintToken(ident *auth.Identity) string {
	w.t.Helper()
	ui := authz.Principal{UserID: ident.User.ID, Email: ident.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	tok, err := w.svc.CreateToken(w.ctx, ui, ident.User.ID, "links-"+uniq("t"), 0)
	if err != nil {
		w.t.Fatalf("mint a token: %v", err)
	}
	return tok.Token
}

// ---- issuing requests against this suite's server ----

// send issues one request as token, with an optional JSON body and headers.
func (w *linksWorld) send(token, method, path string, body any, headers map[string]string) writeResult {
	w.t.Helper()
	return w.sendTo(w.srv, token, method, path, body, headers)
}

// sendTo is send against an explicit server, so the same helper serves the
// token-authenticated listener and any other.
func (w *linksWorld) sendTo(srv *httptest.Server, token, method, path string, body any, headers map[string]string) writeResult {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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

// sendRaw issues a request with a literal body, for the decoder's refusals that a
// map cannot express.
func (w *linksWorld) sendRaw(token, method, path, body string) writeResult {
	w.t.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
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

// get issues a GET and returns the headers as well as the body.
func (w *linksWorld) get(token, path string) (int, http.Header, []byte) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.srv.URL+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

// wantWriteCode asserts a refusal's status and, when the answer carries one, its
// stable code. An empty code means "this refusal has no request.* code" — the
// target-not-found arm of the attach path is the one that does (#3962's
// attachRequestError).
func (w *linksWorld) wantWriteCode(r writeResult, status int, code string) {
	w.t.Helper()
	if r.status != status {
		w.t.Fatalf("%d %v, want %d %s", r.status, r.body, status, code)
	}
	if code == "" {
		return
	}
	if got, _ := r.body["code"].(string); got != code {
		w.t.Fatalf("code = %v, want %q (body %v)", r.body["code"], code, r.body)
	}
}

// ---- the routes' paths, and the rows they need ----

// linkPath names the attach route for one request.
func (w *linksWorld) linkPath(teamID, requestID int) string {
	return fmt.Sprintf("/api/teams/%d/requests/%d/links", teamID, requestID)
}

// handoffPath names the handoff route for one request.
func (w *linksWorld) handoffPath(teamID, requestID int) string {
	return fmt.Sprintf("/api/teams/%d/requests/%d/handoff", teamID, requestID)
}

// attachmentsPath names the file panel and the upload route.
func (w *linksWorld) attachmentsPath(teamID, requestID int) string {
	return fmt.Sprintf("/api/teams/%d/requests/%d/attachments", teamID, requestID)
}

// attachmentPath names one file's keep and delete routes, and the download mint.
func (w *linksWorld) attachmentPath(teamID, requestID int, attachmentID int64, act string) string {
	p := fmt.Sprintf("/api/teams/%d/requests/%d/attachments/%d", teamID, requestID, attachmentID)
	if act != "" {
		p += "/" + act
	}
	return p
}

// stateOf reads a request's stored lifecycle state.
func (w *linksWorld) stateOf(requestID int) domain.RequestStatus {
	w.t.Helper()
	var state string
	if err := w.db.QueryRow(w.ctx, `SELECT state FROM team_requests WHERE id = $1`, requestID).Scan(&state); err != nil {
		w.t.Fatalf("state of %d: %v", requestID, err)
	}
	return domain.RequestStatus(state)
}

// linkRows reads a request's stored relations straight from the table, so a
// refused act's "wrote nothing" is asserted against rows rather than a return value.
func (w *linksWorld) linkRows(requestID int) []domain.RequestLink {
	w.t.Helper()
	rows, err := w.db.Query(w.ctx,
		`SELECT id, request_id, link_kind, target_kind, target_id FROM request_links WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		w.t.Fatalf("links of request %d: %v", requestID, err)
	}
	defer rows.Close()
	var out []domain.RequestLink
	for rows.Next() {
		var l domain.RequestLink
		if err := rows.Scan(&l.ID, &l.RequestID, &l.LinkKind, &l.TargetKind, &l.TargetID); err != nil {
			w.t.Fatalf("scan link: %v", err)
		}
		out = append(out, l)
	}
	return out
}

// linkEvents counts a request's `link` history rows.
func (w *linksWorld) linkEvents(requestID int) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx,
		`SELECT count(*) FROM request_events WHERE request_id = $1 AND action = 'link'`, requestID).Scan(&n); err != nil {
		w.t.Fatalf("count link events of %d: %v", requestID, err)
	}
	return n
}

// auditFor counts this request's audit rows for one action.
func (w *linksWorld) auditFor(action string, requestID int) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND target_type = 'request' AND target_id = $2`,
		action, fmt.Sprint(requestID)).Scan(&n); err != nil {
		w.t.Fatalf("count %s audit rows: %v", action, err)
	}
	return n
}

// attachmentRows counts a request's stored files.
func (w *linksWorld) attachmentRows(requestID int) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx,
		`SELECT count(*) FROM request_attachments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		w.t.Fatalf("count attachments of %d: %v", requestID, err)
	}
	return n
}

// taskIDsOf counts the tickets `planned_as`-linked to a request and returns them.
func (w *linksWorld) plannedTasks(requestID int) []int {
	w.t.Helper()
	var out []int
	for _, l := range w.linkRows(requestID) {
		if l.LinkKind == domain.RequestLinkPlannedAs && l.TargetKind == domain.RequestTargetTask {
			out = append(out, l.TargetID)
		}
	}
	return out
}

// ---- criterion 1a: the attach route ----

// TestRESTRequestLinks_AttachIsRetrySafeAndAuthorized walks the attach route:
//
//   - the first attach is 201 and writes exactly one relation, one `link` history
//     row and one `request.link` audit row;
//   - the same attach again is 200 with changed=false and writes nothing at all
//     (no second row, no second history entry, no second audit entry) — the retry
//     contract the whole surface exists for;
//   - a second, different project is 409 request.conflict naming the current one,
//     and supersede_route moves it while keeping the previous routing as history;
//   - a planned_as to a story and a delivered_by to a ticket land;
//   - a target in another org, a project the caller cannot see and a ticket in it
//     are each the surface's single 404, and nothing is written;
//   - a requester, a sibling team's request and an unknown request id write nothing;
//   - attaching approves nothing, dispatches nothing and moves no state.
func TestRESTRequestLinks_AttachIsRetrySafeAndAuthorized(t *testing.T) {
	w := newLinksWorld(t)
	req := w.mined[0] // the requester's, assigned to the member
	path := w.linkPath(w.team, req.ID)

	// The first attach of the request's route.
	first := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "routes_to", "target_kind": "project", "target_id": w.project,
	}, nil)
	w.wantWriteCode(first, 201, "")
	if changed, _ := first.body["changed"].(bool); !changed {
		t.Fatalf("the first attach reported changed=false: %v", first.body)
	}
	link, _ := first.body["link"].(map[string]any)
	if link == nil {
		t.Fatalf("the first attach answered no relation: %v", first.body)
	}
	if got := link["link_kind"]; got != string(domain.RequestLinkRoutesTo) {
		t.Fatalf("link_kind = %v", got)
	}
	if target, _ := first.body["target"].(map[string]any); target == nil || int(target["project_id"].(float64)) != w.project {
		t.Fatalf("the attach did not report the project it resolved: %v", first.body)
	}
	if got := len(w.linkRows(req.ID)); got != 1 {
		t.Fatalf("the first attach wrote %d relations, want 1", got)
	}
	if got := w.linkEvents(req.ID); got != 1 {
		t.Fatalf("the first attach wrote %d history rows, want 1", got)
	}
	if got := w.auditFor("request.link", req.ID); got != 1 {
		t.Fatalf("the first attach wrote %d audit rows, want 1", got)
	}

	// The replay: 200, changed=false, and nothing at all written.
	replay := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "routes_to", "target_kind": "project", "target_id": w.project,
	}, nil)
	w.wantWriteCode(replay, 200, "")
	if changed, _ := replay.body["changed"].(bool); changed {
		t.Fatalf("a replay reported changed=true: %v", replay.body)
	}
	if got := len(w.linkRows(req.ID)); got != 1 {
		t.Fatalf("a replay wrote a second relation (%d rows)", got)
	}
	if got := w.linkEvents(req.ID); got != 1 {
		t.Fatalf("a replay wrote a second history row (%d)", got)
	}
	if got := w.auditFor("request.link", req.ID); got != 1 {
		t.Fatalf("a replay wrote a second audit row (%d)", got)
	}

	// A second, different project: D9 says one request has one project, so this is a
	// 409 naming the current one — and D9's escape is an explicit supersede.
	conflict := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "routes_to", "target_kind": "project", "target_id": w.project2,
	}, nil)
	w.wantWriteCode(conflict, 409, authz.CodeRequestConflict)
	if msg, _ := conflict.body["error"].(string); !strings.Contains(msg, strconv.Itoa(w.project)) {
		t.Fatalf("the route conflict does not name the current project: %q", msg)
	}
	if got := len(w.linkRows(req.ID)); got != 1 {
		t.Fatalf("a refused route wrote %d relations", got)
	}

	superseded := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "routes_to", "target_kind": "project", "target_id": w.project2, "supersede_route": true,
	}, nil)
	w.wantWriteCode(superseded, 201, "")
	if got := len(w.linkRows(req.ID)); got != 2 {
		t.Fatalf("superseding left %d relations, want both (history is appended, never replaced)", got)
	}

	// planned_as and delivered_by, the two relations a triager's rail shows.
	planned := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "planned_as", "target_kind": "story", "target_id": w.story,
	}, nil)
	w.wantWriteCode(planned, 201, "")
	delivered := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "delivered_by", "target_kind": "task", "target_id": w.deliverable,
	}, nil)
	w.wantWriteCode(delivered, 201, "")

	// A relation the contract never allows, named rather than stored.
	badPair := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "planned_as", "target_kind": "artifact", "target_id": w.deliverable,
	}, nil)
	w.wantWriteCode(badPair, 400, authz.CodeRequestInvalid)

	// A route flag on a relation with no route is answered, not dropped.
	flagOnNonRoute := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "delivered_by", "target_kind": "task", "target_id": w.deliverable, "supersede_route": true,
	}, nil)
	w.wantWriteCode(flagOnNonRoute, 400, authz.CodeRequestInvalid)

	// Never approved, never dispatched, never moved — by this route or any other.
	before := w.stateOf(req.ID)
	linksBefore := len(w.linkRows(req.ID))

	// A target in another org, and work in a project this caller cannot see: both
	// are a 404 and neither writes.
	//
	// The body carries no `request.*` code, and that is the service's own answer
	// (#3962's attachRequestError): "the target is not there" is a statement about
	// a *target*, not about the request, and the request surface's single
	// request.not_found is for the request. A client branching on codes must
	// therefore read the 404 as "this target is not yours or not there" rather
	// than expecting a request.* code on every refusal from this route.
	foreign := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "planned_as", "target_kind": "task", "target_id": w.foreignTaskID(),
	}, nil)
	if foreign.status != 404 {
		t.Fatalf("a target in another org = %d %v, want 404", foreign.status, foreign.body)
	}
	hidden := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"link_kind": "planned_as", "target_kind": "task", "target_id": w.hiddenTask,
	}, nil)
	if hidden.status != 404 {
		t.Fatalf("a target in a project the caller cannot see = %d %v, want 404", hidden.status, hidden.body)
	}
	if fmt.Sprint(foreign.body["error"]) == "" || fmt.Sprint(hidden.body["error"]) == "" {
		t.Fatalf("a refused target answered no message: foreign=%v hidden=%v", foreign.body, hidden.body)
	}
	if got := len(w.linkRows(req.ID)); got != linksBefore {
		t.Fatalf("refused targets wrote %d relations", got-linksBefore)
	}

	// A requester may not attach work to their own request (triage's act), a sibling
	// team cannot reach the request at all, and an id that never existed is the same
	// 404 — all three write nothing.
	for _, c := range []struct {
		what  string
		token string
		path  string
		body  map[string]any
	}{
		{"the requester", w.requesterTok, path,
			map[string]any{"link_kind": "planned_as", "target_kind": "story", "target_id": w.story}},
		{"a sibling team", w.memberTok, w.linkPath(w.otherTeam, req.ID),
			map[string]any{"link_kind": "planned_as", "target_kind": "story", "target_id": w.story}},
		{"an unknown request", w.memberTok, w.linkPath(w.team, 999999),
			map[string]any{"link_kind": "planned_as", "target_kind": "story", "target_id": w.story}},
	} {
		got := w.send(c.token, http.MethodPost, c.path, c.body, nil)
		if got.status != 403 && got.status != 404 {
			t.Fatalf("%s = %d %v, want 403 or 404", c.what, got.status, got.body)
		}
		if code, _ := got.body["code"].(string); code != authz.CodeRequestNotFound {
			t.Fatalf("%s code = %v, want %q", c.what, got.body["code"], authz.CodeRequestNotFound)
		}
	}
	if got := len(w.linkRows(req.ID)); got != linksBefore {
		t.Fatalf("refused callers wrote %d relations", got-linksBefore)
	}
	if got := w.stateOf(req.ID); got != before {
		t.Fatalf("attaching moved the request from %q to %q", before, got)
	}
	if got := w.auditFor("request.handoff", req.ID); got != 0 {
		t.Fatalf("attaching audited a handoff (%d rows)", got)
	}
}

// foreignTaskID is a ticket in the other org's project, for the "another tenant's
// work" case.
func (w *linksWorld) foreignTaskID() int {
	w.t.Helper()
	var id int
	if err := w.db.QueryRow(w.ctx,
		`SELECT id FROM tasks WHERE project_id = $1 ORDER BY id LIMIT 1`, w.foreign).Scan(&id); err != nil {
		w.t.Fatalf("read the foreign project's ticket: %v", err)
	}
	return id
}

// ---- criterion 1b: the handoff route ----

// TestRESTRequestLinks_HandoffIsRetrySafeAndApprovesNothing plans a batch over the
// route and then asks the questions that matter about it:
//
//   - two tickets exist, depend on each other, and quote the request inside the
//     untrusted fence;
//   - neither is approved, dispatched or scheduled, and the REAL dispatch gate
//     refuses each one — the boundary is the product's, not this file's promise;
//   - the same handoff_key replans nothing: created=0, already=2, no new ticket, no
//     new `planned_as` relation and no second audit row;
//   - a new key is a deliberate re-plan and does create;
//   - an unrouted request is 400 naming the route it needs first;
//   - a decision-shaped payload is 400 naming the rule;
//   - 101 items is 400 naming the bound.
func TestRESTRequestLinks_HandoffIsRetrySafeAndApprovesNothing(t *testing.T) {
	w := newLinksWorld(t)
	req := w.mined[1] // the requester's, claimed by the member, in triaging
	path := w.handoffPath(w.team, req.ID)

	// A handoff plans into the request's ACTIVE route, so an unrouted request is
	// refused with what to do about it rather than planned nowhere.
	unrouted := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"handoff_key": "before-route-" + uniq("k"),
		"items":       []any{map[string]any{"ref": "one", "story_id": w.story, "name": "A ticket"}},
	}, nil)
	w.wantWriteCode(unrouted, 400, authz.CodeRequestInvalid)
	if msg, _ := unrouted.body["error"].(string); !strings.Contains(msg, "route") {
		t.Fatalf("the unrouted refusal does not name the route: %q", msg)
	}
	if got := len(w.plannedTasks(req.ID)); got != 0 {
		t.Fatalf("an unrouted handoff linked %d plan items", got)
	}

	route := w.send(w.memberTok, http.MethodPost, w.linkPath(w.team, req.ID), map[string]any{
		"link_kind": "routes_to", "target_kind": "project", "target_id": w.project,
	}, nil)
	w.wantWriteCode(route, 201, "")

	key := "batch-" + uniq("k")
	batch := map[string]any{
		"handoff_key": key,
		"items": []any{
			map[string]any{"ref": "api", "story_id": w.story, "name": "The API step",
				"plan": "Build the endpoint the request needs.", "acceptance_criteria": "It answers 200."},
			map[string]any{"ref": "ui", "story_id": w.story, "name": "The UI step", "depends_on": []any{"api"}},
		},
	}
	first := w.send(w.memberTok, http.MethodPost, path, batch, nil)
	w.wantWriteCode(first, 200, "")
	if got, _ := first.body["project_id"].(float64); int(got) != w.project {
		t.Fatalf("a handoff planned into %v, want the routed project %d: %v", first.body["project_id"], w.project, first.body)
	}
	if created, _ := first.body["created"].(float64); int(created) != 2 {
		t.Fatalf("a first handoff created %v tickets, want 2: %v", first.body["created"], first.body)
	}
	if already, _ := first.body["already"].(float64); int(already) != 0 {
		t.Fatalf("a first handoff reported %v already there", already)
	}
	items, _ := first.body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("the answer names %d items, want 2", len(items))
	}
	taskIDs := w.plannedTasks(req.ID)
	if len(taskIDs) != 2 {
		t.Fatalf("the handoff linked %d plan items, want 2", len(taskIDs))
	}

	// Each created ticket: unapproved, undispatched, not started, in backlog — and
	// the real dispatch gate refuses it with a message naming the approval a person
	// owes. This is criterion 1's approval boundary, driven rather than promised.
	for _, id := range taskIDs {
		ticket, err := w.svc.Repo.GetTask(w.ctx, id)
		if err != nil {
			t.Fatalf("load ticket %d: %v", id, err)
		}
		if ticket.Approved() || ticket.ApprovedAt != nil {
			t.Errorf("the handoff APPROVED ticket %d: approved_at = %v", id, ticket.ApprovedAt)
		}
		if ticket.DispatchedAt != nil || ticket.DispatchID != "" || ticket.StartedAt != nil {
			t.Errorf("the handoff DISPATCHED ticket %d: dispatched_at=%v dispatch_id=%q", id, ticket.DispatchedAt, ticket.DispatchID)
		}
		// Ready work, or blocked on the dependency the batch named — never
		// in_progress or completed: planning creates work, it does not start it.
		if ticket.State != domain.TaskStateBacklog && ticket.State != domain.TaskStateBlocked {
			t.Errorf("ticket %d is %s, want backlog (or blocked on its batch dependency): planning does not start work", id, ticket.State)
		}
		if _, err := w.svc.Dispatch(w.ctx, w.principalOf(w.admin.User.ID), id, "amp:amp-worker"); err == nil {
			t.Fatalf("a handoff-planned ticket was dispatched: the handoff approved the plan")
		} else if authz.StatusOf(err, 500) != 403 || !strings.Contains(err.Error(), "approved") {
			t.Fatalf("the dispatch refusal = %v, want a 403 naming the approval gate", err)
		}
		// The quote: the request's words inside the untrusted fence, so the adapter
		// did not bypass the wrapper (a test that only checks the ticket exists
		// proves nothing about injection safety).
		if !strings.Contains(ticket.Description, domain.UntrustedBegin) || !strings.Contains(ticket.Description, domain.UntrustedEnd) {
			t.Errorf("ticket %d's description is not fenced: %q", id, ticket.Description)
		}
		if !strings.Contains(ticket.Description, req.Subject) {
			t.Errorf("ticket %d's description does not quote the request: %q", id, ticket.Description)
		}
	}

	// The retry: same key, nothing replanned and nothing audited.
	auditsBefore := w.auditFor("request.handoff", req.ID)
	retry := w.send(w.memberTok, http.MethodPost, path, batch, nil)
	w.wantWriteCode(retry, 200, "")
	if created, _ := retry.body["created"].(float64); int(created) != 0 {
		t.Fatalf("a retry created %v tickets, want 0: %v", retry.body["created"], retry.body)
	}
	if already, _ := retry.body["already"].(float64); int(already) != 2 {
		t.Fatalf("a retry reported %v already there, want 2: %v", retry.body["already"], retry.body)
	}
	if got := len(w.plannedTasks(req.ID)); got != 2 {
		t.Fatalf("a retry left %d plan items, want 2", got)
	}
	if got := w.auditFor("request.handoff", req.ID); got != auditsBefore {
		t.Fatalf("a retry wrote %d new handoff audit rows", got-auditsBefore)
	}

	// A new key is a deliberate re-plan (the contract's own wording), so it creates.
	replan := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"handoff_key": "batch-" + uniq("k"),
		"items":       []any{map[string]any{"ref": "followup", "story_id": w.story, "name": "A follow-up"}},
	}, nil)
	w.wantWriteCode(replan, 200, "")
	if created, _ := replan.body["created"].(float64); int(created) != 1 {
		t.Fatalf("a re-plan with a new key created %v tickets, want 1", replan.body["created"])
	}

	// A decision-shaped payload is refused BY NAME: a caller that sent start_at
	// believes it scheduled work.
	for _, c := range []struct {
		field string
		body  map[string]any
	}{
		{"assigned_to", map[string]any{"handoff_key": "k-" + uniq("x"), "items": []any{},
			"assigned_to": "someone@example.com"}},
		{"start_at", map[string]any{"handoff_key": "k-" + uniq("x"), "items": []any{},
			"start_at": "2027-01-01T00:00:00Z"}},
		{"project_id", map[string]any{"handoff_key": "k-" + uniq("x"), "items": []any{},
			"project_id": w.foreign}},
		{"approve", map[string]any{"handoff_key": "k-" + uniq("x"), "items": []any{}, "approve": true}},
	} {
		got := w.send(w.memberTok, http.MethodPost, path, c.body, nil)
		w.wantWriteCode(got, 400, authz.CodeRequestInvalid)
		// The refusal must EXPLAIN the rule, not merely repeat the field name: the
		// decoder's generic "unknown field" message would satisfy a name-only
		// assertion, and that is exactly the answer this route exists to improve on.
		// "planning a request decides what work serves it" is the rule's own first
		// clause (handoffNoDecisionRule), and `project_id` gets a tenant sentence
		// instead — so the assertion is that the message is not the decoder's.
		msg, _ := got.body["error"].(string)
		if !strings.Contains(msg, c.field) {
			t.Fatalf("the refusal for %q does not name it: %q", c.field, msg)
		}
		if strings.Contains(msg, "unknown field") {
			t.Fatalf("the refusal for %q is the decoder's generic message rather than the rule: %q", c.field, msg)
		}
	}

	// The batch's own bound, restated so the refusal names the number rather than
	// arriving uncoded from inside the planning path.
	oversize := make([]any, 0, 101)
	for i := 0; i < 101; i++ {
		oversize = append(oversize, map[string]any{"ref": fmt.Sprintf("r%d", i), "story_id": w.story, "name": "n"})
	}
	tooMany := w.send(w.memberTok, http.MethodPost, path, map[string]any{
		"handoff_key": "big-" + uniq("k"), "items": oversize,
	}, nil)
	w.wantWriteCode(tooMany, 400, authz.CodeRequestInvalid)
	if msg, _ := tooMany.body["error"].(string); !strings.Contains(msg, "100") {
		t.Fatalf("the over-size refusal does not name the bound: %q", msg)
	}
}

// ---- criterion 2: uploads, the panel, the gate and retention ----

// TestRESTRequestAttachments_UploadPanelAndGate walks the attachment surface end to
// end for the two callers the contract gives it to:
//
//   - the requester uploads a file to their own request: 201, the row's content type
//     is what AMP sniffed (not the name's), and the panel lists it as ready;
//   - the mint and serve route still returns exactly those bytes once, under the
//     download posture (the download adapter is #3963's and is reused unchanged);
//   - a team member uploads to the same request, and the file arrives held when the
//     bytes are a binary AMP cannot vouch for;
//   - a stranger, a sibling team and another org are the surface's one 404 on the
//     panel, the upload, the keep, the delete and the mint;
//   - a requester deletes their own file and may not keep it; the team may;
//   - the sixth file is refused by the domain's count bound.
func TestRESTRequestAttachments_UploadPanelAndGate(t *testing.T) {
	w := newLinksWorld(t)
	req := w.mined[0] // the requester's request, so the requester may upload to it
	panel := w.attachmentsPath(w.team, req.ID)

	content := []byte("the requester's private notes: do not leak these\n")
	upload := w.send(w.requesterTok, http.MethodPost, panel, map[string]any{
		"file_name": "notes.txt", "content_base64": base64.StdEncoding.EncodeToString(content),
	}, nil)
	w.wantWriteCode(upload, 201, "")
	att, _ := upload.body["attachment"].(map[string]any)
	if att == nil {
		t.Fatalf("the upload answered no attachment: %v", upload.body)
	}
	fileID := int64(att["id"].(float64))
	// The type is the sniffer's, never the name's: a .txt of plain bytes is text/plain.
	if got := att["content_type"]; got != "text/plain" {
		t.Fatalf("content_type = %v, want the sniffed text/plain", got)
	}
	if att["content"] != nil {
		t.Fatal("the upload answer carried bytes: uploading must not become a read path")
	}
	if held, _ := upload.body["held"].(bool); held {
		t.Fatalf("a plain text file arrived held: %v", upload.body)
	}

	// The panel: the file is listed, ready, with the bounds beside it.
	status, _, raw := w.get(w.requesterTok, panel)
	if status != 200 {
		t.Fatalf("the requester's panel = %d %s", status, raw)
	}
	var listing map[string]any
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatalf("decode panel: %v (%s)", err, raw)
	}
	if count, _ := listing["count"].(float64); int(count) != 1 {
		t.Fatalf("the panel lists %v files, want 1: %s", listing["count"], raw)
	}
	items, _ := listing["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("the panel's items = %d, want 1", len(items))
	}
	row, _ := items[0].(map[string]any)
	if state, _ := row["state"].(map[string]any); state == nil || state["ready"] != true {
		t.Fatalf("the uploaded file's panel state is not ready: %v", row["state"])
	}
	if strings.Contains(string(raw), "private notes") {
		t.Fatal("the panel leaked the file's bytes")
	}
	limits, _ := listing["limits"].(map[string]any)
	if limits == nil || int(limits["max_files"].(float64)) != domain.MaxRequestAttachments {
		t.Fatalf("the panel did not report the domain's file bound: %v", listing["limits"])
	}

	// The download path, unchanged from #3963: mint then serve, once.
	minted := w.send(w.requesterTok, http.MethodPost, w.attachmentPath(w.team, req.ID, fileID, "download"), nil, nil)
	w.wantWriteCode(minted, 200, "")
	dl, _ := minted.body["download"].(map[string]any)
	if dl == nil {
		t.Fatalf("the mint answered no download: %v", minted.body)
	}
	url, _ := dl["url"].(string)
	if !strings.HasPrefix(url, "/api/request-downloads/") {
		t.Fatalf("the minted url %q is not a path on AMP's own surface", url)
	}
	serveStatus, hdr, body := w.get(w.requesterTok, url)
	if serveStatus != 200 {
		t.Fatalf("following the mint = %d %s", serveStatus, body)
	}
	if string(body) != string(content) {
		t.Fatalf("served %q, want the uploaded bytes", body)
	}
	if got := hdr.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want attachment", got)
	}
	if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if again := w.send(w.requesterTok, http.MethodGet, url, nil, nil); again.status != 404 {
		t.Fatalf("a spent download URL answered %d", again.status)
	}

	// A team member uploads to the same request; a binary AMP cannot vouch for
	// arrives held, which is the one state a UI branches on immediately.
	binary := make([]byte, 64)
	for i := range binary {
		binary[i] = 0x7f // the sniffer's fallback: application/octet-stream
	}
	held := w.send(w.memberTok, http.MethodPost, panel, map[string]any{
		"file_name": "tool.exe", "content_base64": base64.StdEncoding.EncodeToString(binary),
	}, nil)
	w.wantWriteCode(held, 201, "")
	if got, _ := held.body["held"].(bool); !got {
		t.Fatalf("a binary named .exe did not arrive held: %v", held.body)
	}
	heldAtt, _ := held.body["attachment"].(map[string]any)
	heldID := int64(heldAtt["id"].(float64))
	if state, _ := heldAtt["state"].(map[string]any); state["refusal"] != string(domain.RequestAttachmentRefusedHeld) {
		t.Fatalf("the held file's state = %v, want held_for_release", heldAtt["state"])
	}
	// And the gate agrees: a mint for it is refused, so a listing and a URL cannot
	// say different things.
	heldMint := w.send(w.memberTok, http.MethodPost, w.attachmentPath(w.team, req.ID, heldID, "download"), nil, nil)
	if heldMint.status != 403 {
		t.Fatalf("a mint for a held file = %d %v, want 403", heldMint.status, heldMint.body)
	}

	// Retention: the team may keep, the requester may not (D3 — "a team keep"), and
	// the requester may withdraw their own content.
	kept := w.send(w.memberTok, http.MethodPost, w.attachmentPath(w.team, req.ID, fileID, "keep"),
		map[string]any{"keep": true}, nil)
	w.wantWriteCode(kept, 200, "")
	if v, _ := kept.body["keep"].(bool); !v {
		t.Fatalf("the keep did not pin the file: %v", kept.body)
	}
	keptAtt, _ := kept.body["attachment"].(map[string]any)
	if keptAtt["expires_at"] != nil {
		t.Fatalf("a kept file still carries an expiry: %v", keptAtt["expires_at"])
	}
	requesterKeep := w.send(w.requesterTok, http.MethodPost, w.attachmentPath(w.team, req.ID, fileID, "keep"),
		map[string]any{"keep": true}, nil)
	if requesterKeep.status != 403 {
		t.Fatalf("a requester's keep = %d %v, want 403", requesterKeep.status, requesterKeep.body)
	}

	deleted := w.send(w.requesterTok, http.MethodDelete, w.attachmentPath(w.team, req.ID, fileID, ""), nil, nil)
	w.wantWriteCode(deleted, 200, "")
	if v, _ := deleted.body["deleted"].(bool); !v {
		t.Fatalf("the delete did not remove the file: %v", deleted.body)
	}
	// A second delete finds nothing to name and answers the surface's 404 — the same
	// bytes an id that never existed gets, which is the product's answer rather than
	// a 200 claiming a row was removed twice.
	again := w.send(w.requesterTok, http.MethodDelete, w.attachmentPath(w.team, req.ID, fileID, ""), nil, nil)
	w.wantWriteCode(again, 404, authz.CodeRequestNotFound)

	// Every foreign caller is the surface's single 404 — on the panel, the upload,
	// the keep, the delete and the mint.
	for _, c := range []struct {
		what   string
		token  string
		method string
		path   string
		body   map[string]any
	}{
		{"a stranger's panel", w.strangerTok, http.MethodGet, panel, nil},
		{"another org's upload", w.strangerTok, http.MethodPost, panel,
			map[string]any{"file_name": "x.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("x"))}},
		{"another org's keep", w.strangerTok, http.MethodPost, w.attachmentPath(w.team, req.ID, heldID, "keep"),
			map[string]any{"keep": true}},
		{"another org's delete", w.strangerTok, http.MethodDelete, w.attachmentPath(w.team, req.ID, heldID, ""), nil},
		{"a sibling team's panel", w.memberTok, http.MethodGet, w.attachmentsPath(w.otherTeam, req.ID), nil},
		{"an unknown request's panel", w.memberTok, http.MethodGet, w.attachmentsPath(w.team, 999999), nil},
	} {
		got := w.send(c.token, c.method, c.path, c.body, nil)
		if got.status != 404 {
			t.Fatalf("%s = %d %v, want the surface's 404", c.what, got.status, got.body)
		}
		if code, _ := got.body["code"].(string); code != authz.CodeRequestNotFound {
			t.Fatalf("%s code = %v, want %q", c.what, got.body["code"], authz.CodeRequestNotFound)
		}
	}
	if got := w.attachmentRows(req.ID); got != 1 {
		t.Fatalf("the foreign refusals left %d files, want only the held one", got)
	}

	// The request's own count bound: five files is the domain's ceiling, so filling
	// it and adding a sixth is a coded refusal that stores nothing.
	for i := 0; i < domain.MaxRequestAttachments-1; i++ {
		got := w.send(w.memberTok, http.MethodPost, panel, map[string]any{
			"file_name": fmt.Sprintf("more-%d.txt", i), "content_base64": base64.StdEncoding.EncodeToString([]byte("more")),
		}, nil)
		if got.status != 201 {
			t.Fatalf("upload %d = %d %v", i, got.status, got.body)
		}
	}
	sixth := w.send(w.memberTok, http.MethodPost, panel, map[string]any{
		"file_name": "sixth.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("sixth")),
	}, nil)
	w.wantWriteCode(sixth, 400, authz.CodeRequestInvalid)
	if got := w.attachmentRows(req.ID); got != domain.MaxRequestAttachments {
		t.Fatalf("the request holds %d files, want %d", got, domain.MaxRequestAttachments)
	}
}

// ---- criterion 3: malformed and oversized routes write nothing ----

// TestRESTRequestLinks_CodedRefusalsWriteNothing sends every malformed payload these
// two files can receive and asserts two things about each: the status and code a
// client branches on, and that nothing at all was written — no relation, no file, no
// audit row. The second half is the one a "returns 400" test usually misses.
func TestRESTRequestLinks_CodedRefusalsWriteNothing(t *testing.T) {
	w := newLinksWorld(t)
	req := w.mined[3] // the member's own request, so the member may upload to it
	panel := w.attachmentsPath(w.team, req.ID)
	links := w.linkPath(w.team, req.ID)
	handoff := w.handoffPath(w.team, req.ID)

	filesBefore := w.attachmentRows(req.ID)
	linksBefore := len(w.linkRows(req.ID))
	auditsBefore := w.auditFor("request.attachment.upload", req.ID) +
		w.auditFor("request.link", req.ID) + w.auditFor("request.handoff", req.ID)

	one := base64.StdEncoding.EncodeToString([]byte("small file"))
	for _, c := range []struct {
		what   string
		method string
		path   string
		body   any
		raw    string
		status int
		code   string
		names  string
	}{
		// An upload may not name a location: a dropped `url` would leave the caller
		// believing a file was attached from a link AMP opened.
		{what: "a url on an upload", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "url": "https://example.com/a.txt"},
			code: authz.CodeRequestInvalid, names: "url"},
		{what: "a content_url on an upload", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "content_url": "https://example.com/a.txt"},
			code: authz.CodeRequestInvalid, names: "content_url"},
		{what: "a path on an upload", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "path": "/etc/passwd"},
			code: authz.CodeRequestInvalid, names: "path"},
		// A claim about the file rather than the file itself.
		{what: "a content_type claim", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "content_base64": one, "content_type": "image/png"},
			code: authz.CodeRequestInvalid, names: "content_type"},
		{what: "a scan_state claim", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "content_base64": one, "scan_state": "clean"},
			code: authz.CodeRequestInvalid, names: "scan_state"},
		// Payloads that cannot be a file.
		{what: "an empty file", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "content_base64": ""},
			code: authz.CodeRequestInvalid, names: "content_base64"},
		{what: "an unreadable payload", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "content_base64": "not base64 at all: !!secret!!"},
			code: authz.CodeRequestInvalid, names: "RFC 4648"},
		// The per-file bound, checked on the ENCODED length before it is decoded.
		{what: "a payload over the per-file bound", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "big.bin", "content_base64": strings.Repeat("A", encodedAttachmentLimit()+1)},
			code: authz.CodeRequestInvalid, names: "per-file limit"},
		// The transport's own bound, reached before the decoder reads a body.
		{what: "a body over the transport bound", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "huge.bin", "content_base64": strings.Repeat("A", encodedAttachmentLimit()+8192)},
			code: authz.CodeRequestInvalid, names: "per-file limit"},
		// Two JSON values and an unknown field, the decoder's own refusals.
		{what: "two JSON values", method: http.MethodPost, path: panel,
			raw: `{"file_name":"a.txt","content_base64":"` + one + `"} {}`, code: authz.CodeRequestInvalid, names: "more than one"},
		{what: "an unknown field", method: http.MethodPost, path: panel,
			body: map[string]any{"file_name": "a.txt", "content_base64": one, "nonsense": true},
			code: authz.CodeRequestInvalid, names: "nonsense"},
		// A keep with no flag would silently start a file's retention clock.
		{what: "a keep with no flag", method: http.MethodPost,
			path: w.attachmentPath(w.team, req.ID, 1, "keep"), body: map[string]any{},
			code: authz.CodeRequestInvalid, names: "keep is required"},
		// Path segments that cannot name anything.
		{what: "a malformed attachment id", method: http.MethodDelete,
			path: fmt.Sprintf("/api/teams/%d/requests/%d/attachments/abc", w.team, req.ID),
			code: authz.CodeRequestInvalid, names: "attachment"},
		{what: "a reference instead of a serial", method: http.MethodPost,
			path: w.linkPath(w.team, 0)[:0] + fmt.Sprintf("/api/teams/%d/requests/%s/links", w.team, req.PublicRef),
			body: map[string]any{"link_kind": "routes_to", "target_kind": "project", "target_id": w.project},
			code: authz.CodeRequestInvalid, names: "serial"},
		// A handoff's own malformed shapes.
		{what: "a handoff with no key", method: http.MethodPost, path: handoff,
			body: map[string]any{"items": []any{map[string]any{"ref": "a", "story_id": w.story, "name": "n"}}},
			code: authz.CodeRequestInvalid, names: "handoff_key"},
		{what: "a handoff item that is a string", method: http.MethodPost, path: handoff,
			body: map[string]any{"handoff_key": "k-" + uniq("x"), "items": []any{"not an object"}},
			code: authz.CodeRequestInvalid, names: "items"},
		{what: "a handoff with a malformed depends_on", method: http.MethodPost, path: handoff,
			body: map[string]any{"handoff_key": "k-" + uniq("x"), "items": []any{
				map[string]any{"ref": "a", "story_id": w.story, "name": "n", "depends_on": []any{map[string]any{"x": 1}}},
			}},
			code: authz.CodeRequestInvalid, names: "depends_on"},
	} {
		var got writeResult
		if c.raw != "" {
			got = w.sendRaw(w.memberTok, c.method, c.path, c.raw)
		} else {
			got = w.send(w.memberTok, c.method, c.path, c.body, nil)
		}
		if got.status != 400 {
			t.Fatalf("%s = %d %v, want 400", c.what, got.status, got.body)
		}
		if code, _ := got.body["code"].(string); code != c.code {
			t.Fatalf("%s code = %v, want %q (%v)", c.what, got.body["code"], c.code, got.body)
		}
		msg, _ := got.body["error"].(string)
		if !strings.Contains(msg, c.names) {
			t.Fatalf("%s refusal = %q, want it to name %q", c.what, msg, c.names)
		}
		// A location- or claim-shaped field must be refused WITH THE RULE, not with
		// the decoder's generic message: a bare "unknown field \"url\"" tells a caller
		// nothing about why AMP refuses the shape, and that explanation is the whole
		// point of refusing the field by name rather than dropping it.
		if decoderRefusalIsGeneric(msg) && uploadExplanationFields[namedUploadField(c.body)] {
			t.Fatalf("%s refusal is the decoder's generic message rather than the rule: %q", c.what, msg)
		}
		// The content is never echoed back: a base64 string is a file.
		if strings.Contains(got.raw, "secret") {
			t.Fatalf("%s echoed the payload: %q", c.what, got.raw)
		}
	}

	// Nothing was written by any of them.
	if got := w.attachmentRows(req.ID); got != filesBefore {
		t.Fatalf("the malformed payloads stored %d files", got-filesBefore)
	}
	if got := len(w.linkRows(req.ID)); got != linksBefore {
		t.Fatalf("the malformed payloads wrote %d relations", got-linksBefore)
	}
	after := w.auditFor("request.attachment.upload", req.ID) +
		w.auditFor("request.link", req.ID) + w.auditFor("request.handoff", req.ID)
	if after != auditsBefore {
		t.Fatalf("the malformed payloads wrote %d audit rows", after-auditsBefore)
	}

	// The submit-only credential reaches exactly one route, and none of these is it:
	// the gate refuses before any handler runs, which is what makes "no route can
	// forget it" true.
	for _, c := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, links},
		{http.MethodPost, handoff},
		{http.MethodGet, panel},
		{http.MethodPost, panel},
		{http.MethodPost, w.attachmentPath(w.team, req.ID, 1, "keep")},
		{http.MethodDelete, w.attachmentPath(w.team, req.ID, 1, "")},
		{http.MethodPost, w.attachmentPath(w.team, req.ID, 1, "download")},
	} {
		got := w.send(w.scopedTok, c.method, c.path, map[string]any{}, nil)
		if got.status != 403 {
			t.Fatalf("submit-only %s %s = %d %v, want 403", c.method, c.path, got.status, got.body)
		}
		if code, _ := got.body["code"].(string); code != authz.CodeTokenScopeDenied {
			t.Fatalf("submit-only %s %s code = %v, want %q", c.method, c.path, got.body["code"], authz.CodeTokenScopeDenied)
		}
	}

	// A wrong method is a route that does not exist, not a handler that must
	// remember to refuse it: every request route here is registered as a
	// method+path pair, so the mux itself answers 404 (this surface's convention —
	// the same holds for the writes, the reads and the lifecycle acts). The one
	// exception is the download prefix, which answers 405 because it is registered
	// by prefix.
	wrongMethod := w.send(w.memberTok, http.MethodPut, panel, map[string]any{}, nil)
	if wrongMethod.status != 404 {
		t.Fatalf("PUT on the attachment panel = %d, want 404 (no such route)", wrongMethod.status)
	}
	wrongRetention := w.send(w.memberTok, http.MethodGet, w.attachmentPath(w.team, req.ID, 1, "keep"), nil, nil)
	if wrongRetention.status != 404 {
		t.Fatalf("GET on a keep = %d, want 404 (no such route)", wrongRetention.status)
	}
}

// uploadExplanationFields are the upload body's refused-by-name fields whose
// refusal must explain itself. They are the keys internal/api/request_uploads.go's
// remoteFetchFields map answers with a sentence; a payload naming one of them is
// answered with the rule (never fetches a location, never claims a type or a scan
// verdict) rather than with encoding/json's generic "unknown field".
var uploadExplanationFields = map[string]bool{
	"url": true, "uri": true, "href": true, "link": true, "source_url": true,
	"content_url": true, "file_url": true, "download_url": true, "fetch": true,
	"fetch_url": true, "remote": true, "remote_url": true, "path": true, "file_path": true,
	"content_type": true, "scan_state": true, "size_bytes": true,
}

// namedUploadField returns the upload body's single refused-by-name field, if it
// carries one. It is how the assertion below knows which case it is looking at: the
// table's `names` says what the message must contain, and this says which field the
// payload actually named.
func namedUploadField(body any) string {
	m, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	for k := range m {
		if uploadExplanationFields[k] {
			return k
		}
	}
	return ""
}

// decoderRefusalIsGeneric reports whether a refusal is encoding/json's own
// "unknown field" answer rather than a refusal that explains itself.
func decoderRefusalIsGeneric(msg string) bool { return strings.Contains(msg, "unknown field") }

// encodedAttachmentLimit is the base64 length of one file at the domain's per-file
// bound. The adapter derives its body bound from the same number, so a test that
// wants to stand just past the per-file check uses this rather than a round figure.
func encodedAttachmentLimit() int {
	return base64.StdEncoding.EncodedLen(domain.MaxRequestAttachmentBytes)
}
