package service

// Tests for the repository-facts refresh coordinator (AMP #3156), against fakes:
// an in-memory cache, an in-memory credential source and a scripted reader. No
// Postgres and no socket, because what this file is responsible for is the POLICY
// — the TTLs, the negative cache, the singleflight and the hot read's promise to
// make no HTTP call — and each of those is a decision about time and concurrency,
// which is what the fakes make controllable.
//
// Every guard here was mutation-tested against its own bug:
//
//   - drop the TTL check            → TestGitHubFactsRepoTTLAndPullTTL
//   - drop the negative cache       → TestGitHubFactsNegativeCache
//   - drop the singleflight         → TestGitHubFactsSingleflight
//   - let Lookup call the reader    → TestGitHubFactsHotReadMakesNoHTTPCall
//   - let a failure look verified   → TestGitHubFactsFailuresAreExplicitUnknown
//   - let a stale default branch classify → TestGitHubFactsStaleDefaultBranchCannotSayInternal
//   - let a hint be the PR's own base → TestGitHubFactsTargetIsDefault
//   - collapse the whole refresh into the pull read → TestGitHubFactsSingleflightScoped

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/githubfacts"
	"github.com/simstech/amp-api/internal/repository"
)

// The ports are real: the production types satisfy them. This is a compile-time
// assertion, so a change to any of the three layers that breaks the coordinator
// fails the build here rather than at a nil interface in a hot read.
var (
	_ GitHubFactStore         = (*repository.Repo)(nil)
	_ GitHubFactReader        = (*githubfacts.Client)(nil)
	_ GitHubFactCredentials   = RepoGitHubFactCredentials{}
	_ GitHubFactReaderFactory = GitHubClientReaderFactory(githubfacts.Options{})
)

// ---- the fakes ----

// memGitHubFactStore is the cache, in memory. It copies on write and on read, so a
// test cannot accidentally share a row with the coordinator and so the views'
// "nil means never read" is real.
type memGitHubFactStore struct {
	mu     sync.Mutex
	repos  map[string]repository.GitHubRepoFacts
	branch map[string]repository.GitHubBranchFacts
	pulls  map[string]repository.GitHubPullFacts

	repoWrites   []repository.GitHubRepoFactsWrite
	branchWrites []repository.GitHubBranchFactsWrite
	pullWrites   []repository.GitHubPullFactsWrite
}

func newMemGitHubFactStore() *memGitHubFactStore {
	return &memGitHubFactStore{
		repos:  make(map[string]repository.GitHubRepoFacts),
		branch: make(map[string]repository.GitHubBranchFacts),
		pulls:  make(map[string]repository.GitHubPullFacts),
	}
}

func storeKey(k repository.GitHubFactsKey) string {
	host := k.Host
	if host == "" {
		host = repository.GitHubFactsHost
	}
	return fmt.Sprintf("%d|%s|%s", k.ProjectID, host, strings.ToLower(k.Slug))
}

func (s *memGitHubFactStore) GetGitHubRepoFacts(_ context.Context, k repository.GitHubFactsKey) (*repository.GitHubRepoFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.repos[storeKey(k)]
	if !ok {
		return nil, repository.ErrNoGitHubRepoFacts
	}
	out := f
	return &out, nil
}

func (s *memGitHubFactStore) UpsertGitHubRepoFacts(_ context.Context, k repository.GitHubFactsKey, w repository.GitHubRepoFactsWrite) (*repository.GitHubRepoFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoWrites = append(s.repoWrites, w)
	key := storeKey(k)
	cur, ok := s.repos[key]
	if !ok {
		cur = repository.GitHubRepoFacts{ProjectID: k.ProjectID, Host: k.Host, Slug: k.Slug}
	}
	// The two rules that matter for the policy, mirrored from 044's upsert: a
	// failed read keeps every good fact and records the class; a verified read
	// replaces them, except that a manual override survives a detection.
	if w.VerifiedAt != nil {
		cur.Completeness = w.Completeness
		cur.FetchedAt = w.VerifiedAt
		cur.CredentialID = w.CredentialID
		if cur.DefaultBranchSource != repository.GitHubBranchSourceManual {
			cur.DefaultBranch = w.DefaultBranch
			cur.DefaultBranchSource = w.DefaultBranchSource
		}
		cur.LastErrorClass, cur.LastErrorAt = "", nil
	} else if w.DefaultBranchSource == repository.GitHubBranchSourceManual {
		cur.DefaultBranch = w.DefaultBranch
		cur.DefaultBranchSource = w.DefaultBranchSource
	} else {
		cur.LastErrorClass = w.FailureClass
		at := time.Now().UTC()
		cur.LastErrorAt = &at
	}
	s.repos[key] = cur
	out := cur
	return &out, nil
}

func branchKey(k repository.GitHubFactsKey, branch string) string {
	return storeKey(k) + "|" + branch
}

func (s *memGitHubFactStore) GetGitHubBranchFacts(_ context.Context, k repository.GitHubFactsKey, branch string) (*repository.GitHubBranchFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.branch[branchKey(k, branch)]
	if !ok {
		return nil, repository.ErrNoGitHubBranchFacts
	}
	out := f
	return &out, nil
}

func (s *memGitHubFactStore) UpsertGitHubBranchFacts(_ context.Context, k repository.GitHubFactsKey, branch string, w repository.GitHubBranchFactsWrite) (*repository.GitHubBranchFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branchWrites = append(s.branchWrites, w)
	key := branchKey(k, branch)
	cur, ok := s.branch[key]
	if !ok {
		cur = repository.GitHubBranchFacts{Branch: branch}
	}
	if w.VerifiedAt != nil {
		cur.Rules = w.Rules
		cur.Completeness = w.Completeness
		cur.FetchedAt = w.VerifiedAt
		cur.LastErrorClass, cur.LastErrorAt = "", nil
	} else {
		cur.LastErrorClass = w.FailureClass
		at := time.Now().UTC()
		cur.LastErrorAt = &at
	}
	s.branch[key] = cur
	out := cur
	return &out, nil
}

func pullKey(k repository.GitHubFactsKey, n int) string {
	return fmt.Sprintf("%s|%d", storeKey(k), n)
}

func (s *memGitHubFactStore) GetGitHubPullFacts(_ context.Context, k repository.GitHubFactsKey, n int) (*repository.GitHubPullFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.pulls[pullKey(k, n)]
	if !ok {
		return nil, repository.ErrNoGitHubPullFacts
	}
	out := f
	return &out, nil
}

func (s *memGitHubFactStore) UpsertGitHubPullFacts(_ context.Context, k repository.GitHubFactsKey, n int, w repository.GitHubPullFactsWrite) (*repository.GitHubPullFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pullWrites = append(s.pullWrites, w)
	key := pullKey(k, n)
	cur, ok := s.pulls[key]
	if !ok {
		cur = repository.GitHubPullFacts{Number: n}
	}
	if w.VerifiedAt != nil {
		cur.BaseBranch, cur.State = w.BaseBranch, w.State
		cur.RequestedReviewers, cur.RequestedTeams = w.RequestedReviewers, w.RequestedTeams
		cur.Completeness = w.Completeness
		cur.FetchedAt = w.VerifiedAt
		cur.LastErrorClass, cur.LastErrorAt = "", nil
	} else {
		cur.LastErrorClass = w.FailureClass
		at := time.Now().UTC()
		cur.LastErrorAt = &at
	}
	s.pulls[key] = cur
	out := cur
	return &out, nil
}

