package main

// Audit log integrity (#2429, migration 033): append-only for every role,
// the per-chain hash chain, signed checkpoints, verify (service, REST and
// CLI), retention through anchors, and the durable audit outbox.
//
// Tampering tests run in their own scratch database (freshDatabase), so
// they never break the shared test database's chains.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/auditchain"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
	"github.com/simstech/amp-api/internal/webhook"
)

// integrityDB is a migrated scratch database.
type integrityDB struct {
	dsn  string
	repo *repository.Repo
	db   *pgx.Conn
	svc  *service.Service
}

func newIntegrityDB(t *testing.T) *integrityDB {
	t.Helper()
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	return &integrityDB{dsn: dsn, repo: repo, db: db, svc: service.New(repo, nil, nil)}
}

func (d *integrityDB) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := d.db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// write adds an audit entry for org (0: none) through the app's writer.
func (d *integrityDB) write(t *testing.T, org int, action string) *domain.AuditEntry {
	t.Helper()
	e := domain.AuditEntry{Action: action, ActorEmail: "a@x", Summary: action}
	if org != 0 {
		e.OrgID = &org
	}
	got, err := d.repo.InsertAudit(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// writeAt adds an entry with a given time (retention tests).
func (d *integrityDB) writeAt(t *testing.T, org *int, action string, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := d.db.QueryRow(context.Background(), `INSERT INTO audit_log (at, org_id, action, actor_email) VALUES ($1, $2, $3, 'a@x') RETURNING id`,
		at, org, action).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (d *integrityDB) verify(t *testing.T) auditchain.Report {
	t.Helper()
	keys, err := d.repo.AuditChainKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := service.VerifyAuditChains(context.Background(), d.repo, service.AuditKeys, keys)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func problemKinds(r auditchain.Report) string {
	var k []string
	for _, c := range r.Chains {
		for _, p := range c.Problems {
			k = append(k, c.ChainKey+":"+p.Kind)
		}
	}
	return strings.Join(k, ",")
}

// withSigningKey makes service.AuditKeys a fresh keyring for the test.
func withSigningKey(t *testing.T) *auditchain.Keyring {
	t.Helper()
	seed, _, _, err := auditchain.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auditchain.LoadKeyring(func(k string) string { return map[string]string{auditchain.EnvSigningKey: seed}[k] })
	if err != nil {
		t.Fatal(err)
	}
	prev := service.AuditKeys
	service.AuditKeys = keys
	t.Cleanup(func() { service.AuditKeys = prev })
	return keys
}

func wantPrivilegeError(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("%s: want insufficient_privilege, got %v", what, err)
	}
}

// The text Go hashes is exactly the text the trigger hashed, for awkward
// values too: escapes, unicode, numbers jsonb normalises or keeps, inet
// with and without a mask, NULLs, and a session in another time zone.
func TestAuditIntegrity_CanonicalMatchesPostgres(t *testing.T) {
	d := newIntegrityDB(t)
	ctx := context.Background()
	d.exec(t, `SET TIME ZONE 'America/Denver'`)
	d.exec(t, `INSERT INTO audit_log (actor_email, org_id, project_id, action, target_type, target_id, summary, details,
		schema_version, actor_type, auth_method, token_id, session_id, on_behalf_of_user_id, agent_profile, target_name,
		outcome, reason, source_ip, user_agent, client, request_id, trace_id, "before", "after", via_token)
		VALUES ('ünï "q" \ x', 3, 9, 'plan.approve', 't', '1', E'line1\nline2\ttab\u0001\u001f/<>&',
		'{"n": 1.50, "e": 1e3, "neg": -0.0, "big": 123456789012345678901234567890, "nested": {"z": [1, "two", null, true], "a": {}}, "日本": "語"}',
		2, 'agent', 'api_token', 77, 'sess', 5, 'amp:worker', 'Thing', 'denied', 'no "way"', '10.1.2.0/24', 'ua', 'mcp', 'req', 'tr',
		'{"k": "v"}', '{"k": null}', true)`)
	d.write(t, 0, "user.create")
	d.exec(t, `INSERT INTO audit_log (action, source_ip, at) VALUES ('ip6', '2001:db8::1', '2026-10-26 12:00:00.5+00')`)

	var checked int
	err := d.repo.ReadAuditChain(ctx, "org:3", func(*auditchain.Head, []auditchain.Checkpoint) {}, func(r auditchain.Row) {
		checked++
		var pgText string
		if err := d.db.QueryRow(ctx, `SELECT audit_canonical(a) FROM audit_log a WHERE id = $1`, r.ID).Scan(&pgText); err != nil {
			t.Fatal(err)
		}
		if got := auditchain.Canonical(r); got != pgText {
			t.Errorf("row %d canonical differs\n go: %s\n pg: %s", r.ID, got, pgText)
		}
	})
	if err != nil || checked != 1 {
		t.Fatalf("read org:3: %d rows, %v", checked, err)
	}
	if r := d.verify(t); !r.OK || len(r.Chains) != 2 {
		t.Fatalf("verify: %+v", r)
	}
}

// Migration 033 chains the rows written before it, in id order.
func TestAuditIntegrity_BackfillsExistingRows(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	var pre []repository.Migration
	for _, m := range migrations {
		if m.Version < "033" {
			pre = append(pre, m)
		}
	}
	if err := repo.RunMigrations(ctx, pre); err != nil {
		t.Fatal(err)
	}
	org := 4
	for i := 0; i < 3; i++ {
		if _, err := repo.InsertAudit(ctx, domain.AuditEntry{Action: "old", OrgID: &org, Summary: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.InsertAudit(ctx, domain.AuditEntry{Action: "old.global"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.InsertAudit(ctx, domain.AuditEntry{Action: "new", OrgID: &org}); err != nil {
		t.Fatal(err)
	}
	r, err := service.VerifyAuditChains(ctx, repo, nil, []string{"global", "org:4"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.Chains[0].Rows != 3 || r.Chains[1].Rows != 4 || r.Chains[1].LastSeq != 4 {
		t.Fatalf("after backfill: %+v", r)
	}
}

// No role, owner or superuser included, can change or remove audit rows or
// checkpoints by SQL; a role with only INSERT can still write (the chain
// and outbox triggers need no rights of their own).
func TestAuditIntegrity_AppendOnly(t *testing.T) {
	d := newIntegrityDB(t)
	ctx := context.Background()
	e := d.write(t, 1, "org.rename")
	d.write(t, 1, "org.rename")
	if _, err := d.svc.WriteAuditCheckpoints(ctx); err != nil {
		t.Fatal(err)
	}

	// An app role with every table privilege, like prod's owner `amp`.
	role := fmt.Sprintf("amp_app_%d", time.Now().UnixNano())
	d.exec(t, `CREATE ROLE `+role+` LOGIN PASSWORD 'pw'`)
	t.Cleanup(func() {
		admin, err := pgx.Connect(context.Background(), testDSN(t))
		if err == nil {
			admin.Exec(context.Background(), `DROP OWNED BY `+role)       //nolint:errcheck
			admin.Exec(context.Background(), `DROP ROLE IF EXISTS `+role) //nolint:errcheck
			admin.Close(context.Background())
		}
	})
	d.exec(t, `GRANT ALL ON audit_log, audit_checkpoints TO `+role)
	d.exec(t, `GRANT USAGE ON SEQUENCE audit_log_id_seq TO `+role)
	u, _ := url.Parse(d.dsn)
	u.User = url.UserPassword(role, "pw")
	app, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as app role: %v", err)
	}
	defer app.Close(ctx)

	for name, conn := range map[string]*pgx.Conn{"superuser": d.db, "app role": app} {
		for _, sql := range []string{
			`UPDATE audit_log SET summary = 'rewritten' WHERE id = $1`,
			`UPDATE audit_log SET actor_user_id = NULL WHERE id = $1`, // only the FK may do this
			`DELETE FROM audit_log WHERE id = $1`,
		} {
			_, err := conn.Exec(ctx, sql, e.ID)
			wantPrivilegeError(t, name+": "+sql, err)
		}
		_, err := conn.Exec(ctx, `TRUNCATE audit_log CASCADE`)
		wantPrivilegeError(t, name+": truncate", err)
		_, err = conn.Exec(ctx, `UPDATE audit_checkpoints SET hash = '\x00'`)
		wantPrivilegeError(t, name+": update checkpoint", err)
		_, err = conn.Exec(ctx, `DELETE FROM audit_checkpoints`)
		wantPrivilegeError(t, name+": delete checkpoint", err)
		_, err = conn.Exec(ctx, `TRUNCATE audit_checkpoints`)
		wantPrivilegeError(t, name+": truncate checkpoints", err)
		// Setting the purge flag by hand doesn't help without an anchor.
		tx, _ := conn.Begin(ctx)
		tx.Exec(ctx, `SELECT set_config('amp.audit_purge', 'on', true)`) //nolint:errcheck
		_, err = tx.Exec(ctx, `DELETE FROM audit_log WHERE id = $1`, e.ID)
		tx.Rollback(ctx) //nolint:errcheck
		wantPrivilegeError(t, name+": delete with the purge flag", err)
	}
	// INSERT works for the app role, and its row is chained and queued.
	var id, seq int64
	if err := app.QueryRow(ctx, `INSERT INTO audit_log (org_id, action) VALUES (1, 'by.app') RETURNING id, chain_seq`).Scan(&id, &seq); err != nil {
		t.Fatalf("app role insert: %v", err)
	}
	var queued bool
	if err := d.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_outbox WHERE audit_id = $1)`, id).Scan(&queued); err != nil || !queued || seq == 0 {
		t.Fatalf("app row: seq %d queued %v err %v", seq, queued, err)
	}
	if r := d.verify(t); !r.OK {
		t.Fatalf("verify after refused writes: %s", problemKinds(r))
	}
}

// Deleting a user still works: the users FK sets actor_user_id to NULL
// through the guard, and the chain still verifies (actor_user_id isn't
// hashed; actor_email keeps who it was).
func TestAuditIntegrity_UserDeleteStillWorks(t *testing.T) {
	d := newIntegrityDB(t)
	ctx := context.Background()
	if _, err := d.repo.UpsertUserFromClaims(ctx, "sub-keep", "keep@x", "Keep", map[string]bool{"keep@x": true}); err != nil {
		t.Fatal(err)
	}
	u, err := d.repo.UpsertUserFromClaims(ctx, "sub-gone", "gone@x", "Gone", nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := d.repo.InsertAudit(ctx, domain.AuditEntry{ActorUserID: &u.ID, ActorEmail: u.Email, Action: "token.create"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.repo.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("delete user with audit rows: %v", err)
	}
	var actor *int
	var email string
	if err := d.db.QueryRow(ctx, `SELECT actor_user_id, actor_email FROM audit_log WHERE id = $1`, e.ID).Scan(&actor, &email); err != nil {
		t.Fatal(err)
	}
	if actor != nil || email != "gone@x" {
		t.Fatalf("after user delete: actor %v email %q", actor, email)
	}
	if r := d.verify(t); !r.OK {
		t.Fatalf("verify after user delete: %s", problemKinds(r))
	}
}

// verify (service and CLI) finds each kind of tampering by someone who can
// turn the guard off: an altered row, a deleted row, a reordered seq, the
// newest rows deleted, and history rewritten and re-hashed after a signed
// checkpoint.
func TestAuditIntegrity_VerifyDetectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		tamper string
		want   string
	}{
		{"altered row", `UPDATE audit_log SET summary = 'nothing happened' WHERE chain_key = 'org:42' AND chain_seq = 2`, "org:42:altered"},
		{"deleted row", `DELETE FROM audit_log WHERE chain_key = 'org:42' AND chain_seq = 3`, "org:42:missing"},
		{"reordered seq", `UPDATE audit_log SET chain_seq = -2 WHERE chain_key = 'org:42' AND chain_seq = 2;
			UPDATE audit_log SET chain_seq = 2 WHERE chain_key = 'org:42' AND chain_seq = 3;
			UPDATE audit_log SET chain_seq = 3 WHERE chain_key = 'org:42' AND chain_seq = -2`, "org:42:altered"},
		{"deleted newest rows", `DELETE FROM audit_log WHERE chain_key = 'org:42' AND chain_seq >= 5`, "org:42:missing_tail"},
		{"rewritten and re-hashed", `UPDATE audit_log SET summary = 'nothing happened' WHERE chain_key = 'org:42' AND chain_seq = 2;
			DO $$ DECLARE r audit_log; h bytea;
			BEGIN
				SELECT hash INTO h FROM audit_log WHERE chain_key = 'org:42' AND chain_seq = 1;
				FOR r IN SELECT * FROM audit_log WHERE chain_key = 'org:42' AND chain_seq > 1 ORDER BY chain_seq LOOP
					r.prev_hash := h; r.hash := audit_link_hash(h, audit_canonical(r)); h := r.hash;
					UPDATE audit_log SET prev_hash = r.prev_hash, hash = r.hash WHERE id = r.id;
				END LOOP;
				UPDATE audit_chain_heads SET hash = h WHERE chain_key = 'org:42';
			END $$`, "org:42:checkpoint_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSigningKey(t)
			d := newIntegrityDB(t)
			for i := 0; i < 4; i++ {
				d.write(t, 42, fmt.Sprintf("team.create.%d", i))
			}
			d.write(t, 0, "user.create")
			// Signs org:42 at seq 4 and writes its announcement as seq 5.
			if n, err := d.svc.WriteAuditCheckpoints(context.Background()); err != nil || n != 2 {
				t.Fatalf("checkpoints: %d %v", n, err)
			}
			before := d.verify(t)
			if !before.OK || !before.Signing.Configured || before.Chains[1].SignedCheckpoints != 1 || before.Chains[1].CheckpointsMatched != 1 {
				t.Fatalf("before tampering: %+v", before)
			}
			t.Setenv("DATABASE_URL", d.dsn)
			t.Setenv(auditchain.EnvSigningKey, "")
			t.Setenv(auditchain.EnvPublicKeys, service.AuditKeys.PublicKey())
			var out, errOut bytes.Buffer
			if code := runAuditCLI([]string{"verify"}, &out, &errOut); code != 0 {
				t.Fatalf("CLI verify before tampering: exit %d\n%s%s", code, out.String(), errOut.String())
			}

			d.exec(t, `ALTER TABLE audit_log DISABLE TRIGGER USER`)
			d.exec(t, tc.tamper)
			d.exec(t, `ALTER TABLE audit_log ENABLE TRIGGER USER`)

			after := d.verify(t)
			if after.OK || !strings.Contains(problemKinds(after), tc.want) {
				t.Fatalf("want %s, got ok=%v %s", tc.want, after.OK, problemKinds(after))
			}
			if after.Chains[0].ChainKey != "global" || !after.Chains[0].OK {
				t.Errorf("the untouched global chain: %+v", after.Chains[0])
			}
			out.Reset()
			if code := runAuditCLI([]string{"verify", "-org", "42"}, &out, &errOut); code != 1 || !strings.Contains(out.String(), "TAMPERING") {
				t.Fatalf("CLI verify after tampering: exit %d\n%s", code, out.String())
			}
		})
	}
}

// Retention deletes old entries chain by chain through a signed anchor,
// records what it did, and the chain still verifies from the anchor.
func TestAuditIntegrity_RetentionKeepsChainVerifiable(t *testing.T) {
	keys := withSigningKey(t)
	d := newIntegrityDB(t)
	ctx := context.Background()
	old, recent := time.Now().Add(-400*24*time.Hour), time.Now().Add(-time.Hour)
	org := 7
	d.writeAt(t, &org, "old.1", old)
	d.writeAt(t, &org, "old.2", old)
	d.writeAt(t, &org, "new.3", recent)
	d.writeAt(t, &org, "old.4", old) // after a newer row: waits, so the chain stays a prefix
	d.writeAt(t, nil, "old.global", old)
	if _, err := d.svc.WriteAuditCheckpoints(ctx); err != nil {
		t.Fatal(err)
	}

	retention := 365 * 24 * time.Hour
	deleted, err := d.svc.PurgeAudit(ctx, time.Now().Add(-retention), retention)
	if err != nil {
		t.Fatal(err)
	}
	if deleted["org:7"] != 2 || deleted["global"] != 1 {
		t.Fatalf("deleted = %v", deleted)
	}
	var left []string
	rows, _ := d.db.Query(ctx, `SELECT action FROM audit_log WHERE chain_key = 'org:7' ORDER BY chain_seq`)
	left, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	if len(left) < 2 || left[0] != "new.3" || left[1] != "old.4" {
		t.Fatalf("org:7 after purge: %v", left)
	}
	cps, err := d.repo.AuditCheckpoints(ctx, "org:7")
	if err != nil {
		t.Fatal(err)
	}
	var anchor *auditchain.Checkpoint
	for i := range cps {
		if cps[i].Kind == auditchain.KindAnchor {
			anchor = &cps[i]
		}
	}
	if anchor == nil || anchor.UptoSeq != 2 || keys.Check(*anchor) != auditchain.SigOK {
		t.Fatalf("anchor = %+v", anchor)
	}
	var purgeRows int
	if err := d.db.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log WHERE action = 'audit.purge' AND details->'chains' ? 'org:7'`).Scan(&purgeRows); err != nil || purgeRows != 1 {
		t.Fatalf("audit.purge entries: %d %v", purgeRows, err)
	}
	r := d.verify(t)
	if !r.OK {
		t.Fatalf("verify after purge: %s", problemKinds(r))
	}
	for _, c := range r.Chains {
		if c.ChainKey == "org:7" && (c.AnchorSeq != 2 || c.FirstSeq != 3) {
			t.Fatalf("org:7 report: %+v", c)
		}
	}
	// A second run has nothing to do; rows the anchor doesn't cover still
	// can't be deleted, even with the purge flag.
	if again, err := d.svc.PurgeAudit(ctx, time.Now().Add(-retention), retention); err != nil || len(again) != 0 {
		t.Fatalf("second purge: %v %v", again, err)
	}
	tx, _ := d.db.Begin(ctx)
	tx.Exec(ctx, `SELECT set_config('amp.audit_purge', 'on', true)`) //nolint:errcheck
	_, err = tx.Exec(ctx, `DELETE FROM audit_log WHERE chain_key = 'org:7' AND chain_seq = 3`)
	tx.Rollback(ctx) //nolint:errcheck
	wantPrivilegeError(t, "delete past the anchor", err)
}

func TestAuditIntegrity_RetentionSetting(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"": service.DefaultAuditRetention, "0": 0, "off": 0, "30": 30 * 24 * time.Hour, "junk": service.DefaultAuditRetention, "-5": service.DefaultAuditRetention,
	} {
		t.Setenv("AUDIT_RETENTION_DAYS", v)
		if got := auditRetentionFromEnv(); got != want {
			t.Errorf("AUDIT_RETENTION_DAYS=%q: %v, want %v", v, got, want)
		}
	}
}

// keygen writes the private key to a new 0600 file and prints only the
// public half.
func TestAuditIntegrity_Keygen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing-key")
	var out, errOut bytes.Buffer
	if code := runAuditCLI([]string{"keygen", "-out", path}, &out, &errOut); code != 0 {
		t.Fatalf("keygen: %d %s", code, errOut.String())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 || strings.Contains(out.String(), string(b)) {
		t.Fatalf("mode %v; printed the private key: %v", fi.Mode().Perm(), strings.Contains(out.String(), string(b)))
	}
	if _, err := auditchain.ParsePrivateKey(string(b)); err != nil {
		t.Fatal(err)
	}
	if code := runAuditCLI([]string{"keygen", "-out", path}, &out, &errOut); code == 0 {
		t.Fatal("keygen overwrote an existing file")
	}
}

// An audit entry written while no dispatcher is running (a replica that
// died, or a full queue before) is still delivered once one runs: it waits
// in audit_outbox, not in memory. The body carries the row's chain hash and
// a stable id for de-duplicating repeats.
func TestAuditIntegrity_OutboxDeliversLater(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	allowPrivateWebhooks(t)
	rc := newReceiver(t)
	ra.want(t, 200, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/webhooks", w.org.ID),
		map[string]any{"url": rc.srv.URL, "events": []string{"audit.team.create"}})
	team := fmt.Sprintf("Outbox %d", time.Now().UnixNano())
	ra.want(t, 201, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/teams", w.org.ID), map[string]any{"name": team})

	entries, err := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{OrgID: w.org.ID, Action: "team.create"})
	if err != nil || len(entries) == 0 {
		t.Fatalf("team.create entry: %v", err)
	}
	e := entries[0]
	if e.ChainKey != fmt.Sprintf("org:%d", w.org.ID) || e.ChainSeq == 0 || len(e.Hash) != 64 {
		t.Fatalf("entry chain fields: %+v", e)
	}

	startWebhooks(t, w, webhook.Options{AllowPrivateNetworks: true})
	got := rc.wait(t, "the queued audit entry", func(g []received) bool { return len(g) >= 1 })
	if got[0].event != "audit.team.create" || !strings.Contains(got[0].body, fmt.Sprintf(`"id":"evt_audit_%d"`, e.ID)) ||
		!strings.Contains(got[0].body, `"hash":"`+e.Hash+`"`) || !strings.Contains(got[0].body, team) {
		t.Fatalf("delivery = %+v", got[0])
	}
	db, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		var queued bool
		if err := db.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM audit_outbox WHERE audit_id = $1)`, e.ID).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if !queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("entry still queued after it was matched")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// GET /api/audit/verify: org owners and admins for their org, global
// admins for any org or every chain; nobody else (non-members get the
// usual 404).
func TestAuditIntegrity_VerifyEndpoint(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ra.want(t, 201, "orgadmin", "POST", fmt.Sprintf("/api/orgs/%d/teams", w.org.ID), map[string]any{"name": "Verified"})
	path := fmt.Sprintf("/api/audit/verify?org_id=%d", w.org.ID)
	got := ra.want(t, 200, "orgadmin", "GET", path, nil)
	chains, _ := got["chains"].([]any)
	if got["ok"] != true || len(chains) != 1 || chains[0].(map[string]any)["chain_key"] != fmt.Sprintf("org:%d", w.org.ID) {
		t.Fatalf("org admin verify = %v", got)
	}
	ra.want(t, 200, "root", "GET", path, nil)
	ra.want(t, 403, "alice", "GET", path, nil)
	ra.want(t, 404, "outsider", "GET", path, nil) // not a member: the org is hidden
	ra.want(t, 403, "orgadmin", "GET", "/api/audit/verify", nil)
}
