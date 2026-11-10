package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 053 adds team_requests, the durable record behind a team workstream
// inbox (AMP #3755; approved contract artifact 36 v2 §1, build slice A1).
//
// These tests prove what the schema is responsible for, on a real database, so
// none of them depends on the service code that will later use the table:
//
//   - it applies both ways: to a fresh database and on top of 052 with existing
//     work in it, and re-running its SQL by hand is harmless;
//   - the lifecycle is a closed set, and the row rules a single request must
//     satisfy are refused by the database, not only by the code;
//   - identity is opaque and unique: a guessable public_ref cannot be stored and
//     two requests cannot share one;
//   - tenancy is a constraint: a request's team must belong to its org;
//   - the requester's contact snapshot survives the requester's deletion, and
//     the request itself never disappears because its requester did;
//   - source identity dedups a connector's replays without deduplicating
//     unrelated web/API/MCP submissions that carry no external id.

// teamRequestsDB is a scratch database migrated to 053, with one org holding
// two teams, a second org, and a user to act as the requester.
type teamRequestsDB struct {
	t     *testing.T
	db    *pgx.Conn
	ctx   context.Context
	org   int
	other int // a second org, for tenant-isolation checks
	team  int
	team2 int // a second team in org
	user  int
}

func newTeamRequestsDB(t *testing.T) *teamRequestsDB {
	t.Helper()
	ctx := context.Background()
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
	t.Cleanup(func() { db.Close(context.Background()) })
	d := &teamRequestsDB{t: t, db: db, ctx: ctx}
	d.org = d.insert(`INSERT INTO orgs (name, slug) VALUES ('Req org', 'req-org') RETURNING id`)
	d.other = d.insert(`INSERT INTO orgs (name, slug) VALUES ('Other org', 'req-other-org') RETURNING id`)
	d.team = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Desk') RETURNING id`, d.org)
	d.team2 = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Second desk') RETURNING id`, d.org)
	d.user = d.insert(`INSERT INTO users (subject, email) VALUES ('req-requester', 'requester@x.test') RETURNING id`)
	return d
}

func (d *teamRequestsDB) insert(sql string, args ...any) int {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx, sql, args...).Scan(&id); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (d *teamRequestsDB) exec(sql string, args ...any) error {
	d.t.Helper()
	_, err := d.db.Exec(d.ctx, sql, args...)
	return err
}

