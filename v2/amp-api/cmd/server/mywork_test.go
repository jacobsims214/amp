package main

import (
	"net/http"
	"testing"
)

// The home page: my open tickets, flags in projects I can see (and not in
// ones I can't), and my agents.
func TestRest_MyWork(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	mine := mkIn(t, alice, w.s.storyID, "alice's ticket")
	flagged := mkIn(t, alice, w.s.storyID, "needs an answer")
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(flagged), "reason": "Which tax provider?"})

	out := ra.want(t, 200, "alice", http.MethodGet, "/api/my-work", nil)
	owned := idSet(out["owned"])
	if !owned[mine] || !owned[flagged] {
		t.Fatalf("alice's open tickets: %v", out["owned"])
	}
	if !idSet(out["flags"])[flagged] {
		t.Fatalf("flag missing: %v", out["flags"])
	}
	if f := out["flags"].([]any)[0].(map[string]any); f["project_code"] == "" || f["project_code"] == nil {
		t.Fatalf("flag without its project: %v", f)
	}

	// Someone outside the project sees neither.
	out = ra.want(t, 200, "outsider", http.MethodGet, "/api/my-work", nil)
	if idSet(out["flags"])[flagged] || len(out["owned"].([]any)) != 0 {
		t.Fatalf("outsider sees another project's work: %v", out)
	}
}

func idSet(v any) map[int]bool {
	out := map[int]bool{}
	list, _ := v.([]any)
	for _, x := range list {
		if m, ok := x.(map[string]any); ok {
			out[int(m["id"].(float64))] = true
		}
	}
	return out
}
