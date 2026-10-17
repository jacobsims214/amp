package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Answering an agent's flag from the UI: the answer becomes a comment and
// the ticket goes back to ready — or to waiting if it still depends on
// unfinished work. Only flagged tickets can be answered.
func TestRest_AnswerFlag(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	open := mkIn(t, root, w.s.storyID, "open dependency")
	free := mkIn(t, root, w.s.storyID, "free")
	waiting := mkIn(t, root, w.s.storyID, "waiting", open)

	root.call(t, "amp_block_task", map[string]any{"task_id": float64(free), "reason": "Need a decision: Avalara or Stripe Tax?"})
	root.call(t, "amp_block_task", map[string]any{"task_id": float64(waiting), "reason": "Which region first?"})

	out := ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/tasks/%d/unblock", free), map[string]any{"answer": "Stripe Tax."})
	task := out["task"].(map[string]any)
	if task["state"] != "backlog" || task["block_reason"] != nil && task["block_reason"] != "" {
		t.Fatalf("answered ticket: %v / %v", task["state"], task["block_reason"])
	}
	_, comments := ra.do(t, "root", http.MethodGet, fmt.Sprintf("/api/tasks/%d/comments", free), nil)
	list := comments["comments"].([]any)
	if len(list) == 0 || !strings.Contains(list[len(list)-1].(map[string]any)["body"].(string), "Stripe Tax.") {
		t.Fatalf("the answer wasn't posted: %v", list)
	}

	// Still depends on unfinished work: back to waiting, not ready.
	out = ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/tasks/%d/unblock", waiting), nil)
	task = out["task"].(map[string]any)
	if task["state"] != "blocked" || task["block_reason"] != nil && task["block_reason"] != "" {
		t.Fatalf("answered ticket with an open dependency: %v / %v", task["state"], task["block_reason"])
	}

	// Not flagged (any more), and not someone outside the project.
	ra.want(t, 400, "root", http.MethodPost, fmt.Sprintf("/api/tasks/%d/unblock", free), nil)
	if code, _ := ra.do(t, "outsider", http.MethodPost, fmt.Sprintf("/api/tasks/%d/unblock", waiting), nil); code < 400 || code == 400 {
		t.Fatalf("outsider answered a flag: %d", code)
	}
}
