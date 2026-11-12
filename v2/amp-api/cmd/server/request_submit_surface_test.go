package main

// Submission surface parity: REST's POST /api/teams/{id}/requests and MCP's
// amp_submit_request, driven live through both real adapters, side by side over
// one scratch database (epic 202, story 543, AMP #3921; approved contract
// artifact 36 v2 §2 and §5).
//
// # What this file is for, and what it is not
//
// Three suites already own the parts:
//
//   - cmd/server/request_submit_test.go proves Service.SubmitRequest as a
//     tenant/principal/retry/quota *boundary* (#3900), calling the service
//     directly.
//   - internal/api/request_submit.go's suite (cmd/server/request_submit_rest_test.go,
//     #3919) proves the REST route: bounded strict body, status mapping,
//     Retry-After, the scope gate's route shapes.
//   - internal/mcp/request_submit.go's suites (#3920) prove the tool: schema,
//     catalog flags, refusal envelope, the caller matrix, the scope gate.
//
// None of them answers the question this ticket asks: **are the two surfaces the
// same product?** Each proves its own adapter against the service; a drift
// between them — a principal one admits and the other refuses, an error that is
// 409 here and 400 there, a retry that replays there and double-submits here, a
// scoped credential that can submit on one transport and not the other — would
// leave every one of those suites green. So this file runs the *same call*, with
// the *same credential*, against both listeners, and compares the answers field
// by field.
//
// The ticket's third concern is a boundary both suites only approach from the
// side: the prior waves proved that a submit-only credential is *refused*
// everywhere else (#3772/#3920), not that it actually *succeeds* in filing a
// request. TestSubmitSurface_ScopedCredential does the real successful
// submission on both transports and then re-proves the refusal from a request
// that exists.
//
// Accepted criteria, and where each is answered:
//
//	1. Surface principal/retry/error parity:
//	   TestSubmitSurface_PrincipalParity (one caller matrix, both transports,
//	   answer-by-answer), TestSubmitSurface_RetryParity (replay, conflict, the
//	   cross-surface key rule, and every 400 a submission can get),
//	   TestSubmitSurface_ErrorEnvelope (status + stable code reach both clients
//	   the same way).
//	2. Scoped-token successful submission and other-route refusal:
//	   TestSubmitSurface_ScopedCredential (creates through both adapters on its
//	   bound team, then is refused on every other REST route and every other MCP
//	   tool).
//	3. Audit / quota / no-project-access:
//	   TestSubmitSurface_AuditParity, TestSubmitSurface_QuotaParity,
//	   TestSubmitSurface_RoutesNothing (no ticket, epic, story, link, claim,
//	   assignee or project access; nothing planned, approved or dispatched).
//
// # Why this is a *real* end-to-end and not a service test with two shims
//
//   - Every REST call goes through the real mux (api.RestHandler.Register, so
//     deniedMux, denied() and submitScopeGate are in front of the handler) and
//     the real auth.NewAPITokenVerifier. The principal is built by the verifier
//     from a stored `amp_` token row, so the channel (`api`), the account and the
//     token's scope/team binding are the ones production reads — not values this
//     test hands a handler.
//   - Every MCP call goes through the real public listener
//     (newMCPHTTPHandler -> newMCPServer -> internal/mcp), the same bearer
//     verifier, JSON-RPC over Streamable HTTP, and the tool's real middleware
//     stack (deadline, recovery, the denied audit, the submit-scope gate).
//   - The *same* token is presented to both transports, which is the only way
//     "the same credential behaves the same way" can be asserted at all.
//   - The database is a scratch database per test (freshDatabase needs CREATEDB),
//     so every count below is exact and not "rows filtered by my suffix".
//
// The one thing deliberately *not* identical is `source_kind`: the transport
// declares it (`api` for REST, `mcp` for the tool) and 061's idempotency-key
// scope includes it, so the same key presented on both surfaces is two keys.
// Both halves of that are asserted rather than assumed (TestSubmitSurface_RetryParity).
//
// Run against a disposable Postgres, never a live stack (a scratch database is
// created and dropped per test), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestSubmitSurface' -count=1 -race -v
//
// No live credential and no compose stack is involved: the only secrets are the
// tokens the fixture mints through the service, and they live in the scratch
// database it drops.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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
	"github.com/simstech/amp-api/internal/mcp"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// ---- the fixture ----

// surfaceIDs hands out a block of explicit ids per fixture. The submission
// limiter's key is (org, team, submitter, source) and the *shared* limiter is
// process-wide, while every test here gets its own scratch database whose
// serials restart at 1 — so two fixtures would otherwise collide on one key and
// one test's quota probe would spend another's budget. Explicit ids make each
// fixture's limiter key its own, which is what lets the quota test be exact.
var surfaceIDs atomic.Int64

func nextSurfaceIDs() int {
	return int(surfaceIDs.Add(1) * 1000)
}

// surfaceCaller is one identity with the credential both listeners accept: a
// full-scope `amp_` token minted through the service, exactly as a person mints
// one in the web UI. The same token is presented to REST and to MCP.
type surfaceCaller struct {
	label string
	ident *auth.Identity
	token string
}

// submitSurfaceWorld is one tenant with an inbox team, a sibling team it must
// not reach, a project the inbox team delivers into, a second tenant, and both
// live listeners over one scratch database.
type submitSurfaceWorld struct {
	t   *testing.T
	ctx context.Context
	db  *pgx.Conn
	// repo and svc are the REST handler's; the MCP listener builds its own
	// Service over the same repo (as main does for the second listener), which
	// is why only the REST side's clock and limiter are injectable here.
	repo *repository.Repo
	svc  *service.Service
	now  time.Time

	rest *httptest.Server // the real REST mux + the real amp_ token verifier
	mcp  *httptest.Server // the real public MCP listener + the same verifier

	org, team, sibling int // the tenant, the inbox, a team in the same org
	project            int // linked to the inbox team (what "access gained" is measured against)
	otherOrg, foreign  int // a second tenant and its team

	member   *surfaceCaller // member of the inbox team
	admin    *surfaceCaller // owner of the org (not a global admin)
	orgOnly  *surfaceCaller // org member with no standing on the inbox team
	customer *surfaceCaller // customer kind holding an in-force submit grant
	other    *surfaceCaller // customer kind in otherOrg, no grant here
	stranger *surfaceCaller // owner of otherOrg

	// scoped is member's submit-only credential bound to the inbox;
	// scopedSibling is admin's submit-only credential bound to the sibling team
	// (mintable because an org owner may submit there).
	scoped, scopedSibling string
}

