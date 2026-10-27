package main

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

// The Approve view's data (AMP #2483, design #2494 / artifact #24 v2, KB
// decisions/human-work-ux.md §2): the approval snapshot returned as
// changed_since.previous, several epics in one request, and stories listed
// even when everything in them is approved.

func planOf(t *testing.T, f *waitFixture, query string) map[string]any {
	t.Helper()
	return f.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work?approve=%s", f.w.s.projectID, query), nil)["plan"].(map[string]any)
}

func planTickets(plan map[string]any) map[int]map[string]any {
	out := map[int]map[string]any{}
	for _, x := range plan["tickets"].([]any) {
		pt := x.(map[string]any)
		out[int(pt["id"].(float64))] = pt
	}
	return out
}

func intsOf(v any) []int {
	out := []int{}
	for _, x := range v.([]any) {
		out = append(out, int(x.(float64)))
	}
	return out
}

// A ticket re-planned after approval carries the ticket as approved, so the
// view can diff it: name, description, acceptance criteria, dependency ids.
func TestApproveView_SnapshotIsPrevious(t *testing.T) {
	f := newWaitFixture(t)
	pid, epic := f.w.s.projectID, f.w.s.epicID
	alice := f.w.s.as(f.w.alice)

	approveAs(t, f.ra, "alice", pid, f.deploy)
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(f.deploy), "name": "deploy everywhere",
		"description": "deploy, now to two regions", "acceptance_criteria": "both regions serve"})
	alice.call(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(f.deploy), "add": []any{float64(f.solo)}})

	d := planTickets(planOf(t, f, fmt.Sprint(epic)))[f.deploy]
	if d == nil || d["change"] != "changed" {
		t.Fatalf("deploy = %v", d)
	}
	cs, _ := d["changed_since"].(map[string]any)
	prev, _ := cs["previous"].(map[string]any)
	if prev == nil {
		t.Fatalf("changed_since without previous: %v", cs)
	}
	if prev["name"] != "deploy" || prev["description"] != "d" || prev["acceptance_criteria"] != "a" {
		t.Fatalf("previous text = %v", prev)
	}
	if got := intsOf(prev["dependency_ids"]); !slices.Equal(got, []int{f.build}) {
		t.Fatalf("previous dependency_ids = %v, want [%d]", got, f.build)
	}
	if now := intsOf(d["dependency_ids"]); !slices.Contains(now, f.solo) {
		t.Fatalf("current dependency_ids = %v", now)
	}

	// Approving it again replaces the snapshot: no longer changed, and the
	// next re-plan diffs against the newer text.
	approveAs(t, f.ra, "alice", pid, f.deploy)
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(f.deploy), "description": "three regions"})
	d = planTickets(planOf(t, f, fmt.Sprint(epic)))[f.deploy]
	prev = d["changed_since"].(map[string]any)["previous"].(map[string]any)
	if prev["name"] != "deploy everywhere" || prev["description"] != "deploy, now to two regions" {
		t.Fatalf("previous after re-approval = %v", prev)
	}
	if got := intsOf(prev["dependency_ids"]); !slices.Equal(got, []int{f.build, f.solo}) && !slices.Equal(got, []int{f.solo, f.build}) {
		t.Fatalf("previous dependency_ids after re-approval = %v", got)
	}

	// A new ticket has no changed_since at all.
	if s := planTickets(planOf(t, f, fmt.Sprint(epic)))[f.solo]; s["change"] != "new" || s["changed_since"] != nil {
		t.Fatalf("new ticket = %v", s)
	}
}

