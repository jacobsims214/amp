package actor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
)

// TaskActor owns exactly one task's state machine.
//
// Responsibilities:
//   - Holds the task in memory (authoritative copy)
//   - Enforces valid state transitions (dispatch, complete, block)
//   - On MsgDepCompleted: checks its own deps — O(degree), not O(N)
//   - Notifies parent StoryActor on dispatch and completion (upward rollup)
//   - Persists every state change to postgres before updating memory
//   - Publishes SSE events and activity log entries on every transition
//   - Manages scheduled task timers (start_at → unblock at scheduled time)
//
// The task data is passed at construction via closure — no DB call on startup.
// The completedDeps set is seeded from the initial dep state provided by StoryActor.
type TaskActor struct {
	task          domain.Task
	completedDeps map[int]bool // which of task.DependencyIDs are already done
	// mergeWaits: dependencies that are done but whose merge isn't recorded
	// (they still hold this task), with why (repository.MergeWaitDetail).
	mergeWaits map[int]string
	detached   bool       // stopped for a move: don't deregister from routing
	storyPID   *actor.PID // parent StoryActor — receives upward notifications
	projectPID *actor.PID // ProjectActor — receives MsgRegisterTask on start
	projectID  int
	repo       *repository.Repo
	hub        *hub.Hub
	log        *slog.Logger
	// stateDispatchID is the dispatch token that MsgSetTaskState presented
	// ("" when it presented none), and stateInUI whether a person set it in
	// the web UI (never a closeout).
	stateDispatchID string
	stateInUI       bool
	scheduleTimer   *time.Timer        // non-nil if task is waiting for start_at
	selfPID         *actor.PID         // this actor's PID — used to send messages to self
	actorSystem     *actor.ActorSystem // actor system reference for sending messages from goroutines
}

// NewTaskActorFromTask constructs a TaskActor from an already-loaded task.
// completedDepIDs is the set of dep IDs already in completed state at spawn time,
// provided by StoryActor during startup so we don't re-query the DB.
func NewTaskActorFromTask(
	t domain.Task,
	completedDepIDs map[int]bool,
	storyPID *actor.PID,
	projectPID *actor.PID,
	repo *repository.Repo,
	h *hub.Hub,
) actor.Actor {
	return &TaskActor{
		task:          t,
		completedDeps: completedDepIDs,
		storyPID:      storyPID,
		projectPID:    projectPID,
		projectID:     t.ProjectID,
		repo:          repo,
		hub:           h,
		log:           slog.Default().With("actor", "task", "task_id", t.ID, "project_id", t.ProjectID),
	}
}

// Receive is the task actor's message loop. Called sequentially — no locks needed.
func (a *TaskActor) Receive(ctx actor.Context) {
	switch msg := ctx.Message().(type) {

	case *actor.Started:
		// Store self PID and actor system for timer callback to send messages to self
		a.selfPID = ctx.Self()
		a.actorSystem = ctx.ActorSystem()

		// Register with ProjectActor so it can route task-targeted messages.
		ctx.Send(a.projectPID, &MsgRegisterTask{TaskID: a.task.ID, EpicID: a.task.EpicID})

		// A task still waiting to start with a future start_at re-arms its
		// timer and is held by its schedule. Any other task keeps its state:
		// a stale start_at must not pull a completed or in-progress task (or
		// one blocked on a question) back to "scheduled".
		if a.task.StartAt != nil && a.task.StartAt.After(time.Now()) && a.waitingToStart() {
			a.armScheduleTimer()
			a.task.State = domain.TaskStateBlocked
			a.task.BlockReason = "scheduled:" + a.task.StartAt.Format(time.RFC3339)
			a.task.UpdatedAt = time.Now()

			if err := a.saveState(domain.TaskStateBlocked, a.task.BlockReason); err != nil {
				a.log.Error("failed to persist scheduled block state", "err", err)
			}
		} else if a.task.StartAt != nil && a.task.State == domain.TaskStateBlocked && strings.HasPrefix(a.task.BlockReason, "scheduled:") {
			// Its start time passed while no actor was running it (a restart,
			// or its project moving to another replica): unblock it now.
			ctx.Send(ctx.Self(), &MsgScheduledUnblock{TaskID: a.task.ID})
		}

		a.log.Info("started", "state", a.task.State, "deps", len(a.task.DependencyIDs))

	case *actor.Stopped:
		// A stopped actor's schedule is picked up by whichever actor runs the
		// ticket next.
		if a.scheduleTimer != nil {
			a.scheduleTimer.Stop()
			a.scheduleTimer = nil
		}
		// Deregister from ProjectActor routing index.
		if !a.detached {
			ctx.Send(a.projectPID, &MsgDeregisterTask{TaskID: a.task.ID, EpicID: a.task.EpicID})
		}

	case *MsgGetTask:
		t := a.withBlockedBy()
		msg.ReplyCh <- ReplyGetTask{Task: &t}

	case *MsgDispatchTask:
		err := a.handleDispatch(msg, ctx)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgCompleteTask:
		closeout, err := a.handleComplete(ctx, msg)
		msg.ReplyCh <- ReplyComplete{Task: ptrTask(a.withBlockedBy()), Closeout: closeout, Err: err}

	case *MsgClaimStart:
		started, err := a.handleClaimStart(msg)
		t := a.withBlockedBy()
		msg.ReplyCh <- ReplyStart{Task: &t, Started: started, DispatchID: a.task.DispatchID,
			AlreadyStarted: !started && a.task.StartedAt != nil, Err: err}

	case *MsgReturnTask:
		err := a.handleReturn(msg)
		t := a.withBlockedBy()
		msg.ReplyCh <- ReplyGetTask{Task: &t, Err: err}

	case *MsgBlockTask:
		err := a.handleBlock(msg.Reason, msg.By)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgAssignTask:
		t, err := a.handleAssign(msg, ctx)
		msg.ReplyCh <- ReplyGetTask{Task: t, Err: err}

	case *MsgUpdateTask:
		err := a.handleUpdate(msg.Req, msg.KeepHuman)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgSetTaskState:
		// A caller that presents the current dispatch is the worker; a
		// caller that doesn't is closing out somebody else's ticket.
		a.stateDispatchID, a.stateInUI = msg.DispatchID, msg.InUI
		if msg.RequireStarted && a.task.State != domain.TaskStateInProgress {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("task %d is %s, not in progress: it wasn't dispatched, so it can't be set to %s off the web UI",
				a.task.ID, a.task.State, msg.State)}
			return
		}
		state := msg.State
		if msg.RequireDeps {
			var err error
			if state, err = a.holdForDeps(msg.State); err != nil {
				msg.ReplyCh <- ReplySimple{Err: err}
				return
			}
		}
		// Cancel timer if armed before applying state change
		if a.scheduleTimer != nil {
			a.scheduleTimer.Stop()
			a.scheduleTimer = nil
		}
		reason := msg.Reason
		if state != msg.State {
			reason = "" // blocked on its dependencies: no reason of its own
		}
		err := a.handleSetState(state, reason, msg.By, msg.ByName)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgSetTaskStartAt:
		err := a.handleSetTaskStartAt(msg.StartAt)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgApproveTask:
		msg.ReplyCh <- a.handleApprove(msg)

	case *MsgAddComment:
		comment, err := a.handleAddComment(msg.Req)
		msg.ReplyCh <- ReplyAddComment{Comment: comment, Err: err}

	case *MsgGetComments:
		comments, err := a.repo.GetComments(context.Background(), a.task.ID)
		msg.ReplyCh <- ReplyGetComments{Comments: comments, Err: err}

	case *MsgSetTaskDeps:
		if err := a.repo.ReplaceTaskDependencies(context.Background(), a.task.ID, msg.DependencyIDs); err != nil {
			msg.ReplyCh <- ReplyGetTask{Err: fmt.Errorf("db set dependencies: %w", err)}
			return
		}
		a.task.DependencyIDs = append([]int(nil), msg.DependencyIDs...)
		if err := a.reconcileDeps("dependencies changed"); err != nil {
			msg.ReplyCh <- ReplyGetTask{Err: err}
			return
		}
		t := a.withBlockedBy()
		msg.ReplyCh <- ReplyGetTask{Task: &t}

	case *MsgReconcileDeps:
		deps, err := a.repo.TaskDependencies(context.Background(), a.task.ID)
		if err == nil {
			a.task.DependencyIDs = deps
			err = a.reconcileDeps("a dependency was removed")
		}
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgDepCompleted:
		if msg.MergeWait != "" {
			// A dependency is done but waits on its merge: it still holds
			// this task. Re-derive from the database (it may have been
			// released before, if the merge was reported after completion).
			if a.dependsOn(msg.CompletedTaskID) {
				if err := a.reconcileDeps("a dependency waits on its merge"); err != nil {
					a.log.Error("failed to reconcile on a merge wait", "err", err)
				}
				// The sender knows the PR even before its run is recorded.
				if !a.completedDeps[msg.CompletedTaskID] {
					if a.mergeWaits == nil {
						a.mergeWaits = map[int]string{}
					}
					a.mergeWaits[msg.CompletedTaskID] = msg.MergeWait
					a.publish(domain.EventTaskUpdated, a.withBlockedBy())
				}
			}
			return
		}
		// A dependency finished — check if we are now unblocked.
		a.completedDeps[msg.CompletedTaskID] = true
		delete(a.mergeWaits, msg.CompletedTaskID)
		a.checkUnblock(ctx)

	case *MsgSetMergeState:
		msg.ReplyCh <- a.handleSetMergeState(ctx, msg)

	case *MsgScheduledUnblock:
		// Timer fired — transition task from blocked to backlog
		a.handleScheduledUnblock(ctx)

	case *MsgDeleteTask:
		// Cancel timer if armed before stopping
		if a.scheduleTimer != nil {
			a.scheduleTimer.Stop()
			a.scheduleTimer = nil
		}
		// Parent is handling DB deletion (or a move); we just stop.
		a.detached = msg.Detach
		ctx.Stop(ctx.Self())
		msg.ReplyCh <- ReplySimple{}
	}
}

