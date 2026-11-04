package service

// The §3.3 fallback, through the pipeline that produces it (AMP #3219).
//
// The classifier's partial-evidence arm —
//
//	partial rules + GitHub says a review is required (or the PR is blocked)
//	               + the person is a requested reviewer      → MergeAlertRule
//
// — used to be reachable only from a hand-built MergeRuleFacts. Nothing in the
// real pipeline populated ReviewDecision or MergeableState: githubfacts.PullFacts
// had no fields for them, and the cached pull write never set them, so the
// projection copied two zero values into the classifier and the arm was dead
// code with live tests.
//
// The tests here are deliberately NOT unit tests of the classifier
// (merge_alert_test.go covers that) and NOT projection tests over hand-built
// views (merge_alert_projection_test.go covers that). They drive the real
// *githubfacts.Client* over a stubbed GitHub, through the real coordinator's
// write path, through mergeAlertInputFromView, into ClassifyMergeAlert — the
// chain a production read takes. If any link stops carrying the signal, these
// fail; that is the property the ticket exists to pin.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/githubfacts"
	"github.com/simstech/amp-api/internal/repository"
)

// pipelineGitHub is a stub GitHub that records every request, so a test can
// assert what was asked as well as what came back.
type pipelineGitHub struct {
	mu      sync.Mutex
	routes  map[string]pipelineAnswer // escaped path → status and JSON body
	paths   []string
	graphQL pipelineAnswer
}

type pipelineAnswer struct {
	status int
	body   string
}

