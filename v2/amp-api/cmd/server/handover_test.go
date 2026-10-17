package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A lead hands one person's open tickets to another: each is reassigned with
// a comment saying why; in-progress tickets stay unless asked; a plain member
// can't hand over someone else's work.
func TestHandover(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	a := mkIn(t, alice, w.s.storyID, "quotes schema")
	b := mkIn(t, alice, w.s.storyID, "quotes ui")
	running := mkIn(t, alice, w.s.storyID, "quotes contract")
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(running), "agent_id": "w"})

	if code, _ := ra.do(t, "carol", http.MethodPost, fmt.Sprintf("/api/projects/%d/handover", w.s.projectID),
		map[string]any{"from": w.alice.Email, "to": w.bob.Email}); code != 403 && code != 404 {
		t.Fatalf("a member handed over someone else's tickets: %d", code)
	}
	out := w.s.as(w.lead).call(t, "amp_handover_tasks", map[string]any{"project_id": float64(w.s.projectID),
		"from": w.alice.Email, "to": w.bob.Email, "reason": "Alice out sick; demo Monday"})
	moved := map[int]bool{}
	for _, r := range out["results"].([]any) {
		m := r.(map[string]any)
		if m["moved"] == true {
			moved[int(m["task_id"].(float64))] = true
		}
	}
	if !moved[a] || !moved[b] || moved[running] {
		t.Fatalf("handover: %v", out)
	}
	task := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(a)})
	if v, ok := task["task"]; ok {
		task = v.(map[string]any)
	}
	if int(task["owner_user_id"].(float64)) != w.bob.User.ID {
		t.Fatalf("owner after handover: %v", task["owner_user_id"])
	}
	c := alice.call(t, "amp_get_task_comments", map[string]any{"task_id": float64(a)})
	if !strings.Contains(fmt.Sprint(c), "Alice out sick; demo Monday") {
		t.Fatalf("no handover comment: %v", c)
	}
}
