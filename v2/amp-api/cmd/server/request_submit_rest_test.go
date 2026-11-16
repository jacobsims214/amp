package main

// Team request submission over REST (epic 202, story 543, AMP #3919; build
// slice F1's submit half; approved contract artifact 36 v2 §2 and §5).
//
// internal/api/request_submit.go is a thin adapter over Service.SubmitRequest,
// which cmd/server/request_submit_test.go already proves as a boundary and
// cmd/server/request_submit_mcp_test.go proves through the sibling MCP tool. So
// this file asks only the questions that are about *the route*:
//
//	Criterion 1 — an authorized projectless POST and its replay succeed:
//	  TestRESTSubmitRequest_CreatesAndReplays (create 201 + replay 200 with the
//	  same public_ref and one row), TestRESTSubmitRequest_NoProjectNamed
//	  (zero project access before/after, no project in the path or body)
//	Criterion 2 — denied / oversized / invalid-key answers are coded and refused
//	  without a write: TestRESTSubmitRequest_RefusalsAreCodedAndWriteNothing
//	  (foreign team, revoked grant, unknown team, submit-only credential bound
//	  elsewhere, oversized body, oversized multibyte key, spoofed fields, two
//	  JSON values), TestRESTSubmitRequest_DenialIsAudited (a 403 leaves one
//	  access.denied row; a 404 and a 400 leave none)
//	Criterion 3 — a rate-limited response carries Retry-After:
//	  TestRESTSubmitRequest_RateLimitedCarriesRetryAfter (429 + header + code +
//	  retry_after_s, and a replay is still free)
//
// Around the criteria the file pins the two things a surface can get wrong that
// the service cannot: TestRESTSubmitRequest_AnswerLeaksNothing checks the answer
// is the requester whitelist and not the stored row, and
// TestRESTSubmitRequest_ScopeAndRouteBoundary checks the submit-only credential
// reaches exactly this route for its bound team and that GET on the same path is
// still a read it may not make.
//
// Every request goes through the real REST mux and the real
// auth.NewAPITokenVerifier middleware (a stored `amp_` token row), so the channel
// and scope the adapter reads are the ones the verifier sets. The database is a
// scratch database per test (freshDatabase), so counts are exact.
//
// Run against a disposable Postgres (never a live stack), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTSubmitRequest' -count=1 -race -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// restSubmitWorld is one tenant with an inbox team, a sibling team it must not
// reach, a second org, and the identities a REST submission's caller can be.
type restSubmitWorld struct {
	t      *testing.T
	srv    *httptest.Server
	ctx    context.Context
	db     *pgx.Conn
	svc    *service.Service
	now    time.Time
	repo   *repository.Repo
	apiTok *service.NewToken // a full-scope token for member
	// member's own full-scope token secret, and the submit-only one bound to
	// the inbox (both minted through the service, as a person in the web UI
	// would mint them).
	full, scoped string

	org, team, otherTeam int // the tenant, the inbox, a sibling team
	otherOrg, foreign    int // a second tenant and its team

	member   *auth.Identity // member of the inbox team
	admin    *auth.Identity // owner of the org
	customer *auth.Identity // customer kind holding a submit grant
	other    *auth.Identity // customer kind in otherOrg, no grant
	stranger *auth.Identity // owner of otherOrg
}

func newRESTSubmitWorld(t *testing.T) *restSubmitWorld {
	t.Helper()
	return newRESTSubmitWorldLimit(t, 30)
}

