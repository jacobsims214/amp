package main

// The real harness link against this relay (AMP #2379). The Rust side is
// amp-tui's tests/remote_live.rs: it plays the laptop (a real chat Shell and
// RemoteLink) and the person's browser. This test serves the relay on the
// real database, runs that test binary, then checks what AMP kept.
//
// Skipped unless AMP_RELAY_LIVE_HARNESS names the compiled Rust test binary;
// v2/scripts/remote-live-e2e.sh builds it and runs this test.
//
// The relay is built from internal/relay's own constructor and handlers
// (relay.New, HarnessHandler, RegisterBrowser), not amp-api's main router, so
// the test carries over when the relay becomes its own service (#2387). Only
// the fixtures (rbacWorld, remoteAudit, testDSN) are from this package.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/relay"
)

func TestRelay_LiveRustHarness(t *testing.T) {
	bin := os.Getenv("AMP_RELAY_LIVE_HARNESS")
	if bin == "" {
		t.Skip("AMP_RELAY_LIVE_HARNESS not set (v2/scripts/remote-live-e2e.sh runs this)")
	}
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	srvURL, w := newLiveRelay(t)
	marker := "LIVE-MARKER-" + rcNonce()[:10]

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--ignored", "--exact", "live_go_relay", "--nocapture", "--test-threads=1")
	cmd.Env = append(os.Environ(),
		"AMP_RELAY_LIVE_URL="+srvURL,
		"AMP_RELAY_LIVE_TOKEN=test-oauth-alice",
		fmt.Sprintf("AMP_RELAY_LIVE_USER_ID=%d", w.alice.User.ID),
		fmt.Sprintf("AMP_RELAY_LIVE_PROJECT_ID=%d", w.s.projectID),
		"AMP_RELAY_LIVE_ORIGIN="+relayOrigin,
		"AMP_RELAY_LIVE_MARKER="+marker,
		"AMP_RELAY_LIVE_BROWSER=alice",
		// A teammate, the org owner, the project-owner team, a global admin, another org.
		"AMP_RELAY_LIVE_INTRUDERS=bob,orgadmin,owner,root,outsider",
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("the Rust side failed: %v\n%s", err, out.String())
	}
	m := regexp.MustCompile(`LIVE-SESSION (rs_\S+) DEVICE (\S+)`).FindStringSubmatch(out.String())
	if m == nil || !strings.Contains(out.String(), "1 passed") {
		t.Fatalf("the Rust test didn't run:\n%s", out.String())
	}
	sid, device := m[1], m[2]
	t.Logf("Rust side:\n%s", out.String())

	db, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())

	// The relay learned the pin from the signed pair_result, for alice only.
	var pinned int
	if err := db.QueryRow(context.Background(), `SELECT COUNT(*) FROM remote_devices WHERE owner_user_id = $1 AND device_id = $2 AND revoked_at IS NULL`,
		w.alice.User.ID, device).Scan(&pinned); err != nil || pinned != 1 {
		t.Fatalf("device %s pinned for alice: %d %v", device, pinned, err)
	}

	// The audit trail: metadata only.
	for _, action := range []string{domain.AuditRemoteEnable, domain.AuditRemotePair, domain.AuditRemoteStop} {
		if a := remoteAudit(t, w.s.repo, w.org.ID, action, sid); len(a) == 0 {
			t.Errorf("no %s audit for %s", action, sid)
		}
	}
	prompts := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemotePrompt, sid)
	if len(prompts) == 0 || prompts[0].Details["sha256"] == nil || prompts[0].Details["len"] == nil {
		t.Errorf("remote.prompt audit: %+v", prompts)
	}
	refused := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemoteRefused, sid)
	codes := map[any]bool{}
	for _, a := range refused {
		codes[a.Details["code"]] = true
	}
	if !codes["bad_signature"] || !codes["unknown_device"] {
		t.Errorf("remote.refused audits: %v", codes)
	}

	// No message content anywhere AMP writes.
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("message content reached the logs:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "rct_") {
		t.Fatal("a connect token reached the logs")
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM audit_log WHERE details::text LIKE $1 OR summary LIKE $1`,
		`SELECT COUNT(*) FROM remote_sessions WHERE row_to_json(remote_sessions)::text LIKE $1`,
		`SELECT COUNT(*) FROM remote_devices WHERE row_to_json(remote_devices)::text LIKE $1`,
		`SELECT COUNT(*) FROM live_events WHERE row_to_json(live_events)::text LIKE $1`,
	} {
		var n int
		if err := db.QueryRow(context.Background(), q, "%"+marker+"%").Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Fatalf("message content stored: %s matched %d rows", q, n)
		}
	}
	var state, reason string
	if err := db.QueryRow(context.Background(), `SELECT state, COALESCE(end_reason, '') FROM remote_sessions WHERE id = $1`, sid).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "ended" || reason != "detached" {
		t.Fatalf("session after detach: %s %s", state, reason)
	}
}

// newLiveRelay serves both legs from internal/relay on a real database, with
// the real clock (the harness signs with it). Identities stand in for the two
// verifiers: the harness's OAuth token is "test-oauth-<user>", and the
// browser names its user in X-Test-As (the oauth2-proxy session, Channel ui).
func newLiveRelay(t *testing.T) (string, *rbacWorld) {
	t.Helper()
	w := newRBACWorld(t)
	enableOrg(t, w.s.repo, w.org.ID, true)
	ids := map[string]*auth.Identity{
		"alice": w.alice, "bob": w.bob, "owner": w.owner, "outsider": w.outsider, "root": w.root, "orgadmin": w.orgadmin,
	}
	identify := func(listener, channel string, who func(*http.Request) string, next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			ident, ok := ids[who(r)]
			if !ok {
				rw.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(rw, `{"error":"missing bearer token"}`)
				return
			}
			c := *ident
			c.Listener, c.Channel = listener, channel
			next.ServeHTTP(rw, r.WithContext(auth.WithIdentity(r.Context(), &c)))
		})
	}
	oauth := func(next http.Handler) http.Handler {
		return identify("mcp", auth.ChannelMCP, func(r *http.Request) string {
			who, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer test-oauth-")
			return who
		}, next)
	}
	h := api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb)
	h.SetRemoteControl(remoteOn)
	rl := relay.New(h.Service(), oauth, relay.Config{AppURL: relayOrigin})
	browser := http.NewServeMux()
	rl.RegisterBrowser(browser)
	root := http.NewServeMux()
	root.Handle("/relay/", rl.HarnessHandler())
	root.Handle("/", identify("rest", auth.ChannelUI, func(r *http.Request) string { return r.Header.Get("X-Test-As") },
		api.RejectCrossSiteWrites(browser)))
	ctx, cancel := context.WithCancel(context.Background())
	go rl.Run(ctx)
	srv := httptest.NewServer(root)
	t.Cleanup(func() { srv.Close(); cancel() })
	return srv.URL, w
}