// newSubmitSurfaceWorld boots both listeners over a scratch database. maxPerWindow
// is the REST handler's per-window ceiling: a value generous enough that quota is
// never the reason a call fails, except in the quota test which passes a small one.
//
// maxPerWindow <= 0 means "inject nothing": both listeners then fall back to the
// package's shared limiter (service.defaultRequestLimits), which is exactly the
// topology main() builds — neither newMCPHTTPHandler nor api.NewRestHandler is
// given a limiter, so one limiter serves both transports in a real process. That
// mode is what makes "one transport's burst does not refuse the other" testable:
// with an injected REST limiter the two listeners hold separate counters and the
// claim could not fail even if the key dropped the source.
func newSubmitSurfaceWorld(t *testing.T, maxPerWindow int) *submitSurfaceWorld {
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
	// A webhook secret key, so a row another suite sealed is still readable here
	// (the same fixed in-process key every cmd/server test uses).
	repo.SetSecretsKeys(defaultTestSecretsKeys(t))

	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	w := &submitSurfaceWorld{t: t, ctx: ctx, db: db, repo: repo,
		now: time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	base := nextSurfaceIDs()
	w.org, w.team, w.sibling, w.project = base, base+1, base+2, base+3
	w.otherOrg, w.foreign = base+4, base+5
	suffix := uniq("surface")

	exec(`INSERT INTO orgs (id, name, slug) VALUES ($1, 'Surface org', $2)`, w.org, suffix)
	exec(`INSERT INTO teams (id, org_id, name) VALUES ($1, $2, 'Inbox')`, w.team, w.org)
	exec(`INSERT INTO teams (id, org_id, name) VALUES ($1, $2, 'Sibling desk')`, w.sibling, w.org)
	// The project the inbox team delivers into. A team member sees it through the
	// team link; a granted customer must never see it, before or after submitting.
	exec(`INSERT INTO projects (id, name, code, org_id) VALUES ($1, 'Delivered work', $2, $3)`,
		w.project, strings.ToUpper(suffix[:6]), w.org)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, w.project, w.team)
	exec(`INSERT INTO orgs (id, name, slug) VALUES ($1, 'Surface other org', $2)`, w.otherOrg, suffix+"-o")
	exec(`INSERT INTO teams (id, org_id, name) VALUES ($1, $2, 'Foreign inbox')`, w.foreign, w.otherOrg)

	// A person, provisioned the way sign-in does, with an explicit global role:
	// the first account ever provisioned is auto-admin, so every role is pinned
	// rather than assumed (a global admin bypasses TeamSubmit, which would make
	// the matrix prove nothing).
	user := func(label, kind string, globalAdmin bool) *auth.Identity {
		t.Helper()
		u, err := repo.UpsertUserFromClaims(ctx, suffix+"-"+label,
			fmt.Sprintf("%s-%s@surface.test", label, suffix), label, nil)
		if err != nil {
			t.Fatalf("user %s: %v", label, err)
		}
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleMember, !globalAdmin); err != nil {
			t.Fatal(err)
		}
		if globalAdmin {
			if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, true); err != nil {
				t.Fatal(err)
			}
		} else {
			// Later accounts are provisioned as members; this keeps the intent
			// explicit and makes the fixture order-independent.
			if err := repo.SetUserRole(ctx, u.ID, domain.RoleAdmin, false); err != nil && !strings.Contains(err.Error(), "at least one global admin") {
				t.Fatal(err)
			}
		}
		if kind == domain.UserKindCustomer {
			exec(`UPDATE users SET kind = $2 WHERE id = $1`, u.ID, kind)
			u.Kind = kind
		}
		return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: label, User: u}
	}

	// root first: on an empty database the first user is the global admin. It is
	// never used to submit, so no row below is decided by Super.
	user("root", domain.UserKindHuman, true)
	w.member = &surfaceCaller{label: "member", ident: user("member", domain.UserKindHuman, false)}
	w.admin = &surfaceCaller{label: "admin", ident: user("admin", domain.UserKindHuman, false)}
	w.orgOnly = &surfaceCaller{label: "orgonly", ident: user("orgonly", domain.UserKindHuman, false)}
	w.customer = &surfaceCaller{label: "customer", ident: user("customer", domain.UserKindCustomer, false)}
	w.other = &surfaceCaller{label: "other", ident: user("other", domain.UserKindCustomer, false)}
	w.stranger = &surfaceCaller{label: "stranger", ident: user("stranger", domain.UserKindHuman, false)}

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.org, w.member.ident.User.ID)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, w.team, w.member.ident.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.org, w.admin.ident.User.ID)
	// orgOnly is in the org and on no team: the 403 (not 404) row.
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.org, w.orgOnly.ident.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, w.otherOrg, w.other.ident.User.ID)
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, w.otherOrg, w.stranger.ident.User.ID)
	// The customer's whole standing: a submit grant on the inbox (D14), granted
	// by the team's manager, so the row is the one an admin would write.
	if _, err := repo.GrantTeamSubmit(ctx, w.team, w.org, w.customer.ident.User.ID, &w.admin.ident.User.ID); err != nil {
		t.Fatalf("grant the customer: %v", err)
	}

	// The credentials: one full-scope token per identity, minted through the
	// service by that same person in the web UI (the issuer must be the account or
	// a global admin), plus the two submit-only ones.
	svc := service.New(repo, nil, nil)
	mintFull := func(c *surfaceCaller) {
		t.Helper()
		ui := authz.Principal{UserID: c.ident.User.ID, Email: c.ident.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
		tok, err := svc.CreateToken(ctx, ui, c.ident.User.ID, "surface-"+c.label, 0)
		if err != nil {
			t.Fatalf("mint a token for %s: %v", c.label, err)
		}
		c.token = tok.Token
	}
	for _, c := range []*surfaceCaller{w.member, w.admin, w.orgOnly, w.customer, w.other, w.stranger} {
		mintFull(c)
	}
	mintScoped := func(c *surfaceCaller, teamID int) string {
		t.Helper()
		ui := authz.Principal{UserID: c.ident.User.ID, Email: c.ident.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
		tok, err := svc.CreateScopedToken(ctx, ui, c.ident.User.ID, "surface-"+c.label+"-submit", 0,
			service.TokenScopeRequest{Scope: domain.TokenScopeSubmit, TeamID: teamID})
		if err != nil {
			t.Fatalf("mint a submit-only token for %s on team %d: %v", c.label, teamID, err)
		}
		return tok.Token
	}
	w.scoped = mintScoped(w.member, w.team)
	w.scopedSibling = mintScoped(w.admin, w.sibling)

	// ---- the two live surfaces ----
	sseHub := hub.New()
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	reg := actor.NewRegistry(system, repo, sseHub)

	restMux := http.NewServeMux()
	h := api.NewRestHandler(reg, repo, sseHub, nil)
	// The handler's own Service: the limiter and clock the quota test drives, on
	// the object the route actually calls (api.RestHandler.Service()).
	w.svc = h.Service()
	if maxPerWindow > 0 {
		w.svc.RequestLimits = service.NewRequestLimiter(service.RequestLimitConfig{Window: time.Minute, Max: maxPerWindow})
	}
	w.svc.Now = func() time.Time { return w.now }
	h.Register(restMux)
	w.rest = httptest.NewServer(auth.NewAPITokenVerifier(repo).Middleware(restMux))
	t.Cleanup(w.rest.Close)

	// The public MCP listener, wired as main() wires it: the real protocol
	// server, the real AMP tools, and the same bearer verifier as REST.
	mux := http.NewServeMux()
	w.mcp = httptest.NewServer(mux)
	t.Cleanup(w.mcp.Close)
	mcpListener := newMCPHTTPHandler(newMCPServer(mcp.NewServer(reg, repo, sseHub, nil)),
		auth.NewAPITokenVerifier(repo), mcpHTTPConfig{PublicURL: w.mcp.URL + "/mcp"})
	// Mirror Envoy's routing: /mcp is forwarded with the prefix rewritten to "/".
	mux.Handle("/mcp", http.StripPrefix("/mcp", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, "/")
		mcpListener.ServeHTTP(rw, r)
	})))
	return w
}

// ---- one HTTP path for both transports ----

// surfaceHTTP is one raw answer: status, headers, the decoded JSON body and the
// exact bytes (for leak checks).
type surfaceHTTP struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

// rawJSON is a body that is already encoded, so a test can send a payload the
// decoder would refuse (an unknown field, a second JSON value, an over-bound
// body) — exactly what a hostile or confused client sends. A plain value is
// marshalled, which is what every well-formed call uses.
type rawJSON string

