package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// freshDatabase creates an empty database next to the test database and
// returns its DSN; it is dropped when the test ends.
func freshDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := fmt.Sprintf("amp_upgrade_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("can't create a scratch database (needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
		admin.Close(context.Background())
	})
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestMigration_UpgradeFromUnversionedSchema replays what every existing
// deployment looks like — migrations 001–003 applied the old way, by
// re-running the concatenated SQL on each boot, with no schema_migrations
// table — then upgrades it, and checks nobody loses access.
func TestMigration_UpgradeFromUnversionedSchema(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	// The pre-upgrade schema, applied exactly as the old main() did.
	var legacy []string
	for _, m := range migrations {
		if m.Version < "004" {
			legacy = append(legacy, m.SQL)
		}
	}
	if len(legacy) != 3 {
		t.Fatalf("expected 3 legacy migrations, got %d", len(legacy))
	}
	for i := 0; i < 2; i++ { // it ran on every boot
		if _, err := db.Exec(ctx, strings.Join(legacy, "\n")); err != nil {
			t.Fatalf("legacy migrate: %v", err)
		}
	}

	// Existing data: an admin, two members, two projects with work in them.
	var admin, m1, m2, p1, p2 int
	q := func(sql string, dest *int, args ...any) {
		t.Helper()
		if err := db.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	q(`INSERT INTO users (subject, email) VALUES ('s-admin', 'admin@x') RETURNING id`, &admin)
	q(`INSERT INTO users (subject, email) VALUES ('s-m1', 'm1@x') RETURNING id`, &m1)
	q(`INSERT INTO users (subject, email) VALUES ('s-m2', 'm2@x') RETURNING id`, &m2)
	db.Exec(ctx, `INSERT INTO user_roles VALUES ($1, 'admin'), ($2, 'member'), ($3, 'member')`, admin, m1, m2) //nolint:errcheck
	q(`INSERT INTO projects (name, code) VALUES ('P1', 'p1') RETURNING id`, &p1)
	q(`INSERT INTO projects (name, code, state) VALUES ('P2', 'p2', 'archived') RETURNING id`, &p2)
	var epic, story, task int
	q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, p1)
	q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, p1, epic)
	q(`INSERT INTO tasks (project_id, epic_id, story_id, name, state, agent_id) VALUES ($1, $2, $3, 't', 'in_progress', 'amp-worker') RETURNING id`, &task, p1, epic, story)
	// Completed before completed_at was persisted: only the log knows when.
	var done, doneNoLog int
	completedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	q(`INSERT INTO tasks (project_id, epic_id, story_id, name, state) VALUES ($1, $2, $3, 'done', 'completed') RETURNING id`, &done, p1, epic, story)
	db.Exec(ctx, `INSERT INTO activity_log (task_id, project_id, actor, action, created_at) VALUES ($1, $2, 'agent', 'completed', $3)`, done, p1, completedAt) //nolint:errcheck
	q(`INSERT INTO tasks (project_id, epic_id, story_id, name, state) VALUES ($1, $2, $3, 'done, no log', 'completed') RETURNING id`, &doneNoLog, p1, epic, story)

	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	// Every migration is recorded, so none of them runs again.
	var recorded int
	q(`SELECT COUNT(*) FROM schema_migrations`, &recorded)
	if recorded != len(migrations) {
		t.Fatalf("schema_migrations has %d rows, want %d", recorded, len(migrations))
	}

	// Everyone kept access to every project; the admin owns the default org.
	for _, uid := range []int{admin, m1, m2} {
		for _, pid := range []int{p1, p2} {
			a, err := repo.GetProjectAccess(ctx, uid, pid)
			if err != nil {
				t.Fatal(err)
			}
			if !a.Member || !a.OwnerTeam {
				t.Errorf("user %d lost access to project %d after upgrade: %+v", uid, pid, a)
			}
		}
	}
	var orgRole, teamRole string
	if err := db.QueryRow(ctx, `SELECT om.role FROM org_members om JOIN orgs o ON o.id = om.org_id WHERE o.slug = 'default' AND om.user_id = $1`, admin).Scan(&orgRole); err != nil || orgRole != "owner" {
		t.Errorf("admin's default-org role = %q (%v), want owner", orgRole, err)
	}
	if err := db.QueryRow(ctx, `SELECT tm.role FROM team_members tm JOIN teams t ON t.id = tm.team_id WHERE t.name = 'Everyone' AND tm.user_id = $1`, m1).Scan(&teamRole); err != nil || teamRole != "member" {
		t.Errorf("member's Everyone role = %q (%v), want member", teamRole, err)
	}

	// Existing work is untouched: no owner, no creator, still in progress.
	got, err := repo.GetTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerUserID != nil || got.CreatedBy != nil || got.State != "in_progress" || got.AgentID != "amp-worker" {
		t.Errorf("legacy task changed by the upgrade: %+v", got)
	}
	// Completion times are recovered from the activity log (or the last update).
	if got, _ := repo.GetTask(ctx, done); got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Errorf("backfilled completed_at = %v, want %v", got.CompletedAt, completedAt)
	}
	if got, _ := repo.GetTask(ctx, doneNoLog); got.CompletedAt == nil {
		t.Error("completed task without a log entry got no completed_at")
	}
	var orgless int
	q(`SELECT COUNT(*) FROM projects WHERE org_id IS NULL`, &orgless)
	if orgless != 0 {
		t.Errorf("%d projects left without an org", orgless)
	}

	// The backfill is one-time: unlink "Everyone" from P2, restart, and it
	// must stay unlinked (re-running it every boot would undo admin changes).
	if _, err := db.Exec(ctx, `
		DELETE FROM project_teams WHERE project_id = $1
		AND team_id = (SELECT id FROM teams WHERE name = 'Everyone')`, p2); err != nil {
		t.Fatal(err)
	}
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if a, _ := repo.GetProjectAccess(ctx, m1, p2); a.Member {
		t.Fatal("re-running migrations re-linked the default team")
	}
}