// ---- Handlers ----

func (a *TaskActor) handleDispatch(msg *MsgDispatchTask, ctx actor.Context) error {
	agentID := msg.AgentID
	// Ownership: only the owner works a ticket. Checked here, not just in
	// the handler, so a claim that lands between the handler's check and
	// this message can't be overridden.
	claim := false
	if msg.By != nil {
		switch {
		case a.task.OwnerUserID == nil:
			claim = true
		case *a.task.OwnerUserID != *msg.By && !msg.Override:
			return fmt.Errorf("task %d is owned by user %d; it can only be dispatched by its owner", a.task.ID, *a.task.OwnerUserID)
		}
	}
	switch {
	case a.task.IsHuman() && !msg.Human:
		return fmt.Errorf("task %d is human work (%s): %s, not an agent. They start and finish it in AMP; "+
			"once it's done, the tickets that depend on it are ready", a.task.ID, a.task.HumanReason, a.ownerPhrase())
	case !a.task.IsHuman() && msg.Human:
		return fmt.Errorf("task %d is agent work; switch it to a person (performer \"human\") before starting it yourself", a.task.ID)
	}
	if msg.Human {
		agentID = domain.HumanAgentID
	}
	if a.task.State == domain.TaskStateInProgress {
		return fmt.Errorf("task %d is already in_progress (agent: %s)", a.task.ID, a.task.AgentID)
	}
	if a.task.State == domain.TaskStateCompleted {
		return fmt.Errorf("task %d is already completed", a.task.ID)
	}
	// Work never starts while a dependency is incomplete, whatever the
	// ticket's state says: a ticket can reach backlog without its
	// dependencies done (a state override, a dependency reopened after it
	// completed), and the state alone would let it through. The states are
	// re-read here, in the actor, so the check is atomic with the dispatch.
	open, err := a.openDeps()
	if err != nil {
		return fmt.Errorf("task %d: couldn't check its dependencies (%v); nothing was started, try again", a.task.ID, err)
	}
	if len(open) > 0 {
		return fmt.Errorf("task %d is blocked — waiting on task IDs: %v (its dependencies aren't complete, so it can't start)", a.task.ID, open)
	}
	if a.task.State == domain.TaskStateBlocked {
		return fmt.Errorf("task %d is blocked (%s); it can't start until it's unblocked", a.task.ID, a.task.BlockReason)
	}

	now := time.Now()
	if claim {
		// Dispatch's claim keeps the approval (dispatch already passed the
		// approval gate, and completing it checks again): AMP #2218.
		if err := a.repo.SetTaskOwner(context.Background(), a.task.ID, msg.By, false); err != nil {
			return fmt.Errorf("db claim on dispatch: %w", err)
		}
		owner := *msg.By
		a.task.OwnerUserID = &owner
		a.logActivityBy(agentID, msg.By, "claimed", "", "", fmt.Sprintf("Claimed by user %d on dispatch", owner))
	}
	a.task.State = domain.TaskStateInProgress
	a.task.AgentID = agentID
	a.task.DispatchedAt = &now
	a.task.UpdatedAt = now

	if err := a.saveState(domain.TaskStateInProgress, ""); err != nil {
		return fmt.Errorf("db dispatch: %w", err)
	}

	// Dispatch only marks the ticket dispatched (AMP #2801): its worker
	// starts it with a claim, and until then started_at stays NULL, which is
	// what the watchdog flags and returns. A person starting their own human
	// ticket is the exception: they are working it now, so it starts here.
	if msg.Human {
		startedAt := now
		if err := a.repo.SetTaskDispatch(context.Background(), a.task.ID, agentID, "", now); err != nil {
			return fmt.Errorf("db dispatch: %w", err)
		}
		if at, err := a.repo.StartTask(context.Background(), a.task.ID, agentID); err != nil {
			return fmt.Errorf("db start: %w", err)
		} else {
			startedAt = at
		}
		a.task.DispatchID = ""
		a.task.StartedAt, a.task.StartedBy = &startedAt, agentID
		a.task.ReturnedAt, a.task.ReturnedReason = nil, ""
	} else {
		dispatchID := msg.DispatchID
		if dispatchID == "" {
			id, err := repository.NewDispatchID()
			if err != nil {
				return fmt.Errorf("dispatch id: %w", err)
			}
			dispatchID = id
		}
		if err := a.repo.SetTaskDispatch(context.Background(), a.task.ID, agentID, dispatchID, now); err != nil {
			return fmt.Errorf("db dispatch: %w", err)
		}
		a.task.DispatchID = dispatchID
		a.task.StartedAt, a.task.StartedBy = nil, ""
		a.task.ReturnedAt, a.task.ReturnedReason = nil, ""
	}

	a.publish(domain.EventTaskDispatched, a.withBlockedBy())
	if msg.Human {
		a.logActivityBy(agentID, msg.By, "started", "backlog", "in_progress", "Started by its owner (human work)")
	} else {
		a.logActivityBy(agentID, msg.By, "dispatched", "backlog", "in_progress",
			fmt.Sprintf("Dispatched to agent %q", agentID))
	}
	a.log.Info("dispatched", "agent", agentID, "started", a.task.StartedAt != nil)

	// Notify parent StoryActor so it can roll up to in_progress.
	ctx.Send(a.storyPID, &MsgTaskDispatched{TaskID: a.task.ID})
	return nil
}

