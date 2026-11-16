package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Migration 055 adds request_links, the durable relation from a team request to
// the work that serves it (AMP #3758; approved contract artifact 36 v2 §3, build
// slice A3).
//
// These tests prove what the schema is responsible for, on a real database, so
// none of them depends on the repository or service that will later write these
// rows:
//
//   - it applies both ways (fresh, and on top of 054 with existing work), and
//     re-running its SQL by hand adds nothing;
//   - the link kinds and target kinds are closed sets matching the domain
//     vocabularies, and a kind only pairs with the target kinds the contract
//     allows;
//   - the same relation cannot be attached twice, while one target may carry
//     several different relations;
//   - a link survives the deletion of the target it names (the tombstone rule),
//     while the request's own lifecycle takes its links with it;
//   - tenancy is a constraint: a link's org must be its request's org.
//
// The absent foreign key on target_id is the point of the whole table, so the
// last test asserts its absence in the catalogue rather than trusting a comment.

// linksDB is the 053/054 fixture plus a project hierarchy to link at.
type linksDB struct {
	*teamRequestsDB
	project int
	epic    int
	story   int
	task    int
}

func newLinksDB(t *testing.T) *linksDB {
	t.Helper()
	d := &linksDB{teamRequestsDB: newTeamRequestsDB(t)}
	d.project = d.insert(`INSERT INTO projects (name, code) VALUES ('Linked project', 'lnk055') RETURNING id`)
	d.epic = d.insert(`INSERT INTO epics (project_id, name) VALUES ($1, 'epic') RETURNING id`, d.project)
	d.story = d.insert(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'story') RETURNING id`,
		d.project, d.epic)
	d.task = d.insert(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'task') RETURNING id`,
		d.project, d.epic, d.story)
	return d
}

// link is one request_links row under test. Zero values are what the schema
// supplies, so a test states only the rule it is proving.
type link struct {
	request  int
	org      int // 0 = the request's org (read from team_requests)
	kind     string
	target   string // "" = 'task'
	targetID int
	creator  int // 0 = NULL, -1 = the fixture's user
}

