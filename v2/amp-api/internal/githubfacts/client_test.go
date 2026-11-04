package githubfacts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- a stubbed GitHub ----
//
// Every test drives the client against a real http.Server (httptest) rather
// than a mocked interface: the encoding of a branch name, the headers that go
// on the wire, and the token's absence from anything a caller sees are
// properties of the HTTP layer, so the wire is what the tests read.

type stub struct {
	mu     sync.Mutex
	routes []route
	paths  []string // RequestURI of every request, in order
	auth   []string // Authorization header of every request
}

type route struct {
	// path is the escaped path to match, or a prefix when it ends in "*".
	path string
	h    func(http.ResponseWriter, *http.Request)
}

func newStub(t *testing.T) (*stub, *httptest.Server) {
	t.Helper()
	s := &stub{}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	escaped := r.URL.EscapedPath()
	s.mu.Lock()
	s.paths = append(s.paths, r.RequestURI)
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	routes := append([]route(nil), s.routes...)
	s.mu.Unlock()

	for _, rt := range routes {
		// Matching on the escaped path is half of what the encoding test
		// proves: a branch's %2F must survive to here.
		if prefix, ok := strings.CutSuffix(rt.path, "*"); ok {
			if strings.HasPrefix(escaped, prefix) {
				rt.h(w, r)
				return
			}
			continue
		}
		if escaped == rt.path {
			rt.h(w, r)
			return
		}
	}
	http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
}

func (s *stub) on(path string, h func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append(s.routes, route{path: path, h: h})
}

