package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 054 adds a request's discussion and history — `request_comments`
// and `request_events` (AMP #3757; approved contract artifact 36 v2 §1,
// invariants I5 and I6, build slice A2).
//
// These tests prove what the schema is responsible for, on a real database, so
// none of them depends on the service that will later write these rows:
//
//   - it applies both ways (fresh, and on top of 053 with existing work), and
//     re-running its SQL by hand adds nothing;
//   - a comment's kind is a closed set that matches domain.RequestCommentKinds,
//     and attribution (the name snapshot and the user behind it) persists;
//   - an event's action is a closed set, a transition carries its from/to
//     states, no other verb may claim states, and the states are the same nine
//     the request itself may hold;
//   - "append-only" is the application contract the migration documents, so the
//     production Go tree carries no UPDATE or DELETE path for either table,
//     while the two writes the schema itself allows (a deleted user nulls the
//     attribution link; the request takes its history with it) do happen.

// historyDB is the 053 fixture (an org, two teams, a user) plus helpers for the
// two new tables.
type historyDB struct{ *teamRequestsDB }

func newHistoryDB(t *testing.T) *historyDB {
	t.Helper()
	return &historyDB{newTeamRequestsDB(t)}
}

// comment is one request_comments row under test. Zero values are what the
// schema's defaults supply, so a test states only the rule it is proving.
type comment struct {
	request int
	kind    string // "" = the 'comment' default
	body    string // "" = a body that satisfies the not-blank rule
	author  string // "" = the 'system' default
	user    int    // 0 = NULL, >0 = that AMP user
}

func (d *historyDB) insertComment(c comment) (int64, error) {
	d.t.Helper()
	kind := c.kind
	if kind == "" {
		kind = "comment"
	}
	body := c.body
	if body == "" {
		body = "a remark"
	}
	var author *string
	if c.author != "" {
		author = &c.author
	}
	var user *int
	if c.user > 0 {
		user = &c.user
	}
	var id int64
	err := d.db.QueryRow(d.ctx, `INSERT INTO request_comments (request_id, kind, body, author, author_user_id)
		VALUES ($1, $2, $3, COALESCE($4, 'system'), $5) RETURNING id`,
		c.request, kind, body, author, user).Scan(&id)
	return id, err
}

func (d *historyDB) mustComment(c comment) int64 {
	d.t.Helper()
	id, err := d.insertComment(c)
	if err != nil {
		d.t.Fatalf("insert comment %+v: %v", c, err)
	}
	return id
}

// event is one request_events row under test.
type event struct {
	request int
	org     int    // 0 = the fixture's org
	team    int    // 0 = the fixture's first team
	action  string // "" = 'submit'
	from    string
	to      string
	actor   string // "" = the 'system' default
	user    int    // 0 = NULL
	detail  string
}