func (d *linksDB) insertLink(l link) (int64, error) {
	d.t.Helper()
	org := l.org
	if org == 0 {
		if err := d.db.QueryRow(d.ctx, `SELECT org_id FROM team_requests WHERE id = $1`, l.request).Scan(&org); err != nil {
			return 0, err
		}
	}
	target := l.target
	if target == "" {
		target = "task"
	}
	var creator *int
	if l.creator < 0 {
		creator = &d.user
	}
	var id int64
	err := d.db.QueryRow(d.ctx, `INSERT INTO request_links
		(request_id, org_id, link_kind, target_kind, target_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		l.request, org, l.kind, target, l.targetID, creator).Scan(&id)
	return id, err
}

func (d *linksDB) mustLink(l link) int64 {
	d.t.Helper()
	id, err := d.insertLink(l)
	if err != nil {
		d.t.Fatalf("insert link %+v: %v", l, err)
	}
	return id
}

// tryInsertLinkErr is insertLink for the cases that must fail: it asserts the
// insert was refused at all and returns the error for wantSQLState.
func (d *linksDB) tryInsertLinkErr(t *testing.T, l link) error {
	t.Helper()
	if _, err := d.insertLink(l); err != nil {
		return err
	}
	t.Fatalf("%+v was accepted", l)
	return nil
}

// validLinkPairs is every (link_kind, target_kind) pair the domain allows, read
// from the domain's own validator so the schema's CHECK cannot silently drift
// from the contract's map. Pairs the domain refuses must be refused here too.
func validLinkPairs() map[domain.RequestLinkKind][]domain.RequestTargetKind {
	out := map[domain.RequestLinkKind][]domain.RequestTargetKind{}
	for _, kind := range domain.RequestLinkKinds {
		for _, target := range domain.RequestTargetKinds {
			if _, _, err := domain.NormalizeRequestLink(string(kind), string(target)); err == nil {
				out[kind] = append(out[kind], target)
			}
		}
	}
	return out
}

// Migration 055 applies on a fresh database and on top of 054 with existing work
// in it: the table starts empty, nothing existing changes, and re-running the
// SQL by hand adds nothing.
func TestMigration_055RequestLinks(t *testing.T) {
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
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM request_links`).Scan(&n); err != nil {
			t.Fatalf("request_links: %v", err)
		}
		if n != 0 {
			t.Fatalf("request_links starts with %d rows, want 0", n)
		}
		// The table must be usable the moment it exists: the routing decision a
		// triager makes first, on a request the app just created.
		d := newLinksDB(t)
		reqID := d.insertReq(req{ref: "ref_FRESH055_0000000001"})
		d.mustLink(link{request: reqID, kind: "routes_to", target: "project", targetID: d.project, creator: -1})
	})

	t.Run("upgrade from 054", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "055" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 055")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 054: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// Work that already exists at 054: a project with a ticket, and a request
		// already routed in triage.
		var projectID, epic, story, taskID, orgID, teamID, userID, requestID int
		q := func(sql string, dst *int, args ...any) {
			t.Helper()
			if err := db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`INSERT INTO projects (name, code) VALUES ('up055', 'up055') RETURNING id`, &projectID)
		q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, projectID)
		q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, projectID, epic)
		q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 't') RETURNING id`, &taskID, projectID, epic, story)
		q(`INSERT INTO orgs (name, slug) VALUES ('pre-055', 'pre055') RETURNING id`, &orgID)
		q(`INSERT INTO teams (org_id, name) VALUES ($1, 'pre-055 desk') RETURNING id`, &teamID, orgID)
		q(`INSERT INTO users (subject, email) VALUES ('pre055-sub', 'pre055@x.test') RETURNING id`, &userID)
		q(`INSERT INTO team_requests (public_ref, org_id, team_id, requester_user_id, requester_contact, subject, state)
			VALUES ('ref_PRE055UPGRADE0001', $1, $2, $3, 'pre055@x.test', 'a request before 055', 'triaging') RETURNING id`,
			&requestID, orgID, teamID, userID)
		var commentID int
		q(`INSERT INTO request_comments (request_id, kind, body, author)
			VALUES ($1, 'comment', 'routing this to the plan', 'Desk lead') RETURNING id`, &commentID, requestID)
		if commentID == 0 {
			t.Fatal("the pre-055 request has no discussion row")
		}

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 055: %v", err)
		}
		// Existing work is untouched, and the new table is empty: an upgrade adds
		// a place to record lineage, it never invents one.
		got, err := repo.GetTask(ctx, taskID)
		if err != nil || got.ProjectID != projectID {
			t.Fatalf("existing ticket after upgrade: %+v %v", got, err)
		}
		var links int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_links`).Scan(&links); err != nil || links != 0 {
			t.Fatalf("request_links after upgrade: %d %v", links, err)
		}
		var state string
		if err := db.QueryRow(ctx, `SELECT state FROM team_requests WHERE id = $1`, requestID).Scan(&state); err != nil || state != "triaging" {
			t.Fatalf("pre-existing request state = %q (%v), want triaging", state, err)
		}
		// The pre-existing request and its pre-existing ticket can now be linked:
		// an upgrade opens the relation for work that was already there.
		if _, err := db.Exec(ctx, `INSERT INTO request_links (request_id, org_id, link_kind, target_kind, target_id, created_by)
			VALUES ($1, $2, 'routes_to', 'project', $3, $4)`, requestID, orgID, projectID, userID); err != nil {
			t.Fatalf("routes_to link on a pre-055 request: %v", err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO request_links (request_id, org_id, link_kind, target_kind, target_id, created_by)
			VALUES ($1, $2, 'planned_as', 'task', $3, $4)`, requestID, orgID, taskID, userID); err != nil {
			t.Fatalf("planned_as link on a pre-055 ticket: %v", err)
		}

		// Re-running 055's SQL by hand is harmless, twice over: nothing is added
		// a second time (a duplicated constraint or index would be a schema that
		// drifts every time someone fixes it by hand).
		counts := func() (constraints, indexes int) {
			t.Helper()
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'request_links'::regclass`).Scan(&constraints); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'request_links'`).Scan(&indexes); err != nil {
				t.Fatal(err)
			}
			return constraints, indexes
		}
		cBefore, iBefore := counts()
		for _, m := range migrations {
			if m.Version == "055_request_links" {
				for i := 0; i < 2; i++ {
					if _, err := db.Exec(ctx, m.SQL); err != nil {
						t.Fatalf("re-running 055 (pass %d): %v", i+1, err)
					}
				}
			}
		}
		cAfter, iAfter := counts()
		if cAfter != cBefore {
			t.Fatalf("re-running 055 changed the constraint count %d -> %d", cBefore, cAfter)
		}
		if iAfter != iBefore {
			t.Fatalf("re-running 055 changed the index count %d -> %d", iBefore, iAfter)
		}
		// The rows planted before the re-run are still there, still one each.
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, requestID).Scan(&n); err != nil || n != 2 {
			t.Fatalf("request_links after re-applying 055 = %d (%v), want 2", n, err)
		}
	})
}

// The five link kinds and seven target kinds are closed sets matching the domain
// vocabularies, and a kind only pairs with the target kinds the contract allows.
func TestMigration_055LinkKindsAndTargetPairs(t *testing.T) {
	d := newLinksDB(t)
	id := d.insertReq(req{ref: "ref_PAIRS_000000000001"})
	other := d.insertReq(req{ref: "ref_PAIRS_other_00001"})

	pairs := validLinkPairs()
	// Guard the guard: the domain must expose all five kinds and all seven
	// targets, or this test would pass by covering nothing.
	if len(pairs) != len(domain.RequestLinkKinds) {
		t.Fatalf("validLinkPairs covers %d of %d link kinds", len(pairs), len(domain.RequestLinkKinds))
	}
	seen := map[domain.RequestTargetKind]bool{}
	for _, targets := range pairs {
		for _, target := range targets {
			seen[target] = true
		}
	}
	if len(seen) != len(domain.RequestTargetKinds) {
		t.Fatalf("validLinkPairs covers %d of %d target kinds", len(seen), len(domain.RequestTargetKinds))
	}

	// Every allowed pair is storable. target_id is any positive serial: with no
	// foreign key on it, the row cannot care whether the target exists — which is
	// also what makes a tombstone possible.
	targetID := 1
	for kind, targets := range pairs {
		for _, target := range targets {
			l := link{request: id, kind: string(kind), target: string(target), targetID: targetID, creator: -1}
			if target == domain.RequestTargetRequest {
				l.targetID = other
			}
			rowID := d.mustLink(l)
			var storedKind, storedTarget string
			var storedTargetID int
			if err := d.db.QueryRow(d.ctx, `SELECT link_kind, target_kind, target_id
				FROM request_links WHERE id = $1`, rowID).Scan(&storedKind, &storedTarget, &storedTargetID); err != nil {
				t.Fatal(err)
			}
			if storedKind != string(kind) || storedTarget != string(target) || storedTargetID != l.targetID {
				t.Fatalf("link %s/%s stored as %q/%q/%d", kind, target, storedKind, storedTarget, storedTargetID)
			}
			targetID++
		}
	}

	// An invented kind or target kind is refused. There is no default either: an
	// unlabelled relation row would be unreadable.
	for _, bad := range []string{"plans", "PLANNED_AS", " ", "planned_as; drop"} {
		wantSQLState(t, "link kind "+bad, d.tryInsertLinkErr(t, link{request: id, kind: bad, target: "task", targetID: 1}), "23514")
	}
	for _, bad := range []string{"ticket", "Task", " ", "run"} {
		wantSQLState(t, "target kind "+bad, d.tryInsertLinkErr(t, link{request: id, kind: "planned_as", target: bad, targetID: 1}), "23514")
	}

	// A kind paired with a target kind the contract never allows is refused: a
	// planned_as link to an artifact, a routes_to link to a ticket, a delivered_by
	// link to a story, a relates_to link to a project.
	refused := []struct{ kind, target string }{
		{"planned_as", "project"},
		{"planned_as", "artifact"},
		{"planned_as", "receipt"},
		{"planned_as", "request"},
		{"routes_to", "epic"},
		{"routes_to", "story"},
		{"routes_to", "task"},
		{"routes_to", "request"},
		{"delivered_by", "project"},
		{"delivered_by", "epic"},
		{"delivered_by", "story"},
		{"delivered_by", "request"},
		{"duplicate_of", "project"},
		{"duplicate_of", "task"},
		{"relates_to", "artifact"},
		{"relates_to", "story"},
	}
	for _, r := range refused {
		err := d.tryInsertLinkErr(t, link{request: id, kind: r.kind, target: r.target, targetID: 1})
		wantSQLState(t, "a "+r.kind+" link to a "+r.target, err, "23514")
	}

	// A request never links to itself: "what does this relate to" would answer
	// with the request itself. Both request-target kinds are covered.
	for _, kind := range []string{"duplicate_of", "relates_to"} {
		wantSQLState(t, "a "+kind+" link from a request to itself",
			d.tryInsertLinkErr(t, link{request: id, kind: kind, target: "request", targetID: id}), "23514")
	}
	// ... while a genuine request-to-request link is fine. (A third request, so
	// this is a relation the pairs sweep above did not already attach.)
	third := d.insertReq(req{ref: "ref_PAIRS_third_000001"})
	d.mustLink(link{request: id, kind: "relates_to", target: "request", targetID: third})

	// A link with no target names nothing: target_id must be a positive serial.
	wantSQLState(t, "a zero target_id", d.tryInsertLinkErr(t, link{request: id, kind: "planned_as", target: "task", targetID: 0}), "23514")
	wantSQLState(t, "a negative target_id", d.tryInsertLinkErr(t, link{request: id, kind: "planned_as", target: "task", targetID: -4}), "23514")

	// A link must belong to a request that exists. The org is passed explicitly:
	// the helper's own lookup would fail first, and the point here is that the
	// composite foreign key refuses the row.
	wantSQLState(t, "a link on a request that does not exist",
		d.tryInsertLinkErr(t, link{request: 999999, org: d.org, kind: "planned_as", target: "task", targetID: 1}), "23503")
}

// The same relation attached twice is one row (the unique key), while one target
// may legitimately carry several different relations — a ticket planned for a
// request and later delivering it is two facts, not a duplicate.
func TestMigration_055DuplicateLinksRejected(t *testing.T) {
	d := newLinksDB(t)
	id := d.insertReq(req{ref: "ref_DUP_links_00000001"})

	d.mustLink(link{request: id, kind: "planned_as", target: "task", targetID: d.task, creator: -1})

	// The replay is refused by the unique key...
	wantSQLState(t, "the same planned_as link attached twice",
		d.tryInsertLinkErr(t, link{request: id, kind: "planned_as", target: "task", targetID: d.task}), "23505")

	// ... and an `ON CONFLICT DO NOTHING` replay converges to the one row that is
	// already there, which is how the repository's idempotent attach will work.
	var inserted int64
	err := d.db.QueryRow(d.ctx, `INSERT INTO request_links
		(request_id, org_id, link_kind, target_kind, target_id, created_by)
		VALUES ($1, (SELECT org_id FROM team_requests WHERE id = $1), 'planned_as', 'task', $2, $3)
		ON CONFLICT DO NOTHING RETURNING id`, id, d.task, d.user).Scan(&inserted)
	if err == nil {
		t.Fatalf("an ON CONFLICT DO NOTHING replay inserted a second row (%d)", inserted)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ON CONFLICT replay should be a no-op (pgx.ErrNoRows), got %v", err)
	}
	var n int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("request_links rows after replay = %d (%v), want 1", n, err)
	}
	// The first row's attribution is what survives a replay (first-write-wins).
	var creator *int
	if err := d.db.QueryRow(d.ctx, `SELECT created_by FROM request_links WHERE request_id = $1`, id).Scan(&creator); err != nil {
		t.Fatal(err)
	}
	if creator == nil || *creator != d.user {
		t.Fatalf("first row's created_by = %v, want the original attacher %d", creator, d.user)
	}

	// A different relation to the same target is a different fact and is stored.
	d.mustLink(link{request: id, kind: "delivered_by", target: "task", targetID: d.task, creator: -1})
	// So is the same relation to a different target.
	d.mustLink(link{request: id, kind: "planned_as", target: "story", targetID: d.story, creator: -1})
	// And a second request may plan the same ticket: many-to-many on both sides.
	second := d.insertReq(req{ref: "ref_DUP_second_000001"})
	d.mustLink(link{request: second, kind: "planned_as", target: "task", targetID: d.task, creator: -1})

	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, id).Scan(&n); err != nil || n != 3 {
		t.Fatalf("this request's links = %d (%v), want 3 distinct relations", n, err)
	}
}

// A link outlives the thing it names. This is why target_id carries no foreign
// key: deleting the target must neither erase the lineage row nor be refused by
// it. The request's own lifecycle is the opposite — its links go with it.
func TestMigration_055LinkSurvivesTargetDeletion(t *testing.T) {
	d := newLinksDB(t)
	id := d.insertReq(req{ref: "ref_TOMBSTONE_0000001"})

	// A delivery recorded against a task and an artifact, and the plan's story.
	d.mustLink(link{request: id, kind: "planned_as", target: "story", targetID: d.story, creator: -1})
	d.mustLink(link{request: id, kind: "delivered_by", target: "task", targetID: d.task, creator: -1})
	artifact := d.insert(`INSERT INTO artifacts (project_id, title, kind) VALUES ($1, 'the mockup', 'html') RETURNING id`, d.project)
	d.mustLink(link{request: id, kind: "delivered_by", target: "artifact", targetID: artifact, creator: -1})
	receipt := d.insert(`INSERT INTO task_receipts (task_id, project_id, summary, state) VALUES ($1, $2, 'done', 'final') RETURNING id`,
		d.task, d.project)
	d.mustLink(link{request: id, kind: "delivered_by", target: "receipt", targetID: receipt, creator: -1})

	// Deleting the task must not be blocked by the link, and must not take the
	// link with it. (A real FK with the house NO ACTION default would refuse this
	// delete; with CASCADE it would silently erase the lineage.)
	d.mustExec(`DELETE FROM tasks WHERE id = $1`, d.task)
	var taskLinks int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links
		WHERE request_id = $1 AND target_kind = 'task' AND target_id = $2`, id, d.task).Scan(&taskLinks); err != nil {
		t.Fatal(err)
	}
	if taskLinks != 1 {
		t.Fatalf("the link to the deleted task is %d rows, want 1 kept as a tombstone", taskLinks)
	}
	// The row still names what it was, so "we delivered this with task N" stays
	// readable even though the ticket no longer exists to join to.
	var storedTargetID int
	var storedKind string
	if err := d.db.QueryRow(d.ctx, `SELECT target_id, link_kind FROM request_links
		WHERE request_id = $1 AND target_kind = 'task'`, id).Scan(&storedTargetID, &storedKind); err != nil {
		t.Fatal(err)
	}
	if storedTargetID != d.task || storedKind != "delivered_by" {
		t.Fatalf("tombstoned link reads %q -> %d, want delivered_by -> %d", storedKind, storedTargetID, d.task)
	}
	// The artifact and receipt links are untouched by the ticket's deletion too.
	var delivered int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links
		WHERE request_id = $1 AND link_kind = 'delivered_by'`, id).Scan(&delivered); err != nil || delivered != 3 {
		t.Fatalf("delivered_by links after the ticket was deleted = %d (%v), want 3", delivered, err)
	}
	// Deleting the artifact likewise leaves its link behind.
	d.mustExec(`DELETE FROM artifacts WHERE id = $1`, artifact)
	var artifactLinks int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links
		WHERE request_id = $1 AND target_kind = 'artifact' AND target_id = $2`, id, artifact).Scan(&artifactLinks); err != nil {
		t.Fatal(err)
	}
	if artifactLinks != 1 {
		t.Fatalf("the link to the deleted artifact is %d rows, want 1", artifactLinks)
	}

	// Unlinking is a real operation (the contract's `unlink` event), so the row is
	// deletable by hand without deleting the request — the request itself stays.
	d.mustExec(`DELETE FROM request_links WHERE request_id = $1 AND target_kind = 'story'`, id)
	var remaining int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, id).Scan(&remaining); err != nil || remaining != 3 {
		t.Fatalf("links after a deliberate unlink = %d (%v), want 3", remaining, err)
	}
	var requestExists bool
	if err := d.db.QueryRow(d.ctx, `SELECT EXISTS (SELECT 1 FROM team_requests WHERE id = $1)`, id).Scan(&requestExists); err != nil || !requestExists {
		t.Fatalf("the request did not survive its link being removed (%v)", err)
	}

	// The request's lifecycle owns its links: deleting the request takes them.
	d.mustExec(`DELETE FROM team_requests WHERE id = $1`, id)
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links WHERE request_id = $1`, id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("links after their request was deleted = %d (%v), want 0", remaining, err)
	}
	// The same when the whole team goes.
	teamReq := d.insertReq(req{ref: "ref_TOMBSTONE_team_001", team: d.team2})
	d.mustLink(link{request: teamReq, kind: "planned_as", target: "story", targetID: d.story, creator: -1})
	d.mustExec(`DELETE FROM teams WHERE id = $1`, d.team2)
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM request_links`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("links after their team was deleted = %d (%v), want 0", remaining, err)
	}
}

