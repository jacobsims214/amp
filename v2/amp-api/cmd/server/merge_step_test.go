package main

// AMP #3009: one merge journey per PR — a plan's human merge step linked to
// the ticket whose PR it tracks (merge_for), so the Merge card and the plan's
// step are one action rather than the duplicates of
// discoveries/merge-actions-user-journey.md.
//
// The contract these tests pin, in the ticket's own order:
//   - a linked PR action respects prerequisites: the step stays open while
//     something it waits on is unfinished, and the merge report says so — but
//     once that prerequisite is satisfied the step finishes by itself, so a
//     merge that already happened is never a click the person still owes;
//   - an external merge reconciles only the matching step: GitHub, CI or
//     Mark merged complete their own ticket's linked step and touch no
//     other ticket;
//   - linking the step records a merge only when the person said they
//     merged, so an approval or sign-off can't be dressed up as one;
//   - invalid and duplicate links are refused before anything is written,
//     and the database refuses a duplicate that two writers raced to make;
//   - a ticket with no link is untouched, and CheckPlan warns about
//     merge-looking person's steps that have none.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// mergeStepChain plans build (agent) → merge (a person's step tracking
// build's PR) → deploy (agent) in w's project: the shape the approved
// mockup's "tracked by #N" chip describes. merge_for is the batch ref
// "build", so the link is wired in the same amp_create_tasks call as the plan.
func mergeStepChain(t *testing.T, w *rbacWorld, name, owner string) (build, step, deploy int) {
	t.Helper()
	res := w.s.as(w.alice).call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "build", "story_id": float64(w.s.storyID), "name": name + " build"},
		map[string]any{"ref": "merge", "story_id": float64(w.s.storyID), "name": "Merge PR: " + name,
			"performer": "human", "human_reason": "review", "owner": owner, "depends_on": []any{"build"},
			"metadata": map[string]any{domain.MetaMergeFor: "build"}},
		map[string]any{"ref": "deploy", "story_id": float64(w.s.storyID), "name": name + " deploy", "depends_on": []any{"merge"}},
	}})
	ids := map[string]int{}
	for _, c := range res["created"].([]any) {
		m := c.(map[string]any)
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	return ids["build"], ids["merge"], ids["deploy"]
}

// mergeForOf is a ticket's own merge_for, as amp_get_task reports it.
func mergeForOf(t *testing.T, s *mcpStack, id int) int {
	t.Helper()
	md, _ := planTask(t, s, id)["metadata"].(map[string]any)
	if v, ok := md[domain.MetaMergeFor].(float64); ok {
		return int(v)
	}
	return 0
}

// mustTask reads a ticket from the repository, for a service call that needs
// it as the service sees it.
func mustTask(t *testing.T, w *rbacWorld, id int) *domain.Task {
	t.Helper()
	task, err := w.s.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("load task %d: %v", id, err)
	}
	return task
}

// linkStep sets (or, with nil, clears) a ticket's merge_for through the
// service, the way amp_update_task and the web UI do.
func linkStep(t *testing.T, w *rbacWorld, taskID int, v any) error {
	t.Helper()
	return service.New(w.s.repo, w.s.reg, w.s.kb).UpdateTask(context.Background(), uiPrincipal(w.alice),
		mustTask(t, w, taskID), domain.UpdateTaskRequest{Metadata: domain.MetadataPatch{domain.MetaMergeFor: v}})
}

