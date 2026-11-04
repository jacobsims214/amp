package main

// AMP #3011: the data contract of one truthful Merge card, as the approved
// mockup (artifact #27 v2) needs it. The card has to say four things AMP could
// previously only guess at, or could not say at all:
//
//   - where the merge goes: the run's reported target (base_branch), and where
//     that target came from. Nothing in amp-api knows a repository's default
//     branch, so *unknown* is an explicit answer here, never "main";
//   - who really waits: only dependents that did not opt out with
//     waits_for_merge: false. The old count included them, so a card said "1
//     ticket waits" about a ticket that had already started;
//   - whether the plan's merge step is due: readiness is computed by the same
//     guards the two bridges (AMP #3009) enforce, so a card never offers Mark
//     merged for a step the backend would keep open;
//   - one card per PR: a step folded into a merge card is not also a Do item.
//
// The assertions read the REST My work tab, which is fed by Service.WaitingOn —
// the same definition MCP's waiting_on_people and the badge counts use — so the
// contract is pinned where every view gets it from.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// waitsForMergeOff opts a dependent out of waiting for its dependencies'
// merges (AMP #2596), as an agent setting the metadata does.
func waitsForMergeOff(t *testing.T, w *rbacWorld, taskID int) {
	t.Helper()
	if _, err := w.s.repo.MergeMetadata(context.Background(), "task", taskID,
		domain.MetadataPatch{domain.MetaWaitsForMerge: false}); err != nil {
		t.Fatalf("waits_for_merge off on #%d: %v", taskID, err)
	}
}

// myWork is the project's My work tab as alice, by kind.
func myWork(t *testing.T, w *rbacWorld) map[string]any {
	t.Helper()
	out := newRestAs(t, w).want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work", w.s.projectID), nil)
	return out["items"].(map[string]any)
}

// reasonOf reads a fold's reason, which omitempty leaves out when it is empty.
func reasonOf(m map[string]any) string {
	s, _ := m["reason"].(string)
	return s
}

// idsOf reads a JSON array of ticket ids (absent or nil: none).
func idsOf(t *testing.T, v any) []int {
	t.Helper()
	rows, _ := v.([]any)
	out := make([]int, 0, len(rows))
	for _, x := range rows {
		out = append(out, int(x.(float64)))
	}
	return out
}

// cardByTask finds one ticket's item of a kind, or nil.
func cardByTask(items map[string]any, kind string, id int) map[string]any {
	rows, _ := items[kind].([]any)
	for _, x := range rows {
		m := x.(map[string]any)
		if int(m["task_id"].(float64)) == id {
			return m
		}
	}
	return nil
}

// A Merge card names the PR, its head and its target, and says where the
// target came from. A run that reported no target leaves base_branch empty and
// target_source "unknown": the card can then say it can't verify where this
// goes instead of implying "main".
func TestMergeCard_TargetAndSourceAreReportedNeverAssumed(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	alice := w.s.as(w.alice)
	story := w.s.storyID

	withTarget := mkIn(t, alice, story, "reports its target")
	silent := mkIn(t, alice, story, "reports no target")
	finishWith(t, alice, withTarget, map[string]any{
		"pr_url": "https://github.com/acme/amp/pull/88", "branch": "feat/reported", "base_branch": "trunk", "pushed": true,
	})
	finishWith(t, alice, silent, map[string]any{
		"pr_url": "https://github.com/acme/amp/pull/89", "branch": "feat/silent", "pushed": true,
	})

	items := myWork(t, w)
	reported := cardByTask(items, "merge", withTarget)
	if reported == nil {
		t.Fatalf("no Merge card for #%d: %v", withTarget, items["merge"])
	}
	if reported["base_branch"] != "trunk" || reported["target_source"] != "run" {
		t.Fatalf("reported target = %v/%v, want trunk/run", reported["base_branch"], reported["target_source"])
	}
	if reported["branch"] != "feat/reported" || reported["pr_url"] != "https://github.com/acme/amp/pull/88" {
		t.Fatalf("card route = %v → %v", reported["branch"], reported["pr_url"])
	}
	unknown := cardByTask(items, "merge", silent)
	if unknown == nil {
		t.Fatalf("no Merge card for #%d: %v", silent, items["merge"])
	}
	// Empty, never a fallback: a guessed "main" is exactly what the mockup's
	// state 9 forbids.
	if v, present := unknown["base_branch"]; present && v != "" {
		t.Fatalf("unreported target got a base_branch %v, want none", v)
	}
	if unknown["target_source"] != "unknown" {
		t.Fatalf("unreported target = %v, want unknown", unknown["target_source"])
	}

	// The repository read carries the same fact, so any other consumer agrees.
	refs, err := w.s.repo.LatestMergeRefs(ctx, []int{withTarget, silent})
	if err != nil {
		t.Fatal(err)
	}
	if got := refs[withTarget].BaseBranch; got != "trunk" {
		t.Fatalf("LatestMergeRefs base = %q, want trunk", got)
	}
	if got := refs[silent].BaseBranch; got != "" {
		t.Fatalf("LatestMergeRefs base = %q, want empty (unknown)", got)
	}
}

