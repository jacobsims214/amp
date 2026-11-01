package main

// Remote control hardening from the #2394 re-review (AMP #2399): the relay
// records audit entries only through relay_record_audit and amp-api renders
// their webhooks (N3), refresh-proof nonces are checked across replicas (N6),
// and losing access to the project ends a session (N7).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/relay"
	"github.com/simstech/amp-api/internal/webhook"
)

// N3: the relay's role can't write audit rows or webhook deliveries
// directly, and relay_record_audit accepts only the relay's own remote.*
// entries, about a session the actor owns, with known scalar details. The
// function fills everything else from the session row.
func TestRelayRole_AuditOnlyThroughFunction(t *testing.T) {
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

	alice, bob := e.w.alice.User.ID, e.w.bob.User.ID
	record := func(actor any, session any, action string, details string) error {
		var id int64
		return conn.QueryRow(ctx, `SELECT audit_id FROM relay_record_audit($1, $2, $3, $4::jsonb)`,
			actor, session, action, details).Scan(&id)
	}
	refused := []struct {
		name            string
		actor, session  any
		action, details string
	}{
		{"an amp-api event", alice, s.id, "task.completed", `{}`},
		{"amp-api's own remote action", alice, s.id, domain.AuditRemotePolicySet, `{}`},
		{"the kill switch", alice, s.id, domain.AuditRemoteKill, `{}`},
		{"someone else's session", alice, bobs.id, domain.AuditRemoteAttach, `{}`},
		{"a missing session", alice, "rs_AAAAAAAAAAAAAAAAAAAAAA", domain.AuditRemoteAttach, `{}`},
		{"no session for a session action", alice, nil, domain.AuditRemotePrompt, `{}`},
		{"no actor for a person's action", nil, s.id, domain.AuditRemotePrompt, `{}`},
		{"an unknown user", 1 << 30, s.id, domain.AuditRemoteAttach, `{}`},
		{"an unknown detail", alice, s.id, domain.AuditRemoteAttach, `{"text":"forged"}`},
		{"a nested detail", alice, s.id, domain.AuditRemoteAttach, `{"op":{"task":"completed"}}`},
		{"a long detail", alice, s.id, domain.AuditRemoteAttach, fmt.Sprintf(`{"op":%q}`, strings.Repeat("x", 201))},
		{"details not an object", alice, s.id, domain.AuditRemoteAttach, `["x"]`},
	}
	for _, c := range refused {
		if err := record(c.actor, c.session, c.action, c.details); err == nil {
			t.Errorf("%s: relay_record_audit accepted it", c.name)
		}
	}

	// A good entry: the function fills the actor's email, org, project,
	// target, summary and the session's own details (not the caller's).
	if err := record(alice, s.id, domain.AuditRemoteAttach,
		`{"op":"attach","session_id":"rs_forged","harness_host":"evil","len":3,"channel":"ui"}`); err != nil {
		t.Fatalf("a good entry: %v", err)
	}
	got := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteAttach, s.id)
	if len(got) != 1 {
		t.Fatalf("attach entries: %+v", got)
	}
	a := got[0]
	if a.ActorEmail != e.w.alice.Email || a.ProjectID == nil || *a.ProjectID != e.w.s.projectID ||
		a.OrgID == nil || *a.OrgID != e.w.org.ID || a.TargetType != "remote_session" ||
		a.Details["session_id"] != s.id || a.Details["harness_host"] == "evil" || a.Details["op"] != "attach" ||
		!strings.HasPrefix(a.Summary, "remote.attach: session "+s.id) {
		t.Fatalf("recorded entry: %+v", a)
	}
	// The relay's sweeps record ends without a person; nothing else may.
	if err := record(nil, s.id, domain.AuditRemoteExpire, `{"op":"end","reason":"idle"}`); err != nil {
		t.Fatalf("the sweep's entry: %v", err)
	}
	if err := record(bob, nil, domain.AuditRemoteDeviceRevoke, `{"op":"revoke_all","devices":2,"sessions":1}`); err != nil {
		t.Fatalf("revoke-all's entry: %v", err)
	}
	// Executable by the relay's role only, not by everyone.
	var public bool
	if err := conn.QueryRow(ctx, `SELECT has_function_privilege('public', 'relay_record_audit(int, text, text, jsonb)', 'EXECUTE')`).
		Scan(&public); err != nil || public {
		t.Fatalf("PUBLIC may execute relay_record_audit (%v)", err)
	}
}

