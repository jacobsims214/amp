package main

// Audit log v2 coverage (#2428, KB decisions/audit-log-v2.md): plan
// approval with its context, refused approvals and gate refusals,
// sign-ins, new accounts and accepted invites, API token use and failure,
// KB changes, exports, a person's ticket marked done, before/after on
// settings, roles and webhooks, and rate-limited access.denied.
//
// Plan approval here is a simulated person in the web UI on a throwaway
// test database (the REST listener with a ChannelUI identity), never an
// agent approving on a real AMP.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/reqctx"
	"github.com/simstech/amp-api/internal/service"
)

// auditREST is the REST listener as main() builds it (reqctx outermost,
// then the identity the auth middleware would resolve), with the test
// client trusted as a proxy so X-Forwarded-For names the client.
type auditREST struct {
	w   *rbacWorld
	srv *httptest.Server
	ids map[string]*auth.Identity
}

func newAuditREST(t *testing.T, w *rbacWorld) *auditREST {
	t.Helper()
	mux := http.NewServeMux()
	api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb).Register(mux)
	trusted, err := reqctx.ParseTrusted("127.0.0.0/8, ::1/128")
	if err != nil {
		t.Fatal(err)
	}
	a := &auditREST{w: w, ids: map[string]*auth.Identity{"root": w.root, "alice": w.alice, "bob": w.bob, "lead": w.lead,
		"orgadmin": w.orgadmin, "dave": w.dave, "owner": w.owner}}
	h := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c := *a.ids[r.Header.Get("X-Test-As")]
		c.Listener, c.Channel, c.AuthMethod, c.SessionID = "rest", auth.ChannelUI, auth.AuthDexSession, "sess-"+r.Header.Get("X-Test-As")
		if r.Header.Get("X-Test-Token") != "" {
			c.Channel, c.APIToken, c.AuthMethod, c.SessionID, c.TokenID = auth.ChannelToken, true, auth.AuthAPIToken, "", 99
		}
		mux.ServeHTTP(rw, r.WithContext(auth.WithIdentity(r.Context(), &c)))
	})
	a.srv = httptest.NewServer(reqctx.Middleware(reqctx.ListenerREST, reqctx.NewResolver(trusted))(h))
	t.Cleanup(a.srv.Close)
	return a
}

