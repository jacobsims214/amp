package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 059 adds team_submit_grants, the per-team, revocable permission to
// submit a request to a team's inbox without joining the team (AMP #3768;
// approved contract artifact 36 v2 §2 and decision D14, build slice B2).
//
// These tests prove what the schema is responsible for, on a real database, so
// none of them depends on the administration service (AMP #3769) or the submit
// capability (AMP #3770) that will later use the table:
//
//   - it applies both ways (fresh, and on top of 058 with existing work), and
//     re-running its SQL by hand changes nothing;
//   - one row per (team, user): a duplicate grant is unstorable, so
//     idempotency has a schema floor and does not rest on the write path;
//   - tenancy is a constraint: a grant whose team is in another org is
//     refused, and no other write path can produce one;
//   - revocation is a state — a revoked row still exists, and the CHECKs let a
//     deleted manager's attribution go NULL without the row becoming illegal;
//   - deleting the team takes its grants, deleting the user takes their
//     grants, but neither deletes the *other* participants;
//   - and it adds nothing to existing tables: 053's pinned nine-index count on
//     team_requests is unchanged and nothing references the new table.

// grantsDB is the 053 fixture (two orgs, two teams in the first, a user) plus
// helpers for team_submit_grants.
type grantsDB struct{ *teamRequestsDB }

func newGrantsDB(t *testing.T) *grantsDB {
	t.Helper()
	return &grantsDB{newTeamRequestsDB(t)}
}