// N3 end to end: the relay's remote.* entries still reach webhooks, rendered
// and signed by amp-api from the stored entry.
func TestRelay_RemoteAuditWebhooksSentByAmpAPI(t *testing.T) {
	e := newRelayEnv(t, nil)
	allowPrivateWebhooks(t)
	startWebhooks(t, e.w, webhook.Options{AllowPrivateNetworks: true})
	rc := newReceiver(t)
	org := e.w.org.ID
	if _, err := e.w.s.repo.CreateWebhook(context.Background(), domain.Webhook{OrgID: &org, Name: "remote", URL: rc.srv.URL,
		Format: "json", Secret: "whsec_test", Events: []string{"audit.remote.*"}, Active: true}); err != nil {
		t.Fatal(err)
	}
	s := e.register(t, "alice")
	got := rc.wait(t, "audit.remote.enable", func(g []received) bool {
		for _, r := range g {
			if r.event == "audit.remote.enable" {
				return true
			}
		}
		return false
	})
	for _, r := range got {
		if r.event != "audit.remote.enable" {
			continue
		}
		var env struct {
			Type string            `json:"type"`
			Data domain.AuditEntry `json:"data"`
		}
		if err := json.Unmarshal([]byte(r.body), &env); err != nil {
			t.Fatal(err)
		}
		if env.Data.TargetID != s.id || env.Data.Action != domain.AuditRemoteEnable || env.Data.ActorEmail != e.w.alice.Email {
			t.Fatalf("delivered body: %s", r.body)
		}
		if !strings.Contains(r.signature, "v1=") {
			t.Fatalf("unsigned delivery: %+v", r)
		}
	}
	// Delivered once: audit_outbox carries relay rows, and nothing else
	// queues them (relayAuditLoop and relay_audit_outbox are retired, #2433).
	time.Sleep(300 * time.Millisecond)
	rc.mu.Lock()
	enables := 0
	for _, r := range rc.got {
		if r.event == "audit.remote.enable" && strings.Contains(r.body, s.id) {
			enables++
		}
	}
	rc.mu.Unlock()
	if enables != 1 {
		t.Fatalf("remote.enable delivered %d times", enables)
	}
	// Nothing is left queued once amp-api has matched it.
	conn, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var queued int
	if err := conn.QueryRow(context.Background(), `SELECT COUNT(*) FROM relay_audit_outbox a JOIN audit_log l ON l.id = a.audit_id
		WHERE l.target_id = $1`, s.id).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("%d entries still queued", queued)
	}
}

// N6: a refresh proof used on one replica is refused on every other one.
func TestRelayCluster_RefreshProofUsedOnceAcrossReplicas(t *testing.T) {
	a, b := twoRelays(t)
	s := a.register(t, "alice")
	good := a.tokenProof(t, s.harness, s.id)
	path := "/relay/v1/sessions/" + s.id + "/token"
	st, body, _ := a.do(t, "POST", path, good, as("alice"))
	wantStatus(t, "refresh on A", st, body, 200, "")
	st, body, _ = b.do(t, "POST", path, good, as("alice"))
	wantStatus(t, "the same proof on B", st, body, 400, "replayed_nonce")
	st, body, _ = a.do(t, "POST", path, good, as("alice"))
	wantStatus(t, "the same proof on A again", st, body, 400, "replayed_nonce")
	// A new proof works on either replica.
	st, body, _ = b.do(t, "POST", path, a.tokenProof(t, s.harness, s.id), as("alice"))
	wantStatus(t, "a new proof on B", st, body, 200, "")
}

