package main

// The remote control relay across replicas (AMP #2377): two in-process
// amp-api relays against one database, each with its own cluster node and
// internal listener. A session's traffic is served on the replica holding
// it, whichever replica the harness or the browser reaches.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/relay"
)

// relayReplica is one amp-relay replica (with its own amp-api in front of
// the same database): public listener, cluster node and internal listener.
type relayReplica struct {
	*relayEnv
}

func newRelayReplica(t *testing.T, w *rbacWorld, clock *testClock, name string) *relayReplica {
	t.Helper()
	e := newRelayEnvIn(t, w, clock, func(c *relay.Config) {
		c.Sweep = 50 * time.Millisecond
		c.RouteTimeout = 10 * time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	go e.rl.Run(ctx)
	stopNode := e.stop
	e.stop = func() { cancel(); stopNode() }
	return &relayReplica{relayEnv: e}
}

// crash stops the replica the way a killed pod stops: no heartbeat, no
// handover, every connection dropped.
func (r *relayReplica) crash() {
	r.stop()
	r.internal.CloseClientConnections()
	r.internal.Close()
	r.srv.CloseClientConnections()
	r.srv.Close()
}

func twoRelays(t *testing.T) (a, b *relayReplica) {
	t.Helper()
	w := newRBACWorld(t)
	enableOrg(t, w.s.repo, w.org.ID, true)
	clock := &testClock{at: time.Now()}
	return newRelayReplica(t, w, clock, "a"), newRelayReplica(t, w, clock, "b")
}

// via returns the session as seen through replica e (same keys, token and
// seq counter's starting point).
func (s *rcSession) via(e *relayEnv) *rcSession {
	c := *s
	c.e = e
	return &c
}

func holderOf(t *testing.T, e *relayEnv, id string) string {
	t.Helper()
	sess, err := e.w.s.repo.GetRemoteSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.HolderNode == nil {
		return ""
	}
	return *sess.HolderNode
}

func listed(t *testing.T, e *relayEnv, who, id string) map[string]any {
	t.Helper()
	st, body, _ := e.do(t, "GET", "/api/sessions", nil, as(who))
	if st != 200 {
		t.Fatalf("list: %d %v", st, body)
	}
	list, _ := body["sessions"].([]any)
	for _, x := range list {
		if m, _ := x.(map[string]any); m["session_id"] == id {
			return m
		}
	}
	t.Fatalf("session %s not listed: %v", id, body)
	return nil
}

// Harness on A, browser on B: the browser's stream, commands, pairing and
// limits are served on A, the holder; harness calls that land on B too. No
// content reaches the logs or the database on the way.
func TestRelayCluster_HarnessOnABrowserOnB(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	const marker = "CLUSTER-CONTENT-7f3a1"

	a, b := twoRelays(t)
	s := a.register(t, "alice")
	if h := holderOf(t, a.relayEnv, s.id); h != a.node.ID() {
		t.Fatalf("holder after register on A: %q", h)
	}
	hc := s.connect(t)
	onB := s.via(b.relayEnv)

	v := onB.watch(t, "")
	if f := v.expect(t, "relay"); dataOf(t, f)["type"] != "attached" || dataOf(t, f)["online"] != true {
		t.Fatalf("attached through B: %v", f)
	}
	if f := hc.expect(t, "viewers"); dataOf(t, f)["count"] != float64(1) {
		t.Fatalf("the harness on A doesn't count B's viewer: %v", f)
	}
	if m := listed(t, b.relayEnv, "alice", s.id); m["online"] != true || m["viewers"] != float64(1) {
		t.Fatalf("presence listed on B: %v", m)
	}
	if st, body, _ := b.do(t, "GET", "/api/sessions/"+s.id, nil, as("alice")); st != 200 || body["online"] != true || body["harness_pub"] != s.harness.pub {
		t.Fatalf("GET on B: %d %v", st, body)
	}
	if st, body, _ := b.do(t, "GET", "/api/sessions/"+s.id, nil, as("bob")); st != 404 {
		t.Fatalf("bob on B: %d %v", st, body)
	}

	// Events posted on A and on B reach the viewer on B, in order.
	s.mustPublish(t, s.hello(t), s.event(t, "chat_event", nil, map[string]any{"type": "user_message", "text": "hi " + marker}))
	onB.seq = s.seq
	onB.mustPublish(t, onB.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "message_id": "m", "delta": "answer " + marker}))
	s.seq = onB.seq
	for _, want := range []string{`"op":"hello"`, "user_message", "text_delta"} {
		if f := v.waitFor(t, "envelope", want); f.ID == "" {
			t.Fatalf("envelope without id: %v", f)
		}
	}
	// The resync buffer is the holder's: a second viewer on B replays it.
	v2 := onB.watch(t, "?from_seq=1")
	v2.waitFor(t, "envelope", "text_delta")
	v2.close()
	// A heartbeat on B counts on A.
	if st, body, _ := b.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token)); st != 200 || body["viewers"] == nil {
		t.Fatalf("heartbeat on B: %d %v", st, body)
	}

	// Commands from B reach the harness on A; the prompt budget is one
	// budget whichever replica the browser reaches.
	dev := newRCKey(t)
	s.pin(t, dev, a.clock.Now().Add(time.Hour))
	seq := int64(0)
	cmd := func(op string, body map[string]any) []byte { seq++; return s.command(t, dev, seq, op, body) }
	for i := 0; i < 5; i++ {
		on := []*rcSession{onB, s}[i%2]
		st, body, _ := on.send(t, cmd("prompt", prompt(fmt.Sprintf("p%d %s", i, marker))))
		wantStatus(t, fmt.Sprintf("prompt %d", i), st, body, 202, "")
		if f := hc.waitFor(t, "command", `"op":"prompt"`); !strings.Contains(f.Data, fmt.Sprintf("p%d", i)) {
			t.Fatalf("command %d: %v", i, f)
		}
	}
	for _, on := range []*rcSession{onB, s} {
		st, body, hdr := on.send(t, cmd("prompt", prompt("one too many")))
		wantStatus(t, "6th prompt across replicas", st, body, 429, "rate_limited")
		if hdr.Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
	}
	st, body, _ := onB.send(t, cmd("stop", map[string]any{}))
	wantStatus(t, "stop on B", st, body, 202, "")
	hc.waitFor(t, "command", `"op":"stop"`)

	// Pairing: the window opened through B is the holder's; tries count
	// across replicas.
	pid := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
	st, body, _ = b.do(t, "POST", "/relay/v1/sessions/"+s.id+"/pairings",
		map[string]any{"pairing_id": pid, "expires_at_ms": a.clock.Now().Add(5 * time.Minute).UnixMilli()}, bearer(s.token))
	wantStatus(t, "open pairing on B", st, body, 201, "")
	pdev := newRCKey(t)
	pairReq := map[string]any{"request": map[string]any{
		"type": "pair_request", "v": 1, "session_id": s.id, "harness_id": s.harness.id, "pairing_id": pid,
		"device_id": pdev.id, "device_pub": pdev.pub, "device_name": "Chrome", "ts": a.clock.Now().UnixMilli(), "nonce": rcNonce(),
	}, "mac": "bWFj", "sig": "c2ln"}
	for i := 0; i < 5; i++ {
		e := []*relayEnv{b.relayEnv, a.relayEnv}[i%2]
		st, body, _ := e.do(t, "POST", "/api/sessions/"+s.id+"/pair", pairReq, as("alice"))
		wantStatus(t, fmt.Sprintf("pair try %d", i), st, body, 202, "")
		if f := hc.waitFor(t, "pair_request", ""); !strings.Contains(f.Data, `"user_id"`) {
			t.Fatalf("pair_request without the verified user: %v", f)
		}
	}
	st, body, _ = b.do(t, "POST", "/api/sessions/"+s.id+"/pair", pairReq, as("alice"))
	wantStatus(t, "6th pair try across replicas", st, body, 429, "pairing_locked")

	// Cross-site refusal still applies to a forwarded call.
	st, body, _ = onB.send(t, cmd("stop", map[string]any{}), fromOrigin("https://evil.test"))
	wantStatus(t, "cross-site on B", st, body, 403, "cross_site")

	// Registrations: 10 a minute per person across replicas.
	for i := 0; i < 10; i++ {
		e := []*relayEnv{a.relayEnv, b.relayEnv}[i%2]
		r := e.register(t, "bob")
		if st, _, _ := e.do(t, "DELETE", "/relay/v1/sessions/"+r.id, nil, bearer(r.token)); st != 204 {
			t.Fatalf("DELETE: %d", st)
		}
	}
	st, body, hdr := b.do(t, "POST", "/relay/v1/sessions", b.registration(newRCKey(t)), as("bob"))
	wantStatus(t, "11th registration across replicas", st, body, 429, "rate_limited")
	if hdr.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}

	// End through B: the harness on A and the viewer on B both close.
	if st, _, _ := b.do(t, "DELETE", "/relay/v1/sessions/"+s.id, nil, bearer(s.token)); st != 204 {
		t.Fatalf("DELETE on B: %d", st)
	}
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "harness_ended" {
		t.Fatalf("harness end: %v", f)
	}
	v.waitFor(t, "relay", "ended")
	hc.closed(t)
	v.closed(t)

	if strings.Contains(logs.String(), marker) {
		t.Fatalf("message content reached the logs:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), s.token) {
		t.Fatal("a connect token reached the logs")
	}
	db, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	for _, q := range []string{
		`SELECT COUNT(*) FROM audit_log WHERE details::text LIKE $1 OR summary LIKE $1`,
		`SELECT COUNT(*) FROM remote_sessions WHERE row_to_json(remote_sessions)::text LIKE $1`,
		`SELECT COUNT(*) FROM remote_devices WHERE row_to_json(remote_devices)::text LIKE $1`,
		`SELECT COUNT(*) FROM live_events WHERE row_to_json(live_events)::text LIKE $1`,
		`SELECT COUNT(*) FROM api_nodes WHERE row_to_json(api_nodes)::text LIKE $1`,
		`SELECT COUNT(*) FROM cluster_settings WHERE row_to_json(cluster_settings)::text LIKE $1`,
	} {
		var n int
		if err := db.QueryRow(context.Background(), q, "%"+marker+"%").Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Fatalf("message content stored: %s matched %d rows", q, n)
		}
	}
	var n int
	if err := db.QueryRow(context.Background(), `SELECT COUNT(*) FROM remote_sessions WHERE row_to_json(remote_sessions)::text LIKE $1`,
		"%"+s.token+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("connect token stored in clear: %d %v", n, err)
	}
}

