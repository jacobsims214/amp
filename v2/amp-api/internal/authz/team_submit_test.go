package authz

// Checker.TeamSubmit — the team-inbox submit capability (AMP #3770, build
// slice B3; approved contract artifact 36 v2 §2).
//
// These are the three claims the capability makes, and each has one test:
//
//   - the full actor matrix decides correctly: an org owner/admin, a global
//     admin, the team's lead and its plain members, and the holder of an
//     in-force submit grant may submit; a lead or member of a different team in
//     the same org, a plain org member, a customer with no grant, an agent with
//     no role and a stranger from another org may not (TestTeamSubmitMatrix);
//   - a revoked grant is denied on the very next call, with no cache to reset
//     (TestTeamSubmitRevocationIsImmediate);
//   - no project Level value satisfies it on its own — a user at Owner or
//     Member level on the inbox's own project, reached through a *different*
//     team, is refused, while a granted customer with zero project access
//     passes (TestTeamSubmitIsNotAProjectLevel).
//
// They run against a real Postgres and the real migrations, and only when
// TEST_DATABASE_URL is set, like the repository package's grant tests:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/authz -run TeamSubmit -count=1
//
// Every fixture gets a random suffix so repeated runs and parallel tests never
// see each other's orgs, teams or users.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// teamSubmitMigrations reads the real migration files, so the tables these
// tests check against are exactly the ones a deployment has.
func teamSubmitMigrations(t *testing.T) []repository.Migration {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "server", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	ms := make([]repository.Migration, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ms = append(ms, repository.Migration{Version: strings.TrimSuffix(filepath.Base(f), ".sql"), SQL: string(b)})
	}
	return ms
}

// submitFixture is one test's tenant: an inbox team and the principals around
// it. Three teams are linked to the inbox's own project on purpose (the inbox
// team as `member`, teamPeer as `member`, teamOwner as `owner`), so a test can
// hand someone a real project Level on that project without ever making them a
// member of the inbox team.
type submitFixture struct {
	t     *testing.T
	repo  *repository.Repo
	check *Checker
	ctx   context.Context

	org      int
	otherOrg int

	// team is the inbox the capability is asked about; otherTeam is a second
	// team in the same org; foreignTeam belongs to another tenant.
	team, otherTeam, foreignTeam int
	// teamPeer and teamOwner are linked to team's own project but are not team.
	teamPeer, teamOwner int
	// project is the inbox's project; projectOther is otherTeam's.
	project, projectOther int

	// admin is an owner of org; lead leads team; member is a plain member of
	// team; peer is a plain member of teamPeer; ownerUser is a plain member of
	// teamOwner (owner link); lead2 leads otherTeam; plainOrg is an org member
	// with no team role; cust is a customer who holds the grant; ungranted is a
	// customer with no grant; stranger owns otherOrg; agent is an agent-kind
	// account with no role of its own; super is a global admin.
	admin, lead, member, peer, ownerUser, lead2, plainOrg int
	cust, ungranted, stranger, agent, super               int
}