// do sends a request as who from client address ip (via X-Forwarded-For).
func (a *auditREST) do(t *testing.T, who, ip, method, path string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, a.srv.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-As", who)
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("User-Agent", "Mozilla/5.0 (coverage test)")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// mcpFrom returns s's tool-call context with the MCP listener's request
// info, as the listener's middleware sets it in production.
func mcpFrom(s *mcpStack, ip, ua string) context.Context {
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.RemoteAddr = ip + ":40000"
	req.Header.Set("User-Agent", ua)
	return reqctx.WithInfo(s.callCtx(), reqctx.Capture(req, reqctx.ListenerMCP, reqctx.NewResolver(nil)))
}

// mcpCall calls a tool in ctx and returns the error text ("" on success).
func mcpCall(t *testing.T, s *mcpStack, ctx context.Context, tool string, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	out, _ := json.Marshal(s.srv.HandleMessage(ctx, raw))
	if strings.Contains(string(out), `"isError":true`) || strings.Contains(string(out), `"error"`) {
		return string(out)
	}
	return ""
}

// auditRows returns every entry for action matching f, newest first.
func auditRows(t *testing.T, repo *repository.Repo, f domain.AuditFilter) []domain.AuditEntry {
	t.Helper()
	got, err := repo.ListAudit(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The plan approval row says who approved, from which address, with what
// user agent and client, on which project, which tickets it covered and
// which version of the plan; every refused attempt is its own denied row.
func TestAuditCoverage_PlanApproval(t *testing.T) {
	w := newRBACWorld(t)
	rest := newAuditREST(t, w)
	root := w.s.as(w.root)
	approvalOn(t, root)
	pid := w.s.projectID
	proj, _ := w.s.repo.GetProject(context.Background(), pid)
	a := mkIn(t, root, w.s.storyID, "approve me")

	// An agent's amp_approve_plan is refused and recorded as
	// plan.approve.denied, once: the generic access.denied hook sees the
	// request already recorded.
	ctx := mcpFrom(root, "198.51.100.20", "claude-code/2.1")
	if msg := mcpCall(t, root, ctx, "amp_approve_plan", map[string]any{"project_id": float64(pid)}); !strings.Contains(msg, "web UI") {
		t.Fatalf("amp_approve_plan: %s", msg)
	}
	den := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.root.User.ID, Action: "plan.approve.denied"})
	if len(den) != 1 {
		t.Fatalf("plan.approve.denied rows = %d", len(den))
	}
	if e := den[0]; e.Outcome != "denied" || !strings.Contains(e.Reason, "web UI") || e.Client != "mcp" || e.SourceIP != "198.51.100.20" ||
		e.UserAgent != "claude-code/2.1" || e.ProjectID == nil || *e.ProjectID != pid || e.TargetName != proj.Name {
		t.Fatalf("plan.approve.denied = %+v", e)
	}
	if n := len(auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.root.User.ID, Action: "access.denied"})); n != 0 {
		t.Fatalf("refusal recorded twice: %d access.denied rows", n)
	}

	// An API token on REST is refused too.
	code, _ := rest.do(t, "bob", "203.0.113.30", "POST", fmt.Sprintf("/api/projects/%d/approve-plan", pid), nil, "X-Test-Token", "1")
	if code != 403 {
		t.Fatalf("approve with a token: %d", code)
	}
	den = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.bob.User.ID, Action: "plan.approve.denied"})
	if len(den) != 1 || den[0].Client != "api" || den[0].AuthMethod != "api_token" || den[0].SourceIP != "203.0.113.30" {
		t.Fatalf("token refusal rows = %+v", den)
	}

	// On behalf without lead access: alice (a member) asks for root's ticket.
	code, _ = rest.do(t, "alice", "203.0.113.31", "POST", fmt.Sprintf("/api/projects/%d/approve-plan", pid),
		map[string]any{"task_ids": []int{a}, "on_behalf": true})
	if code != 403 {
		t.Fatalf("on_behalf without lead: %d", code)
	}
	den = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.alice.User.ID, Action: "plan.approve.denied"})
	if len(den) != 1 || !strings.Contains(den[0].Reason, "lead access") || fmt.Sprint(den[0].Details["others"]) != fmt.Sprint([]any{float64(a)}) ||
		den[0].Client != "web" {
		t.Fatalf("on_behalf refusal rows = %+v", den)
	}

	// Dispatch before approval is refused by the gate.
	if msg := mcpCall(t, root, ctx, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "w"}); !strings.Contains(msg, "approved") {
		t.Fatalf("dispatch: %s", msg)
	}
	dd := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.root.User.ID, Action: "task.dispatch.denied"})
	if len(dd) != 1 || dd[0].TargetType != "task" || dd[0].TargetID != fmt.Sprint(a) || dd[0].Outcome != "denied" || dd[0].Details["agent_id"] != "w" {
		t.Fatalf("task.dispatch.denied = %+v", dd)
	}

	// A person approves in the web UI (simulated on the test database).
	asOf := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	code, out := rest.do(t, "root", "203.0.113.7", "POST", fmt.Sprintf("/api/projects/%d/approve-plan", pid), map[string]any{"as_of": asOf},
		"X-Request-Id", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if code != 200 {
		t.Fatalf("approve in the UI: %d %v", code, out)
	}
	ok := auditRows(t, w.s.repo, domain.AuditFilter{ProjectID: pid, Action: "plan.approve"})
	if len(ok) != 1 {
		t.Fatalf("plan.approve rows = %d", len(ok))
	}
	e := ok[0]
	if e.ActorUserID == nil || *e.ActorUserID != w.root.User.ID || e.ActorEmail != w.root.User.Email || e.SourceIP != "203.0.113.7" ||
		e.UserAgent != "Mozilla/5.0 (coverage test)" || e.Client != "web" || e.AuthMethod != "dex_session" || e.SessionID != "sess-root" ||
		e.RequestID != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" || e.TargetName != proj.Name || e.Outcome != "success" {
		t.Fatalf("plan.approve = %+v", e)
	}
	d := e.Details
	if fmt.Sprint(d["approved"]) != fmt.Sprint([]any{float64(a)}) || d["approved_count"] != 1.0 || !strings.HasSuffix(fmt.Sprint(d["plan_url"]), planPath(pid)) ||
		d["plan_as_of"] != asOf.Format(time.RFC3339Nano) || d["approved_at"] == nil || d["channel"] != "ui" {
		t.Fatalf("plan.approve details = %v", d)
	}
}