// newRESTSubmitWorldLimit is newRESTSubmitWorld with an explicit per-window
// ceiling, so a quota assertion is one probe away. The limiter is installed on
// the same service the REST handler uses (via Handler's Service()), which is
// what makes the route's 429 the real limiter's answer and not a stub.
func newRESTSubmitWorldLimit(t *testing.T, maxPerWindow int) *restSubmitWorld {
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

	w := &restSubmitWorld{t: t, ctx: ctx, db: db, repo: repo,
		now: time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)}
	insert := func(sql string, args ...any) int {
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
	sfx := uniq("restsubmit")
	// The first user ever provisioned is auto-admin, so every role is pinned.
	user := func(label, kind string, admin bool) *auth.Identity {
		u, err := repo.UpsertUserFromClaims(ctx, sfx+"-"+label,
			fmt.Sprintf("%s-%s@rest-submit.test", label, sfx), label, nil)
		if err != nil {
			t.Fatalf("user %s: %v", label, err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleMember, !admin); err != nil {
			t.Fatal(err)
		}
		if admin {
			if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, true); err != nil {
				t.Fatal(err)
			}
		}
		if kind == domain.UserKindCustomer {
			exec(`UPDATE users SET kind = $2 WHERE id = $1`, u.ID, kind)
			u.Kind = kind
		}
		return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: label, User: u}
	}

	w.org = insert(`INSERT INTO orgs (name, slug) VALUES ('REST submit org', $1) RETURNING id`, sfx)
	w.team = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, w.org)
	w.otherTeam = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other desk') RETURNING id`, w.org)
	w.otherOrg = insert(`INSERT INTO orgs (name, slug) VALUES ('REST submit other', $1) RETURNING id`, sfx+"-o")
	w.foreign = insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign inbox') RETURNING id`, w.otherOrg)

	w.stranger = user("stranger", domain.UserKindHuman, true)
	w.member = user("member", domain.UserKindHuman, false)
	w.admin = user("admin", domain.UserKindHuman, false)
	w.customer = user("customer", domain.UserKindCustomer, false)
	w.other = user("othercustomer", domain.UserKindCustomer, false)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.org, w.member.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.org, w.admin.User.ID)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, w.team, w.member.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.otherOrg, w.other.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.otherOrg, w.stranger.User.ID)
	if _, err := repo.GrantTeamSubmit(ctx, w.team, w.org, w.customer.User.ID, nil); err != nil {
		t.Fatalf("grant the customer: %v", err)
	}

	// The tokens, minted through the service as a person in the web UI would.
	ui := authz.Principal{UserID: w.member.User.ID, Email: w.member.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	svc := service.New(repo, nil, nil)
	full, err := svc.CreateToken(ctx, ui, w.member.User.ID, "rest-full", 0)
	if err != nil {
		t.Fatalf("mint full token: %v", err)
	}
	w.full = full.Token
	scoped, err := svc.CreateScopedToken(ctx, ui, w.member.User.ID, "rest-scoped", 0,
		service.TokenScopeRequest{Scope: "submit", TeamID: w.team})
	if err != nil {
		t.Fatalf("mint submit-only token: %v", err)
	}
	w.scoped = scoped.Token

	// The handler's own service, with the limiter and clock the test drives.
	mux := http.NewServeMux()
	h := api.NewRestHandler(nil, repo, nil, nil)
	svcRoute := h.Service()
	svcRoute.RequestLimits = service.NewRequestLimiter(service.RequestLimitConfig{Window: time.Minute, Max: maxPerWindow})
	svcRoute.Now = func() time.Time { return w.now }
	w.svc = svcRoute
	h.Register(mux)
	// The real API-token verifier: the channel and the token's scope are what
	// the verifier sets from the stored row, not something this test hands the
	// handler.
	w.srv = httptest.NewServer(auth.NewAPITokenVerifier(repo).Middleware(mux))
	t.Cleanup(w.srv.Close)
	return w
}

// restResult is one response: status, headers and the decoded body.
type restResult struct {
	status  int
	header  http.Header
	body    map[string]any
	rawBody string
}

// do sends a raw body (so a test can send malformed, oversized or multi-value
// payloads) as the bearer token, with an optional Idempotency-Key.
func (w *restSubmitWorld) do(token, method, path, body string, key string) restResult {
	w.t.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := restResult{status: resp.StatusCode, header: resp.Header, rawBody: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// submitJSON is one well-formed submission body: a subject and a brief. There is
// deliberately no team, project, requester or org field — the path names the
// team and the principal is the requester.
func submitJSON(subject, brief string) string {
	b, _ := json.Marshal(map[string]any{"subject": subject, "input_brief": brief})
	return string(b)
}

// inbox is the inbox path.
func (w *restSubmitWorld) inbox(teamID int) string {
	return fmt.Sprintf("/api/teams/%d/requests", teamID)
}

// wantCode asserts a refusal's status and stable code.
func (w *restSubmitWorld) wantCode(r restResult, status int, code string) {
	w.t.Helper()
	if r.status != status {
		w.t.Fatalf("%d %v, want %d %s", r.status, r.body, status, code)
	}
	if got, _ := r.body["code"].(string); got != code {
		w.t.Fatalf("code = %v, want %q (body %v)", r.body["code"], code, r.body)
	}
}

// requestCount is how many requests exist in this world's org — read straight
// from the database, which is how "the refusal wrote nothing" stays exact.
func (w *restSubmitWorld) requestCount() int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx, `SELECT count(*) FROM team_requests WHERE org_id = $1`, w.org).Scan(&n); err != nil {
		w.t.Fatalf("count requests: %v", err)
	}
	return n
}

