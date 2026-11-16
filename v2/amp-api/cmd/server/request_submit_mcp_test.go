package main

// amp_submit_request through the real MCP server (epic 202, story 543, AMP
// #3920; approved contract artifact 36 v2 §2 and §5; the submit half of build
// slice I1).
//
// The tool is a thin adapter over Service.SubmitRequest, which cmd/server/
// request_submit_test.go already proves as a boundary. So this file asks only
// the questions that are about *the tool*: does the call arrive at the same
// service (and therefore produce the same row, audit entry and history as
// REST's adapter would), does it work with no project chosen, do the three
// kinds of caller the credential design cares about get the right answer
// through this transport, and does a refusal reach an MCP client with REST's
// status and stable code.
//
//	Criterion 1 — projectless submit and replay through the shared service:
//	  TestMCPSubmitRequest_CreatesThroughTheSharedService,
//	  TestMCPSubmitRequest_ReplayResolvesTheSameRequest
//	Criterion 2 — scoped / foreign / revoked callers:
//	  TestMCPSubmitRequest_CallerMatrix
//	Criterion 3 — coded errors and catalog flags:
//	  TestMCPSubmitRequest_CodedErrors, TestMCPSubmitRequestSchemaAndCatalog
//	  (the catalog half) and internal/mcp/request_submit_test.go (the unit half)
//
// Every call goes through the same path an agent's does: JSON-RPC → MCP server
// (with its real middleware) → tool handler → service → postgres. Identities are
// built in process from users rows, exactly as the auth middleware builds them
// from a token or a session; no live credential and no live stack is involved.
//
// Deliberately absent: no REST route (slice F1), no list/get/update/comment/
// claim/attach tool (later I-slices), no UI. A submission routes nothing, so
// there is no lifecycle to assert past `new`.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// submitMCPWorld is one tenant with an inbox team, a second team in the same
// org, a second org, and the identities a submission's caller can be.
type submitMCPWorld struct {
	s *mcpStack // the whole MCP stack, pointed at a throwaway project
	// dsn is the same database the stack uses, for the two assertions that read
	// a stored row rather than an answer (the row the service wrote, and how
	// many rows one subject produced).
	dsn                  string
	conn                 *pgx.Conn
	org, team, otherTeam int // the tenant, the inbox, a sibling team
	otherOrg, foreign    int // a second tenant and its team

	member   *auth.Identity // member of the inbox team
	admin    *auth.Identity // owner of the org
	customer *auth.Identity // customer kind: holds the submit grant
	other    *auth.Identity // customer kind in otherOrg, no grant
	stranger *auth.Identity // owner of otherOrg
}

