package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// An open question leaves only through an answer (AMP #2191): off the UI no
// state change replaces or clears it, a "scheduled:" reason is never set by
// hand, and a comment posted under a person's email is marked as relayed.
func TestQuestions_OnlyAnAnswerClearsThem(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	ctx := context.Background()
	q := mkIn(t, alice, w.s.storyID, "has a question")
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(q), "agent_id": "amp-worker"})
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(q), "reason": "Stripe or Avalara?"})

	for _, tc := range []struct {
		name   string
		args   map[string]any
		expect []string
	}{
		{"cleared reason", map[string]any{"state": "blocked", "reason": ""}, []string{"question", "amp_answer_question"}},
		{"new reason", map[string]any{"state": "blocked", "reason": "something else"}, []string{"question", "amp_answer_question"}},
		{"scheduled reason", map[string]any{"state": "blocked", "reason": "scheduled: 2030-01-01T00:00:00Z"}, []string{"scheduled:", "amp_set_task_start_at"}},
		{"to backlog", map[string]any{"state": "backlog"}, []string{"question", "amp_answer_question"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["task_id"] = float64(q)
			wantErr(t, alice.callExpectError(t, "amp_set_task_state", tc.args), tc.expect...)
			got, err := w.s.repo.GetTask(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "blocked" || got.BlockReason != "Stripe or Avalara?" {
				t.Fatalf("question changed: state=%s reason=%q", got.State, got.BlockReason)
			}
		})
	}
	// Re-setting the same question is a no-op, not a bypass.
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(q), "state": "blocked", "reason": "Stripe or Avalara?"})

	// A comment under the person's email (the caller's own, or another
	// member's) is recorded as relayed; an agent name is kept as typed.
	for author, want := range map[string]string{
		w.alice.User.Email:                         w.alice.User.Email + " (relayed by agent)",
		strings.ToUpper(w.bob.User.Email):          w.bob.User.Email + " (relayed by agent)",
		"Bob <" + w.bob.User.Email + ">":           w.bob.User.Email + " (relayed by agent)",
		"amp-worker":                               "amp-worker",
		"nobody-" + q2s(q) + "@example.invalid":    "nobody-" + q2s(q) + "@example.invalid",
		w.alice.User.Email + " (relayed by agent)": w.alice.User.Email + " (relayed by agent)",
	} {
		alice.call(t, "amp_add_task_comment", map[string]any{"task_id": float64(q), "body": "Stripe.", "author": author})
		comments, _ := w.s.repo.GetComments(ctx, q)
		if got := comments[len(comments)-1].Author; !strings.EqualFold(got, want) {
			t.Errorf("author %q recorded as %q, want %q", author, got, want)
		}
	}

	// The relayed answer still works and is badged.
	alice.call(t, "amp_answer_question", map[string]any{"task_id": float64(q), "answer": "Stripe"})
	comments, _ := w.s.repo.GetComments(ctx, q)
	if a := comments[len(comments)-1].Author; a != w.alice.User.Email+" (relayed by agent)" {
		t.Fatalf("relayed answer author = %q", a)
	}

	// In the UI, a person's own comment is theirs.
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/comments", q), map[string]any{"body": "thanks"})
	comments, _ = w.s.repo.GetComments(ctx, q)
	if a := comments[len(comments)-1].Author; a != w.alice.User.Email {
		t.Fatalf("UI comment author = %q", a)
	}

	// "scheduled:" is refused with no question in the way, too.
	free := mkIn(t, alice, w.s.storyID, "no question")
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(free), "state": "blocked", "reason": "scheduled:2030-01-01T00:00:00Z"}), "amp_set_task_start_at")
}

func q2s(id int) string { return fmt.Sprintf("%d", id) }
