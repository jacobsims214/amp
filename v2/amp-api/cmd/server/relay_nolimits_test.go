package main

// Remote sessions keep themselves alive until a person ends them (AMP #2424,
// user decision 2026-10-26): no idle limit and no lifetime by default, an
// org admin can still opt into either, and a harness that was away for hours
// renews its connect token with its signed proof and carries on.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/relay"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 031: the 30 minutes every org got from 027's column default (the
// web UI never had a control for it) becomes 0, off; a value an admin set
// through the API is kept; live sessions lose their 12 h lifetime, ended ones
// keep their history.
func TestMigration_031RemoteLimitsOff(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var before []repository.Migration
	for _, m := range migrations {
		if m.Version < "031" {
			before = append(before, m)
		}
	}
	if err := repo.RunMigrations(ctx, before); err != nil {
		t.Fatalf("migrate to 030: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var user, dflt, custom, project int
	if err := db.QueryRow(ctx, `INSERT INTO users (subject, email) VALUES ('sub-nl', 'nl@x.test') RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO orgs (name, slug, created_by, remote_control_enabled)
		VALUES ('Default', 'default-nl', $1, TRUE) RETURNING id`, user).Scan(&dflt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO orgs (name, slug, created_by, remote_control_enabled, remote_idle_timeout_min)
		VALUES ('Strict', 'strict-nl', $1, TRUE, 45) RETURNING id`, user).Scan(&custom); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO projects (name, code, org_id) VALUES ('p', 'pnl', $1) RETURNING id`, dflt).Scan(&project); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, state string }{{"rs_live000000000000000000", "active"}, {"rs_gone000000000000000000", "ended"}} {
		if _, err := db.Exec(ctx, `INSERT INTO remote_sessions (id, owner_user_id, project_id, harness_id, harness_pub, state, expires_at)
			VALUES ($1, $2, $3, 'h', 'p', $4, NOW() + INTERVAL '12 hours')`, row.id, user, project, row.state); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate 031: %v", err)
	}
	if pol, err := repo.GetOrgRemotePolicy(ctx, dflt); err != nil || !pol.Enabled || pol.IdleTimeoutMin != 0 || pol.MaxLifetimeHours != 0 {
		t.Fatalf("Default org after 031: %+v %v (want enabled, no limits)", pol, err)
	}
	if pol, err := repo.GetOrgRemotePolicy(ctx, custom); err != nil || pol.IdleTimeoutMin != 45 || pol.MaxLifetimeHours != 0 {
		t.Fatalf("an admin's own idle limit after 031: %+v %v (want 45 kept)", pol, err)
	}
	live, err := repo.GetRemoteSession(ctx, "rs_live000000000000000000")
	if err != nil || live.ExpiresAt != nil {
		t.Fatalf("live session after 031: %+v %v (want no lifetime)", live, err)
	}
	gone, err := repo.GetRemoteSession(ctx, "rs_gone000000000000000000")
	if err != nil || gone.ExpiresAt == nil {
		t.Fatalf("ended session after 031: %+v %v (want its expires_at kept)", gone, err)
	}
	// A new org gets no limits; the range checks allow 0 and refuse the rest.
	var fresh int
	if err := db.QueryRow(ctx, `INSERT INTO orgs (name, slug, created_by) VALUES ('New', 'new-nl', $1) RETURNING id`, user).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if pol, err := repo.GetOrgRemotePolicy(ctx, fresh); err != nil || pol != domain.DefaultRemotePolicy() {
		t.Fatalf("new org: %+v %v", pol, err)
	}
	for _, bad := range []string{`remote_idle_timeout_min = 4`, `remote_idle_timeout_min = -1`, `remote_max_lifetime_hours = 721`,
		`remote_max_lifetime_hours = -1`} {
		if _, err := db.Exec(ctx, `UPDATE orgs SET `+bad+` WHERE id = $1`, fresh); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, good := range []string{`remote_idle_timeout_min = 30`, `remote_idle_timeout_min = 0`, `remote_max_lifetime_hours = 720`} {
		if _, err := db.Exec(ctx, `UPDATE orgs SET `+good+` WHERE id = $1`, fresh); err != nil {
			t.Errorf("%s refused: %v", good, err)
		}
	}
}

// By default a session never ends by time: registration and the heartbeat
// say so with nulls, the session list shows no expiry, and the relay's sweep
// leaves it alone a month later however long it was idle and offline.
func TestRelay_NoTimeLimitsByDefault(t *testing.T) {
	e := newRelayEnv(t, nil)
	ctx := context.Background()
	s := e.register(t, "alice")
	sess, err := e.w.s.repo.GetRemoteSession(ctx, s.id)
	if err != nil || sess.ExpiresAt != nil {
		t.Fatalf("stored session: %+v %v (want no expires_at)", sess, err)
	}
	st, body, _ := e.do(t, "GET", "/api/sessions", nil, as("alice"))
	wantStatus(t, "list", st, body, 200, "")
	list, _ := body["sessions"].([]any)
	if len(list) != 1 {
		t.Fatalf("list: %v", body)
	}
	if row := list[0].(map[string]any); row["expires_at_ms"] != nil || row["state"] != "active" {
		t.Fatalf("list row: %v", row)
	}

	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, `UPDATE remote_sessions SET last_activity_at = NOW() - INTERVAL '29 days' WHERE id = $1`, s.id); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(30 * 24 * time.Hour)
	defer e.clock.Add(-30 * 24 * time.Hour)
	e.rl.ExpireNow(ctx)
	if sess, err := e.w.s.repo.GetRemoteSession(ctx, s.id); err != nil || !sess.Active() {
		t.Fatalf("after a month idle and offline: %+v %v (want still active)", sess, err)
	}
	if a := remoteAudit(t, e.w.s.repo, e.w.org.ID, domain.AuditRemoteExpire, s.id); len(a) != 0 {
		t.Fatalf("remote.expire written: %+v", a)
	}
}

// A laptop that slept for hours (or a relay outage of any length) comes back:
// its connect token expired long ago, the refresh with the harness key's
// signed proof gives it a new one, and the command stream and the viewers'
// presence pick up where they were. With an org's opt-in idle limit the same
// gap ends the session, so the limit still works.
func TestRelay_ReconnectAfterHoursWithSignedRenewal(t *testing.T) {
	e := newRelayEnv(t, func(c *relay.Config) { c.Sweep = 20 * time.Millisecond; c.Expire = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.rl.Run(ctx)

	s := e.register(t, "alice")
	hc := s.connect(t)
	s.mustPublish(t, s.hello(t))
	v := s.watch(t, "")
	if a := dataOf(t, v.expect(t, "relay")); a["online"] != true {
		t.Fatalf("attached: %v", a)
	}

	// The laptop sleeps: its stream drops, and six hours pass.
	hc.close()
	v.waitFor(t, "relay", "offline")
	e.clock.Add(6 * time.Hour)
	defer e.clock.Add(-6 * time.Hour)
	e.rl.ExpireNow(context.Background())

	hb := "/relay/v1/sessions/" + s.id + "/heartbeat"
	st, body, _ := e.do(t, "POST", hb, nil, bearer(s.token))
	wantStatus(t, "the old token after 6 h", st, body, 401, "token_expired")
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+s.id+"/token", e.tokenProof(t, s.harness, s.id), as("alice"))
	wantStatus(t, "signed renewal after 6 h", st, body, 200, "")
	s.token = body["connect_token"].(string)
	if exp, _ := body["connect_token_expires_at_ms"].(float64); int64(exp) <= e.clock.Now().UnixMilli() {
		t.Fatalf("renewed token already expired: %v", body)
	}
	hc2 := s.connect(t)
	defer hc2.close()
	st, body, _ = e.do(t, "POST", hb, nil, bearer(s.token))
	wantStatus(t, "heartbeat after the renewal", st, body, 200, "")
	v.waitFor(t, "relay", `"online"`)
	s.mustPublish(t, s.hello(t))
	if f := v.waitFor(t, "envelope", `"hello"`); !strings.Contains(string(f.Data), s.id) {
		t.Fatalf("hello after the reconnect: %v", f)
	}

	// The same gap with an org's opt-in 30 minute idle limit ends it.
	setOrgLimits(t, e.w.s.repo, e.w.org.ID, 30, 0)
	s2 := e.register(t, "alice")
	hc3 := s2.connect(t)
	e.clock.Add(6 * time.Hour)
	defer e.clock.Add(-6 * time.Hour)
	e.rl.ExpireNow(context.Background())
	if f := hc3.waitFor(t, "end", ""); dataOf(t, f)["reason"] != "idle" {
		t.Fatalf("opt-in idle end: %v", f)
	}
	st, body, _ = e.do(t, "POST", "/relay/v1/sessions/"+s2.id+"/token", e.tokenProof(t, s2.harness, s2.id), as("alice"))
	wantStatus(t, "renewal of an idle-ended session", st, body, 410, domain.RemoteCodeSessionEnded)
	// The idle limit applies to live sessions too (a token renewal isn't
	// remote activity), so the first session ends as well: what the admin
	// asked for.
	if sess, err := e.w.s.repo.GetRemoteSession(context.Background(), s.id); err != nil || sess.Active() {
		t.Fatalf("first session under the new idle limit: %+v %v", sess, err)
	}
}