// holding_up counts only dependents that did not opt out; the opt-out is named
// in not_waiting so the card can say why it isn't counted. A card whose only
// dependent opted out therefore says nothing waits — which is honest, because
// that ticket started when the work completed.
func TestMergeCard_HoldingUpExcludesOptOutDependents(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	alice := w.s.as(w.alice)
	story := w.s.storyID

	build := mkIn(t, alice, story, "build")
	waiting := mkIn(t, alice, story, "after the merge", build)
	optOut := mkIn(t, alice, story, "review the PR", build)
	waitsForMergeOff(t, w, optOut)
	finishWith(t, alice, build, map[string]any{"pr_url": "https://github.com/acme/amp/pull/90", "pushed": true})

	card := cardByTask(myWork(t, w), "merge", build)
	if card == nil {
		t.Fatalf("no Merge card for #%d", build)
	}
	if got := idsOf(t, card["holding_up"]); !slices.Equal(got, []int{waiting}) {
		t.Fatalf("holding_up = %v, want [%d]", got, waiting)
	}
	if got := idsOf(t, card["not_waiting"]); !slices.Equal(got, []int{optOut}) {
		t.Fatalf("not_waiting = %v, want [%d]", got, optOut)
	}

	// The repository queries are the rule itself, so both sets appear in them.
	trueWaiters, err := w.s.repo.TrueWaiters(ctx, []int{build})
	if err != nil {
		t.Fatal(err)
	}
	if got := trueWaiters[build]; !slices.Equal(got, []int{waiting}) {
		t.Fatalf("TrueWaiters = %v, want [%d]", got, waiting)
	}
	all, err := w.s.repo.OpenDependents(ctx, []int{build})
	if err != nil {
		t.Fatal(err)
	}
	if len(all[build]) != 2 {
		t.Fatalf("OpenDependents = %v, want both dependents", all[build])
	}

	// A card with nothing but opt-outs says nothing waits on it.
	alone := mkIn(t, alice, story, "build alone")
	onlyOptOut := mkIn(t, alice, story, "reads it", alone)
	waitsForMergeOff(t, w, onlyOptOut)
	finishWith(t, alice, alone, map[string]any{"pr_url": "https://github.com/acme/amp/pull/91", "pushed": true})
	if c := cardByTask(myWork(t, w), "merge", alone); c != nil {
		if n := len(idsOf(t, c["holding_up"])); n != 0 {
			t.Fatalf("card whose only dependent opted out holds up %d tickets: %v", n, c["holding_up"])
		}
	}
}