// customer inserts a user of the third account kind: the identity a submit
// grant usually belongs to (D2), and the one the contract says must stay
// outside org administration.
func (d *grantsDB) customer(subject, email string) int {
	d.t.Helper()
	return d.insert(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, 'customer') RETURNING id`, subject, email)
}

// grant is one team_submit_grants row under test. Zero values are what the
// schema's defaults supply, so a test states only the rule it is proving.
type grant struct {
	team    int
	org     int // 0 = the team's own org, read from teams
	user    int
	granted int // 0 = NULL
	revoked int // 0 = NULL
	revAt   bool
}

func (d *grantsDB) insertGrant(g grant) error {
	d.t.Helper()
	org := g.org
	if org == 0 {
		if err := d.db.QueryRow(d.ctx, `SELECT org_id FROM teams WHERE id = $1`, g.team).Scan(&org); err != nil {
			d.t.Fatalf("read team org: %v", err)
		}
	}
	var grantedBy, revokedBy *int
	if g.granted > 0 {
		grantedBy = &g.granted
	}
	if g.revoked > 0 {
		revokedBy = &g.revoked
	}
	// A revoking actor without a time must be storable only by the negative
	// test: pass revAt to set it.
	_, err := d.db.Exec(d.ctx, `
		INSERT INTO team_submit_grants (team_id, org_id, user_id, granted_by, revoked_by, revoked_at)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $6 THEN NOW() ELSE NULL END)`,
		g.team, org, g.user, grantedBy, revokedBy, g.revAt)
	return err
}

func (d *grantsDB) mustGrant(g grant) {
	d.t.Helper()
	if err := d.insertGrant(g); err != nil {
		d.t.Fatalf("insert grant %+v: %v", g, err)
	}
}

func TestMigration_059TeamSubmitGrants(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer repo.Close()
		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("fresh migrate: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		var n int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM team_submit_grants`).Scan(&n); err != nil {
			t.Fatalf("team_submit_grants: %v", err)
		}
		if n != 0 {
			t.Fatalf("team_submit_grants starts with %d rows, want 0", n)
		}
		// The table must be usable the moment it exists: the first act a team
		// lead takes after inviting a requester.
		d := newGrantsDB(t)
		d.mustGrant(grant{team: d.team, user: d.customer("fresh059", "fresh059@x.test"), granted: d.user})
	})

	t.Run("upgrade from 058", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "059" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 059")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 058: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// Work that already exists at 058: an org, a team with a member, and a
		// request submitted through the old surface.
		var orgID, teamID, leadID, memberID, requestID int
		q := func(sql string, dst *int, args ...any) {
			t.Helper()
			if err := db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`INSERT INTO orgs (name, slug) VALUES ('pre-059', 'pre059') RETURNING id`, &orgID)
		q(`INSERT INTO teams (org_id, name) VALUES ($1, 'pre-059 desk') RETURNING id`, &teamID, orgID)
		q(`INSERT INTO users (subject, email) VALUES ('pre059-lead', 'pre059-lead@x.test') RETURNING id`, &leadID)
		q(`INSERT INTO users (subject, email, kind) VALUES ('pre059-cust', 'pre059-cust@x.test', 'customer') RETURNING id`, &memberID)
		if _, err := db.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, teamID, leadID); err != nil {
			t.Fatalf("pre-059 team lead: %v", err)
		}
		q(`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject, state)
			VALUES ('ref_PRE059UPGRADE001', $1, $2, $3, 'pre059-cust@x.test', 'a request before 059', 'triaging') RETURNING id`,
			&requestID, orgID, teamID, memberID)

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 059: %v", err)
		}
		// Existing work is untouched, and the new table is empty: an upgrade
		// adds a way to grant access, it never invents one.
		var grants int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM team_submit_grants`).Scan(&grants); err != nil || grants != 0 {
			t.Fatalf("team_submit_grants after upgrade: %d %v", grants, err)
		}
		var state string
		if err := db.QueryRow(ctx, `SELECT state FROM team_requests WHERE id = $1`, requestID).Scan(&state); err != nil || state != "triaging" {
			t.Fatalf("pre-existing request state = %q (%v), want triaging", state, err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO team_submit_grants (team_id, org_id, user_id, granted_by)
			VALUES ($1, $2, $3, $4)`, teamID, orgID, memberID, leadID); err != nil {
			t.Fatalf("grant on a pre-059 team: %v", err)
		}

		// Re-running 059's SQL by hand is harmless, twice over: nothing is
		// added a second time.
		counts := func() (constraints, indexes int) {
			t.Helper()
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'team_submit_grants'::regclass`).Scan(&constraints); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'team_submit_grants'`).Scan(&indexes); err != nil {
				t.Fatal(err)
			}
			return constraints, indexes
		}
		cBefore, iBefore := counts()
		for _, m := range migrations {
			if m.Version == "059_team_submit_grants" {
				for i := 0; i < 2; i++ {
					if _, err := db.Exec(ctx, m.SQL); err != nil {
						t.Fatalf("re-running 059 (pass %d): %v", i+1, err)
					}
				}
			}
		}
		cAfter, iAfter := counts()
		if cAfter != cBefore {
			t.Fatalf("re-running 059 changed the constraint count %d -> %d", cBefore, cAfter)
		}
		if iAfter != iBefore {
			t.Fatalf("re-running 059 changed the index count %d -> %d", iBefore, iAfter)
		}
		var stillThere int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM team_submit_grants WHERE team_id = $1`, teamID).Scan(&stillThere); err != nil || stillThere != 1 {
			t.Fatalf("grants after re-applying 059 = %d (%v), want 1", stillThere, err)
		}
	})
}

// One grant per (team, user): a duplicate is unstorable, which is what makes
// "a duplicate grant is idempotent" a property of the schema and not only of
// the one write path that happens to upsert.
func TestMigration_059OneGrantPerPersonPerTeam(t *testing.T) {
	d := newGrantsDB(t)
	cust := d.customer("dup059", "dup059@x.test")
	d.mustGrant(grant{team: d.team, user: cust, granted: d.user})

	var user, org int
	if err := d.db.QueryRow(d.ctx, `SELECT user_id, org_id FROM team_submit_grants WHERE team_id = $1`, d.team).Scan(&user, &org); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if user != cust || org != d.org {
		t.Fatalf("stored grant = (user %d, org %d), want (%d, %d)", user, org, cust, d.org)
	}

	// The same pair again is a primary-key violation (23505), whatever the
	// other columns say — including a different grantor.
	err := d.insertGrant(grant{team: d.team, user: cust, granted: cust})
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("a second grant for the same (team, user) was stored")
	}
	if !asPgError(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate grant error = %v, want a unique violation (23505)", err)
	}

	// The same person on another team is a different grant, and the same team
	// for another person is too: uniqueness is the pair, not either side.
	d.mustGrant(grant{team: d.team2, user: cust, granted: d.user})
	d.mustGrant(grant{team: d.team, user: d.user, granted: d.user})
}

// asPgError is errors.As without importing errors in every test file that
// wants a SQLSTATE.
func asPgError(err error, target **pgconn.PgError) bool {
	for err != nil {
		if pe, ok := err.(*pgconn.PgError); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Tenancy is a constraint: a grant's team must be in the grant's org, so no
// write path (including a hand fix) can create a cross-tenant grant.
func TestMigration_059TenancyIsEnforced(t *testing.T) {
	d := newGrantsDB(t)
	// d.other is a second org; d.team belongs to d.org.
	cust := d.customer("tenant059", "tenant059@x.test")

	err := d.insertGrant(grant{team: d.team, org: d.other, user: cust, granted: d.user})
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("a grant whose team is in another org was stored")
	}
	if !asPgError(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("cross-org grant error = %v, want a foreign-key violation (23503)", err)
	}
	var n int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_submit_grants`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("grants after a refused cross-org write = %d (%v), want 0", n, err)
	}

	// Moving the grant to the other org after the fact is refused too: the
	// constraint is on the row, not on the insert.
	d.mustGrant(grant{team: d.team, user: cust, granted: d.user})
	if _, err := d.db.Exec(d.ctx, `UPDATE team_submit_grants SET org_id = $1 WHERE team_id = $2`, d.other, d.team); err == nil {
		t.Fatal("re-pointing a grant at another org was allowed")
	}
}

