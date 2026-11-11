package repository

// Tests for request_grants.go against a real Postgres (AMP #3768). They run
// only when TEST_DATABASE_URL is set, like kb_test.go:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run TeamSubmit -count=1
//
// The three claims this layer makes are what the tests assert, in order: a
// grant round-trips (create, read, revoke, read again); a duplicate grant is
// idempotent and a revocation is effective on the very next read; and granting
// creates no membership, so a customer with a grant still reaches no project.
// Each guard was mutation-tested by hand: dropping the `revoked_at IS NULL`
// filter from a read, letting the upsert reset an active grant's attribution,
// or making revoke a DELETE each makes one of these fail.
//
// Every test creates its own org, team and users, so repeated runs and
// concurrent tests don't see each other's rows.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// grantsTestRepo returns a Repo whose database has every migration applied. It
// is deliberately separate from kbTestRepo so a failure here names this
// subsystem, not the KB's.
func grantsTestRepo(t *testing.T) *Repo {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the submit-grant tests against Postgres")
	}
	ctx := context.Background()
	r, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(r.Close)
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "server", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	var ms []Migration
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ms = append(ms, Migration{Version: strings.TrimSuffix(filepath.Base(f), ".sql"), SQL: string(b)})
	}
	if err := r.RunMigrations(ctx, ms); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return r
}

// grantsFixture is one test's org, its two teams, and the identities a grant
// involves: a team lead, a customer (the usual grantee), and a second org that
// must never be reachable.
type grantsFixture struct {
	r        *Repo
	ctx      context.Context
	org      int
	other    int
	team     int
	team2    int
	lead     int
	cust     int
	stranger int
}