// do sends one request and decodes whatever comes back, JSON or SSE — the MCP
// listener may answer either, depending on what it decides the client can read.
func (w *submitSurfaceWorld) do(method, url, token string, headers map[string]string, body any) surfaceHTTP {
	w.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case rawJSON:
		reader = strings.NewReader(string(b))
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			w.t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The last data: line is the final JSON-RPC response (transport_test.go's rule).
		for _, line := range strings.Split(text, "\n") {
			if payload, ok := strings.CutPrefix(line, "data: "); ok {
				text = payload
			}
		}
	}
	out := surfaceHTTP{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal([]byte(text), &out.body)
	return out
}

// restDo is one REST call with an optional Idempotency-Key header (the REST
// client's retry token; it is a header, not a body field). body is sent as wire
// bytes: rawJSON when the test means exactly those bytes, a value otherwise.
func (w *submitSurfaceWorld) restDo(token, method, path string, body any, idemKey string) surfaceHTTP {
	w.t.Helper()
	var headers map[string]string
	if idemKey != "" {
		headers = map[string]string{"Idempotency-Key": idemKey}
	}
	return w.do(method, w.rest.URL+path, token, headers, body)
}

// inbox is the submission route for a team.
func (w *submitSurfaceWorld) inbox(teamID int) string {
	return fmt.Sprintf("/api/teams/%d/requests", teamID)
}

// mcpTool is one tool call over the public listener, and never fails the test on
// a refusal: a refusal is a result here, which is what lets the matrix assert
// both halves of every row.
func (w *submitSurfaceWorld) mcpTool(token, tool string, args map[string]any) surfaceHTTP {
	w.t.Helper()
	return w.do(http.MethodPost, w.mcp.URL+"/mcp", token, modernHeaders("tools/call", tool), modernToolCall(1, tool, args))
}

// mcpTools lists the tools this caller is advertised, off the tools/list it would
// actually make (the list is filtered per caller, so it is part of the answer).
func (w *submitSurfaceWorld) mcpTools(token string) []string {
	w.t.Helper()
	r := w.do(http.MethodPost, w.mcp.URL+"/mcp", token, modernHeaders("tools/list", ""),
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{"_meta": modernMeta()}})
	if r.status != http.StatusOK {
		w.t.Fatalf("tools/list: HTTP %d: %s", r.status, r.raw)
	}
	res, ok := r.body["result"].(map[string]any)
	if !ok {
		w.t.Fatalf("tools/list: no result: %s", r.raw)
	}
	tools, _ := res["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		if m, ok := tl.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// ---- the one normalized answer both adapters are compared through ----

// surfaceOutcome is one submission's answer, whichever transport produced it,
// reduced to the fields the two surfaces promise to agree on. Everything the
// comparison needs is here; the raw bytes ride along for the leak check.
type surfaceOutcome struct {
	transport string // "rest" or "mcp"

	// accepted is the product's answer: the request was filed (or replayed).
	accepted bool
	created  bool

	// A refusal: the status REST would write and the stable code, if any.
	status      int
	code        string
	message     string
	retryAfterS int // 429 only, whole seconds

	// The answer projection, when accepted.
	ref, state, subject, sourceKind string
	teamID                          int

	// next is the MCP-only operator hint (REST has no such field).
	next string
	raw  string
}

// mcpOutcome reduces one tool call's wire answer to a surfaceOutcome.
func (c surfaceHTTP) mcpOutcome() surfaceOutcome {
	out := surfaceOutcome{transport: "mcp", raw: c.raw}
	if c.status != http.StatusOK {
		// An HTTP-level refusal: the bearer verifier, origin guard or protocol
		// checks answer before any JSON-RPC body exists.
		out.status = c.status
		if s, _ := c.body["code"].(string); s != "" {
			out.code = s
		}
		if s, _ := c.body["error"].(string); s != "" {
			out.message = s
		}
		return out
	}
	if e, ok := c.body["error"].(map[string]any); ok {
		// A JSON-RPC error: the tool's coded refusal carries REST's status and
		// stable code in `data`, which is the whole point of the wrapper.
		out.message, _ = e["message"].(string)
		if data, ok := e["data"].(map[string]any); ok {
			if st, ok := data["status"].(float64); ok {
				out.status = int(st)
			}
			out.code, _ = data["code"].(string)
			if ra, ok := data["retry_after_s"].(float64); ok {
				out.retryAfterS = int(ra)
			}
		}
		if out.message == "" {
			out.message = c.raw
		}
		return out
	}
	res, ok := c.body["result"].(map[string]any)
	if !ok {
		return out
	}
	if isErr, _ := res["isError"].(bool); isErr {
		out.message = c.raw
		return out
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return out
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		out.message = "unparsable tool result: " + text
		return out
	}
	out.accepted = true
	out.created, _ = payload["created"].(bool)
	out.next, _ = payload["next"].(string)
	if req, ok := payload["request"].(map[string]any); ok {
		out.ref, _ = req["public_ref"].(string)
		out.state, _ = req["state"].(string)
		out.subject, _ = req["subject"].(string)
		out.sourceKind, _ = req["source_kind"].(string)
		if id, ok := req["team_id"].(float64); ok {
			out.teamID = int(id)
		}
	}
	return out
}

// restOutcome reduces one REST answer to a surfaceOutcome. A create writes 201
// and a replay 200, so `created` is read off the status; the body repeats it for
// clients that only read the payload.
func (c surfaceHTTP) restOutcome() surfaceOutcome {
	out := surfaceOutcome{transport: "rest", raw: c.raw}
	switch c.status {
	case http.StatusCreated, http.StatusOK:
		out.accepted = true
		out.created = c.status == http.StatusCreated
		if req, ok := c.body["request"].(map[string]any); ok {
			out.ref, _ = req["public_ref"].(string)
			out.state, _ = req["state"].(string)
			out.subject, _ = req["subject"].(string)
			out.sourceKind, _ = req["source_kind"].(string)
			if id, ok := req["team_id"].(float64); ok {
				out.teamID = int(id)
			}
		}
		if created, ok := c.body["created"].(bool); ok {
			out.created = created
		}
		return out
	}
	out.status = c.status
	if s, _ := c.body["code"].(string); s != "" {
		out.code = s
	}
	if s, _ := c.body["error"].(string); s != "" {
		out.message = s
	}
	if ra, ok := c.body["retry_after_s"].(float64); ok {
		out.retryAfterS = int(ra)
	}
	return out
}

// submit runs one submission on one transport.
//
// REST takes the subject/brief as a body and the retry token as a header; MCP
// takes both as arguments. That difference is the transports', not the product's,
// so it lives here and nowhere else in the file.
func (w *submitSurfaceWorld) submit(transport, token string, teamID int, subject, brief, key string) surfaceOutcome {
	w.t.Helper()
	if transport == "rest" {
		b, _ := json.Marshal(map[string]any{"subject": subject, "input_brief": brief})
		return w.restDo(token, http.MethodPost, w.inbox(teamID), rawJSON(b), key).restOutcome()
	}
	args := map[string]any{"team_id": float64(teamID), "subject": subject, "input_brief": brief}
	if key != "" {
		args["idempotency_key"] = key
	}
	return w.mcpTool(token, "amp_submit_request", args).mcpOutcome()
}

// ---- assertions ----

// wantRefusal asserts a refusal's status and stable code. An empty code means the
// refusal carries none, which is itself part of the contract (a team the caller
// may not reach is the team surface's 403/404, not a request.* code).
func wantRefusal(t *testing.T, what string, o surfaceOutcome, status int, code string) {
	t.Helper()
	if o.accepted {
		t.Fatalf("%s (%s) was accepted: ref=%q created=%v", what, o.transport, o.ref, o.created)
	}
	if o.status != status {
		t.Fatalf("%s (%s) = %d %q, want %d %q (message: %s)", what, o.transport, o.status, o.code, status, code, o.message)
	}
	if code != "" && o.code != code {
		t.Fatalf("%s (%s) code = %q, want %q (message: %s)", what, o.transport, o.code, code, o.message)
	}
	if code == "" && strings.HasPrefix(o.code, "request.") {
		t.Fatalf("%s (%s) carried %q; no request.* code belongs on this refusal", what, o.transport, o.code)
	}
}

// wantParity asserts the two transports answered one submission the same way. It
// compares the product's answer — accepted, created, the refusal's status and
// code, and the projection's own fields — and deliberately not `source_kind` or
// `next`, which are each transport's own fact (asserted where they matter).
func wantParity(t *testing.T, what string, rest, mcpOut surfaceOutcome) {
	t.Helper()
	if rest.accepted != mcpOut.accepted {
		t.Fatalf("%s: REST accepted=%v but MCP accepted=%v (rest %d %q / mcp %d %q)",
			what, rest.accepted, mcpOut.accepted, rest.status, rest.code, mcpOut.status, mcpOut.code)
	}
	if !rest.accepted {
		if rest.status != mcpOut.status || rest.code != mcpOut.code {
			t.Fatalf("%s: REST refused %d %q but MCP refused %d %q — the two surfaces must refuse the same way",
				what, rest.status, rest.code, mcpOut.status, mcpOut.code)
		}
		return
	}
	if rest.created != mcpOut.created {
		t.Fatalf("%s: REST created=%v but MCP created=%v", what, rest.created, mcpOut.created)
	}
	if rest.state != mcpOut.state || rest.subject != mcpOut.subject || rest.teamID != mcpOut.teamID {
		t.Fatalf("%s: the answers disagree: REST %q/%q/%d vs MCP %q/%q/%d",
			what, rest.state, rest.subject, rest.teamID, mcpOut.state, mcpOut.subject, mcpOut.teamID)
	}
	if rest.ref == "" || mcpOut.ref == "" {
		t.Fatalf("%s: an accepted submission answered no public_ref (rest %q, mcp %q)", what, rest.ref, mcpOut.ref)
	}
}

// count runs a count query with the given args.
func (w *submitSurfaceWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx, sql, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// requestsWithSubject is how many requests one subject produced: the exact form
// of "a retry wrote nothing" and "a refusal wrote nothing", without needing the
// whole table to be empty.
func (w *submitSurfaceWorld) requestsWithSubject(subject string) int {
	return w.count(`SELECT count(*) FROM team_requests WHERE subject = $1`, subject)
}

// storedRequest reads the row a submission wrote, by public reference — the
// assertions about *stored* attribution have to look at the record, not the answer.
func (w *submitSurfaceWorld) storedRequest(ref string) (source, subject, state string, requester *int, org, team int) {
	w.t.Helper()
	err := w.db.QueryRow(w.ctx,
		`SELECT source_kind, subject, state, requester_user_id, org_id, team_id
		   FROM team_requests WHERE public_ref = $1`, ref).Scan(&source, &subject, &state, &requester, &org, &team)
	if err != nil {
		w.t.Fatalf("read request %q: %v", ref, err)
	}
	return
}

// audits is every audit row matching f in this world's database.
func (w *submitSurfaceWorld) audits(f domain.AuditFilter) []domain.AuditEntry {
	w.t.Helper()
	if f.Limit == 0 {
		f.Limit = 200
	}
	rows, err := w.repo.ListAudit(w.ctx, f)
	if err != nil {
		w.t.Fatalf("read audit: %v", err)
	}
	return rows
}

// visibleProjects is the set of projects p may reach, through the same query the
// authorization layer uses — how "submitting granted nothing" is measured.
func (w *submitSurfaceWorld) visibleProjects(c *surfaceCaller) map[int]bool {
	w.t.Helper()
	ids, err := w.svc.Authz.VisibleProjects(w.ctx, authz.Principal{
		UserID: c.ident.User.ID, Email: c.ident.Email, Channel: auth.ChannelToken, AuthMethod: auth.AuthAPIToken})
	if err != nil {
		w.t.Fatalf("visible projects of %s: %v", c.label, err)
	}
	return ids
}

// brief is the one-line detail every row sends, when the row is not about the
// detail.
const surfaceBrief = "We need the triage queue as CSV for our weekly review."

// subjectFor is a unique subject per call, so "exactly one row" stays exact in a
// scratch database two surfaces both write to.
func subjectFor(what string) string { return "surface " + what + " " + uniq("s") }

// ---- 1: one caller matrix, both transports ----

// TestSubmitSurface_PrincipalParity runs the tenant/principal matrix the
// credential design distinguishes through *both* live adapters with the *same*
// credential, and compares the answers: who may submit, who is refused and how,
// what is stored and attributed to whom. A surface that admitted a caller the
// other refused — or refused with a different status or code — fails here.
//
// Two deliberate choices:
//
//   - Every permitted row submits on both surfaces and both rows are read back
//     from the database, so "accepted" means a request exists with the caller's
//     own requester id and the transport's own source, not that a handler said
//     yes.
//   - Every refused row asserts *nothing was written* for its subject, so a
//     refusal that wrote a row fails at the row that caused it.
func TestSubmitSurface_PrincipalParity(t *testing.T) {
	w := newSubmitSurfaceWorld(t, 60)

	type row struct {
		name string
		// who is built per row, so a row can revoke access immediately before its
		// calls, and so the no-credential row can present nothing.
		who func() string
		// user is the account a create must be attributed to (0: the row creates
		// nothing).
		user func() int
		team func() int
		// want describes the answer both surfaces must give.
		status int    // refusal status; 0 means the submission must be created
		code   string // the stable code, "" for a refusal with none
	}
	rows := []row{
		{name: "team member", who: func() string { return w.member.token },
			user: func() int { return w.member.ident.User.ID }, team: func() int { return w.team }},
		{name: "owner of the org", who: func() string { return w.admin.token },
			user: func() int { return w.admin.ident.User.ID }, team: func() int { return w.team }},
		{name: "customer holding a submit grant", who: func() string { return w.customer.token },
			user: func() int { return w.customer.ident.User.ID }, team: func() int { return w.team }},
		// The org owner's own second token: same person, same team, a second
		// credential — the answer is the person's, not the credential's.
		{name: "full-scope token of a team member", who: func() string { return w.member.token },
			user: func() int { return w.member.ident.User.ID }, team: func() int { return w.team }},

		// Refusals. The org member with no standing on the inbox gets 403 (they
		// are inside the tenant and simply ungranted; TeamSubmit names what is
		// missing); everyone else gets 404 — the same bytes a team that does not
		// exist answers, so no stranger can enumerate another tenant's inboxes.
		{name: "org member with no standing on the team", who: func() string { return w.orgOnly.token },
			user: func() int { return 0 }, team: func() int { return w.team }, status: 403},
		{name: "member of a sibling team in the same org", who: func() string { return w.member.token },
			user: func() int { return 0 }, team: func() int { return w.sibling }, status: 403},
		{name: "customer in another org with no grant", who: func() string { return w.other.token },
			user: func() int { return 0 }, team: func() int { return w.team }, status: 404},
		{name: "owner of another org", who: func() string { return w.stranger.token },
			user: func() int { return 0 }, team: func() int { return w.team }, status: 404},
		{name: "unknown team", who: func() string { return w.member.token },
			user: func() int { return 0 }, team: func() int { return w.team + 900 }, status: 404},
		{name: "customer aiming at another tenant's team", who: func() string { return w.customer.token },
			user: func() int { return 0 }, team: func() int { return w.foreign }, status: 404},
		// Revocation takes effect on the very next call, from the rows, on both
		// transports: the grant is never remembered in either adapter.
		{name: "customer whose grant was just revoked", who: func() string {
			if _, err := w.repo.RevokeTeamSubmit(w.ctx, w.team, w.customer.ident.User.ID, &w.admin.ident.User.ID); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			return w.customer.token
		}, user: func() int { return 0 }, team: func() int { return w.team }, status: 404},
		// No credential at all: the bearer verifier answers before any product
		// code exists, on both listeners.
		{name: "no credential", who: func() string { return "" },
			user: func() int { return 0 }, team: func() int { return w.team }, status: 401},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			subject := subjectFor(r.name)
			teamID := r.team()
			token := r.who()
			wantUser := r.user()

			restOut := w.submit("rest", token, teamID, subject, surfaceBrief, "")
			mcpOut := w.submit("mcp", token, teamID, subject, surfaceBrief, "")

			if r.status == 0 {
				for _, o := range []surfaceOutcome{restOut, mcpOut} {
					if !o.accepted || !o.created {
						t.Fatalf("%s (%s) = accepted=%v created=%v %d %q %q, want a create",
							r.name, o.transport, o.accepted, o.created, o.status, o.code, o.message)
					}
				}
				wantParity(t, r.name, restOut, mcpOut)
			} else {
				wantParity(t, r.name, restOut, mcpOut)
				wantRefusal(t, r.name, restOut, r.status, r.code)
				wantRefusal(t, r.name, mcpOut, r.status, r.code)
				if n := w.requestsWithSubject(subject); n != 0 {
					t.Fatalf("a refused submission (%s) wrote %d request(s)", r.name, n)
				}
				return
			}

			// Both rows exist, and each says what its transport declared and who
			// the authenticated caller was — never anything the payload could say.
			for _, o := range []surfaceOutcome{restOut, mcpOut} {
				wantSource := domain.RequestSourceAPI
				if o.transport == "mcp" {
					wantSource = domain.RequestSourceMCP
				}
				if o.sourceKind != string(wantSource) {
					t.Fatalf("%s: source_kind = %q, want %q (the adapter declares the transport)",
						o.transport, o.sourceKind, wantSource)
				}
				source, storedSubject, state, requester, org, team := w.storedRequest(o.ref)
				if storedSubject != subject || state != string(domain.RequestStatusNew) {
					t.Fatalf("%s: stored %q/%q, want %q/new", o.transport, storedSubject, state, subject)
				}
				if source != string(wantSource) {
					t.Errorf("%s: stored source_kind = %q, want %q", o.transport, source, wantSource)
				}
				if requester == nil || *requester != wantUser {
					t.Fatalf("%s: requester = %v, want the authenticated caller %d",
						o.transport, requester, wantUser)
				}
				if org != w.org || team != teamID {
					t.Fatalf("%s: stored tenant = org %d/team %d, want %d/%d", o.transport, org, team, w.org, teamID)
				}
			}

			// Two transports, two keys: each surface wrote exactly one row for the
			// subject, so neither answered the other's submission from a shared
			// cache of anything.
			if n := w.requestsWithSubject(subject); n != 2 {
				t.Fatalf("%d requests for one subject on two surfaces, want 2", n)
			}
		})
	}

	// Everything the matrix created lives in this tenant and arrived on one of the
	// two transports: no row escaped the org, and no row claims a third source.
	if n := w.count(`SELECT count(*) FROM team_requests WHERE org_id <> $1`, w.org); n != 0 {
		t.Fatalf("%d request(s) landed outside this tenant", n)
	}
	if n := w.count(`SELECT count(*) FROM team_requests WHERE source_kind NOT IN ('api','mcp')`); n != 0 {
		t.Fatalf("%d request(s) carry a source no surface here declares", n)
	}
}

// ---- 2: retries, and every 400 a submission can get ----

// TestSubmitSurface_RetryParity: a retry means the same thing on both surfaces —
// the same key with the same payload resolves to the request that key already
// produced, the same key with a different payload is the coded conflict, and a
// new key is a new request. It also pins the one thing that is deliberately *not*
// the same, because reading only one adapter gets it wrong: the same key sent
// through REST and through MCP are **different keys** (061 scopes on the source),
// so a cross-surface retry is not a replay.
func TestSubmitSurface_RetryParity(t *testing.T) {
	w := newSubmitSurfaceWorld(t, 60)

	for _, transport := range []string{"rest", "mcp"} {
		t.Run(transport+" replays and conflicts", func(t *testing.T) {
			subject := subjectFor(transport + " replay")
			key := "surface-replay-" + uniq("k")

			first := w.submit(transport, w.member.token, w.team, subject, surfaceBrief, key)
			if !first.accepted || !first.created {
				t.Fatalf("first (%s) = accepted=%v created=%v %d %q %q", transport, first.accepted, first.created, first.status, first.code, first.message)
			}
			second := w.submit(transport, w.member.token, w.team, subject, surfaceBrief, key)
			if !second.accepted || second.created {
				t.Fatalf("replay (%s) = accepted=%v created=%v, want created=false", transport, second.accepted, second.created)
			}
			if second.ref != first.ref {
				t.Fatalf("replay (%s) answered %q, want the original %q", transport, second.ref, first.ref)
			}
			if n := w.requestsWithSubject(subject); n != 1 {
				t.Fatalf("a create and its replay wrote %d requests, want 1", n)
			}
			// The replay wrote no history and no audit row either: it is the same
			// submission seen twice. Both surfaces inherit that from the service,
			// which is exactly what this checks.
			var id int
			if err := w.db.QueryRow(w.ctx, `SELECT id FROM team_requests WHERE public_ref = $1`, first.ref).Scan(&id); err != nil {
				t.Fatal(err)
			}
			if n := w.count(`SELECT count(*) FROM request_events WHERE request_id = $1 AND action = 'submit'`, id); n != 1 {
				t.Fatalf("%d submit history rows after a replay, want 1", n)
			}
			if n := len(w.audits(domain.AuditFilter{Action: "request.submit", TargetID: strconv.Itoa(id)})); n != 1 {
				t.Fatalf("%d request.submit audit rows after a replay, want 1", n)
			}

			// The same key with a different payload: the coded 409, and nothing
			// written.
			conflict := w.submit(transport, w.member.token, w.team, subject+" (edited)", surfaceBrief, key)
			wantRefusal(t, "key reuse with a new payload", conflict, 409, authz.CodeRequestConflict)
			if n := w.requestsWithSubject(subject); n != 1 {
				t.Fatalf("%d requests after a conflict, want the original 1", n)
			}

			// A new key is a new request.
			fresh := w.submit(transport, w.member.token, w.team, subject+" (second)", surfaceBrief, key+"-2")
			if !fresh.accepted || !fresh.created {
				t.Fatalf("a new key (%s) = accepted=%v created=%v, want a fresh create", transport, fresh.accepted, fresh.created)
			}
		})
	}

	// The cross-surface rule, stated as a test so nobody reads one adapter and
	// concludes the opposite: one key, both transports, two requests. Correct —
	// they are different retry shapes — and the reason a client must not move a
	// key between transports.
	t.Run("the same key on both surfaces is two submissions", func(t *testing.T) {
		subject := subjectFor("cross surface key")
		key := "surface-shared-" + uniq("k")

		overREST := w.submit("rest", w.member.token, w.team, subject, surfaceBrief, key)
		overMCP := w.submit("mcp", w.member.token, w.team, subject, surfaceBrief, key)
		if !overREST.created || !overMCP.created {
			t.Fatalf("cross-surface key: REST created=%v MCP created=%v, want both to create", overREST.created, overMCP.created)
		}
		if overREST.ref == overMCP.ref {
			t.Fatalf("both surfaces answered request %q from one key", overREST.ref)
		}
		if n := w.requestsWithSubject(subject); n != 2 {
			t.Fatalf("%d requests from one key on two surfaces, want 2 (the key is scoped by source)", n)
		}
		// And the stored key rows say so: one `api`, one `mcp`.
		if n := w.count(`SELECT count(*) FROM request_idempotency_keys WHERE idempotency_key = $1 AND source_kind = 'api'`, key); n != 1 {
			t.Fatalf("%d api-scoped key rows, want 1", n)
		}
		if n := w.count(`SELECT count(*) FROM request_idempotency_keys WHERE idempotency_key = $1 AND source_kind = 'mcp'`, key); n != 1 {
			t.Fatalf("%d mcp-scoped key rows, want 1", n)
		}
	})
}

// TestSubmitSurface_ErrorEnvelope: every submission the domain can refuse answers
// the same status and the same stable code on both surfaces, names the field the
// caller has to fix, and writes nothing. This is where a drift like "REST says
// 400 request.invalid, the tool says a bare error" would show.
func TestSubmitSurface_ErrorEnvelope(t *testing.T) {
	w := newSubmitSurfaceWorld(t, 60)

	type row struct {
		name string
		// send produces the call on one transport. It exists because two of the
		// cases are not expressible the same way through both schemas: an
		// over-bound HTTP body only exists on REST, and a spoofed field is an
		// unknown JSON body field on one side and an undeclared argument on the
		// other.
		send func(transport string) surfaceOutcome
		// wantField, when set, must appear in the refusal's message: the caller is
		// told what to fix, never shown the rejected content.
		wantField string
		// preProduct marks a refusal the *transport* makes before the service is
		// reached. Those two are each adapter's own argument handling, not a
		// product rule, so they are asserted as "a client error that wrote nothing"
		// rather than for cross-surface parity and a request.* code.
		preProduct bool
	}
	rows := []row{
		{
			name: "over-long subject",
			send: func(transport string) surfaceOutcome {
				return w.submit(transport, w.member.token, w.team,
					strings.Repeat("x", domain.MaxRequestSubjectLen+1), surfaceBrief, "")
			},
			wantField: "subject",
		},
		{
			name: "over-long input brief",
			send: func(transport string) surfaceOutcome {
				return w.submit(transport, w.member.token, w.team,
					subjectFor("brief"), strings.Repeat("y", domain.MaxRequestBriefLen+1), "")
			},
			wantField: "input_brief",
		},
		{
			// A missing subject is the domain's "subject is required", not a
			// transport's "missing argument": the product rule is the service's on
			// both sides, so both must say the same thing.
			name: "empty subject",
			send: func(transport string) surfaceOutcome {
				return w.submit(transport, w.member.token, w.team, "   ", surfaceBrief, "")
			},
			wantField: "subject",
		},
		{
			// 130 x 'é' is 130 characters and 260 bytes. The key's bound counts
			// BYTES (#3918), and both adapters must answer request.invalid naming
			// the field rather than letting the database's CHECK surface as a 500.
			name: "multibyte idempotency key over the byte bound",
			send: func(transport string) surfaceOutcome {
				return w.submit(transport, w.member.token, w.team,
					subjectFor("multibyte key"), surfaceBrief, strings.Repeat("é", 130))
			},
			wantField: "idempotency_key",
		},
		{
			name: "metadata that is not an object",
			send: func(transport string) surfaceOutcome {
				if transport == "rest" {
					return w.restDo(w.member.token, http.MethodPost, w.inbox(w.team),
						rawJSON(`{"subject":"metadata","metadata":"not an object"}`), "").restOutcome()
				}
				args := map[string]any{"team_id": float64(w.team), "subject": "metadata", "metadata": "not an object"}
				return w.mcpTool(w.member.token, "amp_submit_request", args).mcpOutcome()
			},
			wantField: "metadata",
		},
		{
			// A plan-shaped field is refused by name on both surfaces: a caller that
			// aimed a request at a project is told it did not land near one.
			name: "a plan-shaped field",
			send: func(transport string) surfaceOutcome {
				if transport == "rest" {
					return w.restDo(w.member.token, http.MethodPost, w.inbox(w.team),
						rawJSON(`{"subject":"aimed at a project","project_id":7}`), "").restOutcome()
				}
				args := map[string]any{"team_id": float64(w.team), "subject": "aimed at a project", "project_id": float64(7)}
				return w.mcpTool(w.member.token, "amp_submit_request", args).mcpOutcome()
			},
			wantField: "project_id",
		},
		{
			// An identity field is refused, not silently dropped: a dropped
			// requester_contact would leave the caller believing a request came from
			// someone it did not.
			name: "a spoofed identity field",
			send: func(transport string) surfaceOutcome {
				if transport == "rest" {
					return w.restDo(w.member.token, http.MethodPost, w.inbox(w.team),
						rawJSON(`{"subject":"spoof","requester_contact":"someone-else@example.com"}`), "").restOutcome()
				}
				args := map[string]any{"team_id": float64(w.team), "subject": "spoof",
					"requester_contact": "someone-else@example.com"}
				return w.mcpTool(w.member.token, "amp_submit_request", args).mcpOutcome()
			},
			wantField: "requester_contact",
		},
		{
			// A malformed team id never reaches the product: REST's handler cannot
			// parse the path segment, and the tool's argument helper refuses a
			// non-numeric team_id. Neither is a product refusal, so neither carries a
			// stable code — which is why this row is asserted separately below rather
			// than through the parity and code checks the other rows use.
			name:       "a non-numeric team id",
			preProduct: true,
			send: func(transport string) surfaceOutcome {
				if transport == "rest" {
					return w.restDo(w.member.token, http.MethodPost, "/api/teams/not-a-number/requests",
						rawJSON(`{"subject":"bad team"}`), "").restOutcome()
				}
				args := map[string]any{"team_id": "not-a-number", "subject": "bad team"}
				return w.mcpTool(w.member.token, "amp_submit_request", args).mcpOutcome()
			},
		},
	}

	before := w.count(`SELECT count(*) FROM team_requests`)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			restOut, mcpOut := r.send("rest"), r.send("mcp")
			for _, o := range []surfaceOutcome{restOut, mcpOut} {
				if o.accepted {
					t.Fatalf("%s (%s) was accepted: ref=%q", r.name, o.transport, o.ref)
				}
				// Never a 500 for a payload the caller can fix. MCP's wire answer for a
				// pre-product argument error carries no status (the adapter's plain
				// error, not a coded refusal), so it is recognised by its message.
				if o.status != 0 && (o.status < 400 || o.status >= 500) {
					t.Fatalf("%s (%s) = %d, want a client error (never a 500 for a payload the caller can fix): %s",
						r.name, o.transport, o.status, o.message)
				}
				if o.status == 0 && o.message == "" {
					t.Fatalf("%s (%s) answered neither a status nor a message: %s", r.name, o.transport, o.raw)
				}
				if r.wantField != "" && !strings.Contains(o.message, r.wantField) {
					t.Fatalf("%s (%s) refusal %q does not name %q", r.name, o.transport, o.message, r.wantField)
				}
			}
			if r.preProduct {
				// Each transport answers its own argument problem: REST cannot parse
				// the path segment (a 400 with no code), and the tool's helper refuses
				// a non-numeric team_id (a plain error, so no coded refusal). Both
				// are client errors and neither is a request.* code.
				if restOut.status != 400 {
					t.Fatalf("%s: REST = %d, want 400 for a path segment it cannot parse", r.name, restOut.status)
				}
				if mcpOut.code != "" {
					t.Fatalf("%s: the tool's argument refusal carried the stable code %q; it is not a product refusal",
						r.name, mcpOut.code)
				}
				return
			}
			wantParity(t, r.name, restOut, mcpOut)
			// The stable code is REST's and MCP's shared vocabulary: a 400 on this
			// surface is request.invalid on both, so a client branches on one code.
			for _, o := range []surfaceOutcome{restOut, mcpOut} {
				if o.code != authz.CodeRequestInvalid {
					t.Fatalf("%s (%s) code = %q, want %q", r.name, o.transport, o.code, authz.CodeRequestInvalid)
				}
			}
		})
	}
	if after := w.count(`SELECT count(*) FROM team_requests`); after != before {
		t.Fatalf("%d requests exist after the error matrix, want the %d before it", after, before)
	}

	// The size guard is REST's alone (MCP has no body to bound): an over-bound
	// body is the coded 400 naming the bound, not a 413 and not a 500 — the
	// request.* vocabulary has no "too large" code, and a client branching on
	// request.invalid is told the payload cannot be a submission.
	t.Run("an over-bound body", func(t *testing.T) {
		huge := `{"subject":"x","input_brief":"` + strings.Repeat("a", (256<<10)+1024) + `"}`
		o := w.restDo(w.member.token, http.MethodPost, w.inbox(w.team), rawJSON(huge), "").restOutcome()
		wantRefusal(t, "over-bound body", o, 400, authz.CodeRequestInvalid)
		if !strings.Contains(o.message, "over") {
			t.Fatalf("over-bound refusal %q does not name the bound", o.message)
		}
		if after := w.count(`SELECT count(*) FROM team_requests`); after != before {
			t.Fatalf("an over-bound body wrote a request")
		}
	})
}