func (d *historyDB) insertEvent(e event) (int64, error) {
	d.t.Helper()
	org, team, action := e.org, e.team, e.action
	if org == 0 {
		org = d.org
	}
	if team == 0 {
		team = d.team
	}
	if action == "" {
		action = "submit"
	}
	actor := e.actor
	if actor == "" {
		actor = "system"
	}
	var user *int
	if e.user > 0 {
		user = &e.user
	}
	var id int64
	err := d.db.QueryRow(d.ctx, `INSERT INTO request_events
		(request_id, org_id, team_id, action, from_state, to_state, actor, actor_user_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		e.request, org, team, action, e.from, e.to, actor, user, e.detail).Scan(&id)
	return id, err
}

func (d *historyDB) mustEvent(e event) int64 {
	d.t.Helper()
	id, err := d.insertEvent(e)
	if err != nil {
		d.t.Fatalf("insert event %+v: %v", e, err)
	}
	return id
}

// Migration 054 applies on a fresh database and on top of 053 with existing
// work in it: both new tables start empty, nothing existing changes, and
// re-running the SQL by hand adds nothing.
func TestMigration_054RequestHistory(t *testing.T) {
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
		for _, table := range []string{"request_comments", "request_events"} {
			var n int
			if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("%s starts with %d rows, want 0", table, n)
			}
		}
		// The tables must be usable the moment they exist: one thread row and
		// one history row against a request the app just created.
		d := newHistoryDB(t)
		reqID := d.insertReq(req{ref: "ref_FRESH054_0000000001"})
		d.mustComment(comment{request: reqID, author: "amp:amp-worker", user: d.user})
		d.mustEvent(event{request: reqID, action: "submit", actor: "requester@x.test", user: d.user})
	})

	t.Run("upgrade from 053", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "054" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 054")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 053: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// Work that already exists at 053: a project with a ticket, and a
		// request already in triage.
		var projectID, epic, story, taskID, orgID, teamID, userID, requestID int
		q := func(sql string, dst *int, args ...any) {
			t.Helper()
			if err := db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`INSERT INTO projects (name, code) VALUES ('up054', 'up054') RETURNING id`, &projectID)
		q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, projectID)
		q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, projectID, epic)
		q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 't') RETURNING id`, &taskID, projectID, epic, story)
		q(`INSERT INTO orgs (name, slug) VALUES ('pre-054', 'pre054') RETURNING id`, &orgID)
		q(`INSERT INTO teams (org_id, name) VALUES ($1, 'pre-054 desk') RETURNING id`, &teamID, orgID)
		q(`INSERT INTO users (subject, email) VALUES ('pre054-sub', 'pre054@x.test') RETURNING id`, &userID)
		q(`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject, state)
			VALUES ('ref_PRE054UPGRADE0001', $1, $2, $3, 'pre054@x.test', 'a request before 054', 'triaging') RETURNING id`,
			&requestID, orgID, teamID, userID)

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 054: %v", err)
		}
		got, err := repo.GetTask(ctx, taskID)
		if err != nil || got.ProjectID != projectID {
			t.Fatalf("existing ticket after upgrade: %+v %v", got, err)
		}
		// The pre-existing request is untouched and can now carry history: an
		// upgrade adds a place to put the discussion, it never invents one.
		for _, table := range []string{"request_comments", "request_events"} {
			var n int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s after upgrade: %d %v", table, n, err)
			}
		}
		var state string
		if err := db.QueryRow(ctx, `SELECT state FROM team_requests WHERE id = $1`, requestID).Scan(&state); err != nil || state != "triaging" {
			t.Fatalf("pre-existing request state = %q (%v), want triaging", state, err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO request_comments (request_id, kind, body, author, author_user_id)
			VALUES ($1, 'question', 'which environment?', 'Desk lead', $2)`, requestID, userID); err != nil {
			t.Fatalf("comment on a pre-054 request: %v", err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO request_events (request_id, org_id, team_id, action, from_state, to_state, actor, actor_user_id)
			VALUES ($1, $2, $3, 'transition', 'new', 'triaging', 'Desk lead', $4)`, requestID, orgID, teamID, userID); err != nil {
			t.Fatalf("event on a pre-054 request: %v", err)
		}

		// Re-running 054's SQL by hand is harmless, twice over: nothing is added
		// a second time (a duplicated constraint or index would be a schema that
		// drifts every time someone fixes it by hand).
		counts := func() (constraints, indexes map[string]int) {
			t.Helper()
			constraints, indexes = map[string]int{}, map[string]int{}
			rows, err := db.Query(ctx, `SELECT conrelid::regclass::text, count(*) FROM pg_constraint
				WHERE conrelid IN ('request_comments'::regclass, 'request_events'::regclass) GROUP BY 1`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var table string
				var n int
				if err := rows.Scan(&table, &n); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				constraints[table] = n
			}
			rows.Close()
			irows, err := db.Query(ctx, `SELECT tablename, count(*) FROM pg_indexes
				WHERE tablename IN ('request_comments', 'request_events') GROUP BY 1`)
			if err != nil {
				t.Fatal(err)
			}
			for irows.Next() {
				var table string
				var n int
				if err := irows.Scan(&table, &n); err != nil {
					irows.Close()
					t.Fatal(err)
				}
				indexes[table] = n
			}
			irows.Close()
			return constraints, indexes
		}
		cBefore, iBefore := counts()
		for _, m := range migrations {
			if m.Version == "054_request_history" {
				for i := 0; i < 2; i++ {
					if _, err := db.Exec(ctx, m.SQL); err != nil {
						t.Fatalf("re-running 054 (pass %d): %v", i+1, err)
					}
				}
			}
		}
		cAfter, iAfter := counts()
		for table, n := range cBefore {
			if cAfter[table] != n {
				t.Fatalf("re-running 054 changed %s's constraint count %d -> %d", table, n, cAfter[table])
			}
		}
		for table, n := range iBefore {
			if iAfter[table] != n {
				t.Fatalf("re-running 054 changed %s's index count %d -> %d", table, n, iAfter[table])
			}
		}
		// The rows planted before the re-run are still there, still one each.
		for _, table := range []string{"request_comments", "request_events"} {
			var n int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE request_id = $1`, requestID).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s after re-applying 054 = %d (%v), want 1", table, n, err)
			}
		}
	})
}

// A comment's kind is a closed set matching the domain vocabulary exactly, and
// attribution is stored as written — including after the user who wrote it is
// gone, because the name snapshot is what keeps the thread readable.
func TestMigration_054CommentKindsAndAttribution(t *testing.T) {
	d := newHistoryDB(t)
	id := d.insertReq(req{ref: "ref_CMT_kinds_000000001"})

	// Every kind the domain knows is storable, so the CHECK is a whitelist and
	// not an accidental subset of RequestCommentKinds.
	for _, kind := range domain.RequestCommentKinds {
		rowID := d.mustComment(comment{request: id, kind: string(kind), body: "body of a " + string(kind), author: "Desk lead", user: d.user})
		var storedKind, storedBody string
		if err := d.db.QueryRow(d.ctx, `SELECT kind, body FROM request_comments WHERE id = $1`, rowID).
			Scan(&storedKind, &storedBody); err != nil {
			t.Fatal(err)
		}
		if storedKind != string(kind) || storedBody != "body of a "+string(kind) {
			t.Fatalf("kind %q stored as %q / %q", kind, storedKind, storedBody)
		}
	}
	// The default the schema supplies is the plain discussion kind.
	var defaultKind string
	if err := d.db.QueryRow(d.ctx, `SELECT kind FROM request_comments WHERE request_id = $1 ORDER BY id LIMIT 1`, id).Scan(&defaultKind); err != nil {
		t.Fatal(err)
	}
	if defaultKind != string(domain.RequestCommentDiscussion) {
		t.Fatalf("default comment kind = %q, want %q", defaultKind, domain.RequestCommentDiscussion)
	}

	// An invented kind is refused, including the blank one: a row whose kind
	// nobody defined is a comment no reader or rule can interpret.
	for _, bad := range []string{"reply", "note", "Answer", " ", "system; drop"} {
		_, badErr := d.insertComment(comment{request: id, kind: bad})
		wantSQLState(t, "comment kind "+strconv.Quote(bad), badErr, "23514")
	}

	// A comment always says something and always carries attribution: an
	// anonymous or empty row in a requester-visible thread is a bug, not a state.
	_, err := d.insertComment(comment{request: id, body: "   "})
	wantSQLState(t, "a blank comment body", err, "23514")
	_, err = d.insertComment(comment{request: id, author: "   "})
	wantSQLState(t, "a blank author", err, "23514")

	// Attribution as written: a service client with no AMP user still names
	// itself; a person's row records both the name and the user behind it.
	service := d.mustComment(comment{request: id, kind: "system", author: "integration:acme", body: "received from acme"})
	var author string
	var authorUser *int
	if err := d.db.QueryRow(d.ctx, `SELECT author, author_user_id FROM request_comments WHERE id = $1`, service).
		Scan(&author, &authorUser); err != nil {
		t.Fatal(err)
	}
	if author != "integration:acme" || authorUser != nil {
		t.Fatalf("service comment attribution: author %q user %v, want the integration name and no user", author, authorUser)
	}
	byUser := d.mustComment(comment{request: id, body: "answered above", author: "Sam Requester", user: d.user})
	if err := d.db.QueryRow(d.ctx, `SELECT author, author_user_id FROM request_comments WHERE id = $1`, byUser).
		Scan(&author, &authorUser); err != nil {
		t.Fatal(err)
	}
	if author != "Sam Requester" || authorUser == nil || *authorUser != d.user {
		t.Fatalf("person's comment attribution: author %q user %v", author, authorUser)
	}

	// Deleting the user nulls the link and keeps the name. This is one of the
	// two writes the schema itself makes to a history row, and it is why
	// immutability is an application promise rather than a trigger.
	d.mustExec(`DELETE FROM users WHERE id = $1`, d.user)
	if err := d.db.QueryRow(d.ctx, `SELECT author, author_user_id FROM request_comments WHERE id = $1`, byUser).
		Scan(&author, &authorUser); err != nil {
		t.Fatalf("the comment did not survive its author's deletion: %v", err)
	}
	if author != "Sam Requester" || authorUser != nil {
		t.Fatalf("after the author was deleted: author %q user %v, want the name kept and the link nulled", author, authorUser)
	}
}

// An event's action is a closed set, a transition names both ends, no other
// verb may claim states, and the states are the same nine a request may hold.
func TestMigration_054EventActionsAndTransitions(t *testing.T) {
	d := newHistoryDB(t)
	id := d.insertReq(req{ref: "ref_EVT_shape_000000001"})

	// Every action the domain knows is storable.
	for _, action := range domain.RequestEventActions {
		e := event{request: id, action: string(action), actor: "Desk lead", user: d.user, detail: `{"ref":"x"}`}
		if action == domain.RequestActionTransition {
			e.from, e.to = "new", "triaging"
		}
		rowID := d.mustEvent(e)
		var storedAction, storedDetail string
		var storedUser *int
		if qErr := d.db.QueryRow(d.ctx, `SELECT action, detail, actor_user_id FROM request_events WHERE id = $1`, rowID).
			Scan(&storedAction, &storedDetail, &storedUser); qErr != nil {
			t.Fatal(qErr)
		}
		if storedAction != string(action) || storedDetail != `{"ref":"x"}` || storedUser == nil || *storedUser != d.user {
			t.Fatalf("event %q stored as action %q detail %q user %v", action, storedAction, storedDetail, storedUser)
		}
	}

	// An invented action is refused (and the column has no default to fall back
	// on: an unlabelled history row would be unreadable).
	for _, bad := range []string{"escalate", "completed", "TRANSITION", " "} {
		_, badErr := d.insertEvent(event{request: id, action: bad})
		wantSQLState(t, "event action "+strconv.Quote(bad), badErr, "23514")
	}

	// Every one of the nine lifecycle states can appear on either end of a
	// transition: the history cannot invent a state the request cannot hold.
	for _, from := range domain.RequestStatuses {
		for _, to := range domain.RequestStatuses {
			d.mustEvent(event{request: id, action: "transition", from: string(from), to: string(to)})
		}
	}
	// ... and a state outside that set is refused on either end.
	_, err := d.insertEvent(event{request: id, action: "transition", from: "escalated", to: "triaging"})
	wantSQLState(t, "an invented from_state", err, "23514")
	_, err = d.insertEvent(event{request: id, action: "transition", from: "new", to: "escalated"})
	wantSQLState(t, "an invented to_state", err, "23514")

	// A transition names both ends; no other verb claims states. Half a
	// transition, or states smuggled onto a claim, would make the history rail
	// say something the request never was.
	_, err = d.insertEvent(event{request: id, action: "transition", to: "triaging"})
	wantSQLState(t, "a transition with no from_state", err, "23514")
	_, err = d.insertEvent(event{request: id, action: "transition", from: "new"})
	wantSQLState(t, "a transition with no to_state", err, "23514")
	_, err = d.insertEvent(event{request: id, action: "claim", from: "new", to: "triaging"})
	wantSQLState(t, "a claim carrying states", err, "23514")
	_, err = d.insertEvent(event{request: id, action: "submit", to: "new"})
	wantSQLState(t, "a submit carrying a to_state", err, "23514")

	// From/to persist as written on a transition, and a non-transition row
	// keeps both blank.
	transition := d.mustEvent(event{request: id, action: "transition", from: "triaging", to: "needs_information",
		actor: "amp:amp-manager", user: d.user, detail: `{"question_comment_id":12}`})
	var from, to, actor, detail string
	var actorUser *int
	if qErr := d.db.QueryRow(d.ctx, `SELECT from_state, to_state, actor, actor_user_id, detail
		FROM request_events WHERE id = $1`, transition).Scan(&from, &to, &actor, &actorUser, &detail); qErr != nil {
		t.Fatal(qErr)
	}
	if from != "triaging" || to != "needs_information" || actor != "amp:amp-manager" ||
		actorUser == nil || *actorUser != d.user || detail != `{"question_comment_id":12}` {
		t.Fatalf("transition row: from %q to %q actor %q user %v detail %q", from, to, actor, actorUser, detail)
	}
	claim := d.mustEvent(event{request: id, action: "claim", actor: "Desk lead"})
	if err := d.db.QueryRow(d.ctx, `SELECT from_state, to_state FROM request_events WHERE id = $1`, claim).Scan(&from, &to); err != nil {
		t.Fatal(err)
	}
	if from != "" || to != "" {
		t.Fatalf("claim row states = %q / %q, want both blank", from, to)
	}

	// A history row is never anonymous.
	_, err = d.insertEvent(event{request: id, action: "claim", actor: "  "})
	wantSQLState(t, "a blank event actor", err, "23514")
}

// I5's lookup — "the latest question asked about this request" — is a query the
// schema supports directly, and a thread reads back in append order.
func TestMigration_054DiscussionThreadShape(t *testing.T) {
	d := newHistoryDB(t)
	id := d.insertReq(req{ref: "ref_THREAD_00000000001"})
	other := d.insertReq(req{ref: "ref_THREAD_other_00001"})

	d.mustComment(comment{request: id, kind: "comment", body: "looking at this", author: "Desk lead"})
	firstQ := d.mustComment(comment{request: id, kind: "question", body: "which environment?", author: "Desk lead"})
	d.mustComment(comment{request: id, kind: "answer", body: "staging", author: "requester@x.test"})
	secondQ := d.mustComment(comment{request: id, kind: "question", body: "and the region?", author: "Desk lead"})
	d.mustComment(comment{request: id, kind: "comment", body: "still working on it", author: "Desk lead"})
	// A second request's question must not leak into this request's lookup.
	d.mustComment(comment{request: other, kind: "question", body: "unrelated?"})

	var latestQuestion int64
	var body string
	if err := d.db.QueryRow(d.ctx, `SELECT id, body FROM request_comments
		WHERE request_id = $1 AND kind = 'question' ORDER BY id DESC LIMIT 1`, id).Scan(&latestQuestion, &body); err != nil {
		t.Fatalf("latest question query: %v", err)
	}
	if latestQuestion != secondQ || body != "and the region?" {
		t.Fatalf("latest question = %d %q, want %d", latestQuestion, body, secondQ)
	}
	// The partial index the lookup rides on exists, so the query does not scan
	// the whole thread once threads get long.
	var indexed bool
	if err := d.db.QueryRow(d.ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE tablename = 'request_comments' AND indexname = 'idx_request_comments_question')`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("idx_request_comments_question is missing: the I5 lookup would scan the whole thread")
	}

	// The thread reads back in append order, by id — stable even when several
	// rows share a transaction timestamp.
	var bodies []string
	rows, err := d.db.Query(d.ctx, `SELECT body FROM request_comments WHERE request_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, b)
	}
	want := []string{"looking at this", "which environment?", "staging", "and the region?", "still working on it"}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("thread order = %v, want %v", bodies, want)
	}
	if firstQ >= secondQ {
		t.Fatalf("question ids %d,%d are not in append order", firstQ, secondQ)
	}

	// One request's history does not show up on another's rail.
	otherEvent := d.mustEvent(event{request: other, action: "claim", actor: "Desk lead"})
	var onID int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1 AND id = $2`, id, otherEvent).Scan(&onID); err != nil {
		t.Fatal(err)
	}
	if onID != 0 {
		t.Fatal("another request's event appeared on this request's history rail")
	}
}

