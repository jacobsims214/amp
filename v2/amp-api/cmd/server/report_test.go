package main

import (
	"fmt"
	"net/http"
	"testing"
)

// A project report counts tickets by the state a person reads, credits each
// agent, and breaks down by any metadata key using inherited values.
func TestRest_ProjectReport(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	root.call(t, "amp_update_epic", map[string]any{"epic_id": float64(w.s.epicID), "metadata": map[string]any{"initiative": "Launch"}})
	a := mkIn(t, root, w.s.storyID, "a")
	b := mkIn(t, root, w.s.storyID, "b")
	c := mkIn(t, root, w.s.storyID, "c", a)
	d := mkIn(t, root, w.s.storyID, "d")
	root.call(t, "amp_update_task", map[string]any{"task_id": float64(d), "metadata": map[string]any{"initiative": "Ops"}})
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "claude-1"})
	root.call(t, "amp_complete_task", map[string]any{"task_id": float64(a)})
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "claude-1"})
	root.call(t, "amp_block_task", map[string]any{"task_id": float64(d), "reason": "Which region?"})
	_ = c

	out := ra.want(t, 200, "root", http.MethodGet, fmt.Sprintf("/api/projects/%d/report?group_by=initiative", w.s.projectID), nil)
	st := out["states"].(map[string]any)
	if st["done"] != 1.0 || st["in_progress"] != 1.0 || st["flagged"] != 1.0 || st["ready"] != 1.0 {
		t.Fatalf("states: %v", st)
	}
	if out["completed"] != 1.0 || out["dispatched"] != 2.0 {
		t.Fatalf("range counts: completed %v dispatched %v", out["completed"], out["dispatched"])
	}
	agent := out["agents"].([]any)[0].(map[string]any)
	if agent["key"] != "claude-1" || agent["completed_in_range"] != 1.0 || agent["tickets"] != 2.0 {
		t.Fatalf("agent row: %v", agent)
	}
	rows := map[string]map[string]any{}
	for _, r := range out["breakdown"].([]any) {
		m := r.(map[string]any)
		rows[m["key"].(string)] = m
	}
	// The epic's value is inherited by its tickets; the ticket's own wins.
	if rows["Launch"]["tickets"] != 3.0 || rows["Ops"]["tickets"] != 1.0 || rows["Ops"]["states"].(map[string]any)["flagged"] != 1.0 {
		t.Fatalf("breakdown: %v", rows)
	}
	found := false
	for _, k := range out["metadata_keys"].([]any) {
		if k.(map[string]any)["key"] == "initiative" {
			found = true
		}
	}
	if !found {
		t.Fatalf("metadata keys: %v", out["metadata_keys"])
	}
	if code, _ := ra.do(t, "outsider", http.MethodGet, fmt.Sprintf("/api/projects/%d/report", w.s.projectID), nil); code != 403 && code != 404 {
		t.Fatalf("outsider got the report: %d", code)
	}
}