// Fresh installs get the same starting point: an empty Default org with an
// Everyone team, ready for the first admin to use.
func TestMigration_FreshInstall(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	orgs, err := repo.ListOrgs(ctx, nil)
	if err != nil || len(orgs) != 1 || orgs[0].Slug != "default" {
		t.Fatalf("orgs = %+v (%v)", orgs, err)
	}
	teams, err := repo.ListTeams(ctx, orgs[0].ID)
	if err != nil || len(teams) != 1 || teams[0].Name != "Everyone" {
		t.Fatalf("teams = %+v (%v)", teams, err)
	}
}

// Two processes booting at once (e.g. a rollout overlapping old and new
// pods) must not both apply a migration.
func TestMigration_ConcurrentBoots(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			repo, err := repository.New(ctx, dsn)
			if err != nil {
				errs <- err
				return
			}
			defer repo.Close()
			errs <- repo.RunMigrations(ctx, migrations)
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
}

// AMP must never end up without a global admin: revoking the role from, or
// deleting, the only one is refused.
func TestGlobalAdmin_LastAdminIsProtected(t *testing.T) {
	ctx := context.Background()
	repo, err := repository.New(ctx, freshDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	first, err := repo.UpsertUserFromClaims(ctx, "s-first", "first@x.test", "First", nil) // first user: auto global admin
	if err != nil || !first.HasRole("admin") {
		t.Fatalf("first user %+v %v", first, err)
	}
	second, err := repo.UpsertUserFromClaims(ctx, "s-second", "second@x.test", "Second", nil)
	if err != nil || second.HasRole("admin") {
		t.Fatalf("second user %+v %v", second, err)
	}

	if err := repo.SetUserRole(ctx, first.ID, "admin", false); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("revoking the only admin: %v, want conflict", err)
	}
	if err := repo.DeleteUser(ctx, first.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("deleting the only admin: %v, want conflict", err)
	}
	// With a second admin, the first can step down.
	if err := repo.SetUserRole(ctx, second.ID, "admin", true); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetUserRole(ctx, first.ID, "admin", false); err != nil {
		t.Fatalf("revoking one of two admins: %v", err)
	}
	if err := repo.SetUserRole(ctx, second.ID, "admin", false); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("revoking the remaining admin: %v, want conflict", err)
	}
	// Non-admin roles are unaffected by the guard.
	if err := repo.SetUserRole(ctx, first.ID, "member", false); err != nil {
		t.Fatal(err)
	}
}

