package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// A done ticket's merge gate follows the latest run that put work up
// (AMP #3002, #2966): a later run that corrects where the work went —
// landing it on the ticket's integration branch — releases the dependents
// instead of leaving them waiting on a merge that doesn't exist. A real PR
// to main still gates, only the latest run decides, and a recorded merge is
// never undone.

// reportRun reports a run on a ticket that is already done (a correction
// arriving after the completion run, as #2966's did).
func reportRun(t *testing.T, s *mcpStack, id int, run map[string]any) {
	t.Helper()
	args := map[string]any{"task_id": float64(id), "outcome": domain.RunProgress}
	for k, v := range run {
		args[k] = v
	}
	s.call(t, "amp_report_run", args)
}

// The #2966 shape end to end: the completion run reports the worker's own
// worktree branch (nothing landed there), the gate holds the dependent, and
// a corrective run that says the work went into the integration branch
// releases it — still unmerged, because it is not on the main line yet.
func TestMergeProvenance_LateIntegrationCorrectionReleases(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	ib := uniq("fix/actionable-merge-workflow")
	setStoryMeta(t, w.s.as(w.owner), w.s.storyID, map[string]any{domain.MetaIntegrationBranch: ib})
	build, deploy := mergeChain(t, w, "corrected")

	// The completion run carries the worktree's own branch, pushed.
	worktree := uniq("amp-chain")
	finishWith(t, alice, build, map[string]any{"branch": worktree, "commit": "e39b8fc", "pushed": true})
	b := planTask(t, alice, build)
	if b["merge_state"] != domain.MergeNeeded || b["merge_required"] != true {
		t.Fatalf("build after the worktree-branch completion = %v/%v, want needed/true", b["merge_state"], b["merge_required"])
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy before the correction = %s, want blocked", st)
	}

	// The corrective run: the work went into the integration branch.
	reportRun(t, alice, build, map[string]any{"branch": ib, "commit": "3dea037", "base_branch": ib, "pushed": true})
	after := planTask(t, alice, build)
	if after["merge_state"] != domain.MergeNone || after["merge_required"] != false {
		t.Fatalf("build after the corrective run = %v/%v, want none/false", after["merge_state"], after["merge_required"])
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the corrective run = %s, want backlog", st)
	}
	// Released, not merged: the integration branch hasn't reached main.
	if um := unmergedIDs(t, alice); !strings.Contains(um, ib) {
		t.Fatalf("build left the unmerged queue after a correction: %s", um)
	}
	// Nor does the ticket start waiting for the worktree branch again.
	if wd, _ := planTask(t, alice, deploy)["waiting_detail"].(string); wd != "" {
		t.Fatalf("deploy waiting_detail = %q, want none", wd)
	}
	// The same correction again changes nothing.
	reportRun(t, alice, build, map[string]any{"branch": ib, "commit": "3dea037", "base_branch": ib, "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNone {
		t.Fatalf("build after repeating the correction = %q, want none", ms)
	}
}

// Several runs on one ticket: the latest run that put work up decides.
// After the correction released the gate, a later PR to main on the same
// ticket gates again (there is a real merge to make); a later integration
// run releases it again.
func TestMergeProvenance_LatestRunDecides(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	ib := uniq("harness-provenance")
	setStoryMeta(t, w.s.as(w.owner), w.s.storyID, map[string]any{domain.MetaIntegrationBranch: ib})
	build, deploy := mergeChain(t, w, "latest")

	finishWith(t, alice, build, map[string]any{"branch": ib, "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNone {
		t.Fatalf("build landed on %s = %q, want none", ib, ms)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the landing = %s, want backlog", st)
	}

	// A later run opens the integration branch's own PR to main: that merge
	// is real, so the dependent waits again.
	pr := "https://git.example/pr/" + uniq("main")
	reportRun(t, alice, build, map[string]any{"branch": ib, "pr_url": pr, "base_branch": "main", "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build after its PR to main = %q, want needed", ms)
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy while the PR to main is open = %s, want blocked", st)
	}

	// A further run corrects the record back to the integration landing (the
	// PR was withdrawn): the gate is released again, still unmerged.
	reportRun(t, alice, build, map[string]any{"branch": ib, "base_branch": ib, "pushed": true, "run_note": "PR withdrawn; work is on the integration branch"})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNone {
		t.Fatalf("build after the second correction = %q, want none", ms)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the second correction = %s, want backlog", st)
	}
	if !strings.Contains(unmergedIDs(t, alice), ib) {
		t.Fatalf("build missing from the unmerged queue: %s", unmergedIDs(t, alice))
	}
}

// A recorded merge is never undone: not by a later run reporting the
// worktree branch, and not by one landing on the integration branch (the
// integration branch's merge is what reported the merge in the first place).
func TestMergeProvenance_MergedFirstWins(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	ib := uniq("harness-merged-first")
	setStoryMeta(t, w.s.as(w.owner), w.s.storyID, map[string]any{domain.MetaIntegrationBranch: ib})
	build, deploy := mergeChain(t, w, "mergedfirst")
	pr := "https://git.example/pr/" + uniq("mf")

	finishWith(t, alice, build, map[string]any{"branch": uniq("amp-mf"), "pr_url": pr, "base_branch": "main", "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build with a PR to main = %q, want needed", ms)
	}
	tok := mintToken(t, ra, "carol", "me")
	url := tokenMCPServer(t, w)
	if out, isErr := tokenToolCall(t, url, tok, "amp_report_merged", map[string]any{"pr_url": pr, "merge_commit": "abcdef1234567"}); isErr {
		t.Fatalf("report merged = %v", out)
	}
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeMerged {
		t.Fatalf("build after the merge = %q, want merged", ms)
	}

	// Later runs: the worktree branch, then an integration landing. Neither
	// touches the recorded merge.
	reportRun(t, alice, build, map[string]any{"branch": uniq("amp-mf2"), "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeMerged {
		t.Fatalf("build after a later worktree run = %q, want merged", ms)
	}
	reportRun(t, alice, build, map[string]any{"branch": ib, "base_branch": ib, "pushed": true})
	mf := planTask(t, alice, build)
	if mf["merge_state"] != domain.MergeMerged {
		t.Fatalf("build after a later integration run = %v, want merged", mf["merge_state"])
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy = %s, want backlog", st)
	}
}

// The release is one rule, reached two ways: a metadata edit (AMP #2596) and
// a corrective run (#3002). The same #2966 shape ends in the same state
// whichever arrives.
func TestMergeProvenance_MetadataAndRunParity(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	ib := uniq("harness-parity")
	owner := w.s.as(w.owner)

	// Chain A: corrected by a run report only. Asserted before any metadata
	// edit happens, so the release can only come from the run.
	setStoryMeta(t, owner, w.s.storyID, map[string]any{domain.MetaIntegrationBranch: ib})
	byRun, byRunDep := mergeChain(t, w, "parity-run")
	finishWith(t, alice, byRun, map[string]any{"branch": uniq("amp-pr"), "pushed": true})
	if ms := mergeStateOf(t, alice, byRun); ms != domain.MergeNeeded {
		t.Fatalf("chain A before the correction = %q, want needed", ms)
	}
	reportRun(t, alice, byRun, map[string]any{"branch": ib, "base_branch": ib, "pushed": true})
	assertReleased(t, alice, "released by a run", byRun, byRunDep)

	// Chain B: the same work, corrected by setting integration_branch after
	// the fact (its metadata was absent when it completed, so re-set it: the
	// metadata patch is what triggers reconciliation).
	byMeta, byMetaDep := mergeChain(t, w, "parity-meta")
	byMetaIB := uniq("harness-parity-late")
	finishWith(t, alice, byMeta, map[string]any{"branch": byMetaIB, "pushed": true})
	if ms := mergeStateOf(t, alice, byMeta); ms != domain.MergeNeeded {
		t.Fatalf("chain B before its integration_branch = %q, want needed", ms)
	}
	setTaskMeta(t, alice, byMeta, map[string]any{domain.MetaIntegrationBranch: byMetaIB})
	assertReleased(t, alice, "released by metadata", byMeta, byMetaDep)

	// Both are still unmerged: neither is on the main line.
	for _, c := range []struct {
		why string
		id  int
	}{{"run", byRun}, {"metadata", byMeta}} {
		if um := unmergedIDs(t, alice); !strings.Contains(um, fmt.Sprintf("task_id:%d ", c.id)) {
			t.Fatalf("%s: #%d left the unmerged queue: %s", c.why, c.id, um)
		}
	}
}

// assertReleased checks one #2966-shaped ticket and its dependent: the gate
// is gone, the ticket is still unmerged, and the dependent is ready.
func assertReleased(t *testing.T, s *mcpStack, why string, id, dep int) {
	t.Helper()
	if ms := mergeStateOf(t, s, id); ms != domain.MergeNone {
		t.Fatalf("%s: #%d merge_state = %q, want none", why, id, ms)
	}
	if req := planTask(t, s, id)["merge_required"]; req != false {
		t.Fatalf("%s: #%d merge_required = %v, want false", why, id, req)
	}
	if st := stateOf(t, s, dep); st != "backlog" {
		t.Fatalf("%s: dependent #%d = %s, want backlog", why, dep, st)
	}
}

// No integration branch: a corrective run has nothing to correct to, and a
// PR to main on such a ticket still gates (the ordinary case, pinned so the
// release path can't reach it).
func TestMergeProvenance_NoIntegrationBranchStillGates(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	build, deploy := mergeChain(t, w, "noprovenance")
	pr := "https://git.example/pr/" + uniq("np")
	finishWith(t, alice, build, map[string]any{"pr_url": pr, "base_branch": "main", "pushed": true})
	reportRun(t, alice, build, map[string]any{"branch": "main", "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build = %q, want needed", ms)
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy = %s, want blocked", st)
	}
	if um := unmergedIDs(t, alice); !strings.Contains(um, fmt.Sprintf("task_id:%d ", build)) {
		t.Fatalf("build missing from the unmerged queue: %s", um)
	}
}