// A merge recorded from outside (GitHub's webhook, CI, Mark merged) completes
// the step that tracks it and only that step: the step's dependents start,
// and an unlinked legacy merge ticket in the same plan is left as it was.
func TestMergeStep_ExternalMergeReconcilesOnlyItsOwnStep(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()

	build, step, deploy := mergeStepChain(t, w, "quotes", w.bob.Email)
	// A legacy merge ticket for the same ticket's merge, deliberately
	// unlinked: nothing auto-completes it (the "only the matching step" half).
	legacy := mkHuman(t, alice, w.s.storyID, "Merge the quotes PR (legacy)", w.bob.Email, build)

	pr := "https://github.example/acme/pull/" + uniq("step")
	finishWith(t, alice, build, map[string]any{"pr_url": pr, "base_branch": "main"})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build after a PR to main = %q, want needed", ms)
	}
	if st := stateOf(t, alice, step); st != "backlog" {
		t.Fatalf("the merge step = %s, want backlog (its one dependency is done)", st)
	}
	// Nothing after the step starts before the merge is recorded: that is
	// what makes the step a sequencing gate.
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy before the merge = %s, want blocked on the step", st)
	}

	// GitHub's webhook records the merge.
	recs, err := svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", pr, "", domain.MergeSourceGitHub)
	if err != nil || len(recs) != 1 {
		t.Fatalf("webhook report = %+v %v", recs, err)
	}
	if ls := recs[0].LinkedStep; ls == nil || !recs[0].LinkedStep.Completed || recs[0].LinkedStep.TaskID != step {
		t.Fatalf("the report's linked step = %+v", recs[0].LinkedStep)
	}
	b := planTask(t, alice, build)
	if b["merge_state"] != domain.MergeMerged || b["merge_source"] != domain.MergeSourceGitHub {
		t.Fatalf("build after the webhook = %v", b)
	}
	if st := stateOf(t, alice, step); st != "completed" {
		t.Fatalf("the linked merge step after the webhook = %s, want completed", st)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the linked step completed = %s, want backlog", st)
	}
	// The unlinked legacy ticket is ready (its dependency completed) but not
	// completed: the merge report reconciled only the linked step.
	if st := stateOf(t, alice, legacy); st != "backlog" {
		t.Fatalf("the unlinked legacy merge ticket = %s, want backlog (ready, not completed)", st)
	}
	// The step's history says the merge was recorded, not that its owner
	// signed off work they never did.
	hist := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(step), "include": []any{"history"}})
	h := fmt.Sprint(hist["history"])
	if !strings.Contains(h, "recorded by github") || strings.Contains(h, "Done by its owner") {
		t.Fatalf("step history = %s", h)
	}
	// And it records no run for the person: the step is not their work.
	runs := alice.call(t, "amp_get_task_runs", map[string]any{"task_id": float64(step)})
	if n := len(runs["runs"].([]any)); n != 0 {
		t.Fatalf("the auto-completed step has %d run(s); it should record none", n)
	}
}

// Prerequisites win over an early merge report: a linked step whose own
// dependency is still unfinished stays open, the report says why, and nothing
// after it starts. A later report, once the prerequisite is done, completes it.
func TestMergeStep_PrerequisitesHoldAgainstAMergeReport(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()

	build, deploy := mergeChain(t, w, "gated")
	// The person's step tracks build's merge, and waits on a review that is
	// deliberately never finished: the merge report must not close it.
	open := mkIn(t, alice, w.s.storyID, "an unfinished review")
	step := mkHuman(t, alice, w.s.storyID, "Merge PR: gated", w.bob.Email, build, open)
	deploy = mkIn(t, alice, w.s.storyID, "gated deploy", step)
	pr := "https://git.example/pr/" + uniq("gate")
	finishWith(t, alice, build, map[string]any{"pr_url": pr})
	if err := linkStep(t, w, step, float64(build)); err != nil {
		t.Fatalf("link the step: %v", err)
	}

	recs, err := svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", pr, "", domain.MergeSourceGitHub)
	if err != nil || len(recs) != 1 {
		t.Fatalf("merge report = %+v %v", recs, err)
	}
	ls := recs[0].LinkedStep
	if ls == nil || ls.Completed {
		t.Fatalf("the report closed a step whose prerequisite is unfinished: %+v", ls)
	}
	if !strings.Contains(ls.Message, fmt.Sprint(open)) {
		t.Fatalf("the report doesn't name what holds the step: %q", ls.Message)
	}
	if st := stateOf(t, alice, step); st == "completed" {
		t.Fatal("a merge report completed a step whose prerequisite is unfinished")
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy after an early merge report = %s, want blocked", st)
	}
	// The merge itself was still recorded: the report is about the code.
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeMerged {
		t.Fatalf("build = %q, want merged", ms)
	}

	// Once the prerequisite is done, the step finishes BY ITSELF: the merge is
	// already recorded, so nothing is left for the person to acknowledge. No
	// second report is sent — that is the whole point of the retrigger (a
	// merge that already happened must never come back as a stale click).
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(open), "agent_id": "amp-worker"})
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(open)})
	if st := stateOf(t, alice, step); st != "completed" {
		t.Fatalf("the step after its prerequisite finished = %s, want completed (the merge was already recorded)", st)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the step completed = %s, want backlog", st)
	}
	// Its history still names who recorded the merge, not the reconciliation.
	hist := fmt.Sprint(alice.call(t, "amp_get_task", map[string]any{"task_id": float64(step), "include": []any{"history"}})["history"])
	if !strings.Contains(hist, "recorded by github") || strings.Contains(hist, "Done by its owner") {
		t.Fatalf("step history = %s", hist)
	}
	// And a report that arrives after all that is a plain no-op: it finds the
	// step done, and changes nothing.
	recs, err = svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", pr, "", domain.MergeSourceGitHub)
	if err != nil {
		t.Fatalf("repeat report: %v", err)
	}
	if ls := recs[0].LinkedStep; ls == nil || !ls.AlreadyDone || ls.Completed {
		t.Fatalf("a repeat report should find the step already done: %+v", ls)
	}
}

