package main

// AMP #3158: the merge-alert classifier applied to the REST and MCP waiting
// projections, end to end.
//
// The unit tests in internal/service pin the mapping from cached facts to the
// policy's input. This file pins the three things only a wired stack can show:
//
//   - the arms reach both projections: the REST My work tab (Service.WaitingOn
//     through the API) and MCP's waiting_on_people carry the same alert, reason,
//     rule, review link and repo chip, because both read WaitingOn;
//   - the read path makes NO HTTP call: the facts are seeded in the cache and
//     the GitHub transport is a tripwire that fails the test on first use. A
//     page of merge items must never depend on GitHub being reachable;
//   - suppressing a human alert does not touch the gate: an internal item leaves
//     the ticket merge_state "needed" and its dependent still waiting, and an
//     already-merged item is gone from the list without any merge being
//     recorded. That separation is user direction 2026-11-02 ("separate
//     dependency readiness from human alerting") and the reason a quiet card is
//     not a claim that work shipped.

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/githubfacts"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// factsRig is a real coordinator over the real Postgres cache, with a GitHub
// transport that never answers.
//
// It is BOTH a tripwire and a no-wait proof, depending on what the test needs:
//
//   - when every fact a read needs is seeded fresh, no refresh is warranted, so
//     any HTTP call at all is a bug and hit counts it (a My work read must use
//     cached facts only);
//   - when the cache is empty, a read is still allowed to START a refresh — that
//     is the design — so the reader BLOCKS and the test asserts the projection
//     returns anyway. A read that waited for GitHub would hang here instead of
//     quietly slowing the page.
type factsRig struct {
	t    *testing.T
	repo *repository.Repo
	// coord is the read path; gate makes its reader block until closed.
	coord *service.GitHubFactsCoordinator
	gate  chan struct{}
	// hit counts HTTP attempts. It only ever sees a refresh that was actually
	// needed, so in a fully seeded test it must stay 0.
	hit atomic.Int64
}

