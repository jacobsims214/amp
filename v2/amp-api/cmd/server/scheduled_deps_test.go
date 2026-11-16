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
	"encoding/json"
	"fmt"
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

// stateWaitTimeout bounds waitState. A release crosses an actor message, the
// Postgres write it makes and a polling reader, and the canonical command
// (TEST_CLUSTER=1 go test -race) runs each test through two actor systems and
// two cluster nodes alongside the machine's other stacks: 5 s does not always
// carry it under that load (AMP #3688), so this matches the 30 s the other
// bounded waits in these suites use (AMP #3680 for the same reason).
const stateWaitTimeout = 30 * time.Second

// waitState polls the database until the ticket reaches want, or fails with
// what the row did hold: a timeout that reports only the state it wanted
// cannot be told apart from a different one (AMP #3688).
func waitState(t *testing.T, s *mcpStack, id int, want string) {
	t.Helper()
	deadline := time.Now().Add(stateWaitTimeout)
	for {
		got, err := s.repo.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.State) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s", missingState(id, want, stateWaitTimeout, got))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// missingState is waitState's failure message: the state it wanted and
// everything the row did hold. A timeout that reports only the missing state
// cannot be told apart from a different one (AMP #3688).
func missingState(id int, want string, timeout time.Duration, stored *domain.Task) string {
	return fmt.Sprintf("ticket %d: state %q, want %q within %s; %s",
		id, stored.State, want, timeout, describeTask(stored))
}

// describeTask names what a ticket's row holds, so a timeout says what was
// observed instead of only what did not arrive. Read through the repository
// (never the actor) so it works for a ticket whose actors are stopped.
func describeTask(t *domain.Task) string {
	return fmt.Sprintf("block_reason %q, start_at %s, blocked_by %v, updated_at %s",
		t.BlockReason, describeStartAt(t.StartAt), t.DependencyIDs, t.UpdatedAt.UTC().Format(time.RFC3339Nano))
}

// describeStartAt renders a start_at for a failure message: "none" when there
// is none, else its value and how long from now (negative = past).
func describeStartAt(at *time.Time) string {
	if at == nil {
		return "none"
	}
	return fmt.Sprintf("%s (in %s)", at.UTC().Format(time.RFC3339Nano), time.Until(*at).Round(time.Millisecond))
}

// releaseEvent is one task.blocked / task.unblocked event seen for a ticket:
// which one it was, and the payload the UI applies as-is.
type releaseEvent struct {
	Type domain.EventType
	Task domain.Task
}

// releaseEvents records the task.blocked / task.unblocked events published
// for one ticket from now on — what the UI's SSE stream receives.
type releaseEvents struct {
	mu   sync.Mutex
	evts []releaseEvent
}

func (r *releaseEvents) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.evts)
}

// all copies the events received so far, so a failure message can list them
// without holding the lock.
func (r *releaseEvents) all() []releaseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]releaseEvent(nil), r.evts...)
}

func watchRelease(s *mcpStack, id int) *releaseEvents {
	r := &releaseEvents{}
	s.hub.AddSink(func(e domain.Event) {
		if e.Type != domain.EventTaskBlocked && e.Type != domain.EventTaskUnblocked {
			return
		}
		if t, ok := e.Payload.(domain.Task); ok && t.ID == id {
			r.mu.Lock()
			r.evts = append(r.evts, releaseEvent{Type: e.Type, Task: t})
			r.mu.Unlock()
		}
	})
	return r
}

// observed renders the two things a fired schedule either produced or did not:
// the release events received so far, and the row's own state. A timeout that
// reports only the missing event cannot be told apart from a different one
// (AMP #3688: three production occurrences were byte-identical).
func observed(evts []releaseEvent, stored *domain.Task) string {
	got := make([]string, 0, len(evts))
	for _, e := range evts {
		got = append(got, fmt.Sprintf("%s(state %q, start_at %s)", e.Type, e.Task.State, describeStartAt(e.Task.StartAt)))
	}
	return fmt.Sprintf("received %d: [%s]; row: %s", len(evts), strings.Join(got, " "), describeTask(stored))
}

