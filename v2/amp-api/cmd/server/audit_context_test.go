package main

// Audit log v2 capture (#2427, KB decisions/audit-log-v2.md): every write
// path records the trusted client IP, user agent, client, request id, actor
// type, auth method, token/session id and outcome; spoofed headers from an
// untrusted hop never become the client IP; rows written before v2 still
// read back.

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
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/reqctx"
	"github.com/simstech/amp-api/internal/service"
)

// lastAudit returns the newest entry for action by actor.
func lastAudit(t *testing.T, repo *repository.Repo, actorID int, action string) domain.AuditEntry {
	t.Helper()
	got, err := repo.ListAudit(context.Background(), domain.AuditFilter{ActorUserID: actorID, Action: action, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("no %s entry by user %d", action, actorID)
	}
	return got[0]
}

// The REST listener as main() builds it (reqctx outermost, then auth), with
// the auth middleware stood in for by the identity the test names.
func TestAuditContext_REST(t *testing.T) {
	w := newRBACWorld(t)
	mux := http.NewServeMux()
	api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb).Register(mux)
	trusted, err := reqctx.ParseTrusted("127.0.0.0/8, ::1/128, 10.244.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]*auth.Identity{"root": w.root, "alice": w.alice}
	var h http.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c := *ids[r.Header.Get("X-Test-As")]
		c.Listener, c.Channel, c.AuthMethod, c.SessionID = "rest", auth.ChannelUI, auth.AuthDexSession, "sess-123"
		if r.Header.Get("X-Test-Token") != "" {
			c.Channel, c.APIToken, c.AuthMethod, c.SessionID, c.TokenID = auth.ChannelToken, true, auth.AuthAPIToken, "", 4242
		}
		mux.ServeHTTP(rw, r.WithContext(auth.WithIdentity(r.Context(), &c)))
	})
	srv := httptest.NewServer(reqctx.Middleware(reqctx.ListenerREST, reqctx.NewResolver(trusted))(h))
	t.Cleanup(srv.Close)

	post := func(who string, hdr map[string]string, path string, body any) (int, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-As", who)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-Request-Id")
	}

	// The test client stands in for Envoy (127.0.0.1 is trusted), so its
	// X-Amp-Client-Ip is the client; X-Forwarded-For is only kept raw, and
	// the headers AMP never trusts are ignored.
	slug := fmt.Sprintf("audit-ctx-%d", time.Now().UnixNano())
	code, rid := post("root", map[string]string{
		"X-Amp-Client-Ip":  "203.0.113.7",
		"X-Forwarded-For":  "6.6.6.6, 203.0.113.7, 10.244.1.5",
		"CF-Connecting-IP": "6.6.6.6",
		"X-Real-IP":        "6.6.6.6",
		"User-Agent":       "Mozilla/5.0 (audit test)",
		"X-Request-Id":     "11111111-2222-4333-8444-555555555555",
		"Traceparent":      "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}, "/api/orgs", map[string]any{"name": "Audit Ctx", "slug": slug})
	if code != 201 || rid != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("create org: %d, request id %q", code, rid)
	}
	e := lastAudit(t, w.s.repo, w.root.User.ID, "org.create")
	if e.SourceIP != "203.0.113.7" || e.UserAgent != "Mozilla/5.0 (audit test)" || e.RequestID != rid ||
		e.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || e.Client != "web" || e.ActorType != "human" ||
		e.AuthMethod != "dex_session" || e.SessionID != "sess-123" || e.TokenID != nil || e.Outcome != "success" || e.SchemaVersion != 2 {
		t.Fatalf("org.create entry = %+v", e)
	}
	if e.Details["xff"] != "6.6.6.6, 203.0.113.7, 10.244.1.5" || e.Details["channel"] != "ui" {
		t.Fatalf("details = %v", e.Details)
	}

	// An API token, a garbage request id (replaced), no forwarding header:
	// the peer itself is the client.
	slug2 := slug + "-2"
	code, rid = post("root", map[string]string{"X-Test-Token": "1", "X-Request-Id": "bad id;drop", "User-Agent": "curl/8"},
		"/api/orgs", map[string]any{"name": "Audit Ctx 2", "slug": slug2})
	if code != 201 || rid == "" || rid == "bad id;drop" {
		t.Fatalf("create org via token: %d, request id %q", code, rid)
	}
	e = lastAudit(t, w.s.repo, w.root.User.ID, "org.create")
	if e.SourceIP != "127.0.0.1" || e.RequestID != rid || e.Client != "api" || e.AuthMethod != "api_token" ||
		e.TokenID == nil || *e.TokenID != 4242 || e.SessionID != "" || e.UserAgent != "curl/8" {
		t.Fatalf("token org.create entry = %+v", e)
	}
}

