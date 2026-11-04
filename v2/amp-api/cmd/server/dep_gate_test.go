package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// forceState puts a ticket in state straight through its actor, the way any
// path that skips the service's checks would (an older replica, a future
// caller). The actor's own gates must still hold.
func forceState(t *testing.T, s *mcpStack, id int, state domain.TaskState) {
	t.Helper()
	reply := make(chan actor.ReplySimple, 1)
	if err := s.reg.Send(s.projectID, &actor.MsgSetTaskState{TaskID: id, State: state, ByName: "test", ReplyCh: reply}); err != nil {
		t.Fatal(err)
	}
	if r := actor.Await(context.Background(), reply); r.Err != nil {
		t.Fatalf("force %d to %s: %v", id, state, r.Err)
	}
}

// Work never starts while a dependency is incomplete, whatever state the
// ticket is in and whoever asks (#2196, PR #77 re-review). The bypass it
// closes: with build -> review (a person's ticket) -> deploy, an agent set
// deploy to backlog (its empty "waiting on dependencies" block reason isn't a
// question), dispatched it (the actor only checked deps while blocked) and
// completed it, while the person's review was still open.
func TestDependencies_WorkWaitsForEveryDependency(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	build, review, deploy := humanChain(t, w)
	alice.finish(t, build)
	if st := stateOf(t, alice, review); st != "backlog" {
		t.Fatalf("review after build = %s, want backlog", st)
	}
	reviewLink := fmt.Sprintf("/project/%d/board?task=%d", w.s.projectID, review)

	// 1. The reviewer's chain, over MCP: set backlog is refused, naming the
	// open human ticket and where the person finishes it.
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(deploy), "state": "backlog"}),
		fmt.Sprint(review), reviewLink, "web UI")
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy after refused set backlog = %s, want blocked", st)
	}
	wantErr(t, alice.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(deploy), "agent_id": "amp-worker"}),
		"waiting on task IDs", fmt.Sprint(review))
	alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(deploy)})
	// The same over REST with an API token.
	if code, _ := ra.doVia(t, auth.ChannelToken, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/dispatch", deploy), map[string]any{}); code < 400 {
		t.Fatalf("API token dispatching deploy: %d, want refused", code)
	}

	// 2. Even in backlog (however it got there), the actor refuses to start
	// it: over MCP and from the person in the web UI.
	forceState(t, w.s, deploy, domain.TaskStateBacklog)
	wantErr(t, alice.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(deploy), "agent_id": "amp-worker"}),
		"waiting on task IDs", fmt.Sprint(review))
	if code, out := ra.do(t, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/dispatch", deploy), map[string]any{}); code < 400 {
		t.Fatalf("UI dispatching deploy with an open dependency: %d %v, want refused", code, out)
	}
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(deploy)}), "never dispatched")
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy = %s after refused dispatch, want backlog", st)
	}
	if st := stateOf(t, alice, review); st != "backlog" {
		t.Fatalf("review = %s, want still open", st)
	}

	// 3. A person in the web UI keeps the state override.
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	person := authz.Principal{UserID: w.alice.User.ID, Email: w.alice.User.Email, Channel: auth.ChannelUI}
	forceState(t, w.s, deploy, domain.TaskStateBlocked)
	if _, err := svc.SetTaskState(context.Background(), person, deploy, domain.TaskStateBacklog, ""); err != nil {
		t.Fatalf("UI set backlog: %v", err)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after UI override = %s, want backlog", st)
	}
	forceState(t, w.s, deploy, domain.TaskStateBlocked)

	// 4. The person finishes the review: deploy is ready and runs normally.
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", review), map[string]any{"note": "merged"})
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after review = %s, want backlog", st)
	}
	alice.finish(t, deploy)
	if st := stateOf(t, alice, deploy); st != "completed" {
		t.Fatalf("deploy = %s, want completed", st)
	}
}

// The dependency check reads the dependencies' current states, not a cached
// copy: a dependency moved back from completed holds its dependents again,
// a dependency added mid-flight holds an agent's completion, and an agent
// moving work back while a dependency is open leaves it blocked, not ready.
func TestDependencies_CheckedAgainstCurrentState(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.s.as(w.alice)

	// A dependency reopened after it completed.
	a := mkIn(t, alice, w.s.storyID, "Write the schema")
	b := mkIn(t, alice, w.s.storyID, "Write the API", a)
	alice.finish(t, a)
	if st := stateOf(t, alice, b); st != "backlog" {
		t.Fatalf("b = %s, want backlog", st)
	}
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(a), "state": "backlog", "reason": "schema needs another pass"})
	wantErr(t, alice.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "amp-worker"}),
		"waiting on task IDs", fmt.Sprint(a))

	// A dependency added while the ticket is in progress.
	x := mkIn(t, alice, w.s.storyID, "Wire the client")
	y := mkIn(t, alice, w.s.storyID, "Agree the client contract")
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(x), "agent_id": "amp-worker"})
	alice.call(t, "amp_set_task_dependencies", map[string]any{"task_id": float64(x), "add": []any{float64(y)}})
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(x)}), fmt.Sprint(y))
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(x), "state": "completed"}), fmt.Sprint(y))

	// Moving it back off the UI is allowed, and leaves it waiting on y.
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(x), "state": "backlog", "reason": "contract first"})
	if st := stateOf(t, alice, x); st != "blocked" {
		t.Fatalf("x moved back with an open dependency = %s, want blocked", st)
	}
	alice.finish(t, y)
	if st := stateOf(t, alice, x); st != "backlog" {
		t.Fatalf("x after its dependency = %s, want backlog", st)
	}
	alice.finish(t, x)
}
