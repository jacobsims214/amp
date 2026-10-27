package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// Merge gate shapes (AMP #2596): integration branches and reviews of open
// PRs don't hold their dependents on a merge to main.

func setStoryMeta(t *testing.T, s *mcpStack, storyID int, meta map[string]any) {
	t.Helper()
	s.call(t, "amp_update_story", map[string]any{"story_id": float64(storyID), "metadata": meta})
}

func setTaskMeta(t *testing.T, s *mcpStack, taskID int, meta map[string]any) {
	t.Helper()
	s.call(t, "amp_update_task", map[string]any{"task_id": float64(taskID), "metadata": meta})
}

// An integration chain: work merged into (or PR'd to) the story's
// integration branch releases the next ticket on completion and stays
// unmerged; the ticket that opens the integration branch's PR to main still
// holds its dependents. Reporting the integration branch merged marks the
// whole chain merged. Setting the metadata re-plans nothing.
func TestMergeGate_IntegrationBranchChain(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	url := tokenMCPServer(t, w)
	ib := uniq("harness-m5")

	a := mkIn(t, alice, w.s.storyID, "A: pushed into the integration branch")
	b := mkIn(t, alice, w.s.storyID, "B: PR into the integration branch", a)
	c := mkIn(t, alice, w.s.storyID, "C: integration PR to main", a, b)
	d := mkIn(t, alice, w.s.storyID, "D: after main", c)

	before := map[int]any{}
	for _, id := range []int{a, b, c, d} {
		before[id] = planTask(t, alice, id)["planned_at"]
	}
	setStoryMeta(t, w.s.as(w.owner), w.s.storyID, map[string]any{domain.MetaIntegrationBranch: ib})
	for _, id := range []int{a, b, c, d} {
		if got := planTask(t, alice, id)["planned_at"]; fmt.Sprint(got) != fmt.Sprint(before[id]) {
			t.Fatalf("#%d planned_at %v -> %v: setting integration_branch re-planned it", id, before[id], got)
		}
	}
	if eff := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(a)})["effective_metadata"].(map[string]any); eff[domain.MetaIntegrationBranch] != ib {
		t.Fatalf("A effective_metadata = %v", eff)
	}

	// A merged its work into the integration branch and reported that branch.
	finishWith(t, alice, a, map[string]any{"branch": ib, "pushed": true})
	ta := planTask(t, alice, a)
	if ta["merge_state"] != domain.MergeNone || ta["merge_required"] != false {
		t.Fatalf("A after landing on %s = merge_state %v required %v, want none/false", ib, ta["merge_state"], ta["merge_required"])
	}
	if st := stateOf(t, alice, b); st != "backlog" {
		t.Fatalf("B after A = %s, want backlog (ready)", st)
	}
	if wd, _ := planTask(t, alice, b)["waiting_detail"].(string); wd != "" {
		t.Fatalf("B waiting_detail = %q", wd)
	}
	if !strings.Contains(unmergedIDs(t, alice), ib) {
		t.Fatalf("A missing from the unmerged queue: %s", unmergedIDs(t, alice))
	}

	// B opened a PR whose base is the integration branch.
	prB := "https://git.example/pr/" + uniq("b")
	finishWith(t, alice, b, map[string]any{"branch": uniq("feat/b"), "pr_url": prB, "base_branch": ib, "pushed": true})
	if ms := mergeStateOf(t, alice, b); ms != domain.MergeNone {
		t.Fatalf("B merge_state = %q, want none", ms)
	}
	if st := stateOf(t, alice, c); st != "backlog" {
		t.Fatalf("C after A and B = %s, want backlog", st)
	}

	// C opens the integration branch's PR to main: it gates as today.
	prC := "https://git.example/pr/" + uniq("c")
	finishWith(t, alice, c, map[string]any{"branch": ib, "pr_url": prC, "base_branch": "main", "pushed": true})
	if ms := mergeStateOf(t, alice, c); ms != domain.MergeNeeded {
		t.Fatalf("C (integration PR) merge_state = %q, want needed", ms)
	}
	td := planTask(t, alice, d)
	if td["state"] != "blocked" {
		t.Fatalf("D while the integration PR is open = %v, want blocked", td["state"])
	}
	if wd, _ := td["waiting_detail"].(string); !strings.Contains(wd, fmt.Sprintf("Waiting on merge of #%d", c)) {
		t.Fatalf("D waiting_detail = %q", wd)
	}

	// CI reports the integration branch merged: the whole chain is merged.
	tok := mintToken(t, ra, "carol", "me")
	out, isErr := tokenToolCall(t, url, tok, "amp_report_merged", map[string]any{"branch": ib})
	if isErr {
		t.Fatalf("report merged = %v", out)
	}
	for _, id := range []int{a, b, c} {
		if ms := mergeStateOf(t, alice, id); ms != domain.MergeMerged {
			t.Fatalf("#%d after the integration branch merged = %q, want merged (report %v)", id, ms, out)
		}
	}
	if st := stateOf(t, alice, d); st != "backlog" {
		t.Fatalf("D after the merge = %s, want backlog", st)
	}
	if um := unmergedIDs(t, alice); strings.Contains(um, prB) || strings.Contains(um, prC) {
		t.Fatalf("merged chain still unmerged: %s", um)
	}
}

