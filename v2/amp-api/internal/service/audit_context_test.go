package service

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/reqctx"
)

func reqInfo(listener, ip, ua string) *reqctx.Info {
	return &reqctx.Info{SourceIP: netip.MustParseAddr(ip), UserAgent: ua, RequestID: "req-1", TraceID: "abc", Listener: listener}
}

// Every row gets the v2 context from the principal and the request.
func TestFillAuditContext(t *testing.T) {
	managedBy := 9
	cases := []struct {
		name string
		p    authz.Principal
		ctx  context.Context
		want domain.AuditEntry
	}{
		{"person in the web UI",
			authz.Principal{UserID: 1, Email: "a@x", Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession, SessionID: "s1",
				Req: reqInfo(reqctx.ListenerREST, "203.0.113.7", "Mozilla/5.0")},
			context.Background(),
			domain.AuditEntry{ActorType: "human", AuthMethod: "dex_session", SessionID: "s1", Client: "web", SourceIP: "203.0.113.7",
				UserAgent: "Mozilla/5.0", RequestID: "req-1", TraceID: "abc", Outcome: "success"}},
		{"agent with an API token over MCP",
			authz.Principal{UserID: 2, Agent: true, ViaToken: true, AuthMethod: auth.AuthAPIToken, TokenID: 44, ManagedBy: &managedBy,
				Req: reqInfo(reqctx.ListenerMCP, "2001:db8::1", "claude-code/2")},
			context.Background(),
			domain.AuditEntry{ActorType: "agent", AuthMethod: "api_token", Client: "mcp", SourceIP: "2001:db8::1",
				UserAgent: "claude-code/2", RequestID: "req-1", TraceID: "abc", Outcome: "success"}},
		{"the AMP harness",
			authz.Principal{UserID: 3, AuthMethod: auth.AuthOAuthMCP, Req: reqInfo(reqctx.ListenerMCP, "198.51.100.1", "AMP-Harness/0.3")},
			context.Background(),
			domain.AuditEntry{ActorType: "human", AuthMethod: "oauth_mcp", Client: "harness", SourceIP: "198.51.100.1",
				UserAgent: "AMP-Harness/0.3", RequestID: "req-1", TraceID: "abc", Outcome: "success"}},
		{"API token on REST",
			authz.Principal{UserID: 4, ViaToken: true, AuthMethod: auth.AuthAPIToken, TokenID: 7, Req: reqInfo(reqctx.ListenerREST, "198.51.100.2", "curl/8")},
			context.Background(),
			domain.AuditEntry{ActorType: "human", AuthMethod: "api_token", Client: "api", SourceIP: "198.51.100.2",
				UserAgent: "curl/8", RequestID: "req-1", TraceID: "abc", Outcome: "success"}},
		{"authadmin from the Users page",
			authz.Principal{UserID: 5, AuthMethod: auth.AuthDexSession, Req: reqInfo(reqctx.ListenerAuthAdmin, "198.51.100.3", "Mozilla/5.0")},
			context.Background(),
			domain.AuditEntry{ActorType: "human", AuthMethod: "dex_session", Client: "web", SourceIP: "198.51.100.3",
				UserAgent: "Mozilla/5.0", RequestID: "req-1", TraceID: "abc", Outcome: "success"}},
		{"request context from ctx when the principal has none",
			authz.Principal{UserID: 6, AuthMethod: auth.AuthDexSession},
			reqctx.WithInfo(context.Background(), reqInfo(reqctx.ListenerREST, "203.0.113.9", "ua")),
			domain.AuditEntry{ActorType: "human", AuthMethod: "dex_session", Client: "web", SourceIP: "203.0.113.9",
				UserAgent: "ua", RequestID: "req-1", TraceID: "abc", Outcome: "success"}},
		{"background job",
			authz.Principal{Email: "AMP"},
			context.Background(),
			domain.AuditEntry{ActorType: "system", AuthMethod: "none", Client: "system", Outcome: "success"}},
		{"unauthenticated request",
			authz.Principal{Req: reqInfo(reqctx.ListenerMCPOAuth, "198.51.100.4", "x")},
			context.Background(),
			domain.AuditEntry{ActorType: "anonymous", AuthMethod: "none", Client: "mcp", SourceIP: "198.51.100.4",
				UserAgent: "x", RequestID: "req-1", TraceID: "abc", Outcome: "success"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := domain.AuditEntry{Action: "x.y"}
			fillAuditContext(c.ctx, c.p, &e)
			g, w := e, c.want
			if g.SchemaVersion != domain.AuditSchemaVersion {
				t.Errorf("schema_version = %d", g.SchemaVersion)
			}
			if g.ActorType != w.ActorType || g.AuthMethod != w.AuthMethod || g.SessionID != w.SessionID || g.Client != w.Client ||
				g.SourceIP != w.SourceIP || g.UserAgent != w.UserAgent || g.RequestID != w.RequestID || g.TraceID != w.TraceID ||
				g.Outcome != w.Outcome {
				t.Errorf("got  %+v\nwant %+v", g, w)
			}
			if (c.p.TokenID != 0) != (g.TokenID != nil) || (g.TokenID != nil && *g.TokenID != c.p.TokenID) {
				t.Errorf("token_id = %v, principal's %d", g.TokenID, c.p.TokenID)
			}
			if c.p.Agent && (g.OnBehalfOfUserID == nil || *g.OnBehalfOfUserID != managedBy) {
				t.Errorf("agent's on_behalf_of = %v", g.OnBehalfOfUserID)
			}
			if g.SourceIP == "" && g.Details["ip_reason"] == nil {
				t.Errorf("no source_ip and no ip_reason: %+v", g.Details)
			}
		})
	}
}

