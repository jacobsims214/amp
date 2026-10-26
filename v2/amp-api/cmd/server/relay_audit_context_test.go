package main

// Relay audit rows carry request context (AMP #2433): relay_record_audit's
// five-argument version (migration 036) takes the client IP, user agent,
// client, request id and sign-in method the relay resolved, validates them
// as strictly as the rest of the entry, and keeps every #2399 check.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// Run as the relay's restricted role: forged context is refused, the old
// four-argument version still works, and accepted rows carry the context.
func TestRelayRole_AuditContext(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	bobs := e.register(t, "bob")
	_, dsn := relayRole(t, e.w.s.repo)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	alice := e.w.alice.User.ID
	record5 := func(actor, session any, action, details, rc string) (int64, error) {
		var id int64
		err := conn.QueryRow(ctx, `SELECT audit_id FROM relay_record_audit($1, $2, $3, $4::jsonb, $5::jsonb)`,
			actor, session, action, details, rc).Scan(&id)
		return id, err
	}
	good := `{"ip":"203.0.113.7","user_agent":"amp-harness/0.4.0","client":"relay",` +
		`"request_id":"0f8a6c2e-1b2d-4e5f-9a8b-7c6d5e4f3a2b","auth_method":"oauth_mcp","session_id":"0123456789abcdef0123456789abcdef"}`

	refused := []struct{ name, actor, session, action, details, rc string }{
		// #2399's checks hold on the new version.
		{"an amp-api event", "alice", "s", "task.completed", `{}`, good},
		{"amp-api's own remote action", "alice", "s", domain.AuditRemoteKill, `{}`, good},
		{"someone else's session", "alice", "bob", domain.AuditRemoteAttach, `{}`, good},
		{"an unknown detail", "alice", "s", domain.AuditRemoteAttach, `{"ip_reason":"x"}`, good},
		// Forged context.
		{"another client", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"client":"web"}`},
		{"a network, not an address", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"ip":"6.6.6.0/24"}`},
		{"an IPv6 network", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"ip":"2001:db8::/64"}`},
		{"not an address", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"ip":"6.6.6.6, 10.0.0.1"}`},
		{"an address as a number", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"ip":101}`},
		{"a long user agent", "alice", "s", domain.AuditRemoteAttach, `{}`,
			fmt.Sprintf(`{"user_agent":%q}`, strings.Repeat("u", 513))},
		{"a request id with spaces", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"request_id":"a b"}`},
		{"a long request id", "alice", "s", domain.AuditRemoteAttach, `{}`,
			fmt.Sprintf(`{"request_id":%q}`, strings.Repeat("a", 129))},
		{"an API token sign-in", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"auth_method":"api_token"}`},
		{"a sign-in for a sweep", "", "s", domain.AuditRemoteExpire, `{}`, `{"auth_method":"oauth_mcp"}`},
		{"a session id that isn't hex", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"session_id":"x'; --"}`},
		{"an unknown member", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"source_ip":"1.2.3.4"}`},
		{"an outcome", "alice", "s", domain.AuditRemoteAttach, `{}`, `{"outcome":"success"}`},
		{"context not an object", "alice", "s", domain.AuditRemoteAttach, `{}`, `["relay"]`},
	}
	who := map[string]any{"alice": alice, "": nil}
	sess := map[string]any{"s": s.id, "bob": bobs.id}
	for _, c := range refused {
		if _, err := record5(who[c.actor], sess[c.session], c.action, c.details, c.rc); err == nil {
			t.Errorf("%s: relay_record_audit accepted it", c.name)
		}
	}

	// A good entry carries the context; the function sets the rest.
	id, err := record5(alice, s.id, domain.AuditRemoteAttach, `{"op":"attach"}`, good)
	if err != nil {
		t.Fatalf("a good entry: %v", err)
	}
	a := auditByID(t, e, domain.AuditRemoteAttach, s.id, id)
	if a.SourceIP != "203.0.113.7" || a.UserAgent != "amp-harness/0.4.0" || a.Client != domain.AuditClientRelay ||
		a.RequestID != "0f8a6c2e-1b2d-4e5f-9a8b-7c6d5e4f3a2b" || a.AuthMethod != "oauth_mcp" ||
		a.SessionID != "0123456789abcdef0123456789abcdef" || a.ActorType != domain.AuditActorHuman ||
		a.SchemaVersion != 2 || a.Outcome != domain.AuditOutcomeSuccess || a.ActorEmail != e.w.alice.Email ||
		a.Details["session_id"] != s.id {
		t.Fatalf("recorded entry: %+v", a)
	}
	// No context at all is allowed (a sweep), and is a system row.
	if id, err = record5(nil, s.id, domain.AuditRemoteExpire, `{"op":"end","reason":"idle"}`, `null`); err != nil {
		t.Fatalf("a sweep's entry: %v", err)
	}
	if a := auditByID(t, e, domain.AuditRemoteExpire, s.id, id); a.ActorType != domain.AuditActorSystem ||
		a.Client != domain.AuditClientRelay || a.SourceIP != "" || a.AuthMethod != "none" {
		t.Fatalf("sweep entry: %+v", a)
	}
	// A refusal the relay records is a denied row.
	if id, err = record5(alice, s.id, domain.AuditRemoteRefused, `{"code":"x"}`, `{"client":"relay"}`); err != nil {
		t.Fatalf("a refusal: %v", err)
	}
	if a := auditByID(t, e, domain.AuditRemoteRefused, s.id, id); a.Outcome != domain.AuditOutcomeDenied {
		t.Fatalf("refusal entry: %+v", a)
	}

	// The four-argument version (older relays) still works, with no context.
	var old int64
	if err := conn.QueryRow(ctx, `SELECT audit_id FROM relay_record_audit($1, $2, $3, $4::jsonb)`,
		alice, s.id, domain.AuditRemoteDetach, `{"op":"detach"}`).Scan(&old); err != nil {
		t.Fatalf("the four-argument version: %v", err)
	}
	if a := auditByID(t, e, domain.AuditRemoteDetach, s.id, old); a.SourceIP != "" || a.Client != domain.AuditClientRelay ||
		a.ActorType != domain.AuditActorHuman {
		t.Fatalf("four-argument entry: %+v", a)
	}
	if _, err := conn.Exec(ctx, `SELECT relay_record_audit($1, $2, $3, $4::jsonb)`,
		alice, s.id, "task.completed", `{}`); err == nil {
		t.Fatal("the four-argument version accepted an amp-api event")
	}

	// Neither version is executable by everyone.
	for _, sig := range []string{"relay_record_audit(int, text, text, jsonb)", "relay_record_audit(int, text, text, jsonb, jsonb)"} {
		var public bool
		if err := conn.QueryRow(ctx, `SELECT has_function_privilege('public', $1, 'EXECUTE')`, sig).Scan(&public); err != nil || public {
			t.Fatalf("PUBLIC may execute %s (%v)", sig, err)
		}
	}
}

// End to end through the relay's public listener: the entries it records
// carry the caller's address, user agent and request id.
func TestRelay_AuditRowsCarryRequestContext(t *testing.T) {
	e := newRelayEnv(t, nil)
	k := newRCKey(t)
	const reqID = "5b0c9e1a-0d3c-4b7e-8f21-a1b2c3d4e5f6"
	st, body, hdr := e.do(t, "POST", "/relay/v1/sessions", e.registration(k), as("alice"),
		func(r *http.Request) {
			r.Header.Set("User-Agent", "amp-harness/9.9.9 (test)")
			r.Header.Set("X-Request-Id", reqID)
			// From an untrusted peer, a forwarded address is ignored.
			r.Header.Set("X-Forwarded-For", "6.6.6.6")
		})
	wantStatus(t, "register", st, body, 201, "")
	if got := hdr.Get("X-Request-Id"); got != reqID {
		t.Fatalf("X-Request-Id echoed %q", got)
	}
	id := body["session_id"].(string)
	got := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteEnable, id)
	if len(got) != 1 {
		t.Fatalf("enable entries: %+v", got)
	}
	a := got[0]
	if a.SourceIP != "127.0.0.1" || a.UserAgent != "amp-harness/9.9.9 (test)" || a.RequestID != reqID ||
		a.Client != domain.AuditClientRelay || a.ActorType != domain.AuditActorHuman || a.SchemaVersion != 2 {
		t.Fatalf("enable entry: %+v", a)
	}
	if _, set := a.Details["ip_reason"]; set {
		t.Fatalf("enable entry has ip_reason: %+v", a.Details)
	}
}

func auditByID(t *testing.T, e *relayEnv, action, session string, id int64) domain.AuditEntry {
	t.Helper()
	for _, a := range remoteAudit(t, e.w.s.repo, e.w.org.ID, action, session) {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no %s entry %d", action, id)
	return domain.AuditEntry{}
}