// Every way a session ends, started on the replica that doesn't hold it,
// closes the holder's streams; afterwards every replica answers 410.
func TestRelayCluster_EndsReachTheHolder(t *testing.T) {
	a, b := twoRelays(t)

	// Disconnect from the browser on B.
	s := a.register(t, "alice")
	hc := s.connect(t)
	vA, vB := s.watch(t, ""), s.via(b.relayEnv).watch(t, "")
	if st, _, _ := b.do(t, "POST", "/api/sessions/"+s.id+"/disconnect", nil, as("alice")); st != 204 {
		t.Fatalf("disconnect on B: %d", st)
	}
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "disconnected" {
		t.Fatalf("harness end: %v", f)
	}
	for _, v := range []*sseConn{vA, vB} {
		if f := v.waitFor(t, "relay", "ended"); dataOf(t, f)["reason"] != "disconnected" {
			t.Fatalf("viewer end: %v", f)
		}
		v.closed(t)
	}
	hc.closed(t)
	for _, e := range []*relayEnv{a.relayEnv, b.relayEnv} {
		st, body, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token))
		wantStatus(t, "heartbeat after the end", st, body, 410, "session_ended")
		if body["reason"] != "disconnected" {
			t.Fatalf("410 reason: %v", body)
		}
		st, body, _ = e.do(t, "GET", "/api/sessions/"+s.id+"/events", nil, as("alice"))
		wantStatus(t, "watch after the end", st, body, 410, "session_ended")
	}

	// The org switch turned off on B ends the session held on A.
	s = a.register(t, "alice")
	hc = s.connect(t)
	st, body, _ := b.do(t, "PUT", fmt.Sprintf("/api/orgs/%d/remote-control", b.w.org.ID), map[string]any{"enabled": false}, as("orgadmin"))
	if st != 200 {
		t.Fatalf("org switch off on B: %d %v", st, body)
	}
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "policy_off" {
		t.Fatalf("policy_off end: %v", f)
	}
	enableOrg(t, a.w.s.repo, a.w.org.ID, true)

	// Revoke-all on B: the harnesses held on A and on B each hear
	// device_revoked {all}, then end {revoked}.
	sa, sb := a.register(t, "alice"), b.register(t, "alice")
	ha, hb := sa.connect(t), sb.connect(t)
	keep := a.register(t, "bob")
	hk := keep.connect(t)
	st, body, _ = b.do(t, "POST", "/api/sessions/revoke-all", nil, as("alice"))
	if st != 200 || body["ended"] != float64(2) {
		t.Fatalf("revoke-all on B: %d %v", st, body)
	}
	for _, c := range []*sseConn{ha, hb} {
		if f := c.expect(t, "device_revoked"); dataOf(t, f)["all"] != true {
			t.Fatalf("device_revoked: %v", f)
		}
		if f := c.expect(t, "end"); dataOf(t, f)["reason"] != "revoked" {
			t.Fatalf("revoked end: %v", f)
		}
	}
	select {
	case f := <-hk.frames:
		if !f.Comment {
			t.Fatalf("bob's harness got %+v", f)
		}
	case <-time.After(100 * time.Millisecond):
	}

	// The lifetime sweep on B ends the session held on A (an org that opted
	// into a lifetime, #2424).
	setOrgLimits(t, a.w.s.repo, a.w.org.ID, 0, 12)
	s = a.register(t, "alice")
	hc = s.connect(t)
	a.clock.Add(13 * time.Hour)
	b.rl.ExpireNow(context.Background())
	a.clock.Add(-13 * time.Hour)
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "expired" {
		t.Fatalf("expired end: %v", f)
	}
}

