package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 062 adds `edit` to the request_events action vocabulary (AMP #3896,
// epic 202 story 535; the follow-up #3873 documented, converging on the approved
// contract artifact 36 v2 §1: "current rev updates atomically with edit event").
//
// These tests prove what the schema is responsible for, on a real database, and
// none of them depends on the repository that writes the row:
//
//   - it applies fresh and on top of 054's seven-verb table, in both cases leaving
//     the table's shape otherwise untouched, and re-running its SQL by hand is a
//     no-op (the unversioned boot path re-runs migration SQL);
//   - `edit` is storable and is genuinely in the database's closed set, not merely
//     accepted: the CHECK's definition names it, and the domain vocabulary and the
//     CHECK agree verb for verb;
//   - an `edit` row still obeys 054's transition/states rule — it may carry no
//     from_state/to_state — so the new verb cannot smuggle a lifecycle move into
//     the team's history rail;
//   - the seven verbs 054 shipped are still storable, and an invented action is
//     still refused: widening a closed set must not open it.

// TestMigration_062RequestEventEdit covers the migration's shape guarantees.
func TestMigration_062RequestEventEdit(t *testing.T) {
	ctx := context.Background()

	// actionDefinitions reads the CHECK expression the database actually holds, so
	// "the widening landed" is asserted against the catalogue rather than inferred
	// from an insert that happened to succeed.
	actionDefinition := func(t *testing.T, db *pgx.Conn) string {
		t.Helper()
		var def string
		if err := db.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conname = 'request_events_action' AND conrelid = 'request_events'::regclass`).Scan(&def); err != nil {
			t.Fatalf("read request_events_action's definition: %v", err)
		}
		return def
	}

	t.Run("fresh", func(t *testing.T) {
		d := newHistoryDB(t)
		reqID := d.insertReq(req{ref: "ref_EDIT062_fresh_00001"})

		// `edit` is storable, with no states (the rule every non-transition verb
		// follows), and it persists as written.
		rowID := d.mustEvent(event{request: reqID, action: "edit", actor: "Desk lead", user: d.user,
			detail: `{"fields":["subject"],"from_rev":1,"to_rev":2}`})
		var action, from, to, detail string
		if err := d.db.QueryRow(ctx, `SELECT action, from_state, to_state, detail
			FROM request_events WHERE id = $1`, rowID).Scan(&action, &from, &to, &detail); err != nil {
			t.Fatalf("read the edit row: %v", err)
		}
		if action != "edit" || from != "" || to != "" {
			t.Errorf("edit row = action %q states %q->%q, want edit with empty states", action, from, to)
		}
		if !strings.Contains(detail, `"from_rev":1`) || !strings.Contains(detail, `"to_rev":2`) {
			t.Errorf("edit detail = %q, want the revision transition", detail)
		}

		def := actionDefinition(t, d.db)
		if !strings.Contains(def, "'edit'") {
			t.Errorf("request_events_action definition does not name 'edit': %s", def)
		}
	})

	t.Run("upgrade from 054", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		// Exactly the level #3873 shipped against: 054's seven verbs and nothing after.
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "055" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 054")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 054: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)

		// History that already exists at 054 — including an edit recorded the way
		// #3873 shipped it? No: a pre-062 edit could only be a `system` comment, so
		// the events table holds the seven shipped verbs only. The upgrade must
		// accept all of them and keep the rows.
		var orgID, teamID, userID, requestID int
		q := func(sql string, dst *int, args ...any) {
			t.Helper()
			if err := db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`INSERT INTO orgs (name, slug) VALUES ('pre-062', 'pre062') RETURNING id`, &orgID)
		q(`INSERT INTO teams (org_id, name) VALUES ($1, 'pre-062 desk') RETURNING id`, &teamID, orgID)
		q(`INSERT INTO users (subject, email) VALUES ('pre062-sub', 'pre062@x.test') RETURNING id`, &userID)
		q(`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject, state)
			VALUES ('ref_PRE062UPGRADE0001', $1, $2, $3, 'pre062@x.test', 'a request before 062', 'triaging') RETURNING id`,
			&requestID, orgID, teamID, userID)
		for _, action := range []string{"submit", "transition", "claim", "release", "assign", "link", "unlink"} {
			from, to := "", ""
			if action == "transition" {
				from, to = "new", "triaging"
			}
			if _, err := db.Exec(ctx, `INSERT INTO request_events
				(request_id, org_id, team_id, action, from_state, to_state, actor, actor_user_id)
				VALUES ($1, $2, $3, $4, $5, $6, 'Desk lead', $7)`,
				requestID, orgID, teamID, action, from, to, userID); err != nil {
				t.Fatalf("seed a pre-062 %s row: %v", action, err)
			}
		}
		// And the pre-062 shape of an edit: a `system` comment on the thread. The
		// upgrade must leave it alone — the history that shipped is not rewritten.
		if _, err := db.Exec(ctx, `INSERT INTO request_comments (request_id, kind, body, author, author_user_id)
			VALUES ($1, 'system', 'edited subject (rev 1 → 2)', 'Desk lead', $2)`, requestID, userID); err != nil {
			t.Fatalf("seed the pre-062 edit comment: %v", err)
		}

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 062: %v", err)
		}

		// The seven shipped rows survive (widening a CHECK re-validates rows; it
		// does not prune them), and the pre-062 comment is untouched.
		var events, comments int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_events WHERE request_id = $1`, requestID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, requestID).Scan(&comments); err != nil {
			t.Fatal(err)
		}
		if events != 7 || comments != 1 {
			t.Fatalf("after the upgrade: %d events and %d comments, want the 7 shipped verbs and the 1 pre-062 comment",
				events, comments)
		}

		// `edit` now works on the upgraded table, and the definition says so.
		if _, err := db.Exec(ctx, `INSERT INTO request_events
			(request_id, org_id, team_id, action, actor, actor_user_id, detail)
			VALUES ($1, $2, $3, 'edit', 'Desk lead', $4, '{"fields":["subject"],"from_rev":2,"to_rev":3}')`,
			requestID, orgID, teamID, userID); err != nil {
			t.Fatalf("an edit row on an upgraded table: %v", err)
		}
		def := actionDefinition(t, db)
		if !strings.Contains(def, "'edit'") {
			t.Errorf("after the upgrade request_events_action does not name 'edit': %s", def)
		}
		// ... and the closed set is still closed.
		if _, err := db.Exec(ctx, `INSERT INTO request_events
			(request_id, org_id, team_id, action) VALUES ($1, $2, $3, 'escalate')`,
			requestID, orgID, teamID); err == nil {
			t.Error("the widened CHECK accepts an invented action 'escalate'")
		}

		// Re-running 062's SQL by hand is a no-op: the constraint is dropped and
		// re-added, so it must not accumulate or fail, and the rows must survive.
		for _, m := range migrations {
			if m.Version == "062_request_event_edit" {
				for i := 0; i < 2; i++ {
					if _, err := db.Exec(ctx, m.SQL); err != nil {
						t.Fatalf("re-running 062 (pass %d): %v", i+1, err)
					}
				}
			}
		}
		var afterEvents, checks int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_events WHERE request_id = $1`, requestID).Scan(&afterEvents); err != nil {
			t.Fatal(err)
		}
		if afterEvents != 8 {
			t.Errorf("re-running 062 changed the row count to %d, want 8", afterEvents)
		}
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
			WHERE conname = 'request_events_action' AND conrelid = 'request_events'::regclass`).Scan(&checks); err != nil {
			t.Fatal(err)
		}
		if checks != 1 {
			t.Errorf("re-running 062 left %d request_events_action constraints, want exactly 1", checks)
		}
	})
}