// auditRowsFor is the audit rows this world's caller produced for one action.
func (w *restSubmitWorld) auditRowsFor(userID int, action string) []domain.AuditEntry {
	w.t.Helper()
	rows, err := w.repo.ListAudit(w.ctx, domain.AuditFilter{ActorUserID: userID, Action: action, Limit: 200})
	if err != nil {
		w.t.Fatalf("read audit: %v", err)
	}
	return rows
}

// ---- criterion 1: an authorized projectless create and its replay ----

// TestRESTSubmitRequest_CreatesAndReplays: a member POSTs with no project named
// anywhere, gets 201 with the request's public reference, and a replay with the
// same Idempotency-Key gets 200 with the SAME public_ref and `created: false`
// while the database holds exactly one request. The stored row carries the
// service's own products — the `web` source, the member's requester id, state
// `new` — because the adapter is a thin caller, not a second implementation.
func TestRESTSubmitRequest_CreatesAndReplays(t *testing.T) {
	w := newRESTSubmitWorld(t)
	subject := "CSV export of the queue " + uniq("s")
	body := submitJSON(subject, "We need the triage queue as CSV for our weekly review.")
	key := "rest-replay-" + uniq("k")

	created := w.do(w.full, "POST", w.inbox(w.team), body, key)
	if created.status != 201 {
		t.Fatalf("create = %d %v, want 201", created.status, created.body)
	}
	if got := created.body["created"]; got != true {
		t.Fatalf("created = %v, want true for a first submission", got)
	}
	ref := requestFieldOf(t, created, "public_ref")
	if ref == "" {
		t.Fatal("no public_ref in the answer")
	}
	if state := requestFieldOf(t, created, "state"); state != string(domain.RequestStatusNew) {
		t.Fatalf("state = %q, want %q", state, domain.RequestStatusNew)
	}
	if sk := requestFieldOf(t, created, "source_kind"); sk != string(domain.RequestSourceAPI) {
		t.Fatalf("source_kind = %q, want %q for an API token", sk, domain.RequestSourceAPI)
	}

	// The stored row says what the answer said, and says it about this caller.
	var (
		storedRef, source, storedSubject string
		requesterID                      *int
	)
	if err := w.db.QueryRow(w.ctx, `SELECT public_ref, source_kind, subject, requester_user_id
	                                FROM team_requests WHERE public_ref = $1`, ref).
		Scan(&storedRef, &source, &storedSubject, &requesterID); err != nil {
		t.Fatalf("read the request %q: %v", ref, err)
	}
	if requesterID == nil || *requesterID != w.member.User.ID {
		t.Fatalf("requester_user_id = %v, want the caller's %d", requesterID, w.member.User.ID)
	}
	if storedSubject != subject || source != string(domain.RequestSourceAPI) {
		t.Fatalf("stored subject/source = %q/%q, want %q/api", storedSubject, source, subject)
	}
	// One history row and one audit row went with the one request.
	if got := w.countRows(`SELECT count(*) FROM request_events WHERE request_id =
	                          (SELECT id FROM team_requests WHERE public_ref = $1) AND action = 'submit'`, ref); got != 1 {
		t.Fatalf("%d submit history rows, want 1", got)
	}
	if got := len(w.auditRowsFor(w.member.User.ID, "request.submit")); got != 1 {
		t.Fatalf("%d request.submit audit rows, want 1", got)
	}

	// The replay: same key, same payload → the same request, 200, created false.
	replay := w.do(w.full, "POST", w.inbox(w.team), body, key)
	if replay.status != 200 {
		t.Fatalf("replay = %d %v, want 200", replay.status, replay.body)
	}
	if got := replay.body["created"]; got != false {
		t.Fatalf("replay created = %v, want false", got)
	}
	if again := requestFieldOf(t, replay, "public_ref"); again != ref {
		t.Fatalf("replay answered %q, want the original %q", again, ref)
	}
	if got := w.requestCount(); got != 1 {
		t.Fatalf("%d requests after a create and its replay, want 1", got)
	}
	if got := len(w.auditRowsFor(w.member.User.ID, "request.submit")); got != 1 {
		t.Fatalf("%d request.submit audit rows after the replay, want 1 (a replay wrote none)", got)
	}

	// The same key with a DIFFERENT payload is the coded 409 and writes nothing.
	conflict := w.do(w.full, "POST", w.inbox(w.team), submitJSON(subject+" changed", "different"), key)
	w.wantCode(conflict, 409, authz.CodeRequestConflict)
	if got := w.requestCount(); got != 1 {
		t.Fatalf("%d requests after a key conflict, want 1", got)
	}

	// A different key is a different submission: a second request.
	if second := w.do(w.full, "POST", w.inbox(w.team), body, key+"-2"); second.status != 201 {
		t.Fatalf("a new key = %d %v, want a fresh 201", second.status, second.body)
	}
	if got := w.requestCount(); got != 2 {
		t.Fatalf("%d requests after a new key, want 2", got)
	}
}