func (s *stub) json(path string, status int, body string) {
	s.on(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
}

func (s *stub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *stub) auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// count returns how many requests reached exactly path.
func (s *stub) count(path string) int {
	n := 0
	for _, p := range s.requests() {
		if p == path {
			n++
		}
	}
	return n
}

func clientFor(srv *httptest.Server, token string) *Client {
	return New(Options{
		BaseURL:   srv.URL,
		Transport: srv.Client().Transport,
		Token:     func(context.Context) (string, error) { return token, nil },
	})
}

// The fixtures mirror the live shapes recorded in
// decisions/repo-default-branch.md §1: jacobsims214/amp has an active
// ruleset on main, no classic protection, and no CODEOWNERS.

const (
	repoBody = `{"full_name":"jacobsims214/amp","default_branch":"main","private":true}`

	// PR 9 targets the default branch with a reviewer and a team requested.
	pullBody = `{
		"number": 9,
		"base": {"ref": "main"},
		"requested_reviewers": [{"login": "jacobsims214"}],
		"requested_teams": [{"slug": "engineering"}],
		"merged_at": null
	}`

	// PR 109 targets the integration branch (a slash in the name).
	pullIntegrationBody = `{
		"number": 109,
		"base": {"ref": "fix/actionable-merge-workflow"},
		"requested_reviewers": [],
		"requested_teams": [],
		"merged_at": null
	}`

	// main's evaluated rules, copied from the live read on 2026-11-03.
	rulesBody = `[
		{"type":"deletion","ruleset_id":26480994,"ruleset_source":"main","ruleset_source_type":"Repository"},
		{"type":"non_fast_forward","ruleset_id":26480994,"ruleset_source":"main","ruleset_source_type":"Repository"},
		{"type":"pull_request","ruleset_id":26480994,"ruleset_source":"main","ruleset_source_type":"Repository",
		 "parameters":{
			"required_approving_review_count":0,
			"dismiss_stale_reviews_on_push":false,
			"required_reviewers":[],
			"require_code_owner_review":false,
			"require_last_push_approval":false,
			"required_review_thread_resolution":false,
			"require_extra_approval_for_unattributed_changes":true,
			"allowed_merge_methods":["squash","merge"]
		 }}
	]`

	protectedBody = `{"required_pull_request_reviews":{"required_approving_review_count":2,"require_code_owner_reviews":true,"require_last_push_approval":false}}`
)

// TestFactsComplete proves the happy path: every read answered, so the facts
// are Complete — the only state in which an empty rule list may be read as
// "no rule applies".
func TestFactsComplete(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/jacobsims214/amp", 200, repoBody)
	s.json("/repos/jacobsims214/amp/pulls/9", 200, pullBody)
	s.json("/repos/jacobsims214/amp/rules/branches/main", 200, rulesBody)
	s.json("/repos/jacobsims214/amp/branches/main/protection", 200, protectedBody)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", PR: 9})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if f.Completeness != Complete {
		t.Fatalf("Completeness = %q, want complete (reads: %v)", f.Completeness, f.Reads)
	}
	if f.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", f.DefaultBranch)
	}
	if f.PR != 9 || f.Slug != "jacobsims214/amp" {
		t.Errorf("identity = %s #%d", f.Slug, f.PR)
	}
	if f.Target != "main" || f.TargetSource != SourcePull {
		t.Errorf("target = %q from %q, want main from %q", f.Target, f.TargetSource, SourcePull)
	}
	if !f.Pull.Known || f.Pull.Base != "main" || f.Pull.Merged {
		t.Errorf("pull = %+v", f.Pull)
	}
	if got := strings.Join(f.Pull.RequestedReviewers, ","); got != "jacobsims214" {
		t.Errorf("requested reviewers = %q", got)
	}
	if got := strings.Join(f.Pull.RequestedTeams, ","); got != "engineering" {
		t.Errorf("requested teams = %q", got)
	}
	if !f.Rules.Known || len(f.Rules.Rules) != 3 {
		t.Fatalf("rules = %+v", f.Rules)
	}
	pr := f.Rules.PullRequestRules()
	if len(pr) != 1 {
		t.Fatalf("pull_request rules = %d, want 1", len(pr))
	}
	rule := pr[0]
	if rule.RulesetSource != "main" || rule.RulesetSourceType != "Repository" || rule.RulesetID != 26480994 {
		t.Errorf("rule provenance = %+v", rule)
	}
	if !rule.Parameters.RequireExtraApprovalForUnattributedChanges {
		t.Error("require_extra_approval_for_unattributed_changes was not read")
	}
	if got := strings.Join(rule.Parameters.AllowedMergeMethods, ","); got != "squash,merge" {
		t.Errorf("allowed_merge_methods = %q", got)
	}
	if !f.Rules.Protection.Known || f.Rules.Protection.Protected == nil || !*f.Rules.Protection.Protected {
		t.Fatalf("classic protection = %+v", f.Rules.Protection)
	}
	if f.Rules.Protection.RequiredApprovingReviewCount == nil || *f.Rules.Protection.RequiredApprovingReviewCount != 2 {
		t.Errorf("classic required_approving_review_count = %v", f.Rules.Protection.RequiredApprovingReviewCount)
	}
	if f.Rules.Protection.RequireCodeOwnerReviews == nil || !*f.Rules.Protection.RequireCodeOwnerReviews {
		t.Errorf("classic require_code_owner_reviews = %v", f.Rules.Protection.RequireCodeOwnerReviews)
	}
	// The token went as a header and nowhere else.
	if auths := s.auths(); len(auths) == 0 || auths[0] != "Bearer ghp_testtoken" {
		t.Errorf("Authorization headers = %v", auths)
	}
	for _, p := range s.requests() {
		if strings.Contains(p, "ghp_testtoken") {
			t.Fatalf("token reached the request URL: %s", p)
		}
	}
}

// TestFactsPartialOnUnreadableProtection is the safety rule: rules were read
// and contain nothing that could require a person, but the classic endpoint
// answered 404 — which GitHub returns both for "not protected" and for "you
// may not look". The facts must stay Partial, and protection must stay
// unknown rather than false.
func TestFactsPartialOnUnreadableProtection(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/jacobsims214/amp", 200, repoBody)
	s.json("/repos/jacobsims214/amp/rules/branches/main", 200, `[]`)
	s.json("/repos/jacobsims214/amp/branches/main/protection", 404, `{"message":"Branch not protected"}`)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if f.Completeness != Partial {
		t.Fatalf("Completeness = %q, want partial: an unreadable classic endpoint may not complete the facts", f.Completeness)
	}
	if f.TargetSource != SourceHint {
		t.Errorf("TargetSource = %q, want hint (no PR was read)", f.TargetSource)
	}
	if !f.Rules.Known {
		t.Error("Rules.Known = false: the rules read answered")
	}
	if len(f.Rules.PullRequestRules()) != 0 {
		t.Errorf("rules = %+v", f.Rules.Rules)
	}
	if f.Rules.Protection.Known || f.Rules.Protection.Protected != nil {
		t.Fatalf("protection = %+v, want unknown: a 404 is not proof the branch is unprotected", f.Rules.Protection)
	}
	r, ok := f.ReadFor(OpProtection)
	if !ok || r.OK {
		t.Fatalf("protection read = %+v (attempted=%v)", r, ok)
	}
	if r.Err == nil || r.Err.Kind != KindNotFound || r.Err.Status != 404 {
		t.Fatalf("protection error = %+v, want not_found/404", r.Err)
	}
}

