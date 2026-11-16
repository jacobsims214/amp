package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 061 adds request_idempotency_keys, the storage behind a submit's
// Idempotency-Key (AMP #3871; approved contract artifact 36 v2 §5, build slice
// C1). The repository (internal/repository/request_create.go) is where the
// semantics live; these tests prove the schema floor those semantics rest on, on
// a real database, without depending on the repository at all:
//
//   - it applies both ways (fresh, and on top of 060), and re-running its SQL by
//     hand changes nothing (the unversioned-schema boot path re-runs files);
//   - the scope is the unique key: one row per (org, team, source, submitter,
//     key), and no two of those five can be dropped from the predicate without
//     the table accepting a row it must refuse;
//   - tenancy is a constraint in both halves — a key cannot name a request in
//     another org, and its team must be the same org's;
//   - a key row never outlives the request or the submitter it belongs to, so a
//     retry is never answered with an id that no longer exists;
//   - the closed vocabularies and shapes (source kinds, a non-blank bounded key,
//     a 64-character hex fingerprint) are checked by the database, not only by Go.

// idemDB is the 053/054/057 fixture (org, teams, users) plus helpers for the new
// table.
type idemDB struct{ *teamRequestsDB }

func newIdemDB(t *testing.T) *idemDB {
	t.Helper()
	return &idemDB{teamRequestsDB: newTeamRequestsDB(t)}
}

// key is one request_idempotency_keys row under test. Zero values are what the
// schema supplies, so a test states only the rule it is proving.
type key struct {
	request   int
	org       int    // 0 = the request's org
	team      int    // 0 = the fixture's first team
	source    string // "" = the 'web' default is NOT applied: source_kind is required, so this is a test bug
	submitter int    // 0 = the fixture's user
	token     string // "" = a run-unique token
	hash      string // "" = a valid 64-hex fingerprint
}