// Gates refusing to move work forward: completing a ticket never
// dispatched, setting it in progress by hand.
func TestAuditCoverage_GateRefusals(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	ctx := mcpFrom(alice, "198.51.100.40", "opencode/1")
	id := mkIn(t, alice, w.s.storyID, "not dispatched")
	if msg := mcpCall(t, alice, ctx, "amp_complete_task", map[string]any{"task_id": float64(id)}); !strings.Contains(msg, "never dispatched") {
		t.Fatalf("complete: %s", msg)
	}
	if msg := mcpCall(t, alice, ctx, "amp_set_task_state", map[string]any{"task_id": float64(id), "state": "in_progress"}); !strings.Contains(msg, "dispatch it") {
		t.Fatalf("set in_progress: %s", msg)
	}
	c := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.alice.User.ID, Action: "task.complete.denied"})
	s := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.alice.User.ID, Action: "task.state.denied"})
	if len(c) != 1 || c[0].TargetID != fmt.Sprint(id) || c[0].Details["state"] != "backlog" || c[0].SourceIP != "198.51.100.40" {
		t.Fatalf("task.complete.denied = %+v", c)
	}
	if len(s) != 1 || s[0].Details["to"] != "in_progress" || !strings.Contains(s[0].Reason, "dispatch") {
		t.Fatalf("task.state.denied = %+v", s)
	}
	// Repeats inside the window are counted, not written.
	for range 3 {
		mcpCall(t, alice, mcpFrom(alice, "198.51.100.40", "opencode/1"), "amp_complete_task", map[string]any{"task_id": float64(id)})
	}
	if n := len(auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.alice.User.ID, Action: "task.complete.denied"})); n != 1 {
		t.Fatalf("rate limit: %d rows", n)
	}
}

// Turning plan approval off: refused off the UI (denied row), and in the
// UI recorded with the settings before and after.
func TestAuditCoverage_ProjectSettings(t *testing.T) {
	w := newRBACWorld(t)
	rest := newAuditREST(t, w)
	root := w.s.as(w.root)
	approvalOn(t, root)
	pid := w.s.projectID
	if msg := mcpCall(t, root, mcpFrom(root, "198.51.100.50", "x"), "amp_update_project_settings",
		map[string]any{"project_id": float64(pid), "require_plan_approval": false}); !strings.Contains(msg, "web UI") {
		t.Fatalf("turn off over MCP: %s", msg)
	}
	if den := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.root.User.ID, Action: "project.settings.denied"}); len(den) != 1 ||
		den[0].Details["require_plan_approval"] != false {
		t.Fatalf("project.settings.denied = %+v", den)
	}
	code, out := rest.do(t, "root", "203.0.113.8", "PATCH", fmt.Sprintf("/api/projects/%d", pid),
		map[string]any{"require_plan_approval": false, "metadata": map[string]any{"jira": "PLAT", "api_token": "s3cret"}})
	if code != 200 {
		t.Fatalf("settings in the UI: %d %v", code, out)
	}
	rows := auditRows(t, w.s.repo, domain.AuditFilter{ProjectID: pid, Action: "project.settings", Limit: 1})
	e := rows[0]
	if e.Before["require_plan_approval"] != true || e.After["require_plan_approval"] != false || e.SourceIP != "203.0.113.8" {
		t.Fatalf("project.settings before/after = %v / %v (%+v)", e.Before, e.After, e)
	}
	md, _ := e.After["metadata"].(map[string]any)
	if md["jira"] != "PLAT" || md["api_token"] != "[redacted]" {
		t.Fatalf("metadata not redacted: %v", md)
	}
}