// handleComplete completes the ticket. It returns whether the completion was
// recorded as a closeout: a caller that isn't the ticket's worker (no
// dispatch_id, or not the current one), or a ticket no worker ever started
// (AMP #2801).
func (a *TaskActor) handleComplete(ctx actor.Context, msg *MsgCompleteTask) (bool, error) {
	by := msg.By
	if a.task.State == domain.TaskStateCompleted {
		// Idempotent. A repeat is the worker's only if it presents the
		// dispatch that completed it, which the ticket no longer carries.
		return !a.task.IsHuman(), nil
	}
	if a.task.State != domain.TaskStateInProgress && !msg.CloseoutOK {
		if msg.RequireStarted {
			return false, fmt.Errorf("task %d is %s: it was never dispatched, so it can't be completed off the web UI; dispatch it first, "+
				"or close it out with closeout_reason if its worker already finished it", a.task.ID, a.task.State)
		}
	}
	if msg.RequireDeps {
		if err := a.refuseOpenDeps("completed"); err != nil {
			return false, err
		}
	}
	// Closeout: the caller isn't the worker that was dispatched. A worker's
	// own completion presents the current dispatch token; anything else is
	// somebody closing the ticket out for it, which is allowed but labelled.
	//
	// A ticket whose start was inferred (or backfilled) is the exception: no
	// worker ever presented a token there, because its plugin doesn't claim.
	// Its first activity was the start and this completion is that same
	// run's end, so labelling it a closeout would be false for every older
	// plugin (AMP #2801, backward compatibility).
	closeout := !msg.InUI && a.task.CompleteIsCloseout(msg.DispatchID)
	if msg.DispatchID != "" && !a.task.CurrentDispatch(msg.DispatchID) && a.task.DispatchID != "" {
		return false, fmt.Errorf("task %d: dispatch %q is no longer current (the ticket was returned and re-dispatched as %q); "+
			"re-read the ticket and use the dispatch_id in your prompt, or close it out with closeout_reason if you aren't its worker",
			a.task.ID, msg.DispatchID, a.task.DispatchID)
	}

	prevState := a.task.State // capture before mutation for accurate activity log
	if a.task.State == domain.TaskStateInProgress && a.task.StartedAt == nil {
		// Completion is one of the fallback start signals (AMP #2801): a
		// plugin that doesn't claim still counts as started, so the record
		// says when. The worker's own, or a closeout's by the closer.
		by := domain.StartedByInferred
		if closeout {
			by = domain.StartedByCloseout
		}
		if at, err := a.repo.StartTask(context.Background(), a.task.ID, by); err == nil {
			a.task.StartedAt, a.task.StartedBy = &at, by
		}
	}
	now := time.Now()
	a.task.State = domain.TaskStateCompleted
	a.task.CompletedAt = &now
	a.task.UpdatedAt = now
	a.task.DispatchID = "" // a done ticket has no current dispatch

	if err := a.saveState(domain.TaskStateCompleted, ""); err != nil {
		return false, fmt.Errorf("db complete: %w", err)
	}
	mergeWait := ""
	if msg.MergeRequired {
		// Done, but its work waits on a merge: the tickets after it keep
		// waiting until the merge is recorded (AMP #2475).
		yes := true
		if err := a.repo.SetTaskMerge(context.Background(), a.task.ID, &yes, domain.MergeNeeded, nil, ""); err != nil {
			return closeout, fmt.Errorf("db merge needed: %w", err)
		}
		a.task.MergeRequired, a.task.MergeState, a.task.MergedAt, a.task.MergeSource = true, domain.MergeNeeded, nil, ""
		mergeWait = repository.MergeWaitDetail(a.task.ID, domain.MergeNeeded, msg.MergeWhere)
	} else if domain.MergeGates(a.task.MergeState) {
		// Reopened and done again while its earlier work still waits on a
		// merge: it keeps holding its dependents.
		mergeWait = repository.MergeWaitDetail(a.task.ID, a.task.MergeState, "")
	}

	a.publish(domain.EventTaskCompleted, a.withBlockedBy())
	who := nameOr(msg.ByName, "agent (no dispatch)")
	switch {
	case a.task.IsHuman():
		a.logActivityBy(domain.HumanAgentID, by, "completed", string(prevState), "completed", "Done by its owner (human work)")
	case closeout:
		// Not the worker's own completion: labelled, with why if the caller
		// said (AMP #2801). Never dressed up as the worker's work.
		detail := fmt.Sprintf("Closed out by %s (not the worker)", who)
		if reason := strings.TrimSpace(msg.CloseoutReason); reason != "" {
			detail += ": " + reason
		}
		a.logActivityBy(who, by, "closed_out", string(prevState), "completed", detail)
	case a.task.AgentID != "":
		a.logActivityBy(a.task.AgentID, by, "completed", string(prevState), "completed",
			fmt.Sprintf("Task completed by agent %q", a.task.AgentID))
	default:
		a.logActivityBy(who, by, "completed", string(prevState), "completed",
			fmt.Sprintf("Task completed by %s without a dispatch", who))
	}
	a.log.Info("completed", "closeout", closeout)

	// Synchronously ask ProjectActor to fan MsgDepCompleted to ALL epics/stories/tasks.
	// We WAIT for this to complete before returning, so that any caller who reads
	// task state after MsgCompleteTask returns will see the unblocked state immediately.
	// With a merge needed the fan-out tells dependents why they still wait.
	fanReplyCh := make(chan ReplySimple, 1)
	ctx.Send(a.projectPID, &MsgFanDepCompleted{CompletedTaskID: a.task.ID, MergeWait: mergeWait, ReplyCh: fanReplyCh})
	<-fanReplyCh
	if msg.MergeRequired {
		a.logActivity("system", "merge_needed", "", "", "Done; its work waits on a merge ("+nameOr(msg.MergeWhere, "pushed work")+
			"), so the tickets that depend on it wait until the merge is recorded")
	}

	// Then notify parent StoryActor for state rollup (async is fine for rollup).
	ctx.Send(a.storyPID, &MsgTaskCompleted{TaskID: a.task.ID})
	return closeout, nil
}