// tokenProofAt is tokenProof with the proof's ts.
func (e *relayEnv) tokenProofAt(t *testing.T, k rcKey, id string, ts int64) []byte {
	t.Helper()
	b, err := relay.SignTokenProof(k.priv, id, k.id, ts, rcNonce())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// N5's server half: a stale proof's answer carries the relay's clock, so the
// harness can tell its person how far off the laptop's clock is.
func TestRelay_StaleProofSaysTheRelaysTime(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	now := e.clock.Now().UnixMilli()
	proof := e.tokenProofAt(t, s.harness, s.id, now-5*60_000)
	st, body, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/token", proof, as("alice"))
	wantStatus(t, "stale", st, body, 400, "stale_ts")
	if got, _ := body["now_ms"].(float64); int64(got) != now {
		t.Fatalf("now_ms = %v, want %d", body["now_ms"], now)
	}
}

// N7: losing access to the session's project ends the session (reason
// access_lost, #2455; not revoked, which is Revoke all) the way
// deactivation does: removed from the team, the team unlinked, removed from
// the org; and, as a backstop for access lost any other way, the token
// refresh and the relay's sweep check again.
func TestRelay_LosingProjectAccessEndsSessions(t *testing.T) {
	e := newRelayEnv(t, nil)
	ctx := context.Background()
	ended := func(what string, s *rcSession, hc *sseConn) {
		t.Helper()
		if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != domain.RemoteEndAccessLost {
			t.Fatalf("%s: end %v", what, f)
		}
		st, body, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token))
		if st != 410 {
			t.Fatalf("%s: heartbeat %d %v", what, st, body)
		}
		if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteDisable, s.id); len(a) != 1 || a[0].Details["reason"] != domain.RemoteEndAccessLost {
			t.Fatalf("%s: remote.disable %+v", what, a)
		}
	}

	// Removed from the only team linking alice to the project (as an org
	// admin, through amp-api).
	s := e.register(t, "alice")
	hc := s.connect(t)
	keep := e.register(t, "bob")
	st, body, _ := e.do(t, "DELETE", fmt.Sprintf("/api/teams/%d/members/%d", e.w.devs.ID, e.w.alice.User.ID), nil, as("orgadmin"))
	if st != 200 && st != 204 {
		t.Fatalf("remove alice from the team: %d %v", st, body)
	}
	ended("removed from the team", s, hc)
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+keep.id+"/heartbeat", nil, bearer(keep.token))
	wantStatus(t, "bob is still on the team", st, body, 200, "")

	// The team unlinked from the project ends bob's.
	hk := keep.connect(t)
	st, body, _ = e.do(t, "DELETE", fmt.Sprintf("/api/projects/%d/teams/%d", e.w.s.projectID, e.w.devs.ID), nil, as("orgadmin"))
	if st != 200 && st != 204 {
		t.Fatalf("unlink the team: %d %v", st, body)
	}
	ended("team unlinked", keep, hk)

	// Removed from the org: carol reaches the project through Ops.
	if err := e.w.s.repo.SetProjectTeam(ctx, e.w.s.projectID, e.w.devs.ID, domain.ProjectTeamMember); err != nil {
		t.Fatal(err)
	}
	e.ids["carol"] = e.w.carol
	c := e.register(t, "carol")
	hcc := c.connect(t)
	st, body, _ = e.do(t, "DELETE", fmt.Sprintf("/api/orgs/%d/members/%d", e.w.org.ID, e.w.carol.User.ID), nil, as("orgadmin"))
	if st != 200 && st != 204 {
		t.Fatalf("remove carol from the org: %d %v", st, body)
	}
	ended("removed from the org", c, hcc)

	// Access lost without amp-api's end (a direct database change): the
	// next token refresh ends the session...
	if err := e.w.s.repo.SetTeamMember(ctx, e.w.devs.ID, e.w.alice.User.ID, domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	s2 := e.register(t, "alice")
	hc2 := s2.connect(t)
	if err := e.w.s.repo.RemoveTeamMember(ctx, e.w.devs.ID, e.w.alice.User.ID); err != nil {
		t.Fatal(err)
	}
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+s2.id+"/token", e.tokenProof(t, s2.harness, s2.id), as("alice"))
	wantStatus(t, "refresh after losing access", st, body, 410, domain.RemoteCodeSessionEnded)
	if body["reason"] != domain.RemoteEndAccessLost {
		t.Fatalf("refresh after losing access: reason %v, want %s (#2455)", body["reason"], domain.RemoteEndAccessLost)
	}
	ended("refresh", s2, hc2)

	// ...and so does the relay's sweep.
	if err := e.w.s.repo.SetTeamMember(ctx, e.w.devs.ID, e.w.alice.User.ID, domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	s3 := e.register(t, "alice")
	hc3 := s3.connect(t)
	if err := e.w.s.repo.RemoveTeamMember(ctx, e.w.devs.ID, e.w.alice.User.ID); err != nil {
		t.Fatal(err)
	}
	e.rl.ExpireNow(ctx)
	ended("sweep", s3, hc3)
}