// TestRESTSubmitRequest_NoProjectNamed: a REST submission reaches a team inbox
// with zero projects, and the route is the team's own path — no project id in
// the URL, none in the body, and the caller gains no project access by
// submitting. A payload that tries to name one is refused by name, not ignored.
func TestRESTSubmitRequest_NoProjectNamed(t *testing.T) {
	w := newRESTSubmitWorld(t)

	before, err := w.svc.Authz.VisibleProjects(w.ctx, authz.Principal{UserID: w.member.User.ID, Email: w.member.Email})
	if err != nil {
		t.Fatal(err)
	}
	res := w.do(w.full, "POST", w.inbox(w.team), submitJSON("projectless "+uniq("s"), "nothing to attach it to"), "")
	if res.status != 201 {
		t.Fatalf("projectless submit = %d %v, want 201", res.status, res.body)
	}
	after, err := w.svc.Authz.VisibleProjects(w.ctx, authz.Principal{UserID: w.member.User.ID, Email: w.member.Email})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("visible projects went from %d to %d: submitting granted access", len(before), len(after))
	}
	// No link row either: intake routes nothing (D9).
	if got := w.countRows(`SELECT count(*) FROM request_links`); got != 0 {
		t.Fatalf("%d request_links rows after intake, want 0", got)
	}

	// Every plan-shaped field is refused with the stable code, so a client that
	// believes it aimed a request at a project is told it did not.
	for _, field := range []string{"project_id", "story_id", "epic_id", "task_id"} {
		body := fmt.Sprintf(`{"subject":"aimed at a project","input_brief":"x",%q:1}`, field)
		res := w.do(w.full, "POST", w.inbox(w.team), body, "")
		w.wantCode(res, 400, authz.CodeRequestInvalid)
		if msg, _ := res.body["error"].(string); !strings.Contains(msg, field) {
			t.Fatalf("%s refusal %q does not name the field", field, msg)
		}
	}
}

// ---- criterion 2: coded refusals that write nothing ----