// handleClaimStart starts the ticket its worker was dispatched for
// (amp_claim_task with a dispatch_id), or starts it for an older plugin
// whose first activity counts as the start (Inferred). It returns whether
// this call started it; starting an already-started ticket is a no-op, not
// an error (a retried claim is idempotent).
func (a *TaskActor) handleClaimStart(msg *MsgClaimStart) (bool, error) {
	// An inferred start never refuses: it is the old plugins' path, and a
	// refusal there would break work that used to succeed.
	if a.task.State != domain.TaskStateInProgress {
		if msg.Inferred {
			return false, nil
		}
		return false, fmt.Errorf("task %d is %s, not dispatched: a claim starts the ticket you were dispatched for, and this one isn't running; "+
			"re-read it with amp_get_task and ask your manager to dispatch it", a.task.ID, a.task.State)
	}
	// A token that isn't the current one: the ticket was returned and
	// dispatched again, so this caller is a worker from an earlier run.
	if msg.DispatchID != "" && !a.task.CurrentDispatch(msg.DispatchID) {
		current := "none (the ticket was returned to ready)"
		if a.task.DispatchID != "" {
			current = a.task.DispatchID
		}
		return false, fmt.Errorf("task %d: dispatch %q is no longer current (current: %s). The ticket was returned and dispatched again, "+
			"so this run is over: stop, and tell your manager what you found instead of working it", a.task.ID, msg.DispatchID, current)
	}
	if a.task.StartedAt != nil {
		return false, nil // already started: idempotent
	}
	// started_by says how it started: the agent that claimed it, or
	// "inferred" for a plugin whose first activity is the start (its agent
	// is already the ticket's agent_id).
	by := msg.By
	if msg.Inferred || by == "" {
		by = domain.StartedByInferred
	}
	at, err := a.repo.StartTask(context.Background(), a.task.ID, by)
	if err != nil {
		return false, fmt.Errorf("db start: %w", err)
	}
	a.task.StartedAt, a.task.StartedBy = &at, by
	a.task.UpdatedAt = time.Now()
	a.publish(domain.EventTaskUpdated, a.withBlockedBy())
	// The first activity of an old plugin starts the ticket silently: there
	// is no claim to show, and its own comment is already the record.
	if !msg.Inferred {
		a.logActivityBy(by, msg.ByUser, "started", "", "", "picked this up")
		if msg.Post != "" {
			if _, err := a.handleAddComment(domain.AddCommentRequest{
				TaskID: a.task.ID, Body: msg.Post, Author: by, AuthorUserID: msg.ByUser,
			}); err != nil {
				a.log.Error("claim comment failed", "err", err)
			}
		}
		a.log.Info("started by claim", "agent", by)
	}
	return true, nil
}

// handleReturn sends a dispatched ticket back to ready because no worker
// started it: the watchdog's automatic return, or a harness releasing a
// ticket whose worker ended without closing it (amp_return_task). It clears
// the dispatch, stamps the return, and leaves the state ready (or blocked
// when a dependency is open, as any ticket moved back is).
func (a *TaskActor) handleReturn(msg *MsgReturnTask) error {
	if a.task.State != domain.TaskStateInProgress {
		return fmt.Errorf("task %d is %s, not in progress: only a dispatched ticket can be returned", a.task.ID, a.task.State)
	}
	if a.task.StartedAt != nil {
		// A started ticket is somebody's work: returning it would cancel a
		// live run behind its back. The caller blocks it or completes it.
		return fmt.Errorf("task %d was started (by %s): returning it would pull it out from under a worker that is running. "+
			"Block it with a question, or close it out with closeout_reason", a.task.ID, a.task.StartedBy)
	}
	reason := strings.TrimSpace(msg.Reason)
	if reason == "" {
		return fmt.Errorf("task %d: a reason is required; it's what the manager and the history see", a.task.ID)
	}
	from := string(a.task.State)
	if err := a.repo.ReturnTask(context.Background(), a.task.ID, reason); err != nil {
		return fmt.Errorf("db return: %w", err)
	}
	a.task.DispatchID, a.task.DispatchedAt = "", nil
	now := time.Now()
	a.task.ReturnedAt, a.task.ReturnedReason = &now, reason
	a.task.StartedAt, a.task.StartedBy = nil, ""

	state := domain.TaskStateBacklog
	if open, err := a.openDeps(); err == nil && len(open) > 0 {
		state = domain.TaskStateBlocked
	}
	if err := a.handleSetState(state, "", msg.By, nameOr(msg.ByName, "system")); err != nil {
		return err
	}
	byName := nameOr(msg.ByName, "system")
	if byName != "" {
		a.logActivityBy(byName, msg.By, "returned", from, string(state), "Returned to ready: "+reason)
	}
	a.log.Info("returned", "reason", reason)
	return nil
}

// ptrTask returns a pointer to a copy of t, for replies.
func ptrTask(t domain.Task) *domain.Task { return &t }

// ownerPhrase names who does a human ticket, for refusals.
func (a *TaskActor) ownerPhrase() string {
	if a.task.OwnerUserID == nil {
		return "it has no owner yet; assign the person who does it"
	}
	return fmt.Sprintf("its owner (user %d) does it", *a.task.OwnerUserID)
}

func (a *TaskActor) handleBlock(reason string, by *int) error {
	if a.task.State == domain.TaskStateInProgress && a.task.StartedAt == nil {
		// A block is one of the fallback start signals (AMP #2801): a worker
		// whose plugin doesn't claim is still visibly working the ticket.
		by := domain.StartedByInferred
		if at, err := a.repo.StartTask(context.Background(), a.task.ID, by); err == nil {
			a.task.StartedAt, a.task.StartedBy = &at, by
		}
	}
	a.task.State = domain.TaskStateBlocked
	a.task.BlockReason = reason
	a.task.UpdatedAt = time.Now()

	if err := a.saveState(domain.TaskStateBlocked, reason); err != nil {
		return fmt.Errorf("db block: %w", err)
	}

	a.publish(domain.EventTaskBlocked, a.withBlockedBy())
	a.logActivityBy("system", by, "blocked", "", "blocked", reason)
	a.log.Info("blocked", "reason", reason)
	return nil
}