// Invalid links are refused before anything is written; a duplicate link is
// refused naming the step that already has it; removing then relinking works.
func TestMergeStep_InvalidAndDuplicateLinks(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)

	build, deploy := mergeChain(t, w, "links")
	step := mkHuman(t, alice, w.s.storyID, "Merge PR: links", w.bob.Email, build)
	other := mkHuman(t, alice, w.s.storyID, "Merge PR: links again", w.bob.Email, build)
	agent := mkIn(t, alice, w.s.storyID, "an agent ticket")

	refuse := func(t *testing.T, taskID int, v any, want ...string) {
		t.Helper()
		err := linkStep(t, w, taskID, v)
		if err == nil {
			t.Fatalf("merge_for %v on #%d was accepted", v, taskID)
		}
		for _, sub := range want {
			if !strings.Contains(err.Error(), sub) {
				t.Fatalf("merging_for %v on #%d: error %q lacks %q", v, taskID, err, sub)
			}
		}
		if got := mergeForOf(t, alice, taskID); got != 0 {
			t.Fatalf("a refused link was stored anyway on #%d: %d", taskID, got)
		}
	}

	// Agent work can't be a merge step.
	refuse(t, agent, float64(build), "person's merge step")
	// build is agent work, so it can't track its own merge.
	refuse(t, build, float64(build), "agent work")
	// A person's ticket isn't a merge target.
	refuse(t, other, float64(step), "agent work")
	// A ticket that doesn't exist.
	refuse(t, step, float64(999999), "doesn't exist")
	// A link nothing could read: a stale ref, a negative id, a fraction.
	refuse(t, step, "the quotes PR", "merge_for must be a ticket id")
	refuse(t, step, float64(-3), "merge_for must be a ticket id")
	refuse(t, step, 1.5, "merge_for must be a ticket id")

	// The first valid link works.
	if err := linkStep(t, w, step, float64(build)); err != nil {
		t.Fatalf("link #%d to #%d: %v", step, build, err)
	}
	if got := mergeForOf(t, alice, step); got != build {
		t.Fatalf("merge_for = %d, want %d", got, build)
	}
	// A second step on the same merge is the duplicate the key removes:
	// refused, naming the step that already has it.
	refuse(t, other, float64(build), fmt.Sprintf("already tracked by ticket %d", step))
	// Resending the same link (an unrelated edit carrying it) isn't a
	// conflict with itself.
	if err := linkStep(t, w, step, float64(build)); err != nil {
		t.Fatalf("re-setting the same link: %v", err)
	}
	// null removes it; then the other step may take it.
	if err := linkStep(t, w, step, nil); err != nil {
		t.Fatalf("clear the link: %v", err)
	}
	if got := mergeForOf(t, alice, step); got != 0 {
		t.Fatalf("merge_for after removal = %d", got)
	}
	if err := linkStep(t, w, other, float64(build)); err != nil {
		t.Fatalf("relink after the first was removed: %v", err)
	}
	_ = deploy
}