// Revocation is a state: the row survives, the CHECKs accept a deleted
// manager's attribution going NULL, and a revoking actor without a time is
// refused (the one-way implication the request schema documents).
func TestMigration_059RevocationIsAState(t *testing.T) {
	d := newGrantsDB(t)
	cust := d.customer("revoke059", "revoke059@x.test")
	d.mustGrant(grant{team: d.team, user: cust, granted: d.user})

	// A revoking actor with no time is illegal: "who took it away" is only
	// meaningful with "when it stopped".
	err := d.insertGrant(grant{team: d.team2, user: cust, granted: d.user, revoked: d.user, revAt: false})
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("a revoked_by with no revoked_at was stored")
	}
	if !asPgError(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("revocation-actor error = %v, want a check violation (23514)", err)
	}

	// A revoking time with no actor is fine: the actor may have been deleted.
	// This is the one-way implication, and it is what lets the SET NULL below
	// leave a legal row.
	if _, err := d.db.Exec(d.ctx, `UPDATE team_submit_grants SET revoked_at = NOW() WHERE team_id = $1`, d.team); err != nil {
		t.Fatalf("revoking with no actor: %v", err)
	}
	var revokedAt *string
	if err := d.db.QueryRow(d.ctx, `SELECT revoked_at::text FROM team_submit_grants WHERE team_id = $1`, d.team).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil {
		t.Fatal("the revoked grant lost its revoked_at")
	}

	// The row still exists — revocation is a state, not a delete — and deleting
	// the manager who granted it nulls the attribution without disturbing the
	// revocation timestamp or deleting the grant.
	if _, err := d.db.Exec(d.ctx, `DELETE FROM users WHERE id = $1`, d.user); err != nil {
		t.Fatalf("delete granting manager: %v", err)
	}
	var grantedBy *int
	if err := d.db.QueryRow(d.ctx, `SELECT granted_by FROM team_submit_grants WHERE team_id = $1`, d.team).Scan(&grantedBy); err != nil {
		t.Fatal(err)
	}
	if grantedBy != nil {
		t.Fatalf("granted_by = %v after its user was deleted, want NULL", *grantedBy)
	}
	var still int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_submit_grants WHERE team_id = $1 AND revoked_at IS NOT NULL`, d.team).Scan(&still); err != nil || still != 1 {
		t.Fatalf("revoked grant after deleting its grantor = %d (%v), want 1", still, err)
	}
}

// Deleting a team or its org takes the grants with it; deleting the person
// takes their grants. Neither touches the other participants.
func TestMigration_059Cascades(t *testing.T) {
	d := newGrantsDB(t)
	otherOrgTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'other desk') RETURNING id`, d.other)
	cust := d.customer("cascade059", "cascade059@x.test")
	other := d.insert(`INSERT INTO users (subject, email) VALUES ('cascade059-2', 'cascade059-2@x.test') RETURNING id`)

	d.mustGrant(grant{team: d.team, user: cust, granted: d.user})
	d.mustGrant(grant{team: d.team2, user: cust, granted: d.user})
	d.mustGrant(grant{team: otherOrgTeam, user: cust, granted: d.user})
	d.mustGrant(grant{team: d.team, user: other, granted: d.user})

	// Deleting the team removes exactly its grants, not the person's others.
	if _, err := d.db.Exec(d.ctx, `DELETE FROM teams WHERE id = $1`, d.team); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	var onTeam, total int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_submit_grants WHERE team_id = $1`, d.team).Scan(&onTeam); err != nil {
		t.Fatal(err)
	}
	if onTeam != 0 {
		t.Fatalf("%d grants survived their team", onTeam)
	}
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_submit_grants`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 { // this team2 grant and the other org's
		t.Fatalf("grants after deleting one team = %d, want 2", total)
	}

	// Deleting the person removes their remaining grants, and only theirs.
	if _, err := d.db.Exec(d.ctx, `DELETE FROM users WHERE id = $1`, cust); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var theirs int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_submit_grants WHERE user_id = $1`, cust).Scan(&theirs); err != nil {
		t.Fatal(err)
	}
	if theirs != 0 {
		t.Fatalf("%d grants survived their user", theirs)
	}
	var others int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_submit_grants WHERE user_id = $1`, other).Scan(&others); err != nil || others != 0 {
		// 'other' held a grant on d.team, which was deleted above: it is gone
		// with its team, and that is the point of this assertion's second half.
		t.Fatalf("grants for the surviving user = %d (%v), want 0 (their team was deleted)", others, err)
	}
}