// ?approve=<a>,<b> and ?approve=all return the epics together: one as_of,
// epic_id on each ticket and story, and a dependency between them counts
// as inside the set.
func TestApproveView_SeveralEpics(t *testing.T) {
	f := newWaitFixture(t)
	pid, epic1 := f.w.s.projectID, f.w.s.epicID
	alice := f.w.s.as(f.w.alice)
	story2 := intField(t, alice.call(t, "amp_get_task", map[string]any{"task_id": float64(f.epic2Task)})["task"].(map[string]any), "story_id")
	// epic 2's second ticket runs after epic 1's solo.
	cross := mkIn(t, alice, story2, "after solo", f.solo)

	only2 := planOf(t, f, fmt.Sprint(f.epic2))
	inOutside := func(plan map[string]any, id int) bool {
		for _, x := range plan["outside"].([]any) {
			if int(x.(map[string]any)["id"].(float64)) == id {
				return true
			}
		}
		return false
	}
	if !inOutside(only2, f.solo) {
		t.Fatalf("epic 2 alone: solo should be outside: %v", only2["outside"])
	}

	both := planOf(t, f, fmt.Sprintf("%d,%d", f.epic2, epic1))
	epics := both["epics"].([]any)
	if len(epics) != 2 || int(epics[0].(map[string]any)["id"].(float64)) != f.epic2 || int(epics[1].(map[string]any)["id"].(float64)) != epic1 {
		t.Fatalf("epics = %v", epics)
	}
	if int(both["epic"].(map[string]any)["id"].(float64)) != f.epic2 {
		t.Fatalf("epic (first of the set) = %v", both["epic"])
	}
	if e1 := epics[1].(map[string]any); e1["caller_last_approved_at"] == nil || int(e1["ticket_count"].(float64)) == 0 {
		t.Fatalf("epic 1 entry = %v", e1)
	}
	if inOutside(both, f.solo) {
		t.Fatalf("both epics: solo is inside the set, not outside: %v", both["outside"])
	}
	tk := planTickets(both)
	if tk[cross] == nil || int(tk[cross]["epic_id"].(float64)) != f.epic2 || tk[f.solo] == nil || int(tk[f.solo]["epic_id"].(float64)) != epic1 {
		t.Fatalf("tickets across epics: cross %v solo %v", tk[cross], tk[f.solo])
	}
	// One as_of: the newest planned_at across both epics.
	var newest time.Time
	for _, pt := range tk {
		at, err := time.Parse(time.RFC3339Nano, pt["planned_at"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if at.After(newest) {
			newest = at
		}
	}
	asOf, err := time.Parse(time.RFC3339Nano, both["as_of"].(string))
	if err != nil || !asOf.Equal(newest) {
		t.Fatalf("as_of = %v, want %v", both["as_of"], newest)
	}
	for _, x := range both["stories"].([]any) {
		st := x.(map[string]any)
		if int(st["id"].(float64)) == story2 && int(st["epic_id"].(float64)) != f.epic2 {
			t.Fatalf("story epic_id = %v", st)
		}
	}
	// starts_on_approval is over the whole set: solo (epic 1) starts, the
	// cross ticket waits on it.
	starts := intsOf(both["starts_on_approval"])
	if !slices.Contains(starts, f.solo) || slices.Contains(starts, cross) {
		t.Fatalf("starts_on_approval = %v", starts)
	}

	// all: every epic awaiting alice, oldest first (epic 1, then epic 2).
	all := planOf(t, f, "all")
	ids := []int{}
	for _, x := range all["epics"].([]any) {
		ids = append(ids, int(x.(map[string]any)["id"].(float64)))
	}
	if !slices.Equal(ids, []int{epic1, f.epic2}) {
		t.Fatalf("approve=all epics = %v, want [%d %d]", ids, epic1, f.epic2)
	}

	// The approve call takes tickets from both epics with the one as_of.
	res := f.ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", pid),
		map[string]any{"task_ids": []any{float64(f.solo), float64(cross)}, "as_of": both["as_of"]})
	if got := intsOf(res["approved"]); len(got) != 2 {
		t.Fatalf("approve across epics approved %v (skipped %v)", got, res["skipped"])
	}

	for _, bad := range []string{"x", "1,,2", "-3"} {
		f.ra.want(t, 400, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work?approve=%s", pid, bad), nil)
	}
	f.ra.want(t, 404, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work?approve=%d,999999", pid, epic1), nil)
}

// A story whose open tickets are all approved is still listed (the view
// collapses it to "all N approved"); one with only completed tickets is in
// done_stories.
func TestApproveView_ApprovedAndDoneStories(t *testing.T) {
	f := newWaitFixture(t)
	pid := f.w.s.projectID
	alice := f.w.s.as(f.w.alice)
	approvedStory := intField(t, alice.call(t, "amp_create_story", map[string]any{"project_id": float64(pid), "epic_id": float64(f.epic2),
		"name": "all approved", "acceptance_criteria": "done"}), "id")
	doneStory := intField(t, alice.call(t, "amp_create_story", map[string]any{"project_id": float64(pid), "epic_id": float64(f.epic2),
		"name": "all done", "acceptance_criteria": "done"}), "id")
	a := mkIn(t, alice, approvedStory, "approved work")
	d := mkIn(t, alice, doneStory, "done work")
	approveAs(t, f.ra, "alice", pid, a, d)
	finishWith(t, alice, d, map[string]any{})

	plan := planOf(t, f, fmt.Sprint(f.epic2))
	var listed map[string]any
	for _, x := range plan["stories"].([]any) {
		st := x.(map[string]any)
		switch int(st["id"].(float64)) {
		case approvedStory:
			listed = st
		case doneStory:
			t.Fatalf("a story with only completed tickets is not an open story: %v", st)
		}
	}
	if listed == nil || !slices.Equal(intsOf(listed["task_ids"]), []int{a}) || len(listed["awaiting_ids"].([]any)) != 0 {
		t.Fatalf("fully approved story = %v (stories %v)", listed, plan["stories"])
	}
	done := plan["done_stories"].([]any)
	if len(done) != 1 || int(done[0].(map[string]any)["id"].(float64)) != doneStory || !slices.Equal(intsOf(done[0].(map[string]any)["task_ids"]), []int{d}) {
		t.Fatalf("done_stories = %v", done)
	}
}

// POST /tasks/{id}/reopen: My work's "Reopen ticket" for a PR closed
// without merging. Only a done ticket is reopened.
func TestReopenDoneTicket(t *testing.T) {
	f := newWaitFixture(t)
	// build is done (merge needed); deploy waits on it.
	res := f.ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/reopen", f.build), nil)
	if st := res["task"].(map[string]any)["state"]; st == "completed" {
		t.Fatalf("reopened ticket state = %v", st)
	}
	f.ra.want(t, 409, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/reopen", f.build), nil)
	f.ra.want(t, 409, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/reopen", f.solo), nil)
}
