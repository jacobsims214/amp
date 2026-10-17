package main

import (
	"fmt"
	"net/http"
	"testing"
)

// Deleted tickets go to the trash and come back whole: text, comments,
// history, and dependency links in both directions, with states following.
func TestTrash_DeleteAndRestore(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	a := mkIn(t, root, w.s.storyID, "schema")
	b := mkIn(t, root, w.s.storyID, "endpoints", a)
	c := mkIn(t, root, w.s.storyID, "ui", b)
	root.call(t, "amp_add_task_comment", map[string]any{"task_id": float64(b), "body": "decided: REST, not gRPC"})

	// Wipe b and c (as the wargame's manager did with a whole plan).
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(c)})
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(b)})
	trash := root.call(t, "amp_list_deleted_tasks", map[string]any{"project_id": float64(w.s.projectID)})
	if trash["count"] != 2.0 {
		t.Fatalf("trash: %v", trash)
	}

	res := ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/trash/restore", w.s.projectID), map[string]any{"task_ids": []int{c, b}})
	for _, r := range res["results"].([]any) {
		if m := r.(map[string]any); m["restored"] != true || m["error"] != nil {
			t.Fatalf("restore: %v", res)
		}
	}
	hist := root.call(t, "amp_get_ticket_history", map[string]any{"task_id": float64(b)})
	if len(hist["comments"].([]any)) != 1 {
		t.Fatalf("b's comment didn't come back: %v", hist["comments"])
	}
	task := hist["task"].(map[string]any)
	if fmt.Sprint(task["dependency_ids"]) != fmt.Sprintf("[%d]", a) {
		t.Fatalf("b's dependency on a: %v", task["dependency_ids"])
	}
	// c depended on b; restored in either order, the link is back and c waits.
	ct := root.call(t, "amp_get_task", map[string]any{"task_id": float64(c)})
	if v, ok := ct["task"]; ok {
		ct = v.(map[string]any)
	}
	if fmt.Sprint(ct["dependency_ids"]) != fmt.Sprintf("[%d]", b) || ct["state"] != "blocked" {
		t.Fatalf("c after restore: %v", ct)
	}
	// Completing the chain still flows through the restored tickets.
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "x"})
	root.call(t, "amp_complete_task", map[string]any{"task_id": float64(a)})
	if st := stateOf(t, w.s, b); st != "backlog" {
		t.Fatalf("b after a completed: %s", st)
	}
	// The trash is empty again, and a non-lead who didn't delete can't restore.
	if tr := root.call(t, "amp_list_deleted_tasks", map[string]any{"project_id": float64(w.s.projectID)}); tr["count"] != 0.0 {
		t.Fatalf("trash after restore: %v", tr)
	}
	d := mkIn(t, root, w.s.storyID, "spike")
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(d)})
	out := w.s.as(w.alice).call(t, "amp_restore_tasks", map[string]any{"project_id": float64(w.s.projectID), "task_ids": []any{float64(d)}})
	if r := out["results"].([]any)[0].(map[string]any); r["restored"] == true {
		t.Fatalf("a member restored someone else's deletion: %v", out)
	}
}