// A person finishing their linked step records the merge as the same act (the
// direction #2978 said was missing), and a step can't be linked to a ticket
// whose merge is already recorded.
func TestMergeStep_FinishingTheStepRecordsTheMerge(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)

	build, step, deploy := mergeStepChain(t, w, "one-click", w.bob.Email)
	pr := "https://github.example/acme/pull/" + uniq("one")
	finishWith(t, alice, build, map[string]any{"pr_url": pr})
	if st := stateOf(t, alice, step); st != "backlog" {
		t.Fatalf("the step = %s, want backlog", st)
	}
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build before the step = %q, want needed", ms)
	}
	// bob does his step in the web UI: Start, then Mark done with the PR link
	// saying what he merged (the link is what makes it a merge, not an
	// acknowledgement of one).
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", step), nil)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", step), map[string]any{"note": "merged it", "pr_url": pr})

	if ms := mergeStateOf(t, alice, build); ms != domain.MergeMerged {
		t.Fatalf("build after the step was finished = %q, want merged", ms)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the step was finished = %s, want backlog", st)
	}
	// A step can't be linked to work whose merge is already recorded: it
	// would have nothing to gate. (The step that did the merging is done, so
	// it isn't a duplicate; the refusal is about the merge, not the step.)
	late := mkHuman(t, alice, w.s.storyID, "Merge PR: too late", w.bob.Email, build)
	if err := linkStep(t, w, late, float64(build)); err == nil || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("linking a step to a merged ticket: %v", err)
	}
}

// A ticket with no link is untouched: a merge report completes no unlinked
// step, and it edits nothing else about the ticket.
func TestMergeStep_LegacyTicketsUnchanged(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()

	build, deploy := mergeChain(t, w, "legacy")
	legacy := mkHuman(t, alice, w.s.storyID, "Merge PR: legacy", w.bob.Email, build)
	before := mustTask(t, w, legacy)

	pr := "https://git.example/pr/" + uniq("leg")
	finishWith(t, alice, build, map[string]any{"pr_url": pr})
	recs, err := svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", pr, "", domain.MergeSourceGitHub)
	if err != nil || len(recs) != 1 {
		t.Fatalf("merge report = %+v %v", recs, err)
	}
	if recs[0].LinkedStep != nil {
		t.Fatalf("an unlinked ticket's report claims a linked step: %+v", recs[0].LinkedStep)
	}
	after := mustTask(t, w, legacy)
	if after.State == domain.TaskStateCompleted || !after.PlannedAt.Equal(before.PlannedAt) || service.MergeFor(after) != 0 {
		t.Fatalf("an unlinked ticket changed: before %+v after %+v", before, after)
	}
	// It is ready (its dependency completed), which is the ordinary rule; the
	// point is that the merge report didn't complete it.
	if st := stateOf(t, alice, legacy); st != "backlog" {
		t.Fatalf("legacy = %s, want backlog (ready, not completed)", st)
	}
	_ = deploy
}