// A person's ticket: their agent marking it done is refused (denied row);
// the person marking it done in the UI is recorded.
func TestAuditCoverage_HumanTicket(t *testing.T) {
	w := newRBACWorld(t)
	rest := newAuditREST(t, w)
	build, review, _ := humanChain(t, w)
	alice := w.s.as(w.alice)
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(build), "agent_id": "w"})
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(build)})
	bob := w.s.as(w.bob)
	if msg := mcpCall(t, bob, mcpFrom(bob, "198.51.100.60", "claude-code/2"), "amp_complete_task",
		map[string]any{"task_id": float64(review), "note": "merged"}); !strings.Contains(msg, "web UI") {
		t.Fatalf("agent marking a human ticket done: %s", msg)
	}
	den := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.bob.User.ID, Action: "task.human.complete.denied"})
	if len(den) != 1 || den[0].TargetID != fmt.Sprint(review) || den[0].Details["via_agent"] != true {
		t.Fatalf("task.human.complete.denied = %+v", den)
	}
	code, out := rest.do(t, "bob", "203.0.113.9", "POST", fmt.Sprintf("/api/tasks/%d/complete", review),
		map[string]any{"note": "Merged", "pr_url": "https://git.example.com/acme/pull/42", "hours_worked": 1.5})
	if code != 200 {
		t.Fatalf("complete in the UI: %d %v", code, out)
	}
	done := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.bob.User.ID, Action: "task.human.complete"})
	if len(done) != 1 || done[0].SourceIP != "203.0.113.9" || done[0].Client != "web" || done[0].Details["hours_worked"] != 1.5 ||
		done[0].Details["pr_url"] != "https://git.example.com/acme/pull/42" || done[0].TargetID != fmt.Sprint(review) {
		t.Fatalf("task.human.complete = %+v", done)
	}
}

// A whole-project export is recorded with what it contained.
func TestAuditCoverage_ProjectExport(t *testing.T) {
	w := newRBACWorld(t)
	rest := newAuditREST(t, w)
	mkIn(t, w.s.as(w.owner), w.s.storyID, "exported")
	code, _ := rest.do(t, "owner", "203.0.113.10", "GET", fmt.Sprintf("/api/projects/%d/export", w.s.projectID), nil)
	if code != 200 {
		t.Fatalf("export: %d", code)
	}
	rows := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.owner.User.ID, Action: "project.export"})
	if len(rows) != 1 || rows[0].SourceIP != "203.0.113.10" || rows[0].Details["tasks"] != 1.0 || rows[0].TargetName == "" {
		t.Fatalf("project.export = %+v", rows)
	}
	// The MCP tool too.
	owner := w.s.as(w.owner)
	if msg := mcpCall(t, owner, mcpFrom(owner, "198.51.100.70", "x"), "amp_export_project", map[string]any{"project_id": float64(w.s.projectID)}); msg != "" {
		t.Fatalf("amp_export_project: %s", msg)
	}
	rows = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.owner.User.ID, Action: "project.export"})
	if len(rows) != 2 || rows[0].Client != "mcp" || rows[0].SourceIP != "198.51.100.70" {
		t.Fatalf("project.export via MCP = %+v", rows)
	}
}

// Any other 403 is access.denied, once per actor and route per window.
func TestAuditCoverage_AccessDenied(t *testing.T) {
	w := newRBACWorld(t)
	rest := newAuditREST(t, w)
	path := fmt.Sprintf("/api/orgs/%d/members", w.org.ID)
	for range 3 {
		if code, _ := rest.do(t, "alice", "203.0.113.11", "POST", path, map[string]any{"user": w.bob.Email, "role": "admin"}); code != 403 {
			t.Fatalf("member add by a non-admin: %d", code)
		}
	}
	rows := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.alice.User.ID, Action: "access.denied"})
	if len(rows) != 1 || rows[0].TargetType != "route" || rows[0].TargetID != "POST "+path || rows[0].Outcome != "denied" ||
		rows[0].Reason == "" || rows[0].SourceIP != "203.0.113.11" {
		t.Fatalf("access.denied (REST) = %+v", rows)
	}
	// An MCP tool refused with 403.
	alice := w.s.as(w.alice)
	if msg := mcpCall(t, alice, mcpFrom(alice, "198.51.100.80", "x"), "amp_audit_log", map[string]any{}); msg == "" {
		t.Fatal("amp_audit_log for a non-admin succeeded")
	}
	rows = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.alice.User.ID, Action: "access.denied"})
	if len(rows) != 2 || rows[0].TargetType != "tool" || rows[0].TargetID != "amp_audit_log" || rows[0].Client != "mcp" {
		t.Fatalf("access.denied (MCP) = %+v", rows)
	}
}

