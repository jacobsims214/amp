// A ticket with a start_at becomes ready only when BOTH its start time has
// come AND every dependency is complete — whichever happens last releases it.
//
// Bug (AMP-1864): TaskActor.handleScheduledUnblock moves a scheduled ticket
// to backlog when its start_at arrives without looking at its dependencies,
// so a ticket waiting on unfinished work is released early and can be
// dispatched. Clearing the schedule (handleSetTaskStartAt with nil) does the
// same.
package main

import (
	"context"
	"testing"
	"time"

	protoactor "github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
)

// scheduleAt sets a ticket's start_at through its actor, exactly as
// amp_set_task_start_at does, but with sub-second precision.
func scheduleAt(t *testing.T, s *mcpStack, id int, at *time.Time) {
	t.Helper()
	reply := make(chan actor.ReplySimple, 1)
	if err := s.reg.Send(s.projectID, &actor.MsgSetTaskStartAt{TaskID: id, StartAt: at, ReplyCh: reply}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-reply:
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MsgSetTaskStartAt never answered")
	}
}

// waitState polls the database until the ticket reaches want, or fails.
func waitState(t *testing.T, s *mcpStack, id int, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := dbState(t, s.repo, id)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ticket %d: state %q, want %q", id, got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The start time arrives while a dependency is still unfinished: the ticket
// stays blocked (on its dependencies, no longer on its schedule), and is
// released only when the dependency completes.
func TestScheduledDeps_StartAtFiresWithUnfinishedDependencyStaysBlocked(t *testing.T) {
	s := setupMCP(t)
	dep := mkIn(t, s, s.storyID, "build API")
	id := mkIn(t, s, s.storyID, "build UI", dep)
	if st := stateOf(t, s, id); st != "blocked" {
		t.Fatalf("ticket with an unfinished dependency: %s", st)
	}

	at := time.Now().Add(time.Second)
	scheduleAt(t, s, id, &at)
	if st := stateOf(t, s, id); st != "blocked" {
		t.Fatalf("scheduled ticket: %s", st)
	}

	// Let the timer fire.
	time.Sleep(time.Until(at) + 500*time.Millisecond)

	task := planTask(t, s, id)
	if st := task["state"].(string); st != "blocked" {
		t.Fatalf("start_at passed with dependency %d unfinished: ticket is %q, want blocked", dep, st)
	}
	if by := blockedBy(t, s, id); len(by) != 1 || by[0] != dep {
		t.Fatalf("blocked_by_ids = %v, want [%d]", by, dep)
	}
	if st := dbState(t, s.repo, id); st != "blocked" {
		t.Fatalf("database says %q, want blocked", st)
	}
	// Nothing may dispatch it while it waits.
	s.callExpectError(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(id), "agent_id": "agent"})

	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(dep)})
	waitState(t, s, id, "backlog")
}

// The reverse order: the dependency completes first while the start time is
// still in the future — the ticket waits for its start time, then is ready.
func TestScheduledDeps_DependencyDoneFirstWaitsForStartAt(t *testing.T) {
	s := setupMCP(t)
	dep := mkIn(t, s, s.storyID, "build API")
	id := mkIn(t, s, s.storyID, "build UI", dep)

	at := time.Now().Add(1500 * time.Millisecond)
	scheduleAt(t, s, id, &at)

	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(dep)})
	if time.Now().After(at) {
		t.Skip("machine too slow: start_at passed before the dependency completed")
	}
	if st := stateOf(t, s, id); st != "blocked" {
		t.Fatalf("dependency done but start_at still ahead: ticket is %q, want blocked", st)
	}

	waitState(t, s, id, "backlog")
	if time.Now().Before(at) {
		t.Fatal("released before its start time")
	}
}

// The start time passed while no actor ran the project (a restart or a
// replica takeover). When the project's actors start again, the ticket must
// still wait on its unfinished dependency.
func TestScheduledDeps_StartAtPassedWhileDownWithUnfinishedDependency(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	dep := mkIn(t, s, s.storyID, "build API")
	id := mkIn(t, s, s.storyID, "build UI", dep)

	// What the database holds after a schedule that came due while down.
	past := time.Now().Add(-time.Minute)
	if err := s.repo.SetTaskStartAt(ctx, id, &past); err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SetTaskState(ctx, id, domain.TaskStateBlocked, "scheduled:"+past.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	// A fresh actor system loads the project from the database.
	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	reg := actor.NewRegistry(system, s.repo, hub.New())
	if err := reg.Touch(s.projectID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	if st := dbState(t, s.repo, id); st != "blocked" {
		t.Fatalf("restart after start_at with dependency %d unfinished: ticket is %q, want blocked", dep, st)
	}
}

// Clearing a ticket's schedule must not release it past its dependencies.
func TestScheduledDeps_ClearingScheduleKeepsDependencyBlock(t *testing.T) {
	s := setupMCP(t)
	dep := mkIn(t, s, s.storyID, "build API")
	id := mkIn(t, s, s.storyID, "build UI", dep)

	at := time.Now().Add(time.Hour)
	scheduleAt(t, s, id, &at)
	scheduleAt(t, s, id, nil)

	if st := stateOf(t, s, id); st != "blocked" {
		t.Fatalf("schedule cleared with dependency %d unfinished: ticket is %q, want blocked", dep, st)
	}

	s.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(dep)})
	waitState(t, s, id, "backlog")
}