// CheckPlan warns about merge-looking person's steps with no link (the
// duplicate it can't otherwise see) and about two of them on the same work.
// Warnings don't make the plan NO-GO and nothing is edited.
func TestPlan_WarnsOnUnlinkedMergeSteps(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)

	build, linked, _ := mergeStepChain(t, w, "warned", w.bob.Email)
	// A second ticket whose merge nothing tracks yet, for the relink below.
	other, _ := mergeChain(t, w, "warned-other")
	// Two unlinked, merge-looking steps on the same ticket: the duplicate.
	first := mkHuman(t, alice, w.s.storyID, "Merge PR: the quotes PR", w.bob.Email, build)
	second := mkHuman(t, alice, w.s.storyID, "Review and merge the quotes PR", w.bob.Email, build)
	// A person's step that isn't a merge reads nothing like one: not flagged.
	// It depends on the chain so the plan has no isolated ticket (a blocking
	// issue, which would hide whether the warnings alone keep the plan GO).
	trip := mkHuman(t, alice, w.s.storyID, "Call the bank about the wire", w.bob.Email, build)

	kinds := reportFor(t, w, alice)
	unlinked, ok := kinds["unlinked_merge_step"]
	if !ok {
		t.Fatalf("no unlinked_merge_step warning: %v", kinds)
	}
	if unlinked["severity"] != "warn" {
		t.Fatalf("unlinked_merge_step severity = %v", unlinked["severity"])
	}
	flagged := idList(t, unlinked["task_ids"])
	for _, id := range []int{first, second} {
		if !hasID(flagged, id) {
			t.Fatalf("unlinked_merge_step misses #%d: %v", id, flagged)
		}
	}
	for _, id := range []int{linked, trip} {
		if hasID(idList(t, unlinked["task_ids"]), id) {
			t.Fatalf("unlinked_merge_step flags #%d, which is linked or isn't a merge: %v", id, unlinked["task_ids"])
		}
	}
	dup, ok := kinds["duplicate_merge_step"]
	if !ok {
		t.Fatalf("no duplicate_merge_step warning: %v", kinds)
	}
	dupIDs := idList(t, dup["task_ids"])
	if !hasID(dupIDs, first) || !hasID(dupIDs, second) {
		t.Fatalf("duplicate_merge_step = %v, want both #%d and #%d", dupIDs, first, second)
	}
	// Nothing was edited or deleted.
	if mergeForOf(t, alice, first) != 0 || mergeForOf(t, alice, second) != 0 {
		t.Fatal("CheckPlan linked an unlinked step")
	}
	// Linking one of them takes it out of the warning.
	if err := linkStep(t, w, first, float64(other)); err != nil {
		t.Fatalf("link the first: %v", err)
	}
	kinds = reportFor(t, w, alice)
	if u, ok := kinds["unlinked_merge_step"]; ok {
		if hasID(idList(t, u["task_ids"]), first) {
			t.Fatalf("a linked step is still flagged unlinked: %v", u["task_ids"])
		}
	}
}

// A merge step's dependencies sequence the merge, not just the report: while a
// prerequisite is unfinished the step isn't ready, and the plan reports it as a
// human gate carrying its merge_for link so My work can fold it into the PR's
// card.
func TestPlan_HumanGateCarriesMergeFor(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	build, step, _ := mergeStepChain(t, w, "gate-link", w.bob.Email)

	report := alice.call(t, "amp_check_plan", map[string]any{"project_id": float64(w.s.projectID)})
	for _, g := range report["human_gates"].([]any) {
		m := g.(map[string]any)
		if int(m["id"].(float64)) != step {
			continue
		}
		if int(m["merge_for"].(float64)) != build {
			t.Fatalf("the human gate's merge_for = %v, want %d", m["merge_for"], build)
		}
		return
	}
	t.Fatalf("the merge step isn't a human gate: %v", report["human_gates"])
}

// reportFor is amp_check_plan's issues by kind, failing if a warning alone
// made the plan NO-GO (or an issue about the test environment did).
func reportFor(t *testing.T, w *rbacWorld, s *mcpStack) map[string]map[string]any {
	t.Helper()
	report := s.call(t, "amp_check_plan", map[string]any{"project_id": float64(w.s.projectID)})
	kinds := map[string]map[string]any{}
	var blocking []string
	for _, iss := range report["issues"].([]any) {
		m := iss.(map[string]any)
		kind := m["kind"].(string)
		kinds[kind] = m
		if m["severity"] != "warn" && !envIssue(kind) {
			blocking = append(blocking, fmt.Sprintf("%s: %v (tickets %v)", kind, m["message"], m["task_ids"]))
		}
	}
	if len(blocking) > 0 {
		// Name them: a bare summary leaves the reader guessing which blocking
		// issue appeared, and these fixtures are sensitive to plan shape.
		t.Fatalf("a warning alone made the plan NO-GO; blocking issue(s): %s (%s)", strings.Join(blocking, "; "), report["summary"])
	}
	return kinds
}