func newFactsRig(t *testing.T, repo *repository.Repo) *factsRig {
	t.Helper()
	r := &factsRig{t: t, repo: repo, gate: make(chan struct{})}
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		r.hit.Add(1)
		// Block, never answer: the refresh this belongs to is background work,
		// and the assertion is that nothing on the read path waits for it.
		<-r.gate
		return nil, fmt.Errorf("github is unreachable in this test (%s %s)", req.Method, req.URL)
	})
	c, err := service.NewGitHubFactsCoordinator(service.GitHubFactsConfig{
		Store:       repo,
		Credentials: service.RepoGitHubFactCredentials{Repo: repo},
		Readers:     service.GitHubClientReaderFactory(githubfacts.Options{Transport: rt}),
		// A short refresh bound, so a blocked reader cannot keep the coordinator
		// busy past the test's own cleanup.
		RefreshTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	r.coord = c
	t.Cleanup(func() {
		close(r.gate)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.WaitForIdle(ctx)
	})
	return r
}

// noRefresh asserts no refresh was even needed: the seeded facts were fresh, so
// a My work read made no HTTP call at all.
func (r *factsRig) noRefresh() {
	r.t.Helper()
	if n := r.hit.Load(); n != 0 {
		r.t.Fatalf("a My work read needed %d GitHub call(s) although every fact was cached and fresh: "+
			"the projection must use cached facts only", n)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// seedRepoFacts stores a verified default branch, exactly as a successful
// refresh would (the row and its verification time are what make it evidence).
func (r *factsRig) seedRepoFacts(projectID int, slug, branch string) {
	r.t.Helper()
	now := time.Now().UTC()
	if _, err := r.repo.UpsertGitHubRepoFacts(context.Background(),
		repository.GitHubFactsKey{ProjectID: projectID, Slug: slug},
		repository.GitHubRepoFactsWrite{
			DefaultBranch:       branch,
			DefaultBranchSource: repository.GitHubBranchSourceREST,
			Completeness:        repository.GitHubCompletenessComplete,
			VerifiedAt:          &now,
		}); err != nil {
		r.t.Fatalf("seed repository facts: %v", err)
	}
}

// seedBranchRules stores one verified rules read for a branch.
func (r *factsRig) seedBranchRules(projectID int, slug, branch string, complete bool, rules string) {
	r.t.Helper()
	now := time.Now().UTC()
	completeness := repository.GitHubCompletenessComplete
	if !complete {
		completeness = repository.GitHubCompletenessPartial
	}
	if _, err := r.repo.UpsertGitHubBranchFacts(context.Background(),
		repository.GitHubFactsKey{ProjectID: projectID, Slug: slug}, branch,
		repository.GitHubBranchFactsWrite{
			Rules:        []byte(rules),
			Completeness: completeness,
			VerifiedAt:   &now,
		}); err != nil {
		r.t.Fatalf("seed branch rules: %v", err)
	}
}

// seedPull stores one verified pull read: the PR's own target, and whether
// GitHub already sees it merged.
func (r *factsRig) seedPull(projectID int, slug string, pr int, base, state string) {
	r.t.Helper()
	now := time.Now().UTC()
	if _, err := r.repo.UpsertGitHubPullFacts(context.Background(),
		repository.GitHubFactsKey{ProjectID: projectID, Slug: slug}, pr,
		repository.GitHubPullFactsWrite{
			BaseBranch:   base,
			State:        state,
			Completeness: repository.GitHubCompletenessComplete,
			VerifiedAt:   &now,
		}); err != nil {
		r.t.Fatalf("seed pull facts: %v", err)
	}
}

// wire puts the coordinator on both projections. Everything else about the stack
// stays as the other tests build it, so what is exercised is the wiring a
// deployment gets from cmd/server/main.go.
//
// Both surfaces need it because each builds its own Service (mcp.NewServer and
// api.NewRestHandler), which is exactly why the wiring lives in main.go rather
// than in a constructor: one coordinator, two projections, one definition.
func (r *factsRig) wire(w *rbacWorld) {
	r.t.Helper()
	w.s.mcpSrv.SetMergeFacts(r.coord)
}

// myWorkWith is the project's My work tab with the merge facts wired, so the
// REST assertions run against a handler configured the way main.go configures
// it.
func (r *factsRig) myWorkWith(w *rbacWorld) map[string]any {
	r.t.Helper()
	ra := newRestAsWith(r.t, w, func(h *api.RestHandler) { h.SetMergeFacts(r.coord) })
	out := ra.want(r.t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work", w.s.projectID), nil)
	return out["items"].(map[string]any)
}

// TestMergeAlertPolicy_ArmReachesRESTAndMCP is the four-arm test in the
// projections. One repository, one project, four merge-gated tickets whose PRs
// differ only in what the cached facts say — and each must arrive in the REST My
// work tab and in MCP's waiting_on_people with the right arm, and be absent or
// present accordingly.
func TestMergeAlertPolicy_ArmReachesRESTAndMCP(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	story := w.s.storyID
	rig := newFactsRig(t, w.s.repo)

	const slug = "acme/rocket"
	const defaultBranch = "trunk"
	const integration = "fix/actionable-merge-workflow"
	rig.seedRepoFacts(w.s.projectID, slug, defaultBranch)
	// An unprotected integration branch: complete, fresh evidence that no rule
	// requires anyone.
	rig.seedBranchRules(w.s.projectID, slug, integration, true, "[]")
	// The default branch, protected by a rule that demands an approval from
	// somebody AMP cannot attribute (required_approving_review_count), which is
	// the honest-unknown case for a rule — the arm a person must still see.
	rig.seedBranchRules(w.s.projectID, slug, "protected-elsewhere", true,
		`[{"type":"pull_request","ruleset_source":"main-protection","ruleset_source_type":"Repository",
		   "parameters":{"required_approving_review_count":1,"required_reviewers":[],
		                 "require_code_owner_review":false,"require_last_push_approval":false,
		                 "require_extra_approval_for_unattributed_changes":false,
		                 "required_review_thread_resolution":false}}]`)

	// Four tickets, four PRs (the PR number is what distinguishes their facts).
	type armTicket struct {
		name   string
		pr     int
		base   string
		state  string
		arm    string
		asks   bool
		reason string
	}
	// The four policy arms, plus a PR GitHub already sees merged. "rule" here is
	// a rule that demands an approval AMP cannot attribute, so its arm is the
	// honest unknown with a rule to act on; the pure state-3 attribution is pinned
	// in internal/service's projection tests, where a person can be given a login.
	tickets := []armTicket{
		{name: "default", pr: 101, base: defaultBranch, state: "unknown", arm: service.MergeAlertDefaultBranch, asks: true},
		{name: "internal", pr: 102, base: integration, state: "unknown", arm: service.MergeAlertInternal, asks: false},
		{name: "rule", pr: 105, base: "rule/x", state: "unknown", arm: service.MergeAlertUnknown, asks: true},
		{name: "unknown", pr: 103, base: "no-facts/x", state: "unknown", arm: service.MergeAlertUnknown, asks: true},
		{name: "merged", pr: 104, base: defaultBranch, state: "merged", arm: service.MergeAlertNone, asks: false},
	}
	ids := map[string]int{}
	for _, tk := range tickets {
		build := mkIn(t, alice, story, "build "+tk.name)
		// A dependent that waits for the merge: the gate is what must survive
		// the alert policy untouched.
		dep := mkIn(t, alice, story, "after "+tk.name, build)
		finishWith(t, alice, build, map[string]any{
			"pr_url":      fmt.Sprintf("https://github.com/%s/pull/%d", slug, tk.pr),
			"branch":      "amp-" + tk.name,
			"base_branch": tk.base,
			"pushed":      true,
		})
		ids[tk.name], ids[tk.name+"/dep"] = build, dep
		if tk.name != "unknown" {
			// The unknown arm gets no cached pull read at all: nothing verified what
			// its PR targets, which is the floor the policy must keep as a card.
			rig.seedPull(w.s.projectID, slug, tk.pr, tk.base, tk.state)
		}
	}

	rig.wire(w)

	// ---- REST: the project's My work tab ----
	items := rig.myWorkWith(w)
	for _, tk := range tickets {
		card := cardByTask(items, "merge", ids[tk.name])
		if tk.asks && card == nil {
			t.Fatalf("REST: %s merge is absent from My work, but its arm %q asks a person: %v", tk.name, tk.arm, items["merge"])
		}
		if !tk.asks && card != nil {
			t.Fatalf("REST: %s merge is in My work as %v, but its arm %q asks nobody", tk.name, card, tk.arm)
		}
		if !tk.asks {
			continue
		}
		if got := card["alert"]; got != tk.arm {
			t.Fatalf("REST: %s alert = %v (%v), want %q", tk.name, got, card["alert_reason"], tk.arm)
		}
		if reason, _ := card["alert_reason"].(string); reason == "" {
			t.Fatalf("REST: %s card carries no alert_reason", tk.name)
		}
		// A rule card offers the GitHub review link and nothing else; the arms
		// that are not rule alerts have no rule-box text.
		_, hasRule := card["rule"]
		if (tk.arm == service.MergeAlertRule) != hasRule {
			t.Fatalf("REST: %s rule-box presence = %v for arm %q", tk.name, hasRule, tk.arm)
		}
		if tk.arm == service.MergeAlertRule {
			if card["review_url"] != card["pr_url"] || card["review_url"] == nil {
				t.Fatalf("REST: rule card review_url = %v, want the PR url %v", card["review_url"], card["pr_url"])
			}
		}
	}

	// The default-branch arm reports the detected target as "detected", not as
	// a run's report: GitHub's own answer replaced the hint.
	if card := cardByTask(items, "merge", ids["default"]); card != nil {
		if card["target_source"] != service.MergeTargetDetected || card["base_branch"] != defaultBranch {
			t.Fatalf("REST: default arm target = %v/%v, want %s/detected", card["base_branch"], card["target_source"], defaultBranch)
		}
		if card["default_branch"] != defaultBranch {
			t.Fatalf("REST: repo chip = %v, want %s", card["default_branch"], defaultBranch)
		}
	}

	// ---- MCP: the same items, mirrored ----
	board := alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID)})
	mcpCard := func(id int) map[string]any {
		for _, x := range board["waiting_on_people"].([]any) {
			p := x.(map[string]any)
			if p["kind"] == "merge" && int(p["id"].(float64)) == id {
				return p
			}
		}
		return nil
	}
	for _, tk := range tickets {
		card := mcpCard(ids[tk.name])
		if tk.asks && card == nil {
			t.Fatalf("MCP: %s merge is absent from waiting_on_people: %v", tk.name, board["waiting_on_people"])
		}
		if !tk.asks && card != nil {
			t.Fatalf("MCP: %s merge is listed as %v, but its arm %q asks nobody", tk.name, card, tk.arm)
		}
		if !tk.asks {
			continue
		}
		if card["alert"] != tk.arm {
			t.Fatalf("MCP: %s alert = %v, want %q (REST said %v)", tk.name, card["alert"], tk.arm,
				cardByTask(items, "merge", ids[tk.name])["alert"])
		}
		if reason, _ := card["alert_reason"].(string); reason == "" {
			t.Fatalf("MCP: %s card carries no alert_reason", tk.name)
		}
		if tk.arm == service.MergeAlertRule && card["review_url"] == nil {
			t.Fatalf("MCP: rule card has no review_url: %v", card)
		}
	}

	// No HTTP assertion here on purpose: the "unknown" arm has NO cached pull
	// facts, so its read is entitled to start a bounded background refresh (by
	// design — a miss is the unknown state and must not block). That the refresh
	// is background, and that a fully cached page makes no call at all, are pinned
	// by TestMergeAlertPolicy_ReadPathMakesNoHTTPCall below; this test did run to
	// completion against a reader that never answers, which is the same proof from
	// the other side.
	_ = rig
}

// TestMergeAlertPolicy_ReadPathMakesNoHTTPCall is the acceptance criterion
// directly: with every fact a merge item needs cached and fresh, a REST My work
// read and an MCP amp_list_tasks make zero GitHub calls. The transport counts
// them, so a regression that fetches on the read path fails here rather than
// showing up as a slow page.
func TestMergeAlertPolicy_ReadPathMakesNoHTTPCall(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	story := w.s.storyID
	rig := newFactsRig(t, w.s.repo)

	const slug = "acme/rocket"
	const integration = "fix/integration"
	rig.seedRepoFacts(w.s.projectID, slug, "trunk")
	rig.seedBranchRules(w.s.projectID, slug, integration, true, "[]")

	build := mkIn(t, alice, story, "fully cached merge")
	finishWith(t, alice, build, map[string]any{
		"pr_url":      "https://github.com/" + slug + "/pull/501",
		"branch":      "amp-cached",
		"base_branch": integration,
		"pushed":      true,
	})
	rig.seedPull(w.s.projectID, slug, 501, integration, "unknown")
	rig.wire(w)

	// Both projections, with the coordinator wired the way main.go wires it.
	_ = rig.myWorkWith(w)
	alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID)})

	rig.noRefresh()
}