// The holder dies: viewers elsewhere see the session offline, and when the
// harness reconnects (to whichever replica) it resumes there.
func TestRelayCluster_HolderDies(t *testing.T) {
	a, b := twoRelays(t)
	s := a.register(t, "alice")
	s.connect(t)
	s.mustPublish(t, s.hello(t), s.event(t, "chat_event", nil, map[string]any{"type": "user_message", "text": "before"}))
	onB := s.via(b.relayEnv)
	v := onB.watch(t, "")
	v.waitFor(t, "envelope", "user_message")

	a.crash()
	v.closed(t) // the stream B proxied from A broke

	// B takes the session over once A's heartbeat is past the TTL: it's
	// offline there until the harness comes back.
	start := time.Now()
	v = onB.watch(t, "", header("Last-Event-ID", "1")) // as if seq 2 never arrived
	if v.status != 200 {
		t.Fatalf("watch after the crash: %d %v", v.status, v.body)
	}
	if f := v.expect(t, "relay"); dataOf(t, f)["type"] != "attached" || dataOf(t, f)["online"] != false {
		t.Fatalf("attached after the crash: %v", f)
	}
	if waited := time.Since(start); waited > 8*time.Second {
		t.Fatalf("takeover took %s", waited)
	}
	if h := holderOf(t, b.relayEnv, s.id); h != b.node.ID() {
		t.Fatalf("holder after the crash: %q, want %q", h, b.node.ID())
	}
	if m := listed(t, b.relayEnv, "alice", s.id); m["online"] != false {
		t.Fatalf("listed online after the crash: %v", m)
	}
	dev := newRCKey(t)
	s.pin(t, dev, b.clock.Now().Add(time.Hour))
	st, body, _ := onB.send(t, s.command(t, dev, 1, "prompt", prompt("while offline")))
	wantStatus(t, "prompt while offline", st, body, 409, "harness_offline")

	// The harness reconnects through B and resumes.
	hc := onB.connect(t)
	v.waitFor(t, "relay", "online")
	onB.seq = s.seq
	onB.mustPublish(t, onB.event(t, "chat_event", nil, map[string]any{"type": "text_delta", "message_id": "m", "delta": "after"}))
	// B's buffer starts at the resumed seq: the viewer that asked for 2 hears of the gap.
	if f := v.waitFor(t, "relay", "gap"); dataOf(t, f)["requested_seq"] != float64(2) || dataOf(t, f)["oldest_seq"] != float64(3) {
		t.Fatalf("gap after the takeover: %v", f)
	}
	if f := v.waitFor(t, "envelope", "text_delta"); f.ID != fmt.Sprint(onB.seq) {
		t.Fatalf("resumed event id %s, want %d", f.ID, onB.seq)
	}
	st, body, _ = onB.send(t, s.command(t, dev, 2, "prompt", prompt("after the crash")))
	wantStatus(t, "prompt after the reconnect", st, body, 202, "")
	if f := hc.waitFor(t, "command", ""); !strings.Contains(f.Data, "after the crash") {
		t.Fatalf("command after the reconnect: %v", f)
	}
	if m := listed(t, b.relayEnv, "alice", s.id); m["online"] != true || m["viewers"] != float64(1) {
		t.Fatalf("listed after the reconnect: %v", m)
	}
}