// InvalidateGitHubPullFacts mirrors 044's scoped delete: one pull request's row,
// matched case-insensitively on the slug and never across projects.
func (s *memGitHubFactStore) InvalidateGitHubPullFacts(_ context.Context, k repository.GitHubFactsKey, n int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := pullKey(k, n)
	if _, ok := s.pulls[key]; !ok {
		return false, nil
	}
	delete(s.pulls, key)
	return true, nil
}

// seedRepo puts facts in the cache as if a refresh had stored them, so a test can
// set up a hot read without going through one.
func (s *memGitHubFactStore) seedRepo(projectID int, slug string, w repository.GitHubRepoFactsWrite) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey(repository.GitHubFactsKey{ProjectID: projectID, Slug: slug})
	cur := repository.GitHubRepoFacts{ProjectID: projectID, Host: repository.GitHubFactsHost, Slug: slug}
	if w.VerifiedAt != nil {
		cur.Completeness, cur.FetchedAt = w.Completeness, w.VerifiedAt
		cur.DefaultBranch, cur.DefaultBranchSource = w.DefaultBranch, w.DefaultBranchSource
		cur.CredentialID = w.CredentialID
	} else {
		cur.LastErrorClass = w.FailureClass
		at := time.Now().UTC()
		cur.LastErrorAt = &at
	}
	s.repos[key] = cur
}

func (s *memGitHubFactStore) seedBranch(projectID int, slug, branch string, w repository.GitHubBranchFactsWrite) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := branchKey(repository.GitHubFactsKey{ProjectID: projectID, Slug: slug}, branch)
	cur := repository.GitHubBranchFacts{Branch: branch}
	if w.VerifiedAt != nil {
		cur.Rules, cur.Completeness, cur.FetchedAt = w.Rules, w.Completeness, w.VerifiedAt
	} else {
		cur.LastErrorClass = w.FailureClass
		at := time.Now().UTC()
		cur.LastErrorAt = &at
	}
	s.branch[key] = cur
}

func (s *memGitHubFactStore) seedPull(projectID int, slug string, n int, w repository.GitHubPullFactsWrite) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := pullKey(repository.GitHubFactsKey{ProjectID: projectID, Slug: slug}, n)
	cur := repository.GitHubPullFacts{Number: n}
	if w.VerifiedAt != nil {
		cur.BaseBranch, cur.State = w.BaseBranch, w.State
		cur.RequestedReviewers, cur.RequestedTeams = w.RequestedReviewers, w.RequestedTeams
		cur.Completeness, cur.FetchedAt = w.Completeness, w.VerifiedAt
	} else {
		cur.LastErrorClass = w.FailureClass
		at := time.Now().UTC()
		cur.LastErrorAt = &at
	}
	s.pulls[key] = cur
}

// memGitHubFactCredentials is the credential source: one token per project, or
// none, and the id that names it.
type memGitHubFactCredentials struct {
	tokens  map[int]string
	failID  error
	failSec error
}

func (c *memGitHubFactCredentials) CredentialID(_ context.Context, projectID int, _ string) (int, bool, error) {
	if c.failID != nil {
		return 0, false, c.failID
	}
	if _, ok := c.tokens[projectID]; !ok {
		return 0, false, nil
	}
	return projectID * 100, true, nil
}

func (c *memGitHubFactCredentials) Secret(_ context.Context, projectID int, _ string) (string, error) {
	if c.failSec != nil {
		return "", c.failSec
	}
	return c.tokens[projectID], nil
}

// scriptedReader is the reader: it records every call and answers from a function,
// so a test decides per request whether the read succeeds, fails, or blocks.
type scriptedReader struct {
	mu      sync.Mutex
	calls   []githubfacts.Request
	answer  func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error)
	tokens  []string
	tokenOf func(ctx context.Context) (string, error)
}

func (r *scriptedReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *scriptedReader) Facts(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	answer := r.answer
	r.mu.Unlock()
	if answer != nil {
		return answer(ctx, req)
	}
	return okFacts(req), nil
}

// okFacts is a complete, successful read: default branch, rules, protection and
// (when asked for) the pull request all answered.
func okFacts(req githubfacts.Request) *githubfacts.Facts {
	target := req.Target
	f := &githubfacts.Facts{
		Slug:          req.Slug,
		PR:            req.PR,
		ReadAt:        time.Now().UTC(),
		DefaultBranch: "main",
		Target:        target,
		TargetSource:  githubfacts.SourceHint,
		Completeness:  githubfacts.Complete,
		Reads:         []githubfacts.Read{{Op: githubfacts.OpRepo, OK: true, Status: 200}},
		Rules:         githubfacts.RulesFacts{Known: true, Rules: []githubfacts.Rule{}},
	}
	protected := true
	f.Rules.Protection = githubfacts.ClassicProtection{Known: true, Protected: &protected}
	f.Reads = append(f.Reads,
		githubfacts.Read{Op: githubfacts.OpRules, OK: true, Status: 200},
		githubfacts.Read{Op: githubfacts.OpProtection, OK: true, Status: 200})
	if req.PR > 0 {
		f.Pull = githubfacts.PullFacts{Known: true, Base: target, RequestedReviewers: []string{"octocat"}}
		f.Target, f.TargetSource = target, githubfacts.SourcePull
		f.Reads = append(f.Reads, githubfacts.Read{Op: githubfacts.OpPull, OK: true, Status: 200})
	}
	return f
}

// failingFacts is a read where nothing answered: the private-repository case.
func failingFacts(kind githubfacts.Kind) *githubfacts.Facts {
	f := &githubfacts.Facts{Completeness: githubfacts.Unknown, ReadAt: time.Now().UTC()}
	for _, op := range []githubfacts.Op{githubfacts.OpRepo, githubfacts.OpRules, githubfacts.OpProtection} {
		f.Reads = append(f.Reads, githubfacts.Read{Op: op, Err: &githubfacts.Error{Op: op, Kind: kind, Status: 404}})
	}
	return f
}