func (a *TaskActor) handleUpdate(req domain.UpdateTaskRequest, keepHuman string) error {
	clr, err := domain.ParseClears(req.Clear, domain.TaskClearable)
	if err != nil {
		return err
	}
	// A person's ticket becomes agent work only by that choice in the web UI;
	// otherwise an agent could flip it, dispatch it and complete it. Checked
	// before anything changes, so a refusal leaves the ticket as it was.
	if keepHuman != "" && a.task.IsHuman() && req.Performer != "" {
		if p, _, err := domain.NormalizePerformer(req.Performer, ""); err == nil && p == domain.PerformerAgent {
			return errors.New(keepHuman)
		}
	}
	if req.Name != "" {
		a.task.Name = req.Name
	}
	a.task.Description = clr.Apply("description", req.Description, a.task.Description)
	a.task.AssignedTo = clr.Apply("assigned_to", req.AssignedTo, a.task.AssignedTo)
	a.task.AgentID = clr.Apply("agent_id", req.AgentID, a.task.AgentID)
	a.task.AcceptanceCriteria = clr.Apply("acceptance_criteria", req.AcceptanceCriteria, a.task.AcceptanceCriteria)
	if req.Priority != "" {
		a.task.Priority = req.Priority
	}
	// Who does it: switching is refused mid-work, so an agent's run or a
	// person's work isn't cut off by the change.
	prevPerformer := a.task.Performer
	if req.Performer != "" || req.HumanReason != "" {
		performer := req.Performer
		if performer == "" {
			performer = a.task.Performer
		}
		p, reason, err := domain.NormalizePerformer(performer, req.HumanReason)
		if err != nil {
			return err
		}
		if p != a.task.Performer && a.task.State == domain.TaskStateInProgress {
			return fmt.Errorf("task %d is in progress; finish or stop it before changing who does it", a.task.ID)
		}
		if p == domain.PerformerHuman && req.HumanReason == "" && a.task.HumanReason != "" {
			reason = a.task.HumanReason // keep the reason it had
		}
		req.Performer, req.HumanReason = p, reason
		a.task.Performer, a.task.HumanReason = p, reason
	} else if clr["human_reason"] && a.task.IsHuman() {
		return fmt.Errorf("a human ticket keeps a reason; set human_reason instead of clearing it")
	}
	a.task.UpdatedAt = time.Now()

	planned, err := a.repo.UpdateTaskPlanned(context.Background(), req)
	if err != nil {
		return fmt.Errorf("db update: %w", err)
	}
	// A change to what the ticket is (name, description, acceptance
	// criteria) re-plans it, so the last plan approval no longer covers it.
	// The database decides (Repo.UpdateTaskPlanned), on the same clock as
	// plan_approved_at.
	a.task.PlannedAt = planned
	if prevPerformer != a.task.Performer {
		who := map[string]string{domain.PerformerAgent: "an agent", domain.PerformerHuman: "a person"}
		detail := fmt.Sprintf("Now done by %s (was %s)", who[a.task.Performer], who[prevPerformer])
		if a.task.IsHuman() {
			detail += ": " + a.task.HumanReason
		}
		a.logActivity("system", "performer_changed", "", "", detail)
	}
	if req.Metadata != nil {
		meta, err := a.repo.MergeMetadata(context.Background(), "task", a.task.ID, req.Metadata)
		if err != nil {
			return err
		}
		a.task.Metadata = meta
	}

	a.publish(domain.EventTaskUpdated, a.withBlockedBy())
	return nil
}

// saveState writes the task's state to the database and keeps the in-memory
// completion time in step with it (the database stamps completed_at).
func (a *TaskActor) saveState(state domain.TaskState, blockReason string) error {
	if err := a.repo.SetTaskState(context.Background(), a.task.ID, state, blockReason); err != nil {
		return err
	}
	switch {
	case state != domain.TaskStateCompleted:
		a.task.CompletedAt = nil
	case a.task.CompletedAt == nil:
		now := time.Now()
		a.task.CompletedAt = &now
	}
	return nil
}

func (a *TaskActor) handleSetState(state domain.TaskState, reason string, by *int, byName string) error {
	if byName == "" {
		byName = "manager"
	}
	from := string(a.task.State)
	action, detail := "state_change", reason
	if state == domain.TaskStateCompleted && a.task.State != domain.TaskStateCompleted && !a.task.IsHuman() {
		// A completion made this way is the worker's only when it presented
		// the current dispatch; anything else is a closeout (AMP #2801).
		// Completion is also a fallback start signal.
		closeout := !a.stateInUI && a.task.CompleteIsCloseout(a.stateDispatchID)
		if a.task.StartedAt == nil && a.task.State == domain.TaskStateInProgress {
			startBy := domain.StartedByInferred
			if closeout {
				startBy = domain.StartedByCloseout
			}
			if at, err := a.repo.StartTask(context.Background(), a.task.ID, startBy); err == nil {
				a.task.StartedAt, a.task.StartedBy = &at, startBy
			}
		}
		if closeout {
			// Labelled, so history never reads as the worker's own work.
			action = "closed_out"
			detail = fmt.Sprintf("Closed out by %s (not the worker)", byName)
			if why := strings.TrimSpace(reason); why != "" {
				detail += ": " + why
			}
		}
	}
	a.task.DispatchID = ""
	a.task.State = state
	a.task.UpdatedAt = time.Now()
	// A block reason only means something while blocked; leaving the old
	// one on an unblocked ticket would keep showing it as flagged.
	blockReason := ""
	if state == domain.TaskStateBlocked {
		blockReason = reason
	}
	a.task.BlockReason = blockReason

	if err := a.saveState(state, blockReason); err != nil {
		return fmt.Errorf("db set state: %w", err)
	}
	if err := a.repo.LogActivity(context.Background(), domain.ActivityLog{
		TaskID:    a.task.ID,
		ProjectID: a.projectID,
		Actor:     byName,
		Action:    action,
		FromState: from,
		ToState:   string(state),
		Detail:    detail,
		// Who overrode the state, when known.
		ActorUserID: by,
	}); err != nil {
		a.log.Error("failed to log "+action+" activity", "err", err)
	}

	a.publish(domain.EventTaskUpdated, a.withBlockedBy())
	return nil
}

func (a *TaskActor) handleSetTaskStartAt(startAt *time.Time) error {
	// A schedule says when a ticket may start, so it applies only to tickets
	// that haven't started. Scheduling one already in progress or completed
	// would block it again; refuse and leave it untouched. Clearing (nil) is
	// still allowed, so a stale start_at can be removed.
	if startAt != nil && (a.task.State == domain.TaskStateInProgress || a.task.State == domain.TaskStateCompleted) {
		return fmt.Errorf("task %d is %s: a start time can only be set on a ticket that hasn't started (backlog or blocked)",
			a.task.ID, a.task.State)
	}
	// A ticket blocked for a reason other than its schedule or dependencies (a
	// question, a hand block) isn't waiting to start: scheduling it would
	// replace that reason with "scheduled:..." and lose the question. Refuse,
	// as restart does by leaving it alone (waitingToStart); once the question
	// is answered the ticket can be scheduled.
	if startAt != nil && a.task.State == domain.TaskStateBlocked && !a.waitingToStart() {
		return fmt.Errorf("task %d is blocked (%q): answer or unblock it first, then set its start time",
			a.task.ID, a.task.BlockReason)
	}

	// Cancel existing timer if armed
	if a.scheduleTimer != nil {
		a.scheduleTimer.Stop()
		a.scheduleTimer = nil
	}

	// Update in-memory task
	a.task.StartAt = startAt
	a.task.UpdatedAt = time.Now()

	// Persist to database
	if err := a.repo.SetTaskStartAt(context.Background(), a.task.ID, startAt); err != nil {
		return fmt.Errorf("db set start_at: %w", err)
	}

	// If new start_at is in the future, arm the timer and block the task
	if startAt != nil && startAt.After(time.Now()) {
		a.armScheduleTimer()
		a.task.State = domain.TaskStateBlocked
		a.task.BlockReason = "scheduled:" + startAt.Format(time.RFC3339)

		if err := a.saveState(domain.TaskStateBlocked, a.task.BlockReason); err != nil {
			return fmt.Errorf("db set scheduled block state: %w", err)
		}

		a.log.Info("scheduled task", "start_at", startAt)
	} else if startAt == nil {
		// Clearing the schedule — a task blocked only by its schedule is
		// released, unless it still waits on its dependencies.
		if a.task.State == domain.TaskStateBlocked && strings.HasPrefix(a.task.BlockReason, "scheduled:") {
			if err := a.releaseFromSchedule("Schedule cleared"); err != nil {
				return fmt.Errorf("db unblock task: %w", err)
			}
			a.log.Info("cleared schedule, released task from its schedule", "state", a.task.State)
			return nil
		}
	}

	a.publish(domain.EventTaskUpdated, a.withBlockedBy())
	return nil
}