func newSubmitFixture(t *testing.T) *submitFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the TeamSubmit tests against Postgres")
	}
	ctx := context.Background()

	// The repository is what migrations and the Checker run through; the raw
	// connection is only the fixture's writer (orgs, teams, project links and
	// users have no repository API this test should depend on).
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.RunMigrations(ctx, teamSubmitMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (fixture): %v", err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })

	suffix := rand.IntN(1_000_000_000)
	f := &submitFixture{t: t, repo: repo, check: NewChecker(repo), ctx: ctx}

	q := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := db.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	f.org = q(`INSERT INTO orgs (name, slug) VALUES ('submit org', $1) RETURNING id`, fmt.Sprintf("submit-%d", suffix))
	f.otherOrg = q(`INSERT INTO orgs (name, slug) VALUES ('submit other', $1) RETURNING id`, fmt.Sprintf("submit-other-%d", suffix))
	f.team = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, f.org)
	f.otherTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Second desk') RETURNING id`, f.org)
	f.foreignTeam = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign desk') RETURNING id`, f.otherOrg)
	f.teamPeer = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Peer team') RETURNING id`, f.org)
	f.teamOwner = q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Owner team') RETURNING id`, f.org)
	f.project = q(`INSERT INTO projects (name, code, org_id) VALUES ('Inbox work', $1, $2) RETURNING id`,
		fmt.Sprintf("SUB%d", suffix), f.org)
	f.projectOther = q(`INSERT INTO projects (name, code, org_id) VALUES ('Other work', $1, $2) RETURNING id`,
		fmt.Sprintf("SBO%d", suffix), f.org)
	// The inbox's own project, reachable through three different teams: the
	// inbox team itself, a peer team, and a team linked as owner.
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.project, f.team)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.project, f.teamPeer)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'owner')`, f.project, f.teamOwner)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, f.projectOther, f.otherTeam)

	user := func(handle, kind string) int {
		t.Helper()
		return q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("%s-%d", handle, suffix), fmt.Sprintf("%s-%d@submit.test", handle, suffix), kind)
	}
	f.admin = user("submit-admin", domain.UserKindHuman)
	f.lead = user("submit-lead", domain.UserKindHuman)
	f.member = user("submit-member", domain.UserKindHuman)
	f.peer = user("submit-peer", domain.UserKindHuman)
	f.ownerUser = user("submit-owneruser", domain.UserKindHuman)
	f.lead2 = user("submit-lead2", domain.UserKindHuman)
	f.plainOrg = user("submit-plainorg", domain.UserKindHuman)
	f.cust = user("submit-cust", domain.UserKindCustomer)
	f.ungranted = user("submit-ungranted", domain.UserKindCustomer)
	f.stranger = user("submit-stranger", domain.UserKindHuman)
	f.super = user("submit-super", domain.UserKindHuman)

	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.org, f.admin)
	for _, u := range []int{f.lead, f.member, f.peer, f.ownerUser, f.lead2, f.plainOrg} {
		exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, f.org, u)
	}
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, f.otherOrg, f.stranger)
	// The customers are deliberately in no org and on no team: a grant is the
	// only thing that can let them submit.
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, f.team, f.lead)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.team, f.member)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.teamPeer, f.peer)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.teamOwner, f.ownerUser)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, f.otherTeam, f.lead2)
	exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, f.super)
	// An agent account holds its own access; it inherits nothing from the lead
	// that manages it (the contract's "agents do what their token's human may"
	// is a token property, not an account one).
	f.agent = q(`INSERT INTO users (subject, email, kind, managed_by) VALUES ($1, $2, 'agent', $3) RETURNING id`,
		fmt.Sprintf("submit-agent-%d", suffix), fmt.Sprintf("submit-agent-%d@submit.test", suffix), f.lead)

	return f
}

func (f *submitFixture) principal(userID int) Principal {
	return Principal{UserID: userID, Email: fmt.Sprintf("user-%d@submit.test", userID)}
}

func (f *submitFixture) grant(teamID, userID, by int) {
	f.t.Helper()
	if _, err := f.repo.GrantTeamSubmit(f.ctx, teamID, f.org, userID, &by); err != nil {
		f.t.Fatalf("GrantTeamSubmit(%d,%d): %v", teamID, userID, err)
	}
}

func (f *submitFixture) revoke(teamID, userID, by int) {
	f.t.Helper()
	if _, err := f.repo.RevokeTeamSubmit(f.ctx, teamID, userID, &by); err != nil {
		f.t.Fatalf("RevokeTeamSubmit(%d,%d): %v", teamID, userID, err)
	}
}

// TestTeamSubmitMatrix is the capability matrix: who may submit to an inbox.
func TestTeamSubmitMatrix(t *testing.T) {
	f := newSubmitFixture(t)
	f.grant(f.team, f.cust, f.lead)

	rows := []struct {
		name   string
		p      Principal
		teamID int
		// wantStatus is 0 when the call must pass; otherwise the HTTP status
		// the refusal must carry (404 = the caller learns nothing about the
		// team, 403 = they can already see it and simply may not submit).
		wantStatus int
	}{
		{"org owner", f.principal(f.admin), f.team, 0},
		{"global admin", Principal{UserID: f.super, Super: true}, f.team, 0},
		{"team lead", f.principal(f.lead), f.team, 0},
		{"team member", f.principal(f.member), f.team, 0},
		{"customer holding a submit grant", f.principal(f.cust), f.team, 0},

		{"lead of another team in the same org", f.principal(f.lead2), f.team, 403},
		{"plain org member with no team role", f.principal(f.plainOrg), f.team, 403},
		{"member of a peer team linked to the same project", f.principal(f.peer), f.team, 403},
		{"owner of another team linked to the same project", f.principal(f.ownerUser), f.team, 403},

		{"customer with no grant", f.principal(f.ungranted), f.team, 404},
		{"agent account with no role of its own", f.principal(f.agent), f.team, 404},
		{"owner of another org", f.principal(f.stranger), f.team, 404},
		{"no AMP user and not Super", Principal{}, f.team, 404},
		{"a team that does not exist", f.principal(f.admin), 1 << 30, 404},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			err := f.check.TeamSubmit(f.ctx, row.p, row.teamID)
			if row.wantStatus == 0 {
				if err != nil {
					t.Fatalf("TeamSubmit = %v, want pass", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("TeamSubmit passed, want %d", row.wantStatus)
			}
			if got := StatusOf(err, 500); got != row.wantStatus {
				t.Fatalf("TeamSubmit = %v (status %d), want status %d", err, got, row.wantStatus)
			}
			if _, ok := err.(*Error); !ok {
				t.Fatalf("TeamSubmit error %v is not an authz error, so a listener could not map it", err)
			}
		})
	}
}

// TestTeamSubmitRevocationIsImmediate proves the check reads the grant every
// time: the same Checker instance (nothing rebuilt, no cache reset) passes,
// then fails on the very next call after a revoke, then passes again when the
// grant is revived, then fails again. A cached allow-set would fail this test
// at the first "revoked" step.
func TestTeamSubmitRevocationIsImmediate(t *testing.T) {
	f := newSubmitFixture(t)
	p := f.principal(f.cust)

	// No grant yet: a customer in no org learns nothing (404, never 403).
	if err := f.check.TeamSubmit(f.ctx, p, f.team); StatusOf(err, 500) != 404 {
		t.Fatalf("ungranted customer: %v, want 404", err)
	}

	f.grant(f.team, f.cust, f.lead)
	if err := f.check.TeamSubmit(f.ctx, p, f.team); err != nil {
		t.Fatalf("after grant: %v, want pass", err)
	}

	f.revoke(f.team, f.cust, f.lead)
	if err := f.check.TeamSubmit(f.ctx, p, f.team); StatusOf(err, 500) != 404 {
		t.Fatalf("immediately after revoke: %v, want 404 with no cache reset", err)
	}

	// Reviving a revoked grant is the access now in force: submit again.
	f.grant(f.team, f.cust, f.lead)
	if err := f.check.TeamSubmit(f.ctx, p, f.team); err != nil {
		t.Fatalf("after re-grant: %v, want pass", err)
	}

	f.revoke(f.team, f.cust, f.lead)
	if err := f.check.TeamSubmit(f.ctx, p, f.team); err == nil {
		t.Fatal("after second revoke: passed, want refusal")
	}
}

// TestTeamSubmitIsNotAProjectLevel proves the capability is outside the project
// Level ladder in both directions: real project access on the inbox's *own*
// project does not let someone submit (at Owner as well as Member), and a
// granted customer with no project access at all does. That rules out an
// implementation that quietly consulted ProjectLevel.
func TestTeamSubmitIsNotAProjectLevel(t *testing.T) {
	f := newSubmitFixture(t)
	f.grant(f.team, f.cust, f.lead)

	for _, row := range []struct {
		name   string
		userID int
		min    Level
	}{
		{"member of a peer team on the inbox project", f.peer, Member},
		{"member of a team that owns the inbox project", f.ownerUser, Owner},
	} {
		t.Run(row.name, func(t *testing.T) {
			lvl, err := f.check.ProjectLevel(f.ctx, f.principal(row.userID), f.project)
			if err != nil {
				t.Fatalf("ProjectLevel: %v", err)
			}
			if lvl < row.min {
				t.Fatalf("fixture is wrong: level on the inbox project = %s, want at least %s", lvl, row.min)
			}
			if err := f.check.TeamSubmit(f.ctx, f.principal(row.userID), f.team); err == nil {
				t.Fatalf("project level %s satisfied TeamSubmit, which must never happen", lvl)
			}
		})
	}

	// The other direction: submit access without project visibility.
	lvl, err := f.check.ProjectLevel(f.ctx, f.principal(f.cust), f.project)
	if err != nil {
		t.Fatalf("ProjectLevel(customer): %v", err)
	}
	if lvl != None {
		t.Fatalf("granted customer's level on the inbox project = %s, want none", lvl)
	}
	if _, err := f.check.RequireProject(f.ctx, f.principal(f.cust), f.project, Member); StatusOf(err, 500) != 404 {
		t.Fatalf("granted customer reached the project: %v, want 404", err)
	}
	if err := f.check.TeamSubmit(f.ctx, f.principal(f.cust), f.team); err != nil {
		t.Fatalf("granted customer with no project access: %v, want pass", err)
	}

	// And the grant is per team: submitting to a sibling team, or to a team in
	// another org, is not implied.
	if err := f.check.TeamSubmit(f.ctx, f.principal(f.cust), f.otherTeam); err == nil {
		t.Fatal("a grant on one team satisfied another team")
	}
	if err := f.check.TeamSubmit(f.ctx, f.principal(f.cust), f.foreignTeam); err == nil {
		t.Fatal("a grant in one org satisfied a team in another org")
	}
}