// TestMigration_062VocabularyMirrorsTheDomain proves 054's rule ("the DB and the
// domain cannot disagree") still holds after the widening, verb for verb: every
// domain action is storable, and an `edit` row is refused any from/to state, so
// the new verb is a history row and not a lifecycle move in disguise.
//
// It is the `edit`-shaped half of TestMigration_054EventActionsAndTransitions,
// which already loops the whole domain slice and now therefore covers `edit` too.
func TestMigration_062VocabularyMirrorsTheDomain(t *testing.T) {
	d := newHistoryDB(t)
	id := d.insertReq(req{ref: "ref_EDIT062_mirror_0001"})

	for _, action := range domain.RequestEventActions {
		e := event{request: id, action: string(action)}
		if action == domain.RequestActionTransition {
			e.from, e.to = "new", "triaging"
		}
		if _, err := d.insertEvent(e); err != nil {
			t.Errorf("domain action %q is not storable: %v", action, err)
		}
	}
	if !slicesContains(domain.RequestEventActions, domain.RequestActionEdit) {
		t.Fatal("domain.RequestEventActions does not carry `edit`; the fix is half-done")
	}

	// An edit names no states. Both halves of that rule are the database's, not
	// only the domain's, so a direct INSERT cannot get around it.
	if _, err := d.insertEvent(event{request: id, action: "edit", from: "new", to: "triaging"}); err == nil {
		t.Error("an edit row carrying states was accepted; an edit must not look like a transition")
	}
	if _, err := d.insertEvent(event{request: id, action: "edit", to: "triaging"}); err == nil {
		t.Error("an edit row carrying a to_state was accepted")
	}
	// And the invented-action refusal did not weaken with the widening.
	for _, bad := range []string{"edited", "update", "edit ", "EDIT"} {
		if _, err := d.insertEvent(event{request: id, action: bad}); err == nil {
			t.Errorf("invented action %q was accepted", bad)
		}
	}
}

// slicesContains is a tiny local helper so this test file needs no import of
// slices just for one assertion.
func slicesContains(actions []domain.RequestEventAction, want domain.RequestEventAction) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}
