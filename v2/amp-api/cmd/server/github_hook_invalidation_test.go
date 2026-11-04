package main

// A GitHub delivery invalidates the repository facts it contradicts (AMP #3160).
//
// The webhook is the only thing that tells AMP's repository-facts cache that
// something changed on GitHub before its TTL expires. What this file has to prove
// is that it does so for exactly the delivery it should and never for one it
// should not: the signature is the only authentication on a public route, so a
// delivery that cannot prove it holds the project's secret must not be able to
// change a cached fact — and a delivery to one project's hook must not reach
// another project's copy of the same repository.
//
// These run against real Postgres through the real REST handler, because the
// thing under test is the wiring (handler → service → coordinator → SQL), not the
// shape of a method call.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/repository"
)

// seedPullFacts stores one verified pull read in the real cache, exactly as a
// successful refresh would (the row and its verification time are what make it
// evidence), and returns the key it stored it under.
func seedPullFacts(t *testing.T, repo *repository.Repo, projectID int, slug string, pr int, base string) repository.GitHubFactsKey {
	t.Helper()
	ctx := context.Background()
	key := repository.GitHubFactsKey{ProjectID: projectID, Slug: slug}
	now := time.Now().UTC()
	if _, err := repo.UpsertGitHubRepoFacts(ctx, key, repository.GitHubRepoFactsWrite{
		DefaultBranch:       "main",
		DefaultBranchSource: repository.GitHubBranchSourceREST,
		Completeness:        repository.GitHubCompletenessComplete,
		VerifiedAt:          &now,
	}); err != nil {
		t.Fatalf("seed repository facts: %v", err)
	}
	if _, err := repo.UpsertGitHubPullFacts(ctx, key, pr, repository.GitHubPullFactsWrite{
		BaseBranch:         base,
		State:              "unknown",
		RequestedReviewers: []string{"octocat"},
		Completeness:       repository.GitHubCompletenessComplete,
		VerifiedAt:         &now,
	}); err != nil {
		t.Fatalf("seed pull facts: %v", err)
	}
	return key
}

// pullFactsAbsent asserts the pull request's cached facts are gone (the invalidation
// happened) or still there (it did not).
func pullFactsAbsent(t *testing.T, repo *repository.Repo, key repository.GitHubFactsKey, pr int, want bool) {
	t.Helper()
	_, err := repo.GetGitHubPullFacts(context.Background(), key, pr)
	absent := err != nil
	if absent != want {
		if want {
			t.Fatalf("pull %s#%d is still cached: %v", key.Slug, pr, err)
		}
		t.Fatalf("pull %s#%d was invalidated, and must not have been: %v", key.Slug, pr, err)
	}
}

// repoFactsPresent asserts the repository row survived: a pull-request delivery
// says nothing about the default branch, so invalidating it would discard good
// evidence and make the 15m TTL meaningless.
func repoFactsPresent(t *testing.T, repo *repository.Repo, key repository.GitHubFactsKey) {
	t.Helper()
	f, err := repo.GetGitHubRepoFacts(context.Background(), key)
	if err != nil || f.DefaultBranch != "main" {
		t.Fatalf("the repository's default branch was invalidated by a pull-request delivery: %+v %v", f, err)
	}
}

// A valid delivery drops the facts it contradicts; a bad or missing signature
// changes nothing; a replay invalidates nothing twice; an unsupported event stays
// harmless.
func TestGitHubHookInvalidatesFacts(t *testing.T) {
	const slug = "acme/invalidate"
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	build, _ := mergeChain(t, w, "gh-invalidate")
	prURL := "https://github.com/" + slug + "/pull/7"
	finishWith(t, alice, build, map[string]any{"pr_url": prURL, "pushed": true})

	rig := newFactsRig(t, w.s.repo)
	key := seedPullFacts(t, w.s.repo, w.s.projectID, slug, 7, "feat/x")

	// The REST handler a deployment gets: the hook route, with the coordinator
	// wired the way main.go wires it.
	ra := newRestAsWith(t, w, func(h *api.RestHandler) { h.SetMergeFacts(rig.coord) })
	h := setUpGitHubHook(t, ra, w.s.projectID)

	payload := func(action string) map[string]any {
		return map[string]any{
			"action":     action,
			"number":     7,
			"repository": map[string]any{"full_name": slug},
			"pull_request": map[string]any{
				"number":           7,
				"html_url":         prURL,
				"merged":           false,
				"merge_commit_sha": "",
				"head":             map[string]any{"ref": "feat/x", "sha": sha("head-inv"), "repo": map[string]any{"full_name": slug}},
				"base":             map[string]any{"ref": "feat/x", "sha": "0000000", "repo": map[string]any{"full_name": slug}},
			},
		}
	}

	// A delivery that cannot prove it holds the secret changes nothing. This is
	// the whole authentication of a public route, so it is the first thing
	// checked: an unsigned delivery that dropped a fact would let anyone who can
	// reach the URL force AMP to re-read GitHub (and, with no credential, fall
	// back to unknown).
	for _, sig := range []string{"-", "sha256=" + strings.Repeat("ab", 32), "sha1=deadbeef"} {
		h.want(t, 401, "pull_request", "", sig, payload("edited"))
		pullFactsAbsent(t, w.s.repo, key, 7, false)
		repoFactsPresent(t, w.s.repo, key)
	}

	// An unsupported event is harmless: 204 and no cache change.
	if out := h.want(t, 204, "issues", "", "", map[string]any{"action": "opened"}); out["invalidated"] == true {
		t.Fatalf("an unsupported event invalidated facts: %v", out)
	}
	pullFactsAbsent(t, w.s.repo, key, 7, false)

	// A valid pull_request delivery — not just `closed`: an `edited` or
	// `review_requested` delivery changes exactly the target and reviewers the
	// classifier reads — drops the pull request's facts. `edited` is not an action
	// AMP records a merge for, so the answer is 204; the invalidation is the part
	// this test is about, and it still happens.
	h.want(t, 204, "pull_request", "", "", payload("edited"))
	pullFactsAbsent(t, w.s.repo, key, 7, true)
	// The default branch is NOT dropped: a pull-request event says nothing about it.
	repoFactsPresent(t, w.s.repo, key)

	// A replay of the same delivery id is a no-op even though the facts were
	// re-seeded in the meantime: GitHub retries, and AMP must not re-do work it
	// already did (nor report having done it).
	seedPullFacts(t, w.s.repo, w.s.projectID, slug, 7, "feat/x")
	id := uniq("inv-replay")
	h.want(t, 204, "pull_request", id, "", payload("edited"))
	seedPullFacts(t, w.s.repo, w.s.projectID, slug, 7, "feat/x")
	again := h.want(t, 200, "pull_request", id, "", payload("edited"))
	if again["replayed"] != true || again["invalidated"] == true {
		t.Fatalf("a replay invalidated facts again: %v", again)
	}
	pullFactsAbsent(t, w.s.repo, key, 7, false)
}