// Migration 023 turns plan approval on for every project. Projects that
// didn't require it get today's plan recorded as approved, with an audit
// row, so their boards don't freeze; projects that already required it keep
// whatever awaits approval. Every ticket gets planned_at = created_at.
func TestMigration_023TurnsPlanApprovalOn(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var before, after []repository.Migration
	for _, m := range migrations {
		if m.Version < "023" {
			before = append(before, m)
		} else {
			after = append(after, m)
		}
	}
	if len(after) == 0 {
		t.Fatal("no migration 023")
	}
	if err := repo.RunMigrations(ctx, before); err != nil {
		t.Fatalf("migrate to 022: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	q := func(sql string, dest *int, args ...any) {
		t.Helper()
		if err := db.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// off: approval off (the old default), with a ready ticket.
	// gated: approval on, approved yesterday, with a ticket planned since.
	var off, gated, epic, story, offTask, gatedTask int
	q(`INSERT INTO projects (name, code) VALUES ('off', 'off') RETURNING id`, &off)
	q(`INSERT INTO projects (name, code, require_plan_approval, plan_approved_at) VALUES ('gated', 'gated', TRUE, NOW() - INTERVAL '1 day') RETURNING id`, &gated)
	q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, off)
	q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, off, epic)
	q(`INSERT INTO tasks (project_id, epic_id, story_id, name, created_at) VALUES ($1, $2, $3, 'ready', NOW() - INTERVAL '2 hours') RETURNING id`, &offTask, off, epic, story)
	var gEpic, gStory int
	q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &gEpic, gated)
	q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &gStory, gated, gEpic)
	q(`INSERT INTO tasks (project_id, epic_id, story_id, name, created_at) VALUES ($1, $2, $3, 'awaiting', NOW() - INTERVAL '1 hour') RETURNING id`, &gatedTask, gated, gEpic, gStory)

	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate 023: %v", err)
	}

	for _, pid := range []int{off, gated} {
		p, err := repo.GetProject(ctx, pid)
		if err != nil || !p.RequirePlanApproval {
			t.Fatalf("project %d after 023: %+v %v", pid, p, err)
		}
	}
	pOff, _ := repo.GetProject(ctx, off)
	tOff, _ := repo.GetTask(ctx, offTask)
	if pOff.PlanApprovedAt == nil || pOff.AwaitsApproval(tOff) || pOff.PlanApprovedBy != nil {
		t.Fatalf("grandfathered project: approved %v by %v, ticket awaits=%v", pOff.PlanApprovedAt, pOff.PlanApprovedBy, pOff.AwaitsApproval(tOff))
	}
	if !tOff.PlannedAt.Equal(tOff.CreatedAt) {
		t.Fatalf("planned_at %v, created_at %v", tOff.PlannedAt, tOff.CreatedAt)
	}
	pGated, _ := repo.GetProject(ctx, gated)
	tGated, _ := repo.GetTask(ctx, gatedTask)
	if !pGated.AwaitsApproval(tGated) {
		t.Fatal("migration approved a ticket that was waiting for a person")
	}
	entries, err := repo.ListAudit(ctx, domain.AuditFilter{Action: "plan.approve"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ProjectID == nil || *entries[0].ProjectID != off || entries[0].ActorEmail != "migration 023" ||
		entries[0].Details["grandfathered"] != true {
		t.Fatalf("audit rows: %+v", entries)
	}
	// New projects default to on, and new tickets get planned_at.
	var def bool
	if err := db.QueryRow(ctx, `INSERT INTO projects (name, code) VALUES ('new', 'new') RETURNING require_plan_approval`).Scan(&def); err != nil || !def {
		t.Fatalf("new project default: %v %v", def, err)
	}
}

// Migration 025 moves plan approval onto each ticket (AMP #2198). Every
// ticket the project-wide approval covered (planned at or before
// plan_approved_at) is backfilled as approved then, by that person (NULL for
// a grandfathered approval); tickets planned later, and tickets of projects
// never approved, keep waiting. The backfill only fills rows still NULL, so
// running it again changes nothing.
func TestMigration_025BackfillsTicketApproval(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var before []repository.Migration
	var m025 *repository.Migration
	for i, m := range migrations {
		switch {
		case m.Version < "025":
			before = append(before, m)
		case strings.HasPrefix(m.Version, "025"):
			m025 = &migrations[i]
		}
	}
	if m025 == nil {
		t.Fatal("no migration 025")
	}
	if err := repo.RunMigrations(ctx, before); err != nil {
		t.Fatalf("migrate to 024: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	q := func(sql string, dest *int, args ...any) {
		t.Helper()
		if err := db.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var approver int
	q(`INSERT INTO users (subject, email) VALUES ('sub-approver', 'approver@x.test') RETURNING id`, &approver)
	// approved: approved by a person an hour ago; grandfathered: approved by
	// migration 023 (by NULL); never: never approved.
	var approved, grandfathered, never int
	q(`INSERT INTO projects (name, code, require_plan_approval, plan_approved_at, plan_approved_by)
	   VALUES ('approved', 'approved', TRUE, NOW() - INTERVAL '1 hour', $1) RETURNING id`, &approved, approver)
	q(`INSERT INTO projects (name, code, require_plan_approval, plan_approved_at)
	   VALUES ('grandfathered', 'grandfathered', TRUE, NOW() - INTERVAL '1 hour') RETURNING id`, &grandfathered)
	q(`INSERT INTO projects (name, code, require_plan_approval) VALUES ('never', 'never', TRUE) RETURNING id`, &never)
	mk := func(project int, name, plannedAgo string) int {
		t.Helper()
		var epic, story, id int
		q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, project)
		q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, project, epic)
		q(`INSERT INTO tasks (project_id, epic_id, story_id, name, created_at, planned_at)
		   VALUES ($1, $2, $3, $4, NOW() - $5::interval, NOW() - $5::interval) RETURNING id`, &id, project, epic, story, name, plannedAgo)
		return id
	}
	covered := mk(approved, "planned before the approval", "2 hours")
	later := mk(approved, "planned after the approval", "10 minutes")
	old := mk(grandfathered, "grandfathered", "2 hours")
	waiting := mk(never, "never approved", "2 hours")

	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate 025: %v", err)
	}
	// Running the backfill again (a re-applied migration) changes nothing.
	if _, err := db.Exec(ctx, m025.SQL); err != nil {
		t.Fatalf("re-run 025: %v", err)
	}
	p, _ := repo.GetProject(ctx, approved)
	check := func(id int, wantApproved bool, wantBy *int) {
		t.Helper()
		tk, err := repo.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.Approved() != wantApproved || wantApproved != (tk.ApprovedAt != nil) {
			t.Fatalf("ticket %q: approved_at %v (planned %v), want approved=%v", tk.Name, tk.ApprovedAt, tk.PlannedAt, wantApproved)
		}
		if (tk.ApprovedBy == nil) != (wantBy == nil) || (wantBy != nil && *tk.ApprovedBy != *wantBy) {
			t.Fatalf("ticket %q: approved_by %v, want %v", tk.Name, tk.ApprovedBy, wantBy)
		}
	}
	check(covered, true, &approver)
	if tk, _ := repo.GetTask(ctx, covered); !tk.ApprovedAt.Equal(*p.PlanApprovedAt) {
		t.Fatalf("covered ticket approved at %v, want the project's approval %v", tk.ApprovedAt, p.PlanApprovedAt)
	}
	check(later, false, nil)
	check(old, true, nil)
	check(waiting, false, nil)
}