// Membership, role, org and webhook changes carry before/after; webhook
// secrets and URL paths never reach the log.
func TestAuditCoverage_BeforeAfter(t *testing.T) {
	w := newRBACWorld(t)
	rest := newAuditREST(t, w)
	// Org role: dave member → admin.
	if code, out := rest.do(t, "orgadmin", "203.0.113.12", "PUT", fmt.Sprintf("/api/orgs/%d/members/%d", w.org.ID, w.dave.User.ID),
		map[string]any{"role": "admin"}); code != 200 {
		t.Fatalf("set role: %d %v", code, out)
	}
	e := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.orgadmin.User.ID, Action: "org.member.set", Limit: 1})[0]
	if e.Before["role"] != "member" || e.After["role"] != "admin" {
		t.Fatalf("org.member.set before/after = %v / %v", e.Before, e.After)
	}
	// Team role: bob member → lead.
	if code, out := rest.do(t, "orgadmin", "203.0.113.12", "PUT", fmt.Sprintf("/api/teams/%d/members/%d", w.devs.ID, w.bob.User.ID),
		map[string]any{"role": "lead"}); code != 200 {
		t.Fatalf("team role: %d %v", code, out)
	}
	e = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.orgadmin.User.ID, Action: "team.member.set", Limit: 1})[0]
	if e.Before["role"] != "member" || e.After["role"] != "lead" {
		t.Fatalf("team.member.set before/after = %v / %v", e.Before, e.After)
	}
	// Org rename.
	if code, out := rest.do(t, "orgadmin", "203.0.113.12", "PATCH", fmt.Sprintf("/api/orgs/%d", w.org.ID), map[string]any{"name": "Acme Renamed"}); code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	e = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.orgadmin.User.ID, Action: "org.rename", Limit: 1})[0]
	if e.Before["name"] != w.org.Name || e.After["name"] != "Acme Renamed" {
		t.Fatalf("org.rename before/after = %v / %v", e.Before, e.After)
	}

	// Webhooks, through the service as the person.
	prev := service.AllowPrivateWebhooks
	service.AllowPrivateWebhooks = true
	t.Cleanup(func() { service.AllowPrivateWebhooks = prev })
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	p := authz.Principal{UserID: w.orgadmin.User.ID, Email: w.orgadmin.User.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	ctx := context.Background()
	url1, url2 := "http://127.0.0.1:9/hooks/T000/B000/SECRETPATH?token=abc", "http://127.0.0.1:9/other/SECRET2"
	name, events := "siem", []string{"audit.*"}
	hook, err := svc.CreateWebhook(ctx, p, &w.org.ID, nil, service.WebhookChange{Name: &name, URL: &url1, Events: &events})
	if err != nil {
		t.Fatal(err)
	}
	active := false
	if _, err := svc.UpdateWebhook(ctx, p, hook.ID, service.WebhookChange{URL: &url2, Active: &active}); err != nil {
		t.Fatal(err)
	}
	e = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.orgadmin.User.ID, Action: "webhook.update", Limit: 1})[0]
	if e.Before["active"] != true || e.After["active"] != false || e.Before["url"] != "http://127.0.0.1:9/…" || e.After["url"] != "http://127.0.0.1:9/…" {
		t.Fatalf("webhook.update before/after = %v / %v", e.Before, e.After)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), hook.Secret) || strings.Contains(string(b), "token=abc") {
		t.Fatalf("webhook secret material in the audit row: %s", b)
	}

	// Global role.
	rootP := authz.Principal{UserID: w.root.User.ID, Email: w.root.User.Email, Super: true, Channel: auth.ChannelUI}
	if err := svc.SetUserRole(ctx, rootP, w.carol.User.ID, domain.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	e = auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.root.User.ID, Action: "user.role", Limit: 1})[0]
	if fmt.Sprint(e.Before["roles"]) == fmt.Sprint(e.After["roles"]) || !strings.Contains(fmt.Sprint(e.After["roles"]), "admin") {
		t.Fatalf("user.role before/after = %v / %v", e.Before, e.After)
	}
}