func (a *TaskActor) handleAddComment(req domain.AddCommentRequest) (*domain.Comment, error) {
	comment, err := a.repo.AddComment(context.Background(), req)
	if err != nil {
		return nil, fmt.Errorf("db add comment: %w", err)
	}
	a.publish(domain.EventCommentAdded, comment)
	a.logActivityBy(req.Author, req.AuthorUserID, "comment", "", "", req.Body)
	return comment, nil
}

// handleAssign applies an ownership change as a compare-and-set against
// the owner the caller was authorised for.
func (a *TaskActor) handleAssign(msg *MsgAssignTask, ctx actor.Context) (*domain.Task, error) {
	if !sameOwner(a.task.OwnerUserID, msg.ExpectOwner) {
		return nil, fmt.Errorf("task %d: owner changed to %s while you were acting; reload and retry",
			a.task.ID, ownerString(a.task.OwnerUserID))
	}
	if msg.RequireIdle && a.task.State == domain.TaskStateInProgress {
		return nil, fmt.Errorf("task %d was started while you were acting; only its owner, a team lead, or a project owner can reassign it now", a.task.ID)
	}
	if sameOwner(a.task.OwnerUserID, msg.NewOwner) {
		t := a.withBlockedBy()
		return &t, nil // no-op (e.g. claiming a ticket you already own)
	}
	drop := a.task.DropsApprovalOn(msg.NewOwner, msg.ByLead)
	wasApproved := a.task.Approved()
	if err := a.repo.SetTaskOwner(context.Background(), a.task.ID, msg.NewOwner, drop); err != nil {
		return nil, fmt.Errorf("db set owner: %w", err)
	}
	from := ownerString(a.task.OwnerUserID)
	approvedFor := a.task.ApprovedFor
	a.task.OwnerUserID = msg.NewOwner
	a.task.UpdatedAt = time.Now()
	if drop {
		a.task.ApprovedAt, a.task.ApprovedBy, a.task.ApprovedFor = nil, nil, nil
	}

	detail := fmt.Sprintf("Owner %s → %s", from, ownerString(msg.NewOwner))
	if msg.NewOwner == nil && a.task.State == domain.TaskStateInProgress {
		// Released mid-flight: nobody is working it any more, and its
		// dispatch (if no worker started it) is over with it (AMP #2801).
		a.task.State = domain.TaskStateBacklog
		a.task.AgentID = ""
		a.task.DispatchedAt = nil
		a.task.DispatchID = ""
		a.task.StartedAt, a.task.StartedBy = nil, ""
		if err := a.saveState(domain.TaskStateBacklog, ""); err != nil {
			return nil, fmt.Errorf("db release state: %w", err)
		}
		if err := a.repo.SetTaskAgent(context.Background(), a.task.ID, "", nil); err != nil {
			return nil, fmt.Errorf("db release agent: %w", err)
		}
		detail += " (was in progress; returned to backlog)"
	}

	action := "assigned"
	if msg.NewOwner == nil {
		action = "unassigned"
	}
	a.logActivityBy(msg.ByName, msg.By, action, "", "", detail)
	if drop && wasApproved {
		// Only a live approval is worth telling anyone about: one already
		// stale (the ticket was replanned since) is cleared silently.
		next := a.task.ApprovalOwner()
		a.logActivityBy(msg.ByName, msg.By, "approval_dropped", "", "", fmt.Sprintf(
			"Plan approval dropped: it was approved for %s, and the ticket is now %s's (owner changed by %s, not by a project lead in the AMP web UI). "+
				"%s approves it again in the AMP web UI, or a project lead on their behalf",
			ownerString(approvedFor), ownerString(next), nameOr(msg.ByName, ownerString(msg.By)), ownerString(next)))
	}
	t := a.withBlockedBy()
	a.publish(domain.EventTaskAssigned, t)
	a.log.Info(action, "owner", ownerString(msg.NewOwner))
	return &t, nil
}

// nameOr returns name, or fallback when name is empty.
func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