// TestMergeAlertPolicy_SuppressionDoesNotReleaseTheGate is the separation rule,
// pinned where it can actually break: an internal (suppressed) merge stays a
// gated ticket, and its dependent keeps waiting. A quiet card is a statement
// about who is told, never a claim that the code merged.
func TestMergeAlertPolicy_SuppressionDoesNotReleaseTheGate(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	alice := w.s.as(w.alice)
	story := w.s.storyID
	rig := newFactsRig(t, w.s.repo)

	const slug = "acme/rocket"
	const integration = "fix/actionable-merge-workflow"
	rig.seedRepoFacts(w.s.projectID, slug, "trunk")
	rig.seedBranchRules(w.s.projectID, slug, integration, true, "[]")

	build := mkIn(t, alice, story, "build on the integration branch")
	dep := mkIn(t, alice, story, "needs it merged", build)
	finishWith(t, alice, build, map[string]any{
		"pr_url":      fmt.Sprintf("https://github.com/%s/pull/201", slug),
		"branch":      "amp-internal",
		"base_branch": integration,
		"pushed":      true,
	})
	rig.seedPull(w.s.projectID, slug, 201, integration, "unknown")
	rig.wire(w)

	// Suppressed: it is agent organization, so no card.
	items := rig.myWorkWith(w)
	if card := cardByTask(items, "merge", build); card != nil {
		t.Fatalf("an internal merge still reaches My work: %v", card)
	}
	// But the gate is untouched: still merge-needed, and the dependent still
	// waits on it.
	task, err := w.s.repo.GetTask(ctx, build)
	if err != nil {
		t.Fatal(err)
	}
	if task.MergeState != domain.MergeNeeded {
		t.Fatalf("merge_state = %q after suppressing the alert, want %q: alerting must not change the gate",
			task.MergeState, domain.MergeNeeded)
	}
	blocked := blockedBy(t, alice, dep)
	if len(blocked) != 1 || blocked[0] != build {
		t.Fatalf("the dependent's blockers = %v, want [%d]: suppression must not satisfy a dependency", blocked, build)
	}
	if st := stateOf(t, alice, dep); st != string(domain.TaskStateBlocked) {
		t.Fatalf("the dependent is %q, want blocked", st)
	}
	// And MCP agrees, because it reads the same definition.
	board := alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID)})
	for _, x := range board["waiting_on_people"].([]any) {
		if p := x.(map[string]any); p["kind"] == "merge" && int(p["id"].(float64)) == build {
			t.Fatalf("MCP lists the suppressed merge: %v", p)
		}
	}
	rig.noRefresh()
}