func newGrantsFixture(t *testing.T) *grantsFixture {
	t.Helper()
	r := grantsTestRepo(t)
	ctx := context.Background()
	// Random suffixes keep concurrent tests and repeated runs apart, the same
	// way kb_test.go's newScope does.
	suffix := rand.IntN(1_000_000_000)
	f := &grantsFixture{r: r, ctx: ctx}
	insert := func(sql string, dst *int, args ...any) {
		t.Helper()
		if err := r.db.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	insert(`INSERT INTO orgs (name, slug) VALUES ('grants org', $1) RETURNING id`, &f.org, fmt.Sprintf("grants-%d", suffix))
	insert(`INSERT INTO orgs (name, slug) VALUES ('grants other', $1) RETURNING id`, &f.other, fmt.Sprintf("grants-other-%d", suffix))
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Desk') RETURNING id`, &f.team, f.org)
	insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Second desk') RETURNING id`, &f.team2, f.org)
	insert(`INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`, &f.lead, fmt.Sprintf("grants-lead-%d", suffix), fmt.Sprintf("grants-lead-%d@x.test", suffix))
	insert(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, 'customer') RETURNING id`, &f.cust, fmt.Sprintf("grants-cust-%d", suffix), fmt.Sprintf("grants-cust-%d@x.test", suffix))
	insert(`INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`, &f.stranger, fmt.Sprintf("grants-str-%d", suffix), fmt.Sprintf("grants-str-%d@x.test", suffix))
	return f
}

// A grant round-trips: create, read, revoke, read again — and each step is
// visible to the check the submit capability actually makes.
func TestTeamSubmitGrantRoundTrip(t *testing.T) {
	f := newGrantsFixture(t)

	// Nothing before the grant.
	ok, err := f.r.HasTeamSubmitGrant(f.ctx, f.team, f.cust)
	if err != nil {
		t.Fatalf("HasTeamSubmitGrant before: %v", err)
	}
	if ok {
		t.Fatal("a user with no grant may already submit")
	}

	g, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, f.cust, &f.lead)
	if err != nil {
		t.Fatalf("GrantTeamSubmit: %v", err)
	}
	if g.TeamID != f.team || g.OrgID != f.org || g.UserID != f.cust {
		t.Fatalf("granted row = %+v, want team %d org %d user %d", g, f.team, f.org, f.cust)
	}
	if !g.Active() || g.RevokedAt != nil {
		t.Fatalf("a new grant is not active: %+v", g)
	}
	if g.GrantedBy == nil || *g.GrantedBy != f.lead {
		t.Fatalf("granted_by = %v, want %d", g.GrantedBy, f.lead)
	}
	if g.GrantedAt.IsZero() {
		t.Fatal("a new grant has no granted_at")
	}

	// The capability's read and the administration read agree.
	if ok, err := f.r.HasTeamSubmitGrant(f.ctx, f.team, f.cust); err != nil || !ok {
		t.Fatalf("HasTeamSubmitGrant after grant = %v (%v), want true", ok, err)
	}
	got, err := f.r.GetTeamSubmitGrant(f.ctx, f.team, f.cust)
	if err != nil {
		t.Fatalf("GetTeamSubmitGrant: %v", err)
	}
	if got.UserID != f.cust || !got.Active() {
		t.Fatalf("read back %+v, want the active grant for %d", got, f.cust)
	}

	// Revoke, and the very next check fails with nothing to reset.
	revoked, err := f.r.RevokeTeamSubmit(f.ctx, f.team, f.cust, &f.lead)
	if err != nil {
		t.Fatalf("RevokeTeamSubmit: %v", err)
	}
	if revoked.Active() || revoked.RevokedAt == nil {
		t.Fatalf("revoked grant is still active: %+v", revoked)
	}
	if revoked.RevokedBy == nil || *revoked.RevokedBy != f.lead {
		t.Fatalf("revoked_by = %v, want %d", revoked.RevokedBy, f.lead)
	}
	if ok, err := f.r.HasTeamSubmitGrant(f.ctx, f.team, f.cust); err != nil || ok {
		t.Fatalf("HasTeamSubmitGrant after revoke = %v (%v), want false", ok, err)
	}

	// The row is still there — revocation is a state, not a delete — and no
	// longer appears in the "may submit" listing.
	still, err := f.r.GetTeamSubmitGrant(f.ctx, f.team, f.cust)
	if err != nil {
		t.Fatalf("GetTeamSubmitGrant after revoke: %v", err)
	}
	if still.Active() {
		t.Fatal("the revoked row reads as active")
	}
	teams, err := f.r.TeamSubmitGrantTeams(f.ctx, f.cust)
	if err != nil {
		t.Fatalf("TeamSubmitGrantTeams: %v", err)
	}
	if len(teams) != 0 {
		t.Fatalf("TeamSubmitGrantTeams after revoke = %v, want none", teams)
	}

	// Re-granting revives the same row with the new grantor, and the access is
	// back on the next read.
	again, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, f.cust, &f.stranger)
	if err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if !again.Active() || again.RevokedAt != nil || again.RevokedBy != nil {
		t.Fatalf("re-vived grant = %+v, want it active with no revoker", again)
	}
	if again.GrantedBy == nil || *again.GrantedBy != f.stranger {
		t.Fatalf("re-grant granted_by = %v, want the new grantor %d", again.GrantedBy, f.stranger)
	}
	if ok, err := f.r.HasTeamSubmitGrant(f.ctx, f.team, f.cust); err != nil || !ok {
		t.Fatalf("HasTeamSubmitGrant after re-grant = %v (%v), want true", ok, err)
	}
}

// A duplicate grant is idempotent: the same pair twice leaves one row, and an
// already-active grant keeps its original grantor and time rather than being
// rewritten by whoever replayed it.
func TestTeamSubmitGrantDuplicateIsIdempotent(t *testing.T) {
	f := newGrantsFixture(t)

	first, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, f.cust, &f.lead)
	if err != nil {
		t.Fatalf("first grant: %v", err)
	}
	second, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, f.cust, &f.stranger)
	if err != nil {
		t.Fatalf("replayed grant: %v", err)
	}
	if second.GrantedBy == nil || *second.GrantedBy != f.lead {
		t.Fatalf("a replayed grant re-attributed the grant to %v, want the original %d",
			second.GrantedBy, f.lead)
	}
	if !second.GrantedAt.Equal(first.GrantedAt) {
		t.Fatalf("a replayed grant moved granted_at %v -> %v", first.GrantedAt, second.GrantedAt)
	}
	var n int
	if err := f.r.db.QueryRow(f.ctx,
		`SELECT count(*) FROM team_submit_grants WHERE team_id = $1 AND user_id = $2`, f.team, f.cust).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a replayed grant left %d rows, want 1", n)
	}

	// Revoking twice is a no-op that reports the row rather than an error: the
	// second caller learns it was already revoked and by whom.
	rev, err := f.r.RevokeTeamSubmit(f.ctx, f.team, f.cust, &f.lead)
	if err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	againRev, err := f.r.RevokeTeamSubmit(f.ctx, f.team, f.cust, &f.stranger)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if againRev.RevokedBy == nil || *againRev.RevokedBy != f.lead {
		t.Fatalf("a second revoke overwrote the revoker with %v, want the first %d", againRev.RevokedBy, f.lead)
	}
	if !againRev.RevokedAt.Equal(*rev.RevokedAt) {
		t.Fatalf("a second revoke moved revoked_at %v -> %v", rev.RevokedAt, againRev.RevokedAt)
	}
}

// The contract's central separation: granting submit access creates no
// membership row, and a granted customer still reaches zero projects.
func TestTeamSubmitGrantCreatesNoMembership(t *testing.T) {
	f := newGrantsFixture(t)
	// A project to be invisible from: a customer must reach none of it.
	var project int
	if err := f.r.db.QueryRow(f.ctx,
		`INSERT INTO projects (name, code, org_id) VALUES ('grants project', $1, $2) RETURNING id`,
		fmt.Sprintf("grants-%d", rand.IntN(1_000_000_000)), f.org).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.db.Exec(f.ctx, `INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, project, f.team); err != nil {
		t.Fatal(err)
	}

	if _, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, f.cust, &f.lead); err != nil {
		t.Fatalf("GrantTeamSubmit: %v", err)
	}

	var teamRows, orgRows int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_members WHERE user_id = $1`, f.cust).Scan(&teamRows); err != nil {
		t.Fatal(err)
	}
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM org_members WHERE user_id = $1`, f.cust).Scan(&orgRows); err != nil {
		t.Fatal(err)
	}
	if teamRows != 0 || orgRows != 0 {
		t.Fatalf("the grant created %d team_members and %d org_members rows, want 0 and 0", teamRows, orgRows)
	}

	// The access layer that decides project visibility knows nothing about
	// grants, which is the point: the granted customer sees nothing.
	projects, err := f.r.AccessibleProjectIDs(f.ctx, f.cust)
	if err != nil {
		t.Fatalf("AccessibleProjectIDs: %v", err)
	}
	if projects[project] || len(projects) != 0 {
		t.Fatalf("a granted customer reaches projects %v, want none", projects)
	}
	access, err := f.r.GetProjectAccess(f.ctx, f.cust, project)
	if err != nil {
		t.Fatalf("GetProjectAccess: %v", err)
	}
	if access.Member || access.Lead || access.OwnerTeam || access.OrgRole != "" {
		t.Fatalf("a granted customer has project access %+v, want none", access)
	}

	// And the grant is not a side effect of membership either: the customer
	// submitted nothing and joined nothing, yet may submit to exactly one team.
	teams, err := f.r.TeamSubmitGrantTeams(f.ctx, f.cust)
	if err != nil {
		t.Fatalf("TeamSubmitGrantTeams: %v", err)
	}
	if len(teams) != 1 || teams[0] != f.team {
		t.Fatalf("TeamSubmitGrantTeams = %v, want [%d]", teams, f.team)
	}
}