// missingRelease is the message when no release event ever arrived for a
// schedule that fired: what did arrive instead, if anything, and what the row
// holds (was start_at still stored?).
func missingRelease(id int, timeout time.Duration, evts []releaseEvent, stored *domain.Task) string {
	return fmt.Sprintf("ticket %d: no task.blocked/task.unblocked event within %s when its schedule fired; %s",
		id, timeout, observed(evts, stored))
}

// assertScheduleConsumed checks the fired schedule is gone everywhere: the
// release event carried no start_at (the UI applies the payload as-is, so a
// stale one shows "Starts N min ago" on a released ticket), and neither the
// actor nor the database still holds one.
func assertScheduleConsumed(t *testing.T, s *mcpStack, id int, r *releaseEvents) {
	t.Helper()
	evts := r.all()
	stored, err := s.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) == 0 {
		t.Fatalf("%s", missingRelease(id, scheduleWaitTimeout, evts, stored))
	}
	for _, e := range evts {
		if e.Task.StartAt != nil {
			t.Fatalf("ticket %d: release event carries start_at %v, want none; %s",
				id, *e.Task.StartAt, observed(evts, stored))
		}
	}
	if v, ok := planTask(t, s, id)["start_at"]; ok && v != nil {
		t.Fatalf("ticket %d: actor still reports start_at %v after the schedule fired; %s",
			id, v, observed(evts, stored))
	}
	if stored.StartAt != nil {
		t.Fatalf("ticket %d: database still holds start_at %v; %s",
			id, *stored.StartAt, observed(evts, stored))
	}
}

// scheduleWaitTimeout bounds waitScheduleFired: the same actor -> Postgres path
// as stateWaitTimeout, plus the release event's trip through the hub (in the
// cluster suite, across a second replica). 10 s does not always carry that
// under the canonical command's load (AMP #3688), so this matches the 30 s
// above, and #3680's precedent.
const scheduleWaitTimeout = 30 * time.Second

// waitScheduleFired waits for a ticket's timer to have fired and its effects to
// be visible (a release event published, start_at gone from the database),
// then checks them with assertScheduleConsumed. When the effects land after
// start_at depends on the machine's load, so this waits on them, not on time.
func waitScheduleFired(t *testing.T, s *mcpStack, id int, r *releaseEvents) {
	t.Helper()
	deadline := time.Now().Add(scheduleWaitTimeout)
	for time.Now().Before(deadline) {
		stored, err := s.repo.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.count() > 0 && stored.StartAt == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	assertScheduleConsumed(t, s, id, r)
}

// beforeStartAt runs setup, which schedules a ticket lead from now, checks
// what must hold before its start time (returning what did not), and starts
// watching for its release. Those checks mean something only if setup ended
// before the start time: on a loaded machine (the full suite, -race,
// TEST_CLUSTER) it may not have, and the timer may have fired partway through.
// Then it tries again with twice the lead, rather than failing or passing on
// what it saw after the start time.
func beforeStartAt(t *testing.T, setup func(lead time.Duration) (int, time.Time, *releaseEvents, []string)) (int, *releaseEvents) {
	t.Helper()
	for lead := 2 * time.Second; ; lead *= 2 {
		id, at, release, problems := setup(lead)
		if time.Now().Before(at) {
			if len(problems) > 0 {
				t.Fatal(strings.Join(problems, "\n"))
			}
			return id, release
		}
		if lead >= 16*time.Second {
			t.Fatalf("setup took longer than %v, past the start time it was checking against", lead)
		}
		t.Logf("setup ended after its start time (lead %v); trying again with a longer lead", lead)
	}
}

// callFails calls a tool and returns its error message, or "" if it succeeded.
func (s *mcpStack) callFails(toolName string, args map[string]interface{}) string {
	raw, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]interface{}{"name": toolName, "arguments": args},
	})
	respBytes, _ := json.Marshal(s.srv.HandleMessage(s.callCtx(), raw))
	var rpcResp struct {
		Error *struct{ Message string } `json:"error"`
	}
	_ = json.Unmarshal(respBytes, &rpcResp)
	if rpcResp.Error != nil {
		return rpcResp.Error.Message
	}
	return ""
}