// KB deletes, restores and reindexes in any KB, and writes to team and org
// KBs (project KB writes stay in the doc's own history).
func TestAuditCoverage_KB(t *testing.T) {
	w := newRBACWorld(t)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	p := authz.Principal{UserID: w.lead.User.ID, Email: w.lead.User.Email, Channel: auth.ChannelMCP, AuthMethod: auth.AuthOAuthMCP}
	ctx := mcpFrom(w.s, "198.51.100.90", "x")
	team := &service.KB{Scope: kb.Team(w.devs.ID), Kind: kb.ScopeTeam, ID: w.devs.ID, Name: w.devs.Name}
	proj := &service.KB{Scope: kb.Project(w.s.projectID), Kind: kb.ScopeProject, ID: w.s.projectID, Name: "rbac"}
	svc.AuditKB(ctx, p, "kb.write", team, "runbooks/deploy.md", map[string]any{"reason": "every project deploys this way"})
	svc.AuditKB(ctx, p, "kb.write", proj, "notes.md", nil) // not recorded
	svc.AuditKB(ctx, p, "kb.delete", proj, "notes.md", nil)
	svc.AuditKB(ctx, p, "kb.restore", proj, "notes.md", nil)
	svc.AuditKB(ctx, p, "kb.reindex", team, "", nil)
	rows := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.lead.User.ID, Action: "kb."})
	got := map[string]domain.AuditEntry{}
	for _, r := range rows {
		got[r.Action] = r
	}
	if len(rows) != 4 {
		t.Fatalf("kb rows = %d: %+v", len(rows), rows)
	}
	if e := got["kb.write"]; e.OrgID == nil || *e.OrgID != w.org.ID || e.Details["scope"] != "team" || e.Details["path"] != "runbooks/deploy.md" ||
		e.SourceIP != "198.51.100.90" {
		t.Fatalf("kb.write = %+v", e)
	}
	if e := got["kb.delete"]; e.ProjectID == nil || *e.ProjectID != w.s.projectID || e.TargetType != "kb_doc" {
		t.Fatalf("kb.delete = %+v", e)
	}
	if e := got["kb.reindex"]; e.TargetType != "kb" || e.OrgID == nil {
		t.Fatalf("kb.reindex = %+v", e)
	}
	if _, ok := got["kb.restore"]; !ok {
		t.Fatal("no kb.restore")
	}
}

