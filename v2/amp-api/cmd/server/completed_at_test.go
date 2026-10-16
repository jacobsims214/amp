package main

import (
	"context"
	"testing"

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/hub"
)

// A completed task's completion time lives in the database, so it survives
// its actor restarting (the Report page reads it), and is cleared if the
// task is reopened.
func TestCompletedAtSurvivesActorRestart(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	tid := taskID(t, s.mkTask(t, "finish me", nil))
	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(tid)})

	stored, err := s.repo.GetTask(ctx, tid)
	if err != nil || stored.CompletedAt == nil {
		t.Fatalf("completed_at not stored: %+v %v", stored, err)
	}

	// A fresh actor system loads the project from the database, as after a restart.
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	reg := actor.NewRegistry(system, s.repo, hub.New())
	pid, err := reg.Get(s.projectID)
	if err != nil {
		t.Fatal(err)
	}
	replyCh := make(chan actor.ReplyGetTask, 1)
	system.Root.Send(pid, &actor.MsgGetTask{TaskID: tid, ReplyCh: replyCh})
	reloaded := <-replyCh
	if reloaded.Err != nil || reloaded.Task.CompletedAt == nil || !reloaded.Task.CompletedAt.Equal(*stored.CompletedAt) {
		t.Fatalf("after restart: completed_at = %v (err %v), want %v", reloaded.Task.CompletedAt, reloaded.Err, stored.CompletedAt)
	}

	// Reopening clears it; completing again stamps a new one.
	s.call(t, "amp_set_task_state", map[string]interface{}{"task_id": float64(tid), "state": "backlog", "reason": "reopened"})
	if got, _ := s.repo.GetTask(ctx, tid); got.CompletedAt != nil {
		t.Fatalf("reopened task kept completed_at %v", got.CompletedAt)
	}
	task := s.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(tid)})
	if v, ok := task["completed_at"]; ok && v != nil {
		t.Fatalf("reopened task (actor view) completed_at = %v", v)
	}
	s.call(t, "amp_set_task_state", map[string]interface{}{"task_id": float64(tid), "state": "completed", "reason": "done after all"})
	if got, _ := s.repo.GetTask(ctx, tid); got.CompletedAt == nil {
		t.Fatal("manually completed task has no completed_at")
	}
}
