package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
)

// A person's open ticket is a gate: off the web UI, an agent can't remove it
// by deleting the ticket (or its story or epic) or by dropping the dependency
// on it. Adding dependencies and other edits stay allowed, a completed human
// ticket can be deleted, and the person in the UI can do all of it (#2190).
func TestHuman_GateCantBeRemovedOffTheUI(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	_, review, deploy := humanChain(t, w)
	link := fmt.Sprintf("/project/%d/board?task=%d", w.s.projectID, review)

	// Dropping the dependency on the open human ticket: refused over MCP
	// (full list and remove), and from an API token on REST.
	wantErr(t, alice.callExpectError(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(deploy), "dependency_ids": []any{}}), "web UI", link)
	wantErr(t, alice.callExpectError(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(deploy), "remove": []any{float64(review)}}), "web UI", link)
	if code, _ := ra.doVia(t, auth.ChannelToken, "alice", http.MethodPut, fmt.Sprintf("/api/tasks/%d/dependencies", deploy), map[string]any{"dependency_ids": []int{}}); code != 403 {
		t.Fatalf("API token dropping a human gate: %d, want 403", code)
	}
	// Other dependency edits work: add one, then remove it again.
	extra := mkIn(t, alice, w.s.storyID, "Write the quotes runbook")
	alice.call(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(deploy), "add": []any{float64(extra)}})
	alice.call(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(deploy), "remove": []any{float64(extra)}})
	if deps := planTask(t, alice, deploy)["dependency_ids"].([]any); len(deps) != 1 || int(deps[0].(float64)) != review {
		t.Fatalf("deploy deps = %v, want [%d]", deps, review)
	}

	// Deleting the open human ticket, its story or its epic: refused.
	wantErr(t, alice.callExpectError(t, "amp_delete_task", map[string]any{"task_id": float64(review)}), "web UI", link)
	owner := w.s.as(w.owner)
	wantErr(t, owner.callExpectError(t, "amp_delete_story", map[string]any{"story_id": float64(w.s.storyID)}), "web UI", link)
	wantErr(t, owner.callExpectError(t, "amp_delete_epic", map[string]any{"epic_id": float64(w.s.epicID)}), "web UI", link)
	for _, path := range []string{fmt.Sprintf("/api/tasks/%d", review), fmt.Sprintf("/api/stories/%d", w.s.storyID), fmt.Sprintf("/api/epics/%d", w.s.epicID)} {
		if code, _ := ra.doVia(t, auth.ChannelToken, "owner", http.MethodDelete, path, nil); code != 403 {
			t.Fatalf("API token DELETE %s: %d, want 403", path, code)
		}
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy is %s after refused edits, want blocked", st)
	}

	// The person in the web UI can do both.
	ra.want(t, 200, "alice", http.MethodPut, fmt.Sprintf("/api/tasks/%d/dependencies", deploy), map[string]any{"dependency_ids": []int{}})
	ra.want(t, 200, "alice", http.MethodDelete, fmt.Sprintf("/api/tasks/%d", review), nil)

	// A completed human ticket is no longer a gate: an agent may drop it
	// as a dependency and delete it.
	res := alice.call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "signoff", "story_id": float64(w.s.storyID), "name": "Sign off the quotes copy",
			"performer": "human", "human_reason": "approval", "owner": w.bob.Email},
	}})
	signoff := int(res["created"].([]any)[0].(map[string]any)["id"].(float64))
	after := mkIn(t, alice, w.s.storyID, "Publish the quotes copy", signoff)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", signoff), map[string]any{"note": "signed off"})
	alice.call(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(after), "dependency_ids": []any{}})
	alice.call(t, "amp_delete_task", map[string]any{"task_id": float64(signoff)})
}
