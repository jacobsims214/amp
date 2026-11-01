package main

// /remote off during a relay outage (AMP #2396): the harness couldn't DELETE
// the session while amp-relay was down, so it stayed listed as live and
// offline until its idle limit. When the relay is back (on any replica, after
// the holder is gone and the connect token has lapsed), the harness ends it
// with what it has: a token refresh signed by its harness key, then DELETE.

import (
	"testing"
	"time"
)

func TestRelay_EndAfterOutageWithSignedRefresh(t *testing.T) {
	a, b := twoRelays(t)
	s := a.register(t, "alice")
	s.connect(t)
	s.mustPublish(t, s.hello(t))

	// The outage: the replica holding the session goes away, and the harness's
	// DELETE never arrives. It stays listed, live and offline.
	a.crash()
	b.clock.Add(16 * time.Minute) // longer than the connect token's 15 minutes
	if m := listed(t, b.relayEnv, "alice", s.id); m["state"] != "active" || m["online"] != false {
		t.Fatalf("listed during the outage: %v", m)
	}

	// The old token has lapsed: DELETE alone isn't enough.
	path := "/relay/v1/sessions/" + s.id
	st, body, _ := b.do(t, "DELETE", path, map[string]any{"reason": "harness_ended"}, bearer(s.token))
	wantStatus(t, "DELETE with the lapsed token", st, body, 401, "token_expired")

	// The harness renews with its key (the OAuth login alone can't), then ends it.
	st, body, _ = b.do(t, "POST", path+"/token", b.tokenProof(t, s.harness, s.id), as("alice"))
	wantStatus(t, "signed refresh after the outage", st, body, 200, "")
	tok, _ := body["connect_token"].(string)
	st, body, _ = b.do(t, "DELETE", path, map[string]any{"reason": "harness_ended"}, bearer(tok))
	wantStatus(t, "DELETE after the refresh", st, body, 204, "")

	m := listed(t, b.relayEnv, "alice", s.id)
	if m["state"] != "ended" || m["end_reason"] != "harness_ended" {
		t.Fatalf("listed after the late end: %v", m)
	}
	// Retrying is harmless: an ended session answers 410 with its reason, which
	// the harness takes as done.
	st, body, _ = b.do(t, "DELETE", path, map[string]any{"reason": "harness_ended"}, bearer(tok))
	wantStatus(t, "a second DELETE", st, body, 410, "session_ended")
	st, body, _ = b.do(t, "POST", path+"/token", b.tokenProof(t, s.harness, s.id), as("alice"))
	wantStatus(t, "a refresh after the end", st, body, 410, "session_ended")
}