// Without the key nothing changes: a run on a branch that happens to be
// named like an integration branch still gates.
func TestMergeGate_NoKeyGatesAsBefore(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	build, deploy := mergeChain(t, w, "nokey")
	finishWith(t, alice, build, map[string]any{"branch": uniq("harness"), "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("merge_state = %q, want needed", ms)
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy = %s, want blocked", st)
	}
}

// Setting integration_branch after the fact releases tickets whose work
// already landed there; they stay unmerged.
func TestMergeGate_IntegrationSetLaterReleases(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	ib := uniq("feat/human-work-ux")
	build, deploy := mergeChain(t, w, "later")
	other, otherDep := mergeChain(t, w, "later-other")
	finishWith(t, alice, build, map[string]any{"branch": ib, "pushed": true})
	finishWith(t, alice, other, map[string]any{"branch": uniq("feat/elsewhere"), "pushed": true})
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy before the key = %s, want blocked", st)
	}
	setStoryMeta(t, w.s.as(w.owner), w.s.storyID, map[string]any{domain.MetaIntegrationBranch: ib})
	tb := planTask(t, alice, build)
	if tb["merge_state"] != domain.MergeNone || tb["merge_required"] != false {
		t.Fatalf("build after integration_branch set = %v/%v, want none/false", tb["merge_state"], tb["merge_required"])
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after integration_branch set = %s, want backlog", st)
	}
	if !strings.Contains(unmergedIDs(t, alice), ib) {
		t.Fatalf("build left the unmerged queue: %s", unmergedIDs(t, alice))
	}
	// Work on another branch still waits.
	if ms := mergeStateOf(t, alice, other); ms != domain.MergeNeeded {
		t.Fatalf("other merge_state = %q, want needed", ms)
	}
	if st := stateOf(t, alice, otherDep); st != "blocked" {
		t.Fatalf("other's dependent = %s, want blocked", st)
	}
}

// A review ticket with waits_for_merge:false starts once the PR is up,
// while its sibling dependent still waits for the merge. Setting the key
// after the PR is up releases a waiting dependent at once; removing it
// makes it wait again. No re-planning.
func TestMergeGate_WaitsForMergeFalseReview(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	build := mkIn(t, alice, w.s.storyID, "build")
	review := mkIn(t, alice, w.s.storyID, "review the PR", build)
	deploy := mkIn(t, alice, w.s.storyID, "deploy", build)
	late := mkIn(t, alice, w.s.storyID, "late opt-out", build)

	planned := planTask(t, alice, review)["planned_at"]
	setTaskMeta(t, alice, review, map[string]any{domain.MetaWaitsForMerge: false})
	if got := planTask(t, alice, review)["planned_at"]; fmt.Sprint(got) != fmt.Sprint(planned) {
		t.Fatalf("review planned_at %v -> %v: waits_for_merge re-planned it", planned, got)
	}

	pr := "https://git.example/pr/" + uniq("review")
	finishWith(t, alice, build, map[string]any{"pr_url": pr, "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build merge_state = %q", ms)
	}
	tr := planTask(t, alice, review)
	if tr["state"] != "backlog" || tr["waiting_detail"] != nil && tr["waiting_detail"] != "" {
		t.Fatalf("review while the PR is open = %v (%v), want backlog", tr["state"], tr["waiting_detail"])
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy while the PR is open = %s, want blocked", st)
	}
	// The review can be started (dispatch re-checks dependencies).
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(review), "agent_id": "amp-github-reviewer"})
	if st := stateOf(t, alice, review); st != "in_progress" {
		t.Fatalf("review after dispatch = %s", st)
	}

	// Opting out later releases the ticket now; opting back in holds it.
	setTaskMeta(t, alice, late, map[string]any{domain.MetaWaitsForMerge: false})
	if st := stateOf(t, alice, late); st != "backlog" {
		t.Fatalf("late after opting out = %s, want backlog", st)
	}
	setTaskMeta(t, alice, late, map[string]any{domain.MetaWaitsForMerge: nil})
	if st := stateOf(t, alice, late); st != "blocked" {
		t.Fatalf("late after removing the opt-out = %s, want blocked", st)
	}

	// The merge releases the rest as before.
	ra.want(t, 200, "alice", "POST", fmt.Sprintf("/api/tasks/%d/merge", build), nil)
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the merge = %s, want backlog", st)
	}
}

// The webhook: a PR merged into a ticket's integration branch releases it
// (still unmerged), even when the agent didn't report base_branch.
func TestMergeGate_WebhookIntegrationBase(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	ib := uniq("harness-m6")
	setStoryMeta(t, w.s.as(w.owner), w.s.storyID, map[string]any{domain.MetaIntegrationBranch: ib})
	build, deploy := mergeChain(t, w, "wh")
	head := uniq("feat/wh")
	pr := "https://github.com/" + testRepo + "/pull/" + uniq("8")
	// Base not reported: the PR gates until GitHub says where it merged.
	finishWith(t, alice, build, map[string]any{"branch": head, "pr_url": pr, "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build merge_state = %q, want needed", ms)
	}
	h := setUpGitHubHook(t, ra, w.s.projectID)
	into := prClosed(true, pr, testRepo, head, sha("h-wh"), sha("m-wh"))
	into["pull_request"].(map[string]any)["base"].(map[string]any)["ref"] = ib
	out := h.want(t, 200, "pull_request", "", "", into)
	if fmt.Sprint(out["task_ids"]) != fmt.Sprintf("[%d]", build) {
		t.Fatalf("delivery = %v", out)
	}
	tb := planTask(t, alice, build)
	if tb["merge_state"] != domain.MergeNone || tb["merge_required"] != false {
		t.Fatalf("build after its PR merged into %s = %v/%v, want none/false", ib, tb["merge_state"], tb["merge_required"])
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy = %s, want backlog", st)
	}
	if !strings.Contains(unmergedIDs(t, alice), pr) {
		t.Fatalf("build left the unmerged queue")
	}
}