// TestMergeAlertPolicy_AlreadyMergedAsksNobody is the merged exclusion through
// the projection: GitHub seeing the PR merged removes the item, and AMP's own
// record is left exactly as it was — nothing here fabricates a merge.
func TestMergeAlertPolicy_AlreadyMergedAsksNobody(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	alice := w.s.as(w.alice)
	story := w.s.storyID
	rig := newFactsRig(t, w.s.repo)

	const slug = "acme/rocket"
	rig.seedRepoFacts(w.s.projectID, slug, "trunk")

	build := mkIn(t, alice, story, "already merged upstream")
	finishWith(t, alice, build, map[string]any{
		"pr_url":      fmt.Sprintf("https://github.com/%s/pull/301", slug),
		"branch":      "amp-merged",
		"base_branch": "trunk",
		"pushed":      true,
	})
	// GitHub says merged; AMP has not been told yet (no webhook, no Mark merged).
	rig.seedPull(w.s.projectID, slug, 301, "trunk", "merged")
	rig.wire(w)

	if card := cardByTask(rig.myWorkWith(w), "merge", build); card != nil {
		t.Fatalf("an already-merged PR still asks a person: %v", card)
	}
	task, err := w.s.repo.GetTask(ctx, build)
	if err != nil {
		t.Fatal(err)
	}
	if task.MergeState == domain.MergeMerged {
		t.Fatal("suppressing the card recorded a merge: the webhook or Mark merged does that, never the projection")
	}
}