// TestRESTSubmitRequest_RefusalsAreCodedAndWriteNothing is the refusal matrix:
// a team the caller may not submit to (as a foreign-org customer, a revoked
// grant, an unknown team and a submit-only credential bound elsewhere), an
// oversized body, an oversized multibyte key, a spoofed identity field and a
// two-value body each answer with their own status and stable code, and none of
// them writes a request.
func TestRESTSubmitRequest_RefusalsAreCodedAndWriteNothing(t *testing.T) {
	w := newRESTSubmitWorld(t)
	inbox := w.inbox(w.team)
	body := submitJSON("a request that must be refused "+uniq("s"), "please")

	t.Run("foreign org customer gets not-found, never forbidden", func(t *testing.T) {
		// The other tenant's customer holds no grant here. TeamSubmit answers
		// not-found (the same bytes as a team that does not exist), so the
		// answer never confirms the team exists.
		res := w.do(w.tokenFor(t, w.other), "POST", inbox, body, "")
		if res.status != 404 {
			t.Fatalf("foreign customer = %d %v, want 404", res.status, res.body)
		}
		if got := w.requestCount(); got != 0 {
			t.Fatalf("%d requests after a foreign refusal, want 0", got)
		}
	})

	t.Run("another tenant's team is not found", func(t *testing.T) {
		res := w.do(w.tokenFor(t, w.admin), "POST", w.inbox(w.foreign), body, "")
		if res.status != 404 {
			t.Fatalf("cross-org team = %d %v, want 404", res.status, res.body)
		}
		if got := w.requestCount(); got != 0 {
			t.Fatalf("%d requests after a cross-org refusal, want 0", got)
		}
	})

	t.Run("unknown team is not found", func(t *testing.T) {
		res := w.do(w.full, "POST", "/api/teams/99999999/requests", body, "")
		if res.status != 404 {
			t.Fatalf("unknown team = %d %v, want 404", res.status, res.body)
		}
	})

	t.Run("revoked grant takes effect on the very next request", func(t *testing.T) {
		tok := w.tokenFor(t, w.customer)
		// In force: the granted customer may submit.
		if res := w.do(tok, "POST", inbox, submitJSON("granted "+uniq("s"), "please"), ""); res.status != 201 {
			t.Fatalf("granted customer = %d %v, want 201", res.status, res.body)
		}
		if _, err := w.repo.RevokeTeamSubmit(w.ctx, w.team, w.customer.User.ID, nil); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		res := w.do(tok, "POST", inbox, submitJSON("revoked "+uniq("s"), "please"), "")
		if res.status != 404 {
			t.Fatalf("revoked customer = %d %v, want the team surface's 404", res.status, res.body)
		}
		// Only the one authorized create is stored.
		if got := w.requestCount(); got != 1 {
			t.Fatalf("%d requests after grant + revoke, want the 1 authorized create", got)
		}
	})

	t.Run("oversized body is a coded 400 naming the bound, before any read", func(t *testing.T) {
		huge := `{"subject":"x","input_brief":"` + strings.Repeat("a", (256<<10)+1024) + `"}`
		res := w.do(w.full, "POST", inbox, huge, "")
		w.wantCode(res, 400, authz.CodeRequestInvalid)
		if msg, _ := res.body["error"].(string); !strings.Contains(msg, "over") {
			t.Fatalf("oversized refusal %q does not name the bound", msg)
		}
	})

	t.Run("oversized multibyte idempotency key spends nothing", func(t *testing.T) {
		// 130 x 'é' is 130 runes and 260 bytes: the key bound counts bytes
		// (#3918), so this must be refused by the service as request.invalid
		// rather than reaching the database's CHECK.
		res := w.do(w.full, "POST", inbox, body, strings.Repeat("é", 130))
		w.wantCode(res, 400, authz.CodeRequestInvalid)
		if msg, _ := res.body["error"].(string); !strings.Contains(msg, "idempotency_key") {
			t.Fatalf("key refusal %q does not name the field", msg)
		}
	})

	t.Run("spoofed identity and tenant fields are refused, not dropped", func(t *testing.T) {
		for _, field := range []string{"org_id", "actor", "requester_user_id", "requester_contact", "team_id", "source_kind"} {
			payload := fmt.Sprintf(`{"subject":"spoof","input_brief":"x",%q:"someone else"}`, field)
			res := w.do(w.full, "POST", inbox, payload, "")
			w.wantCode(res, 400, authz.CodeRequestInvalid)
			if msg, _ := res.body["error"].(string); !strings.Contains(msg, field) {
				t.Fatalf("%s refusal %q does not name the field", field, msg)
			}
		}
	})

	t.Run("a connector external id is refused by name", func(t *testing.T) {
		res := w.do(w.full, "POST", inbox,
			`{"subject":"connector id","input_brief":"x","source_external_id":"vendor-123"}`, "")
		w.wantCode(res, 400, authz.CodeRequestInvalid)
		if msg, _ := res.body["error"].(string); !strings.Contains(msg, "source_external_id") {
			t.Fatalf("connector refusal %q does not name the field", msg)
		}
	})

	t.Run("two JSON values in one body are refused", func(t *testing.T) {
		res := w.do(w.full, "POST", inbox, body+`{"subject":"a second one"}`, "")
		w.wantCode(res, 400, authz.CodeRequestInvalid)
	})

	t.Run("no principal is a 401, not a submission", func(t *testing.T) {
		if res := w.do("", "POST", inbox, body, ""); res.status != 401 {
			t.Fatalf("no token = %d %v, want 401", res.status, res.body)
		}
	})

	// Nothing in the matrix above wrote a request: the only create that
	// succeeded was the granted customer's, before the revocation.
	if got := w.requestCount(); got != 1 {
		t.Fatalf("%d requests after the refusal matrix, want 1", got)
	}
}

