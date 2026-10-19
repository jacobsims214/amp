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
	"strings"
	"sync"
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

// releaseEvents records the task.blocked / task.unblocked events published
// for one ticket from now on — what the UI's SSE stream receives.
type releaseEvents struct {
	mu   sync.Mutex
	evts []domain.Task
}

func watchRelease(s *mcpStack, id int) *releaseEvents {
	r := &releaseEvents{}
	s.hub.AddSink(func(e domain.Event) {
		if e.Type != domain.EventTaskBlocked && e.Type != domain.EventTaskUnblocked {
			return
		}
		if t, ok := e.Payload.(domain.Task); ok && t.ID == id {
			r.mu.Lock()
			r.evts = append(r.evts, t)
			r.mu.Unlock()
		}
	})
	return r
}

// assertScheduleConsumed checks the fired schedule is gone everywhere: the
// release event carried no start_at (the UI applies the payload as-is, so a
// stale one shows "Starts N min ago" on a released ticket), and neither the
// actor nor the database still holds one.
func assertScheduleConsumed(t *testing.T, s *mcpStack, id int, r *releaseEvents) {
	t.Helper()
	r.mu.Lock()
	evts := append([]domain.Task(nil), r.evts...)
	r.mu.Unlock()
	if len(evts) == 0 {
		t.Fatalf("ticket %d: no task.blocked/task.unblocked event when its schedule fired", id)
	}
	for _, e := range evts {
		if e.StartAt != nil {
			t.Fatalf("ticket %d: release event carries start_at %v, want none", id, *e.StartAt)
		}
	}
	if v, ok := planTask(t, s, id)["start_at"]; ok && v != nil {
		t.Fatalf("ticket %d: actor still reports start_at %v after the schedule fired", id, v)
	}
	stored, err := s.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StartAt != nil {
		t.Fatalf("ticket %d: database still holds start_at %v", id, *stored.StartAt)
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
	release := watchRelease(s, id)

	// Let the timer fire.
	time.Sleep(time.Until(at) + 500*time.Millisecond)
	assertScheduleConsumed(t, s, id, release)

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
	release := watchRelease(s, id)

	waitState(t, s, id, "backlog")
	if time.Now().Before(at) {
		t.Fatal("released before its start time")
	}
	time.Sleep(200 * time.Millisecond) // the actor clears start_at in the DB just after the state
	assertScheduleConsumed(t, s, id, release)
}

// The start time passed while no actor ran the project (a restart or a
// replica takeover). When the project's actors start again, they must release
// the expired schedule (block_reason and start_at cleared), keep the ticket
// blocked on its unfinished dependency, and move it to backlog once that
// dependency completes. A ticket left on "scheduled:..." would never move.
func TestScheduledDeps_StartAtPassedWhileDownWithUnfinishedDependency(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	dep := mkIn(t, s, s.storyID, "build API")
	id := mkIn(t, s, s.storyID, "build UI", dep)

	// The process goes down: stop the original project actors. Otherwise two
	// live actor systems would own the project, and the original one (whose
	// in-memory ticket was never schedule-blocked) could release the ticket on
	// the dependency's completion by itself, which would hide a restart that
	// never ran the release. Everything below goes through the fresh registry
	// and the database only; s.call would respawn the original actors.
	s.reg.StopAll()
	time.Sleep(200 * time.Millisecond) // Root.Stop is asynchronous

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

	// On start, the expired schedule is released: no "scheduled:" reason and
	// no start_at left, but still blocked on the unfinished dependency.
	deadline := time.Now().Add(5 * time.Second)
	for {
		stored, err := s.repo.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.BlockReason == "" && stored.StartAt == nil {
			if stored.State != domain.TaskStateBlocked {
				t.Fatalf("restart after start_at with dependency %d unfinished: ticket is %q, want blocked", dep, stored.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart never released the expired schedule: state %q, block_reason %q, start_at %v",
				stored.State, stored.BlockReason, stored.StartAt)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Completing the dependency through the restarted actors hands the ticket
	// on to backlog: the release left it where checkUnblock picks it up.
	reply := make(chan actor.ReplySimple, 1)
	if err := reg.Send(s.projectID, &actor.MsgCompleteTask{TaskID: dep, ReplyCh: reply}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-reply:
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MsgCompleteTask never answered")
	}
	waitState(t, s, id, "backlog")
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

// AMP-1873: on start, a task actor re-arms a future start_at. It must do that
// only for a ticket still waiting to start (backlog, or blocked by its
// schedule or on its dependencies). A completed or in-progress ticket, or one
// blocked on a question, keeps its state and block reason even though its
// start_at is still in the future.
func TestScheduledDeps_RestartLeavesStartedTicketsAlone(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	scheduled := "scheduled:" + future.Format(time.RFC3339)

	cases := []struct {
		name       string
		state      domain.TaskState
		reason     string
		wantState  domain.TaskState
		wantReason string
	}{
		{"completed", domain.TaskStateCompleted, "", domain.TaskStateCompleted, ""},
		{"in progress", domain.TaskStateInProgress, "", domain.TaskStateInProgress, ""},
		{"blocked on a question", domain.TaskStateBlocked, "Avalara or Stripe?", domain.TaskStateBlocked, "Avalara or Stripe?"},
		// Backward compatible: tickets waiting to start still re-arm.
		{"backlog", domain.TaskStateBacklog, "", domain.TaskStateBlocked, scheduled},
		{"scheduled", domain.TaskStateBlocked, scheduled, domain.TaskStateBlocked, scheduled},
	}
	ids := make([]int, len(cases))
	for i, c := range cases {
		ids[i] = mkIn(t, s, s.storyID, c.name)
	}

	// The process goes down (see the restart test above for why the original
	// actors are stopped), and the database holds a future start_at on each.
	s.reg.StopAll()
	time.Sleep(200 * time.Millisecond) // Root.Stop is asynchronous
	for i, c := range cases {
		if err := s.repo.SetTaskStartAt(ctx, ids[i], &future); err != nil {
			t.Fatal(err)
		}
		if err := s.repo.SetTaskState(ctx, ids[i], c.state, c.reason); err != nil {
			t.Fatal(err)
		}
	}

	system := protoactor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	reg := actor.NewRegistry(system, s.repo, hub.New())
	if err := reg.Touch(s.projectID); err != nil {
		t.Fatal(err)
	}

	for i, c := range cases {
		// A reply from the task's actor means its Started handler has run.
		reply := make(chan actor.ReplyGetTask, 1)
		var got *domain.Task
		deadline := time.Now().Add(5 * time.Second)
		for got == nil {
			if err := reg.Send(s.projectID, &actor.MsgGetTask{TaskID: ids[i], ReplyCh: reply}); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-reply:
				if r.Err == nil && r.Task != nil {
					got = r.Task
				}
			case <-time.After(time.Second):
			}
			if got == nil && time.Now().After(deadline) {
				t.Fatalf("%s: restarted actor never answered MsgGetTask", c.name)
			}
			if got == nil {
				time.Sleep(50 * time.Millisecond)
			}
		}
		// The re-armed reason is rebuilt from the stored start_at, whose time
		// zone may differ from ours: match "scheduled:" reasons by prefix.
		reasonOK := func(r string) bool {
			if strings.HasPrefix(c.wantReason, "scheduled:") {
				return strings.HasPrefix(r, "scheduled:")
			}
			return r == c.wantReason
		}
		if got.State != c.wantState || !reasonOK(got.BlockReason) {
			t.Errorf("%s after restart (actor): state %q, block_reason %q; want %q, %q",
				c.name, got.State, got.BlockReason, c.wantState, c.wantReason)
		}
		stored, err := s.repo.GetTask(ctx, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != c.wantState || !reasonOK(stored.BlockReason) {
			t.Errorf("%s after restart (db): state %q, block_reason %q; want %q, %q",
				c.name, stored.State, stored.BlockReason, c.wantState, c.wantReason)
		}
		if stored.StartAt == nil {
			t.Errorf("%s after restart: start_at cleared, want it kept", c.name)
		}
	}
}

// Setting a future start_at on a ticket that has already started (in
// progress or completed) is refused and leaves it as it was: state, block
// reason and start_at. A backlog ticket is still scheduled as before.
func TestScheduledDeps_SetStartAtRefusedOnStartedTickets(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)

	setState := func(id int, state domain.TaskState) {
		t.Helper()
		reply := make(chan actor.ReplySimple, 1)
		if err := s.reg.Send(s.projectID, &actor.MsgSetTaskState{TaskID: id, State: state, ReplyCh: reply}); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-reply:
			if r.Err != nil {
				t.Fatal(r.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("MsgSetTaskState never answered")
		}
	}

	for _, state := range []domain.TaskState{domain.TaskStateInProgress, domain.TaskStateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			id := mkIn(t, s, s.storyID, "started "+string(state))
			setState(id, state)

			reply := make(chan actor.ReplySimple, 1)
			if err := s.reg.Send(s.projectID, &actor.MsgSetTaskStartAt{TaskID: id, StartAt: &future, ReplyCh: reply}); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case r := <-reply:
				err = r.Err
			case <-time.After(5 * time.Second):
				t.Fatal("MsgSetTaskStartAt never answered")
			}
			if err == nil || !strings.Contains(err.Error(), "hasn't started") {
				t.Fatalf("set start_at on %s ticket: err %v, want a refusal", state, err)
			}

			// The actor's view is unchanged...
			got := make(chan actor.ReplyGetTask, 1)
			if err := s.reg.Send(s.projectID, &actor.MsgGetTask{TaskID: id, ReplyCh: got}); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-got:
				if r.Err != nil {
					t.Fatal(r.Err)
				}
				if r.Task.State != state || r.Task.BlockReason != "" || r.Task.StartAt != nil {
					t.Errorf("actor: state %q, block_reason %q, start_at %v; want %q, \"\", nil",
						r.Task.State, r.Task.BlockReason, r.Task.StartAt, state)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("MsgGetTask never answered")
			}
			// ...and so is the database.
			stored, err := s.repo.GetTask(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != state || stored.BlockReason != "" || stored.StartAt != nil {
				t.Errorf("db: state %q, block_reason %q, start_at %v; want %q, \"\", nil",
					stored.State, stored.BlockReason, stored.StartAt, state)
			}

			// Clearing a schedule is still allowed.
			scheduleAt(t, s, id, nil)
		})
	}

	// Backward compatible: a backlog ticket is scheduled exactly as before.
	id := mkIn(t, s, s.storyID, "not started")
	scheduleAt(t, s, id, &future)
	waitState(t, s, id, string(domain.TaskStateBlocked))
	stored, err := s.repo.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.BlockReason, "scheduled:") || stored.StartAt == nil {
		t.Errorf("backlog ticket: block_reason %q, start_at %v; want scheduled", stored.BlockReason, stored.StartAt)
	}
}