// envIssue reports whether issue kind is about the test environment rather
// than the plan: no_context fires when the run has a live knowledge base
// (Typesense, which CI has and a plain local run may not) and the project's
// KB is empty, which every fixture here is. It is a real finding for a
// project and noise for a fixture, and checking for it would make these tests
// pass or fail on whether Typesense happens to be reachable.
func envIssue(kind string) bool { return kind == "no_context" }

// hasID reports whether ids has id.
func hasID(ids []int, id int) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// The reverse bridge is merge-specific, not "any completion of a linked step
// records a merge". A step whose person says nothing about a merge, or one
// linked to work whose merge is already recorded, records no merge: otherwise
// linking an approval, a deploy or a sign-off to a ticket would claim a merge
// nobody performed — the opposite of what the key is for.
func TestMergeStep_FinishingAStepOnlyRecordsTheMergeItOwes(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)

	build, step, _ := mergeStepChain(t, w, "truthful", w.bob.Email)
	pr := "https://github.example/acme/pull/" + uniq("truth")
	finishWith(t, alice, build, map[string]any{"pr_url": pr})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build before the step = %q, want needed", ms)
	}

	// bob finishes his step with a note but no PR link: he says the step is
	// done, not that he merged anything. The merge stays unrecorded, so the
	// ticket keeps waiting — nothing is claimed on his behalf.
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", step), nil)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", step), map[string]any{"note": "called it done"})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("a completion with no PR link recorded a merge: build = %q", ms)
	}

	// The PR link on the run is the person's own statement that they merged,
	// so it records the merge. (Reopening and finishing again is how the step's
	// owner corrects a completion, and the bridge still applies.)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/reopen", step), nil)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", step), nil)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", step), map[string]any{"note": "merged it after all", "pr_url": pr})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeMerged {
		t.Fatalf("build after a completion that names the PR = %q, want merged", ms)
	}
}

// Two steps reaching one merge is refused by the database as well as by the
// service check, so concurrent linkers converge on one winner instead of both
// storing the duplicate. The service reports it as the duplicate it is, not
// as a bare SQL error.
func TestMergeStep_DuplicateRefusedAtWrite(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	ctx := context.Background()

	build, _ := mergeChain(t, w, "race")
	first := mkHuman(t, alice, w.s.storyID, "Merge PR: race", w.bob.Email, build)
	second := mkHuman(t, alice, w.s.storyID, "Merge PR: race again", w.bob.Email, build)
	for _, id := range []int{first, second} {
		if err := linkStep(t, w, id, float64(build)); err != nil {
			t.Fatalf("link #%d: %v", id, err)
		}
		// Clear it again so the write path below starts from no link.
		if err := linkStep(t, w, id, nil); err != nil {
			t.Fatalf("clear #%d: %v", id, err)
		}
	}

	// Bypass the service check, as two requests that both passed it at the
	// same instant would: both metadata writes are attempted directly.
	if _, err := w.s.repo.MergeMetadata(ctx, "task", first, domain.MetadataPatch{domain.MetaMergeFor: float64(build)}); err != nil {
		t.Fatalf("first link: %v", err)
	}
	_, err := w.s.repo.MergeMetadata(ctx, "task", second, domain.MetadataPatch{domain.MetaMergeFor: float64(build)})
	if err == nil {
		t.Fatal("the database accepted two open steps tracking one merge")
	}
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("a duplicate link error = %v, want a repository conflict", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(first)) || !strings.Contains(err.Error(), "one step per merge") {
		t.Fatalf("the duplicate error doesn't name the step that holds it: %v", err)
	}
	// The loser's link was not stored.
	if got := mergeForOf(t, alice, second); got != 0 {
		t.Fatalf("a refused link was stored anyway on #%d: %d", second, got)
	}
	// Clearing the winner frees the link: a wrong link is always fixable.
	if err := linkStep(t, w, first, nil); err != nil {
		t.Fatalf("clear the winner: %v", err)
	}
	if err := linkStep(t, w, second, float64(build)); err != nil {
		t.Fatalf("relink after clearing: %v", err)
	}
}