// TestRESTSubmitRequest_DenialIsAudited: a refused submission follows the same
// audit rule as every other 403 on this surface (denied() records
// access.denied), while a 404 — the team surface's answer — and a 400 do not,
// because neither is an access decision (KB decisions/audit-log-v2.md).
func TestRESTSubmitRequest_DenialIsAudited(t *testing.T) {
	w := newRESTSubmitWorld(t)
	body := submitJSON("audit me "+uniq("s"), "please")

	// A 403: a customer who is in the org (so TeamSubmit answers Forbidden, not
	// NotFound) but holds no grant and has no team membership.
	// w.other is an org member of the OTHER org; give this org a plain member
	// with no standing on the inbox.
	member2, err := w.repo.UpsertUserFromClaims(w.ctx, uniq("nobody"), uniq("nobody")+"@rest-submit.test", "nobody", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.repo.SetUserRole(w.ctx, member2.ID, domain.RoleMember, true); err != nil {
		t.Fatal(err)
	}
	if err := w.repo.SetOrgMember(w.ctx, w.org, member2.ID, domain.OrgRoleMember); err != nil {
		t.Fatal(err)
	}
	res := w.do(w.tokenForID(t, member2.ID, member2.Email), "POST", w.inbox(w.team), body, "")
	w.wantCode(res, 403, "") // a 403 need not carry a request.* code
	if got := len(w.auditRowsFor(member2.ID, "access.denied")); got != 1 {
		t.Fatalf("%d access.denied rows for the refused submission, want 1", got)
	}
	rows := w.auditRowsFor(member2.ID, "access.denied")
	if rows[0].Outcome != domain.AuditOutcomeDenied || rows[0].Reason == "" {
		t.Fatalf("access.denied row = %+v, want outcome denied and a reason", rows[0])
	}
	if rows[0].TargetType != "route" || !strings.Contains(rows[0].TargetID, "/requests") {
		t.Fatalf("access.denied target = %s/%s, want the submission route", rows[0].TargetType, rows[0].TargetID)
	}

	// A 404 (foreign team) is not an access event: recording it would file a
	// probe's enumeration attempts in the log.
	if res := w.do(w.tokenFor(t, w.admin), "POST", w.inbox(w.foreign), body, ""); res.status != 404 {
		t.Fatalf("cross-org team = %d, want 404", res.status)
	}
	if got := len(w.auditRowsFor(w.admin.User.ID, "access.denied")); got != 0 {
		t.Fatalf("%d access.denied rows for a 404, want 0", got)
	}

	// A malformed body is the client's problem, not an access decision.
	bad := w.do(w.full, "POST", w.inbox(w.team), `{"subject":`, "")
	w.wantCode(bad, 400, authz.CodeRequestInvalid)
	if got := len(w.auditRowsFor(w.member.User.ID, "access.denied")); got != 0 {
		t.Fatalf("%d access.denied rows for a 400, want 0", got)
	}

	// The successful submission is audited once by the service, not here.
	if res := w.do(w.full, "POST", w.inbox(w.team), body, ""); res.status != 201 {
		t.Fatalf("authorized submit = %d %v, want 201", res.status, res.body)
	}
	if got := len(w.auditRowsFor(w.member.User.ID, "request.submit")); got != 1 {
		t.Fatalf("%d request.submit rows, want 1", got)
	}
	if got := len(w.auditRowsFor(w.member.User.ID, "access.denied")); got != 0 {
		t.Fatalf("%d access.denied rows after a successful submit, want 0", got)
	}
}

// ---- criterion 3: the rate-limited answer carries Retry-After ----

// TestRESTSubmitRequest_RateLimitedCarriesRetryAfter drives the limiter to its
// exact ceiling and asserts the 429 is complete: the status, the Retry-After
// header, the stable request.rate_limited code, and retry_after_s in the body —
// the same number in all three places, from the service's own RetryAfterS. It
// also pins that a replay is free: a retry that resolves to an existing request
// is not refused for quota.
func TestRESTSubmitRequest_RateLimitedCarriesRetryAfter(t *testing.T) {
	w := newRESTSubmitWorldLimit(t, 2)
	inbox := w.inbox(w.team)

	// The window's two units: one create, and one keyed create kept for its
	// replay below.
	first := w.do(w.full, "POST", inbox, submitJSON("first "+uniq("s"), "please"), "")
	if first.status != 201 {
		t.Fatalf("first submission = %d %v, want 201", first.status, first.body)
	}
	key := "rest-quota-" + uniq("k")
	keyed := submitJSON("keyed "+uniq("s"), "please")
	if res := w.do(w.full, "POST", inbox, keyed, key); res.status != 201 {
		t.Fatalf("second submission = %d %v, want 201", res.status, res.body)
	}

	// Over the ceiling: the coded 429 with a wait.
	limited := w.do(w.full, "POST", inbox, submitJSON("third "+uniq("s"), "please"), "")
	w.wantCode(limited, 429, authz.CodeRequestRateLimited)
	ra := limited.header.Get("Retry-After")
	if ra == "" {
		t.Fatal("a 429 carried no Retry-After header")
	}
	raS, err := strconv.Atoi(ra)
	if err != nil || raS < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer number of seconds", ra)
	}
	bodyS, ok := limited.body["retry_after_s"].(float64)
	if !ok {
		t.Fatalf("429 body has no retry_after_s: %v", limited.body)
	}
	if int(bodyS) != raS {
		t.Fatalf("retry_after_s = %v but Retry-After = %q: the header and the body must be one number", bodyS, ra)
	}
	// Nothing was written by the refusal.
	if got := w.requestCount(); got != 2 {
		t.Fatalf("%d requests after the refusal, want the 2 before it", got)
	}

	// A replay of the keyed submission was charged its unit and never refunded
	// while the window lasted, so the retry arriving with the budget exhausted is
	// refused (request.rate_limited) rather than answered — the documented
	// consequence of charging the limiter before CreateRequest can recognise a
	// replay (#3900's boundary suite pins the same rule; the pre-Allow key probe
	// that would close it is a later slice's work).
	charged := w.do(w.full, "POST", inbox, keyed, key)
	w.wantCode(charged, 429, authz.CodeRequestRateLimited)

	// In the next window the replay is free and resolves to the request the key
	// already produced: nothing was written, and the unit it charged is refunded.
	w.now = w.now.Add(2 * time.Minute)
	replay := w.do(w.full, "POST", inbox, keyed, key)
	if replay.status != 200 {
		t.Fatalf("replay in a fresh window = %d %v, want 200", replay.status, replay.body)
	}
	if got := replay.body["created"]; got != false {
		t.Fatalf("replay created = %v, want false", got)
	}
	if got := w.requestCount(); got != 2 {
		t.Fatalf("%d requests after the replay, want the 2 before it", got)
	}

	// The same fresh window has budget left for a new submission: the replay's
	// unit was refunded, so only one of the window's two units is spent.
	if res := w.do(w.full, "POST", inbox, submitJSON("after replay "+uniq("s"), "please"), ""); res.status != 201 {
		t.Fatalf("submission after a replay = %d %v, want 201 (the replay must be free)", res.status, res.body)
	}
}