// A plan's merge step (merge_for) is folded into the card of the PR it tracks,
// and its readiness is the backend's own rule: not ready while a prerequisite
// is open, ready once they are all satisfied — and the step is never also a Do
// item of the same list. Recording the merge then finishes the step, so nobody
// owes a second click.
func TestMergeCard_FoldedStepReadinessAndNoDuplicate(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	story := w.s.storyID
	const pr = "https://github.com/acme/amp/pull/92"

	res := alice.call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "build", "story_id": float64(story), "name": "build"},
		map[string]any{"ref": "review", "story_id": float64(story), "name": "review the code"},
		map[string]any{"ref": "merge", "story_id": float64(story), "name": "Merge PR: card",
			"performer": "human", "human_reason": "review", "owner": w.alice.Email,
			"depends_on": []any{"build", "review"}, "metadata": map[string]any{domain.MetaMergeFor: "build"}},
		map[string]any{"ref": "deploy", "story_id": float64(story), "name": "deploy", "depends_on": []any{"merge"}},
	}})
	ids := map[string]int{}
	for _, x := range res["created"].([]any) {
		m := x.(map[string]any)
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	build, review, step, deploy := ids["build"], ids["review"], ids["merge"], ids["deploy"]

	finishWith(t, alice, build, map[string]any{"pr_url": pr, "base_branch": "trunk", "pushed": true})

	// Ready is not a guess: a prerequisite is still open, so it is not.
	items := myWork(t, w)
	card := cardByTask(items, "merge", build)
	if card == nil {
		t.Fatalf("no Merge card for #%d", build)
	}
	tracked, ok := card["tracked_by"].(map[string]any)
	if !ok {
		t.Fatalf("Merge card tracks no step: %v", card)
	}
	if int(tracked["task_id"].(float64)) != step || tracked["ready"] != false {
		t.Fatalf("tracked_by = %v, want #%d not ready", tracked, step)
	}
	if got := idsOf(t, tracked["waiting_on"]); !slices.Equal(got, []int{review}) {
		t.Fatalf("tracked_by waiting_on = %v, want [%d]", got, review)
	}
	if reasonOf(tracked) == "" {
		t.Fatalf("a not-ready step gives no reason: %v", tracked)
	}
	// One card per PR: the step is folded in, so it is not a Do item as well.
	if also := cardByTask(items, "do", step); also != nil {
		t.Fatalf("step #%d is both folded into the Merge card and a Do item: %v", step, also)
	}
	// And the plan's sequencing still holds the work after the merge.
	if state := stateOf(t, alice, deploy); state == "completed" {
		t.Fatalf("deploy completed before the merge step")
	}

	// The prerequisite finishes: the same card is ready, because the backend
	// would now accept the completion.
	finishWith(t, alice, review, nil)
	card = cardByTask(myWork(t, w), "merge", build)
	if card == nil {
		t.Fatalf("Merge card vanished after the prerequisite finished")
	}
	tracked = card["tracked_by"].(map[string]any)
	if tracked["ready"] != true || reasonOf(tracked) != "" {
		t.Fatalf("tracked_by after its prerequisite = %v, want ready", tracked)
	}

	// Mark merged on the card finishes the step: one action, one outcome.
	w.s.as(w.alice).call(t, "amp_report_merged", map[string]any{"pr_url": pr})
	if state := stateOf(t, alice, step); state != "completed" {
		t.Fatalf("step state after the merge was recorded = %s, want completed", state)
	}
	if card := cardByTask(myWork(t, w), "merge", build); card != nil {
		t.Fatalf("the Merge card is still listed after its merge was recorded: %v", card)
	}
}

// A step its owner already started is theirs to finish, so the card must not
// offer the action the backend would refuse.
func TestMergeCard_StartedStepIsNotReady(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	build, step, _ := mergeStepChain(t, w, "started step", w.alice.Email)

	finishWith(t, alice, build, map[string]any{"pr_url": "https://github.com/acme/amp/pull/93", "pushed": true})
	// The person started their step (in production the task actor writes it).
	newRestAs(t, w).want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", step), nil)

	card := cardByTask(myWork(t, w), "merge", build)
	if card == nil {
		t.Fatalf("no Merge card for #%d", build)
	}
	tracked, ok := card["tracked_by"].(map[string]any)
	if !ok {
		t.Fatalf("Merge card tracks no step: %v", card)
	}
	if tracked["ready"] == true {
		t.Fatalf("a started step is offered as ready: %v", tracked)
	}
	if reasonOf(tracked) == "" {
		t.Fatalf("a started step gives no reason: %v", tracked)
	}
}