// coordinator builds a coordinator over the fakes, with a controllable clock.
func coordinator(t *testing.T, store GitHubFactStore, creds GitHubFactCredentials, reader GitHubFactReader, now func() time.Time) *GitHubFactsCoordinator {
	t.Helper()
	c, err := NewGitHubFactsCoordinator(GitHubFactsConfig{
		Store:                store,
		Credentials:          creds,
		Readers:              func(githubfacts.TokenSource) GitHubFactReader { return reader },
		Now:                  now,
		RepoTTL:              15 * time.Minute,
		PullTTL:              60 * time.Second,
		NegativeTTL:          60 * time.Second,
		RefreshTimeout:       5 * time.Second,
		MaxConcurrentRefresh: 8,
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	return c
}

// clock is a settable clock the coordinator reads.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// waitIdle blocks until the coordinator has no refresh in flight, so a test can
// assert on what a BACKGROUND refresh stored. Bounded, so a hung refresh fails the
// test instead of stalling it.
func waitIdle(t *testing.T, c *GitHubFactsCoordinator) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitForIdle(ctx); err != nil {
		t.Fatalf("refreshes did not finish: %v", err)
	}
}

func testRequest() GitHubFactsRequest {
	return GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", PR: 12, Target: "feat/x"}
}

// ---- the request ----

// TestGitHubFactsRequestIsRefusedWhenItCannotNameARow: a hot read must not build a
// cache key it cannot use, and the error must never echo the value (a pasted URL
// can carry a token).
func TestGitHubFactsRequestIsRefusedWhenItCannotNameARow(t *testing.T) {
	reader := &scriptedReader{}
	c := coordinator(t, newMemGitHubFactStore(), &memGitHubFactCredentials{}, reader, newClock().now)

	cases := []struct {
		name string
		req  GitHubFactsRequest
	}{
		{"no project", GitHubFactsRequest{Slug: "acme/rocket"}},
		{"a URL, not a slug", GitHubFactsRequest{ProjectID: 1, Slug: "https://github.com/acme/rocket"}},
		{"a slug with a path", GitHubFactsRequest{ProjectID: 1, Slug: "acme/rocket/tree/main"}},
		{"no repository", GitHubFactsRequest{ProjectID: 1}},
		{"a negative PR number", GitHubFactsRequest{ProjectID: 1, Slug: "acme/rocket", PR: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Lookup(context.Background(), tc.req)
			if err == nil {
				t.Fatal("the request was accepted")
			}
			if strings.Contains(err.Error(), tc.req.Slug) && tc.req.Slug != "" {
				t.Errorf("the error echoed the slug: %v", err)
			}
		})
	}
	if n := reader.callCount(); n != 0 {
		t.Errorf("an unusable request made %d read(s)", n)
	}
}

// ---- TTLs ----

// TestGitHubFactsRepoTTLAndPullTTL is the TTL contract, in one table: a fresh read
// is evidence, an old one is not, and the repository/rules window (15m) is
// deliberately longer than the pull request's (60s) because rule content moves per
// PR while a default branch does not.
func TestGitHubFactsRepoTTLAndPullTTL(t *testing.T) {
	type want struct {
		repoTrusted, branchTrusted, pullTrusted bool
		refreshStarted                          bool
	}
	tests := []struct {
		name string
		age  time.Duration // how long ago the facts were read
		want want
	}{
		{
			name: "just read: nothing to refresh",
			age:  0,
			want: want{repoTrusted: true, branchTrusted: true, pullTrusted: true, refreshStarted: false},
		},
		{
			name: "#3146 the pull facts age out 60s after the read, the rules do not",
			age:  61 * time.Second,
			want: want{repoTrusted: true, branchTrusted: true, pullTrusted: false, refreshStarted: true},
		},
		{
			name: "14m59s: still inside the repository window",
			age:  14*time.Minute + 59*time.Second,
			want: want{repoTrusted: true, branchTrusted: true, pullTrusted: false, refreshStarted: true},
		},
		{
			name: "15m1s: the rules are stale too",
			age:  15*time.Minute + time.Second,
			want: want{repoTrusted: false, branchTrusted: false, pullTrusted: false, refreshStarted: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			readAt := clk.now().Add(-tc.age)
			store := newMemGitHubFactStore()
			slug := "acme/rocket"
			store.seedRepo(7, slug, repository.GitHubRepoFactsWrite{
				DefaultBranch: "main", DefaultBranchSource: repository.GitHubBranchSourceREST,
				Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
			})
			store.seedBranch(7, slug, "feat/x", repository.GitHubBranchFactsWrite{
				Rules: []byte(`[]`), Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
			})
			store.seedPull(7, slug, 12, repository.GitHubPullFactsWrite{
				BaseBranch: "feat/x", State: "unknown", Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
			})
			// The reader blocks so the test sees whether a refresh was STARTED
			// without racing the assertion against one that finished.
			gate := make(chan struct{})
			reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
				<-gate
				return okFacts(req), nil
			}}
			c := coordinator(t, store, &memGitHubFactCredentials{}, reader, clk.now)
			view, err := c.Lookup(context.Background(), testRequest())
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if got := view.Repo.Trusted; got != tc.want.repoTrusted {
				t.Errorf("Repo.Trusted = %v, want %v (%s)", got, tc.want.repoTrusted, view.Repo.Unknown)
			}
			if got := view.Branch.Trusted; got != tc.want.branchTrusted {
				t.Errorf("Branch.Trusted = %v, want %v (%s)", got, tc.want.branchTrusted, view.Branch.Unknown)
			}
			if got := view.Pull.Trusted; got != tc.want.pullTrusted {
				t.Errorf("Pull.Trusted = %v, want %v (%s)", got, tc.want.pullTrusted, view.Pull.Unknown)
			}
			if got := view.Refresh.Started; got != tc.want.refreshStarted {
				t.Errorf("Refresh.Started = %v, want %v (%+v)", got, tc.want.refreshStarted, view.Refresh)
			}
			if !tc.want.refreshStarted && view.Refresh.Skipped != GitHubFactsSkippedFresh {
				t.Errorf("Refresh.Skipped = %q, want %q", view.Refresh.Skipped, GitHubFactsSkippedFresh)
			}
			// A stale fact must SAY it is stale rather than just being false.
			if !tc.want.branchTrusted && view.Branch.Unknown == "" {
				t.Error("a stale rules read has no Unknown reason")
			}
			close(gate)
			waitIdle(t, c)
		})
	}
}

// TestGitHubFactsFreshnessIsNotEvidenceOfNeverReading: a fact read in the future
// cannot be judged, and must not be treated as fresh.
func TestGitHubFactsFreshnessIsNotEvidenceOfNeverReading(t *testing.T) {
	clk := newClock()
	future := clk.now().Add(time.Hour)
	store := newMemGitHubFactStore()
	store.seedRepo(7, "acme/rocket", repository.GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: repository.GitHubBranchSourceREST,
		Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &future,
	})
	c := coordinator(t, store, &memGitHubFactCredentials{}, &scriptedReader{}, clk.now)
	view, err := c.Lookup(context.Background(), GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if view.Repo.Trusted {
		t.Error("a future-stamped read was treated as fresh")
	}
	if isd := view.TargetIsDefault(); isd != nil {
		t.Errorf("TargetIsDefault = %v, want nil (undecidable)", *isd)
	}
	waitIdle(t, c)
}

// ---- the negative cache ----