// The start time arrives while a dependency is still unfinished: the ticket
// stays blocked (on its dependencies, no longer on its schedule), and is
// released only when the dependency completes.
func TestScheduledDeps_StartAtFiresWithUnfinishedDependencyStaysBlocked(t *testing.T) {
	s := setupMCP(t)
	var dep int
	id, release := beforeStartAt(t, func(lead time.Duration) (int, time.Time, *releaseEvents, []string) {
		dep = mkIn(t, s, s.storyID, "build API")
		id := mkIn(t, s, s.storyID, "build UI", dep)
		if st := stateOf(t, s, id); st != "blocked" {
			t.Fatalf("ticket with an unfinished dependency: %s", st)
		}

		at := time.Now().Add(lead)
		scheduleAt(t, s, id, &at)
		var problems []string
		if st := stateOf(t, s, id); st != "blocked" {
			problems = append(problems, "scheduled ticket: "+st)
		}
		// Watching from before the start time, so the release event is seen.
		return id, at, watchRelease(s, id), problems
	})

	// Let the timer fire.
	waitScheduleFired(t, s, id, release)

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

	s.finish(t, dep)
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

	s.finish(t, dep)
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
	// The actor clears start_at in the DB just after the state.
	waitScheduleFired(t, s, id, release)
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
	reply := make(chan actor.ReplyComplete, 1)
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

	s.finish(t, dep)
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

	// A ticket blocked on a question keeps its question: setting a start time
	// is refused and the ticket is left as it was (AMP #1915).
	t.Run("blocked on a question", func(t *testing.T) {
		const question = "Avalara or Stripe Tax?"
		id := mkIn(t, s, s.storyID, "question-blocked")
		reply := make(chan actor.ReplySimple, 1)
		if err := s.reg.Send(s.projectID, &actor.MsgSetTaskState{TaskID: id, State: domain.TaskStateBlocked, Reason: question, ReplyCh: reply}); err != nil {
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

		reply = make(chan actor.ReplySimple, 1)
		if err := s.reg.Send(s.projectID, &actor.MsgSetTaskStartAt{TaskID: id, StartAt: &future, ReplyCh: reply}); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-reply:
			if r.Err == nil || !strings.Contains(r.Err.Error(), "answer or unblock it first") {
				t.Fatalf("set start_at on question-blocked ticket: err %v, want a refusal", r.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("MsgSetTaskStartAt never answered")
		}

		got := make(chan actor.ReplyGetTask, 1)
		if err := s.reg.Send(s.projectID, &actor.MsgGetTask{TaskID: id, ReplyCh: got}); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-got:
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			if r.Task.State != domain.TaskStateBlocked || r.Task.BlockReason != question || r.Task.StartAt != nil {
				t.Errorf("actor: state %q, block_reason %q, start_at %v; want blocked, %q, nil",
					r.Task.State, r.Task.BlockReason, r.Task.StartAt, question)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("MsgGetTask never answered")
		}
		stored, err := s.repo.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != domain.TaskStateBlocked || stored.BlockReason != question || stored.StartAt != nil {
			t.Errorf("db: state %q, block_reason %q, start_at %v; want blocked, %q, nil",
				stored.State, stored.BlockReason, stored.StartAt, question)
		}

		// Clearing a schedule is still allowed and leaves the question alone.
		scheduleAt(t, s, id, nil)
		if stored, err := s.repo.GetTask(ctx, id); err != nil {
			t.Fatal(err)
		} else if stored.BlockReason != question {
			t.Errorf("after clearing: block_reason %q, want %q", stored.BlockReason, question)
		}
	})

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

// A scheduled ticket that gets a question keeps its schedule once the question
// is answered (AMP #1936): answering puts it back on its schedule (blocked
// "scheduled:...", timer armed, start_at unchanged) instead of making it ready
// before its start time, and the timer releases it as usual. Without a
// start_at, or with one already past, answering makes it ready as before.
func TestScheduledDeps_AnswerKeepsFutureSchedule(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()

	stored := func(id int) *domain.Task {
		t.Helper()
		task, err := s.repo.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	askAndAnswer := func(id int) map[string]interface{} {
		t.Helper()
		s.call(t, "amp_block_task", map[string]interface{}{"task_id": float64(id), "reason": "Avalara or Stripe Tax?"})
		if got := stored(id); got.State != domain.TaskStateBlocked || got.BlockReason != "Avalara or Stripe Tax?" {
			t.Fatalf("after the question: state %q, block_reason %q", got.State, got.BlockReason)
		}
		return s.call(t, "amp_answer_question", map[string]interface{}{"task_id": float64(id), "answer": "Stripe Tax."})
	}

	t.Run("future start_at", func(t *testing.T) {
		id, release := beforeStartAt(t, func(lead time.Duration) (int, time.Time, *releaseEvents, []string) {
			id := mkIn(t, s, s.storyID, "scheduled, then asked")
			at := time.Now().Add(lead).Truncate(time.Microsecond)
			scheduleAt(t, s, id, &at)

			var problems []string
			if out := askAndAnswer(id); out["state"] != "blocked" {
				problems = append(problems, fmt.Sprintf("answered before its start time: state %v, want blocked (on its schedule)", out["state"]))
			}
			got := stored(id)
			if got.State != domain.TaskStateBlocked || got.BlockReason != "scheduled:"+at.Format(time.RFC3339) {
				problems = append(problems, fmt.Sprintf("db: state %q, block_reason %q; want blocked scheduled:%s", got.State, got.BlockReason, at.Format(time.RFC3339)))
			}
			if got.StartAt == nil || !got.StartAt.Equal(at) {
				problems = append(problems, fmt.Sprintf("db: start_at %v, want %v unchanged", got.StartAt, at))
			}
			// Not dispatchable before its start time.
			if s.callFails("amp_dispatch_task", map[string]interface{}{"task_id": float64(id), "agent_id": "agent"}) == "" {
				problems = append(problems, "dispatched before its start time")
			}
			return id, at, watchRelease(s, id), problems
		})

		// The re-armed timer releases it.
		waitScheduleFired(t, s, id, release)
		waitState(t, s, id, string(domain.TaskStateBacklog))
	})

	t.Run("future start_at with an unfinished dependency", func(t *testing.T) {
		var dep int
		id, release := beforeStartAt(t, func(lead time.Duration) (int, time.Time, *releaseEvents, []string) {
			dep = mkIn(t, s, s.storyID, "dependency")
			id := mkIn(t, s, s.storyID, "scheduled with a dependency, then asked", dep)
			at := time.Now().Add(lead)
			scheduleAt(t, s, id, &at)

			askAndAnswer(id)
			var problems []string
			if got := stored(id); got.State != domain.TaskStateBlocked || !strings.HasPrefix(got.BlockReason, "scheduled:") {
				problems = append(problems, fmt.Sprintf("answered before its start time: state %q, block_reason %q; want blocked scheduled:...", got.State, got.BlockReason))
			}
			return id, at, watchRelease(s, id), problems
		})
		waitScheduleFired(t, s, id, release)
		// Released from its schedule but still waiting on its dependency.
		if got := stored(id); got.State != domain.TaskStateBlocked || got.BlockReason != "" {
			t.Fatalf("after start_at with dependency unfinished: state %q, block_reason %q; want blocked, \"\"", got.State, got.BlockReason)
		}
		s.finish(t, dep)
		waitState(t, s, id, string(domain.TaskStateBacklog))
	})

	t.Run("no start_at", func(t *testing.T) {
		id := mkIn(t, s, s.storyID, "asked, never scheduled")
		if out := askAndAnswer(id); out["state"] != "backlog" {
			t.Fatalf("after the answer: %v, want backlog", out["state"])
		}
		if got := stored(id); got.State != domain.TaskStateBacklog || got.BlockReason != "" || got.StartAt != nil {
			t.Fatalf("db: state %q, block_reason %q, start_at %v; want backlog, \"\", nil", got.State, got.BlockReason, got.StartAt)
		}
	})

	t.Run("past start_at", func(t *testing.T) {
		id := mkIn(t, s, s.storyID, "start_at passed while asked")
		// Its timer fires while the question holds it: the release is
		// refused (not held by its schedule), so start_at stays, now past.
		at := time.Now().Add(500 * time.Millisecond)
		scheduleAt(t, s, id, &at)
		s.call(t, "amp_block_task", map[string]interface{}{"task_id": float64(id), "reason": "Avalara or Stripe Tax?"})
		time.Sleep(time.Until(at) + 300*time.Millisecond)
		out := s.call(t, "amp_answer_question", map[string]interface{}{"task_id": float64(id), "answer": "Stripe Tax."})
		if out["state"] != "backlog" {
			t.Fatalf("answered after its start time: %v, want backlog", out["state"])
		}
		if got := stored(id); got.State != domain.TaskStateBacklog || got.BlockReason != "" {
			t.Fatalf("db: state %q, block_reason %q; want backlog, \"\"", got.State, got.BlockReason)
		}
	})
}

// TestScheduledDeps_WaitBoundsAndTimeoutsAreDiagnosable pins both halves of
// AMP #3688 without a stack, like #3680's sibling check for webhook deliveries:
// the bounds are long enough for the canonical cluster command on a loaded
// machine, and an expiry says what WAS observed instead of only what did not
// arrive (three production occurrences of the state wait were byte-identical,
// so none could be diagnosed). A future shrink of a bound, or a message that
// goes back to naming only the missing piece, fails here in milliseconds
// instead of as the next red run of the 225 s suite.
func TestScheduledDeps_WaitBoundsAndTimeoutsAreDiagnosable(t *testing.T) {
	for _, c := range []struct {
		name string
		got  time.Duration
	}{
		{"stateWaitTimeout", stateWaitTimeout},
		{"scheduleWaitTimeout", scheduleWaitTimeout},
	} {
		if c.got < 30*time.Second {
			t.Errorf("%s = %s, want at least 30s for the loaded TEST_CLUSTER run", c.name, c.got)
		}
	}

	// A row that never reached the wanted state, still holding the schedule it
	// was blocked on: the message must name what it found.
	at := time.Now().Add(2 * time.Minute)
	stored := &domain.Task{
		ID:            7,
		State:         domain.TaskStateBlocked,
		BlockReason:   "scheduled:" + at.Format(time.RFC3339),
		StartAt:       &at,
		DependencyIDs: []int{5},
		UpdatedAt:     time.Now(),
	}
	msg := missingState(7, "backlog", stateWaitTimeout, stored)
	for _, want := range []string{
		`state "blocked", want "backlog" within 30s`,
		`block_reason "scheduled:`,
		at.UTC().Format(time.RFC3339Nano), // the start_at it still held
		"blocked_by [5]",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("state timeout message = %q, want it to contain %q", msg, want)
		}
	}

	// No release event and start_at still stored: the message must say so, and
	// list the events it did get (here none) rather than only the missing one.
	noEvents := missingRelease(7, scheduleWaitTimeout, nil, stored)
	for _, want := range []string{
		"no task.blocked/task.unblocked event within 30s",
		"received 0: []",
		at.UTC().Format(time.RFC3339Nano), // start_at was still stored
	} {
		if !strings.Contains(noEvents, want) {
			t.Errorf("release timeout message = %q, want it to contain %q", noEvents, want)
		}
	}

	// Events did arrive but not the one the schedule should have produced: they
	// are listed, with the start_at each carried.
	released := at
	msg = missingRelease(7, scheduleWaitTimeout, []releaseEvent{
		{Type: domain.EventTaskUpdated, Task: domain.Task{ID: 7, State: domain.TaskStateBlocked}},
		{Type: domain.EventTaskBlocked, Task: domain.Task{ID: 7, State: domain.TaskStateBlocked, StartAt: &released}},
	}, stored)
	for _, want := range []string{
		"received 2: [task.updated(state \"blocked\", start_at none)",
		"task.blocked(state \"blocked\", start_at " + at.UTC().Format(time.RFC3339Nano),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("release timeout message with events = %q, want it to contain %q", msg, want)
		}
	}
}
