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
func TestRest_AnswerQuestion(t *testing.T) {
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

// The harness round trip over MCP: a worker can't wait, so it blocks its
// ticket with the question and stops; the orchestrator finds it in
// amp_list_tasks' questions, asks the user, records the answer with
// amp_answer_question, and the ticket is ready again with the answer in
// its comments for the next worker.
func TestMCP_QuestionRoundTrip(t *testing.T) {
	s := setupMCP(t)
	id := mkIn(t, s, s.storyID, "pick a tax provider")
	s.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "worker-1"})

	// A question needs a reason.
	if msg := s.callExpectError(t, "amp_block_task", map[string]any{"task_id": float64(id), "reason": "  "}); !strings.Contains(msg, "reason is required") {
		t.Fatalf("blocked without a question: %q", msg)
	}
	s.call(t, "amp_block_task", map[string]any{"task_id": float64(id), "reason": "Avalara or Stripe Tax?"})

	list := s.call(t, "amp_list_tasks", map[string]any{"project_id": float64(s.projectID), "owner": "any"})
	inList := func(bucket string) bool {
		for _, x := range list[bucket].([]any) {
			if int(x.(map[string]any)["id"].(float64)) == id {
				return true
			}
		}
		return false
	}
	if !inList("questions") || !inList("blocked") || inList("in_progress") {
		t.Fatalf("question not listed as one: questions=%v", list["questions"])
	}

	out := s.call(t, "amp_answer_question", map[string]any{"task_id": float64(id), "answer": "Stripe Tax; finance confirmed EU OSS is covered."})
	if out["state"] != "backlog" {
		t.Fatalf("after the answer: %v", out)
	}
	list = s.call(t, "amp_list_tasks", map[string]any{"project_id": float64(s.projectID), "owner": "any"})
	if !inList("ready_to_dispatch") || inList("questions") {
		t.Fatalf("answered ticket not ready: %v", list["questions"])
	}
	comments := s.call(t, "amp_get_task_comments", map[string]any{"task_id": float64(id)})
	found := false
	for _, c := range comments["comments"].([]any) {
		if strings.Contains(c.(map[string]any)["body"].(string), "finance confirmed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the answer isn't on the ticket: %v", comments)
	}
	// Answering again: there's no question any more.
	if msg := s.callExpectError(t, "amp_answer_question", map[string]any{"task_id": float64(id), "answer": "again"}); !strings.Contains(msg, "no open question") {
		t.Fatalf("answered a ticket without a question: %q", msg)
	}
}
