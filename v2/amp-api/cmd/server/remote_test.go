package main

// Remote control (epic 165, AMP #2371): the migration, the org setting, the
// instance switch and capability flag, the session/device store, and the
// metadata-only, fail-closed audit trail.

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

var remoteOn = service.RemoteConfig{InstanceEnabled: true, AuthEnabled: true}

// remoteKey returns a fresh P-256 public key as b64u raw bytes, and its
// fingerprint.
func remoteKey(t *testing.T) (pub, fp string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub = base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
	fp, err = service.RemoteFingerprint(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pub, fp
}

// principalOf is the Principal the auth middleware resolves for ident on the
// web UI (channel "" means ui).
func principalOf(t *testing.T, ident *auth.Identity, channel string) authz.Principal {
	t.Helper()
	c := *ident
	c.Channel = auth.ChannelUI
	if channel != "" {
		c.Channel = channel
		c.APIToken = channel == auth.ChannelToken
	}
	p, err := authz.FromContext(auth.WithIdentity(context.Background(), &c))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func remoteService(w *rbacWorld, cfg service.RemoteConfig) *service.Service {
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	svc.Remote = cfg
	return svc
}

func enableOrg(t *testing.T, repo *repository.Repo, orgID int, on bool) {
	t.Helper()
	pol := domain.DefaultRemotePolicy()
	pol.Enabled = on
	if err := repo.SetOrgRemotePolicy(context.Background(), orgID, pol); err != nil {
		t.Fatal(err)
	}
}

// remoteAudit returns the remote.* audit entries about session id (or, with
// id "", all of the org's), newest first.
func remoteAudit(t *testing.T, repo *repository.Repo, orgID int, action, id string) []domain.AuditEntry {
	t.Helper()
	all, err := repo.ListAudit(context.Background(), domain.AuditFilter{OrgID: orgID, Action: action, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.AuditEntry
	for _, e := range all {
		if id == "" || e.TargetID == id {
			out = append(out, e)
		}
	}
	return out
}

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %d %s", status, code)
	}
	if got := authz.StatusOf(err, 500); got != status || authz.CodeOf(err) != code {
		t.Fatalf("got %d %q (%v), want %d %s", got, authz.CodeOf(err), err, status, code)
	}
}

// Migration 027 is additive: on a database with orgs and projects it only
// adds columns (remote control off for every existing org) and two empty
// tables; existing rows are untouched, and running it again changes nothing.
func TestMigration_027RemoteControlIsAdditive(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var before []repository.Migration
	var m027 *repository.Migration
	for i, m := range migrations {
		switch {
		case m.Version < "027":
			before = append(before, m)
		case strings.HasPrefix(m.Version, "027"):
			m027 = &migrations[i]
		}
	}
	if m027 == nil {
		t.Fatal("no migration 027")
	}
	if err := repo.RunMigrations(ctx, before); err != nil {
		t.Fatalf("migrate to 026: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var user, org, project int
	if err := db.QueryRow(ctx, `INSERT INTO users (subject, email) VALUES ('sub-rc', 'rc@x.test') RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO orgs (name, slug, created_by) VALUES ('Old org', 'old-org', $1) RETURNING id`, user).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO projects (name, code, org_id) VALUES ('p', 'p', $1) RETURNING id`, org).Scan(&project); err != nil {
		t.Fatal(err)
	}

	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate 027: %v", err)
	}
	if _, err := db.Exec(ctx, m027.SQL); err != nil {
		t.Fatalf("re-run 027: %v", err)
	}
	pol, err := repo.GetOrgRemotePolicy(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if pol != domain.DefaultRemotePolicy() || pol.Enabled {
		t.Fatalf("existing org's policy = %+v, want the default (off)", pol)
	}
	var name, slug string
	if err := db.QueryRow(ctx, `SELECT name, slug FROM orgs WHERE id = $1`, org).Scan(&name, &slug); err != nil || name != "Old org" || slug != "old-org" {
		t.Fatalf("existing org changed: %q %q %v", name, slug, err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM remote_sessions) + (SELECT COUNT(*) FROM remote_devices)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("new tables: %d rows, %v", n, err)
	}
	// The range checks hold.
	if _, err := db.Exec(ctx, `UPDATE orgs SET remote_idle_timeout_min = 4 WHERE id = $1`, org); err == nil {
		t.Fatal("idle timeout 4 accepted")
	}
	if _, err := db.Exec(ctx, `UPDATE orgs SET remote_device_pin_days = 31 WHERE id = $1`, org); err == nil {
		t.Fatal("pin days 31 accepted")
	}
	// A project with no org gets the default policy: off.
	var orphan int
	if err := db.QueryRow(ctx, `INSERT INTO projects (name, code) VALUES ('o', 'o') RETURNING id`).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	got, orgID, err := repo.ProjectRemotePolicy(ctx, orphan)
	if err != nil || orgID != nil || got.Enabled {
		t.Fatalf("no-org project policy = %+v org %v err %v", got, orgID, err)
	}
}

// The capability flag on /api/me, for each combination of the instance
// switch, auth, the caller and the org setting.
func TestRemote_CapabilityOnMe(t *testing.T) {
	w := newRBACWorld(t)
	agent := *w.alice
	agentUser := *w.alice.User
	agentUser.Kind = domain.UserKindAgent
	agent.User = &agentUser

	capOf := func(ra *restAs, channel, who string) map[string]any {
		t.Helper()
		code, out := ra.doVia(t, channel, who, "GET", "/api/me", nil)
		if code != 200 {
			t.Fatalf("/api/me as %s: %d %v", who, code, out)
		}
		caps, _ := out["capabilities"].(map[string]any)
		rc, ok := caps["remote_control"].(map[string]any)
		if !ok {
			t.Fatalf("/api/me has no capabilities.remote_control: %v", out)
		}
		return rc
	}
	want := func(rc map[string]any, available bool, reason any, orgs []int) {
		t.Helper()
		if rc["available"] != available || rc["reason"] != reason {
			t.Fatalf("remote_control = %v, want available=%v reason=%v", rc, available, reason)
		}
		var got []int
		for _, v := range rc["org_ids"].([]any) {
			got = append(got, int(v.(float64)))
		}
		if !slices.Equal(got, orgs) {
			t.Fatalf("org_ids = %v, want %v", got, orgs)
		}
	}

	// Default (instance switch off): off for everyone, even with the org on.
	enableOrg(t, w.s.repo, w.org.ID, true)
	def := newRestAs(t, w)
	want(capOf(def, "", "alice"), false, domain.RemoteUnavailableInstance, nil)
	enableOrg(t, w.s.repo, w.org.ID, false)

	// Instance on, auth off (compose / local dev).
	noAuth := newRestAsWith(t, w, func(h *api.RestHandler) {
		h.SetRemoteControl(service.RemoteConfig{InstanceEnabled: true})
	})
	want(capOf(noAuth, "", "alice"), false, domain.RemoteUnavailableAuth, nil)

	on := newRestAsWith(t, w, func(h *api.RestHandler) { h.SetRemoteControl(remoteOn) })
	on.ids["agent"] = &agent
	// Org setting off (the default).
	want(capOf(on, "", "alice"), false, domain.RemoteUnavailableNoOrg, nil)
	// Org setting on: available for its members, with the org listed.
	enableOrg(t, w.s.repo, w.org.ID, true)
	want(capOf(on, "", "alice"), true, nil, []int{w.org.ID})
	want(capOf(on, "", "dave"), true, nil, []int{w.org.ID})
	// Not for someone outside the org, an API token, or an agent account.
	want(capOf(on, "", "outsider"), false, domain.RemoteUnavailableNoOrg, nil)
	want(capOf(on, auth.ChannelToken, "alice"), false, domain.RemoteUnavailablePerson, nil)
	want(capOf(on, "", "agent"), false, domain.RemoteUnavailablePerson, nil)
	// Both orgs on: both listed.
	enableOrg(t, w.s.repo, w.other.ID, true)
	want(capOf(on, "", "outsider"), true, nil, []int{w.other.ID})
}

// GET/PUT /api/orgs/:id/remote-control: org admins read and change it, only
// from the web UI; values are range-checked; every change is audited; off
// ends the org's live sessions.
func TestRemote_OrgSettingEndpoint(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAsWith(t, w, func(h *api.RestHandler) { h.SetRemoteControl(remoteOn) })
	path := fmt.Sprintf("/api/orgs/%d/remote-control", w.org.ID)

	got := ra.want(t, 200, "orgadmin", "GET", path, nil)
	if got["enabled"] != false || got["idle_timeout_min"] != float64(30) || got["device_pin_days"] != float64(30) {
		t.Fatalf("default policy = %v", got)
	}
	ra.want(t, 403, "alice", "GET", path, nil)
	// Outside the org it doesn't exist.
	ra.want(t, 404, "outsider", "PUT", path, map[string]any{"enabled": true})
	ra.want(t, 403, "dave", "PUT", path, map[string]any{"enabled": true})

	// Not over an API token or MCP, even for an org admin.
	code, out := ra.doVia(t, auth.ChannelToken, "orgadmin", "PUT", path, map[string]any{"enabled": true})
	if code != 403 || out["code"] != domain.RemoteCodeNotUIChannel {
		t.Fatalf("PUT via token: %d %v", code, out)
	}
	code, out = ra.do(t, "orgadmin", "PUT", path, map[string]any{"enabled": true, "idle_timeout_min": 4})
	if code != 400 || out["code"] != domain.RemoteCodeMalformed || out["error"] == "" {
		t.Fatalf("PUT idle 4: %d %v", code, out)
	}
	got = ra.want(t, 200, "orgadmin", "PUT", path, map[string]any{"enabled": true, "device_pin_days": 0})
	if got["enabled"] != true || got["device_pin_days"] != float64(0) || got["idle_timeout_min"] != float64(30) {
		t.Fatalf("PUT on = %v", got)
	}
	ra.want(t, 200, "root", "GET", path, nil)

	sets := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemotePolicySet, "")
	if len(sets) != 1 || sets[0].Details["enabled"] != true || sets[0].Details["was_enabled"] != false {
		t.Fatalf("remote.policy.set entries: %+v", sets)
	}

	// A live session, then the admin turns it off: the session ends.
	svc := remoteService(w, remoteOn)
	pub, fp := remoteKey(t)
	sess, err := svc.EnableRemoteSession(context.Background(), principalOf(t, w.alice, ""),
		service.RemoteRegistration{ProjectID: w.s.projectID, HarnessID: fp, HarnessPub: pub, Host: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	ra.want(t, 200, "orgadmin", "PUT", path, map[string]any{"enabled": false})
	after, err := w.s.repo.GetRemoteSession(context.Background(), sess.ID)
	if err != nil || after.Active() || after.EndReason == nil || *after.EndReason != domain.RemoteEndPolicyOff {
		t.Fatalf("session after policy off: %+v %v", after, err)
	}
	if d := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemoteDisable, sess.ID); len(d) != 1 || d[0].Details["reason"] != domain.RemoteEndPolicyOff {
		t.Fatalf("remote.disable entries: %+v", d)
	}
}

// Registering a session: who may, on what, and the remote.enable audit entry.
func TestRemote_EnableSession(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	svc := remoteService(w, remoteOn)
	alice := principalOf(t, w.alice, "")
	pub, fp := remoteKey(t)
	reg := service.RemoteRegistration{ProjectID: w.s.projectID, HarnessID: fp, HarnessPub: pub,
		Host: "jacobs-mbp", OS: "macos", HarnessVersion: "0.4.0", ProtocolVersion: 1}

	// Org setting off (default): refused.
	_, err := svc.EnableRemoteSession(ctx, alice, reg)
	wantCode(t, err, 403, domain.RemoteCodeDisabled)
	enableOrg(t, w.s.repo, w.org.ID, true)

	// Instance switch off, or auth off: as if the route didn't exist.
	_, err = remoteService(w, service.RemoteConfig{}).EnableRemoteSession(ctx, alice, reg)
	wantCode(t, err, 404, domain.RemoteCodeNotFound)
	_, err = remoteService(w, service.RemoteConfig{InstanceEnabled: true}).EnableRemoteSession(ctx, alice, reg)
	wantCode(t, err, 404, domain.RemoteCodeNotFound)

	// Not an API token, not an agent account.
	_, err = svc.EnableRemoteSession(ctx, principalOf(t, w.alice, auth.ChannelToken), reg)
	wantCode(t, err, 403, domain.RemoteCodeTokenNotAllowed)
	agent := alice
	agent.Agent = true
	_, err = svc.EnableRemoteSession(ctx, agent, reg)
	wantCode(t, err, 403, domain.RemoteCodeAgentNotAllowed)

	// The harness id must be its key's fingerprint.
	bad := reg
	bad.HarnessID = strings.Repeat("A", 43)
	_, err = svc.EnableRemoteSession(ctx, alice, bad)
	wantCode(t, err, 400, domain.RemoteCodeMalformed)
	bad = reg
	bad.HarnessPub = "not-a-key"
	_, err = svc.EnableRemoteSession(ctx, alice, bad)
	wantCode(t, err, 400, domain.RemoteCodeMalformed)

	// A project the caller can't see is not found.
	_, err = svc.EnableRemoteSession(ctx, principalOf(t, w.outsider, ""), reg)
	wantCode(t, err, 404, domain.RemoteCodeNotFound)

	sess, err := svc.EnableRemoteSession(ctx, alice, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.ID, "rs_") || len(sess.ID) != 25 || sess.OwnerUserID != w.alice.User.ID ||
		!sess.Active() || sess.Host != "jacobs-mbp" || sess.ExpiresAt.Sub(sess.StartedAt) < 11*time.Hour {
		t.Fatalf("session = %+v", sess)
	}
	en := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemoteEnable, sess.ID)
	if len(en) != 1 {
		t.Fatalf("remote.enable entries: %+v", en)
	}
	e := en[0]
	if e.ActorUserID == nil || *e.ActorUserID != w.alice.User.ID || e.ProjectID == nil || *e.ProjectID != w.s.projectID ||
		e.OrgID == nil || *e.OrgID != w.org.ID {
		t.Fatalf("remote.enable attribution: %+v", e)
	}
	for k, want := range map[string]any{"session_id": sess.ID, "harness_id": fp, "key_fp": fp, "harness_host": "jacobs-mbp", "op": "register", "device_id": nil} {
		if e.Details[k] != want {
			t.Fatalf("details[%s] = %v, want %v (%v)", k, e.Details[k], want, e.Details)
		}
	}

	// Owner only: someone else's session is not found, like a missing one.
	_, err = svc.OwnRemoteSession(ctx, principalOf(t, w.bob, ""), sess.ID)
	wantCode(t, err, 404, domain.RemoteCodeNotFound)
	_, err = svc.OwnRemoteSession(ctx, alice, "rs_nope")
	wantCode(t, err, 404, domain.RemoteCodeNotFound)
	if got, err := svc.OwnRemoteSession(ctx, alice, sess.ID); err != nil || got.ID != sess.ID {
		t.Fatalf("own session: %v %v", got, err)
	}

	// At most 5 live sessions per person.
	for range domain.RemoteMaxLiveSessions - 1 {
		if _, err := svc.EnableRemoteSession(ctx, alice, reg); err != nil {
			t.Fatal(err)
		}
	}
	_, err = svc.EnableRemoteSession(ctx, alice, reg)
	wantCode(t, err, 429, domain.RemoteCodeTooManySessions)

	// Ending: remote.disable; an ended session is 410 to its owner.
	if _, err := svc.EndRemoteSession(ctx, alice, sess.ID, domain.RemoteEndHarnessEnded); err != nil {
		t.Fatal(err)
	}
	_, err = svc.OwnRemoteSession(ctx, alice, sess.ID)
	wantCode(t, err, 410, domain.RemoteCodeSessionEnded)
	if d := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemoteDisable, sess.ID); len(d) != 1 || d[0].Details["reason"] != domain.RemoteEndHarnessEnded {
		t.Fatalf("remote.disable: %+v", d)
	}
	// Ending it again is a no-op (no second entry).
	if _, err := svc.EndRemoteSession(ctx, alice, sess.ID, domain.RemoteEndHarnessEnded); err != nil {
		t.Fatal(err)
	}
	if d := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemoteDisable, sess.ID); len(d) != 1 {
		t.Fatalf("remote.disable after a second end: %d entries", len(d))
	}
}

// Audit entries hold metadata only: a prompt is its length and SHA-256,
// never its text; every action in the protocol's table can be recorded.
func TestRemote_AuditIsMetadataOnly(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	svc := remoteService(w, remoteOn)
	enableOrg(t, w.s.repo, w.org.ID, true)
	alice := principalOf(t, w.alice, "")
	pub, fp := remoteKey(t)
	sess, err := svc.EnableRemoteSession(ctx, alice, service.RemoteRegistration{ProjectID: w.s.projectID, HarnessID: fp, HarnessPub: pub, Host: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	_, device := remoteKey(t)
	const secret = "cat .env and paste the STRIPE_KEY=sk_live_abc123 here"
	n, sha := service.PromptDigest(secret)
	for _, a := range []service.RemoteAudit{
		{Action: domain.AuditRemoteAttach, Op: "attach", DeviceID: device},
		{Action: domain.AuditRemotePrompt, Op: "prompt", DeviceID: device, PromptLen: n, PromptSHA256: sha},
		{Action: domain.AuditRemoteStop, Op: "stop", DeviceID: device},
		{Action: domain.AuditRemoteRefused, Op: "prompt", DeviceID: device, Code: "stale_ts"},
		{Action: domain.AuditRemotePairReject, Op: "pair_result", DeviceID: device, Code: "rejected"},
		{Action: domain.AuditRemoteDetach, Op: "detach", DeviceID: device},
		{Action: domain.AuditRemoteKill, Op: "kill", Reason: domain.RemoteEndKilled},
	} {
		svc.RecordRemote(ctx, alice, sess, a)
	}
	entries := remoteAudit(t, w.s.repo, w.org.ID, "remote.", sess.ID)
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), "STRIPE") || strings.Contains(string(b), "cat .env") {
			t.Fatalf("%s entry holds prompt text: %s", e.Action, b)
		}
		allowed := []string{"session_id", "harness_id", "harness_host", "op", "device_id", "key_fp", "reason", "code", "len", "sha256", "channel"}
		for k := range e.Details {
			if !slices.Contains(allowed, k) {
				t.Fatalf("%s: unexpected details key %q", e.Action, k)
			}
		}
	}
	for _, a := range []string{domain.AuditRemoteEnable, domain.AuditRemoteAttach, domain.AuditRemotePrompt, domain.AuditRemoteStop,
		domain.AuditRemoteRefused, domain.AuditRemotePairReject, domain.AuditRemoteDetach, domain.AuditRemoteKill} {
		if !slices.Contains(actions, a) {
			t.Fatalf("no %s entry in %v", a, actions)
		}
	}
	p := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemotePrompt, sess.ID)[0]
	if p.Details["len"] != float64(len(secret)) || p.Details["sha256"] != sha || len(sha) != 64 || p.Details["key_fp"] != device {
		t.Fatalf("remote.prompt details = %v", p.Details)
	}
}

// remote.enable and remote.pair fail closed: when the audit insert fails the
// action is refused with 503 audit_unavailable and nothing is left behind.
// Other remote entries stay best effort.
func TestRemote_EnableAndPairFailClosed(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	svc := remoteService(w, remoteOn)
	enableOrg(t, w.s.repo, w.org.ID, true)
	alice := principalOf(t, w.alice, "")
	pub, fp := remoteKey(t)
	reg := service.RemoteRegistration{ProjectID: w.s.projectID, HarnessID: fp, HarnessPub: pub, Host: "laptop"}
	sess, err := svc.EnableRemoteSession(ctx, alice, reg) // before the audit log breaks
	if err != nil {
		t.Fatal(err)
	}

	// Break audit inserts for alice's remote.* entries only (other tests share
	// the database).
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	sfx := fmt.Sprint(time.Now().UnixNano())
	fn, trig := "rc_audit_fail_"+sfx, "rc_audit_fail_trg_"+sfx
	if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.actor_email = %s AND NEW.action LIKE 'remote.%%' THEN
				RAISE EXCEPTION 'audit log unavailable (test)';
			END IF;
			RETURN NEW;
		END $$`, fn, pgQuote(w.alice.Email))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION %s()`, trig, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db2, err := pgx.Connect(context.Background(), testDSN(t))
		if err != nil {
			return
		}
		defer db2.Close(context.Background())
		db2.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON audit_log`, trig)) //nolint:errcheck
		db2.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))             //nolint:errcheck
	})

	live, _ := w.s.repo.CountActiveRemoteSessions(ctx, w.alice.User.ID)
	_, err = svc.EnableRemoteSession(ctx, alice, reg)
	wantCode(t, err, 503, domain.RemoteCodeAuditUnavailable)
	if now, _ := w.s.repo.CountActiveRemoteSessions(ctx, w.alice.User.ID); now != live {
		t.Fatalf("live sessions %d → %d: the refused registration left a session behind", live, now)
	}
	_, device := remoteKey(t)
	err = svc.RecordRemotePair(ctx, alice, sess, device)
	wantCode(t, err, 503, domain.RemoteCodeAuditUnavailable)
	// Best-effort entries don't fail the action.
	svc.RecordRemote(ctx, alice, sess, service.RemoteAudit{Action: domain.AuditRemoteStop, Op: "stop", DeviceID: device})
	if _, err := svc.EndRemoteSession(ctx, alice, sess.ID, domain.RemoteEndDetached); err != nil {
		t.Fatalf("end (best-effort audit) failed: %v", err)
	}

	// With the audit log back, pairing is recorded.
	if _, err := db.Exec(ctx, fmt.Sprintf(`DROP TRIGGER %s ON audit_log`, trig)); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordRemotePair(ctx, alice, sess, device); err != nil {
		t.Fatal(err)
	}
	if p := remoteAudit(t, w.s.repo, w.org.ID, domain.AuditRemotePair, sess.ID); len(p) != 1 || p[0].Details["device_id"] != device {
		t.Fatalf("remote.pair: %+v", p)
	}
}

func pgQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// The store: connect tokens, heartbeats, the holding replica, devices with
// their pin expiry, revoke-all, and the expiry sweep.
func TestRemote_SessionAndDeviceStore(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	repo := w.s.repo
	svc := remoteService(w, remoteOn)
	enableOrg(t, repo, w.org.ID, true)
	alice := principalOf(t, w.alice, "")
	pub, fp := remoteKey(t)
	reg := service.RemoteRegistration{ProjectID: w.s.projectID, HarnessID: fp, HarnessPub: pub, Host: "laptop", Node: "node-a"}
	sess, err := svc.EnableRemoteSession(ctx, alice, reg)
	if err != nil {
		t.Fatal(err)
	}
	if sess.HolderNode == nil || *sess.HolderNode != "node-a" {
		t.Fatalf("holder = %v", sess.HolderNode)
	}

	// Connect token: found by hash, never returned.
	exp := time.Now().Add(15 * time.Minute)
	if err := repo.SetRemoteConnectToken(ctx, sess.ID, "hash-"+sess.ID, exp); err != nil {
		t.Fatal(err)
	}
	byTok, err := repo.RemoteSessionByConnectToken(ctx, "hash-"+sess.ID)
	if err != nil || byTok.ID != sess.ID || byTok.ConnectTokenExpiresAt == nil {
		t.Fatalf("by token: %+v %v", byTok, err)
	}
	if ok, err := repo.ClaimRemoteSession(ctx, sess.ID, "node-b"); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := repo.TouchRemoteHeartbeat(ctx, sess.ID); !ok || err != nil {
		t.Fatalf("heartbeat: %v %v", ok, err)
	}
	if ok, err := repo.TouchRemoteActivity(ctx, sess.ID); !ok || err != nil {
		t.Fatalf("activity: %v %v", ok, err)
	}
	got, _ := repo.GetRemoteSession(ctx, sess.ID)
	if *got.HolderNode != "node-b" || got.HeartbeatAt == nil {
		t.Fatalf("after claim: %+v", got)
	}
	if l, _ := repo.ListActiveRemoteSessions(ctx, w.alice.User.ID); len(l) != 1 || l[0].ID != sess.ID {
		t.Fatalf("active list: %+v", l)
	}

	// Devices: pinned for the org's device_pin_days (30), re-pairing refreshes.
	devPub, devID := remoteKey(t)
	d, err := svc.PinRemoteDevice(ctx, sess, devID, devPub, "Safari on iPhone")
	if err != nil {
		t.Fatal(err)
	}
	if days := d.ExpiresAt.Sub(d.PairedAt).Hours() / 24; days < 29.9 || days > 30.1 {
		t.Fatalf("pin lasts %.1f days, want 30", days)
	}
	if _, err := svc.PinRemoteDevice(ctx, sess, devID, pub, "wrong key"); authz.CodeOf(err) != domain.RemoteCodeMalformed {
		t.Fatalf("pin with a key that isn't the device id's: %v", err)
	}
	// Org pin days 0: until the session ends.
	pol := domain.DefaultRemotePolicy()
	pol.Enabled, pol.DevicePinDays = true, 0
	if err := repo.SetOrgRemotePolicy(ctx, w.org.ID, pol); err != nil {
		t.Fatal(err)
	}
	dev2Pub, dev2ID := remoteKey(t)
	d2, err := svc.PinRemoteDevice(ctx, sess, dev2ID, dev2Pub, "Chrome on macOS")
	if err != nil || !d2.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Fatalf("pin with 0 days: %+v %v (session expires %v)", d2, err, sess.ExpiresAt)
	}
	if l, _ := repo.ListRemoteDevices(ctx, w.alice.User.ID, fp); len(l) != 2 {
		t.Fatalf("devices: %+v", l)
	}
	if n, err := repo.PruneRemoteDevices(ctx, w.alice.User.ID, fp, []string{devID}); n != 1 || err != nil {
		t.Fatalf("prune: %d %v", n, err)
	}

	// Revoke all: sessions end (revoked), devices revoked, both audited.
	ended, devs, err := svc.RevokeAllRemote(ctx, alice)
	if err != nil || len(ended) != 1 || devs != 1 {
		t.Fatalf("revoke all: %d sessions, %d devices, %v", len(ended), devs, err)
	}
	if l, _ := repo.ListRemoteDevices(ctx, w.alice.User.ID, ""); len(l) != 0 {
		t.Fatalf("devices after revoke: %+v", l)
	}
	if _, err := repo.RemoteSessionByConnectToken(ctx, "hash-"+sess.ID); err == nil {
		t.Fatal("an ended session's connect token still resolves")
	}
	if ok, _ := repo.TouchRemoteHeartbeat(ctx, sess.ID); ok {
		t.Fatal("heartbeat on an ended session")
	}
	// Revoke-all is about no one session (no project), so look it up by actor.
	revokes, _ := repo.ListAudit(ctx, domain.AuditFilter{ActorUserID: w.alice.User.ID, Action: domain.AuditRemoteDeviceRevoke})
	if len(revokes) != 1 || revokes[0].Details["devices"] != float64(1) || revokes[0].Details["sessions"] != float64(1) {
		t.Fatalf("remote.device.revoke: %+v", revokes)
	}
	if d := remoteAudit(t, repo, w.org.ID, domain.AuditRemoteDisable, sess.ID); len(d) != 1 || d[0].Details["reason"] != domain.RemoteEndRevoked {
		t.Fatalf("remote.disable on revoke-all: %+v", d)
	}

	// The sweep: a session past its lifetime ends as expired, one idle past
	// its org's timeout (+ grace) as idle, a fresh one stays.
	mk := func() *domain.RemoteSession {
		s, err := svc.EnableRemoteSession(ctx, alice, reg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	old, idle, fresh := mk(), mk(), mk()
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, `UPDATE remote_sessions SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE remote_sessions SET last_activity_at = NOW() - INTERVAL '32 minutes' WHERE id = $1`, idle.ID); err != nil {
		t.Fatal(err)
	}
	gone, err := svc.ExpireRemoteSessions(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, s := range gone {
		reasons[s.ID] = *s.EndReason
	}
	if reasons[old.ID] != domain.RemoteEndExpired || reasons[idle.ID] != domain.RemoteEndIdle || reasons[fresh.ID] != "" {
		t.Fatalf("sweep ended %v", reasons)
	}
	if e := remoteAudit(t, repo, w.org.ID, domain.AuditRemoteExpire, idle.ID); len(e) != 1 || e[0].Details["reason"] != domain.RemoteEndIdle {
		t.Fatalf("remote.expire: %+v", e)
	}
}