// TestGitHubFactsNegativeCache: after a failed refresh, nothing retries that scope
// for 60s, and the read says so instead of hammering GitHub. After the window a
// new attempt happens — the cache delays attempts, it does not give up.
func TestGitHubFactsNegativeCache(t *testing.T) {
	clk := newClock()
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		return failingFacts(githubfacts.KindNotFound), nil
	}}
	c := coordinator(t, newMemGitHubFactStore(), &memGitHubFactCredentials{}, reader, clk.now)

	first, err := c.Lookup(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if !first.Refresh.Started {
		t.Fatalf("the first lookup started no refresh: %+v", first.Refresh)
	}
	waitIdle(t, c)
	if n := reader.callCount(); n != 1 {
		t.Fatalf("the first refresh made %d reads, want 1", n)
	}

	// Inside the window: a hot read must not start another refresh, and must say
	// why. This is the whole point — a repository no token can read would
	// otherwise cost a round of GitHub calls per My work load.
	second, err := c.Lookup(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if second.Refresh.Started || second.Refresh.Joined {
		t.Errorf("a negative-cached scope started a refresh: %+v", second.Refresh)
	}
	if second.Refresh.Failure != repository.GitHubFactNotFound {
		t.Errorf("Refresh.Failure = %q, want %q", second.Refresh.Failure, repository.GitHubFactNotFound)
	}
	if !strings.Contains(second.Refresh.Skipped, repository.GitHubFactNotFound) {
		t.Errorf("Refresh.Skipped = %q, want it to name the failure class", second.Refresh.Skipped)
	}
	// And the failure stays an explicit unknown, not a silent "nothing applies".
	if second.Repo.Trusted || second.Repo.Unknown == "" {
		t.Errorf("a failed read is not explicit-unknown: trusted=%v unknown=%q", second.Repo.Trusted, second.Repo.Unknown)
	}
	if n := reader.callCount(); n != 1 {
		t.Errorf("a negative-cached scope made %d reads in total, want 1", n)
	}

	// Past the window: one more attempt, which is what makes the cache safe.
	clk.advance(61 * time.Second)
	third, err := c.Lookup(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("third lookup: %v", err)
	}
	if !third.Refresh.Started {
		t.Errorf("the window expired but nothing retried: %+v", third.Refresh)
	}
	waitIdle(t, c)
	if n := reader.callCount(); n != 2 {
		t.Errorf("after the window there were %d reads in total, want 2", n)
	}
}

// TestGitHubFactsNegativeCacheIsScopedPerProject: one project's failure must not
// silence another's — the cross-project rule the SQL enforces, kept here too.
func TestGitHubFactsNegativeCacheIsScopedPerProject(t *testing.T) {
	clk := newClock()
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		if strings.HasPrefix(req.Slug, "acme/") {
			return failingFacts(githubfacts.KindForbidden), nil
		}
		return okFacts(req), nil
	}}
	c := coordinator(t, newMemGitHubFactStore(), &memGitHubFactCredentials{}, reader, clk.now)

	bad := GitHubFactsRequest{ProjectID: 1, Slug: "acme/rocket"}
	if _, err := c.Lookup(context.Background(), bad); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	waitIdle(t, c)

	good := GitHubFactsRequest{ProjectID: 2, Slug: "beta/rocket"}
	view, err := c.Lookup(context.Background(), good)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !view.Refresh.Started {
		t.Errorf("another project's success was silenced by the failure: %+v", view.Refresh)
	}
	waitIdle(t, c)
}

// ---- the singleflight ----

// TestGitHubFactsSingleflight: concurrent readers of the SAME facts share one
// refresh. Without it, a page of merge items for one repository opens one GitHub
// call per item — the thundering herd the research warns about (§1d).
func TestGitHubFactsSingleflight(t *testing.T) {
	clk := newClock()
	started := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		once.Do(func() { close(started) })
		<-gate
		return okFacts(req), nil
	}}
	c := coordinator(t, newMemGitHubFactStore(), &memGitHubFactCredentials{}, reader, clk.now)

	const readers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		start   int
		join    int
		skipped int
	)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, err := c.Lookup(context.Background(), testRequest())
			if err != nil {
				t.Errorf("lookup: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case view.Refresh.Started:
				start++
			case view.Refresh.Joined:
				join++
			default:
				skipped++
			}
		}()
	}
	<-started // one refresh is running; let the rest arrive
	wg.Wait()
	close(gate)
	waitIdle(t, c)

	if n := reader.callCount(); n != 1 {
		t.Errorf("%d concurrent lookups made %d reads, want exactly 1", readers, n)
	}
	if start != 1 {
		t.Errorf("%d lookups reported Started, want exactly 1", start)
	}
	// Every other reader either joined the running refresh or found the facts
	// already stored by it — never a second refresh.
	if join+skipped != readers-1 {
		t.Errorf("Start=%d Join=%d Skipped=%d, want the remaining %d split between Join and Skipped", start, join, skipped, readers-1)
	}

	// The refresh landed: the same lookup now needs no refresh at all.
	view, err := c.Lookup(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("lookup after the refresh: %v", err)
	}
	if !view.Repo.Trusted || !view.Branch.Trusted || !view.Pull.Trusted {
		t.Errorf("the shared refresh did not land: repo=%v branch=%v pull=%v", view.Repo.Trusted, view.Branch.Trusted, view.Pull.Trusted)
	}
	if view.Refresh.Started {
		t.Errorf("freshly refreshed facts still started a refresh: %+v", view.Refresh)
	}
}

// TestGitHubFactsSingleflightScoped: different projects, targets or pull requests
// are DIFFERENT refreshes. A refresh reads one repository under one credential and
// one target's rules, so collapsing two would answer one of them with the other's
// facts — a cross-project leak, or a branch classified by a sibling's rules.
func TestGitHubFactsSingleflightScoped(t *testing.T) {
	clk := newClock()
	gate := make(chan struct{})
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		<-gate
		return okFacts(req), nil
	}}
	c := coordinator(t, newMemGitHubFactStore(), &memGitHubFactCredentials{}, reader, clk.now)

	reqs := []GitHubFactsRequest{
		{ProjectID: 1, Slug: "acme/rocket", Target: "feat/x"},
		{ProjectID: 2, Slug: "acme/rocket", Target: "feat/x"},         // another project
		{ProjectID: 1, Slug: "acme/rocket", Target: "feat/y"},         // another target
		{ProjectID: 1, Slug: "acme/rocket", Target: "feat/x", PR: 12}, // another PR
		{ProjectID: 1, Slug: "acme/other", Target: "feat/x"},          // another repository
	}
	var seen []GitHubFactsRequest
	for _, req := range reqs {
		n := len(seen) + 1
		seen = append(seen, req)
		go func() {
			if _, err := c.Lookup(context.Background(), req); err != nil {
				t.Errorf("lookup %+v: %v", req, err)
			}
		}()
		// Wait for THIS one to be in flight before spawning the next, so the
		// assertion is about scope and not about scheduling: if two requests
		// collapsed, the count would stop rising here rather than later.
		waitInFlight(t, c, n)
	}
	close(gate)
	waitIdle(t, c)
	if n := reader.callCount(); n != len(reqs) {
		t.Errorf("%d differently-scoped lookups made %d reads, want %d", len(reqs), n, len(reqs))
	}
}

// waitInFlight waits (bounded) until the coordinator has at least n refreshes
// running.
func waitInFlight(t *testing.T, c *GitHubFactsCoordinator, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.InFlight() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d refresh(es) in flight (have %d)", n, c.InFlight())
}

// ---- the hot read ----

// TestGitHubFactsHotReadMakesNoHTTPCall is the acceptance criterion the whole
// design turns on: a My work read answers from the cache and starts a background
// refresh, and does not wait for it. The reader here BLOCKS, so if Lookup waited the
// test would hang rather than fail — so Lookup is also required to return while the
// refresh is still running.
func TestGitHubFactsHotReadMakesNoHTTPCall(t *testing.T) {
	clk := newClock()
	gate := make(chan struct{})
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		<-gate
		return okFacts(req), nil
	}}
	c := coordinator(t, newMemGitHubFactStore(), &memGitHubFactCredentials{}, reader, clk.now)

	done := make(chan GitHubFactsView, 1)
	go func() {
		view, err := c.Lookup(context.Background(), testRequest())
		if err != nil {
			t.Errorf("lookup: %v", err)
		}
		done <- view
	}()

	var view GitHubFactsView
	select {
	case view = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Lookup waited for the refresh: a hot read must not block on HTTP")
	}
	// The read returned with nothing known, and said so per fact.
	if view.Repo.Trusted {
		t.Error("a never-read repository came back trusted")
	}
	if view.Repo.Unknown == "" || view.Branch.Unknown == "" || view.Pull.Unknown == "" {
		t.Errorf("the miss was not explicit: repo=%q branch=%q pull=%q", view.Repo.Unknown, view.Branch.Unknown, view.Pull.Unknown)
	}
	if !view.AnyUnknown() {
		t.Error("AnyUnknown = false for a fully unknown view")
	}
	// The refresh is still running: the read did not wait for it.
	waitInFlight(t, c, 1)
	close(gate)
	waitIdle(t, c)

	// And it did land, so the NEXT read is served from the cache.
	after, err := c.Lookup(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("lookup after the refresh: %v", err)
	}
	if !after.Repo.Trusted || !after.Branch.Trusted || !after.Pull.Trusted {
		t.Errorf("the background refresh did not populate the cache: %+v", after)
	}
}

