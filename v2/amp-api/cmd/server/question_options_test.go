package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Structured agent questions (#130): amp_block_task takes options and the
// one recommended; the ticket carries them while the question is open; the
// person's choice is posted as the answer the agent reads; and a question
// without options behaves exactly as before.
func TestQuestions_OptionsAndRecommended(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	ctx := context.Background()

	q := mkIn(t, alice, w.s.storyID, "lead import")
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(q), "agent_id": "amp-worker"})
	res := alice.call(t, "amp_block_task", map[string]any{"task_id": float64(q),
		"reason": "Which CRM should the lead import write to: HubSpot or Pipedrive?",
		"options": []any{
			map[string]any{"label": "HubSpot", "consequence": "Holds the marketing contacts"},
			map[string]any{"label": "Pipedrive", "consequence": "What sales uses"},
			map[string]any{"label": "Both"},
		},
		"recommended": float64(1)})
	if res["question"] == nil {
		t.Fatalf("amp_block_task doesn't echo the question it recorded: %v", res)
	}
	got, err := w.s.repo.GetTask(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Question == nil || len(got.Question.Options) != 3 || got.Question.Recommended == nil || *got.Question.Recommended != 1 ||
		got.Question.Options[1].Label != "Pipedrive" || got.Question.Options[0].Consequence != "Holds the marketing contacts" {
		t.Fatalf("stored question = %+v", got.Question)
	}
	// amp_get_task shows it to the agent and the UI reads it on the ticket.
	task := alice.call(t, "amp_get_task", map[string]any{"task_id": float64(q)})["task"].(map[string]any)
	if fmt.Sprint(task["question"]) == "<nil>" {
		t.Fatalf("amp_get_task has no question: %v", task)
	}

	// Re-setting the same question keeps its options.
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(q), "state": "blocked", "reason": got.BlockReason})
	if again, _ := w.s.repo.GetTask(ctx, q); again.Question == nil {
		t.Fatal("re-setting the same question dropped its options")
	}

	// Someone who can't see the ticket learns nothing about its question:
	// the same error as for a ticket that doesn't exist.
	outsider := w.s.as(w.outsider)
	hidden := outsider.callExpectError(t, "amp_answer_question", map[string]any{"task_id": float64(q), "option": float64(9)})
	missing := outsider.callExpectError(t, "amp_answer_question", map[string]any{"task_id": float64(999999999), "option": float64(9)})
	if hidden != strings.Replace(missing, "999999999", fmt.Sprint(q), 1) || strings.Contains(hidden, "HubSpot") || strings.Contains(hidden, "option") {
		t.Fatalf("an outsider can tell the ticket exists or read its options: %q vs %q", hidden, missing)
	}

	// An index needs the label that was shown with it; a label that no
	// longer matches (the question changed) is refused with 409 and nothing
	// changes; the matching pair posts the choice.
	unblock := fmt.Sprintf("/api/tasks/%d/unblock", q)
	ra.want(t, 400, "alice", http.MethodPost, unblock, map[string]any{"option": 1})
	ra.want(t, 409, "alice", http.MethodPost, unblock, map[string]any{"option": 3, "option_label": "Both"})
	stale := ra.want(t, 409, "alice", http.MethodPost, unblock, map[string]any{"option": 1, "option_label": "HubSpot"})
	if msg := fmt.Sprint(stale); !strings.Contains(msg, "changed") {
		t.Fatalf("409 doesn't say the question changed: %v", stale)
	}
	if still, _ := w.s.repo.GetTask(ctx, q); still.State != "blocked" || still.Question == nil {
		t.Fatalf("a refused answer changed the ticket: %s %+v", still.State, still.Question)
	}
	out := ra.want(t, 200, "alice", http.MethodPost, unblock, map[string]any{"option": 1, "option_label": "Pipedrive", "answer": "sales owns leads"})
	if st := out["task"].(map[string]any)["state"]; st != "backlog" {
		t.Fatalf("answered ticket is %v", st)
	}
	comments, _ := w.s.repo.GetComments(ctx, q)
	if body := comments[len(comments)-1].Body; body != "Chose: Pipedrive\n\nsales owns leads" {
		t.Fatalf("answer comment = %q", body)
	}
	if after, _ := w.s.repo.GetTask(ctx, q); after.Question != nil || after.BlockReason != "" {
		t.Fatalf("the answer left the question behind: %q %+v", after.BlockReason, after.Question)
	}

	// A relayed choice over MCP, by index; the next question without options
	// has none (nothing carried over).
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(q), "agent_id": "amp-worker"})
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(q), "reason": "Daily or hourly?",
		"options": []any{map[string]any{"label": "Daily"}, map[string]any{"label": "Hourly"}}})
	wantErr(t, alice.callExpectError(t, "amp_answer_question", map[string]any{"task_id": float64(q), "option": float64(0)}), "option_label")
	wantErr(t, alice.callExpectError(t, "amp_answer_question", map[string]any{"task_id": float64(q), "option": float64(0), "option_label": "Hourly"}), "changed")
	alice.call(t, "amp_answer_question", map[string]any{"task_id": float64(q), "option": float64(0), "option_label": "Daily"})
	comments, _ = w.s.repo.GetComments(ctx, q)
	if body := comments[len(comments)-1].Body; body != "Chose: Daily" {
		t.Fatalf("relayed choice = %q", body)
	}
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(q), "agent_id": "amp-worker"})
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(q), "reason": "Anything else?"})
	if plain, _ := w.s.repo.GetTask(ctx, q); plain.Question != nil {
		t.Fatalf("a question without options got some: %+v", plain.Question)
	}
	// Old callers: an answer in words still works, and option is refused
	// on a question that has none.
	wantErr(t, alice.callExpectError(t, "amp_answer_question", map[string]any{"task_id": float64(q), "option": float64(0), "option_label": "Daily"}), "no options", "answer it in words")
	alice.call(t, "amp_answer_question", map[string]any{"task_id": float64(q), "answer": "No."})

	// Bad options are refused before anything changes.
	b := mkIn(t, alice, w.s.storyID, "bad options")
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "amp-worker"})
	for _, args := range []map[string]any{
		{"options": []any{map[string]any{"label": "Only one"}}},
		{"options": []any{map[string]any{"label": "A"}, map[string]any{"label": "B"}}, "recommended": float64(2)},
		{"recommended": float64(0)},
	} {
		args["task_id"], args["reason"] = float64(b), "Which?"
		alice.callExpectError(t, "amp_block_task", args)
	}
	if bt, _ := w.s.repo.GetTask(ctx, b); bt.State != "in_progress" {
		t.Fatalf("a refused block changed the ticket: %s", bt.State)
	}
}