// Sign-in events: one auth.login per Dex session (also across replicas),
// user.create and invite.accept when a first sign-in provisions the
// account, refused sign-ups.
func TestAuditCoverage_SignIn(t *testing.T) {
	w := newRBACWorld(t)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()
	repo := w.s.repo
	sfx := fmt.Sprint(time.Now().UnixNano())
	from := func(ip string) context.Context {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.RemoteAddr = ip + ":1"
		req.Header.Set("User-Agent", "Mozilla/5.0")
		return reqctx.WithInfo(ctx, reqctx.Capture(req, reqctx.ListenerREST, reqctx.NewResolver(nil)))
	}

	// A new person signs in with a pending invite.
	email := "newbie-" + sfx + "@rbac.test"
	if _, err := repo.CreateInvite(ctx, domain.Invite{Email: email, OrgID: w.org.ID, OrgRole: domain.OrgRoleMember, TeamID: &w.devs.ID,
		TeamRole: domain.TeamRoleLead, InvitedBy: &w.orgadmin.User.ID}); err != nil {
		t.Fatal(err)
	}
	u, pv, err := repo.ProvisionUserFromClaims(ctx, "sub-newbie-"+sfx, email, "Newbie", nil)
	if err != nil || !pv.Created || len(pv.Invites) != 1 {
		t.Fatalf("provision: %v %+v", err, pv)
	}
	ident := &auth.Identity{Subject: u.Subject, Email: u.Email, User: u, Listener: "rest", Channel: auth.ChannelUI,
		AuthMethod: auth.AuthDexSession, SessionID: "sess-newbie-" + sfx}
	svc.RecordAuthEvent(from("203.0.113.13"), auth.Event{Kind: auth.EventProvisioned, Ident: ident, Provisioned: pv})
	for range 3 {
		svc.RecordAuthEvent(from("203.0.113.13"), auth.Event{Kind: auth.EventLogin, Ident: ident})
	}
	created := auditRows(t, repo, domain.AuditFilter{ActorUserID: u.ID, Action: "user.create"})
	if len(created) != 1 || created[0].SourceIP != "203.0.113.13" || created[0].Details["initial_role"] != "member" {
		t.Fatalf("user.create = %+v", created)
	}
	acc := auditRows(t, repo, domain.AuditFilter{ActorUserID: u.ID, Action: "invite.accept"})
	if len(acc) != 1 || acc[0].After["team_role"] != "lead" || acc[0].After["org_role"] != "member" || acc[0].OrgID == nil || *acc[0].OrgID != w.org.ID {
		t.Fatalf("invite.accept = %+v", acc)
	}
	logins := auditRows(t, repo, domain.AuditFilter{ActorUserID: u.ID, Action: "auth.login"})
	if len(logins) != 1 || logins[0].SessionID != ident.SessionID || logins[0].Client != "web" {
		t.Fatalf("auth.login = %+v", logins)
	}
	// Another replica already recorded this session: none written here.
	other := *ident
	other.SessionID = "sess-elsewhere-" + sfx
	if _, err := repo.InsertAudit(ctx, domain.AuditEntry{Action: "auth.login", ActorUserID: &u.ID, ActorEmail: u.Email, SessionID: other.SessionID}); err != nil {
		t.Fatal(err)
	}
	svc.RecordAuthEvent(from("203.0.113.13"), auth.Event{Kind: auth.EventLogin, Ident: &other})
	if n := len(auditRows(t, repo, domain.AuditFilter{ActorUserID: u.ID, Action: "auth.login"})); n != 2 {
		t.Fatalf("auth.login across replicas: %d rows, want 2 (the seeded one and the first)", n)
	}

	// A refused sign-up: anonymous, with the email the token claimed.
	svc.RecordAuthEvent(from("203.0.113.14"), auth.Event{Kind: auth.EventSignupRefused, Email: "stranger-" + sfx + "@else.test", Reason: "domain not allowed"})
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var actorType, outcome, ip string
	if err := db.QueryRow(ctx, `SELECT actor_type, outcome, host(source_ip) FROM audit_log WHERE action = 'auth.signup.refused' AND actor_email = $1`,
		"stranger-"+sfx+"@else.test").Scan(&actorType, &outcome, &ip); err != nil {
		t.Fatal(err)
	}
	if actorType != "anonymous" || outcome != "denied" || ip != "203.0.113.14" {
		t.Fatalf("auth.signup.refused = %s %s %s", actorType, outcome, ip)
	}
}