// TestGitHubFactsRefreshIsBounded: the background refresh is bounded in number, so
// a page of merge items cannot open one GitHub call per item against the secondary
// rate limit.
func TestGitHubFactsRefreshIsBounded(t *testing.T) {
	clk := newClock()
	gate := make(chan struct{})
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		<-gate
		return okFacts(req), nil
	}}
	c, err := NewGitHubFactsCoordinator(GitHubFactsConfig{
		Store:                newMemGitHubFactStore(),
		Credentials:          &memGitHubFactCredentials{},
		Readers:              func(githubfacts.TokenSource) GitHubFactReader { return reader },
		Now:                  clk.now,
		MaxConcurrentRefresh: 2,
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	var deferred int
	for i := 0; i < 5; i++ {
		slug := fmt.Sprintf("acme/rocket%d", i)
		view, err := c.Lookup(context.Background(), GitHubFactsRequest{ProjectID: 1, Slug: slug, Target: "feat/x"})
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if view.Refresh.Deferred {
			deferred++
		}
		waitInFlight(t, c, min(i+1, 2))
	}
	if deferred != 3 {
		t.Errorf("%d of 5 refreshes were deferred, want 3 (the bound is 2)", deferred)
	}
	close(gate)
	waitIdle(t, c)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- failures are explicit unknowns ----

// TestGitHubFactsFailuresAreExplicitUnknown: a failed read never looks like a
// success. Nothing becomes Verified, the failure class is recorded, and every fact
// it touched carries a reason. This is the "no optimistic completeness" rule.
func TestGitHubFactsFailuresAreExplicitUnknown(t *testing.T) {
	tests := []struct {
		name string
		kind githubfacts.Kind
	}{
		{"a private repository the token cannot see", githubfacts.KindNotFound},
		{"a token without the scope", githubfacts.KindForbidden},
		{"rate-limited", githubfacts.KindRateLimited},
		{"a timeout", githubfacts.KindTimeout},
		{"no credential the store could produce", githubfacts.KindNoCredential},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			store := newMemGitHubFactStore()
			reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
				return failingFacts(tc.kind), nil
			}}
			c := coordinator(t, store, &memGitHubFactCredentials{}, reader, clk.now)

			// Refresh synchronously, so the assertion is about what was stored.
			if _, _, err := c.Refresh(context.Background(), testRequest()); err != nil {
				t.Fatalf("refresh: %v", err)
			}

			// Nothing may claim verification.
			if len(store.repoWrites) == 0 {
				t.Fatal("no repository row was written")
			}
			for _, w := range store.repoWrites {
				if w.VerifiedAt != nil {
					t.Errorf("a failed read wrote verified facts: %+v", w)
				}
				if w.FailureClass != string(tc.kind) {
					t.Errorf("repository failure class = %q, want %q", w.FailureClass, tc.kind)
				}
				if w.DefaultBranch != "" {
					t.Errorf("a failed read wrote default branch %q", w.DefaultBranch)
				}
			}
			for _, w := range store.branchWrites {
				if w.VerifiedAt != nil {
					t.Errorf("a failed rules read wrote a verification time: %+v", w)
				}
				if w.FailureClass != string(tc.kind) {
					t.Errorf("branch failure class = %q, want %q", w.FailureClass, tc.kind)
				}
			}

			// And the hot read that follows says unknown, per fact.
			view, err := c.Lookup(context.Background(), testRequest())
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if view.Repo.Trusted || view.Branch.Trusted {
				t.Errorf("failed facts came back trusted: repo=%v branch=%v", view.Repo.Trusted, view.Branch.Trusted)
			}
			if !strings.Contains(view.Repo.Unknown, string(tc.kind)) {
				t.Errorf("Repo.Unknown = %q, want it to name %q", view.Repo.Unknown, tc.kind)
			}
			if view.Repo.Facts == nil || !view.Repo.Facts.LastRefreshFailed() {
				t.Error("the cached row does not show the failed refresh")
			}
		})
	}
}

// TestGitHubFactsPartialRulesIsNotProofOfNoRule: the case the merge policy turns
// on. Rules read plus classic protection unreadable is PARTIAL — a positive signal
// for a rule, never proof that none applies — and only COMPLETE may be read as "no
// rule". A reader that reports this wrong would quiet real merge cards.
func TestGitHubFactsPartialRulesIsNotProofOfNoRule(t *testing.T) {
	tests := []struct {
		name         string
		protectionOK bool
		wantCompl    repository.GitHubCompleteness
		wantComplete bool
		wantPartial  bool
	}{
		{name: "rules and protection both answered", protectionOK: true, wantCompl: repository.GitHubCompletenessComplete, wantComplete: true},
		{name: "rules read, protection not readable (a member token)", protectionOK: false, wantCompl: repository.GitHubCompletenessPartial, wantPartial: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			store := newMemGitHubFactStore()
			reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
				f := okFacts(req)
				if !tc.protectionOK {
					// A member token: rules answer, protection 404s.
					f.Reads = []githubfacts.Read{
						{Op: githubfacts.OpRepo, OK: true, Status: 200},
						{Op: githubfacts.OpRules, OK: true, Status: 200},
						{Op: githubfacts.OpProtection, Err: &githubfacts.Error{Op: githubfacts.OpProtection, Kind: githubfacts.KindNotFound, Status: 404}},
					}
					f.Rules.Protection = githubfacts.ClassicProtection{}
					f.Completeness = githubfacts.Partial
				}
				return f, nil
			}}
			c := coordinator(t, store, &memGitHubFactCredentials{}, reader, clk.now)
			if _, _, err := c.Refresh(context.Background(), testRequest()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			view, err := c.Lookup(context.Background(), GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", PR: 12, Target: "feat/x"})
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if view.Branch.Completeness != tc.wantCompl {
				t.Errorf("branch completeness = %q, want %q", view.Branch.Completeness, tc.wantCompl)
			}
			if view.Branch.Complete != tc.wantComplete {
				t.Errorf("Branch.Complete = %v, want %v", view.Branch.Complete, tc.wantComplete)
			}
			if view.Branch.Partial != tc.wantPartial {
				t.Errorf("Branch.Partial = %v, want %v", view.Branch.Partial, tc.wantPartial)
			}
			// The two are mutually exclusive: partial never claims completeness.
			if view.Branch.Complete && view.Branch.Partial {
				t.Error("a read is both complete and partial")
			}
		})
	}
}

// ---- the default-branch comparison ----