// TestFactsUnknownWhenNothingRead: a private repository a tokenless client
// cannot see answers 404 everywhere. That is Unknown, never "no rules".
func TestFactsUnknownWhenNothingRead(t *testing.T) {
	s, srv := newStub(t)
	// No routes: the stub answers 404 to everything, as GitHub does for a
	// repository the caller cannot see.
	f, err := New(Options{BaseURL: srv.URL, Transport: srv.Client().Transport}).
		Facts(context.Background(), Request{Slug: "acme/private", PR: 7, Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if f.Completeness != Unknown {
		t.Fatalf("Completeness = %q, want unknown", f.Completeness)
	}
	if f.DefaultBranch != "" || f.Rules.Known || f.Pull.Known {
		t.Fatalf("facts invented from 404s: %+v", f)
	}
	if len(f.Reads) != 4 {
		t.Fatalf("reads = %v, want repo, pull, rules, protection", f.Reads)
	}
	for _, r := range f.Reads {
		if r.OK || r.Err == nil || r.Err.Kind != KindNotFound {
			t.Errorf("read %s = %+v, want a not_found error", r.Op, r)
		}
	}
	if len(s.auths()) != 4 {
		t.Fatalf("requests = %v", s.auths())
	}
	for _, a := range s.auths() {
		if a != "" {
			t.Errorf("no token source, but sent %q", a)
		}
	}
}

// TestFactsRulesAnswerDespiteRepoFailure: one failed read does not discard
// the ones that answered — but it does keep the facts off Complete.
func TestFactsRulesAnswerDespiteRepoFailure(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/jacobsims214/amp", 500, `{"message":"Server Error"}`)
	s.json("/repos/jacobsims214/amp/rules/branches/main", 200, rulesBody)
	s.json("/repos/jacobsims214/amp/branches/main/protection", 200, `{}`)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if f.Completeness != Partial {
		t.Fatalf("Completeness = %q, want partial", f.Completeness)
	}
	if f.DefaultBranch != "" {
		t.Errorf("DefaultBranch = %q, want empty", f.DefaultBranch)
	}
	if !f.Rules.Known || len(f.Rules.PullRequestRules()) != 1 {
		t.Errorf("rules were discarded with the repo failure: %+v", f.Rules)
	}
	if !f.Rules.Protection.Known || f.Rules.Protection.Protected == nil {
		t.Errorf("protection = %+v", f.Rules.Protection)
	}
	r, _ := f.ReadFor(OpRepo)
	if r.Err == nil || r.Err.Kind != KindServer || r.Err.Status != 500 {
		t.Fatalf("repo error = %+v, want server/500", r.Err)
	}
}

// TestBranchNameIsEscaped: a slash in a branch name must reach the wire as
// one escaped segment (decisions/repo-default-branch.md, Gotchas).
func TestBranchNameIsEscaped(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/jacobsims214/amp", 200, repoBody)
	s.json("/repos/jacobsims214/amp/pulls/109", 200, pullIntegrationBody)
	s.json("/repos/jacobsims214/amp/rules/branches/fix%2Factionable-merge-workflow", 200, `[]`)
	s.json("/repos/jacobsims214/amp/branches/fix%2Factionable-merge-workflow/protection", 404, `{"message":"Branch not protected"}`)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", PR: 109})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	want := []string{
		"/repos/jacobsims214/amp",
		"/repos/jacobsims214/amp/pulls/109",
		"/repos/jacobsims214/amp/rules/branches/fix%2Factionable-merge-workflow",
		"/repos/jacobsims214/amp/branches/fix%2Factionable-merge-workflow/protection",
	}
	got := s.requests()
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
	if f.Target != "fix/actionable-merge-workflow" {
		t.Errorf("Target = %q, want the decoded branch", f.Target)
	}
	// The rules read can only have matched if %2F survived: the stub matches
	// the escaped path exactly.
	if !f.Rules.Known {
		t.Errorf("the escaped rules route did not match: %v", f.Reads)
	}
}

// TestPullBaseOverridesRunHint: a run may report a stale or wrong base; the
// PR's own base.ref wins, and it is GitHub's answer, so it is "detected".
func TestPullBaseOverridesRunHint(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/jacobsims214/amp", 200, repoBody)
	s.json("/repos/jacobsims214/amp/pulls/9", 200, `{"base":{"ref":"main"},"requested_reviewers":[],"requested_teams":[],"merged_at":"2026-11-03T14:00:00Z"}`)
	s.json("/repos/jacobsims214/amp/rules/branches/main", 200, rulesBody)
	s.json("/repos/jacobsims214/amp/branches/main/protection", 200, `{}`)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", PR: 9, Target: "feat/stale-hint"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if f.Target != "main" || f.TargetSource != SourcePull {
		t.Fatalf("target = %q from %q, want main from %q", f.Target, f.TargetSource, SourcePull)
	}
	if !f.Pull.Merged {
		t.Error("merged_at was set, but Pull.Merged is false")
	}
	if joined := strings.Join(s.requests(), " "); strings.Contains(joined, "stale-hint") {
		t.Errorf("the run's hint was used despite the PR's own base: %v", joined)
	}
}