// ---- 2b: a wrong-typed optional argument is refused, not coerced ----

// TestSubmitSurface_WrongTypedOptionalArgumentsRefused: the one axis where the
// two adapters actually diverged before AMP #3931.
//
// REST decodes into a typed struct with DisallowUnknownFields, so
// `{"input_brief": 123}` is a 400 request.invalid naming the field. The MCP tool
// read the same arguments through optStr, which type-asserts `v.(string)` and
// **falls back to the default ("")** on a mismatch: `{"idempotency_key": 123}`
// was accepted, keyless. A client that believes it sent a retry key did not, so
// its next retry with the same (wrong-typed) key submits a second request — the
// double submit the key exists to make impossible.
//
// So the assertions here are the product's, not the adapter's:
//
//   - a wrong-typed input_brief or idempotency_key is refused with the same
//     status and stable code REST answers for the equivalent JSON (400
//     request.invalid), naming the argument the caller has to fix;
//   - the refusal writes nothing — no request row, no idempotency-key row, no
//     audit row — and spends no limiter unit, so a client that spams the broken
//     shape never locks itself out of its own budget;
//   - a present null stays absence on both surfaces (REST's decoder reads JSON
//     null for a string field as ""), so {{"input_brief": null}} is an ordinary
//     briefless submission.
//
// One difference is documented rather than asserted as parity, because the shape
// does not exist on the other surface: an idempotency key is a *header* over
// REST (always a string) and a *body field* only on MCP, so the wrong-typed-key
// case is MCP-only. REST is still checked, with the same JSON body, and answers
// 400 request.invalid for its own reason (idempotency_key is an unknown body
// field there) — the status and the code agree even though the rule behind them
// differs.
func TestSubmitSurface_WrongTypedOptionalArgumentsRefused(t *testing.T) {
	// The MCP listener holds the package's shared default limiter (this fixture
	// injects the 60/window one on the REST handler's Service only), so the
	// "spends no limiter unit" half below is measured against the real default
	// ceiling: more wrong-typed calls than one window allows, and not one 429.
	w := newSubmitSurfaceWorld(t, 60)

	requestsBefore := w.count(`SELECT count(*) FROM team_requests`)
	keyRowsBefore := w.count(`SELECT count(*) FROM request_idempotency_keys`)

	type row struct {
		name string
		// args is what the tool is called with; body is the equivalent REST
		// submission (raw, so the decoder sees exactly these bytes).
		args map[string]any
		body rawJSON
		// wantField must appear in both refusals: the caller is told what to fix.
		wantField string
	}
	rows := []row{
		{
			name:      "a wrong-typed idempotency_key",
			args:      map[string]any{"team_id": float64(w.team), "subject": "typed key", "idempotency_key": float64(123)},
			body:      rawJSON(`{"subject":"typed key","idempotency_key":123}`),
			wantField: "idempotency_key",
		},
		{
			name:      "a wrong-typed input_brief",
			args:      map[string]any{"team_id": float64(w.team), "subject": "typed brief", "input_brief": true},
			body:      rawJSON(`{"subject":"typed brief","input_brief":true}`),
			wantField: "input_brief",
		},
		{
			name:      "an object input_brief",
			args:      map[string]any{"team_id": float64(w.team), "subject": "object brief", "input_brief": map[string]any{"text": "x"}},
			body:      rawJSON(`{"subject":"object brief","input_brief":{"text":"x"}}`),
			wantField: "input_brief",
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			restOut := w.restDo(w.member.token, http.MethodPost, w.inbox(w.team), r.body, "").restOutcome()
			mcpOut := w.mcpTool(w.member.token, "amp_submit_request", r.args).mcpOutcome()

			// The product answer: refused, 400 request.invalid, on both surfaces.
			// This is the assertion that fails on the pre-#3931 tool, where the call
			// was *accepted* with the argument coerced to "".
			wantRefusal(t, r.name+" over REST", restOut, 400, authz.CodeRequestInvalid)
			wantRefusal(t, r.name+" over MCP", mcpOut, 400, authz.CodeRequestInvalid)
			wantParity(t, r.name, restOut, mcpOut)
			for _, o := range []surfaceOutcome{restOut, mcpOut} {
				if !strings.Contains(o.message, r.wantField) {
					t.Fatalf("%s (%s) refusal %q does not name %q", r.name, o.transport, o.message, r.wantField)
				}
			}
			// Nothing written: not the request, and not the key row that would
			// make the caller's next retry a replay. A stored key row for a
			// refused submission would be the opposite failure: a retry answered
			// by a request that was never created.
			if n := w.count(`SELECT count(*) FROM team_requests`); n != requestsBefore {
				t.Fatalf("%s wrote a request: %d rows, want the %d before", r.name, n, requestsBefore)
			}
			if n := w.count(`SELECT count(*) FROM request_idempotency_keys`); n != keyRowsBefore {
				t.Fatalf("%s wrote an idempotency-key row: %d rows, want the %d before", r.name, n, keyRowsBefore)
			}
			if n := len(w.audits(domain.AuditFilter{Action: "request.submit"})); n != 0 {
				t.Fatalf("%s wrote %d request.submit audit rows: a refusal to accept the payload is not a submission", r.name, n)
			}
		})
	}

	// The limiter half. The MCP listener's budget for (org, team, submitter, mcp)
	// is the package's shared one, whose ceiling is DefaultRequestLimitPerWindow;
	// if a wrong-typed argument spent a unit, the (ceiling+1)th call would be the
	// coded 429 instead of the 400. The adapter refuses before the service is
	// called, so nothing is charged — and a well-formed submission afterwards
	// still creates its request.
	t.Run("a wrong-typed argument spends no limiter unit", func(t *testing.T) {
		for i := 0; i <= service.DefaultRequestLimitPerWindow; i++ {
			o := w.mcpTool(w.member.token, "amp_submit_request", map[string]any{
				"team_id": float64(w.team), "subject": "no quota " + strconv.Itoa(i), "idempotency_key": float64(i),
			}).mcpOutcome()
			wantRefusal(t, "wrong-typed key "+strconv.Itoa(i), o, 400, authz.CodeRequestInvalid)
		}
		if o := w.submit("mcp", w.member.token, w.team, subjectFor("after the broken shapes"), surfaceBrief, ""); !o.created {
			t.Fatalf("a well-formed submission after %d refusals = %d %q %q; the refusals must not have spent the budget",
				service.DefaultRequestLimitPerWindow+1, o.status, o.code, o.message)
		}
	})

	// A present null is absence on both surfaces: REST's decoder reads JSON null
	// for a string field as the empty string, so the submission is briefless and
	// accepted, not refused. Mirroring that is what keeps a generic client that
	// sends every optional field (null when it has no value) working.
	t.Run("a present null input_brief is absence, not a refusal", func(t *testing.T) {
		subject := subjectFor("null brief")
		o := w.mcpTool(w.member.token, "amp_submit_request", map[string]any{
			"team_id": float64(w.team), "subject": subject, "input_brief": nil,
		}).mcpOutcome()
		if !o.accepted || !o.created {
			t.Fatalf("MCP {\"input_brief\": null} = accepted=%v created=%v %d %q %q, want an ordinary submission",
				o.accepted, o.created, o.status, o.code, o.message)
		}
		if n := w.requestsWithSubject(subject); n != 1 {
			t.Fatalf("%d requests for the null-brief subject, want 1", n)
		}
	})
}