// ---- what a surface can get wrong that the service cannot ----

// TestRESTSubmitRequest_AnswerLeaksNothing: the answer is the requester
// whitelist, not the stored row. A request row carries its org, its triage
// claim, the assignee, its revision and a reject reason — the team's internal
// state a requester must never see (contract §2) — so the JSON must not contain
// any of those keys, and must contain the six fields it promises.
func TestRESTSubmitRequest_AnswerLeaksNothing(t *testing.T) {
	w := newRESTSubmitWorld(t)
	res := w.do(w.full, "POST", w.inbox(w.team), submitJSON("no leak "+uniq("s"), "please"), "")
	if res.status != 201 {
		t.Fatalf("submit = %d %v, want 201", res.status, res.body)
	}
	req, ok := res.body["request"].(map[string]any)
	if !ok {
		t.Fatalf("answer has no request object: %v", res.body)
	}
	for _, key := range []string{"public_ref", "state", "subject", "team_id", "source_kind", "created_at"} {
		if _, present := req[key]; !present {
			t.Errorf("answer is missing %q: %v", key, req)
		}
	}
	// Every internal field of domain.TeamRequest that a requester must not see.
	for _, key := range []string{"id", "org_id", "requester_user_id", "requester_contact",
		"source_external_id", "claim_user_id", "owner_user_id", "duplicate_of", "reject_reason",
		"rev", "reopened_count", "metadata", "outcome_brief", "needs_info_since"} {
		if _, present := req[key]; present {
			t.Errorf("the submission answer leaks %q: %v", key, req)
		}
	}
	// And the contact address is nowhere in the raw body, not just absent as a
	// key: a leak through a nested value is the same leak.
	if strings.Contains(res.rawBody, w.member.Email) {
		t.Fatalf("the answer echoes the requester's contact address: %s", res.rawBody)
	}
}

