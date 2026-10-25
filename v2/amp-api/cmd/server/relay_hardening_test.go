package main

// Relay hardening from the #2381 security review (AMP #2390): the token
// refresh needs the harness key (M1), viewer queues and harness event bytes
// are bounded (M2), and deactivating a person ends their sessions (L4).

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/relay"
)

// Another program with the owner's own OAuth login (Claude Code with the AMP
// plugin, a manager on another machine) knows the session id but not the
// harness key: it can't renew the connect token, so it can't open the
// command stream or void the harness's token.
func TestRelay_RefreshNeedsHarnessKey(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	hc := s.connect(t)
	path := "/relay/v1/sessions/" + s.id + "/token"
	now := e.clock.Now().UnixMilli()
	proof := func(k rcKey, sessionID, harnessID string, ts int64) []byte {
		b, err := relay.SignTokenProof(k.priv, sessionID, harnessID, ts, rcNonce())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	thief := newRCKey(t)
	for _, c := range []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"no body", nil, 415, "unsupported_media_type"},
		{"no proof", map[string]any{}, 400, "malformed"},
		{"proof and more", []byte(`{"proof":{},"x":1}`), 400, "malformed"},
		{"another session's id", proof(s.harness, "rs_AAAAAAAAAAAAAAAAAAAAAA", s.harness.id, now), 400, "wrong_session"},
		{"the thief's own key", proof(thief, s.id, thief.id, now), 400, "wrong_harness"},
		{"signed by another key", proof(thief, s.id, s.harness.id, now), 403, "bad_signature"},
		{"stale", proof(s.harness, s.id, s.harness.id, now-61_000), 400, "stale_ts"},
	} {
		st, body, _ := e.do(t, "POST", path, c.body, as("alice"))
		wantStatus(t, c.name, st, body, c.status, c.code)
	}
	// None of that touched the harness's token or its stream.
	st, body, _ := e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token))
	wantStatus(t, "heartbeat after refused refreshes", st, body, 200, "")
	thiefStream := e.openSSE(t, "/relay/v1/sessions/"+s.id+"/commands", as("alice"))
	if thiefStream.status != 401 {
		t.Fatalf("command stream with the OAuth login only: %d %v", thiefStream.status, thiefStream.body)
	}

	// The harness itself renews; the same proof can't be used twice.
	good := e.tokenProof(t, s.harness, s.id)
	st, body, _ = e.do(t, "POST", path, good, as("alice"))
	wantStatus(t, "the harness's refresh", st, body, 200, "")
	if tok, _ := body["connect_token"].(string); !strings.HasPrefix(tok, "rct_") {
		t.Fatalf("refresh: %v", body)
	}
	st, body, _ = e.do(t, "POST", path, good, as("alice"))
	wantStatus(t, "a replayed proof", st, body, 400, "replayed_nonce")
	// The refresh doesn't cut the open stream.
	select {
	case f, ok := <-hc.frames:
		if ok && !f.Comment {
			t.Fatalf("the harness's stream got %s %s", f.Event, f.Data)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

// pad is a chat_event of about n bytes.
func (s *rcSession) pad(t *testing.T, n int) json.RawMessage {
	t.Helper()
	return s.event(t, "chat_event", nil, map[string]any{"kind": "pad", "text": strings.Repeat("x", n)})
}

// stalledViewer attaches a viewer on a raw connection that never reads a
// byte, and waits until the relay and the harness count it.
func stalledViewer(t *testing.T, e *relayEnv, s *rcSession, hc *sseConn) net.Conn {
	t.Helper()
	u, _ := url.Parse(e.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10)
	}
	fmt.Fprintf(conn, "GET /api/sessions/%s/events HTTP/1.1\r\nHost: %s\r\nX-Test-As: alice\r\nAccept: text/event-stream\r\n\r\n", s.id, u.Host)
	live := e.rl.Registry().Get(s.id)
	deadline := time.Now().Add(5 * time.Second)
	for live.Viewers() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the viewer never attached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hc.waitFor(t, "viewers", `"count":1`)
	return conn
}

// waitQueuedZero waits until no viewer bytes are queued on the replica.
func waitQueuedZero(t *testing.T, e *relayEnv, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for e.rl.QueuedViewerBytes() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d viewer bytes still queued", e.rl.QueuedViewerBytes())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A viewer that never reads is dropped once its queue passes the byte bound
// (the old 512-frame queue held about 460 MiB of 900 KiB events), and the
// harness hears the viewer count drop.
func TestRelay_SlowViewerBoundedByBytes(t *testing.T) {
	// A write deadline far off: the byte bound, not the deadline, drops it.
	e := newRelayEnv(t, func(c *relay.Config) { c.WriteTimeout = 5 * time.Minute })
	s := e.register(t, "alice")
	hc := s.connect(t)
	s.mustPublish(t, s.hello(t))
	conn := stalledViewer(t, e, s, hc)
	live := e.rl.Registry().Get(s.id)

	// 18 MiB of events, well past one viewer's 4 MiB.
	maxSeen := int64(0)
	for i := 0; i < 10; i++ {
		e.clock.Add(2 * time.Second) // keep under the event byte rate
		s.mustPublish(t, s.pad(t, 900<<10), s.pad(t, 900<<10))
		if q := e.rl.QueuedViewerBytes(); q > maxSeen {
			maxSeen = q
		}
	}
	if maxSeen > 4<<20 {
		t.Fatalf("a viewer queued %d bytes, over 4 MiB", maxSeen)
	}
	if n := live.Viewers(); n != 0 {
		t.Fatalf("the slow viewer is still attached (%d viewers)", n)
	}
	hc.waitFor(t, "viewers", `"count":0`)
	// When its handler ends, what was still queued is released.
	conn.Close()
	waitQueuedZero(t, e, 10*time.Second)
	// A reading viewer still works.
	v := s.watch(t, "")
	v.expect(t, "relay")
}

// A client that stops reading can't hold its handler (and the frames it
// still had queued) forever: each write has a deadline.
func TestRelay_StalledViewerWriteDeadline(t *testing.T) {
	e := newRelayEnv(t, func(c *relay.Config) { c.WriteTimeout = 500 * time.Millisecond })
	s := e.register(t, "alice")
	hc := s.connect(t)
	s.mustPublish(t, s.hello(t))
	stalledViewer(t, e, s, hc) // never closed until the test ends
	for i := 0; i < 4; i++ {
		e.clock.Add(2 * time.Second)
		s.mustPublish(t, s.pad(t, 900<<10), s.pad(t, 900<<10))
	}
	hc.waitFor(t, "viewers", `"count":0`)
	waitQueuedZero(t, e, 10*time.Second)
}

// A flooding harness gets 429 past 8 MiB of events, and room again as the
// bucket refills at 2 MiB/s.
func TestRelay_HarnessEventByteRate(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	s.connect(t)
	s.mustPublish(t, s.hello(t))
	var st int
	var body map[string]any
	sent := 0
	for i := 0; i < 10; i++ {
		st, body = s.publish(t, s.pad(t, 900<<10), s.pad(t, 900<<10))
		if st != 200 {
			break
		}
		sent++
	}
	wantStatus(t, "flooding", st, body, 429, "rate_limited")
	if sent < 3 || sent > 5 {
		t.Fatalf("%d batches of 1.8 MiB passed before the 429, want 3-5 (8 MiB burst)", sent)
	}
	if body["retry_after_s"] == nil {
		t.Fatalf("429 without retry_after_s: %v", body)
	}
	// The refused batch wasn't accepted: resend it after a wait.
	s.seq -= 2
	e.clock.Add(2 * time.Second)
	s.mustPublish(t, s.pad(t, 900<<10), s.pad(t, 900<<10))
}

// Deactivating a person ends their live sessions (reason revoked): the
// harness's stream gets end, the connect token stops working, and the
// relay refuses a deactivated owner's token even for a session left active.
func TestRelay_DeactivationEndsSessions(t *testing.T) {
	e := newRelayEnv(t, nil)
	s := e.register(t, "alice")
	hc := s.connect(t)
	v := s.watch(t, "")
	v.expect(t, "relay")
	keep := e.register(t, "bob")

	st, body, _ := e.do(t, "POST", fmt.Sprintf("/api/users/%d/deactivate", e.w.alice.User.ID), nil, as("root"))
	if st != 200 {
		t.Fatalf("deactivate: %d %v", st, body)
	}
	if f := hc.waitFor(t, "end", ""); dataOf(t, f)["reason"] != domain.RemoteEndRevoked {
		t.Fatalf("end: %v", f)
	}
	v.waitFor(t, "relay", `"ended"`)
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/heartbeat", nil, bearer(s.token))
	if st != 401 && st != 410 {
		t.Fatalf("heartbeat after deactivation: %d %v", st, body)
	}
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteDisable, s.id); len(a) != 1 || a[0].Details["reason"] != domain.RemoteEndRevoked {
		t.Fatalf("remote.disable: %+v", a)
	}
	// Someone else's session is untouched.
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+keep.id+"/heartbeat", nil, bearer(keep.token))
	wantStatus(t, "bob's heartbeat", st, body, 200, "")

	// The backstop: an owner deactivated without the end (a failed call)
	// can't use the token either.
	if _, err := e.w.s.repo.SetUserDisabled(t.Context(), e.w.bob.User.ID, true); err != nil {
		t.Fatal(err)
	}
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+keep.id+"/heartbeat", nil, bearer(keep.token))
	wantStatus(t, "a deactivated owner's token", st, body, 401, "unauthenticated")
}