// MCP tool calls: the request context set by the MCP listener's middleware
// reaches the audit row through the principal.
func TestAuditContext_MCP(t *testing.T) {
	w := newRBACWorld(t)
	c := *w.alice
	c.Listener, c.Channel, c.AuthMethod, c.SessionID = "mcp", auth.ChannelMCP, auth.AuthOAuthMCP, "sess-mcp"
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.RemoteAddr = "[2001:db8::42]:55555" // untrusted peer: its XFF is ignored
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("User-Agent", "AMP-Harness/0.4")
	info := reqctx.Capture(req, reqctx.ListenerMCP, reqctx.NewResolver(nil))
	ctx := reqctx.WithInfo(auth.WithIdentity(context.Background(), &c), info)

	sfx := fmt.Sprint(time.Now().UnixNano())
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "amp_create_project", "arguments": map[string]any{"name": "audit mcp", "code": "am-" + sfx}}})
	out, _ := json.Marshal(w.s.srv.HandleMessage(ctx, raw))
	if strings.Contains(string(out), `"isError":true`) || strings.Contains(string(out), `"error"`) {
		t.Fatalf("amp_create_project: %s", out)
	}
	e := lastAudit(t, w.s.repo, w.alice.User.ID, "project.create")
	if e.SourceIP != "2001:db8::42" || e.UserAgent != "AMP-Harness/0.4" || e.Client != "harness" || e.RequestID != info.RequestID ||
		e.ActorType != "human" || e.AuthMethod != "oauth_mcp" || e.SessionID != "sess-mcp" || e.Outcome != "success" {
		t.Fatalf("project.create entry = %+v", e)
	}
	if _, ok := e.Details["xff"]; !ok {
		t.Fatalf("raw XFF not kept: %v", e.Details)
	}
}

