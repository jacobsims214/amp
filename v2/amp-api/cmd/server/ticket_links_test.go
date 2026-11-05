package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
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

// GET /api/projects/:id/stories lists every story in the project in one
// request (#132: the board no longer fetches stories once per epic).
func TestREST_ProjectStories(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	got := ra.want(t, 200, "root", http.MethodGet, fmt.Sprintf("/api/projects/%d/stories", w.s.projectID), nil)
	stories, _ := got["stories"].([]any)
	found := false
	for _, s := range stories {
		if int(s.(map[string]any)["id"].(float64)) == w.s.storyID {
			found = true
		}
	}
	if !found {
		t.Fatalf("project stories = %v, want story %d", got, w.s.storyID)
	}
	ra.want(t, 404, "outsider", http.MethodGet, fmt.Sprintf("/api/projects/%d/stories", w.s.projectID), nil)
}

// Every delete, over MCP or REST, of a ticket, a story or an epic sends the
// live "deleted" signal (a task.updated whose payload isn't a ticket), so open
// board views drop what was deleted (#132 review N1).
func TestDeletesPublishTheDeletedSignal(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	var mu sync.Mutex
	var got []map[string]interface{}
	w.s.hub.AddSink(func(e domain.Event) {
		if p, ok := e.Payload.(map[string]interface{}); ok && e.Type == domain.EventTaskUpdated && e.ProjectID == w.s.projectID && p["deleted"] == true {
			mu.Lock()
			got = append(got, p)
			mu.Unlock()
		}
	})
	take := func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}

	viaMCP := mkIn(t, root, w.s.storyID, "delete me over MCP")
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(viaMCP)})
	if sig := take(); len(sig) != 1 || sig[0]["task_id"] != viaMCP {
		t.Fatalf("MCP ticket delete signals = %v, want one {task_id: %d, deleted}", sig, viaMCP)
	}

	viaREST := mkIn(t, root, w.s.storyID, "delete me over REST")
	ra.want(t, 200, "root", http.MethodDelete, fmt.Sprintf("/api/tasks/%d", viaREST), nil)
	if sig := take(); len(sig) != 1 || sig[0]["task_id"] != viaREST {
		t.Fatalf("REST ticket delete signals = %v, want exactly one {task_id: %d, deleted}", sig, viaREST)
	}

	epicID := intField(t, root.call(t, "amp_create_epic", map[string]any{"project_id": float64(w.s.projectID), "name": "doomed epic"}), "id")
	storyID := intField(t, root.call(t, "amp_create_story", map[string]any{"project_id": float64(w.s.projectID), "epic_id": float64(epicID),
		"name": "doomed story", "acceptance_criteria": "x"}), "id")
	inStory := intField(t, root.call(t, "amp_create_task", map[string]any{"project_id": float64(w.s.projectID), "epic_id": float64(epicID),
		"story_id": float64(storyID), "name": "goes with its story", "description": ".", "acceptance_criteria": "."}), "id")
	root.call(t, "amp_delete_story", map[string]any{"story_id": float64(storyID)})
	sig := take()
	if len(sig) != 1 || sig[0]["story_id"] != storyID || fmt.Sprint(sig[0]["task_ids"]) != fmt.Sprint([]int{inStory}) {
		t.Fatalf("story delete signals = %v, want one {story_id: %d, task_ids: [%d], deleted}", sig, storyID, inStory)
	}

	root.call(t, "amp_delete_epic", map[string]any{"epic_id": float64(epicID)})
	if sig := take(); len(sig) != 1 || sig[0]["epic_id"] != epicID {
		t.Fatalf("epic delete signals = %v, want one {epic_id: %d, deleted}", sig, epicID)
	}
}
