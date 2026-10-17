package main

import (
	"fmt"
	"net/http"
	"testing"
)

// Plan approval: tickets planned since the last approval are listed by the
// plan check; with the project's setting on, they can't be dispatched until
// a person approves; agents can't approve.
func TestPlanApproval(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	a := mkIn(t, root, w.s.storyID, "first")

	check := root.call(t, "amp_check_plan", map[string]any{"project_id": float64(w.s.projectID)})
	if !containsID(check["awaiting_approval"], a) || check["require_plan_approval"] != false {
		t.Fatalf("never-approved plan: %v / %v", check["awaiting_approval"], check["require_plan_approval"])
	}

	// Off by default: dispatch works as before.
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "w"})

	root.call(t, "amp_update_project_settings", map[string]any{"project_id": float64(w.s.projectID), "require_plan_approval": true})
	b := mkIn(t, root, w.s.storyID, "second")
	msg := root.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "w"})
	wantErr(t, msg, "approved")

	// A person approves (over REST, as the UI does); now it dispatches.
	ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", w.s.projectID), nil)
	check = root.call(t, "amp_check_plan", map[string]any{"project_id": float64(w.s.projectID)})
	if len(check["awaiting_approval"].([]any)) != 0 || check["plan_approved_at"] == nil {
		t.Fatalf("after approval: %v", check)
	}
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "w"})

	// Planning more needs approving again; the orchestrator records it over MCP.
	c := mkIn(t, root, w.s.storyID, "third")
	wantErr(t, root.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(c), "agent_id": "w"}), "approved")
	root.call(t, "amp_approve_plan", map[string]any{"project_id": float64(w.s.projectID)})
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(c), "agent_id": "w"})

	// Plain members can't approve; leads and owners can.
	if code, _ := ra.do(t, "bob", http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", w.s.projectID), nil); code != 403 && code != 404 {
		t.Fatalf("a member approved: %d", code)
	}
}

func containsID(v any, id int) bool {
	list, _ := v.([]any)
	for _, x := range list {
		if int(x.(float64)) == id {
			return true
		}
	}
	return false
}