// Grants are per team: a grant on one team opens that inbox and no other, and a
// team's listing shows only its own grants.
func TestTeamSubmitGrantIsPerTeam(t *testing.T) {
	f := newGrantsFixture(t)
	if _, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, f.cust, &f.lead); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if ok, err := f.r.HasTeamSubmitGrant(f.ctx, f.team2, f.cust); err != nil || ok {
		t.Fatalf("a grant on team %d allowed submitting to team %d (%v)", f.team, f.team2, err)
	}
	if ok, err := f.r.HasTeamSubmitGrant(f.ctx, f.team, f.stranger); err != nil || ok {
		t.Fatalf("another user's grant allowed submitting (%v)", err)
	}

	listed, err := f.r.ListTeamSubmitGrants(f.ctx, f.team)
	if err != nil {
		t.Fatalf("ListTeamSubmitGrants: %v", err)
	}
	if len(listed) != 1 || listed[0].UserID != f.cust {
		t.Fatalf("team %d listing = %+v, want only the grant for %d", f.team, listed, f.cust)
	}
	if listed[0].Kind != domain.UserKindCustomer {
		t.Fatalf("listed grantee kind = %q, want customer", listed[0].Kind)
	}
	empty, err := f.r.ListTeamSubmitGrants(f.ctx, f.team2)
	if err != nil {
		t.Fatalf("ListTeamSubmitGrants team2: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("team %d listing = %+v, want none", f.team2, empty)
	}

	// A revoked grant stays in the listing (the manager's "who was taken away")
	// and is ordered after the active ones, but it is never access.
	if _, err := f.r.RevokeTeamSubmit(f.ctx, f.team, f.cust, &f.lead); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	after, err := f.r.ListTeamSubmitGrants(f.ctx, f.team)
	if err != nil {
		t.Fatalf("ListTeamSubmitGrants after revoke: %v", err)
	}
	if len(after) != 1 || after[0].Active() {
		t.Fatalf("listing after revoke = %+v, want the one revoked row", after)
	}
}