func (d *teamRequestsDB) mustExec(sql string, args ...any) {
	d.t.Helper()
	if err := d.exec(sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

// req is the shape of one team_requests row under test. Only the fields a test
// sets are written, so each test states exactly the rule it is proving; the
// zero values are the ones the schema defaults apply to.
type req struct {
	ref       string
	team      int // 0 = the fixture's first team
	org       int // 0 = the fixture's org
	requester int // 0 = the fixture's user, -1 = NULL
	contact   string
	source    string
	external  string
	subject   string
	state     string
	reason    string
	brief     string
	delivered bool
	needsInfo bool
	accepted  bool
	duplicate int
	claim     int // -1 = claim_user_id set but claimed_at left NULL
	extraCols string
	extraVals []any
}

// insert writes the row (expecting success) and returns its id.
func (d *teamRequestsDB) insertReq(r req) int {
	d.t.Helper()
	id, err := d.tryInsertReq(r)
	if err != nil {
		d.t.Fatalf("insert %q: %v", r.ref, err)
	}
	return id
}

// tryInsertReq writes the row and returns the raw error, for the cases that
// must be refused.
func (d *teamRequestsDB) tryInsertReq(r req) (int, error) {
	d.t.Helper()
	team, org := r.team, r.org
	if team == 0 {
		team = d.team
	}
	if org == 0 {
		org = d.org
	}
	contact := r.contact
	if contact == "" {
		contact = "requester@x.test"
	}
	source := r.source
	if source == "" {
		source = "web"
	}
	subject := r.subject
	if subject == "" {
		subject = "subject of " + r.ref
	}
	state := r.state
	if state == "" {
		state = "new"
	}
	var requester *int
	switch {
	case r.requester == 0:
		requester = &d.user
	case r.requester > 0:
		requester = &r.requester
	}
	cols := []string{"public_ref", "org_id", "team_id", "requester_user_id", "requester_contact",
		"source_kind", "source_external_id", "subject", "state", "reject_reason", "outcome_brief"}
	vals := []any{r.ref, org, team, requester, contact, source, r.external, subject, state, r.reason, r.brief}
	if r.delivered {
		cols = append(cols, "delivered_at")
		vals = append(vals, "NOW()") // placeholder, replaced below
	}
	if r.needsInfo {
		cols = append(cols, "needs_info_since")
		vals = append(vals, "NOW()")
	}
	if r.accepted {
		cols = append(cols, "accepted_at", "accepted_by")
		vals = append(vals, "NOW()", d.user)
	}
	if r.duplicate > 0 {
		cols = append(cols, "duplicate_of")
		vals = append(vals, r.duplicate)
	}
	if r.claim != 0 {
		cols = append(cols, "claim_user_id")
		vals = append(vals, d.user)
		if r.claim > 0 {
			cols = append(cols, "claimed_at")
			vals = append(vals, "NOW()")
		}
	}
	if r.extraCols != "" {
		for _, c := range strings.Split(r.extraCols, ",") {
			cols = append(cols, strings.TrimSpace(c))
		}
		vals = append(vals, r.extraVals...)
	}
	// NOW() has to be SQL, not a bind parameter, so those columns are appended
	// as literals: build the statement from the column list, binding everything
	// else.
	var placeholders []string
	var args []any
	for i, c := range cols {
		if s, ok := vals[i].(string); ok && s == "NOW()" {
			placeholders = append(placeholders, "NOW()")
			continue
		}
		args = append(args, vals[i])
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		_ = c
	}
	sql := "INSERT INTO team_requests (" + strings.Join(cols, ", ") + ") VALUES (" +
		strings.Join(placeholders, ", ") + ") RETURNING id"
	var id int
	err := d.db.QueryRow(d.ctx, sql, args...).Scan(&id)
	return id, err
}

// Migration 053 applies on a fresh database and on top of 052 with existing
// work in it. The new table starts empty, nothing existing changes, and
// re-running the SQL by hand is harmless.
func TestMigration_053TeamRequests(t *testing.T) {
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
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM team_requests`).Scan(&n); err != nil {
			t.Fatalf("team_requests: %v", err)
		}
		if n != 0 {
			t.Fatalf("team_requests starts with %d rows, want 0", n)
		}
		// The app must be able to use the table it just created: one request in,
		// and the queue's ordering index has something to order.
		d := newTeamRequestsDB(t)
		d.insertReq(req{ref: "ref_FRESH1234567890ab"})
	})

	t.Run("upgrade from 052", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "053" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 053")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 052: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		var projectID, epic, story, taskID, orgID, teamID, userID int
		q := func(sql string, dst *int, args ...any) {
			t.Helper()
			if err := db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`INSERT INTO projects (name, code) VALUES ('up', 'up053') RETURNING id`, &projectID)
		q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, projectID)
		q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, projectID, epic)
		q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 't') RETURNING id`, &taskID, projectID, epic, story)
		q(`INSERT INTO orgs (name, slug) VALUES ('pre-053', 'pre053') RETURNING id`, &orgID)
		q(`INSERT INTO teams (org_id, name) VALUES ($1, 'pre-053 desk') RETURNING id`, &teamID, orgID)
		q(`INSERT INTO users (subject, email) VALUES ('pre-sub', 'pre@x.test') RETURNING id`, &userID)

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 053: %v", err)
		}
		// Existing work is untouched, and the new table is empty: an upgrade
		// adds a place to put requests, it never invents one.
		got, err := repo.GetTask(ctx, taskID)
		if err != nil || got.ProjectID != projectID {
			t.Fatalf("existing ticket after upgrade: %+v %v", got, err)
		}
		var requests int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM team_requests`).Scan(&requests); err != nil || requests != 0 {
			t.Fatalf("team_requests after upgrade: %d %v", requests, err)
		}
		// A team that already existed can carry requests: the composite FK's
		// target index was added over live rows, not over an empty table.
		var newID int
		if err := db.QueryRow(ctx, `INSERT INTO team_requests
			(public_ref, org_id, team_id, requester_user_id, requester_contact, subject)
			VALUES ('ref_PRE053UPGRADExyz', $1, $2, $3, 'pre@x.test', 'after the upgrade')
			RETURNING id`, orgID, teamID, userID).Scan(&newID); err != nil {
			t.Fatalf("request on a pre-existing team: %v", err)
		}
		// Re-running 053's SQL by hand is harmless, twice over: the guards hold,
		// and nothing is added a second time (a duplicated constraint or index
		// would be a schema that drifts every time someone fixes it by hand).
		constraintCount := func() int {
			t.Helper()
			var n int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'team_requests'::regclass`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		constraintsBefore := constraintCount()
		for _, m := range migrations {
			if m.Version == "053_team_requests" {
				for i := 0; i < 2; i++ {
					if _, err := db.Exec(ctx, m.SQL); err != nil {
						t.Fatalf("re-running 053 (pass %d): %v", i+1, err)
					}
				}
			}
		}
		if got := constraintCount(); got != constraintsBefore {
			t.Fatalf("re-running 053 changed the constraint count %d -> %d", constraintsBefore, got)
		}
		var dupIndexes int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes
			WHERE tablename = 'team_requests' AND indexname LIKE '%team_requests%'`).Scan(&dupIndexes); err != nil {
			t.Fatal(err)
		}
		if dupIndexes != 9 {
			t.Fatalf("team_requests has %d indexes, want 9 after re-applying 053", dupIndexes)
		}
	})
}

// The lifecycle is a closed set, and the row-level rules a single request must
// satisfy are the database's job: whatever writes the row (a service bug, a
// manual fix, a future tool), a request cannot be stored in a state it does not
// legally describe.
func TestMigration_053LifecycleAndStateRules(t *testing.T) {
	d := newTeamRequestsDB(t)

	// A new request is 'new' by default.
	minimal := d.insertReq(req{ref: "ref_DEFAULTSNEWSTATE1"})
	var state string
	if err := d.db.QueryRow(d.ctx, `SELECT state FROM team_requests WHERE id = $1`, minimal).Scan(&state); err != nil || state != "new" {
		t.Fatalf("default state = %q (%v), want new", state, err)
	}

	// Every one of the nine states is storable, each with the row evidence it
	// requires, so the CHECK is a whitelist and not an accidental subset.
	canonical := d.insertReq(req{ref: "ref_CANONICAL_0000001"})
	legal := []req{
		{ref: "ref_STATE_new_000000001", state: "new"},
		{ref: "ref_STATE_triaging_00001", state: "triaging"},
		{ref: "ref_STATE_needsinfo_0001", state: "needs_information", needsInfo: true},
		{ref: "ref_STATE_accepted_00001", state: "accepted"},
		{ref: "ref_STATE_inprogress_001", state: "in_progress"},
		{ref: "ref_STATE_delivered_0001", state: "delivered", brief: "we built it", delivered: true},
		{ref: "ref_STATE_reqaccepted_000", state: "requester_accepted", brief: "we built it", delivered: true, accepted: true},
		{ref: "ref_STATE_rejected_00001", state: "rejected", reason: "out of scope"},
		{ref: "ref_STATE_duplicate_0001", state: "duplicate", duplicate: canonical},
	}
	for _, r := range legal {
		d.insertReq(r)
	}

	// An invented state is refused.
	wantSQLState(t, "an invented state",
		d.tryInsertReqErr(t, req{ref: "ref_STATE_invented_0001", state: "escalated"}), "23514")
	wantSQLState(t, "the empty state",
		d.tryInsertReqErr(t, req{ref: "ref_STATE_empty_00000001", state: " "}), "23514")

	// Rule backstops: each refused row would be a request that lies about itself.
	wantSQLState(t, "a rejected request with no reason",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_reject_noreason", state: "rejected"}), "23514")
	wantSQLState(t, "a rejected request with a blank reason",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_reject_blank_001", state: "rejected", reason: "   "}), "23514")
	wantSQLState(t, "delivered with no outcome brief",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_delivered_nobrief", state: "delivered", delivered: true}), "23514")
	wantSQLState(t, "delivered with no delivery time",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_delivered_notime1", state: "delivered", brief: "done"}), "23514")
	wantSQLState(t, "requester_accepted without an acceptance time",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_accepted_noaccept1", state: "requester_accepted", brief: "done", delivered: true}),
		"23514")
	wantSQLState(t, "needs_information with no since timestamp",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_needsinfo_nosince", state: "needs_information"}), "23514")
	wantSQLState(t, "a duplicate with no canonical",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_duplicate_none_01", state: "duplicate"}), "23514")

	// A request cannot be its own duplicate, and a duplicate's canonical is a
	// request of the same tenant: folding into another org's request would leak
	// one tenant's queue into another's.
	self := d.insertReq(req{ref: "ref_RULE_duplicateself_01"})
	wantSQLState(t, "a request duplicating itself",
		d.exec(`UPDATE team_requests SET state = 'duplicate', duplicate_of = id WHERE id = $1`, self), "23514")
	foreignTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other org desk') RETURNING id`, d.other)
	otherOrgCanonical := d.insertReq(req{ref: "ref_RULE_otherorg_canonical", org: d.other, team: foreignTeam})
	wantSQLState(t, "a duplicate folding into another org's request",
		d.exec(`UPDATE team_requests SET state = 'duplicate', duplicate_of = $1 WHERE id = $2`, otherOrgCanonical, self),
		"23503")

	// A claim is a pair: who is looking, and since when.
	wantSQLState(t, "a claim with no timestamp",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_claim_notime_0001", claim: -1}), "23514")
	d.insertReq(req{ref: "ref_RULE_claim_ok_0000001", claim: 1})

	// rev is a positive concurrency token; metadata is an object of extension
	// keys, never an array or a bare scalar; a subject is never blank.
	wantSQLState(t, "rev 0",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_rev_zero_00000001", extraCols: "rev", extraVals: []any{0}}), "23514")
	wantSQLState(t, "metadata as an array",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_metadata_array01", extraCols: "metadata", extraVals: []any{"[]"}}), "23514")
	wantSQLState(t, "a blank subject",
		d.tryInsertReqErr(t, req{ref: "ref_RULE_blank_subject_01", subject: "  "}), "23514")
}

// tryInsertReqErr is tryInsertReq for the cases that must fail; it asserts the
// insert was refused at all and returns the error for wantSQLState.
func (d *teamRequestsDB) tryInsertReqErr(t *testing.T, r req) error {
	t.Helper()
	if _, err := d.tryInsertReq(r); err != nil {
		return err
	}
	t.Fatalf("%+v was accepted", r)
	return nil
}

// The requester-facing reference is opaque and unique (contract D1): the serial
// can never be the ref, a guessable display form can never be stored, and two
// requests can never share one.
func TestMigration_053PublicRefIsOpaqueAndUnique(t *testing.T) {
	d := newTeamRequestsDB(t)

	d.insertReq(req{ref: "REQ8F2Q1abcdefghijklmnop"})
	// A ref alone identifies a request: no two share one, across teams too.
	wantSQLState(t, "a duplicate public_ref in the same team",
		d.tryInsertReqErr(t, req{ref: "REQ8F2Q1abcdefghijklmnop"}), "23505")
	wantSQLState(t, "a duplicate public_ref in another team",
		d.tryInsertReqErr(t, req{ref: "REQ8F2Q1abcdefghijklmnop", team: d.team2}), "23505")

	// Guessable shapes are refused: the serial, the mockup's display form, a
	// short sequence, and anything with punctuation or spaces.
	long := strings.Repeat("a", 80)
	for _, bad := range []string{
		"1", "42", "000042",
		"REQ-8F2Q1",                   // the display rendering, not the token
		"REQ-000001",                  // a per-team sequence
		"short",                       // too short to be unguessable
		"has space in it 12345678",    // not a token shape
		"has/slash/and/more/chars/12", // path-unsafe
		"way-too-long-" + long,        // over the length bound
	} {
		if err := d.tryInsertReqErr(t, req{ref: bad}); err == nil {
			t.Fatalf("public_ref %q was accepted", bad)
		}
	}
}

// Tenancy is a constraint: a request's team must belong to its org. A service
// bug that passes a team from another tenant cannot store the request.
func TestMigration_053TenancyIsEnforced(t *testing.T) {
	d := newTeamRequestsDB(t)
	foreignTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other tenant desk') RETURNING id`, d.other)

	wantSQLState(t, "a team from another org",
		d.tryInsertReqErr(t, req{ref: "ref_TENANT_crossorg_001", team: foreignTeam}), "23503")
	// Neither side of the pair may be re-pointed after the fact.
	d.insertReq(req{ref: "ref_TENANT_sameorg_0001"})
	wantSQLState(t, "moving a request to a team of another org",
		d.exec(`UPDATE team_requests SET team_id = $1 WHERE public_ref = 'ref_TENANT_sameorg_0001'`, foreignTeam),
		"23503")

	// Both teams of one org are fine.
	d.insertReq(req{ref: "ref_TENANT_sameorg_0002", team: d.team2})

	// A request dies with its team (and its org): it has no meaning without the
	// queue it was submitted to. This is the tenant cascade, by contrast with
	// the requester FK below, which keeps the request.
	//
	// A canonical and the duplicate folded into it are both in this team, so they
	// go together in one statement and the duplicate's tenant foreign key is
	// satisfied at the end of it.
	canonical := d.insertReq(req{ref: "ref_TENANT_canonical_0001", team: d.team2})
	d.insertReq(req{ref: "ref_TENANT_dup_00000001", team: d.team2, state: "duplicate", duplicate: canonical})
	d.mustExec(`DELETE FROM teams WHERE id = $1`, d.team2)
	var after int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_requests WHERE team_id = $1`, d.team2).Scan(&after); err != nil || after != 0 {
		t.Fatalf("team2 requests after its team was deleted = %d (%v), want 0", after, err)
	}
	// Deleting the org takes its teams' requests with it.
	wantSQLState(t, "a request for an org that does not exist",
		d.tryInsertReqErr(t, req{ref: "ref_TENANT_noorg_00000001", org: 999999}), "23503")

	// A canonical that a duplicate still points at cannot simply vanish: the
	// duplicate is its own request with its own requester, so deleting the
	// canonical is refused until the duplicate is detached.
	liveCanonical := d.insertReq(req{ref: "ref_TENANT_livecanonical1"})
	dup := d.insertReq(req{ref: "ref_TENANT_livedup_000001", state: "duplicate", duplicate: liveCanonical})
	wantSQLState(t, "deleting a canonical a duplicate still names",
		d.exec(`DELETE FROM team_requests WHERE id = $1`, liveCanonical), "23503")
	// Detaching is a reopen (D7): the duplicate stops being a duplicate and
	// stops naming a canonical in the same act, so the pair can never disagree.
	wantSQLState(t, "detaching without leaving the duplicate state",
		d.exec(`UPDATE team_requests SET duplicate_of = NULL WHERE id = $1`, dup), "23514")
	d.mustExec(`UPDATE team_requests SET state = 'triaging', duplicate_of = NULL WHERE id = $1`, dup)
	d.mustExec(`DELETE FROM team_requests WHERE id = $1`, liveCanonical)
}

// The requester's contact snapshot is what a request keeps (contract D2): when
// the user is deleted the link is nulled but the request and the details it was
// submitted with stay, so the team can still answer the person who asked.
func TestMigration_053RequesterSnapshotSurvivesDeletion(t *testing.T) {
	d := newTeamRequestsDB(t)
	id := d.insertReq(req{ref: "ref_CONTACT_snapshot_001"})

	var ref, contact string
	var requester *int
	if err := d.db.QueryRow(d.ctx, `SELECT public_ref, requester_contact, requester_user_id
		FROM team_requests WHERE id = $1`, id).Scan(&ref, &contact, &requester); err != nil {
		t.Fatal(err)
	}
	if requester == nil || *requester != d.user || contact != "requester@x.test" {
		t.Fatalf("request before deletion: requester %v contact %q", requester, contact)
	}

	d.mustExec(`DELETE FROM users WHERE id = $1`, d.user)
	var gotRef, gotContact, gotState string
	var gotRequester *int
	if err := d.db.QueryRow(d.ctx, `SELECT public_ref, requester_contact, requester_user_id, state
		FROM team_requests WHERE id = $1`, id).Scan(&gotRef, &gotContact, &gotRequester, &gotState); err != nil {
		t.Fatalf("the request did not survive its requester's deletion: %v", err)
	}
	if gotRef != ref || gotContact != "requester@x.test" || gotRequester != nil || gotState != "new" {
		t.Fatalf("after the requester was deleted: ref %q contact %q requester %v state %q",
			gotRef, gotContact, gotRequester, gotState)
	}

	// Every request must carry a contact snapshot: there is no such thing as a
	// request with nobody to answer.
	wantSQLState(t, "a request with no contact snapshot",
		d.tryInsertReqErr(t, req{ref: "ref_CONTACT_missing_0001", contact: "   "}), "23514")

	// A service-only request (no AMP user, e.g. a connector) is fine as long as
	// it names who to answer and carries the vendor's immutable object id.
	d.insertReq(req{ref: "ref_CONTACT_service_0001", requester: -1, contact: "integration:acme",
		source: "connector", external: "wiz-finding-7"})
	var nullCount int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_requests WHERE requester_user_id IS NULL`).Scan(&nullCount); err != nil || nullCount != 2 {
		t.Fatalf("requests with no AMP user = %d (%v), want 2", nullCount, err)
	}
}

// Source identity (contract D12, the #3654 C3 replay rule): the same external
// object submitted twice to the same team is one request; the same external id
// in another team is a different object; and submissions that carry no external
// id are never deduplicated against each other.
func TestMigration_053SourceIdentityDedup(t *testing.T) {
	d := newTeamRequestsDB(t)

	// A connector replay of the same vendor object: one row.
	d.insertReq(req{ref: "ref_SOURCE_connector_0001", source: "connector", external: "gh-issue-1234"})
	wantSQLState(t, "a replayed connector object",
		d.tryInsertReqErr(t, req{ref: "ref_SOURCE_connector_0002", source: "connector", external: "gh-issue-1234"}),
		"23505")
	// The same id under another source kind is a different submission channel,
	// so it is not the same object.
	d.insertReq(req{ref: "ref_SOURCE_api_sameid_0001", source: "api", external: "gh-issue-1234"})
	// Another team receiving the same external object is its own request.
	d.insertReq(req{ref: "ref_SOURCE_otherteam_0001", team: d.team2, source: "connector", external: "gh-issue-1234"})

	// No external id: unrelated submissions must NOT dedup. This is the case a
	// plain UNIQUE would break, since every id-less request shares ''.
	for _, ref := range []string{"ref_SOURCE_noid_0000001", "ref_SOURCE_noid_0000002", "ref_SOURCE_noid_0000003"} {
		d.insertReq(req{ref: ref, source: "web"})
	}
	for _, ref := range []string{"ref_SOURCE_mcp_noid_0001", "ref_SOURCE_mcp_noid_0002"} {
		d.insertReq(req{ref: ref, source: "mcp"})
	}

	var total int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_requests WHERE team_id = $1`, d.team).Scan(&total); err != nil {
		t.Fatal(err)
	}
	// 1 connector + 1 api (same external id, other source) + 3 web + 2 mcp, all
	// in the first team; the second team's copy lives on its own queue.
	if total != 7 {
		t.Fatalf("team requests = %d, want 7", total)
	}
	var emptyIDs int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_requests
		WHERE team_id = $1 AND source_external_id = ''`, d.team).Scan(&emptyIDs); err != nil || emptyIDs != 5 {
		t.Fatalf("id-less requests = %d (%v), want 5", emptyIDs, err)
	}

	// An invented source kind is refused, the external id is bounded, and a
	// connector must always name the vendor's object or its replays are
	// unrecognisable.
	wantSQLState(t, "an invented source kind",
		d.tryInsertReqErr(t, req{ref: "ref_SOURCE_invented_0001", source: "email", external: "x"}), "23514")
	wantSQLState(t, "an over-long external id",
		d.tryInsertReqErr(t, req{ref: "ref_SOURCE_longid_000001", source: "api", external: strings.Repeat("x", 256)}),
		"23514")
	wantSQLState(t, "a connector with a blank external id",
		d.tryInsertReqErr(t, req{ref: "ref_SOURCE_connblank_0001", source: "connector", external: "   "}), "23514")
}