// insertKey writes one key row and returns the database's error, so a test can
// assert which rule refused it.
func (d *idemDB) insertKey(k key) error {
	d.t.Helper()
	org, team, submitter, hash := k.org, k.team, k.submitter, k.hash
	if org == 0 {
		org = d.org
	}
	if team == 0 {
		team = d.team
	}
	if submitter == 0 {
		submitter = d.user
	}
	if hash == "" {
		hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}
	if k.token == "" {
		k.token = "idem-token-1"
	}
	if k.source == "" {
		k.source = "web"
	}
	_, err := d.db.Exec(d.ctx, `INSERT INTO request_idempotency_keys
		(org_id, team_id, source_kind, submitter_user_id, idempotency_key, payload_hash, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, org, team, k.source, submitter, k.token, hash, k.request)
	return err
}

// mustRequest inserts one bare request in the fixture's first team and returns its
// id. It is deliberately a direct INSERT: this test file is about the schema, and
// the repository's own tests cover the create path that will write these rows in
// production.
func (d *idemDB) mustRequest(ref string) int {
	d.t.Helper()
	return d.insert(`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject)
		VALUES ($1, $2, $3, $4, 'someone@x.test', 'A request') RETURNING id`, ref, d.org, d.team, d.user)
}

func TestMigration_061RequestIdempotency(t *testing.T) {
	ctx := context.Background()

	t.Run("applies fresh and the table is usable immediately", func(t *testing.T) {
		d := newIdemDB(t)
		req := d.mustRequest("ref_IDEM_fresh_00001")
		if err := d.insertKey(key{request: req}); err != nil {
			t.Fatalf("insert a key row: %v", err)
		}
		var n int
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM request_idempotency_keys WHERE request_id = $1`, req).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("request %d has %d key rows, want 1", req, n)
		}
	})

	t.Run("upgrade from 060 keeps existing requests untouched", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "061" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 061 in the embedded set")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 060: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// A pre-061 world: an org, a team, a user and a request, as 060 left them.
		var org, team, user, request int
		if err := db.QueryRow(ctx, `INSERT INTO orgs (name, slug) VALUES ('Pre 061', 'pre061') RETURNING id`).Scan(&org); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 'Desk') RETURNING id`, org).Scan(&team); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO users (subject, email) VALUES ('pre-061', 'pre-061@x.test') RETURNING id`).Scan(&user); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject)
			VALUES ('ref_PRE_061_000001', $1, $2, $3, 'pre@x.test', 'Already here') RETURNING id`, org, team, user).Scan(&request); err != nil {
			t.Fatal(err)
		}

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("migrate 061: %v", err)
		}
		// The existing request is unchanged and still readable...
		var subject string
		if err := db.QueryRow(ctx, `SELECT subject FROM team_requests WHERE id = $1`, request).Scan(&subject); err != nil {
			t.Fatalf("pre-061 request after upgrade: %v", err)
		}
		if subject != "Already here" {
			t.Fatalf("pre-061 request's subject = %q after upgrade", subject)
		}
		// ...and the new table is empty, because nothing could have used it yet.
		var keys int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_idempotency_keys`).Scan(&keys); err != nil {
			t.Fatalf("count after upgrade: %v", err)
		}
		if keys != 0 {
			t.Fatalf("request_idempotency_keys starts with %d rows, want 0", keys)
		}
	})

	t.Run("re-running the SQL is a no-op", func(t *testing.T) {
		d := newIdemDB(t)
		req := d.mustRequest("ref_IDEM_idem_000001")
		if err := d.insertKey(key{request: req}); err != nil {
			t.Fatalf("insert before re-run: %v", err)
		}
		var sql string
		for _, m := range migrations {
			if m.Version == "061_request_idempotency" {
				sql = m.SQL
			}
		}
		if sql == "" {
			t.Fatal("migration 061_request_idempotency not embedded")
		}
		for i := 0; i < 2; i++ {
			if _, err := d.db.Exec(ctx, sql); err != nil {
				t.Fatalf("re-running 061 (attempt %d): %v", i, err)
			}
		}
		var n int
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM request_idempotency_keys WHERE request_id = $1`, req).Scan(&n); err != nil {
			t.Fatalf("count after re-run: %v", err)
		}
		if n != 1 {
			t.Fatalf("%d key rows after re-running the migration, want 1", n)
		}
	})

	// The scope is the whole point: each of the five columns is part of the
	// uniqueness rule, so a row that matches on four of them is a different key.
	// Dropping any one from the index (or from the write path's predicate) would
	// let one person's retry answer for another's submission.
	t.Run("the scope is the unique key", func(t *testing.T) {
		d := newIdemDB(t)
		req1 := d.mustRequest("ref_IDEM_scope_00001")
		req2 := d.mustRequest("ref_IDEM_scope_00002")
		req3 := d.mustRequest("ref_IDEM_scope_00003")
		other := d.insert(`INSERT INTO users (subject, email) VALUES ('idem-other', 'idem-other@x.test') RETURNING id`)

		if err := d.insertKey(key{request: req1}); err != nil {
			t.Fatalf("the first key row: %v", err)
		}
		// Same five values: refused.
		if err := d.insertKey(key{request: req2}); err == nil {
			t.Fatal("a second row with the same (org, team, source, submitter, key) was accepted")
		}
		// Vary each scope column in turn: each is a different key, so each stores.
		sameTokenOtherUser := key{request: req2, submitter: other}
		if err := d.insertKey(sameTokenOtherUser); err != nil {
			t.Errorf("the same token by another submitter was refused (%v); keys are scoped per person", err)
		}
		sameTokenOtherSource := key{request: req3, source: "mcp"}
		if err := d.insertKey(sameTokenOtherSource); err != nil {
			t.Errorf("the same token on another source was refused (%v); sources are separate namespaces", err)
		}
		// Same token, another team in the same org.
		sameTokenOtherTeam := key{request: d.mustRequest("ref_IDEM_scope_00004"), team: d.team2}
		if err := d.insertKey(sameTokenOtherTeam); err != nil {
			t.Errorf("the same token in another team was refused (%v); teams are separate namespaces", err)
		}
		// Same token, another org (its own request there, so the tenant FKs hold).
		otherOrg := d.insert(`INSERT INTO orgs (name, slug) VALUES ('Idem other org', 'idem-other-org') RETURNING id`)
		otherTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other desk') RETURNING id`, otherOrg)
		otherOrgUser := d.insert(`INSERT INTO users (subject, email) VALUES ('idem-org2', 'idem-org2@x.test') RETURNING id`)
		otherOrgReq := d.insert(`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject)
			VALUES ('ref_IDEM_scope_other', $1, $2, $3, 'o@x.test', 'Elsewhere') RETURNING id`, otherOrg, otherTeam, otherOrgUser)
		sameTokenOtherOrg := key{request: otherOrgReq, org: otherOrg, team: otherTeam, submitter: otherOrgUser}
		if err := d.insertKey(sameTokenOtherOrg); err != nil {
			t.Errorf("the same token in another org was refused (%v); orgs are separate namespaces", err)
		}
	})

	t.Run("a key names its request and its team, in one tenant", func(t *testing.T) {
		d := newIdemDB(t)
		req := d.mustRequest("ref_IDEM_tenant_0001")
		otherOrg := d.insert(`INSERT INTO orgs (name, slug) VALUES ('Idem foreign org', 'idem-foreign-org') RETURNING id`)
		foreignTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign desk') RETURNING id`, otherOrg)

		cases := []struct {
			name string
			k    key
		}{
			// The request is in the fixture's org; naming another org must be refused
			// by the composite foreign key, not stored as a cross-tenant key.
			{"a key in another org pointing at this request", key{request: req, org: otherOrg, team: foreignTeam}},
			// The team must be the request's org's team.
			{"a team from another org", key{request: req, team: foreignTeam}},
			// A request that does not exist.
			{"an unknown request", key{request: 1 << 30}},
		}
		for _, c := range cases {
			if err := d.insertKey(c.k); err == nil {
				t.Errorf("%s was accepted", c.name)
			}
		}

		// "No pending rows": request_id is NOT NULL, so the half-written key that
		// a claim-style design would leave behind (a key with no request yet —
		// the stuck claim a retry would have to wait out or steal) cannot be
		// stored at all. This is the schema floor under the retry criterion, so it
		// is asserted rather than described.
		_, err := d.db.Exec(ctx, `INSERT INTO request_idempotency_keys
			(org_id, team_id, source_kind, submitter_user_id, idempotency_key, payload_hash, request_id)
			VALUES ($1, $2, 'web', $3, 'pending-token', $4, NULL)`,
			d.org, d.team, d.user, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		if err == nil {
			t.Error("a key row with a NULL request_id was accepted: a pending claim must be unrepresentable")
		}
	})

	t.Run("a key never outlives its request or its submitter", func(t *testing.T) {
		d := newIdemDB(t)
		req := d.mustRequest("ref_IDEM_cascade_001")
		if err := d.insertKey(key{request: req}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := d.exec(`DELETE FROM team_requests WHERE id = $1`, req); err != nil {
			t.Fatalf("delete the request: %v", err)
		}
		var n int
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM request_idempotency_keys WHERE request_id = $1`, req).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("%d key rows survived their request's deletion, want 0", n)
		}

		req2 := d.mustRequest("ref_IDEM_cascade_002")
		submitter := d.insert(`INSERT INTO users (subject, email) VALUES ('idem-gone', 'idem-gone@x.test') RETURNING id`)
		if err := d.insertKey(key{request: req2, submitter: submitter, token: "gone-token"}); err != nil {
			t.Fatalf("insert for the second submitter: %v", err)
		}
		if err := d.exec(`DELETE FROM users WHERE id = $1`, submitter); err != nil {
			t.Fatalf("delete the submitter: %v", err)
		}
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM request_idempotency_keys WHERE idempotency_key = 'gone-token'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("%d key rows survived their submitter's deletion, want 0", n)
		}
	})

	t.Run("the closed shapes are the database's", func(t *testing.T) {
		d := newIdemDB(t)
		cases := []struct {
			name string
			k    key
		}{
			{"an unknown source kind", key{request: d.mustRequest("ref_IDEM_shape_00001"), source: "email"}},
			{"a blank key", key{request: d.mustRequest("ref_IDEM_shape_00002"), token: "   "}},
			{"a key over 255 characters", key{request: d.mustRequest("ref_IDEM_shape_00003"), token: string(make([]byte, 256))}},
			{"a fingerprint that is not 64 hex characters",
				key{request: d.mustRequest("ref_IDEM_shape_00004"), token: "hash-token", hash: "not-a-sha256"}},
			{"an upper-case fingerprint", key{request: d.mustRequest("ref_IDEM_shape_00005"), token: "upper-token",
				hash: "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"}},
		}
		for _, c := range cases {
			if err := d.insertKey(c.k); err == nil {
				t.Errorf("%s was accepted", c.name)
			}
		}
	})
}