// A refusal writes nothing: an unknown team is ErrNotFound, a team from
// another org is ErrConflict, and an unknown user is ErrNotFound — in each case
// the table is left exactly as it was.
func TestTeamSubmitGrantRefusals(t *testing.T) {
	f := newGrantsFixture(t)

	if _, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.other, f.cust, &f.lead); !errors.Is(err, ErrConflict) {
		t.Fatalf("a cross-org grant error = %v, want ErrConflict", err)
	}
	if _, err := f.r.GrantTeamSubmit(f.ctx, 1<<30, f.org, f.cust, &f.lead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown team error = %v, want ErrNotFound", err)
	}
	if _, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, 1<<30, &f.lead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown user error = %v, want ErrNotFound", err)
	}
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_submit_grants WHERE team_id = $1`, f.team).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a refused grant left %d rows, want 0", n)
	}

	// Revoking a pair that was never granted is ErrNotFound, not a silent
	// success: the caller must be able to tell "removed it" from "there was
	// nothing to remove".
	if _, err := f.r.RevokeTeamSubmit(f.ctx, f.team, f.cust, &f.lead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a nonexistent grant error = %v, want ErrNotFound", err)
	}
	if _, err := f.r.GetTeamSubmitGrant(f.ctx, f.team, f.cust); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading a nonexistent grant error = %v, want ErrNotFound", err)
	}
}

// Two revocations racing: exactly one reports success (the others see the row
// already revoked), and the checked-out access is gone either way.
func TestTeamSubmitGrantRevocationRace(t *testing.T) {
	f := newGrantsFixture(t)
	if _, err := f.r.GrantTeamSubmit(f.ctx, f.team, f.org, f.cust, &f.lead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	const n = 5
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := f.r.RevokeTeamSubmit(context.Background(), f.team, f.cust, &f.lead)
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent revoke: %v", err)
		}
	}
	if ok, err := f.r.HasTeamSubmitGrant(f.ctx, f.team, f.cust); err != nil || ok {
		t.Fatalf("after concurrent revocations HasTeamSubmitGrant = %v (%v), want false", ok, err)
	}
	var rows int
	if err := f.r.db.QueryRow(f.ctx,
		`SELECT count(*) FROM team_submit_grants WHERE team_id = $1 AND user_id = $2`, f.team, f.cust).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("concurrent revocations left %d rows, want the one revoked row", rows)
	}
}