func sameOwner(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ownerString(id *int) string {
	if id == nil {
		return "unassigned"
	}
	return fmt.Sprintf("user %d", *id)
}

// checkUnblock is called after every MsgDepCompleted.
// O(degree) — only checks this task's own dep list.
// Only a task blocked by its dependencies (no block reason) unblocks this
// way; one blocked by hand or by its schedule stays blocked.
func (a *TaskActor) checkUnblock(ctx actor.Context) {
	if a.task.State != domain.TaskStateBlocked || a.task.BlockReason != "" {
		return
	}
	for _, depID := range a.task.DependencyIDs {
		if !a.completedDeps[depID] {
			return // still waiting
		}
	}
	// All deps complete — move to backlog.
	a.task.State = domain.TaskStateBacklog
	a.task.UpdatedAt = time.Now()

	if err := a.saveState(domain.TaskStateBacklog, ""); err != nil {
		a.log.Error("failed to persist unblock", "err", err)
		return
	}

	a.publish(domain.EventTaskUnblocked, a.withBlockedBy())
	a.logActivity("system", "unblocked", "blocked", "backlog",
		"All dependencies completed — task moved to backlog")
	a.log.Info("auto-unblocked")
}

// reconcileDeps re-reads which of the task's dependencies are complete and
// moves it between backlog and blocked to match: a ready task that now
// waits on something becomes blocked, and a task blocked only by its
// dependencies becomes ready once none are left. Tasks in progress,
// completed, or blocked for another reason keep their state.
func (a *TaskActor) reconcileDeps(why string) error {
	done, err := a.readDeps()
	if err != nil {
		return fmt.Errorf("read dependency states: %w", err)
	}
	waiting := false
	for _, d := range a.task.DependencyIDs {
		if !done[d] {
			waiting = true
			break
		}
	}
	a.task.UpdatedAt = time.Now()
	switch {
	case waiting && a.task.State == domain.TaskStateBacklog:
		a.task.State = domain.TaskStateBlocked
		a.task.BlockReason = ""
		if err := a.saveState(domain.TaskStateBlocked, ""); err != nil {
			return fmt.Errorf("db block: %w", err)
		}
		a.publish(domain.EventTaskBlocked, a.withBlockedBy())
		a.logActivity("system", "blocked", "backlog", "blocked", "Waiting on its dependencies ("+why+")")
	case !waiting && a.task.State == domain.TaskStateBlocked && a.task.BlockReason == "":
		a.task.State = domain.TaskStateBacklog
		if err := a.saveState(domain.TaskStateBacklog, ""); err != nil {
			return fmt.Errorf("db unblock: %w", err)
		}
		a.publish(domain.EventTaskUnblocked, a.withBlockedBy())
		a.logActivity("system", "unblocked", "blocked", "backlog", "No dependencies left to wait on ("+why+")")
	default:
		a.publish(domain.EventTaskUpdated, a.withBlockedBy())
	}
	return nil
}

// ---- Helpers ----

// openDeps re-reads which of the task's dependencies are complete (the
// in-memory set can be stale: a dependency moved back from completed sends
// no message) and returns the incomplete ones.
func (a *TaskActor) openDeps() ([]int, error) {
	if len(a.task.DependencyIDs) == 0 {
		return nil, nil
	}
	done, err := a.readDeps()
	if err != nil {
		return nil, err
	}
	var open []int
	for _, d := range a.task.DependencyIDs {
		if !done[d] {
			open = append(open, d)
		}
	}
	return open, nil
}

// refuseOpenDeps returns an error naming the incomplete dependencies, if any.
func (a *TaskActor) refuseOpenDeps(doing string) error {
	open, err := a.openDeps()
	if err != nil {
		return fmt.Errorf("task %d: couldn't check its dependencies (%v); nothing changed, try again", a.task.ID, err)
	}
	if len(open) > 0 {
		return fmt.Errorf("task %d can't be %s off the web UI: it's waiting on task IDs: %v, which aren't complete", a.task.ID, doing, open)
	}
	return nil
}

// holdForDeps applies MsgSetTaskState.RequireDeps: the state to store
// instead of want, or a refusal. Only a ticket whose dependencies are all
// complete is made ready (backlog), started or done by hand off the UI.
func (a *TaskActor) holdForDeps(want domain.TaskState) (domain.TaskState, error) {
	switch want {
	case domain.TaskStateBacklog, domain.TaskStateInProgress, domain.TaskStateCompleted:
	default:
		return want, nil
	}
	if want == domain.TaskStateCompleted && a.task.State == domain.TaskStateCompleted {
		return want, nil
	}
	if want == domain.TaskStateBacklog && (a.task.State == domain.TaskStateInProgress || a.task.State == domain.TaskStateCompleted) {
		// Moving work back stays allowed; with a dependency open it waits
		// on it (blocked, no reason) rather than showing as ready.
		open, err := a.openDeps()
		if err != nil {
			return want, fmt.Errorf("task %d: couldn't check its dependencies (%v); nothing changed, try again", a.task.ID, err)
		}
		if len(open) > 0 {
			return domain.TaskStateBlocked, nil
		}
		return want, nil
	}
	doing := map[domain.TaskState]string{domain.TaskStateBacklog: "made ready (backlog)",
		domain.TaskStateInProgress: "set in progress", domain.TaskStateCompleted: "completed"}[want]
	return want, a.refuseOpenDeps(doing)
}

// withBlockedBy returns a copy of the task with BlockedByIDs computed from
// completedDeps, and WaitingDetail naming the dependencies among them that
// are done but wait on their merge.
func (a *TaskActor) withBlockedBy() domain.Task {
	t := a.task
	t.BlockedByIDs = nil
	t.WaitingDetail = ""
	var waits []string
	for _, depID := range a.task.DependencyIDs {
		if !a.completedDeps[depID] {
			t.BlockedByIDs = append(t.BlockedByIDs, depID)
			if w := a.mergeWaits[depID]; w != "" {
				waits = append(waits, w)
			}
		}
	}
	t.WaitingDetail = strings.Join(waits, "; ")
	t.Phase = domain.PhaseOf(t.State, t.StartedAt)
	// How long it has waited for a worker, so no client has to work it out
	// from the timestamps (AMP #2801).
	t.NotStartedMinutes = 0
	if t.Phase == domain.PhaseNotStarted {
		from := t.UpdatedAt
		if t.DispatchedAt != nil {
			from = *t.DispatchedAt
		}
		if m := int(time.Since(from).Minutes()); m > 0 {
			t.NotStartedMinutes = m
		}
	}
	return t
}

// readDeps re-reads which dependencies are satisfied, and which of the
// rest are done but wait on their merge, into completedDeps/mergeWaits. A
// task whose effective metadata says waits_for_merge: false waits only for
// its dependencies to complete (AMP #2596).
func (a *TaskActor) readDeps() (map[int]bool, error) {
	done, waits, err := a.repo.DepStatesFor(context.Background(), a.task.ID, a.task.DependencyIDs)
	if err != nil {
		return nil, err
	}
	a.completedDeps, a.mergeWaits = done, waits
	return done, nil
}

// dependsOn reports whether id is one of the task's dependencies.
func (a *TaskActor) dependsOn(id int) bool {
	for _, d := range a.task.DependencyIDs {
		if d == id {
			return true
		}
	}
	return false
}

// handleSetMergeState applies MsgSetMergeState. A change that stops the
// ticket holding its dependents (merged, or not needed) releases them the
// way completion does; one that starts holding them (a PR reported after
// completion, an abandoned PR) makes them wait again.
func (a *TaskActor) handleSetMergeState(ctx actor.Context, msg *MsgSetMergeState) ReplyMergeState {
	cur := a.task.MergeState
	if cur == "" {
		cur = domain.MergeNone
	}
	if len(msg.From) > 0 {
		ok := false
		for _, f := range msg.From {
			ok = ok || f == cur
		}
		if !ok {
			t := a.withBlockedBy()
			return ReplyMergeState{Task: &t}
		}
	}
	required := a.task.MergeRequired
	if msg.Required != nil {
		required = *msg.Required
	}
	if cur == msg.State && required == a.task.MergeRequired {
		t := a.withBlockedBy()
		return ReplyMergeState{Task: &t}
	}
	at, source := a.task.MergedAt, a.task.MergeSource
	if msg.State == domain.MergeMerged {
		at, source = msg.At, msg.Source
		if at == nil {
			now := time.Now()
			at = &now
		}
	} else if msg.State != cur {
		at, source = nil, ""
	}
	if err := a.repo.SetTaskMerge(context.Background(), a.task.ID, &required, msg.State, at, source); err != nil {
		return ReplyMergeState{Err: fmt.Errorf("db merge state: %w", err)}
	}
	done := a.task.State == domain.TaskStateCompleted
	heldBefore := done && domain.MergeGates(cur)
	heldAfter := done && domain.MergeGates(msg.State)
	a.task.MergeRequired, a.task.MergeState, a.task.MergedAt, a.task.MergeSource = required, msg.State, at, source
	a.task.UpdatedAt = time.Now()

	detail := msg.Detail
	if detail == "" {
		detail = "Merge state " + cur + " → " + msg.State
	}
	a.logActivityBy(nameOr(msg.ByName, "system"), msg.By, "merge_"+msg.State, "", "", detail)
	t := a.withBlockedBy()
	a.publish(domain.EventTaskUpdated, t)

	switch {
	case heldBefore && !heldAfter:
		fan := make(chan ReplySimple, 1)
		ctx.Send(a.projectPID, &MsgFanDepCompleted{CompletedTaskID: a.task.ID, ReplyCh: fan})
		<-fan
	case heldAfter:
		// Newly held, or held for a new reason (an abandoned PR): dependents
		// wait (again) and say why.
		fan := make(chan ReplySimple, 1)
		ctx.Send(a.projectPID, &MsgFanDepCompleted{CompletedTaskID: a.task.ID,
			MergeWait: repository.MergeWaitDetail(a.task.ID, msg.State, msg.Where), ReplyCh: fan})
		<-fan
	}
	return ReplyMergeState{Task: &t, Changed: true}
}

func (a *TaskActor) publish(evtType domain.EventType, payload interface{}) {
	a.hub.Publish(domain.Event{
		Type:      evtType,
		ProjectID: a.projectID,
		Payload:   payload,
		At:        time.Now(),
	})
}

// handleApprove records a person's approval of the ticket's plan, checked
// against the ticket as it is now (see MsgApproveTask). The database writes
// approved_at with clock_timestamp(), the clock planned_at uses, and only
// while planned_at is still at or before AsOf.
func (a *TaskActor) handleApprove(msg *MsgApproveTask) ReplyApproveTask {
	t := a.task
	skip := func(why string) ReplyApproveTask {
		cur := a.withBlockedBy()
		return ReplyApproveTask{Task: &cur, Why: why}
	}
	switch {
	case !sameUser(t.OwnerUserID, msg.ExpectOwner):
		return skip("its owner changed")
	case t.State == domain.TaskStateCompleted:
		return skip("it is completed")
	case t.Approved():
		return skip("it is already approved")
	case msg.AsOf != nil && t.Planned().After(*msg.AsOf):
		return skip("it was planned after what was approved (as_of)")
	}
	at, forUser, err := a.repo.ApproveTask(context.Background(), t.ID, msg.By, msg.AsOf)
	if err != nil {
		return ReplyApproveTask{Err: fmt.Errorf("db approve task %d: %w", t.ID, err)}
	}
	if at == nil {
		return skip("it was planned after what was approved (as_of)")
	}
	a.task.ApprovedAt, a.task.ApprovedBy, a.task.ApprovedFor = at, msg.By, forUser
	detail := "Plan approved by " + msg.ByName
	if msg.OnBehalfOf != "" {
		detail = "Plan approved on behalf of " + msg.OnBehalfOf + " by " + msg.ByName
	}
	a.logActivityBy(msg.ByName, msg.By, "plan_approved", "", "", detail)
	cur := a.withBlockedBy()
	a.publish(domain.EventTaskUpdated, cur)
	return ReplyApproveTask{Task: &cur, Approved: true}
}

// sameUser reports whether two optional user ids are the same (both unset
// counts as the same).
func sameUser(x, y *int) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return *x == *y
}