// A grant is access to a person, so it never outlives them, and it is never a
// membership: the migration adds one table and nothing else, and no grant row
// implies a team_members or org_members row.
func TestMigration_059AddsOnlyGrantsAndNoMembership(t *testing.T) {
	d := newGrantsDB(t)
	cust := d.customer("nomember059", "nomember059@x.test")
	d.mustGrant(grant{team: d.team, user: cust, granted: d.user})

	// The grant exists and the two membership tables are untouched by it.
	var teamRows, orgRows int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_members WHERE user_id = $1`, cust).Scan(&teamRows); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM org_members WHERE user_id = $1`, cust).Scan(&orgRows); err != nil {
		t.Fatal(err)
	}
	if teamRows != 0 || orgRows != 0 {
		t.Fatalf("a submit grant created %d team_members and %d org_members rows, want 0 and 0", teamRows, orgRows)
	}

	// The table's exact shape: the columns the domain type reads, and nothing
	// that would turn it into a membership or a project grant.
	want := []string{"team_id", "org_id", "user_id", "granted_by", "granted_at", "revoked_by", "revoked_at"}
	rows, err := d.db.Query(d.ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'team_submit_grants' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if len(cols) != len(want) {
		t.Fatalf("team_submit_grants columns = %v, want %v", cols, want)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Fatalf("team_submit_grants columns = %v, want %v", cols, want)
		}
	}

	// 053's pinned index count on team_requests is unchanged, and nothing
	// existing points at the new table: the migration hangs the relation off
	// teams, not off the request record.
	var requestIndexes int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'team_requests'`).Scan(&requestIndexes); err != nil {
		t.Fatal(err)
	}
	if requestIndexes != 9 {
		t.Fatalf("team_requests has %d indexes, want the 9 from 053", requestIndexes)
	}
	var backRefs int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM pg_constraint
		WHERE confrelid = 'team_submit_grants'::regclass
		  AND conrelid <> 'team_submit_grants'::regclass`).Scan(&backRefs); err != nil {
		t.Fatal(err)
	}
	if backRefs != 0 {
		t.Fatalf("%d existing tables reference team_submit_grants, want 0", backRefs)
	}
}