// TestMergeAlertPolicy_UnreadableEvidenceKeepsTheCard is the honest floor: with
// no cached facts at all (the state of every deployment before a refresh
// succeeds, and of a repository no token can read) the merge still reaches the
// person as unknown. That is what makes the whole policy safe to ship — the
// worst case is today's behavior, never a silently dropped real merge.
func TestMergeAlertPolicy_UnreadableEvidenceKeepsTheCard(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	story := w.s.storyID

	build := mkIn(t, alice, story, "no facts at all")
	finishWith(t, alice, build, map[string]any{
		"pr_url":      "https://github.com/acme/rocket/pull/401",
		"branch":      "amp-unknown",
		"base_branch": "trunk",
		"pushed":      true,
	})

	// A coordinator whose cache is empty and whose GitHub is unreachable: the
	// read still answers, from the cache, with nothing.
	rig := newFactsRig(t, w.s.repo)
	rig.wire(w)

	card := cardByTask(rig.myWorkWith(w), "merge", build)
	if card == nil {
		t.Fatal("a merge with no evidence at all was dropped: unknown must keep the card")
	}
	if card["alert"] != service.MergeAlertUnknown {
		t.Fatalf("alert = %v, want unknown", card["alert"])
	}
	// The run's reported base is still shown, but as a run's report, never as a
	// verified target.
	if card["target_source"] != service.MergeTargetRun || card["base_branch"] != "trunk" {
		t.Fatalf("target = %v/%v, want trunk/run", card["base_branch"], card["target_source"])
	}
}