func (a *TaskActor) logActivity(actorName, action, fromState, toState, detail string) {
	a.logActivityBy(actorName, nil, action, fromState, toState, detail)
}

// logActivityBy appends to the activity log, recording the AMP user behind
// the action when there is one.
func (a *TaskActor) logActivityBy(actorName string, by *int, action, fromState, toState, detail string) {
	if err := a.repo.LogActivity(context.Background(), domain.ActivityLog{
		TaskID:      a.task.ID,
		ProjectID:   a.projectID,
		Actor:       actorName,
		Action:      action,
		FromState:   fromState,
		ToState:     toState,
		Detail:      detail,
		ActorUserID: by,
	}); err != nil {
		a.log.Error("failed to log activity", "action", action, "err", err)
	}
}

// armScheduleTimer sets up a timer to fire at the task's start_at time.
// The timer callback sends a MsgScheduledUnblock message to self via the actor system.
func (a *TaskActor) armScheduleTimer() {
	if a.task.StartAt == nil {
		return
	}

	delay := time.Until(*a.task.StartAt)
	if delay <= 0 {
		return // already past the time
	}

	a.scheduleTimer = time.AfterFunc(delay, func() {
		// Timer fires in a goroutine — must send message to actor via actor system, not call methods directly
		if a.selfPID != nil && a.actorSystem != nil {
			a.actorSystem.Root.Send(a.selfPID, &MsgScheduledUnblock{TaskID: a.task.ID})
		}
	})

	a.log.Info("scheduled timer armed", "start_at", a.task.StartAt, "delay_seconds", delay.Seconds())
}

// waitingToStart reports whether the task has not started yet and nothing but
// its schedule or its dependencies holds it: backlog, or blocked by its
// schedule or on its dependencies (empty block reason). A task blocked for any
// other reason (a question, a hand block) keeps that reason.
func (a *TaskActor) waitingToStart() bool {
	switch a.task.State {
	case domain.TaskStateBacklog:
		return true
	case domain.TaskStateBlocked:
		return a.task.BlockReason == "" || strings.HasPrefix(a.task.BlockReason, "scheduled:")
	}
	return false
}

// handleScheduledUnblock is called when the scheduled timer fires, and on
// actor start when the start time passed while no actor was running the task.
// It consumes the schedule (clears start_at) and releases the task from it:
// to backlog when every dependency is complete, otherwise it stays blocked on
// its dependencies (empty block reason) so checkUnblock releases it when the
// last one completes.
func (a *TaskActor) handleScheduledUnblock(ctx actor.Context) {
	// Guard against spurious fires — verify start_at is set and time has passed
	if a.task.StartAt == nil || a.task.StartAt.After(time.Now()) {
		a.log.Warn("spurious scheduled unblock", "start_at", a.task.StartAt)
		return
	}

	// Only release a task its schedule is still holding: one blocked by hand
	// (or already moved on) keeps its state.
	if a.task.State != domain.TaskStateBlocked || !strings.HasPrefix(a.task.BlockReason, "scheduled:") {
		a.log.Warn("scheduled unblock but task not blocked by its schedule", "state", a.task.State, "reason", a.task.BlockReason)
		return
	}

	// The schedule is consumed. Clear start_at in memory before the release so
	// the task.blocked/task.unblocked event it publishes carries start_at nil
	// (the UI applies event payloads directly). The DB keeps its order: state
	// first, then start_at — clearing start_at first would strand the task if
	// the state save failed, since Started only re-sends MsgScheduledUnblock
	// while start_at is set.
	prevStartAt := a.task.StartAt
	a.task.StartAt = nil
	if err := a.releaseFromSchedule("Scheduled start time reached"); err != nil {
		a.task.StartAt = prevStartAt
		a.log.Error("failed to persist scheduled unblock state", "err", err)
		return
	}

	if err := a.repo.SetTaskStartAt(context.Background(), a.task.ID, nil); err != nil {
		a.log.Error("failed to clear start_at", "err", err)
		return
	}

	// Clear the timer reference
	a.scheduleTimer = nil

	a.log.Info("scheduled unblock complete", "state", a.task.State)
}

// releaseFromSchedule lifts the schedule's hold on a task blocked by it. The
// dependency states are re-read from the database (the in-memory set may be
// stale after a restart or replica move). If every dependency is complete the
// task moves to backlog; otherwise it stays blocked with an empty block
// reason — "waiting on its dependencies" — which checkUnblock/reconcileDeps
// release once the last dependency completes. Publishes and logs the change.
func (a *TaskActor) releaseFromSchedule(why string) error {
	done, err := a.readDeps()
	if err != nil {
		return fmt.Errorf("read dependency states: %w", err)
	}
	waiting := 0
	for _, d := range a.task.DependencyIDs {
		if !done[d] {
			waiting++
		}
	}

	a.task.BlockReason = ""
	a.task.UpdatedAt = time.Now()
	if waiting > 0 {
		a.task.State = domain.TaskStateBlocked
		if err := a.saveState(domain.TaskStateBlocked, ""); err != nil {
			return err
		}
		a.publish(domain.EventTaskBlocked, a.withBlockedBy())
		a.logActivity("system", "blocked", "blocked", "blocked",
			fmt.Sprintf("%s — still waiting on its dependencies (%d unfinished)", why, waiting))
		return nil
	}

	a.task.State = domain.TaskStateBacklog
	if err := a.saveState(domain.TaskStateBacklog, ""); err != nil {
		return err
	}
	a.publish(domain.EventTaskUnblocked, a.withBlockedBy())
	a.logActivity("system", "unblocked", "blocked", "backlog", why)
	return nil
}