// TestRESTSubmitRequest_ScopeAndRouteBoundary: the submit-only credential
// reaches exactly this route, for its bound team, and nothing else — not GET on
// the same path (a read), not a request sub-path, not the other team's inbox.
// The full-scope token is unaffected. This is the scope gate's answer applied to
// a route that now exists.
func TestRESTSubmitRequest_ScopeAndRouteBoundary(t *testing.T) {
	w := newRESTSubmitWorld(t)

	// The bound team's submission is allowed and works.
	res := w.do(w.scoped, "POST", w.inbox(w.team), submitJSON("scoped "+uniq("s"), "please"), "")
	if res.status != 201 {
		t.Fatalf("scoped submit to its own team = %d %v, want 201", res.status, res.body)
	}
	if sk := requestFieldOf(t, res, "source_kind"); sk != string(domain.RequestSourceAPI) {
		t.Fatalf("source_kind = %q, want %q for an API token", sk, domain.RequestSourceAPI)
	}

	// The other team's inbox: the gate refuses the route shape outright (it is
	// not the credential's one operation).
	res = w.do(w.scoped, "POST", w.inbox(w.otherTeam), submitJSON("not mine", "please"), "")
	w.wantCode(res, 403, authz.CodeTokenScopeDenied)

	// A sub-path of the submission route is a different operation.
	for _, sub := range []string{"1", "1/comments", "1/claim"} {
		res := w.do(w.scoped, "POST", w.inbox(w.team)+"/"+sub, "", "")
		w.wantCode(res, 403, authz.CodeTokenScopeDenied)
	}

	// GET on the submission path is a read of the inbox, so the gate refuses it
	// even for the bound team (the list route is slice F2, not this ticket).
	res = w.do(w.scoped, "GET", w.inbox(w.team), "", "")
	w.wantCode(res, 403, authz.CodeTokenScopeDenied)

	// No route was widened: an ordinary read still refuses a scoped credential.
	res = w.do(w.scoped, "GET", "/api/me", "", "")
	w.wantCode(res, 403, authz.CodeTokenScopeDenied)

	// The same person's full-scope token is unaffected: it submits, and it can
	// still read what it could read before.
	if res := w.do(w.full, "POST", w.inbox(w.team), submitJSON("full "+uniq("s"), "please"), ""); res.status != 201 {
		t.Fatalf("full-scope token submit = %d %v, want 201", res.status, res.body)
	}
	if res := w.do(w.full, "GET", "/api/me", "", ""); res.status != 200 {
		t.Fatalf("full-scope token GET /api/me = %d %v, want 200", res.status, res.body)
	}
}

// ---- helpers ----

// tokenFor mints a full-scope API token for an identity in this world, through
// the service (the issuer is that same identity in the web UI, which is who may
// mint a token for themselves).
func (w *restSubmitWorld) tokenFor(t *testing.T, ident *auth.Identity) string {
	t.Helper()
	return w.tokenForID(t, ident.User.ID, ident.Email)
}

func (w *restSubmitWorld) tokenForID(t *testing.T, userID int, email string) string {
	t.Helper()
	ui := authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	tok, err := w.svc.CreateToken(w.ctx, ui, userID, "rest-"+uniq("t"), 0)
	if err != nil {
		t.Fatalf("mint a token for %s: %v", email, err)
	}
	return tok.Token
}

// countRows runs a count query with the given args.
func (w *restSubmitWorld) countRows(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx, sql, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// requestFieldOf reads request.<key> from a submission answer.
func requestFieldOf(t *testing.T, r restResult, key string) string {
	t.Helper()
	req, ok := r.body["request"].(map[string]any)
	if !ok {
		t.Fatalf("answer has no request object: %v", r.body)
	}
	v, _ := req[key].(string)
	return v
}