// The parent request lifecycle owns retention: history is part of the record, so
// it is deleted with the request and with the team, never left orphaned. This is
// the documented behaviour — by contrast with immutability, which the schema
// does not enforce.
func TestMigration_054HistoryFollowsRequestLifecycle(t *testing.T) {
	d := newHistoryDB(t)
	id := d.insertReq(req{ref: "ref_CASCADE_0000000001"})
	d.mustComment(comment{request: id, kind: "question", body: "which environment?"})
	d.mustEvent(event{request: id, action: "transition", from: "new", to: "triaging"})

	// History cannot be written for a request that does not exist.
	_, err := d.insertComment(comment{request: 999999})
	wantSQLState(t, "a comment on a request that does not exist", err, "23503")
	_, err = d.insertEvent(event{request: 999999})
	wantSQLState(t, "an event for a request that does not exist", err, "23503")

	d.mustExec(`DELETE FROM team_requests WHERE id = $1`, id)
	for _, table := range []string{"request_comments", "request_events"} {
		var n int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM `+table+` WHERE request_id = $1`, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s after its request was deleted = %d (%v), want 0", table, n, err)
		}
	}

	// The same when the whole team goes: history has no life of its own.
	teamReq := d.insertReq(req{ref: "ref_CASCADE_team_0000001", team: d.team2})
	d.mustComment(comment{request: teamReq, author: "Desk lead"})
	d.mustEvent(event{request: teamReq, team: d.team2, action: "claim", actor: "Desk lead"})
	d.mustExec(`DELETE FROM teams WHERE id = $1`, d.team2)
	for _, table := range []string{"request_comments", "request_events"} {
		var n int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s after its team was deleted = %d (%v), want 0", table, n, err)
		}
	}

	// Tenant consistency is a constraint, not a convention in the write path.
	// Both halves of the event's scope are foreign keys, so a history row can
	// never cross an org through either the request or the team.
	foreignTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other tenant desk') RETURNING id`, d.other)
	foreignReq := d.insertReq(req{ref: "ref_CASCADE_otherorg_0001", org: d.other, team: foreignTeam})
	_, err = d.insertEvent(event{request: foreignReq, org: d.org, team: d.team})
	wantSQLState(t, "an event whose org is not its request's org", err, "23503")
	_, err = d.insertEvent(event{request: foreignReq, org: d.other, team: d.team})
	wantSQLState(t, "an event whose team is in another org", err, "23503")
	// The consistent row is accepted.
	d.mustEvent(event{request: foreignReq, org: d.other, team: foreignTeam, action: "submit", actor: "connector:acme"})
	// A foreign key violation is not confused with a CHECK violation: the
	// tenant rule is enforced by the relation, not by a string comparison.
	var withoutFK int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'request_events'::regclass AND contype = 'f'`).Scan(&withoutFK); err != nil {
		t.Fatal(err)
	}
	if withoutFK < 3 {
		t.Fatalf("request_events has %d foreign keys, want the request/org, team/org and user relations", withoutFK)
	}
}

// "Append-only" is the application contract migration 054 documents, so this
// asserts it where it now lives: the production Go tree carries no UPDATE,
// DELETE or TRUNCATE path against either table. A future service that edits a
// comment in place or prunes history by hand fails here first, and has to change
// the documented contract along with it.
func TestMigration_054NoUpdateOrDeletePath(t *testing.T) {
	const moduleRoot = "../.." // v2/amp-api, relative to this package
	verbPatterns := func(table string) []*regexp.Regexp {
		return []*regexp.Regexp{
			regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?` + table + `\b`),
			regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+(?:ONLY\s+)?` + table + `\b`),
			regexp.MustCompile(`(?is)\bTRUNCATE\b[^;]{0,200}\b` + table + `\b`),
		}
	}
	tables := []string{"request_comments", "request_events"}

	fset := token.NewFileSet()
	var scanned int
	var offenders []string
	err := filepath.WalkDir(moduleRoot, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			name := e.Name()
			if name == "node_modules" || name == "vendor" || name == "testdata" ||
				(strings.HasPrefix(name, ".") && path != moduleRoot) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		// SQL lives in string literals, so the scan is over the literals the AST
		// holds, not the raw file: a comment that mentions a table is not a write
		// path. Literals are also joined per file, so a statement split across
		// concatenated strings is still seen as one.
		var literals []string
		ast.Inspect(src, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				literals = append(literals, lit.Value)
			}
			return true
		})
		joined := strings.Join(literals, " ")
		for _, table := range tables {
			for _, re := range verbPatterns(table) {
				for _, lit := range append(literals, joined) {
					if re.MatchString(lit) {
						offenders = append(offenders, filepath.ToSlash(path)+": "+table+" in "+firstLine(lit))
						break
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", moduleRoot, err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files under %s: the walk is not reaching the module", scanned, moduleRoot)
	}
	if len(offenders) > 0 {
		t.Fatalf("request history is append-only by contract; these write paths update or delete it:\n%s\n"+
			"An edit or a retraction is a new row, and retention follows the parent request lifecycle.",
			strings.Join(offenders, "\n"))
	}

	// The migration must not rewrite the tables it creates either: 054 is the
	// last word on their shape until a later, numbered migration says otherwise.
	var historySQL string
	for _, m := range migrations {
		if m.Version == "054_request_history" {
			historySQL = m.SQL
		}
	}
	if historySQL == "" {
		t.Fatal("migration 054 is not embedded")
	}
	upper := strings.ToUpper(historySQL)
	for _, forbidden := range []string{"ALTER TABLE REQUEST_COMMENTS", "ALTER TABLE REQUEST_EVENTS",
		"DROP TABLE REQUEST_COMMENTS", "DROP TABLE REQUEST_EVENTS"} {
		if strings.Contains(upper, forbidden) {
			t.Fatalf("054 contains %q", forbidden)
		}
	}
	// It must also carry no UPDATE/DELETE of its own: history starts empty, and
	// a migration that back-fills or prunes it would be a write path by another
	// name.
	for _, table := range tables {
		for _, re := range verbPatterns(table) {
			if re.MatchString(historySQL) {
				t.Fatalf("054 itself writes to %s (%s)", table, re)
			}
		}
	}
}

// The two tables are additive: nothing that existed at 053 changed shape or
// gained an index, and history hangs off the request rather than the other way
// round. (053's own test asserts team_requests' index count from the other
// side; this one is the 054-side promise.)
func TestMigration_054LeavesExistingSchemaAlone(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
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

	var indexes int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'team_requests'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 9 {
		t.Fatalf("team_requests has %d indexes, want the 9 from 053", indexes)
	}
	var backRefs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'team_requests'::regclass
		  AND confrelid IN ('request_comments'::regclass, 'request_events'::regclass)`).Scan(&backRefs); err != nil {
		t.Fatal(err)
	}
	if backRefs != 0 {
		t.Fatalf("team_requests references the history tables (%d FKs), want history to hang off the request", backRefs)
	}

	// Their own catalogue: the tables exist with exactly the columns the domain
	// reads, in the order the types expect.
	wantCols := map[string][]string{
		"request_comments": {"id", "request_id", "kind", "body", "author", "author_user_id", "created_at"},
		"request_events":   {"id", "request_id", "org_id", "team_id", "action", "from_state", "to_state", "actor", "actor_user_id", "detail", "created_at"},
	}
	for table, cols := range wantCols {
		rows, err := db.Query(ctx, `SELECT column_name FROM information_schema.columns
			WHERE table_name = $1 ORDER BY ordinal_position`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, c)
		}
		rows.Close()
		if strings.Join(got, ",") != strings.Join(cols, ",") {
			t.Fatalf("%s columns = %v, want %v", table, got, cols)
		}
	}
}

// firstLine trims a SQL literal for an assertion message.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
