package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// amp_get_task carries the ticket's canonical web UI link, /t/<CODE>-<id>,
// so an agent can quote it to a person (#131).
func TestMCP_GetTaskURLIsCanonicalTicketLink(t *testing.T) {
	s := setupMCP(t)
	created := s.mkTask(t, "linkable", nil)
	id := int(created["id"].(float64))
	proj, err := s.repo.GetProject(context.Background(), s.projectID)
	if err != nil {
		t.Fatal(err)
	}
	out := s.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(id)})
	want := fmt.Sprintf("/t/%s-%d", strings.ToUpper(proj.Code), id)
	if out["url"] != want {
		t.Fatalf("amp_get_task url = %v, want %s", out["url"], want)
	}
	if _, ok := out["task"].(map[string]interface{}); !ok {
		t.Fatalf("amp_get_task lost its task: %v", out)
	}
}

// GET /api/runs/:id returns a run with where it belongs (task, project, the
// ticket's ref and /t/ link) for anyone who can see the ticket, and 404 for
// everyone else, so /r/<run id> can open the ticket at the run (#131). A run
// recorded with a session_id keeps it, so it links to /sessions/<id>.
func TestRuns_GetRunByIDAndSession(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	a := mkIn(t, root, w.s.storyID, "run me")
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "amp-worker"})
	done := root.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "model": "kimi-k3", "session_id": "rs_abc123"})
	run, _ := done["run"].(map[string]any)
	if run == nil || run["session_id"] != "rs_abc123" {
		t.Fatalf("completion run = %v, want session_id rs_abc123", done["run"])
	}
	runID := int64(run["id"].(float64))

	got := ra.want(t, 200, "root", http.MethodGet, fmt.Sprintf("/api/runs/%d", runID), nil)
	proj, err := w.s.repo.GetProject(context.Background(), w.s.projectID)
	if err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("%s-%d", strings.ToUpper(proj.Code), a)
	if int(got["task_id"].(float64)) != a || int(got["project_id"].(float64)) != w.s.projectID || got["ref"] != ref || got["url"] != "/t/"+ref {
		t.Fatalf("GET /api/runs/%d = %v, want task %d, project %d, ref %s", runID, got, a, w.s.projectID, ref)
	}
	if r := got["run"].(map[string]any); r["session_id"] != "rs_abc123" || r["model"] != "kimi-k3" {
		t.Fatalf("run body = %v", r)
	}
	// Someone who can't see the project gets 404, the same as a run that doesn't exist.
	ra.want(t, 404, "outsider", http.MethodGet, fmt.Sprintf("/api/runs/%d", runID), nil)
	ra.want(t, 404, "root", http.MethodGet, "/api/runs/999999999", nil)
	ra.want(t, 400, "root", http.MethodGet, "/api/runs/abc", nil)

	// A session id that couldn't be a /sessions/ link is refused, over MCP and REST, for that reason.
	if err := root.callExpectError(t, "amp_report_run", map[string]any{"task_id": float64(a), "session_id": "../../etc"}); !strings.Contains(err, "session_id") {
		t.Fatalf("a session_id with slashes: error %q, want a refusal naming session_id", err)
	}
	bad := ra.want(t, 400, "root", http.MethodPost, fmt.Sprintf("/api/tasks/%d/runs", a), map[string]any{"outcome": "progress", "session_id": "a/b"})
	if msg, _ := bad["error"].(string); !strings.Contains(msg, "session_id") {
		t.Fatalf("REST run with a bad session_id: %v, want a refusal naming session_id", bad)
	}

	// A run whose ticket was deleted is not found either: there's nothing to open.
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(a)})
	ra.want(t, 404, "root", http.MethodGet, fmt.Sprintf("/api/runs/%d", runID), nil)
}