// TestGitHubFactsStaleDefaultBranchCannotSayInternal is the safety rule that made
// this ticket necessary. A stale default branch must never be compared to a PR's
// target and found different: that silent comparison is what classifies a real
// merge as internal agent organization and stops asking a person about it. Absence
// of evidence may not be a negative answer.
func TestGitHubFactsStaleDefaultBranchCannotSayInternal(t *testing.T) {
	tests := []struct {
		name string
		seed func(store *memGitHubFactStore, clk *clock)
		want *bool // nil: cannot be decided
	}{
		{
			name: "never read",
			seed: func(*memGitHubFactStore, *clock) {},
			want: nil,
		},
		{
			name: "stale (read 20m ago)",
			seed: func(store *memGitHubFactStore, clk *clock) {
				at := clk.now().Add(-20 * time.Minute)
				store.seedRepo(7, "acme/rocket", repository.GitHubRepoFactsWrite{
					DefaultBranch: "main", DefaultBranchSource: repository.GitHubBranchSourceREST,
					Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &at,
				})
			},
			want: nil,
		},
		{
			name: "fresh, and the target is another branch",
			seed: func(store *memGitHubFactStore, clk *clock) {
				at := clk.now()
				store.seedRepo(7, "acme/rocket", repository.GitHubRepoFactsWrite{
					DefaultBranch: "main", DefaultBranchSource: repository.GitHubBranchSourceREST,
					Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &at,
				})
				// The PR's own base, since a hint alone may not decide anything.
				store.seedPull(7, "acme/rocket", 12, repository.GitHubPullFactsWrite{
					BaseBranch: "feat/x", Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &at,
				})
			},
			want: boolPtr(false),
		},
		{
			name: "fresh, and the target IS the default branch",
			seed: func(store *memGitHubFactStore, clk *clock) {
				at := clk.now()
				store.seedRepo(7, "acme/rocket", repository.GitHubRepoFactsWrite{
					DefaultBranch: "trunk", DefaultBranchSource: repository.GitHubBranchSourceREST,
					Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &at,
				})
				store.seedPull(7, "acme/rocket", 12, repository.GitHubPullFactsWrite{
					BaseBranch: "trunk", Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &at,
				})
			},
			want: boolPtr(true),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			store := newMemGitHubFactStore()
			tc.seed(store, clk)
			c := coordinator(t, store, &memGitHubFactCredentials{}, &scriptedReader{}, clk.now)
			view, err := c.Lookup(context.Background(), testRequest())
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			got := view.TargetIsDefault()
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("TargetIsDefault = %v, want nil (undecidable)", *got)
			case tc.want != nil && got == nil:
				t.Errorf("TargetIsDefault = nil, want %v", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("TargetIsDefault = %v, want %v", *got, *tc.want)
			}
			waitIdle(t, c)
		})
	}
}

// TestGitHubFactsTargetIsDefault: only GitHub's OWN answer for the PR's target may
// be compared to the default branch. A run's reported base_branch is a hint, and a
// hint that happens not to match must not become a silent "internal".
func TestGitHubFactsTargetIsDefault(t *testing.T) {
	clk := newClock()
	store := newMemGitHubFactStore()
	at := clk.now()
	store.seedRepo(7, "acme/rocket", repository.GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: repository.GitHubBranchSourceREST,
		Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &at,
	})
	c := coordinator(t, store, &memGitHubFactCredentials{}, &scriptedReader{}, clk.now)

	// A hint only: the PR's own base was never read.
	hint, err := c.Lookup(context.Background(), GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", Target: "feat/x"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if hint.TargetSource != githubfacts.SourceHint {
		t.Errorf("TargetSource = %q, want %q", hint.TargetSource, githubfacts.SourceHint)
	}
	if isd := hint.TargetIsDefault(); isd != nil {
		t.Errorf("a hint alone decided the default-branch question: %v", *isd)
	}
	waitIdle(t, c)

	// The PR read answered: GitHub's base overrides the hint, and it is now
	// decidable.
	store.seedPull(7, "acme/rocket", 12, repository.GitHubPullFactsWrite{
		BaseBranch: "main", Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &at,
	})
	verified, err := c.Lookup(context.Background(), GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", PR: 12, Target: "feat/x"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if verified.Target != "main" {
		t.Errorf("Target = %q, want the PR's own base %q, not the hint", verified.Target, "main")
	}
	if verified.TargetSource != githubfacts.SourcePull {
		t.Errorf("TargetSource = %q, want %q", verified.TargetSource, githubfacts.SourcePull)
	}
	if isd := verified.TargetIsDefault(); isd == nil || !*isd {
		t.Errorf("TargetIsDefault = %v, want true", isd)
	}
	waitIdle(t, c)
}