// A replica that was judged gone and comes back releases the sessions taken
// over meanwhile, so their streams move to the new holder.
func TestRelayCluster_StaleHolderReleases(t *testing.T) {
	a, b := twoRelays(t)
	s := a.register(t, "alice")
	hc := s.connect(t)
	// Pretend A was gone: B takes the session (as after a missed TTL).
	if _, err := a.w.s.repo.ClaimRemoteSession(context.Background(), s.id, b.node.ID()); err != nil {
		t.Fatal(err)
	}
	hc.closed(t) // A's sweep released it, with no end frame
	hc = s.connect(t)
	if h := holderOf(t, a.relayEnv, s.id); h != b.node.ID() {
		t.Fatalf("holder: %q", h)
	}
	if st, body, _ := b.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token)); st != 200 || body["viewers"] != float64(0) {
		t.Fatalf("heartbeat: %d %v", st, body)
	}
	_ = hc
}

// The internal listener serves nothing without the cluster token, so a
// forged forwarded caller is refused.
func TestRelayCluster_InternalListenerNeedsTheToken(t *testing.T) {
	a, _ := twoRelays(t)
	s := a.register(t, "alice")
	p, _ := json.Marshal(map[string]any{"UserID": a.w.alice.User.ID, "Email": a.w.alice.Email, "Channel": "ui"})
	for _, tok := range []string{"", "wrong"} {
		for _, path := range []string{"/internal/relay/api/sessions/" + s.id, "/internal/relay/ctl/presence?id=" + s.id} {
			req, _ := http.NewRequest("GET", a.internal.URL+path, nil)
			req.Header.Set("X-AMP-Relay-Principal", base64.RawURLEncoding.EncodeToString(p))
			if tok != "" {
				req.Header.Set("X-AMP-Cluster-Token", tok)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("%s with token %q: %d", path, tok, resp.StatusCode)
			}
		}
	}
}