// Tenancy is a constraint: a link's org must be its request's org, so a service
// bug cannot record a request's lineage into another tenant's scope. The target's
// tenant is the link service's check (there is no target FK to enforce it), which
// is exactly why the row carries the scope as a typed column.
func TestMigration_055TenancyIsEnforced(t *testing.T) {
	d := newLinksDB(t)
	id := d.insertReq(req{ref: "ref_TENANCY_links_0001"})
	foreignTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other tenant desk') RETURNING id`, d.other)
	foreignReq := d.insertReq(req{ref: "ref_TENANCY_foreign_001", org: d.other, team: foreignTeam})

	// The consistent row is accepted, and it is the request's own org.
	rowID := d.mustLink(link{request: id, kind: "planned_as", target: "task", targetID: d.task, creator: -1})
	var org int
	if err := d.db.QueryRow(d.ctx, `SELECT org_id FROM request_links WHERE id = $1`, rowID).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if org != d.org {
		t.Fatalf("link org = %d, want the request's org %d", org, d.org)
	}

	// The request's org in another tenant's row is refused by the composite FK.
	wantSQLState(t, "a link claiming an org its request is not in",
		d.tryInsertLinkErr(t, link{request: foreignReq, org: d.org, kind: "planned_as", target: "task", targetID: d.task}), "23503")

	// A foreign key violation is not confused with a CHECK violation: tenancy is
	// enforced by the relation, not by a string comparison. The composite key
	// counts as one foreign key, and created_by as the other.
	var fks int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'request_links'::regclass AND contype = 'f'`).Scan(&fks); err != nil {
		t.Fatal(err)
	}
	if fks != 2 {
		t.Fatalf("request_links has %d foreign keys, want the request/org relation and the user", fks)
	}
}

// The table is additive and its defining property is an ABSENT foreign key, so
// the last two are asserted against the catalogue: nothing at 054 changed, and
// target_id references nothing — which is what makes a tombstone storable at all.
func TestMigration_055LeavesExistingSchemaAlone(t *testing.T) {
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

	// 053's and 054's tables keep the index sets their own tests pin, and 055
	// adds no index to them and no back-reference from them.
	var requestIndexes int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'team_requests'`).Scan(&requestIndexes); err != nil {
		t.Fatal(err)
	}
	if requestIndexes != 9 {
		t.Fatalf("team_requests has %d indexes, want the 9 from 053", requestIndexes)
	}
	var backRefs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid IN ('team_requests'::regclass, 'request_comments'::regclass, 'request_events'::regclass)
		  AND confrelid = 'request_links'::regclass`).Scan(&backRefs); err != nil {
		t.Fatal(err)
	}
	if backRefs != 0 {
		t.Fatalf("an existing request table references request_links (%d FKs), want the relation to hang off the request", backRefs)
	}

	// request_links' own catalogue: the columns the domain's RequestLink reads
	// plus the tenant scope the link service checks, and nothing else.
	rows, err := db.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'request_links' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	want := []string{"id", "request_id", "org_id", "link_kind", "target_kind", "target_id", "created_by", "created_at"}
	if len(cols) != len(want) {
		t.Fatalf("request_links columns = %v, want %v", cols, want)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Fatalf("request_links columns = %v, want %v", cols, want)
		}
	}

	// No foreign key reaches a linkable target: target_id is a plain INT, so a
	// deleted target neither erases the row nor blocks its own deletion. This is
	// the single most load-bearing absence in the table.
	var targetFKs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'request_links'::regclass AND contype = 'f'
		  AND confrelid IN ('projects'::regclass, 'epics'::regclass, 'stories'::regclass,
		                    'tasks'::regclass, 'artifacts'::regclass, 'task_receipts'::regclass)`,
	).Scan(&targetFKs); err != nil {
		t.Fatal(err)
	}
	if targetFKs != 0 {
		t.Fatalf("request_links has %d foreign keys onto linkable targets, want 0 (the tombstone rule)", targetFKs)
	}
	// The unique key and the three read indexes are the whole index set.
	var indexes int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'request_links'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 5 { // primary key, unique key, three read indexes
		t.Fatalf("request_links has %d indexes, want 5", indexes)
	}
}