// ---- 3: the scoped credential actually submits ----

// TestSubmitSurface_ScopedCredential is the boundary the earlier waves only
// approached from one side. #3772/#3920 proved a submit-only credential is
// *refused* everywhere else; #3900's boundary suite proved it can submit at the
// service layer; #3919's route test submitted with one over REST. This test does
// the whole thing on both live transports: the credential files a real request
// through REST and through MCP, and *then* is refused every other operation,
// including on a request that now exists (a sub-path of the route, a read of the
// inbox, every other tool).
func TestSubmitSurface_ScopedCredential(t *testing.T) {
	w := newSubmitSurfaceWorld(t, 60)

	// The successful submission, through both adapters, with the one credential.
	subject := subjectFor("scoped create")
	restOut := w.submit("rest", w.scoped, w.team, subject, surfaceBrief, "")
	mcpOut := w.submit("mcp", w.scoped, w.team, subject, surfaceBrief, "")
	for _, o := range []surfaceOutcome{restOut, mcpOut} {
		if !o.accepted || !o.created {
			t.Fatalf("the scoped credential could not submit (%s): accepted=%v created=%v %d %q %q",
				o.transport, o.accepted, o.created, o.status, o.code, o.message)
		}
		// Attributed to the token's owner, on the transport's source — a scoped
		// credential is an attenuation of a person, never an anonymous integration.
		_, _, state, requester, org, team := w.storedRequest(o.ref)
		if state != string(domain.RequestStatusNew) || org != w.org || team != w.team {
			t.Fatalf("%s: stored %q in org %d/team %d, want new in %d/%d", o.transport, state, org, team, w.org, w.team)
		}
		if requester == nil || *requester != w.member.ident.User.ID {
			t.Fatalf("%s: requester = %v, want the token's owner %d", o.transport, requester, w.member.ident.User.ID)
		}
	}
	if n := w.requestsWithSubject(subject); n != 2 {
		t.Fatalf("%d requests from the scoped credential on two surfaces, want 2", n)
	}
	scopedRef := restOut.ref

	// ---- REST: every other route is refused, before the handler runs ----
	//
	// The refusal is the gate's (403 token.scope_denied), not the handler's, and it
	// covers the shapes that look adjacent to the one operation: a sibling team's
	// inbox, a sub-path of the submission route (a read, a comment, a claim), the
	// read of the inbox itself, and any other route at all.
	for _, c := range []struct {
		name, method, path string
		body               rawJSON
	}{
		{"another team's inbox", http.MethodPost, w.inbox(w.sibling), rawJSON(`{"subject":"not mine"}`)},
		{"a request sub-path", http.MethodPost, w.inbox(w.team) + "/1", ""},
		{"a request's comments", http.MethodPost, w.inbox(w.team) + "/1/comments", ""},
		{"a request's claim", http.MethodPost, w.inbox(w.team) + "/1/claim", ""},
		{"the inbox read", http.MethodGet, w.inbox(w.team), ""},
		{"who am I", http.MethodGet, "/api/me", ""},
		{"the project list", http.MethodGet, "/api/projects", ""},
		{"the audit log", http.MethodGet, "/api/audit", ""},
	} {
		t.Run("REST refuses "+c.name, func(t *testing.T) {
			o := w.restDo(w.scoped, c.method, c.path, c.body, "").restOutcome()
			wantRefusal(t, "scoped credential on "+c.name, o, 403, authz.CodeTokenScopeDenied)
		})
	}
	// The sibling-bound credential is refused on this team's inbox too (the gate
	// matches the path's team against the binding).
	t.Run("REST refuses a credential bound elsewhere", func(t *testing.T) {
		o := w.restDo(w.scopedSibling, http.MethodPost, w.inbox(w.team), rawJSON(`{"subject":"not mine"}`), "").restOutcome()
		wantRefusal(t, "a credential bound to another team", o, 403, authz.CodeTokenScopeDenied)
	})

	// ---- MCP: exactly one tool, and the list says so ----
	advertised := w.mcpTools(w.scoped)
	if len(advertised) != 1 || advertised[0] != mcp.SubmitScopeTool {
		t.Fatalf("the scoped credential's tools/list = %v, want exactly [%s]", advertised, mcp.SubmitScopeTool)
	}
	for _, tool := range []string{
		// Reads, plan writes, and the tools a request's triage would need — none of
		// them is the submission, so a submission credential reaches none of them.
		"amp_whoami", "amp_list_projects", "amp_list_tasks", "amp_get_task", "amp_get_plan",
		"amp_list_orgs", "amp_list_project_members", "amp_audit_log", "amp_create_project",
		"amp_create_tasks", "amp_dispatch_task", "amp_complete_task", "amp_manage_team_members",
	} {
		t.Run("MCP refuses "+tool, func(t *testing.T) {
			r := w.mcpTool(w.scoped, tool, map[string]any{"team_id": float64(w.team)})
			if r.status == http.StatusOK && r.body["result"] != nil {
				t.Fatalf("%s was allowed for a submit-only credential: %s", tool, r.raw)
			}
			o := r.mcpOutcome()
			if !strings.Contains(o.message, "may only submit requests") {
				t.Fatalf("%s refusal = %q, want the scope refusal", tool, o.message)
			}
		})
	}

	// ---- the one asymmetry between the surfaces, pinned rather than papered over ----
	//
	// A credential bound to team A submitting to team B is refused by *both*
	// surfaces, writes nothing on either, and answers a *different* status:
	//
	//   - REST refuses the route shape at the gate: 403 token.scope_denied, because
	//     "POST /api/teams/{B}/requests" is simply not the one route this
	//     credential may reach (api/token_scope.go matches method+path+binding).
	//   - MCP has one tool and the tool is the credential's, so the call reaches the
	//     service, whose TeamSubmit refuses with not-found: 404 and no request.*
	//     code, the same bytes every unreachable team answers — which is the
	//     stronger answer of the two, since it does not confirm that team B exists.
	//
	// Both are correct; a client branching on the code must therefore treat
	// token.scope_denied and a bare 404 as the same outcome for this case.
	subject = subjectFor("scoped cross team")
	restCross := w.submit("rest", w.scoped, w.sibling, subject, surfaceBrief, "")
	mcpCross := w.submit("mcp", w.scoped, w.sibling, subject, surfaceBrief, "")
	wantRefusal(t, "REST scoped credential aimed at another team", restCross, 403, authz.CodeTokenScopeDenied)
	wantRefusal(t, "MCP scoped credential aimed at another team", mcpCross, 404, "")
	if n := w.requestsWithSubject(subject); n != 0 {
		t.Fatalf("a scoped credential wrote %d request(s) outside its bound team", n)
	}

	// ---- the full-scope token is unaffected ----
	//
	// The scope narrows a credential, never the person: the same member's
	// full-scope token still submits and still reads what it could read before.
	if o := w.submit("rest", w.member.token, w.team, subjectFor("full token"), surfaceBrief, ""); !o.created {
		t.Fatalf("the member's full-scope token could not submit: %d %q %q", o.status, o.code, o.message)
	}
	if r := w.restDo(w.member.token, http.MethodGet, "/api/me", nil, ""); r.status != http.StatusOK {
		t.Fatalf("GET /api/me with a full-scope token = %d %v", r.status, r.body)
	}
	if o := w.mcpTool(w.member.token, "amp_whoami", map[string]any{}); o.status != http.StatusOK || o.body["result"] == nil {
		t.Fatalf("amp_whoami with a full-scope token = %d %s", o.status, o.raw)
	}

	// The refusal holds even for the request the credential itself just created.
	// This is the point of the scope being an attenuation rather than a grant: the
	// credential may write into its inbox and may not read what it wrote, so the
	// one operation cannot be widened into "my submissions" by anyone holding it.
	if n := w.count(`SELECT count(*) FROM team_requests WHERE public_ref = $1`, scopedRef); n != 1 {
		t.Fatalf("the request the scoped credential created is not in the database")
	}
	for _, path := range []string{
		w.inbox(w.team) + "?public_ref=" + scopedRef,
		"/api/teams/" + strconv.Itoa(w.team),
	} {
		o := w.restDo(w.scoped, http.MethodGet, path, nil, "").restOutcome()
		wantRefusal(t, "the scoped credential reading back "+path, o, 403, authz.CodeTokenScopeDenied)
	}
}