// API tokens through the real verifier: the first use from a new address
// is recorded (token.use.new_ip), repeats aren't; a rejected token is
// recorded with its prefix only.
func TestAuditCoverage_APITokens(t *testing.T) {
	w := newRBACWorld(t)
	repo := w.s.repo
	ctx := context.Background()
	token, hash, prefix, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := repo.CreateAPIToken(ctx, w.carol.User.ID, "ci", hash, prefix, &w.carol.User.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := auth.NewAPITokenVerifier(repo)
	v.OnEvent = service.New(repo, w.s.reg, w.s.kb).RecordAuthEvent
	h := reqctx.Middleware(reqctx.ListenerREST, reqctx.NewResolver(nil))(v.Middleware(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {})))
	call := func(ip, bearer string) int {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.RemoteAddr = ip + ":1"
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, ip := range []string{"203.0.113.15", "203.0.113.15", "203.0.113.15", "203.0.113.16"} {
		if code := call(ip, token); code != 200 {
			t.Fatalf("token request: %d", code)
		}
	}
	rows := auditRows(t, repo, domain.AuditFilter{ActorUserID: w.carol.User.ID, Action: "token.use.new_ip"})
	if len(rows) != 2 || rows[0].SourceIP != "203.0.113.16" || rows[1].SourceIP != "203.0.113.15" || rows[0].TokenID == nil ||
		*rows[0].TokenID != int64(tok.ID) || rows[0].AuthMethod != "api_token" {
		t.Fatalf("token.use.new_ip = %+v", rows)
	}

	bad := "amp_" + strings.Repeat("Z", 43)
	if code := call("203.0.113.17", bad); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var details, actorType, outcome string
	if err := db.QueryRow(ctx, `SELECT details::text, actor_type, outcome FROM audit_log WHERE action = 'auth.token.fail' AND source_ip = '203.0.113.17'::inet
		ORDER BY id DESC LIMIT 1`).Scan(&details, &actorType, &outcome); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"token_prefix": "amp_ZZZZZZ"`) || strings.Contains(details, bad) || actorType != "anonymous" || outcome != "denied" {
		t.Fatalf("auth.token.fail = %s %s %s", details, actorType, outcome)
	}
}

// audit.export is recorded with its filter and row count (the export
// endpoint itself ships in #2431).
func TestAuditCoverage_AuditExport(t *testing.T) {
	w := newRBACWorld(t)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	p := authz.Principal{UserID: w.orgadmin.User.ID, Email: w.orgadmin.User.Email, Channel: auth.ChannelUI}
	svc.RecordAuditExport(context.Background(), p, domain.AuditFilter{OrgID: w.org.ID, Action: "plan."}, "csv", 12)
	rows := auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.orgadmin.User.ID, Action: "audit.export"})
	if len(rows) != 1 || rows[0].Details["rows"] != 12.0 || rows[0].Details["format"] != "csv" || rows[0].OrgID == nil {
		t.Fatalf("audit.export = %+v", rows)
	}
}

// The KB tools record their changes (needs the KB: TEST_TYPESENSE_URL).
func TestAuditCoverage_KBTools(t *testing.T) {
	w := newRBACWorld(t)
	if w.s.kb == nil {
		t.Skip("no knowledge base configured (TEST_TYPESENSE_URL)")
	}
	lead := w.s.as(w.lead)
	ctx := mcpFrom(lead, "198.51.100.91", "x")
	team := map[string]any{"scope": "team", "team_id": float64(w.devs.ID), "project_id": float64(w.s.projectID)}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range team {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	if msg := mcpCall(t, lead, ctx, "amp_kb_write", with(map[string]any{"path": "runbooks/audit.md", "title": "Audit", "content": "# Audit\n\nbody",
		"tags": []any{"a", "b", "c"}, "reason": "every project this team runs deploys the same way; the user decided"})); msg != "" {
		t.Fatalf("amp_kb_write: %s", msg)
	}
	if msg := mcpCall(t, lead, ctx, "amp_kb_delete", with(map[string]any{"path": "runbooks/audit.md"})); msg != "" {
		t.Fatalf("amp_kb_delete: %s", msg)
	}
	if msg := mcpCall(t, lead, ctx, "amp_kb_restore", with(map[string]any{"path": "runbooks/audit.md"})); msg != "" {
		t.Fatalf("amp_kb_restore: %s", msg)
	}
	got := map[string]bool{}
	for _, r := range auditRows(t, w.s.repo, domain.AuditFilter{ActorUserID: w.lead.User.ID, Action: "kb."}) {
		got[r.Action] = true
		if r.SourceIP != "198.51.100.91" || r.Details["path"] != "runbooks/audit.md" || r.OrgID == nil {
			t.Fatalf("%s = %+v", r.Action, r)
		}
	}
	if !got["kb.write"] || !got["kb.delete"] || !got["kb.restore"] {
		t.Fatalf("kb rows: %v", got)
	}
}