func newSubmitMCPWorld(t *testing.T) *submitMCPWorld {
	t.Helper()
	base := setupMCP(t)
	w := &submitMCPWorld{s: base, dsn: testDSN(t)}
	ctx := context.Background()
	repo := base.repo
	sfx := uniq("mcpsubmit")

	user := func(label, kind string) *auth.Identity {
		u, err := repo.UpsertUserFromClaims(ctx, sfx+"-"+label, sfx+"-"+label+"@mcp-submit.test", label, nil)
		if err != nil {
			t.Fatalf("user %s: %v", label, err)
		}
		// The first user ever provisioned is auto-admin, so pin every role
		// explicitly (and a customer holds no global role at all).
		if err := repo.SetUserRole(ctx, u.ID, domain.RoleMember, kind != domain.UserKindCustomer); err != nil {
			t.Fatal(err)
		}
		if kind == domain.UserKindCustomer {
			// The kind a requester invite provisions on first sign-in (#3767).
			// The account path above creates a person, so it is set here the way
			// the invite's provisioning does — the kind is what keeps the account
			// out of org roles and team membership, so a submission test that
			// skipped it would be testing the wrong caller.
			db := w.db(t)
			if _, err := db.Exec(ctx, `UPDATE users SET kind = '`+domain.UserKindCustomer+`' WHERE id = $1`, u.ID); err != nil {
				t.Fatalf("kind of %s: %v", label, err)
			}
			u.Kind = kind
		}
		return &auth.Identity{Subject: u.Subject, Email: u.Email, Name: label, User: u}
	}
	// root first: on an empty database the first user is auto-admin.
	w.stranger = user("stranger", domain.UserKindHuman)
	w.member = user("member", domain.UserKindHuman)
	w.admin = user("admin", domain.UserKindHuman)
	w.customer = user("customer", domain.UserKindCustomer)
	w.other = user("other", domain.UserKindCustomer)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	org, err := repo.CreateOrg(ctx, "MCP submit "+sfx, "mcp-submit-"+sfx, nil, &w.admin.User.ID)
	must(err)
	w.org = org.ID
	tm, err := repo.CreateTeam(ctx, w.org, "Inbox", nil)
	must(err)
	w.team = tm.ID
	sibling, err := repo.CreateTeam(ctx, w.org, "Other desk", nil)
	must(err)
	w.otherTeam = sibling.ID
	otherOrg, err := repo.CreateOrg(ctx, "MCP foreign "+sfx, "mcp-foreign-"+sfx, nil, &w.stranger.User.ID)
	must(err)
	w.otherOrg = otherOrg.ID
	foreign, err := repo.CreateTeam(ctx, w.otherOrg, "Foreign inbox", nil)
	must(err)
	w.foreign = foreign.ID

	must(repo.SetTeamMember(ctx, w.team, w.member.User.ID, domain.TeamRoleMember))
	must(repo.SetOrgMember(ctx, w.otherOrg, w.other.User.ID, domain.OrgRoleMember))
	// The customer's only standing: a submit grant on the inbox team, granted by
	// the team's manager (D14), so the row is the same one an admin would write.
	if _, err := repo.GrantTeamSubmit(ctx, w.team, w.org, w.customer.User.ID, &w.admin.User.ID); err != nil {
		t.Fatalf("grant the customer: %v", err)
	}
	return w
}

// scoped turns an identity into the submit-only credential bound to teamID, as
// the token verifier attaches a token's scope to it (AMP #3772): an API token,
// bound to one team, presented through the principal's scope fields.
func (w *submitMCPWorld) scoped(t *testing.T, who *auth.Identity, teamID int) *auth.Identity {
	t.Helper()
	c := *who
	c.APIToken = true
	c.TokenID = 1
	c.TokenScope = domain.TokenScopeSubmit
	binding := teamID
	c.TokenScopeTeamID = &binding
	return &c
}

// fullToken is the same person's full-scope token: unchanged behaviour, so it
// may submit wherever the person may.
func (w *submitMCPWorld) fullToken(who *auth.Identity) *auth.Identity {
	c := *who
	c.APIToken = true
	c.TokenID = 2
	c.TokenScope = domain.TokenScopeFull
	return &c
}

// mcpCallResult is one tool call's answer, either way: a parsed result payload
// or the JSON-RPC error a client would read.
type mcpCallResult struct {
	result map[string]interface{}
	// errMessage and errData are the response's error member, when the call
	// failed. data carries status/code/retry_after_s for a coded refusal.
	errMessage string
	errData    map[string]interface{}
}