// Migration 026 records whom each ticket's approval was for (AMP #2218):
// approved_for, backfilled from the ticket's owner (or, unassigned, its
// creator) for approved tickets only. It's additive: approved_at and
// approved_by are untouched, nothing approved stops being approved, and
// running it again changes nothing.
func TestMigration_026BackfillsApprovedFor(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var before []repository.Migration
	var m026 *repository.Migration
	for i, m := range migrations {
		switch {
		case m.Version < "026":
			before = append(before, m)
		case strings.HasPrefix(m.Version, "026"):
			m026 = &migrations[i]
		}
	}
	if m026 == nil {
		t.Fatal("no migration 026")
	}
	if err := repo.RunMigrations(ctx, before); err != nil {
		t.Fatalf("migrate to 025: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	q := func(sql string, dest *int, args ...any) {
		t.Helper()
		if err := db.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var approver, owner, planner, project, epic, story int
	q(`INSERT INTO users (subject, email) VALUES ('sub-approver', 'approver@x.test') RETURNING id`, &approver)
	q(`INSERT INTO users (subject, email) VALUES ('sub-owner', 'owner@x.test') RETURNING id`, &owner)
	q(`INSERT INTO users (subject, email) VALUES ('sub-planner', 'planner@x.test') RETURNING id`, &planner)
	q(`INSERT INTO projects (name, code, require_plan_approval) VALUES ('p', 'p', TRUE) RETURNING id`, &project)
	q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, &epic, project)
	q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, &story, project, epic)
	mk := func(name string, ownerID, createdBy *int, approved bool) int {
		t.Helper()
		var id int
		q(`INSERT INTO tasks (project_id, epic_id, story_id, name, owner_user_id, created_by, created_at, planned_at,
		       approved_at, approved_by)
		   VALUES ($1, $2, $3, $4, $5, $6, NOW() - INTERVAL '2 hours', NOW() - INTERVAL '2 hours',
		       CASE WHEN $7 THEN NOW() - INTERVAL '1 hour' END, CASE WHEN $7 THEN $8::int END) RETURNING id`,
			&id, project, epic, story, name, ownerID, createdBy, approved, approver)
		return id
	}
	owned := mk("owned, approved", &owner, &planner, true)
	pool := mk("unassigned, approved", nil, &planner, true)
	nobodys := mk("nobody's, approved", nil, nil, true)
	waiting := mk("owned, awaiting", &owner, &planner, false)

	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate 026: %v", err)
	}
	if _, err := db.Exec(ctx, m026.SQL); err != nil {
		t.Fatalf("re-run 026: %v", err)
	}
	check := func(id int, wantApproved bool, wantFor *int) {
		t.Helper()
		tk, err := repo.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.Approved() != wantApproved {
			t.Fatalf("ticket %q: approved=%v, want %v", tk.Name, tk.Approved(), wantApproved)
		}
		if wantApproved && (tk.ApprovedBy == nil || *tk.ApprovedBy != approver) {
			t.Fatalf("ticket %q: approved_by %v, want %d (untouched)", tk.Name, tk.ApprovedBy, approver)
		}
		if (tk.ApprovedFor == nil) != (wantFor == nil) || (wantFor != nil && *tk.ApprovedFor != *wantFor) {
			t.Fatalf("ticket %q: approved_for %v, want %v", tk.Name, tk.ApprovedFor, wantFor)
		}
	}
	check(owned, true, &owner)
	check(pool, true, &planner)
	check(nobodys, true, nil)
	check(waiting, false, nil)
}