// A background job has no client: system actor and client, no address, and
// details.ip_reason says why.
func TestAuditContext_SystemJob(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	art, err := s.repo.CreateArtifact(ctx, domain.Artifact{ProjectID: s.projectID, Title: "expiring", Kind: "html"},
		[]byte("<p>x</p>"), "text/html", "", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.New(s.repo, s.reg, s.kb).PurgeExpiredArtifacts(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.repo.TargetAudit(ctx, "artifact", fmt.Sprint(art.ID), 5)
	if err != nil || len(got) == 0 {
		t.Fatalf("no artifact.expire entry: %v %v", got, err)
	}
	e := got[0]
	if e.Action != "artifact.expire" || e.ActorType != "system" || e.Client != "system" || e.AuthMethod != "none" ||
		e.SourceIP != "" || e.Details["ip_reason"] == nil || e.Outcome != "success" || e.SchemaVersion != 2 {
		t.Fatalf("artifact.expire entry = %+v", e)
	}
}

// Rows written before v2 (the old column list, as m1's relay_record_audit
// and older replicas still insert) read back as schema_version 1 with
// empty context; a v2 row round-trips every field.
func TestAuditContext_LegacyAndRoundTrip(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	action := fmt.Sprintf("test.legacy.%d", time.Now().UnixNano())
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, `INSERT INTO audit_log (actor_email, action, target_type, target_id, summary, details)
		VALUES ('old@x', $1, 'thing', '1', 'old row', '{"channel":"ui"}')`, action); err != nil {
		t.Fatal(err)
	}
	got, err := s.repo.ListAudit(ctx, domain.AuditFilter{Action: action})
	if err != nil || len(got) != 1 {
		t.Fatalf("legacy row: %v %v", got, err)
	}
	if e := got[0]; e.SchemaVersion != 1 || e.Outcome != "success" || e.ActorType != "" || e.SourceIP != "" || e.TokenID != nil || e.Before != nil {
		t.Fatalf("legacy row = %+v", e)
	}
	// The v1 JSON shape is a subset of what's returned now.
	b, _ := json.Marshal(got[0])
	for _, k := range []string{`"id"`, `"at"`, `"actor_email"`, `"action"`, `"summary"`, `"details"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("legacy JSON lacks %s: %s", k, b)
		}
	}

	tok, behalf := int64(77), 5
	in := domain.AuditEntry{Action: action + ".v2", ActorEmail: "n@x", TargetType: "thing", TargetID: "2", Summary: "new",
		ActorType: "agent", AuthMethod: "api_token", TokenID: &tok, SessionID: "s", OnBehalfOfUserID: &behalf, AgentProfile: "amp:worker",
		TargetName: "Thing Two", Outcome: "denied", Reason: "no", SourceIP: "2001:db8::9", UserAgent: "ua", Client: "mcp",
		RequestID: "r", TraceID: "t", Before: map[string]any{"a": 1.0}, After: map[string]any{"a": 2.0}}
	if _, err := s.repo.InsertAudit(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err = s.repo.ListAudit(ctx, domain.AuditFilter{Action: action + ".v2"})
	if err != nil || len(got) != 1 {
		t.Fatalf("v2 row: %v %v", got, err)
	}
	e := got[0]
	if e.SchemaVersion != 2 || e.ActorType != "agent" || e.AuthMethod != "api_token" || *e.TokenID != 77 || e.SessionID != "s" ||
		*e.OnBehalfOfUserID != 5 || e.AgentProfile != "amp:worker" || e.TargetName != "Thing Two" || e.Outcome != "denied" ||
		e.Reason != "no" || e.SourceIP != "2001:db8::9" || e.UserAgent != "ua" || e.Client != "mcp" || e.RequestID != "r" ||
		e.TraceID != "t" || e.Before["a"] != 1.0 || e.After["a"] != 2.0 {
		t.Fatalf("v2 row = %+v", e)
	}
}

// A process newer than its database (authadmin doesn't migrate, so it can
// start before amp-api applies 031) still records the change, in the v1
// shape, instead of losing it.
func TestAuditContext_InsertBeforeMigration(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	var pre []repository.Migration
	for _, m := range migrations {
		if m.Version < "031" {
			pre = append(pre, m)
		}
	}
	if err := repo.RunMigrations(ctx, pre); err != nil {
		t.Fatal(err)
	}
	e, err := repo.InsertAudit(ctx, domain.AuditEntry{Action: "user.password.reset", ActorEmail: "a@x", SourceIP: "203.0.113.7", Client: "web"})
	if err != nil {
		t.Fatalf("insert against a pre-031 schema: %v", err)
	}
	if e.SchemaVersion != 1 || e.ID == 0 {
		t.Fatalf("entry = %+v", e)
	}
	// Then the migration applies cleanly on top, and the row reads as v1.
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var v int
	var outcome string
	if err := db.QueryRow(ctx, `SELECT schema_version, outcome FROM audit_log WHERE id = $1`, e.ID).Scan(&v, &outcome); err != nil || v != 1 || outcome != "success" {
		t.Fatalf("after 031: version %d outcome %q err %v", v, outcome, err)
	}
}