// TestNoTargetIsUnknown: with no PR and no hint there is no branch to
// evaluate, and the rule set must stay unknown rather than reading some
// default.
func TestNoTargetIsUnknown(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/jacobsims214/amp", 200, repoBody)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if f.Completeness == Complete {
		t.Fatal("Completeness = complete without reading any rules")
	}
	if f.Completeness != Partial {
		t.Errorf("Completeness = %q, want partial (the repo read answered)", f.Completeness)
	}
	r, ok := f.ReadFor(OpRules)
	if !ok || r.Err == nil || r.Err.Kind != KindNoTarget {
		t.Fatalf("rules read = %+v (attempted=%v), want no_target", r, ok)
	}
	if f.Rules.Known {
		t.Error("Rules.Known = true without a rules read")
	}
	if reqs := s.requests(); len(reqs) != 1 {
		t.Errorf("requests = %v: no rules request should have been made", reqs)
	}
}

// TestTimeoutIsTypedAndBounded: a server that never answers must produce a
// timeout error in bounded time and leave the facts partial.
func TestTimeoutIsTypedAndBounded(t *testing.T) {
	s, srv := newStub(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.json("/repos/jacobsims214/amp", 200, repoBody)
	s.on("/repos/jacobsims214/amp/rules/branches/*", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	s.json("/repos/jacobsims214/amp/branches/main/protection", 200, `{}`)

	c := New(Options{
		BaseURL:   srv.URL,
		Transport: srv.Client().Transport,
		Token:     func(context.Context) (string, error) { return "ghp_testtoken", nil },
		Timeout:   150 * time.Millisecond,
		Total:     2 * time.Second,
	})
	start := time.Now()
	f, err := c.Facts(context.Background(), Request{Slug: "jacobsims214/amp", Target: "main"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Facts took %v: the request deadline did not bound it", elapsed)
	}
	r, ok := f.ReadFor(OpRules)
	if !ok || r.Err == nil {
		t.Fatalf("rules read = %+v", r)
	}
	if r.Err.Kind != KindTimeout {
		t.Fatalf("rules error = %+v, want a timeout", r.Err)
	}
	if f.Completeness == Complete {
		t.Fatal("a timed-out read completed the facts")
	}
	if f.Rules.Known {
		t.Error("Rules.Known = true after a timeout")
	}
	// The reads after the timeout still happened: one slow endpoint does not
	// stop the rest, it only keeps the facts off Complete.
	if !f.Rules.Protection.Known {
		t.Errorf("protection was skipped after the rules timeout: %v", f.Reads)
	}
}

// TestContextDeadlineIsHonored: the caller's own deadline is the outer
// bound, and cancelation is reported as such, not as a network failure.
func TestContextDeadlineIsHonored(t *testing.T) {
	s, srv := newStub(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	s.on("/repos/jacobsims214/amp*", slow)

	// Total: -1 means the client adds no bound of its own.
	c := New(Options{BaseURL: srv.URL, Transport: srv.Client().Transport, Timeout: -1, Total: -1})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	f, err := c.Facts(ctx, Request{Slug: "jacobsims214/amp", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	r, _ := f.ReadFor(OpRepo)
	if r.Err == nil || r.Err.Kind != KindTimeout {
		t.Fatalf("repo error = %+v, want a timeout", r.Err)
	}
	if f.Completeness != Unknown {
		t.Errorf("Completeness = %q, want unknown", f.Completeness)
	}

	// An already-canceled context is a cancelation, not a timeout.
	canceled, stop := context.WithCancel(context.Background())
	stop()
	f2, err := c.Facts(canceled, Request{Slug: "jacobsims214/amp", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	r2, _ := f2.ReadFor(OpRepo)
	if r2.Err == nil || r2.Err.Kind != KindCanceled {
		t.Fatalf("repo error = %+v, want canceled", r2.Err)
	}
}

// TestRateLimitIsNotWaitedOut: a rate limit that asks for an hour is
// reported. It must not become a backoff loop, and no retry happens.
func TestRateLimitIsNotWaitedOut(t *testing.T) {
	s, srv := newStub(t)
	s.on("/repos/jacobsims214/amp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	})

	start := time.Now()
	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", Target: "main"})
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the read waited %v out on an advertised hour-long rate limit", waited)
	}
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	r, _ := f.ReadFor(OpRepo)
	if r.Err == nil || r.Err.Kind != KindRateLimited {
		t.Fatalf("repo error = %+v, want rate_limited", r.Err)
	}
	if n := s.count("/repos/jacobsims214/amp"); n != 1 {
		t.Fatalf("repo endpoint called %d times: a long rate limit must not be retried", n)
	}
	if f.Completeness == Complete {
		t.Fatal("a rate-limited read completed the facts")
	}
}

// TestShortRateLimitIsRetriedOnce: a zero-second Retry-After is cheap enough
// to sit out, and the retry is the only one allowed.
func TestShortRateLimitIsRetriedOnce(t *testing.T) {
	s, srv := newStub(t)
	var mu sync.Mutex
	calls := 0
	s.on("/repos/jacobsims214/amp", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, repoBody)
	})
	s.json("/repos/jacobsims214/amp/rules/branches/main", 200, rulesBody)
	s.json("/repos/jacobsims214/amp/branches/main/protection", 200, `{}`)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if !f.ReadOK(OpRepo) || f.DefaultBranch != "main" {
		t.Fatalf("the retry did not recover: %+v", f.Reads)
	}
	if f.Completeness != Complete {
		t.Errorf("Completeness = %q, want complete", f.Completeness)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("repo endpoint called %d times, want exactly 2 (one retry)", calls)
	}
}

// TestRepeatedRateLimitStopsAfterOneRetry: a server that keeps answering
// 429 must not be retried more than once.
func TestRepeatedRateLimitStopsAfterOneRetry(t *testing.T) {
	s, srv := newStub(t)
	s.on("/repos/jacobsims214/amp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"secondary rate limit"}`)
	})
	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "jacobsims214/amp", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	r, _ := f.ReadFor(OpRepo)
	if r.Err == nil || r.Err.Kind != KindRateLimited {
		t.Fatalf("repo error = %+v, want rate_limited", r.Err)
	}
	if n := s.count("/repos/jacobsims214/amp"); n != 2 {
		t.Fatalf("repo endpoint called %d times, want 2", n)
	}
}

// TestNoSecretExposed: the token must not reach an error message, even when
// the server echoes it back in its own message, and must not reach a
// redirect Location.
func TestNoSecretExposed(t *testing.T) {
	const token = "ghp_SUPERSECRETVALUE1234567890"

	tests := []struct {
		name   string
		status int
		body   string
		header map[string]string
		kind   Kind
	}{
		{
			name:   "forbidden echoing the token",
			status: http.StatusForbidden,
			body:   `{"message":"Bad credentials: ` + token + `"}`,
			kind:   KindForbidden,
		},
		{
			name:   "not found echoing the token",
			status: http.StatusNotFound,
			body:   `{"message":"no repo for ` + token + `"}`,
			kind:   KindNotFound,
		},
		{
			name:   "moved with the token in Location",
			status: http.StatusMovedPermanently,
			body:   `{"message":"Moved Permanently"}`,
			header: map[string]string{"Location": "https://api.github.com/repos/x/y?access_token=" + token},
			kind:   KindMoved,
		},
		{
			name:   "rate limited echoing the token",
			status: http.StatusTooManyRequests,
			body:   `{"message":"slow down ` + token + `"}`,
			header: map[string]string{"Retry-After": "3600"},
			kind:   KindRateLimited,
		},
		{
			name:   "server error echoing the token",
			status: http.StatusBadGateway,
			body:   `{"message":"upstream said ` + token + `"}`,
			kind:   KindServer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, srv := newStub(t)
			s.on("/repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			})

			f, err := clientFor(srv, token).Facts(context.Background(), Request{Slug: "acme/widgets", Target: "main"})
			if err != nil {
				t.Fatalf("Facts: %v", err)
			}
			r, ok := f.ReadFor(OpRepo)
			if !ok || r.Err == nil {
				t.Fatalf("repo read = %+v", r)
			}
			if r.Err.Kind != tt.kind {
				t.Errorf("Kind = %q, want %q", r.Err.Kind, tt.kind)
			}
			// Error() is what a person or a log sees; Message is what a
			// projection might carry.
			for _, seen := range []string{r.Err.Error(), r.Err.Message} {
				if strings.Contains(seen, token) {
					t.Fatalf("the credential leaked into an error: %q", seen)
				}
			}
			if rendered := fmt.Sprintf("%+v %v", f, f); strings.Contains(rendered, token) {
				t.Fatalf("the credential leaked into the facts: %s", rendered)
			}
			// It was sent as a bearer header, which is the only place it
			// belongs.
			auths := s.auths()
			if len(auths) == 0 || auths[0] != "Bearer "+token {
				t.Fatalf("Authorization = %v, want the bearer header", auths)
			}
			for _, p := range s.requests() {
				if strings.Contains(p, token) {
					t.Fatalf("the credential reached the URL: %s", p)
				}
			}
		})
	}
}

// TestCredentialUnavailableIsTyped: a credential store that fails is its own
// failure class, and its error is not forwarded (it could quote a ciphertext
// or a key).
func TestCredentialUnavailableIsTyped(t *testing.T) {
	s, srv := newStub(t)
	c := New(Options{
		BaseURL:   srv.URL,
		Transport: srv.Client().Transport,
		Token: func(context.Context) (string, error) {
			return "", errors.New("ciphertext corrupted under key v3")
		},
	})
	f, err := c.Facts(context.Background(), Request{Slug: "acme/widgets", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	r, _ := f.ReadFor(OpRepo)
	if r.Err == nil || r.Err.Kind != KindNoCredential {
		t.Fatalf("repo error = %+v, want no_credential", r.Err)
	}
	if strings.Contains(r.Err.Error(), "ciphertext") {
		t.Errorf("the credential store's error leaked into the message: %q", r.Err.Error())
	}
	if !errors.Is(r.Err, r.Err.err) {
		t.Error("the cause is not reachable with errors.Is")
	}
	if len(s.requests()) != 0 {
		t.Errorf("requests = %v: no request should be sent without a credential answer", s.requests())
	}
	if f.Completeness == Complete {
		t.Fatal("a missing credential completed the facts")
	}
}

// TestBadSlugIsRejectedBeforeAnyRequest: a slug is validated and escaped, so
// a value that would escape the path never reaches the wire.
func TestBadSlugIsRejectedBeforeAnyRequest(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"owner",
		"/repo",
		"owner/",
		"../org/repo",
		"owner/repo/extra",
		"own er/repo",
		"owner/re po",
		"owner/re%2Fpo",
		"owner/../repo",
	}
	for _, slug := range tests {
		t.Run(slug, func(t *testing.T) {
			s, srv := newStub(t)
			f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: slug})
			if err == nil {
				t.Fatalf("Facts(%q) = %+v, want an error", slug, f)
			}
			var gfErr *Error
			if !errors.As(err, &gfErr) || gfErr.Kind != KindInvalid {
				t.Fatalf("error = %v, want a typed invalid error", err)
			}
			if got := s.requests(); len(got) != 0 {
				t.Fatalf("requests = %v, want none for an invalid slug", got)
			}
		})
	}
}

// TestNegativePRNumberIsRejected: a number that cannot exist is a caller
// bug, reported before any read.
func TestNegativePRNumberIsRejected(t *testing.T) {
	_, srv := newStub(t)
	if _, err := clientFor(srv, "t").Facts(context.Background(), Request{Slug: "acme/widgets", PR: -1}); err == nil {
		t.Fatal("a negative PR number was accepted")
	}
}

// TestParsePullURL is how a run's pr_url supplies the repository identity.
func TestParsePullURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		slug    string
		number  int
		wantErr Kind
	}{
		{name: "plain", raw: "https://github.com/jacobsims214/amp/pull/114", slug: "jacobsims214/amp", number: 114},
		{name: "www host", raw: "https://www.github.com/acme/widgets/pull/7", slug: "acme/widgets", number: 7},
		{name: "trailing slash", raw: "https://github.com/acme/widgets/pull/7/", slug: "acme/widgets", number: 7},
		{name: "surrounding space", raw: "  https://github.com/acme/widgets/pull/7  ", slug: "acme/widgets", number: 7},
		{name: "not github", raw: "https://gitlab.com/acme/widgets/pull/7", wantErr: KindUnsupportedHost},
		{name: "enterprise host", raw: "https://ghe.internal/acme/widgets/pull/7", wantErr: KindUnsupportedHost},
		{name: "not a pull url", raw: "https://github.com/acme/widgets", wantErr: KindInvalid},
		{name: "an issue, not a pull", raw: "https://github.com/acme/widgets/issues/7", wantErr: KindInvalid},
		{name: "not a number", raw: "https://github.com/acme/widgets/pull/abc", wantErr: KindInvalid},
		{name: "zero", raw: "https://github.com/acme/widgets/pull/0", wantErr: KindInvalid},
		{name: "empty", raw: "", wantErr: KindInvalid},
		{name: "garbage", raw: "not a url at all", wantErr: KindInvalid},
		{name: "no scheme", raw: "github.com/acme/widgets/pull/7", wantErr: KindInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slug, number, err := ParsePullURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePullURL(%q) = %q #%d, want %q", tt.raw, slug, number, tt.wantErr)
				}
				if err.Kind != tt.wantErr {
					t.Fatalf("Kind = %q, want %q", err.Kind, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePullURL(%q): %v", tt.raw, err)
			}
			if slug != tt.slug || number != tt.number {
				t.Fatalf("= %q #%d, want %q #%d", slug, number, tt.slug, tt.number)
			}
		})
	}
}

// TestEmptyRulesAreNotNoRules pins the distinction the policy turns on: the
// rules read answering with [] means "nothing applies" only when the rest of
// the facts are Complete, and PullRequestRules ignores rule types that never
// require a review.
func TestEmptyRulesAreNotNoRules(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/acme/widgets", 200, `{"default_branch":"trunk"}`)
	s.json("/repos/acme/widgets/rules/branches/trunk", 200, `[
		{"type":"deletion","ruleset_source":"trunk"},
		{"type":"required_status_checks","ruleset_source":"trunk"}
	]`)
	s.json("/repos/acme/widgets/branches/trunk/protection", 404, `{"message":"Branch not protected"}`)

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "acme/widgets", Target: "trunk"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if len(f.Rules.PullRequestRules()) != 0 {
		t.Fatalf("pull_request rules = %+v", f.Rules.PullRequestRules())
	}
	if f.Completeness == Complete {
		t.Fatal("a 404 on classic protection must not produce Complete facts")
	}
	// With the classic endpoint readable, the same empty answer is Complete
	// and the empty pull_request list becomes trustworthy.
	s2, srv2 := newStub(t)
	s2.json("/repos/acme/widgets", 200, `{"default_branch":"trunk"}`)
	s2.json("/repos/acme/widgets/rules/branches/trunk", 200, `[]`)
	s2.json("/repos/acme/widgets/branches/trunk/protection", 200, `{}`)
	f2, err := clientFor(srv2, "ghp_testtoken").Facts(context.Background(), Request{Slug: "acme/widgets", Target: "trunk"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if f2.Completeness != Complete {
		t.Fatalf("Completeness = %q, want complete", f2.Completeness)
	}
	if f2.Rules.Protection.Protected == nil || !*f2.Rules.Protection.Protected {
		t.Errorf("protection = %+v", f2.Rules.Protection)
	}
	if f2.Rules.Protection.RequiredApprovingReviewCount != nil {
		t.Errorf("no required_pull_request_reviews setting, but count = %v", *f2.Rules.Protection.RequiredApprovingReviewCount)
	}
}

// TestFactsConcurrent proves the client holds no mutable state: parallel
// reads against the same client do not interfere.
func TestFactsConcurrent(t *testing.T) {
	s, srv := newStub(t)
	s.json("/repos/acme/widgets", 200, `{"default_branch":"main"}`)
	s.json("/repos/acme/widgets/pulls/1", 200, `{"base":{"ref":"feat/x"},"requested_reviewers":[],"requested_teams":[],"merged_at":null}`)
	s.json("/repos/acme/widgets/rules/branches/feat%2Fx", 200, rulesBody)
	s.json("/repos/acme/widgets/branches/feat%2Fx/protection", 200, `{}`)
	c := clientFor(srv, "ghp_testtoken")

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := c.Facts(context.Background(), Request{Slug: "acme/widgets", PR: 1})
			if err != nil {
				errs <- err
				return
			}
			if f.Completeness != Complete || f.Target != "feat/x" {
				errs <- fmt.Errorf("facts = %+v", f)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestRedirectIsReported: a 301 is a renamed repository, and following it
// would report another repository's facts under the old slug.
func TestRedirectIsReported(t *testing.T) {
	s, srv := newStub(t)
	s.on("/repos/old/name", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://api.github.com/repositories/42")
		w.WriteHeader(http.StatusMovedPermanently)
	})

	f, err := clientFor(srv, "ghp_testtoken").Facts(context.Background(), Request{Slug: "old/name", Target: "main"})
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	r, _ := f.ReadFor(OpRepo)
	if r.Err == nil || r.Err.Kind != KindMoved {
		t.Fatalf("repo error = %+v, want moved", r.Err)
	}
	for _, p := range s.requests() {
		if strings.Contains(p, "repositories/42") {
			t.Fatalf("the redirect was followed: %v", s.requests())
		}
	}
	if f.Completeness == Complete {
		t.Fatal("a moved repository completed the facts")
	}
}

// TestRuleParametersZeroValues pins that an unreadable parameters object
// leaves the rule's parameters at their zero value, which a classifier must
// not read as "0 reviews required".
func TestRuleParametersZeroValues(t *testing.T) {
	rule := Rule{Type: "pull_request", RulesetSource: "main"}
	if rule.Parameters.RequiredApprovingReviewCount != 0 || rule.Parameters.RequireCodeOwnerReview {
		t.Fatalf("zero RuleParameters are not the zero value: %+v", rule.Parameters)
	}
	rules := RulesFacts{Known: true, Rules: []Rule{rule, {Type: "deletion"}}}
	if got := len(rules.PullRequestRules()); got != 1 {
		t.Fatalf("PullRequestRules = %d, want 1", got)
	}
	if got := (RulesFacts{}).PullRequestRules(); len(got) != 0 {
		t.Fatalf("unknown rules yield %+v, want nothing", got)
	}
}