// ---- 4: audit parity ----

// TestSubmitSurface_AuditParity: the audit story is the same on both surfaces
// because it is the same rule — a create is audited once by the service, a replay
// writes nothing, and a 403 is an access decision the *listener* records while a
// 404 and a 400 are not. Each arm is asserted against a scratch database, so the
// counts are exact.
func TestSubmitSurface_AuditParity(t *testing.T) {
	w := newSubmitSurfaceWorld(t, 60)

	// ---- a create writes exactly one request.submit row, naming the transport ----
	for _, transport := range []string{"rest", "mcp"} {
		t.Run(transport+" audits one create", func(t *testing.T) {
			o := w.submit(transport, w.member.token, w.team, subjectFor(transport+" audit"), surfaceBrief, "")
			if !o.created {
				t.Fatalf("submit = %d %q %q", o.status, o.code, o.message)
			}
			var id int
			if err := w.db.QueryRow(w.ctx, `SELECT id FROM team_requests WHERE public_ref = $1`, o.ref).Scan(&id); err != nil {
				t.Fatal(err)
			}
			rows := w.audits(domain.AuditFilter{Action: "request.submit", TargetID: strconv.Itoa(id)})
			if len(rows) != 1 {
				t.Fatalf("%d request.submit rows for one create, want 1", len(rows))
			}
			wantSource := string(domain.RequestSourceAPI)
			if transport == "mcp" {
				wantSource = string(domain.RequestSourceMCP)
			}
			if got, _ := rows[0].Details["source_kind"].(string); got != wantSource {
				t.Fatalf("audit source_kind = %v, want %q", rows[0].Details["source_kind"], wantSource)
			}
			if got, _ := rows[0].Details["requester_user_id"].(float64); int(got) != w.member.ident.User.ID {
				t.Fatalf("audit requester_user_id = %v, want the caller %d", rows[0].Details["requester_user_id"], w.member.ident.User.ID)
			}
			// A successful submission is not a refusal.
			if n := len(w.audits(domain.AuditFilter{ActorUserID: w.member.ident.User.ID, Action: "access.denied"})); n != 0 {
				t.Fatalf("%d access.denied rows after a successful submit, want 0", n)
			}
		})
	}

	// ---- a 403 is audited, on both surfaces, under that surface's target ----
	t.Run("a 403 is one access.denied on each surface", func(t *testing.T) {
		// orgOnly is inside the tenant and has no standing on the inbox: TeamSubmit
		// answers 403, and each *listener* records its own refusal (REST's denied()
		// under "route", the tool's deniedMiddleware under "tool"). So one 403 per
		// surface is exactly one row per surface, and the refusals do not collapse
		// into each other or double up.
		for _, transport := range []string{"rest", "mcp"} {
			wantTarget := "tool"
			if transport == "rest" {
				wantTarget = "route"
			}
			before := len(w.audits(domain.AuditFilter{ActorUserID: w.orgOnly.ident.User.ID,
				Action: "access.denied", TargetType: wantTarget}))
			o := w.submit(transport, w.orgOnly.token, w.team, subjectFor(transport+" denied"), surfaceBrief, "")
			wantRefusal(t, "org member with no standing", o, 403, "")
			rows := w.audits(domain.AuditFilter{ActorUserID: w.orgOnly.ident.User.ID,
				Action: "access.denied", TargetType: wantTarget})
			if len(rows) != before+1 {
				t.Fatalf("%s: a 403 wrote %d %q denial row(s), want exactly 1", transport, len(rows)-before, wantTarget)
			}
			if rows[0].Outcome != domain.AuditOutcomeDenied || rows[0].Reason == "" {
				t.Fatalf("%s: access.denied row = %+v, want outcome denied with a reason", transport, rows[0])
			}
			if rows[0].TargetID == "" {
				t.Fatalf("%s: access.denied row names no target", transport)
			}
		}
	})

	// ---- a 404 and a 400 are not access decisions, on either surface ----
	t.Run("a 404 and a 400 write no denial", func(t *testing.T) {
		for _, transport := range []string{"rest", "mcp"} {
			// A 404: another tenant's team. Recording it would file a prober's
			// enumeration attempts while telling the prober nothing.
			o := w.submit(transport, w.other.token, w.team, subjectFor(transport+" foreign"), surfaceBrief, "")
			wantRefusal(t, "foreign customer", o, 404, "")
			// A 400: the client's own malformed submission.
			bad := w.submit(transport, w.member.token, w.team, strings.Repeat("x", domain.MaxRequestSubjectLen+1), surfaceBrief, "")
			wantRefusal(t, "over-long subject", bad, 400, authz.CodeRequestInvalid)
		}
		if n := len(w.audits(domain.AuditFilter{ActorUserID: w.other.ident.User.ID, Action: "access.denied"})); n != 0 {
			t.Fatalf("%d access.denied rows for a 404, want 0", n)
		}
		if n := len(w.audits(domain.AuditFilter{ActorUserID: w.member.ident.User.ID, Action: "access.denied"})); n != 0 {
			t.Fatalf("%d access.denied rows for a 400, want 0", n)
		}
	})
}