// TestGitHubFactsUnknownTargetDoesNotReadRules: with no target known, no branch is
// guessed. Reading some other branch's rules would answer a question nobody asked
// and, worse, could quiet a merge with the wrong branch's evidence.
func TestGitHubFactsUnknownTargetDoesNotReadRules(t *testing.T) {
	clk := newClock()
	reader := &scriptedReader{}
	c := coordinator(t, newMemGitHubFactStore(), &memGitHubFactCredentials{}, reader, clk.now)
	view, err := c.Lookup(context.Background(), GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if view.Target != "" {
		t.Errorf("a target was invented: %q", view.Target)
	}
	if view.Branch.Trusted || view.Branch.Unknown == "" {
		t.Errorf("rules with no target are not explicit-unknown: %+v", view.Branch)
	}
	waitIdle(t, c)
	for _, call := range reader.calls {
		if strings.Contains(call.Slug, "/rules/") {
			t.Errorf("a rules read happened with no target: %+v", call)
		}
	}
}

// ---- the credential ----

// TestGitHubFactsCredentialFollowsTheRead: the cached facts record which credential
// produced them, because "read anonymously" is a weaker claim the classifier must
// treat as such. A credential store that cannot even identify the credential is not
// fatal — the facts are stored as anonymous, never as a stronger claim.
func TestGitHubFactsCredentialFollowsTheRead(t *testing.T) {
	tests := []struct {
		name     string
		creds    *memGitHubFactCredentials
		wantCred *int
	}{
		{
			name:     "a project credential",
			creds:    &memGitHubFactCredentials{tokens: map[int]string{7: "ghp_example_token"}},
			wantCred: intPtr(700),
		},
		{
			name:     "no credential attached",
			creds:    &memGitHubFactCredentials{},
			wantCred: nil,
		},
		{
			name:     "the store could not identify it",
			creds:    &memGitHubFactCredentials{tokens: map[int]string{7: "ghp_example_token"}, failID: errors.New("ciphertext unreadable")},
			wantCred: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			store := newMemGitHubFactStore()
			c := coordinator(t, store, tc.creds, &scriptedReader{}, clk.now)
			if _, _, err := c.Refresh(context.Background(), testRequest()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if len(store.repoWrites) == 0 {
				t.Fatal("no repository row was written")
			}
			got := store.repoWrites[len(store.repoWrites)-1].CredentialID
			switch {
			case tc.wantCred == nil && got != nil:
				t.Errorf("CredentialID = %d, want nil", *got)
			case tc.wantCred != nil && got == nil:
				t.Errorf("CredentialID = nil, want %d", *tc.wantCred)
			case tc.wantCred != nil && got != nil && *got != *tc.wantCred:
				t.Errorf("CredentialID = %d, want %d", *got, *tc.wantCred)
			}
		})
	}
}

// TestGitHubFactsTokenNeverAppearsInAnError: the credential reaches a request header
// and nothing else. A credential store whose error quotes the ciphertext must not
// leak it into a failure the caller can see or log.
func TestGitHubFactsTokenNeverAppearsInAnError(t *testing.T) {
	const secret = "ghp_should_never_appear_in_an_error"
	clk := newClock()
	cli, err := NewGitHubFactsCoordinator(GitHubFactsConfig{
		Store:       newMemGitHubFactStore(),
		Credentials: &memGitHubFactCredentials{tokens: map[int]string{7: secret}, failSec: errors.New("cannot open sealed value")},
		Readers: func(githubfacts.TokenSource) GitHubFactReader {
			// A reader that surfaces the token source's error the way the client
			// does: as a KindNoCredential, never with the value.
			return readerFunc(func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
				f := failingFacts(githubfacts.KindNoCredential)
				return f, nil
			})
		},
		Now: clk.now,
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	view, err := cli.Lookup(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", view), secret) {
		t.Errorf("the view exposed the token: %+v", view)
	}
	waitIdle(t, cli)
	if strings.Contains(view.Repo.Unknown, secret) || strings.Contains(view.Refresh.Skipped, secret) {
		t.Error("an error message exposed the token")
	}
}

type readerFunc func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error)

func (f readerFunc) Facts(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
	return f(ctx, req)
}

// ---- the mapping onto the cache ----

// TestGitHubFactsStoredRulesAreTheReducedArray: 044 CHECKs that rules is a JSON
// array, and the classifier reads the pull_request parameters out of it. The
// mapping must reduce the reader's rules to that shape — and an empty list must
// belong to a read that happened, not to a write that forgot.
func TestGitHubFactsStoredRulesAreTheReducedArray(t *testing.T) {
	clk := newClock()
	store := newMemGitHubFactStore()
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		f := okFacts(req)
		f.Rules.Rules = []githubfacts.Rule{
			{Type: "deletion", RulesetID: 9},
			{
				Type: "pull_request", RulesetID: 9, RulesetSource: "main", RulesetSourceType: "Repository",
				Parameters: githubfacts.RuleParameters{
					RequiredApprovingReviewCount: 1,
					RequireCodeOwnerReview:       true,
					RequireLastPushApproval:      true,
					RequiredReviewers: []githubfacts.Reviewer{
						{Type: "User", ID: 1, Login: "octocat"},
						{Type: "Team", ID: 2, Slug: "rocketeers"},
					},
				},
			},
		}
		return f, nil
	}}
	c := coordinator(t, store, &memGitHubFactCredentials{}, reader, clk.now)
	if _, _, err := c.Refresh(context.Background(), testRequest()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(store.branchWrites) == 0 {
		t.Fatal("no branch row was written")
	}
	rules := store.branchWrites[len(store.branchWrites)-1].Rules
	var arr []map[string]any
	if err := json.Unmarshal(rules, &arr); err != nil {
		t.Fatalf("the stored rules are not a JSON array: %s (%v)", rules, err)
	}
	if len(arr) != 2 {
		t.Fatalf("stored %d rules, want 2: %s", len(arr), rules)
	}
	if arr[0]["type"] != "deletion" {
		t.Errorf("rule 0 type = %v, want deletion", arr[0]["type"])
	}
	if arr[1]["type"] != "pull_request" {
		t.Fatalf("rule 1 type = %v, want pull_request", arr[1]["type"])
	}
	if arr[1]["ruleset_source"] != "main" || arr[1]["ruleset_source_type"] != "Repository" {
		t.Errorf("the ruleset is not recorded: %v", arr[1])
	}
	params, ok := arr[1]["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("the pull_request rule has no parameters: %v", arr[1])
	}
	// The classifier reads these names; a rename here silently breaks it.
	for _, key := range []string{
		"required_approving_review_count", "require_code_owner_review", "required_reviewers",
		"require_last_push_approval", "require_extra_approval_for_unattributed_changes",
		"required_review_thread_resolution",
	} {
		if _, ok := params[key]; !ok {
			t.Errorf("the stored parameters lack %q: %v", key, params)
		}
	}
	if got := params["required_approving_review_count"]; got != float64(1) {
		t.Errorf("required_approving_review_count = %v, want 1", got)
	}
	reviewers, ok := params["required_reviewers"].([]any)
	if !ok || len(reviewers) != 2 {
		t.Fatalf("required_reviewers = %v, want 2 entries", params["required_reviewers"])
	}
}