func newPipelineGitHub(t *testing.T) (*pipelineGitHub, *httptest.Server) {
	t.Helper()
	g := &pipelineGitHub{routes: map[string]pipelineAnswer{}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

// answer registers a 200 with body.
func (g *pipelineGitHub) answer(path, body string) {
	g.answerStatus(path, http.StatusOK, body)
}

// answerStatus is for the answers whose STATUS carries the meaning — above all
// classic protection's 404, which is how GitHub says "not protected" and "you
// may not look" at once, and is what makes the rules read partial.
func (g *pipelineGitHub) answerStatus(path string, status int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.routes[path] = pipelineAnswer{status: status, body: body}
}

func (g *pipelineGitHub) answerGraphQL(body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.graphQL = pipelineAnswer{status: http.StatusOK, body: body}
}

func (g *pipelineGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	path := r.URL.EscapedPath()
	g.paths = append(g.paths, path)
	answer, ok := g.routes[path]
	if path == "/graphql" {
		answer, ok = g.graphQL, g.graphQL.body != ""
	}
	g.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok || answer.body == "" {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	}
	w.WriteHeader(answer.status)
	fmt.Fprint(w, answer.body)
}

// pipelineCoordinator wires the REAL reader into the real coordinator over an
// in-memory cache: the only fake is where the facts are stored, so the write
// path under test is the production one.
func pipelineCoordinator(t *testing.T, srv *httptest.Server, clock *clock) (*GitHubFactsCoordinator, *memGitHubFactStore) {
	t.Helper()
	store := newMemGitHubFactStore()
	reader := githubfacts.New(githubfacts.Options{
		BaseURL:   srv.URL,
		Transport: srv.Client().Transport,
		Token:     func(context.Context) (string, error) { return "ghp_pipeline", nil },
	})
	creds := &memGitHubFactCredentials{tokens: map[int]string{7: "ghp_pipeline"}}
	return coordinator(t, store, creds, reader, clock.now), store
}

// The stub's four REST answers plus the GraphQL one, for a PR targeting a
// non-default branch whose rules read is complete but whose classic protection
// is unreadable — the exact state §3.3 exists for.
const (
	pipelinePRJSON = `{
		"number": 12,
		"base": {"ref": "fix/integration"},
		"requested_reviewers": [{"login": "octocat"}],
		"requested_teams": [],
		"merged_at": null,
		"mergeable_state": %s
	}`
	pipelineRepoJSON = `{"full_name":"acme/rocket","default_branch":"main"}`
)

// pipelineRefresh performs one refresh through the production path and returns
// the alert the classifier reaches from the CACHED facts — not from the reader's
// answer — so the write path and the projection are both exercised.
func pipelineRefresh(t *testing.T, c *GitHubFactsCoordinator, person string) MergeAlert {
	t.Helper()
	ctx := context.Background()
	req := GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", PR: 12, Target: "fix/integration"}
	if _, _, err := c.Refresh(ctx, req); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	view, err := c.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	in := mergeAlertInputFromView(view, MergeAlertInput{Now: view.Repo.ReadAt})
	in.Person = MergeAlertPerson{Login: person}
	return ClassifyMergeAlert(in)
}

// TestMergeAlertPipelinePartialRulesFallBackToReviewDecision is the ticket's
// acceptance criterion end to end: with the rules read partial (classic
// protection unreadable) and GitHub's reviewDecision saying a review is
// required, the person GitHub requested is asked — through the real reader,
// the real write path and the real projection.
func TestMergeAlertPipelinePartialRulesFallBackToReviewDecision(t *testing.T) {
	gh, srv := newPipelineGitHub(t)
	gh.answer("/repos/acme/rocket", pipelineRepoJSON)
	gh.answer("/repos/acme/rocket/pulls/12", fmt.Sprintf(pipelinePRJSON, `"clean"`))
	gh.answer("/repos/acme/rocket/rules/branches/fix%2Fintegration", `[]`)
	// A member token's 404: "not protected" and "may not look" are the same
	// answer, so the rules read is partial.
	gh.answerStatus("/repos/acme/rocket/branches/fix%2Fintegration/protection", http.StatusNotFound, `{"message":"Branch not protected"}`)
	gh.answerGraphQL(`{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED"}}}}`)

	c, store := pipelineCoordinator(t, srv, newClock())
	// Prove the chain the ticket is about: the cached row carries the decision.
	if _, _, err := c.Refresh(context.Background(), GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", PR: 12}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	row, err := store.GetGitHubPullFacts(context.Background(), repository.GitHubFactsKey{ProjectID: 7, Slug: "acme/rocket"}, 12)
	if err != nil {
		t.Fatalf("cached pull facts: %v", err)
	}
	if row.ReviewDecision != "review_required" {
		t.Fatalf("cached review_decision = %q, want review_required: the write path dropped it", row.ReviewDecision)
	}
	if row.Completeness != repository.GitHubCompletenessComplete {
		t.Fatalf("cached completeness = %q, want complete (both pull reads answered)", row.Completeness)
	}

	alert := pipelineRefresh(t, c, "octocat")
	if alert.Kind != MergeAlertRule {
		t.Fatalf("alert = %q (%s), want rule: §3.3 is unreachable through the pipeline", alert.Kind, alert.Reason)
	}
	if !strings.Contains(alert.Reason, "could not be read completely") {
		t.Errorf("reason = %q, want the partial-rules wording", alert.Reason)
	}
	if alert.Rule == "" {
		t.Error("a rule alert must name what is required")
	}
}

// TestMergeAlertPipelinePartialRulesFallBackToBlockedState: the same shape, with
// the OTHER §3.3 signal — mergeable_state "blocked" from the REST pull body and
// no review decision at all. Blocked alone would not alert (required status
// checks block too), so this also pins that the requested reviewer is required
// for the arm.
func TestMergeAlertPipelinePartialRulesFallBackToBlockedState(t *testing.T) {
	gh, srv := newPipelineGitHub(t)
	gh.answer("/repos/acme/rocket", pipelineRepoJSON)
	gh.answer("/repos/acme/rocket/pulls/12", fmt.Sprintf(pipelinePRJSON, `"blocked"`))
	gh.answer("/repos/acme/rocket/rules/branches/fix%2Fintegration", `[]`)
	gh.answerStatus("/repos/acme/rocket/branches/fix%2Fintegration/protection", http.StatusNotFound, `{"message":"Branch not protected"}`)
	gh.answerGraphQL(`{"data":{"repository":{"pullRequest":{"reviewDecision":null}}}}`)

	c, _ := pipelineCoordinator(t, srv, newClock())

	alert := pipelineRefresh(t, c, "octocat")
	if alert.Kind != MergeAlertRule {
		t.Fatalf("alert = %q (%s), want rule from the blocked mergeable state", alert.Kind, alert.Reason)
	}

	// Somebody GitHub did not request is not asked: the blocked state proves a
	// review is required, not that THIS person must give it.
	c2, _ := pipelineCoordinator(t, srv, newClock())
	alert = pipelineRefresh(t, c2, "someone-else")
	if alert.Kind != MergeAlertUnknown {
		t.Fatalf("alert for an unrequested person = %q (%s), want unknown", alert.Kind, alert.Reason)
	}
}

// TestMergeAlertPipelineNoPositiveSignalStaysUnknown: §5's asymmetry through the
// pipeline. A partial rules read with a requested reviewer but NO positive
// signal — the decision read failed and the PR is clean — cannot prove a rule
// requires the person, so the item stays unknown rather than alerting or (worse)
// quieting to internal.
func TestMergeAlertPipelineNoPositiveSignalStaysUnknown(t *testing.T) {
	gh, srv := newPipelineGitHub(t)
	gh.answer("/repos/acme/rocket", pipelineRepoJSON)
	gh.answer("/repos/acme/rocket/pulls/12", fmt.Sprintf(pipelinePRJSON, `"clean"`))
	gh.answer("/repos/acme/rocket/rules/branches/fix%2Fintegration", `[]`)
	gh.answerStatus("/repos/acme/rocket/branches/fix%2Fintegration/protection", http.StatusNotFound, `{"message":"Branch not protected"}`)
	// GraphQL refuses: the decision is unknown, and nothing else is positive.
	gh.answerGraphQL(`{"errors":[{"message":"Resource not accessible by integration"}]}`)

	c, _ := pipelineCoordinator(t, srv, newClock())

	alert := pipelineRefresh(t, c, "octocat")
	if alert.Kind != MergeAlertUnknown {
		t.Fatalf("alert = %q (%s), want unknown: no positive signal may not alert", alert.Kind, alert.Reason)
	}
}

// TestMergeAlertPipelineCompleteRulesQuietRegardlessOfDecision pins that
// populating the decision did not break the OTHER direction: with rules that
// are complete and require nobody, a REVIEW_REQUIRED decision is a contradiction
// between two reads, which stays unknown — never internal (that would quiet a
// card on ambiguous evidence) and never a rule alert.
func TestMergeAlertPipelineCompleteRulesQuietRegardlessOfDecision(t *testing.T) {
	gh, srv := newPipelineGitHub(t)
	gh.answer("/repos/acme/rocket", pipelineRepoJSON)
	gh.answer("/repos/acme/rocket/pulls/12", fmt.Sprintf(pipelinePRJSON, `"clean"`))
	gh.answer("/repos/acme/rocket/rules/branches/fix%2Fintegration", `[]`)
	// Protection is readable and has no required reviews: the rules are
	// COMPLETE and require nobody.
	gh.answer("/repos/acme/rocket/branches/fix%2Fintegration/protection", `{}`)
	gh.answerGraphQL(`{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED"}}}}`)

	c, _ := pipelineCoordinator(t, srv, newClock())

	alert := pipelineRefresh(t, c, "octocat")
	if alert.Kind != MergeAlertUnknown {
		t.Fatalf("alert = %q (%s), want unknown: complete rules contradicting the decision are ambiguous", alert.Kind, alert.Reason)
	}

	// And with a decision GitHub says needs nothing, the same complete rules
	// quiet the item to internal — the positive knowledge to do so is the
	// rules read, which is what the policy says may quiet a card.
	gh2, srv2 := newPipelineGitHub(t)
	gh2.answer("/repos/acme/rocket", pipelineRepoJSON)
	gh2.answer("/repos/acme/rocket/pulls/12", fmt.Sprintf(pipelinePRJSON, `"clean"`))
	gh2.answer("/repos/acme/rocket/rules/branches/fix%2Fintegration", `[]`)
	gh2.answer("/repos/acme/rocket/branches/fix%2Fintegration/protection", `{}`)
	gh2.answerGraphQL(`{"data":{"repository":{"pullRequest":{"reviewDecision":"APPROVED"}}}}`)

	c2, _ := pipelineCoordinator(t, srv2, newClock())
	alert = pipelineRefresh(t, c2, "octocat")
	if alert.Kind != MergeAlertInternal {
		t.Fatalf("alert = %q (%s), want internal: complete rules requiring nobody", alert.Kind, alert.Reason)
	}
}

// TestMergeAlertPipelineProtectionUnreadableIsPartialThroughTheWritePath: the
// pull row's own completeness, which the decision read changed. With REST
// answered and GraphQL refused the row is Partial — verified, so its base and
// reviewers stay evidence — and the cached decision is "unknown", never a value
// the classifier could mistake for an approval state.
func TestMergeAlertPipelineProtectionUnreadableIsPartialThroughTheWritePath(t *testing.T) {
	gh, srv := newPipelineGitHub(t)
	gh.answer("/repos/acme/rocket", pipelineRepoJSON)
	gh.answer("/repos/acme/rocket/pulls/12", fmt.Sprintf(pipelinePRJSON, `"blocked"`))
	gh.answer("/repos/acme/rocket/rules/branches/fix%2Fintegration", `[]`)
	gh.answerStatus("/repos/acme/rocket/branches/fix%2Fintegration/protection", http.StatusNotFound, `{"message":"Branch not protected"}`)
	gh.answerGraphQL(`{"errors":[{"message":"Resource not accessible by integration"}]}`)

	c, store := pipelineCoordinator(t, srv, newClock())
	if _, _, err := c.Refresh(context.Background(), GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", PR: 12}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	row, err := store.GetGitHubPullFacts(context.Background(), repository.GitHubFactsKey{ProjectID: 7, Slug: "acme/rocket"}, 12)
	if err != nil {
		t.Fatalf("cached pull facts: %v", err)
	}
	if row.Completeness != repository.GitHubCompletenessPartial {
		t.Fatalf("completeness = %q, want partial: the decision read failed", row.Completeness)
	}
	if row.ReviewDecision != "unknown" {
		t.Fatalf("review_decision = %q, want unknown", row.ReviewDecision)
	}
	if row.MergeableState != "blocked" {
		t.Fatalf("mergeable_state = %q, want blocked: the REST read still answered", row.MergeableState)
	}
	if !row.Verified() {
		t.Fatal("a partial pull read must still be verified evidence about its own facts")
	}

	// And the blocked state alone still alerts, because it is evidence from the
	// REST read that did answer.
	alert := pipelineRefresh(t, c, "octocat")
	if alert.Kind != MergeAlertRule {
		t.Fatalf("alert = %q (%s), want rule from the REST mergeable state", alert.Kind, alert.Reason)
	}
}

// TestMergeAlertPipelineStaleDecisionIsNotEvidence: the pull row's TTL applies
// to the decision like every other pull fact. A stale row must not stand in for
// the rules read — §5 — even though the value is still sitting in the cache.
func TestMergeAlertPipelineStaleDecisionIsNotEvidence(t *testing.T) {
	gh, srv := newPipelineGitHub(t)
	gh.answer("/repos/acme/rocket", pipelineRepoJSON)
	gh.answer("/repos/acme/rocket/pulls/12", fmt.Sprintf(pipelinePRJSON, `"clean"`))
	gh.answer("/repos/acme/rocket/rules/branches/fix%2Fintegration", `[]`)
	gh.answerStatus("/repos/acme/rocket/branches/fix%2Fintegration/protection", http.StatusNotFound, `{"message":"Branch not protected"}`)
	gh.answerGraphQL(`{"data":{"repository":{"pullRequest":{"reviewDecision":"REVIEW_REQUIRED"}}}}`)

	clk := newClock()
	c, _ := pipelineCoordinator(t, srv, clk)
	ctx := context.Background()
	req := GitHubFactsRequest{ProjectID: 7, Slug: "acme/rocket", PR: 12}
	if _, _, err := c.Refresh(ctx, req); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Past the 60s pull TTL the row is no longer evidence, so §3.3 must not fire
	// on it; the item degrades to unknown instead.
	clk.advance(2 * time.Minute)
	view, err := c.Lookup(ctx, req)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if view.Pull.Trusted {
		t.Fatal("the pull facts are still trusted past their TTL")
	}
	in := mergeAlertInputFromView(view, MergeAlertInput{Now: clk.now()})
	in.Person = MergeAlertPerson{Login: "octocat"}
	if alert := ClassifyMergeAlert(in); alert.Kind != MergeAlertUnknown {
		t.Fatalf("alert = %q (%s), want unknown: stale PR facts are not evidence", alert.Kind, alert.Reason)
	}
}