// What the caller set is kept; the reason is clipped; before/after redacted.
func TestFillAuditContext_KeepsCallerValues(t *testing.T) {
	e := domain.AuditEntry{Action: "plan.approve.denied", Outcome: "denied", Client: "relay", Reason: strings.Repeat("r", 900),
		Before:  map[string]any{"url": "https://hooks.slack.com/services/T0/B0/secret", "secret": "s3cr3t"},
		Details: map[string]any{"ip_reason": "custom"}}
	fillAuditContext(context.Background(), authz.Principal{UserID: 1}, &e)
	if e.Outcome != "denied" || e.Client != "relay" || len(e.Reason) != maxAuditReason || e.Details["ip_reason"] != "custom" {
		t.Fatalf("%+v", e)
	}
	if e.Before["secret"] != redactedValue || strings.Contains(e.Before["url"].(string), "services") {
		t.Fatalf("before not redacted: %v", e.Before)
	}
}

func TestRedact(t *testing.T) {
	in := map[string]any{
		"name":          "hook",
		"Secret":        "s",
		"auth_header":   "Bearer x",
		"api_key":       "k",
		"password_hash": "h",
		"url":           "https://example.com/hook/abc?key=1",
		"plain":         "not a url ?key=1",
		"nested":        map[string]any{"token": "t", "ok": 1, "urls": []any{"http://u:p@example.com/x"}},
	}
	out := redact(in)
	for _, k := range []string{"Secret", "auth_header", "api_key", "password_hash"} {
		if out[k] != redactedValue {
			t.Errorf("%s = %v", k, out[k])
		}
	}
	if out["name"] != "hook" || out["plain"] != "not a url ?key=1" {
		t.Errorf("redacted too much: %v", out)
	}
	if out["url"] != "https://example.com/…" {
		t.Errorf("url = %v", out["url"])
	}
	n := out["nested"].(map[string]any)
	if n["token"] != redactedValue || n["ok"] != 1 || n["urls"].([]any)[0] != "http://example.com/…" {
		t.Errorf("nested = %v", n)
	}
	if in["Secret"] != "s" {
		t.Error("redact changed its input")
	}
	if redact(nil) != nil {
		t.Error("nil became non-nil")
	}
}

func TestDeniedAction(t *testing.T) {
	for in, want := range map[string]string{
		"plan.approve":        "plan.approve.denied",
		"task.dispatch":       "task.dispatch.denied",
		"access.denied":       "access.denied",
		"auth.token.fail":     "auth.token.fail",
		"auth.signup.refused": "auth.signup.refused",
	} {
		if got := deniedAction(in); got != want {
			t.Errorf("deniedAction(%q) = %q, want %q", in, got, want)
		}
	}
}

// One refusal row per key per window; the next one written says how many
// were skipped.
func TestDeniedAllow(t *testing.T) {
	key := "test|" + t.Name()
	t0 := time.Now()
	if ok, n := deniedAllow(key, t0); !ok || n != 0 {
		t.Fatalf("first = %v %d", ok, n)
	}
	for i := 1; i <= 3; i++ {
		if ok, _ := deniedAllow(key, t0.Add(time.Duration(i)*time.Minute)); ok {
			t.Fatalf("repeat %d inside the window was written", i)
		}
	}
	if ok, _ := deniedAllow(key+"-other-target", t0); !ok {
		t.Fatal("a different key was suppressed")
	}
	if ok, n := deniedAllow(key, t0.Add(deniedWindow+time.Second)); !ok || n != 3 {
		t.Fatalf("after the window = %v %d, want written with 3 suppressed", ok, n)
	}
}