// A delivery to one project's hook never reaches another project's cached facts
// for the same repository: the facts key carries the project, so the two copies
// are independent. The other project's copy stays stale until its own TTL, which
// is the safe direction — stale, never wrong.
func TestGitHubHookInvalidationIsProjectScoped(t *testing.T) {
	const slug = "acme/shared"
	w1 := newRBACWorld(t)
	w2 := newRBACWorld(t)
	rig1 := newFactsRig(t, w1.s.repo)
	ra := newRestAsWith(t, w2, func(h *api.RestHandler) { h.SetMergeFacts(rig1.coord) })

	// The same repository cached by both projects, and the same PR number.
	key1 := seedPullFacts(t, w1.s.repo, w1.s.projectID, slug, 9, "main")
	key2 := seedPullFacts(t, w2.s.repo, w2.s.projectID, slug, 9, "main")

	h := setUpGitHubHook(t, ra, w2.s.projectID)
	payload := map[string]any{
		"action":     "closed",
		"number":     9,
		"repository": map[string]any{"full_name": slug},
		"pull_request": map[string]any{
			"number":           9,
			"html_url":         "https://github.com/" + slug + "/pull/9",
			"merged":           true,
			"merge_commit_sha": sha("shared-merge"),
			"head":             map[string]any{"ref": "feat/shared", "sha": sha("shared-head"), "repo": map[string]any{"full_name": slug}},
			"base":             map[string]any{"ref": "main", "sha": "0000000", "repo": map[string]any{"full_name": slug}},
		},
	}
	h.want(t, 200, "pull_request", "", "", payload)
	pullFactsAbsent(t, w2.s.repo, key2, 9, true)  // its own: dropped
	pullFactsAbsent(t, w1.s.repo, key1, 9, false) // the other project's: untouched
}

// TestGitHubHookInvalidatesOnTheClosedDeliveryItAlsoActsOn: a `closed` delivery is
// the one event AMP does real work for (recording the merge), and it must still
// invalidate. This is the path where a mistake in ordering would hide — an
// invalidation placed after the recording reads identically for every 204 event,
// and only differs here, where the handler returns a body.
//
// Ordering itself (invalidate before acting) is a design choice recorded in the
// handler's comment, not pinned here: making the recording fail needs a database
// fault, and a guard that cannot fail is not worth the ceremony. What this test
// does pin is that a delivery with a real effect also drops its facts, and that
// the scope holds on that path — the repository row survives.
func TestGitHubHookInvalidatesOnTheClosedDeliveryItAlsoActsOn(t *testing.T) {
	const slug = "acme/invalidate-closed"
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	build, _ := mergeChain(t, w, "gh-inv-closed")
	prURL := "https://github.com/" + slug + "/pull/4"
	finishWith(t, alice, build, map[string]any{"pr_url": prURL, "pushed": true})

	rig := newFactsRig(t, w.s.repo)
	key := seedPullFacts(t, w.s.repo, w.s.projectID, slug, 4, "main")

	ra := newRestAsWith(t, w, func(h *api.RestHandler) { h.SetMergeFacts(rig.coord) })
	h := setUpGitHubHook(t, ra, w.s.projectID)

	payload := map[string]any{
		"action":     "closed",
		"number":     4,
		"repository": map[string]any{"full_name": slug},
		"pull_request": map[string]any{
			"number":           4,
			"html_url":         prURL,
			"merged":           true,
			"merge_commit_sha": sha("closed-merge"),
			"head":             map[string]any{"ref": "feat/c", "sha": sha("closed-head"), "repo": map[string]any{"full_name": slug}},
			"base":             map[string]any{"ref": "main", "sha": "0000000", "repo": map[string]any{"full_name": slug}},
		},
	}
	out := h.want(t, 200, "pull_request", "", "", payload)
	// The delivery did both of its jobs, and says so in its own result: the
	// recording is visible as a task id, and the invalidation as the clause (a
	// 204 has no body, so the delivery log is where an operator reads this).
	if out["task_ids"] == nil || !strings.Contains(fmt.Sprint(out["result"]), "dropped the cached repository facts") {
		t.Fatalf("a closed delivery did not both record and invalidate: %v", out)
	}
	if out["invalidated"] != true {
		t.Fatalf("Invalidated = %v, want true", out["invalidated"])
	}
	pullFactsAbsent(t, w.s.repo, key, 4, true)
	repoFactsPresent(t, w.s.repo, key)
}