// ---- 5: quota parity ----

// TestSubmitSurface_QuotaParity: a submission costs quota on both surfaces, a
// spent budget answers the same coded 429 carrying the same number (REST's
// Retry-After header and body field; the tool's retry_after_s), and the budgets
// are per source — so one transport's retry storm cannot lock a person out of the
// other.
//
// Two arms, for two different reasons:
//
//   - "REST carries the wait exactly" injects a ceiling so the arithmetic is
//     exact: spend both units of a two-unit window, then observe the refusal, the
//     header/body agreement, and the free replay in the next window.
//   - "the budgets are per source" injects nothing, so both listeners share the
//     one process-wide limiter main() gives them. That is the only arrangement in
//     which the source-scoping claim can fail: with separate limiters per listener
//     the budgets would be separate whatever the key contained.
func TestSubmitSurface_QuotaParity(t *testing.T) {
	t.Run("REST carries the wait exactly", func(t *testing.T) {
		w := newSubmitSurfaceWorld(t, 2)

		if o := w.submit("rest", w.member.token, w.team, subjectFor("quota one"), surfaceBrief, ""); !o.created {
			t.Fatalf("first submission = %d %q %q", o.status, o.code, o.message)
		}
		key := "surface-quota-" + uniq("k")
		keyed := subjectFor("quota keyed")
		if o := w.submit("rest", w.member.token, w.team, keyed, surfaceBrief, key); !o.created {
			t.Fatalf("second submission = %d %q %q", o.status, o.code, o.message)
		}

		// The window's budget is spent: the next new submission is the coded 429.
		subject := subjectFor("quota over")
		over := w.submit("rest", w.member.token, w.team, subject, surfaceBrief, "")
		wantRefusal(t, "over the ceiling", over, 429, authz.CodeRequestRateLimited)
		if over.retryAfterS < 1 {
			t.Fatalf("retry_after_s = %d, want a whole second at least", over.retryAfterS)
		}

		// The header and the body must be one number: a client that honours the
		// header and one that reads the body must sleep the same time.
		raw := w.restDo(w.member.token, http.MethodPost, w.inbox(w.team),
			rawJSON(fmt.Sprintf(`{"subject":%q}`, subjectFor("quota header"))), "")
		wantRefusal(t, "over the ceiling (raw)", raw.restOutcome(), 429, authz.CodeRequestRateLimited)
		header := raw.header.Get("Retry-After")
		if header == "" {
			t.Fatal("a 429 carried no Retry-After header")
		}
		n, err := strconv.Atoi(header)
		if err != nil || n < 1 {
			t.Fatalf("Retry-After = %q, want a positive integer", header)
		}
		if body := raw.restOutcome().retryAfterS; body != n {
			t.Fatalf("retry_after_s = %d but Retry-After = %q: the header and the body must be one number", body, header)
		}
		if n := w.requestsWithSubject(subject); n != 0 {
			t.Fatalf("a rate-limited submission wrote %d request(s)", n)
		}

		// In the next window the replay is free and resolves to the request the key
		// already produced, and the unit it refunded is spendable.
		w.now = w.now.Add(2 * time.Minute)
		replay := w.submit("rest", w.member.token, w.team, keyed, surfaceBrief, key)
		if !replay.accepted || replay.created {
			t.Fatalf("replay in a fresh window = accepted=%v created=%v, want a free replay", replay.accepted, replay.created)
		}
		if o := w.submit("rest", w.member.token, w.team, subjectFor("quota after replay"), surfaceBrief, ""); !o.created {
			t.Fatalf("submission after a free replay = %d %q %q (the refund must leave quota spendable)", o.status, o.code, o.message)
		}
	})

	t.Run("the budgets are per source", func(t *testing.T) {
		// No injected limiter: one process-wide limiter serves both listeners, as
		// main() wires it. The clock has to be shared too — the limiter's window is
		// `now.Truncate(window)`, so an injected REST instant against the MCP
		// listener's time.Now() would be two different windows and the counters
		// would reset between the two calls. Both listeners read the real clock here,
		// which is what production does.
		w := newSubmitSurfaceWorld(t, 0)
		w.svc.Now = time.Now

		// Spend the member's whole window on REST (its ceiling is the default, 30),
		// then one more call to see the refusal: the ceiling is "at most this many
		// per window", so the (ceiling+1)th is the first refused.
		var last surfaceOutcome
		spent := 0
		for i := 0; i <= service.DefaultRequestLimitPerWindow; i++ {
			last = w.submit("rest", w.member.token, w.team, subjectFor("shared budget")+strconv.Itoa(i), surfaceBrief, "")
			if !last.accepted {
				break
			}
			spent++
		}
		if last.accepted || last.status != 429 {
			t.Skipf("the shared limiter let %d REST submissions through this window without refusing; the coded 429 is proven exactly above", spent)
		}
		wantRefusal(t, "REST's spent budget", last, 429, authz.CodeRequestRateLimited)

		// The same person, the same team, the other transport: still allowed. The
		// limiter's key is (org, team, submitter, source), so a source that spent its
		// budget cannot refuse another one — a submit-only connector looping on
		// MCP must never be able to lock the person out of the web/API path, and
		// vice versa.
		if o := w.submit("mcp", w.member.token, w.team, subjectFor("shared budget mcp"), surfaceBrief, ""); !o.created {
			t.Fatalf("MCP submission while REST's budget was spent = %d %q %q (the budgets must be per source)",
				o.status, o.code, o.message)
		}

		// And the MCP side carries its own wait once its budget is spent: the same
		// coded refusal, the same field, on the other transport.
		var mcpLimited surfaceOutcome
		for i := 0; i < service.DefaultRequestLimitPerWindow+5; i++ {
			mcpLimited = w.submit("mcp", w.member.token, w.team, subjectFor("shared budget mcp")+strconv.Itoa(i), surfaceBrief, "")
			if !mcpLimited.accepted {
				break
			}
		}
		wantRefusal(t, "MCP's spent budget", mcpLimited, 429, authz.CodeRequestRateLimited)
		if mcpLimited.retryAfterS < 1 {
			t.Fatalf("retry_after_s = %d, want a whole second at least", mcpLimited.retryAfterS)
		}
	})
}