// TestGitHubFactsEmptyRulesAreStoredAsAnEmptyArray: "the read happened and nothing
// applies" must stay distinguishable from "never read", so an empty list is stored
// as the empty array rather than omitted.
func TestGitHubFactsEmptyRulesAreStoredAsAnEmptyArray(t *testing.T) {
	clk := newClock()
	store := newMemGitHubFactStore()
	c := coordinator(t, store, &memGitHubFactCredentials{}, &scriptedReader{}, clk.now)
	if _, _, err := c.Refresh(context.Background(), testRequest()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rules := store.branchWrites[len(store.branchWrites)-1].Rules
	if string(rules) != "[]" {
		t.Errorf("stored rules = %q, want %q", rules, "[]")
	}
}

// TestGitHubFactsPullStateIsOnlyMergedOrUnknown: REST's merged_at is null for an
// open PR and for a closed-unmerged one alike, and the read does not ask for
// `state`. Guessing "open" would claim knowledge the read does not have.
func TestGitHubFactsPullStateIsOnlyMergedOrUnknown(t *testing.T) {
	tests := []struct {
		name   string
		merged bool
		want   string
	}{
		{name: "merged", merged: true, want: "merged"},
		{name: "not merged (open or closed, unknowable here)", merged: false, want: "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pullFactsState(githubfacts.PullFacts{Known: true, Merged: tc.merged}); got != tc.want {
				t.Errorf("pullFactsState = %q, want %q", got, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// ---- webhook invalidation (AMP #3160) ----

// TestGitHubFactsInvalidatePullDropsOnlyThatPull is the guard on the invalidation's
// SCOPE: a pull-request delivery drops that pull request's facts and nothing else.
//
// Dropping the repository or branch rows here would be the plausible bug: they are
// in the same cache and it is one statement to delete them. It is wrong because a
// pull-request event says nothing about the default branch or a branch's rules —
// so the "fix" would throw away good evidence on every delivery and make the 15m
// TTL mean nothing. Mutation: make InvalidatePull call InvalidateGitHubRepoFacts
// and this test fails on the surviving repository and branch rows.
func TestGitHubFactsInvalidatePullDropsOnlyThatPull(t *testing.T) {
	clk := newClock()
	store := newMemGitHubFactStore()
	slug := "acme/rocket"
	readAt := clk.now()
	store.seedRepo(7, slug, repository.GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: repository.GitHubBranchSourceREST,
		Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	store.seedBranch(7, slug, "feat/x", repository.GitHubBranchFactsWrite{
		Rules: []byte(`[]`), Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	store.seedPull(7, slug, 12, repository.GitHubPullFactsWrite{
		BaseBranch: "feat/x", State: "unknown", Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	// Sibling PR and a same-named repository in another project: both must survive.
	store.seedPull(7, slug, 13, repository.GitHubPullFactsWrite{
		BaseBranch: "main", State: "unknown", Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	store.seedRepo(8, slug, repository.GitHubRepoFactsWrite{
		DefaultBranch: "trunk", DefaultBranchSource: repository.GitHubBranchSourceREST,
		Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	store.seedPull(8, slug, 12, repository.GitHubPullFactsWrite{
		BaseBranch: "trunk", State: "merged", Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	c := coordinator(t, store, &memGitHubFactCredentials{}, &scriptedReader{}, clk.now)

	ctx := context.Background()
	if _, err := c.Lookup(ctx, GitHubFactsRequest{ProjectID: 7, Slug: slug, PR: 12, Target: "feat/x"}); err != nil {
		t.Fatalf("lookup before invalidation: %v", err)
	}

	had, err := c.InvalidatePull(ctx, GitHubFactsRequest{ProjectID: 7, Slug: slug, PR: 12})
	if err != nil || !had {
		t.Fatalf("InvalidatePull = (%v, %v), want (true, nil)", had, err)
	}

	// The pull request's own facts are gone: the next read sees them as unread.
	if _, err := store.GetGitHubPullFacts(ctx, repository.GitHubFactsKey{ProjectID: 7, Slug: slug}, 12); !errors.Is(err, repository.ErrNoGitHubPullFacts) {
		t.Errorf("the invalidated pull request's facts survive: %v", err)
	}
	// ... and everything else is untouched.
	if b, err := store.GetGitHubPullFacts(ctx, repository.GitHubFactsKey{ProjectID: 7, Slug: slug}, 13); err != nil || b.BaseBranch != "main" {
		t.Errorf("a sibling pull request's facts were dropped: %+v %v", b, err)
	}
	if f, err := store.GetGitHubRepoFacts(ctx, repository.GitHubFactsKey{ProjectID: 7, Slug: slug}); err != nil || f.DefaultBranch != "main" {
		t.Errorf("the repository's default branch was dropped: %+v %v", f, err)
	}
	if b, err := store.GetGitHubBranchFacts(ctx, repository.GitHubFactsKey{ProjectID: 7, Slug: slug}, "feat/x"); err != nil || len(b.Rules) == 0 {
		t.Errorf("the branch's rules were dropped: %+v %v", b, err)
	}
	// Cross-project: same slug, same PR number, another project — never reached.
	if p, err := store.GetGitHubPullFacts(ctx, repository.GitHubFactsKey{ProjectID: 8, Slug: slug}, 12); err != nil || p.State != "merged" {
		t.Errorf("another project's pull request was dropped: %+v %v", p, err)
	}

	// A second invalidation of the same facts is a miss, not an error: a delivery
	// for a repository AMP never cached is the ordinary case, not a failure.
	if had, err := c.InvalidatePull(ctx, GitHubFactsRequest{ProjectID: 7, Slug: slug, PR: 12}); err != nil || had {
		t.Errorf("second InvalidatePull = (%v, %v), want (false, nil)", had, err)
	}
	// A request that cannot name a row is refused before it reaches the cache.
	if _, err := c.InvalidatePull(ctx, GitHubFactsRequest{ProjectID: 7, Slug: slug}); err == nil {
		t.Error("InvalidatePull with no pull request number was accepted")
	}
	if _, err := c.InvalidatePull(ctx, GitHubFactsRequest{ProjectID: 0, Slug: slug, PR: 12}); err == nil {
		t.Error("InvalidatePull with no project was accepted")
	}
}

// TestGitHubFactsInvalidatePullClearsTheNegativeWindow: a delivery is new evidence,
// so it must also clear the failure that was keeping the scope quiet. Otherwise the
// row is dropped and the next read still refuses to refill it for up to NegativeTTL
// — the invalidation would do nothing for exactly as long as the error it was meant
// to clear. Mutation: drop the forgetPull call and the "after invalidation" read
// below starts no refresh.
func TestGitHubFactsInvalidatePullClearsTheNegativeWindow(t *testing.T) {
	clk := newClock()
	fail := true
	seed := make(chan struct{})
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		<-seed
		if fail {
			return failingFacts(githubfacts.KindNotFound), nil
		}
		return okFacts(req), nil
	}}
	close(seed)
	store := newMemGitHubFactStore()
	c := coordinator(t, store, &memGitHubFactCredentials{}, reader, clk.now)
	req := testRequest()

	// A first read whose refresh fails, which negative-caches the scope.
	if _, err := c.Lookup(context.Background(), req); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	waitIdle(t, c)
	if n := reader.callCount(); n != 1 {
		t.Fatalf("the first refresh made %d reads, want 1", n)
	}
	// Inside the window, nothing retries — that is the negative cache working.
	if v, err := c.Lookup(context.Background(), req); err != nil || v.Refresh.Started || v.Refresh.Joined {
		t.Fatalf("a negative-cached scope started a refresh: %+v %v", v.Refresh, err)
	}

	// A delivery arrives. It drops the (empty) row and must forget the failure.
	clk.advance(2 * time.Second) // still inside the 60s window
	fail = false                 // the next refresh succeeds
	if _, err := c.InvalidatePull(context.Background(), GitHubFactsRequest{ProjectID: req.ProjectID, Slug: req.Slug, PR: req.PR}); err != nil {
		t.Fatalf("InvalidatePull: %v", err)
	}

	after, err := c.Lookup(context.Background(), req)
	if err != nil {
		t.Fatalf("lookup after invalidation: %v", err)
	}
	if !after.Refresh.Started && !after.Refresh.Joined {
		t.Errorf("a delivery did not clear the negative window: %+v", after.Refresh)
	}
	if after.Refresh.Failure != "" {
		t.Errorf("Refresh.Failure = %q after a delivery, want none", after.Refresh.Failure)
	}
	waitIdle(t, c)
	if n := reader.callCount(); n != 2 {
		t.Errorf("reads after the delivery = %d, want 2", n)
	}
}

// TestGitHubFactsInvalidatedPullIsUnknownNotAbsent: after an invalidation the pulled
// facts are unknown and say so, and the read wants to refill them. A card must
// never read "dropped" as "GitHub says no reviewer is required".
func TestGitHubFactsInvalidatedPullIsUnknownNotAbsent(t *testing.T) {
	clk := newClock()
	readAt := clk.now()
	store := newMemGitHubFactStore()
	store.seedRepo(7, "acme/rocket", repository.GitHubRepoFactsWrite{
		DefaultBranch: "main", DefaultBranchSource: repository.GitHubBranchSourceREST,
		Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	store.seedBranch(7, "acme/rocket", "feat/x", repository.GitHubBranchFactsWrite{
		Rules: []byte(`[]`), Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	store.seedPull(7, "acme/rocket", 12, repository.GitHubPullFactsWrite{
		BaseBranch: "feat/x", RequestedReviewers: []string{"octocat"},
		Completeness: repository.GitHubCompletenessComplete, VerifiedAt: &readAt,
	})
	// The refill blocks, so the assertion is about the VIEW, not a race.
	gate := make(chan struct{})
	reader := &scriptedReader{answer: func(ctx context.Context, req githubfacts.Request) (*githubfacts.Facts, error) {
		<-gate
		return okFacts(req), nil
	}}
	c := coordinator(t, store, &memGitHubFactCredentials{}, reader, clk.now)
	req := testRequest()

	before, err := c.Lookup(context.Background(), req)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	waitIdle(t, c)
	if !before.Pull.Trusted || len(before.Pull.RequestedReviewers) != 1 {
		t.Fatalf("setup: the pull facts are not trusted: %+v", before.Pull)
	}
	if _, err := c.InvalidatePull(context.Background(), GitHubFactsRequest{ProjectID: req.ProjectID, Slug: req.Slug, PR: req.PR}); err != nil {
		t.Fatalf("InvalidatePull: %v", err)
	}

	after, err := c.Lookup(context.Background(), req)
	if err != nil {
		t.Fatalf("lookup after invalidation: %v", err)
	}
	if after.Pull.Trusted {
		t.Error("an invalidated pull request's facts are still trusted")
	}
	if after.Pull.Unknown == "" {
		t.Error("an invalidated pull request's facts have no Unknown reason: dropped must not read as absent")
	}
	if len(after.Pull.RequestedReviewers) != 0 {
		t.Errorf("the dropped reviewers are still present: %v", after.Pull.RequestedReviewers)
	}
	if !after.Refresh.Started && !after.Refresh.Joined {
		t.Errorf("nothing wanted to refill the invalidated facts: %+v", after.Refresh)
	}
	close(gate)
	waitIdle(t, c)
}