// An unlinked person's ticket is untouched: no fold into any card and no
// suppression, exactly the legacy behaviour AMP #3009 preserved.
func TestMergeCard_UnlinkedStepsUnchanged(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	build := mkIn(t, alice, w.s.storyID, "legacy build")
	legacy := mkHuman(t, alice, w.s.storyID, "Merge PR: legacy", w.alice.Email)
	finishWith(t, alice, build, map[string]any{"pr_url": "https://github.com/acme/amp/pull/94", "pushed": true})

	items := myWork(t, w)
	card := cardByTask(items, "merge", build)
	if card == nil {
		t.Fatalf("no Merge card for #%d", build)
	}
	if card["tracked_by"] != nil {
		t.Fatalf("an unlinked plan folded a step into the card: %v", card["tracked_by"])
	}
	if card["target_source"] != "unknown" {
		t.Fatalf("no run reported a target, so target_source = %v, want unknown", card["target_source"])
	}
	if cardByTask(items, "do", legacy) == nil {
		t.Fatalf("a legacy person's merge ticket disappeared from the list")
	}
}

// MCP's waiting_on_people carries the same contract as REST, from the same
// Service.WaitingOn: a card that only reached one of them would leave an agent
// and a person disagreeing about the same merge.
func TestMergeCard_MCPWaitingOnPeopleCarriesTheContract(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	story := w.s.storyID
	const pr = "https://github.com/acme/amp/pull/95"

	res := alice.call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "build", "story_id": float64(story), "name": "build",
			"metadata": map[string]any{}},
		map[string]any{"ref": "merge", "story_id": float64(story), "name": "Merge PR: mcp",
			"performer": "human", "human_reason": "review", "owner": w.alice.Email,
			"depends_on": []any{"build"}, "metadata": map[string]any{domain.MetaMergeFor: "build"}},
		map[string]any{"ref": "optout", "story_id": float64(story), "name": "reads the PR", "depends_on": []any{"build"}},
	}})
	ids := map[string]int{}
	for _, x := range res["created"].([]any) {
		m := x.(map[string]any)
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	waitsForMergeOff(t, w, ids["optout"])
	finishWith(t, alice, ids["build"], map[string]any{"pr_url": pr, "base_branch": "trunk", "pushed": true})

	board := alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID)})
	var card map[string]any
	for _, x := range board["waiting_on_people"].([]any) {
		p := x.(map[string]any)
		if p["kind"] == "merge" && int(p["id"].(float64)) == ids["build"] {
			card = p
		}
	}
	if card == nil {
		t.Fatalf("no merge item in waiting_on_people: %v", board["waiting_on_people"])
	}
	if card["base_branch"] != "trunk" || card["target_source"] != "run" || card["pr_url"] != pr {
		t.Fatalf("MCP merge card route = %v/%v/%v", card["base_branch"], card["target_source"], card["pr_url"])
	}
	// The opt-out is not counted, and is named instead.
	if got := idsOf(t, card["holding_up"]); len(got) != 0 {
		t.Fatalf("MCP holding_up = %v, want none (its only dependent opted out)", got)
	}
	if got := idsOf(t, card["not_waiting"]); !slices.Equal(got, []int{ids["optout"]}) {
		t.Fatalf("MCP not_waiting = %v, want [%d]", got, ids["optout"])
	}
	tracked, ok := card["tracked_by"].(map[string]any)
	if !ok {
		t.Fatalf("MCP merge card tracks no step: %v", card)
	}
	// Not ready: its own prerequisite is the PR's ticket, which is still
	// merge-needed — and that one is skipped, so it IS ready. Pinning the
	// exemption from the MCP side too.
	if int(tracked["task_id"].(float64)) != ids["merge"] || tracked["ready"] != true {
		t.Fatalf("MCP tracked_by = %v, want #%d ready (its only prerequisite is the tracked ticket)", tracked, ids["merge"])
	}
	// And a person's step folded into the card is not a separate do item.
	for _, x := range board["waiting_on_people"].([]any) {
		if p := x.(map[string]any); p["kind"] == "do" && int(p["id"].(float64)) == ids["merge"] {
			t.Fatalf("the folded step is also a do item in waiting_on_people: %v", p)
		}
	}
}