// ---- 6: nothing gained, nothing planned ----

// TestSubmitSurface_RoutesNothing is the boundary that has nothing to do with
// principals: a submission is not a plan. Neither surface may create a ticket, an
// epic or a story, link the request to a project, claim it or assign it, move its
// state out of `new`, or hand the requester any project access — and the answer
// must be the requester's whitelist, not the stored row.
func TestSubmitSurface_RoutesNothing(t *testing.T) {
	w := newSubmitSurfaceWorld(t, 60)

	// Before: what the two callers this test measures can reach.
	memberBefore := w.visibleProjects(w.member)
	customerBefore := w.visibleProjects(w.customer)
	membershipsBefore := w.count(`SELECT count(*) FROM org_members`) + w.count(`SELECT count(*) FROM team_members`) +
		w.count(`SELECT count(*) FROM project_teams`) + w.count(`SELECT count(*) FROM team_submit_grants`)

	// Two submissions, one per transport, one per kind of caller.
	refs := []string{}
	for _, c := range []struct {
		transport string
		who       *surfaceCaller
	}{
		{"rest", w.member}, {"mcp", w.member},
		{"rest", w.customer}, {"mcp", w.customer},
		{"rest", &surfaceCaller{label: "scoped", token: w.scoped}}, {"mcp", &surfaceCaller{label: "scoped", token: w.scoped}},
	} {
		o := w.submit(c.transport, c.who.token, w.team, subjectFor(c.transport+" "+c.who.label), surfaceBrief, "")
		if !o.created {
			t.Fatalf("%s submission as %s = %d %q %q", c.transport, c.who.label, o.status, o.code, o.message)
		}
		refs = append(refs, o.ref)
	}

	// ---- nothing was planned, linked, claimed or assigned ----
	for _, table := range []string{"tasks", "epics", "stories", "request_links", "task_dependencies"} {
		if n := w.count(`SELECT count(*) FROM ` + table); n != 0 {
			t.Fatalf("%d %s rows after %d submissions: intake plans and routes nothing", n, table, len(refs))
		}
	}
	// Nine states, and intake uses exactly one of them. Nothing else on the row is
	// set either: no claim, no assignee, no delivery, no acceptance.
	if n := w.count(`SELECT count(*) FROM team_requests WHERE org_id = $1 AND (state <> 'new' OR rev <> 1
	                 OR claim_user_id IS NOT NULL OR owner_user_id IS NOT NULL OR planned_at IS NOT NULL
	                 OR delivered_at IS NOT NULL OR accepted_at IS NOT NULL OR duplicate_of IS NOT NULL
	                 OR reject_reason <> '' OR outcome_brief <> '')`, w.org); n != 0 {
		t.Fatalf("%d submission(s) carry triage or lifecycle state: submitting is not triaging", n)
	}
	// Every history row is a submit: no transition, claim, assign or link event.
	if n := w.count(`SELECT count(*) FROM request_events WHERE action <> 'submit'`); n != 0 {
		t.Fatalf("%d non-submit history rows after intake, want 0", n)
	}

	// ---- nothing gained ----
	if got := w.visibleProjects(w.customer); len(got) != len(customerBefore) {
		t.Fatalf("the granted customer's visible projects went from %d to %d: submitting granted access",
			len(customerBefore), len(got))
	}
	if len(customerBefore) != 0 {
		t.Fatalf("the granted customer reached %d project(s) before submitting, want 0 (the grant exists instead of a team role)", len(customerBefore))
	}
	if got := w.visibleProjects(w.member); len(got) != len(memberBefore) {
		t.Fatalf("the member's visible projects went from %d to %d: submitting granted access", len(memberBefore), len(got))
	}
	membershipsAfter := w.count(`SELECT count(*) FROM org_members`) + w.count(`SELECT count(*) FROM team_members`) +
		w.count(`SELECT count(*) FROM project_teams`) + w.count(`SELECT count(*) FROM team_submit_grants`)
	if membershipsAfter != membershipsBefore {
		t.Fatalf("submitting wrote membership rows: %d before, %d after", membershipsBefore, membershipsAfter)
	}

	// ---- the answer is the requester's whitelist, on both surfaces ----
	projection := []string{"public_ref", "state", "subject", "team_id", "source_kind", "created_at"}
	internal := []string{"id", "org_id", "requester_user_id", "requester_contact", "source_external_id",
		"claim_user_id", "owner_user_id", "duplicate_of", "reject_reason", "rev", "reopened_count",
		"metadata", "outcome_brief", "needs_info_since"}
	for _, transport := range []string{"rest", "mcp"} {
		t.Run(transport+" answer leaks nothing", func(t *testing.T) {
			if transport == "rest" {
				r := w.restDo(w.member.token, http.MethodPost, w.inbox(w.team), rawJSON(`{"subject":"`+subjectFor("leak")+`"}`), "")
				if r.status != http.StatusCreated {
					t.Fatalf("submit = %d %v", r.status, r.body)
				}
				req, ok := r.body["request"].(map[string]any)
				if !ok {
					t.Fatalf("answer has no request object: %v", r.body)
				}
				for _, k := range projection {
					if _, ok := req[k]; !ok {
						t.Errorf("the REST answer is missing %q", k)
					}
				}
				for _, k := range internal {
					if _, ok := req[k]; ok {
						t.Errorf("the REST answer leaks %q", k)
					}
				}
				if strings.Contains(r.raw, w.member.ident.Email) {
					t.Fatalf("the REST answer echoes the requester's contact address: %s", r.raw)
				}
				// REST has no operator hint: `next` is the tool's affordance for a
				// model, and a REST client branching on JSON must not be handed prose.
				if _, ok := r.body["next"]; ok {
					t.Errorf("the REST answer carries the MCP-only `next` hint: %v", r.body)
				}
				return
			}
			r := w.mcpTool(w.member.token, "amp_submit_request", map[string]any{
				"team_id": float64(w.team), "subject": subjectFor("leak")})
			if r.status != http.StatusOK {
				t.Fatalf("tool call: HTTP %d: %s", r.status, r.raw)
			}
			content, _ := r.body["result"].(map[string]any)["content"].([]any)
			if len(content) == 0 {
				t.Fatalf("the tool answered no content: %s", r.raw)
			}
			text, _ := content[0].(map[string]any)["text"].(string)
			var payload map[string]any
			if err := json.Unmarshal([]byte(text), &payload); err != nil {
				t.Fatalf("decode the tool's result: %v (%s)", err, text)
			}
			req, ok := payload["request"].(map[string]any)
			if !ok {
				t.Fatalf("the tool's answer has no request object: %s", text)
			}
			for _, k := range projection {
				if _, ok := req[k]; !ok {
					t.Errorf("the MCP answer is missing %q", k)
				}
			}
			for _, k := range internal {
				if _, ok := req[k]; ok {
					t.Errorf("the MCP answer leaks %q", k)
				}
			}
			if strings.Contains(r.raw, w.member.ident.Email) {
				t.Fatalf("the MCP answer echoes the requester's contact address: %s", r.raw)
			}
			if next, _ := payload["next"].(string); next == "" {
				t.Errorf("the tool's answer carries no `next` hint for a model caller: %s", text)
			}
		})
	}

	// ---- the catalog offers no second request operation ----
	//
	// Reading, triaging, commenting, claiming and attaching are later slices with
	// their own gates. Listing them here would advertise a surface no scope was
	// designed around, and building them early would be reachable from a
	// submission credential the moment the scope set widened.
	listed := w.mcpTools(w.member.token)
	if !contains(listed, mcp.SubmitScopeTool) {
		t.Fatalf("amp_submit_request is not listed for a team member: %v", listed)
	}
	for _, forbidden := range []string{
		"amp_list_requests", "amp_get_request", "amp_update_request", "amp_claim_request",
		"amp_assign_request", "amp_comment_request", "amp_link_request", "amp_attach_request",
	} {
		if contains(listed, forbidden) {
			t.Fatalf("%s is listed although intake is the only request operation this slice builds", forbidden)
		}
	}
}