// A step held by a prerequisite that is satisfied not by a completion but by
// its own merge being recorded, or by a state change to done, still finishes
// by itself: the merge the step tracks is the fact, and every way a
// prerequisite can become satisfied has to revisit it.
func TestMergeStep_HeldStepFinishesWhicheverWayThePrerequisiteFinishes(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()

	// review (agent, opens a PR, doesn't wait for merges) and step (tracks
	// build, waits on the review). The review opens its PR before build is
	// merged, which is how a review of an open PR works.
	build := mkIn(t, alice, w.s.storyID, "ways build")
	review := mkIn(t, alice, w.s.storyID, "ways review")
	// A review of an open PR doesn't wait for that PR's merge (AMP #2596).
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(review),
		"metadata": map[string]any{domain.MetaWaitsForMerge: false}})
	step := mkHuman(t, alice, w.s.storyID, "Merge PR: ways", w.bob.Email, build, review)
	deploy := mkIn(t, alice, w.s.storyID, "ways deploy", step)

	reviewPR := "https://git.example/pr/" + uniq("ways-review")
	finishWith(t, alice, review, map[string]any{"pr_url": reviewPR})
	buildPR := "https://git.example/pr/" + uniq("ways-build")
	finishWith(t, alice, build, map[string]any{"pr_url": buildPR})
	if err := linkStep(t, w, step, float64(build)); err != nil {
		t.Fatalf("link the step: %v", err)
	}

	// build's merge is recorded first: the step stays open on its review.
	recs, err := svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", buildPR, "", domain.MergeSourceGitHub)
	if err != nil {
		t.Fatalf("record build's merge: %v", err)
	}
	if ls := recs[0].LinkedStep; ls == nil || ls.Completed {
		t.Fatalf("the step closed while its review is unfinished: %+v", ls)
	}
	if st := stateOf(t, alice, step); st == "completed" {
		t.Fatal("the step completed with its review unfinished")
	}

	// The review's own MERGE is recorded (not its completion): that is what
	// satisfies the step's wait on it, so the step must finish now.
	if _, err := svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", reviewPR, "", domain.MergeSourceGitHub); err != nil {
		t.Fatalf("record the review's merge: %v", err)
	}
	if st := stateOf(t, alice, step); st != "completed" {
		t.Fatalf("the step after its prerequisite's merge was recorded = %s, want completed", st)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the step = %s, want backlog", st)
	}
}

// The same retrigger applies when a prerequisite is completed by a state
// change (amp_set_task_state completed) rather than by a completion: that is
// another way a dependency becomes satisfied, so the step held by it finishes.
func TestMergeStep_StateChangeCompletionRetriggers(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()

	build, _ := mergeChain(t, w, "statechange")
	review := mkIn(t, alice, w.s.storyID, "statechange review", build)
	step := mkHuman(t, alice, w.s.storyID, "Merge PR: statechange", w.bob.Email, build, review)
	pr := "https://git.example/pr/" + uniq("sc")
	finishWith(t, alice, build, map[string]any{"pr_url": pr})
	if err := linkStep(t, w, step, float64(build)); err != nil {
		t.Fatalf("link: %v", err)
	}
	// The merge lands while the review is open: the step stays open.
	if _, err := svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", pr, "", domain.MergeSourceGitHub); err != nil {
		t.Fatalf("record the merge: %v", err)
	}
	if st := stateOf(t, alice, step); st == "completed" {
		t.Fatal("the step closed with its review open")
	}
	// The review is completed through a state change, not a completion.
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(review), "agent_id": "amp-worker"})
	if _, err := svc.SetTaskState(ctx, uiPrincipal(w.alice), review, domain.TaskStateCompleted, ""); err != nil {
		t.Fatalf("complete the review by state change: %v", err)
	}
	if st := stateOf(t, alice, step); st != "completed" {
		t.Fatalf("the step after a state-change completion of its prerequisite = %s, want completed", st)
	}
}