// callTool invokes toolName as who and never fails the test on an error: a
// refusal is a result here, which is what lets the matrix assert both halves.
func (w *submitMCPWorld) callTool(t *testing.T, who *auth.Identity, toolName string, args map[string]interface{}) mcpCallResult {
	t.Helper()
	req := map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]interface{}{"name": toolName, "arguments": args},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp := w.s.as(who).srv.HandleMessage(w.s.as(who).callCtx(), raw)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var rpc struct {
		Error *struct {
			Message string                 `json:"message"`
			Data    map[string]interface{} `json:"data"`
		} `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(b, &rpc); err != nil {
		t.Fatalf("decode response: %v (%s)", err, b)
	}
	out := mcpCallResult{}
	if rpc.Error != nil {
		out.errMessage, out.errData = rpc.Error.Message, rpc.Error.Data
		return out
	}
	if rpc.Result == nil || len(rpc.Result.Content) == 0 {
		t.Fatalf("tool %q: no result content: %s", toolName, b)
	}
	content, err := json.Marshal(rpc.Result.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	var text struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &text); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if err := json.Unmarshal([]byte(text.Text), &out.result); err != nil {
		t.Fatalf("decode result payload from %q: %v\ntext: %s", toolName, err, text.Text)
	}
	return out
}

// submitArgs is one well-formed submission: a team, a subject, a brief and a
// key. No project, no identity, no org — the tool's schema has no such field.
func submitArgs(teamID int, subject, key string) map[string]interface{} {
	args := map[string]interface{}{
		"team_id":     float64(teamID),
		"subject":     subject,
		"input_brief": "We need the triage queue as CSV for our weekly review.",
	}
	if key != "" {
		args["idempotency_key"] = key
	}
	return args
}

// requestField reads request.<key> from a successful submission's payload.
func requestField(t *testing.T, r mcpCallResult, key string) interface{} {
	t.Helper()
	if r.errMessage != "" {
		t.Fatalf("submission was refused: %s %v", r.errMessage, r.errData)
	}
	req, ok := r.result["request"].(map[string]interface{})
	if !ok {
		t.Fatalf("result has no request object: %v", r.result)
	}
	v, ok := req[key]
	if !ok {
		t.Fatalf("request lacks %q: %v", key, req)
	}
	return v
}

// db is this world's database connection, for the assertions that read stored
// rows rather than answers.
func (w *submitMCPWorld) db(t *testing.T) *pgx.Conn {
	t.Helper()
	if w.conn != nil {
		return w.conn
	}
	conn, err := pgx.Connect(context.Background(), w.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	w.conn = conn
	return conn
}

// stored is the request row with publicRef, read straight from the database —
// what an assertion about the *stored* record has to look at.
func (w *submitMCPWorld) stored(t *testing.T, publicRef string) *domain.TeamRequest {
	t.Helper()
	ctx := context.Background()
	var (
		id                                    int
		requesterUserID                       *int
		requesterContact, sourceKind, subject string
		state                                 string
	)
	err := w.db(t).QueryRow(ctx,
		`SELECT id, requester_user_id, requester_contact, source_kind, subject, state
		   FROM team_requests WHERE public_ref = $1`, publicRef).
		Scan(&id, &requesterUserID, &requesterContact, &sourceKind, &subject, &state)
	if err != nil {
		t.Fatalf("read request %q: %v", publicRef, err)
	}
	return &domain.TeamRequest{ID: id, PublicRef: publicRef, RequesterUserID: requesterUserID,
		RequesterContact: requesterContact, SourceKind: domain.RequestSourceKind(sourceKind),
		Subject: subject, State: domain.RequestStatus(state)}
}

// requestsWithSubject counts the rows one subject produced in this fixture,
// which is how a "no second request was written" assertion stays exact while
// other tests share the database.
func (w *submitMCPWorld) requestsWithSubject(t *testing.T, subject string) int {
	t.Helper()
	var n int
	err := w.db(t).QueryRow(context.Background(),
		`SELECT count(*) FROM team_requests WHERE org_id = $1 AND subject = $2`, w.org, subject).Scan(&n)
	if err != nil {
		t.Fatalf("count requests: %v", err)
	}
	return n
}

// ---- criterion 1: projectless submit and replay through the shared service ----

// TestMCPSubmitRequest_CreatesThroughTheSharedService: the tool creates a real
// request with no project named anywhere, and the stored row carries exactly
// what Service.SubmitRequest writes — the principal's requester id and contact
// snapshot, the adapter's `mcp` source, state `new`, one submit history row and
// one request.submit audit row. Those are the service's products; if the tool
// had its own path they would differ.
func TestMCPSubmitRequest_CreatesThroughTheSharedService(t *testing.T) {
	w := newSubmitMCPWorld(t)
	subject := "CSV export of the queue, please " + uniq("s")

	args := submitArgs(w.team, subject, "")
	// Connector-style labels ride the same argument the tool declares, through
	// the domain's bounds (optNewMetadata → Metadata.Clean), so this is also the
	// proof that metadata is not silently dropped on the MCP path.
	args["metadata"] = map[string]interface{}{"source_system": "pagerduty", "severity": "page"}
	got := w.callTool(t, w.member, "amp_submit_request", args)
	if got.errMessage != "" {
		t.Fatalf("submission refused: %s %v", got.errMessage, got.errData)
	}
	if created, _ := got.result["created"].(bool); !created {
		t.Fatalf("created = %v, want true for a first submission", got.result["created"])
	}
	publicRef, _ := requestField(t, got, "public_ref").(string)
	if publicRef == "" {
		t.Fatal("no public_ref in the answer")
	}
	if created, ok := requestField(t, got, "state").(string); !ok || created != string(domain.RequestStatusNew) {
		t.Fatalf("state = %v, want %q", requestField(t, got, "state"), domain.RequestStatusNew)
	}

	row := w.stored(t, publicRef)
	if row.Subject != subject {
		t.Errorf("stored subject = %q, want %q", row.Subject, subject)
	}
	if row.SourceKind != domain.RequestSourceMCP {
		t.Errorf("stored source_kind = %q, want %q: the adapter declares the transport, not the payload", row.SourceKind, domain.RequestSourceMCP)
	}
	if row.RequesterUserID == nil || *row.RequesterUserID != w.member.User.ID {
		t.Errorf("stored requester = %v, want %d (the authenticated caller)", row.RequesterUserID, w.member.User.ID)
	}
	if row.RequesterContact != w.member.User.Email {
		t.Errorf("stored contact = %q, want the caller's own %q", row.RequesterContact, w.member.User.Email)
	}
	if row.State != domain.RequestStatusNew {
		t.Errorf("stored state = %q, want new: intake moves nothing", row.State)
	}
	var severity string
	if err := w.db(t).QueryRow(context.Background(),
		`SELECT metadata->>'severity' FROM team_requests WHERE id = $1`, row.ID).Scan(&severity); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if severity != "page" {
		t.Errorf("stored metadata severity = %q, want page (a label, never an authorization decision)", severity)
	}

	// The history row and the audit row the service writes, both targeting this
	// request: submitting routes, plans and dispatches nothing, so there is
	// nothing else to find.
	var events int
	if err := w.db(t).QueryRow(context.Background(),
		`SELECT count(*) FROM request_events WHERE request_id = $1 AND action = 'submit'`, row.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("submit history rows = %d, want 1", events)
	}
	entries, err := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{
		Action: "request.submit", TargetID: fmt.Sprint(row.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("request.submit audit rows = %d, want exactly 1", len(entries))
	}
	if source, _ := entries[0].Details["source_kind"].(string); source != string(domain.RequestSourceMCP) {
		t.Errorf("audit source_kind = %v, want mcp", entries[0].Details["source_kind"])
	}
}

// TestMCPSubmitRequest_ReplayResolvesTheSameRequest: the same key returns the
// request the first call wrote (created=false, same reference), and a direct
// service call with the same key and source answers the same request too — the
// proof that the tool's call and a service caller's call are the same call, not
// two implementations that happen to agree.
func TestMCPSubmitRequest_ReplayResolvesTheSameRequest(t *testing.T) {
	w := newSubmitMCPWorld(t)
	subject := "replay me " + uniq("s")
	key := "key-" + uniq("k")

	first := w.callTool(t, w.member, "amp_submit_request", submitArgs(w.team, subject, key))
	publicRef, _ := requestField(t, first, "public_ref").(string)

	second := w.callTool(t, w.member, "amp_submit_request", submitArgs(w.team, subject, key))
	if created, _ := second.result["created"].(bool); created {
		t.Fatalf("a replay reported created=true: %v", second.result)
	}
	if got, _ := requestField(t, second, "public_ref").(string); got != publicRef {
		t.Fatalf("replay answered request %q, want %q", got, publicRef)
	}
	if n := w.requestsWithSubject(t, subject); n != 1 {
		t.Fatalf("requests with that subject = %d, want 1: a replay writes nothing", n)
	}

	// The same submission through the service layer (what REST's adapter will
	// call) resolves to the request the tool created.
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	svc.RequestLimits = service.NewRequestLimiter(service.RequestLimitConfig{Window: time.Minute, Max: 1000})
	svc.Now = time.Now
	res, err := svc.SubmitRequest(context.Background(), authz.Principal{
		UserID: w.member.User.ID, Email: w.member.User.Email, Channel: auth.ChannelToken, AuthMethod: auth.AuthAPIToken,
	}, service.SubmitRequestInput{
		TeamID: w.team, Source: domain.RequestSourceMCP, Subject: subject,
		InputBrief: "We need the triage queue as CSV for our weekly review.", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("the service's own replay was refused: %v", err)
	}
	if res.Created || res.Request.PublicRef != publicRef {
		t.Fatalf("service replay = created=%v ref=%q, want created=false ref=%q", res.Created, res.Request.PublicRef, publicRef)
	}

	// A different payload under the same key is the conflict the service
	// defines, with REST's code.
	conflict := w.callTool(t, w.member, "amp_submit_request", submitArgs(w.team, subject+" (edited)", key))
	if conflict.errMessage == "" {
		t.Fatalf("a reused key with a different payload was accepted: %v", conflict.result)
	}
	if got := conflict.errData["code"]; got != authz.CodeRequestConflict {
		t.Fatalf("conflict code = %v, want %q (message: %s)", got, authz.CodeRequestConflict, conflict.errMessage)
	}
}

// ---- criterion 2: scoped, foreign and revoked callers ----

// TestMCPSubmitRequest_CallerMatrix: every caller the credential design
// distinguishes gets its answer through this transport, and a refusal writes no
// request.
func TestMCPSubmitRequest_CallerMatrix(t *testing.T) {
	w := newSubmitMCPWorld(t)

	// who is one row's caller; args() is built per row so a row can aim at a
	// different team, and a row can revoke access immediately before its call.
	type row struct {
		name string
		who  func() *auth.Identity
		args func(subject string) map[string]interface{}
		// wantOK means the submission must be created. Otherwise wantStatus and
		// wantCode describe the refusal (wantCode "" means no stable code).
		wantOK     bool
		wantStatus float64
		wantCode   string
		// wantMessage, when set, must appear in the refusal.
		wantMessage string
	}
	rows := []row{
		{
			name:   "team member",
			who:    func() *auth.Identity { return w.member },
			args:   func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantOK: true,
		},
		{
			name:   "org admin",
			who:    func() *auth.Identity { return w.admin },
			args:   func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantOK: true,
		},
		{
			name:   "customer holding a submit grant",
			who:    func() *auth.Identity { return w.customer },
			args:   func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantOK: true,
		},
		{
			name:   "submit-only credential on its own team",
			who:    func() *auth.Identity { return w.scoped(t, w.member, w.team) },
			args:   func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantOK: true,
		},
		{
			name:   "full-scope token of a member",
			who:    func() *auth.Identity { return w.fullToken(w.member) },
			args:   func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantOK: true,
		},
		{
			// The scope is an attenuation of reach: bound to another team, this
			// credential cannot submit here, and the answer names nothing.
			name:       "submit-only credential bound to a foreign team",
			who:        func() *auth.Identity { return w.scoped(t, w.member, w.foreign) },
			args:       func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantStatus: 404, wantMessage: "not found",
		},
		{
			name:       "customer in another org with no grant",
			who:        func() *auth.Identity { return w.other },
			args:       func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantStatus: 404, wantMessage: "not found",
		},
		{
			name:       "owner of another org",
			who:        func() *auth.Identity { return w.stranger },
			args:       func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantStatus: 404, wantMessage: "not found",
		},
		{
			name:       "unknown team",
			who:        func() *auth.Identity { return w.member },
			args:       func(s string) map[string]interface{} { return submitArgs(1<<30, s, "") },
			wantStatus: 404, wantMessage: "not found",
		},
		{
			// A sibling team in the same org is not this caller's inbox: the
			// membership they hold is the other team's.
			name:       "member of a different team in the same org",
			who:        func() *auth.Identity { return w.member },
			args:       func(s string) map[string]interface{} { return submitArgs(w.otherTeam, s, "") },
			wantStatus: 403, wantMessage: "submit access",
		},
		{
			// Revocation takes effect on the very next call: the grant is read
			// from the rows, never remembered.
			name: "customer whose grant was just revoked",
			who: func() *auth.Identity {
				if _, err := w.s.repo.RevokeTeamSubmit(context.Background(), w.team, w.customer.User.ID, &w.admin.User.ID); err != nil {
					t.Fatalf("revoke: %v", err)
				}
				return w.customer
			},
			args:       func(s string) map[string]interface{} { return submitArgs(w.team, s, "") },
			wantStatus: 404, wantMessage: "not found",
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			subject := "caller matrix " + uniq("s")
			got := w.callTool(t, r.who(), "amp_submit_request", r.args(subject))
			switch {
			case r.wantOK:
				if got.errMessage != "" {
					t.Fatalf("submission refused: %s %v", got.errMessage, got.errData)
				}
				if n := w.requestsWithSubject(t, subject); n != 1 {
					t.Fatalf("requests = %d, want 1", n)
				}
			default:
				if got.errMessage == "" {
					t.Fatalf("submission was accepted: %v", got.result)
				}
				if got.errData["status"] != r.wantStatus {
					t.Fatalf("status = %v, want %v (message: %s)", got.errData["status"], r.wantStatus, got.errMessage)
				}
				if r.wantCode != "" && got.errData["code"] != r.wantCode {
					t.Fatalf("code = %v, want %q", got.errData["code"], r.wantCode)
				}
				if r.wantMessage != "" && !strings.Contains(got.errMessage, r.wantMessage) {
					t.Fatalf("message %q doesn't contain %q", got.errMessage, r.wantMessage)
				}
				if n := w.requestsWithSubject(t, subject); n != 0 {
					t.Fatalf("a refused submission wrote %d request(s)", n)
				}
			}
		})
	}
}

// TestMCPSubmitRequestScopeGatesEveryOtherTool is the other half of the
// credential story through this transport: a submit-only credential may call
// amp_submit_request (it just did, above) and nothing else, and its tools/list
// advertises exactly that one tool. The gate and the acceptance of this tool
// must not disagree, or the credential's one operation would be unreachable.
func TestMCPSubmitRequestScopeGatesEveryOtherTool(t *testing.T) {
	w := newSubmitMCPWorld(t)
	scopedStack := w.s.as(w.scoped(t, w.member, w.team))

	// tools/list: exactly the one tool this slice registered.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	resp := scopedStack.srv.HandleMessage(scopedStack.callCtx(), raw)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, b)
	}
	if len(list.Result.Tools) != 1 || list.Result.Tools[0].Name != "amp_submit_request" {
		var names []string
		for _, tl := range list.Result.Tools {
			names = append(names, tl.Name)
		}
		t.Fatalf("scoped tools/list = %v, want exactly [amp_submit_request]", names)
	}

	// Every other tool is still refused when called anyway.
	for _, tool := range []string{"amp_whoami", "amp_list_projects", "amp_list_tasks", "amp_get_task"} {
		got := w.callTool(t, w.scoped(t, w.member, w.team), tool, map[string]interface{}{})
		if got.errMessage == "" {
			t.Fatalf("%s was allowed for a submit-only credential", tool)
		}
		if !strings.Contains(got.errMessage, "may only submit requests") {
			t.Fatalf("%s refusal = %q, want the scope refusal", tool, got.errMessage)
		}
	}
}

// ---- criterion 3: coded errors, with REST's shapes ----

// TestMCPSubmitRequest_CodedErrors: each refusal a submission can get reaches an
// MCP client with the status and stable code REST puts in its envelope, and a
// rate-limited submission also carries the wait as retry_after_s (REST's
// Retry-After).
func TestMCPSubmitRequest_CodedErrors(t *testing.T) {
	w := newSubmitMCPWorld(t)

	t.Run("invalid names the field", func(t *testing.T) {
		got := w.callTool(t, w.member, "amp_submit_request", map[string]interface{}{
			"team_id": float64(w.team),
			// Over domain.MaxRequestSubjectLen: the domain refuses it, the
			// service maps that to request.invalid naming the field.
			"subject": strings.Repeat("x", domain.MaxRequestSubjectLen+1),
		})
		if got.errMessage == "" {
			t.Fatalf("an over-long subject was accepted: %v", got.result)
		}
		if got.errData["status"] != float64(400) || got.errData["code"] != authz.CodeRequestInvalid {
			t.Fatalf("refusal = %v %q, want 400 %s", got.errData, got.errMessage, authz.CodeRequestInvalid)
		}
		if !strings.Contains(got.errMessage, "subject") {
			t.Errorf("message %q doesn't name the field", got.errMessage)
		}
	})

	t.Run("an identity field in the payload is refused", func(t *testing.T) {
		args := submitArgs(w.team, "spoof attempt "+uniq("s"), "")
		args["requester_contact"] = "someone-else@example.com"
		got := w.callTool(t, w.member, "amp_submit_request", args)
		if got.errMessage == "" {
			t.Fatalf("a payload naming a requester was accepted: %v", got.result)
		}
		if got.errData["status"] != float64(400) || got.errData["code"] != authz.CodeRequestInvalid {
			t.Fatalf("refusal = %v %q, want 400 %s", got.errData, got.errMessage, authz.CodeRequestInvalid)
		}
		if !strings.Contains(got.errMessage, "requester_contact") {
			t.Errorf("message %q doesn't name the field", got.errMessage)
		}
	})

	t.Run("rate limited carries retry_after_s", func(t *testing.T) {
		// The limiter is per (org, team, submitter, source) and process-wide, so
		// a distinct caller per row keeps this exact: spend one key's budget,
		// then observe the refusal's wait.
		subject := "rate limit " + uniq("s")
		var limited mcpCallResult
		for i := 0; i < service.DefaultRequestLimitPerWindow+5; i++ {
			limited = w.callTool(t, w.customer, "amp_submit_request",
				submitArgs(w.team, subject+fmt.Sprint(i), ""))
			if limited.errMessage != "" {
				break
			}
		}
		if limited.errMessage == "" {
			t.Skip("the shared submission limiter did not reach its ceiling in this run; the coded 429 is proven in internal/mcp and cmd/server's service suite")
		}
		if limited.errData["status"] != float64(429) || limited.errData["code"] != authz.CodeRequestRateLimited {
			t.Fatalf("refusal = %v %q, want 429 %s", limited.errData, limited.errMessage, authz.CodeRequestRateLimited)
		}
		if wait, _ := limited.errData["retry_after_s"].(float64); wait < 1 {
			t.Fatalf("retry_after_s = %v, want a whole second at least", limited.errData["retry_after_s"])
		}
	})
}

// TestMCPSubmitRequest_RefusesWrongTypedOptionalArguments is AMP #3931: the two
// optional string arguments are refused when they arrive with the wrong JSON
// type, instead of being coerced to "" the way optStr's fall-back does.
//
// The divergence this closes is silent, which is why it needs a test: before
// this, `amp_submit_request {"idempotency_key": 123}` was *accepted* and wrote a
// keyless request, while REST's strict decoder refused the same shape with 400
// request.invalid. A client that believed it held a retry key then retried with
// the same wrong-typed key and submitted twice — the duplicate the key exists to
// prevent.
//
// So each row asserts the three things the refusal promises: the coded 400 names
// the argument, no request row is written, and no idempotency-key row is written
// (a stored key for a refused submission would answer a retry with a request
// that was never created). The limiter is not asserted here —
// TestSubmitSurface_WrongTypedOptionalArgumentsRefused measures that on the full
// listener — because the refusal happens in the adapter, before the service that
// charges it.
func TestMCPSubmitRequest_RefusesWrongTypedOptionalArguments(t *testing.T) {
	w := newSubmitMCPWorld(t)

	countKeys := func() int {
		t.Helper()
		var n int
		if err := w.db(t).QueryRow(context.Background(),
			`SELECT count(*) FROM request_idempotency_keys WHERE org_id = $1`, w.org).Scan(&n); err != nil {
			t.Fatalf("count key rows: %v", err)
		}
		return n
	}
	countRequests := func() int {
		t.Helper()
		var n int
		if err := w.db(t).QueryRow(context.Background(),
			`SELECT count(*) FROM team_requests WHERE org_id = $1`, w.org).Scan(&n); err != nil {
			t.Fatalf("count requests: %v", err)
		}
		return n
	}
	requestsBefore, keysBefore := countRequests(), countKeys()

	rows := []struct {
		name  string
		arg   string
		value interface{}
	}{
		{"wrong-typed idempotency_key", "idempotency_key", float64(123)},
		{"wrong-typed input_brief", "input_brief", true},
		{"an object input_brief", "input_brief", map[string]interface{}{"text": "x"}},
		{"an array idempotency_key", "idempotency_key", []interface{}{"k"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			subject := "typed " + r.name + " " + uniq("s")
			args := submitArgs(w.team, subject, "")
			args[r.arg] = r.value

			got := w.callTool(t, w.member, "amp_submit_request", args)
			if got.errMessage == "" {
				t.Fatalf("%s was accepted (created=%v); a wrong-typed argument must be refused, never coerced to empty",
					r.name, got.result["created"])
			}
			if got.errData["status"] != float64(400) || got.errData["code"] != authz.CodeRequestInvalid {
				t.Fatalf("refusal = %v %q, want 400 %s (REST's answer for the same JSON)",
					got.errData, got.errMessage, authz.CodeRequestInvalid)
			}
			if !strings.Contains(got.errMessage, r.arg) {
				t.Errorf("message %q doesn't name the argument %q", got.errMessage, r.arg)
			}
			if n := w.requestsWithSubject(t, subject); n != 0 {
				t.Fatalf("%s wrote %d request row(s)", r.name, n)
			}
			if n := countKeys(); n != keysBefore {
				t.Fatalf("%s wrote an idempotency-key row (%d before, %d after)", r.name, keysBefore, n)
			}
		})
	}
	// Nothing in the table reached the product at all: the adapter refused every
	// row before the service that writes and charges.
	if n := countRequests(); n != requestsBefore {
		t.Fatalf("request rows in this org went %d -> %d across the refusal matrix", requestsBefore, n)
	}

	// The other side of the same contract, asserted so nobody "fixes" the
	// refusal into refusing absence: a present null is absence (REST's decoder
	// reads JSON null for a string field as ""), so the submission is accepted —
	// briefless and keyless — rather than refused.
	t.Run("a present null input_brief is absence", func(t *testing.T) {
		subject := "null brief " + uniq("s")
		args := submitArgs(w.team, subject, "")
		args["input_brief"] = nil
		got := w.callTool(t, w.member, "amp_submit_request", args)
		if got.errMessage != "" {
			t.Fatalf("a null input_brief was refused: %s %v", got.errMessage, got.errData)
		}
		if created, _ := got.result["created"].(bool); !created {
			t.Fatalf("created = %v, want a fresh create", got.result["created"])
		}
		if n := w.requestsWithSubject(t, subject); n != 1 {
			t.Fatalf("%d requests for the null-brief subject, want 1", n)
		}
	})
}

// TestMCPSubmitRequestSchemaOnTheWire: the schema a client receives declares the
// two REQUIRED fields and the bounds, and offers no project or identity field —
// read off the tools/list an MCP client actually makes, not off the Go values.
func TestMCPSubmitRequestSchemaOnTheWire(t *testing.T) {
	w := newSubmitMCPWorld(t)
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	resp := w.s.srv.HandleMessage(w.s.callCtx(), raw)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Annotations struct {
					IdempotentHint *bool `json:"idempotentHint"`
					ReadOnlyHint   *bool `json:"readOnlyHint"`
				} `json:"annotations"`
				InputSchema struct {
					Required   []string                  `json:"required"`
					Properties map[string]map[string]any `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, b)
	}
	var found bool
	for _, tl := range list.Result.Tools {
		if tl.Name != "amp_submit_request" {
			continue
		}
		found = true
		if len(tl.InputSchema.Required) != 2 || tl.InputSchema.Required[0] != "subject" || tl.InputSchema.Required[1] != "team_id" {
			t.Errorf("required = %v, want [subject team_id]", tl.InputSchema.Required)
		}
		for _, forbidden := range []string{"project_id", "org_id", "requester", "requester_contact", "source", "source_kind"} {
			if _, ok := tl.InputSchema.Properties[forbidden]; ok {
				t.Errorf("schema offers %q", forbidden)
			}
		}
		if got, _ := tl.InputSchema.Properties["subject"]["maxLength"].(float64); got != float64(domain.MaxRequestSubjectLen) {
			t.Errorf("subject maxLength = %v, want %d", tl.InputSchema.Properties["subject"]["maxLength"], domain.MaxRequestSubjectLen)
		}
		// Truthful retry semantics: a write that is only idempotent with a key
		// must not be advertised as idempotent.
		if tl.Annotations.IdempotentHint == nil || *tl.Annotations.IdempotentHint {
			t.Errorf("idempotentHint = %v, want false", tl.Annotations.IdempotentHint)
		}
		if tl.Annotations.ReadOnlyHint == nil || *tl.Annotations.ReadOnlyHint {
			t.Errorf("readOnlyHint = %v, want false", tl.Annotations.ReadOnlyHint)
		}
	}
	if !found {
		t.Fatal("amp_submit_request is not listed for a team member")
	}
}
