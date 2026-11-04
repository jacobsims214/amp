package repository

// Tests for github_facts.go against a real Postgres (AMP #3153). They run only
// when TEST_DATABASE_URL is set, like kb_test.go:
//
//	TEST_DATABASE_URL=postgres://amp:amp@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run GitHub -count=1
//
// Every test creates its own projects (and credentials), so repeated runs and
// concurrent tests don't see each other's rows. The three things this layer is
// responsible for are what the tests assert, in order: a read/upsert/invalidate
// round trip, cross-project isolation, and the "not knowing is a state"
// discipline (a failed refresh must never look verified, and a manual override
// must survive a detection). Each guard was mutation-tested: dropping the
// project scope from a read or a delete, letting a failure overwrite a
// verification, letting a detection overwrite a manual override, or removing the
// out-of-order guard each makes one of these tests fail.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// githubFactsTestRepo returns a Repo whose database has every migration
// applied. It is deliberately separate from kbTestRepo so a failure here names
// this subsystem, not the KB's.
func githubFactsTestRepo(t *testing.T) *Repo {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the repository-facts tests against Postgres")
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

// githubFactsTestProject creates a project no other test uses and returns its
// id. Its code carries a random suffix so repeated runs never collide.
func githubFactsTestProject(t *testing.T, r *Repo, prefix string) int {
	t.Helper()
	var id int
	code := fmt.Sprintf("%s%06d", prefix, rand.IntN(1_000_000))
	if err := r.db.QueryRow(context.Background(),
		`INSERT INTO projects (name, code) VALUES ($1, $2) RETURNING id`, "facts "+code, code).Scan(&id); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return id
}

// githubFactsTestCredential attaches a credential row for a project (reference
// metadata only — the sealed secret is #3154's column and defaults to empty).
func githubFactsTestCredential(t *testing.T, r *Repo, projectID int) int {
	t.Helper()
	var id int
	if err := r.db.QueryRow(context.Background(),
		`INSERT INTO project_github_credentials (project_id, label) VALUES ($1, 'test') RETURNING id`,
		projectID).Scan(&id); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	return id
}

// ts returns a pointer to a timestamp offset from now, truncated to
// milliseconds because that is Postgres's resolution — so a comparison of what
// was written with what was read back is exact rather than flaky.
func ts(t *testing.T, offset time.Duration) *time.Time {
	t.Helper()
	v := time.Now().UTC().Add(offset).Truncate(time.Millisecond)
	return &v
}

// A verified read round-trips: what was written is what a reader gets, a later
// read replaces it, and the slug is found case-insensitively because GitHub
// treats Owner/Repo and owner/repo as one repository.
func TestGitHubRepoFactsRoundTrip(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "grf")
	cred := githubFactsTestCredential(t, r, p)
	key := GitHubFactsKey{ProjectID: p, Slug: "Owner/Repo"}

	if _, err := r.GetGitHubRepoFacts(ctx, key); !errors.Is(err, ErrNoGitHubRepoFacts) {
		t.Fatalf("get before any write: err = %v, want ErrNoGitHubRepoFacts", err)
	}

	first := ts(t, -10*time.Minute)
	got, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		CredentialID:        &cred,
		DefaultBranch:       "main",
		DefaultBranchSource: GitHubBranchSourceREST,
		Completeness:        GitHubCompletenessComplete,
		VerifiedAt:          first,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.ID == 0 || got.ProjectID != p || got.Host != GitHubFactsHost {
		t.Fatalf("upsert returned %+v", got)
	}
	if got.Slug != "Owner/Repo" {
		t.Errorf("stored slug = %q, want the reader's spelling %q", got.Slug, "Owner/Repo")
	}
	if got.DefaultBranch != "main" || got.DefaultBranchSource != GitHubBranchSourceREST {
		t.Errorf("branch = %q/%q, want main/rest", got.DefaultBranch, got.DefaultBranchSource)
	}
	if got.Completeness != GitHubCompletenessComplete || got.FetchedAt == nil || !got.FetchedAt.Equal(*first) {
		t.Errorf("completeness/fetched_at = %q/%v, want complete/%v", got.Completeness, got.FetchedAt, *first)
	}
	if got.CredentialID == nil || *got.CredentialID != cred {
		t.Errorf("credential_id = %v, want %d", got.CredentialID, cred)
	}
	if !got.Verified() || got.LastRefreshFailed() {
		t.Errorf("Verified()=%v LastRefreshFailed()=%v on a clean verified read", got.Verified(), got.LastRefreshFailed())
	}

	// A differently-cased lookup is the same row — no second row, no new answer.
	read, err := r.GetGitHubRepoFacts(ctx, GitHubFactsKey{ProjectID: p, Slug: "owner/repo"})
	if err != nil {
		t.Fatalf("case-insensitive get: %v", err)
	}
	if read.ID != got.ID || read.Slug != "Owner/Repo" {
		t.Errorf("case-insensitive get returned id=%d slug=%q, want id=%d slug=%q",
			read.ID, read.Slug, got.ID, "Owner/Repo")
	}
	var n int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM github_repo_facts WHERE project_id = $1`, p).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for one repository in one project = %d (%v), want 1", n, err)
	}

	// A later verified read replaces the branch, its source, completeness and the
	// verification time — and clears a failure, which the next section sets.
	later := ts(t, -time.Minute)
	got, err = r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		CredentialID:        &cred,
		DefaultBranch:       "trunk",
		DefaultBranchSource: GitHubBranchSourceGraphQL,
		Completeness:        GitHubCompletenessPartial,
		VerifiedAt:          later,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got.ID != read.ID {
		t.Errorf("second upsert created a new row (%d -> %d)", read.ID, got.ID)
	}
	if got.DefaultBranch != "trunk" || got.DefaultBranchSource != GitHubBranchSourceGraphQL ||
		got.Completeness != GitHubCompletenessPartial || got.FetchedAt == nil || !got.FetchedAt.Equal(*later) {
		t.Errorf("after second upsert: %+v", got)
	}

	// Listing is project-scoped and includes what was written.
	list, err := r.ListGitHubRepoFacts(ctx, p)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != got.ID {
		t.Fatalf("list = %+v, want just row %d", list, got.ID)
	}
}

// A failed refresh keeps the last good facts and their verification time; it only
// records why. That is the whole staleness mechanism: "verified at T" and "the
// refresh at T2 failed" are both visible, so stale facts stay visibly stale rather
// than looking freshly checked. A later success clears the failure.
func TestGitHubRepoFactsFailureKeepsVerifiedFacts(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "grff")
	cred := githubFactsTestCredential(t, r, p)
	key := GitHubFactsKey{ProjectID: p, Slug: "owner/repo"}

	verified := ts(t, -20*time.Minute)
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		CredentialID:        &cred,
		DefaultBranch:       "main",
		DefaultBranchSource: GitHubBranchSourceREST,
		Completeness:        GitHubCompletenessComplete,
		VerifiedAt:          verified,
	}); err != nil {
		t.Fatalf("verified upsert: %v", err)
	}

	// The refresh failed: a rate limit. Nothing good may be lost.
	failed, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{FailureClass: GitHubFactRateLimited})
	if err != nil {
		t.Fatalf("failed upsert: %v", err)
	}
	if failed.DefaultBranch != "main" || failed.DefaultBranchSource != GitHubBranchSourceREST {
		t.Errorf("a failed refresh changed the branch to %q/%q", failed.DefaultBranch, failed.DefaultBranchSource)
	}
	if failed.Completeness != GitHubCompletenessComplete {
		t.Errorf("a failed refresh changed completeness to %q", failed.Completeness)
	}
	if failed.FetchedAt == nil || !failed.FetchedAt.Equal(*verified) {
		t.Fatalf("a failed refresh moved fetched_at from %v to %v", *verified, failed.FetchedAt)
	}
	if failed.LastErrorClass != GitHubFactRateLimited || failed.LastErrorAt == nil {
		t.Errorf("failure not recorded: class=%q at=%v", failed.LastErrorClass, failed.LastErrorAt)
	}
	// Still verified, still honestly stale, and now visibly failed: the three
	// states a reader distinguishes.
	if !failed.Verified() || !failed.LastRefreshFailed() {
		t.Errorf("Verified()=%v LastRefreshFailed()=%v, want both true", failed.Verified(), failed.LastRefreshFailed())
	}
	if age := time.Since(*failed.FetchedAt); age < 19*time.Minute {
		t.Errorf("fetched_at looks fresh (%v old) after a failed refresh", age)
	}

	// A stale failure never overwrote the good read, so a reader's TTL sees it as
	// stale — that is what "a failed refresh leaves the facts visibly stale" means.
	read, err := r.GetGitHubRepoFacts(ctx, key)
	if err != nil {
		t.Fatalf("get after failure: %v", err)
	}
	if read.FetchedAt == nil || !read.FetchedAt.Equal(*verified) || read.LastErrorClass != GitHubFactRateLimited {
		t.Errorf("stored row after failure = %+v", read)
	}

	// A success clears the failure and moves the verification time forward.
	refreshed := ts(t, -time.Minute)
	ok, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		CredentialID:        &cred,
		DefaultBranch:       "main",
		DefaultBranchSource: GitHubBranchSourceREST,
		Completeness:        GitHubCompletenessComplete,
		VerifiedAt:          refreshed,
	})
	if err != nil {
		t.Fatalf("refresh after failure: %v", err)
	}
	if ok.LastErrorClass != "" || ok.LastErrorAt != nil {
		t.Errorf("a verified read left a failure behind: %q at %v", ok.LastErrorClass, ok.LastErrorAt)
	}
	if ok.FetchedAt == nil || !ok.FetchedAt.Equal(*refreshed) {
		t.Errorf("fetched_at = %v, want %v", ok.FetchedAt, *refreshed)
	}
	if !ok.Verified() || ok.LastRefreshFailed() {
		t.Errorf("Verified()=%v LastRefreshFailed()=%v after a clean refresh", ok.Verified(), ok.LastRefreshFailed())
	}

	// A failure with NO previous facts is honest too: a row that was never
	// verified (fetched_at NULL, completeness unknown) and carries the class. That
	// is "never fetched", distinct from "fetched, last refresh failed".
	neverKey := GitHubFactsKey{ProjectID: p, Slug: "owner/never"}
	never, err := r.UpsertGitHubRepoFacts(ctx, neverKey, GitHubRepoFactsWrite{FailureClass: GitHubFactNotFound})
	if err != nil {
		t.Fatalf("failure without prior facts: %v", err)
	}
	if never.FetchedAt != nil || never.Completeness != GitHubCompletenessUnknown || never.DefaultBranch != "" {
		t.Errorf("a first-attempt failure claimed facts: %+v", never)
	}
	if never.Verified() {
		t.Error("a failure with no prior facts reported itself verified")
	}
	if never.LastErrorClass != GitHubFactNotFound || never.LastErrorAt == nil {
		t.Errorf("failure not recorded: %q at %v", never.LastErrorClass, never.LastErrorAt)
	}
}

// An out-of-order verification — a slow duplicate arriving after a newer read —
// must not walk the facts backwards. The stored row wins and is returned, so a
// caller sees what is actually cached rather than what it just tried to write.
func TestGitHubRepoFactsOutOfOrderVerification(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "grfo")
	key := GitHubFactsKey{ProjectID: p, Slug: "owner/repo"}

	newer := ts(t, -time.Minute)
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "trunk", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: newer,
	}); err != nil {
		t.Fatalf("newer upsert: %v", err)
	}
	older := ts(t, -15*time.Minute)
	got, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessPartial, VerifiedAt: older,
	})
	if err != nil {
		t.Fatalf("older upsert: %v", err)
	}
	if got.DefaultBranch != "trunk" || got.Completeness != GitHubCompletenessComplete {
		t.Errorf("an older verification overwrote the newer row: %+v", got)
	}
	if got.FetchedAt == nil || !got.FetchedAt.Equal(*newer) {
		t.Errorf("fetched_at = %v, want the newer %v", got.FetchedAt, *newer)
	}

	// A failed refresh is not a verification, so it is never "out of order": it
	// records the failure against the facts that are already stored.
	failed, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{FailureClass: GitHubFactForbidden})
	if err != nil {
		t.Fatalf("failure against newer facts: %v", err)
	}
	if failed.LastErrorClass != GitHubFactForbidden || failed.DefaultBranch != "trunk" {
		t.Errorf("failure against newer facts = %+v", failed)
	}
}

// A person's manual default branch wins over detection: a detected read never
// silently replaces an override, and a manual declaration replaces either. A
// manual declaration carries no read time of its own, so the classifier treats it
// as not aging (#3133) — the last read's completeness and verification stay as
// they were rather than being destroyed.
func TestGitHubRepoFactsManualOverride(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "grfm")
	key := GitHubFactsKey{ProjectID: p, Slug: "owner/repo"}

	readAt := ts(t, -5*time.Minute)
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: readAt,
	}); err != nil {
		t.Fatalf("detected upsert: %v", err)
	}

	// The owner declares release/2. It replaces the detected branch and its
	// provenance, without inventing a verification time.
	manual, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "release/2", DefaultBranchSource: GitHubBranchSourceManual,
	})
	if err != nil {
		t.Fatalf("manual upsert: %v", err)
	}
	if manual.DefaultBranch != "release/2" || manual.DefaultBranchSource != GitHubBranchSourceManual {
		t.Fatalf("manual declaration did not win: %+v", manual)
	}
	if manual.FetchedAt == nil || !manual.FetchedAt.Equal(*readAt) {
		t.Errorf("a manual declaration changed the read time to %v, want %v", manual.FetchedAt, *readAt)
	}

	// A later detection does not overwrite the override — the owner's word is
	// still what the policy reads.
	later := ts(t, -time.Second)
	kept, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: later,
	})
	if err != nil {
		t.Fatalf("detection after override: %v", err)
	}
	if kept.DefaultBranch != "release/2" || kept.DefaultBranchSource != GitHubBranchSourceManual {
		t.Errorf("a detected read overwrote a manual override: %+v", kept)
	}
	// The detection's evidence still updates: the read happened, and its time and
	// completeness are honest facts about it.
	if kept.Completeness != GitHubCompletenessComplete || kept.FetchedAt == nil || !kept.FetchedAt.Equal(*later) {
		t.Errorf("a detected read after an override did not record its evidence: %+v", kept)
	}

	// A second declaration replaces the first: a person changing their mind is the
	// only thing that replaces a person's declaration.
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "trunk", DefaultBranchSource: GitHubBranchSourceManual,
	}); err != nil {
		t.Fatalf("second manual declaration: %v", err)
	}
	read, err := r.GetGitHubRepoFacts(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if read.DefaultBranch != "trunk" || read.DefaultBranchSource != GitHubBranchSourceManual {
		t.Errorf("a second declaration did not replace the first: %+v", read)
	}
}

// The credential recorded on the facts follows a read that succeeded, not the
// default branch specifically: "which token produced this answer" belongs to the
// answer. That matters on the one path where it can change — an anonymous read
// (credential_id NULL: a public repo, or no credential attached yet) followed by a
// credentialed one. A person's declaration and a failed refresh change nothing: a
// declaration is not a read, and a failed read produced no facts for a token to be
// credited with.
//
// credential_id can only ever go NULL -> set on a given row, because 044 keeps one
// credential per (project, host) and deleting it cascades to the facts it produced.
// So the case this guards is exactly the anonymous-then-credentialed one, and it
// stays reachable while a manual override is in place — which is why the credential
// must not be tied to the branch columns.
func TestGitHubRepoFactsCredentialFollowsTheRead(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "grfc")
	cred := githubFactsTestCredential(t, r, p)
	key := GitHubFactsKey{ProjectID: p, Slug: "owner/repo"}

	// An anonymous read: credential_id NULL is the honest "public repository, or no
	// credential attached yet" state, which the classifier must treat as weaker.
	anon, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: ts(t, -3*time.Minute),
	})
	if err != nil {
		t.Fatalf("anonymous read: %v", err)
	}
	if anon.CredentialID != nil {
		t.Fatalf("an anonymous read recorded credential_id %v", *anon.CredentialID)
	}

	// The owner declares an override before any credentialed read happens.
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "release/2", DefaultBranchSource: GitHubBranchSourceManual,
	}); err != nil {
		t.Fatalf("declaration: %v", err)
	}

	// A credentialed read under the override records its credential, and still leaves
	// the person's branch in place.
	got, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		CredentialID: &cred, DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: ts(t, -2*time.Minute),
	})
	if err != nil {
		t.Fatalf("credentialed read: %v", err)
	}
	if got.DefaultBranch != "release/2" || got.DefaultBranchSource != GitHubBranchSourceManual {
		t.Errorf("a credentialed read overwrote the override: %+v", got)
	}
	if got.CredentialID == nil || *got.CredentialID != cred {
		t.Errorf("a credentialed read under an override left credential_id %v, want %d", got.CredentialID, cred)
	}

	// A person's declaration does not change which token produced the facts.
	manual, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "trunk", DefaultBranchSource: GitHubBranchSourceManual,
	})
	if err != nil {
		t.Fatalf("second declaration: %v", err)
	}
	if manual.CredentialID == nil || *manual.CredentialID != cred {
		t.Errorf("a declaration changed the credential to %v, want %d", manual.CredentialID, cred)
	}

	// Neither does a failed refresh, and it does not clear the read's evidence.
	failed, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{FailureClass: GitHubFactServer})
	if err != nil {
		t.Fatalf("failed refresh: %v", err)
	}
	if failed.CredentialID == nil || *failed.CredentialID != cred {
		t.Errorf("a failed refresh changed the credential to %v, want %d", failed.CredentialID, cred)
	}
	if failed.LastErrorClass != GitHubFactServer || failed.DefaultBranch != "trunk" {
		t.Errorf("failed refresh = %+v", failed)
	}
}

// Cross-project isolation: the same repository in two projects is two independent
// rows with independent answers, and neither project's branch, PR or invalidation
// reaches the other's. A credential from another project is refused by the schema
// and reported as such, never stored.
func TestGitHubFactsCrossProjectIsolation(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p1 := githubFactsTestProject(t, r, "grfi1")
	p2 := githubFactsTestProject(t, r, "grfi2")
	c1 := githubFactsTestCredential(t, r, p1)
	c2 := githubFactsTestCredential(t, r, p2)
	k1 := GitHubFactsKey{ProjectID: p1, Slug: "owner/repo"}
	k2 := GitHubFactsKey{ProjectID: p2, Slug: "owner/repo"}
	at := ts(t, -time.Minute)

	// The same slug, read with two projects' credentials: two answers. Project 1's
	// token sees main; project 2's cannot see the repo's settings at all (a 404,
	// which stays unknown — never "no rule").
	if _, err := r.UpsertGitHubRepoFacts(ctx, k1, GitHubRepoFactsWrite{
		CredentialID: &c1, DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("project 1 facts: %v", err)
	}
	if _, err := r.UpsertGitHubRepoFacts(ctx, k2, GitHubRepoFactsWrite{
		CredentialID: &c2, FailureClass: GitHubFactNotFound,
	}); err != nil {
		t.Fatalf("project 2 facts: %v", err)
	}

	f1, err := r.GetGitHubRepoFacts(ctx, k1)
	if err != nil {
		t.Fatalf("project 1 read: %v", err)
	}
	f2, err := r.GetGitHubRepoFacts(ctx, k2)
	if err != nil {
		t.Fatalf("project 2 read: %v", err)
	}
	if f1.ID == f2.ID {
		t.Fatal("the same repository in two projects is one row; facts would leak across projects")
	}
	if f1.DefaultBranch != "main" || f1.LastErrorClass != "" {
		t.Errorf("project 1's facts were changed by project 2's read: %+v", f1)
	}
	if f2.DefaultBranch != "" || f2.Completeness != GitHubCompletenessUnknown || f2.LastErrorClass != GitHubFactNotFound {
		t.Errorf("project 2's facts = %+v, want unknown with a not_found class", f2)
	}
	if f2.Verified() {
		t.Error("project 2's failed read reported itself verified")
	}
	// Each row names its own project's credential, so no token-derived visibility
	// crosses over.
	if f1.CredentialID == nil || *f1.CredentialID != c1 || f2.CredentialID == nil || *f2.CredentialID != c2 {
		t.Errorf("credentials crossed projects: p1=%v p2=%v", f1.CredentialID, f2.CredentialID)
	}

	// Branch and PR facts hang off the repository row: writing them under project
	// 2's key must not touch project 1's rows for the same branch and PR number.
	if _, err := r.UpsertGitHubBranchFacts(ctx, k1, "main", GitHubBranchFactsWrite{
		Rules:        []byte(`[{"type":"pull_request","ruleset_source":"main"}]`),
		Completeness: GitHubCompletenessPartial, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("project 1 branch facts: %v", err)
	}
	if _, err := r.UpsertGitHubPullFacts(ctx, k1, 7, GitHubPullFactsWrite{
		BaseBranch: "main", State: "open", ReviewDecision: "review_required",
		RequestedReviewers: []string{"alice"}, MergeableState: "blocked",
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("project 1 PR facts: %v", err)
	}
	if _, err := r.UpsertGitHubBranchFacts(ctx, k2, "main", GitHubBranchFactsWrite{
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("project 2 branch facts: %v", err)
	}
	if _, err := r.UpsertGitHubPullFacts(ctx, k2, 7, GitHubPullFactsWrite{
		BaseBranch: "release/2", State: "open", MergeableState: "clean",
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("project 2 PR facts: %v", err)
	}

	b1, err := r.GetGitHubBranchFacts(ctx, k1, "main")
	if err != nil {
		t.Fatalf("project 1 branch read: %v", err)
	}
	b2, err := r.GetGitHubBranchFacts(ctx, k2, "main")
	if err != nil {
		t.Fatalf("project 2 branch read: %v", err)
	}
	if b1.RepoFactsID == b2.RepoFactsID {
		t.Error("branch facts for two projects hang off one repository row")
	}
	var rules []map[string]any
	if err := json.Unmarshal(b1.Rules, &rules); err != nil || len(rules) != 1 {
		t.Errorf("project 1 rules = %s (%v), want the one rule it read", b1.Rules, err)
	}
	if string(b2.Rules) != "[]" {
		t.Errorf("project 2 rules = %s, want the empty array it read", b2.Rules)
	}
	if b2.Completeness != GitHubCompletenessComplete || !b2.Verified() {
		t.Errorf("project 2 branch facts = %+v", b2)
	}

	pr1, err := r.GetGitHubPullFacts(ctx, k1, 7)
	if err != nil {
		t.Fatalf("project 1 PR read: %v", err)
	}
	pr2, err := r.GetGitHubPullFacts(ctx, k2, 7)
	if err != nil {
		t.Fatalf("project 2 PR read: %v", err)
	}
	if pr1.BaseBranch != "main" || pr2.BaseBranch != "release/2" {
		t.Errorf("PR facts crossed projects: p1=%q p2=%q", pr1.BaseBranch, pr2.BaseBranch)
	}
	if len(pr1.RequestedReviewers) != 1 || pr1.RequestedReviewers[0] != "alice" {
		t.Errorf("project 1 requested reviewers = %v", pr1.RequestedReviewers)
	}
	if len(pr2.RequestedReviewers) != 0 {
		t.Errorf("project 2 requested reviewers = %v, want none", pr2.RequestedReviewers)
	}

	// A write under one project's key cannot name another project's credential:
	// the composite foreign key refuses it and the error says what it was, without
	// echoing a value.
	_, err = r.UpsertGitHubRepoFacts(ctx, GitHubFactsKey{ProjectID: p1, Slug: "owner/leak"},
		GitHubRepoFactsWrite{CredentialID: &c2, FailureClass: GitHubFactForbidden})
	if err == nil {
		t.Fatal("project 1's facts accepted project 2's credential")
	}
	if !strings.Contains(err.Error(), "does not belong to this project") {
		t.Errorf("cross-project credential error = %v, want it to name the isolation failure", err)
	}
	if _, gerr := r.GetGitHubRepoFacts(ctx, GitHubFactsKey{ProjectID: p1, Slug: "owner/leak"}); !errors.Is(gerr, ErrNoGitHubRepoFacts) {
		t.Errorf("a refused write left a row behind: %v", gerr)
	}

	// Invalidating project 2's facts leaves project 1's alone — including its
	// branch and PR facts.
	if ok, err := r.InvalidateGitHubRepoFacts(ctx, k2); err != nil || !ok {
		t.Fatalf("invalidate project 2 = %v, %v; want true", ok, err)
	}
	if _, err := r.GetGitHubRepoFacts(ctx, k2); !errors.Is(err, ErrNoGitHubRepoFacts) {
		t.Errorf("project 2's facts survived invalidation: %v", err)
	}
	if _, err := r.GetGitHubRepoFacts(ctx, k1); err != nil {
		t.Errorf("invalidating project 2 reached project 1: %v", err)
	}
	if _, err := r.GetGitHubBranchFacts(ctx, k1, "main"); err != nil {
		t.Errorf("invalidating project 2 reached project 1's branch facts: %v", err)
	}
	if _, err := r.GetGitHubPullFacts(ctx, k1, 7); err != nil {
		t.Errorf("invalidating project 2 reached project 1's PR facts: %v", err)
	}

	// Invalidating a project's repository facts takes its branch and PR facts with
	// it (044's cascade), and a second invalidation is a no-op, not an error.
	if ok, err := r.InvalidateGitHubRepoFacts(ctx, k1); err != nil || !ok {
		t.Fatalf("invalidate project 1 = %v, %v; want true", ok, err)
	}
	if _, err := r.GetGitHubBranchFacts(ctx, k1, "main"); !errors.Is(err, ErrNoGitHubBranchFacts) {
		t.Errorf("branch facts outlived their repository row: %v", err)
	}
	if _, err := r.GetGitHubPullFacts(ctx, k1, 7); !errors.Is(err, ErrNoGitHubPullFacts) {
		t.Errorf("PR facts outlived their repository row: %v", err)
	}
	if ok, err := r.InvalidateGitHubRepoFacts(ctx, k1); err != nil || ok {
		t.Errorf("second invalidation = %v, %v; want false, nil", ok, err)
	}
}

// Branch and PR invalidation are project-scoped too: dropping one project's cached
// rules or PR facts for a branch/PR number must leave another project's rows for the
// same branch and number alone. The branch and PR deletes reach their repository row
// through a join, so this is the test that keeps that join filtering by project
// rather than only by slug — two projects watching the same repository and the same
// branch is exactly the case a webhook invalidation hits.
func TestGitHubFactsInvalidationIsProjectScoped(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p1 := githubFactsTestProject(t, r, "griv1")
	p2 := githubFactsTestProject(t, r, "griv2")
	k1 := GitHubFactsKey{ProjectID: p1, Slug: "owner/repo"}
	k2 := GitHubFactsKey{ProjectID: p2, Slug: "owner/repo"}
	at := ts(t, -time.Minute)

	for _, key := range []GitHubFactsKey{k1, k2} {
		if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
			DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
			Completeness: GitHubCompletenessComplete, VerifiedAt: at,
		}); err != nil {
			t.Fatalf("repository facts for project %d: %v", key.ProjectID, err)
		}
		if _, err := r.UpsertGitHubBranchFacts(ctx, key, "main", GitHubBranchFactsWrite{
			Rules: []byte(`[]`), Completeness: GitHubCompletenessComplete, VerifiedAt: at,
		}); err != nil {
			t.Fatalf("branch facts for project %d: %v", key.ProjectID, err)
		}
		if _, err := r.UpsertGitHubPullFacts(ctx, key, 7, GitHubPullFactsWrite{
			BaseBranch: "main", State: "open", MergeableState: "clean",
			Completeness: GitHubCompletenessComplete, VerifiedAt: at,
		}); err != nil {
			t.Fatalf("PR facts for project %d: %v", key.ProjectID, err)
		}
	}

	// Project 1's webhook delivery drops its own branch rules and PR facts.
	if ok, err := r.InvalidateGitHubBranchFacts(ctx, k1, "main"); err != nil || !ok {
		t.Fatalf("invalidate project 1's branch = %v, %v; want true", ok, err)
	}
	if ok, err := r.InvalidateGitHubPullFacts(ctx, k1, 7); err != nil || !ok {
		t.Fatalf("invalidate project 1's PR = %v, %v; want true", ok, err)
	}
	if _, err := r.GetGitHubBranchFacts(ctx, k1, "main"); !errors.Is(err, ErrNoGitHubBranchFacts) {
		t.Errorf("project 1's branch facts survived their invalidation: %v", err)
	}
	if _, err := r.GetGitHubPullFacts(ctx, k1, 7); !errors.Is(err, ErrNoGitHubPullFacts) {
		t.Errorf("project 1's PR facts survived their invalidation: %v", err)
	}
	// Project 2 watches the same repository, branch and PR number. It did not get a
	// delivery about them, so its cache is untouched.
	if _, err := r.GetGitHubBranchFacts(ctx, k2, "main"); err != nil {
		t.Errorf("invalidating project 1's branch reached project 2's: %v", err)
	}
	if _, err := r.GetGitHubPullFacts(ctx, k2, 7); err != nil {
		t.Errorf("invalidating project 1's PR reached project 2's: %v", err)
	}
	if _, err := r.GetGitHubRepoFacts(ctx, k2); err != nil {
		t.Errorf("invalidating project 1's branch or PR reached project 2's repository row: %v", err)
	}
}

// Branch facts round-trip, keep their last good rules when a refresh fails, and are
// dropped by a branch invalidation without disturbing the repository row.
func TestGitHubBranchFactsRoundTrip(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "gbrf")
	key := GitHubFactsKey{ProjectID: p, Slug: "owner/repo"}
	at := ts(t, -2*time.Minute)

	// A branch fact hangs off the repository row: without one there is nothing to
	// hang it on, so this is a miss, not a row with no scope.
	if _, err := r.UpsertGitHubBranchFacts(ctx, key, "main", GitHubBranchFactsWrite{
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); !errors.Is(err, ErrNoGitHubRepoFacts) {
		t.Fatalf("branch facts without repository facts: err = %v, want ErrNoGitHubRepoFacts", err)
	}
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("repository facts: %v", err)
	}

	// "Rules read, classic protection unreadable" is partial — a positive signal for
	// a rule, never proof that none applies.
	rules := []byte(`[{"type":"pull_request","ruleset_source":"main","parameters":{"required_approving_review_count":1}}]`)
	got, err := r.UpsertGitHubBranchFacts(ctx, key, "main", GitHubBranchFactsWrite{
		Rules: rules, Completeness: GitHubCompletenessPartial, VerifiedAt: at,
	})
	if err != nil {
		t.Fatalf("branch upsert: %v", err)
	}
	if got.Branch != "main" || got.Completeness != GitHubCompletenessPartial {
		t.Errorf("branch facts = %+v", got)
	}
	if !got.Verified() || got.LastRefreshFailed() {
		t.Errorf("Verified()=%v LastRefreshFailed()=%v", got.Verified(), got.LastRefreshFailed())
	}
	if len(got.Rules) == 0 || !strings.Contains(string(got.Rules), "required_approving_review_count") {
		t.Errorf("rules round-tripped as %s", got.Rules)
	}
	// A read on another branch is its own row.
	if _, err := r.UpsertGitHubBranchFacts(ctx, key, "fix/x", GitHubBranchFactsWrite{
		Rules: []byte(`[]`), Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("second branch upsert: %v", err)
	}
	branches, err := r.ListGitHubBranchFacts(ctx, key)
	if err != nil {
		t.Fatalf("list branch facts: %v", err)
	}
	if len(branches) != 2 || branches[0].Branch != "fix/x" || branches[1].Branch != "main" {
		t.Errorf("branch facts list = %+v, want fix/x and main", branches)
	}

	// A failed refresh records the class and keeps the rules and verification time:
	// an empty list must never appear because a refresh failed.
	failed, err := r.UpsertGitHubBranchFacts(ctx, key, "main", GitHubBranchFactsWrite{FailureClass: GitHubFactTimeout})
	if err != nil {
		t.Fatalf("failed branch upsert: %v", err)
	}
	if string(failed.Rules) == "[]" || !strings.Contains(string(failed.Rules), "required_approving_review_count") {
		t.Errorf("a failed refresh replaced the rules with %s", failed.Rules)
	}
	if failed.Completeness != GitHubCompletenessPartial {
		t.Errorf("a failed refresh changed completeness to %q", failed.Completeness)
	}
	if failed.FetchedAt == nil || !failed.FetchedAt.Equal(*at) {
		t.Errorf("a failed refresh moved fetched_at to %v, want %v", failed.FetchedAt, *at)
	}
	if failed.LastErrorClass != GitHubFactTimeout || failed.LastErrorAt == nil {
		t.Errorf("failure not recorded: %q at %v", failed.LastErrorClass, failed.LastErrorAt)
	}
	if !failed.Verified() || !failed.LastRefreshFailed() {
		t.Errorf("Verified()=%v LastRefreshFailed()=%v after a failed refresh, want both true",
			failed.Verified(), failed.LastRefreshFailed())
	}

	// A success clears the failure.
	later := ts(t, -time.Second)
	cleared, err := r.UpsertGitHubBranchFacts(ctx, key, "main", GitHubBranchFactsWrite{
		Rules: []byte(`[]`), Completeness: GitHubCompletenessComplete, VerifiedAt: later,
	})
	if err != nil {
		t.Fatalf("refresh after failure: %v", err)
	}
	if cleared.LastErrorClass != "" || cleared.LastErrorAt != nil {
		t.Errorf("a verified read left a failure behind: %q at %v", cleared.LastErrorClass, cleared.LastErrorAt)
	}
	if string(cleared.Rules) != "[]" || cleared.Completeness != GitHubCompletenessComplete {
		t.Errorf("refresh = %+v", cleared)
	}

	// Invalidating one branch leaves the other, and the repository row, alone.
	if ok, err := r.InvalidateGitHubBranchFacts(ctx, key, "main"); err != nil || !ok {
		t.Fatalf("invalidate branch = %v, %v; want true", ok, err)
	}
	if _, err := r.GetGitHubBranchFacts(ctx, key, "main"); !errors.Is(err, ErrNoGitHubBranchFacts) {
		t.Errorf("branch facts survived invalidation: %v", err)
	}
	if _, err := r.GetGitHubBranchFacts(ctx, key, "fix/x"); err != nil {
		t.Errorf("invalidating main reached fix/x: %v", err)
	}
	if _, err := r.GetGitHubRepoFacts(ctx, key); err != nil {
		t.Errorf("invalidating a branch reached its repository row: %v", err)
	}
	if ok, err := r.InvalidateGitHubBranchFacts(ctx, key, "main"); err != nil || ok {
		t.Errorf("second branch invalidation = %v, %v; want false, nil", ok, err)
	}
}

// PR facts round-trip (including the array columns), keep their last good facts when
// a refresh fails, and are dropped by a PR invalidation — the webhook path for a
// review or a dismissal.
func TestGitHubPullFactsRoundTrip(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "gprf")
	key := GitHubFactsKey{ProjectID: p, Slug: "owner/repo"}
	at := ts(t, -20*time.Second)

	if _, err := r.UpsertGitHubPullFacts(ctx, key, 7, GitHubPullFactsWrite{
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); !errors.Is(err, ErrNoGitHubRepoFacts) {
		t.Fatalf("PR facts without repository facts: err = %v, want ErrNoGitHubRepoFacts", err)
	}
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	}); err != nil {
		t.Fatalf("repository facts: %v", err)
	}

	got, err := r.UpsertGitHubPullFacts(ctx, key, 7, GitHubPullFactsWrite{
		BaseBranch: "main", State: "open", ReviewDecision: "review_required",
		RequestedReviewers: []string{"alice", "bob"}, RequestedTeams: []string{"core"},
		MergeableState: "blocked", Completeness: GitHubCompletenessComplete, VerifiedAt: at,
	})
	if err != nil {
		t.Fatalf("PR upsert: %v", err)
	}
	if got.Number != 7 || got.BaseBranch != "main" || got.State != "open" ||
		got.ReviewDecision != "review_required" || got.MergeableState != "blocked" {
		t.Errorf("PR facts = %+v", got)
	}
	if len(got.RequestedReviewers) != 2 || len(got.RequestedTeams) != 1 {
		t.Errorf("reviewer arrays = %v / %v", got.RequestedReviewers, got.RequestedTeams)
	}
	if !got.Verified() || got.LastRefreshFailed() {
		t.Errorf("Verified()=%v LastRefreshFailed()=%v", got.Verified(), got.LastRefreshFailed())
	}

	// A refresh replaces the PR-level facts — these change on every reviewer action.
	later := ts(t, -time.Second)
	updated, err := r.UpsertGitHubPullFacts(ctx, key, 7, GitHubPullFactsWrite{
		BaseBranch: "main", State: "open", ReviewDecision: "approved",
		MergeableState: "clean", Completeness: GitHubCompletenessComplete, VerifiedAt: later,
	})
	if err != nil {
		t.Fatalf("PR refresh: %v", err)
	}
	if updated.ReviewDecision != "approved" || updated.MergeableState != "clean" {
		t.Errorf("PR refresh = %+v", updated)
	}
	if len(updated.RequestedReviewers) != 0 {
		t.Errorf("PR refresh kept stale reviewers %v", updated.RequestedReviewers)
	}

	// A failed refresh keeps the good facts and records the failure.
	failed, err := r.UpsertGitHubPullFacts(ctx, key, 7, GitHubPullFactsWrite{FailureClass: GitHubFactForbidden})
	if err != nil {
		t.Fatalf("failed PR upsert: %v", err)
	}
	if failed.ReviewDecision != "approved" || failed.MergeableState != "clean" || failed.State != "open" {
		t.Errorf("a failed refresh changed the PR facts: %+v", failed)
	}
	if failed.FetchedAt == nil || !failed.FetchedAt.Equal(*later) {
		t.Errorf("a failed refresh moved fetched_at to %v, want %v", failed.FetchedAt, *later)
	}
	if failed.LastErrorClass != GitHubFactForbidden || failed.LastErrorAt == nil {
		t.Errorf("failure not recorded: %q at %v", failed.LastErrorClass, failed.LastErrorAt)
	}
	if !failed.Verified() || !failed.LastRefreshFailed() {
		t.Errorf("Verified()=%v LastRefreshFailed()=%v, want both true", failed.Verified(), failed.LastRefreshFailed())
	}

	// Invalidation drops just this PR's facts; the repository row stays.
	if ok, err := r.InvalidateGitHubPullFacts(ctx, key, 7); err != nil || !ok {
		t.Fatalf("invalidate PR = %v, %v; want true", ok, err)
	}
	if _, err := r.GetGitHubPullFacts(ctx, key, 7); !errors.Is(err, ErrNoGitHubPullFacts) {
		t.Errorf("PR facts survived invalidation: %v", err)
	}
	if _, err := r.GetGitHubRepoFacts(ctx, key); err != nil {
		t.Errorf("invalidating a PR reached its repository row: %v", err)
	}
	if ok, err := r.InvalidateGitHubPullFacts(ctx, key, 7); err != nil || ok {
		t.Errorf("second PR invalidation = %v, %v; want false, nil", ok, err)
	}
}

// The layer refuses a value the schema would refuse, before it reaches SQL, and
// names the SHAPE of the offending value — never its content. That matters because
// the common mistake is pasting a URL or a token into a repository field.
func TestGitHubFactsRejectsUnstorableValues(t *testing.T) {
	r := githubFactsTestRepo(t)
	ctx := context.Background()
	p := githubFactsTestProject(t, r, "grfv")
	key := GitHubFactsKey{ProjectID: p, Slug: "owner/repo"}

	for _, tc := range []struct {
		name string
		key  GitHubFactsKey
		want string
	}{
		{"no project", GitHubFactsKey{Slug: "owner/repo"}, "project id is required"},
		{"empty slug", GitHubFactsKey{ProjectID: p}, "owner/repo"},
		{"bare owner", GitHubFactsKey{ProjectID: p, Slug: "owner"}, "owner/repo"},
		{"a URL", GitHubFactsKey{ProjectID: p, Slug: "https://github.com/owner/repo"}, "owner/repo"},
		{"a path", GitHubFactsKey{ProjectID: p, Slug: "owner/repo/extra"}, "owner/repo"},
		{"a double slash", GitHubFactsKey{ProjectID: p, Slug: "owner//repo"}, "owner/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.GetGitHubRepoFacts(ctx, tc.key); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("get = %v, want an error mentioning %q", err, tc.want)
			}
			if _, err := r.UpsertGitHubRepoFacts(ctx, tc.key, GitHubRepoFactsWrite{}); err == nil {
				t.Error("upsert accepted the key")
			}
			if _, err := r.InvalidateGitHubRepoFacts(ctx, tc.key); err == nil {
				t.Error("invalidate accepted the key")
			}
		})
	}

	for _, tc := range []struct {
		name string
		w    GitHubRepoFactsWrite
		want string
	}{
		{"an invented completeness", GitHubRepoFactsWrite{Completeness: "probably", VerifiedAt: ts(t, 0)}, "completeness"},
		{"an invented source", GitHubRepoFactsWrite{DefaultBranch: "main", DefaultBranchSource: "guessed", VerifiedAt: ts(t, 0)}, "source"},
		{"a source without a branch", GitHubRepoFactsWrite{DefaultBranchSource: GitHubBranchSourceREST, VerifiedAt: ts(t, 0)}, "must name the branch"},
		{"an invented failure class", GitHubRepoFactsWrite{FailureClass: "GET failed: 401 (token ghp_x)"}, "failure class"},
		{"complete without a time", GitHubRepoFactsWrite{Completeness: GitHubCompletenessComplete}, "needs the time"},
		{"verified but unknown", GitHubRepoFactsWrite{VerifiedAt: ts(t, 0)}, "cannot be 'unknown'"},
		{"verified and failed at once", GitHubRepoFactsWrite{Completeness: GitHubCompletenessPartial, VerifiedAt: ts(t, 0), FailureClass: GitHubFactTimeout}, "both verified and a failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.UpsertGitHubRepoFacts(ctx, key, tc.w)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("upsert = %v, want an error mentioning %q", err, tc.want)
			}
			// Nothing was stored by a refused write.
			if _, gerr := r.GetGitHubRepoFacts(ctx, key); !errors.Is(gerr, ErrNoGitHubRepoFacts) {
				t.Errorf("a refused write stored a row: %v", gerr)
			}
		})
	}

	// Branch and PR facts validate their own vocabularies and need a branch name and
	// a positive PR number.
	if _, err := r.UpsertGitHubRepoFacts(ctx, key, GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: GitHubBranchSourceREST,
		Completeness: GitHubCompletenessComplete, VerifiedAt: ts(t, 0),
	}); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"", "   ", strings.Repeat("a", 256)} {
		if _, err := r.UpsertGitHubBranchFacts(ctx, key, branch, GitHubBranchFactsWrite{
			Completeness: GitHubCompletenessComplete, VerifiedAt: ts(t, 0),
		}); err == nil {
			t.Errorf("branch %q was accepted", branch)
		}
		if _, err := r.GetGitHubBranchFacts(ctx, key, branch); err == nil {
			t.Errorf("get branch %q was accepted", branch)
		}
	}
	if _, err := r.UpsertGitHubBranchFacts(ctx, key, "main", GitHubBranchFactsWrite{
		Rules: []byte(`{"type":"pull_request"}`), Completeness: GitHubCompletenessComplete, VerifiedAt: ts(t, 0),
	}); err == nil || !strings.Contains(err.Error(), "array") {
		t.Errorf("rules as an object = %v, want an error saying they must be an array", err)
	}
	for _, number := range []int{0, -1} {
		if _, err := r.UpsertGitHubPullFacts(ctx, key, number, GitHubPullFactsWrite{
			Completeness: GitHubCompletenessComplete, VerifiedAt: ts(t, 0),
		}); err == nil {
			t.Errorf("PR number %d was accepted", number)
		}
		if _, err := r.GetGitHubPullFacts(ctx, key, number); err == nil {
			t.Errorf("get PR number %d was accepted", number)
		}
		if _, err := r.InvalidateGitHubPullFacts(ctx, key, number); err == nil {
			t.Errorf("invalidate PR number %d was accepted", number)
		}
	}
	for _, tc := range []struct {
		name string
		w    GitHubPullFactsWrite
	}{
		{"an invented state", GitHubPullFactsWrite{State: "maybe"}},
		{"an invented review decision", GitHubPullFactsWrite{ReviewDecision: "looks_fine"}},
		{"an invented mergeable state", GitHubPullFactsWrite{MergeableState: "probably"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.w.Completeness, tc.w.VerifiedAt = GitHubCompletenessComplete, ts(t, 0)
			if _, err := r.UpsertGitHubPullFacts(ctx, key, 1, tc.w); err == nil {
				t.Error("upsert accepted the value")
			}
		})
	}
	// Every class githubfacts.Kind can report is storable verbatim, so the cache never
	// needs a mapping table that drifts from the client (#3151).
	for _, class := range []string{
		GitHubFactNotFound, GitHubFactForbidden, GitHubFactRateLimited, GitHubFactTimeout,
		GitHubFactCanceled, GitHubFactNetwork, GitHubFactServer, GitHubFactMoved,
		GitHubFactInvalid, GitHubFactNoCredential, GitHubFactNoTarget, GitHubFactUnsupportedHost,
	} {
		if !githubFactFailureClasses[class] {
			t.Errorf("failure class %q is not storable", class)
		}
	}
}
