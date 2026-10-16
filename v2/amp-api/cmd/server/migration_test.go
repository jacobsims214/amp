package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
