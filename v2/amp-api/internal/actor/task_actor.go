package actor

import (
	"context"
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
	detached      bool         // stopped for a move: don't deregister from routing
	storyPID      *actor.PID   // parent StoryActor — receives upward notifications
	projectPID    *actor.PID   // ProjectActor — receives MsgRegisterTask on start
	projectID     int
	repo          *repository.Repo
	hub           *hub.Hub
	log           *slog.Logger
	scheduleTimer *time.Timer        // non-nil if task is waiting for start_at
	selfPID       *actor.PID         // this actor's PID — used to send messages to self
	actorSystem   *actor.ActorSystem // actor system reference for sending messages from goroutines
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

		// Check if this task has a future start_at time and arm the timer
		if a.task.StartAt != nil && a.task.StartAt.After(time.Now()) {
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
		err := a.handleComplete(ctx, msg.By)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgBlockTask:
		err := a.handleBlock(msg.Reason, msg.By)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgAssignTask:
		t, err := a.handleAssign(msg, ctx)
		msg.ReplyCh <- ReplyGetTask{Task: t, Err: err}

	case *MsgUpdateTask:
		err := a.handleUpdate(msg.Req)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgSetTaskState:
		// Cancel timer if armed before applying state change
		if a.scheduleTimer != nil {
			a.scheduleTimer.Stop()
			a.scheduleTimer = nil
		}
		err := a.handleSetState(msg.State, msg.Reason, msg.By)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgSetTaskStartAt:
		err := a.handleSetTaskStartAt(msg.StartAt)
		msg.ReplyCh <- ReplySimple{Err: err}

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
		// A dependency finished — check if we are now unblocked.
		a.completedDeps[msg.CompletedTaskID] = true
		a.checkUnblock(ctx)

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
	if a.task.State == domain.TaskStateBlocked {
		var incomplete []int
		for _, depID := range a.task.DependencyIDs {
			if !a.completedDeps[depID] {
				incomplete = append(incomplete, depID)
			}
		}
		return fmt.Errorf("task %d is blocked — waiting on task IDs: %v", a.task.ID, incomplete)
	}
	if a.task.State == domain.TaskStateInProgress {
		return fmt.Errorf("task %d is already in_progress (agent: %s)", a.task.ID, a.task.AgentID)
	}
	if a.task.State == domain.TaskStateCompleted {
		return fmt.Errorf("task %d is already completed", a.task.ID)
	}

	now := time.Now()
	if claim {
		if err := a.repo.SetTaskOwner(context.Background(), a.task.ID, msg.By); err != nil {
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
	if err := a.repo.SetTaskAgent(context.Background(), a.task.ID, agentID, &now); err != nil {
		return fmt.Errorf("db set agent: %w", err)
	}

	a.publish(domain.EventTaskDispatched, a.withBlockedBy())
	if msg.Human {
		a.logActivityBy(agentID, msg.By, "started", "backlog", "in_progress", "Started by its owner (human work)")
	} else {
		a.logActivityBy(agentID, msg.By, "dispatched", "backlog", "in_progress",
			fmt.Sprintf("Dispatched to agent %q", agentID))
	}
	a.log.Info("dispatched", "agent", agentID)

	// Notify parent StoryActor so it can roll up to in_progress.
	ctx.Send(a.storyPID, &MsgTaskDispatched{TaskID: a.task.ID})
	return nil
}

func (a *TaskActor) handleComplete(ctx actor.Context, by *int) error {
	if a.task.State == domain.TaskStateCompleted {
		return nil // idempotent
	}

	prevState := a.task.State // capture before mutation for accurate activity log
	now := time.Now()
	a.task.State = domain.TaskStateCompleted
	a.task.CompletedAt = &now
	a.task.UpdatedAt = now

	if err := a.saveState(domain.TaskStateCompleted, ""); err != nil {
		return fmt.Errorf("db complete: %w", err)
	}

	a.publish(domain.EventTaskCompleted, a.withBlockedBy())
	if a.task.IsHuman() {
		a.logActivityBy(domain.HumanAgentID, by, "completed", string(prevState), "completed", "Done by its owner (human work)")
	} else {
		a.logActivityBy(a.task.AgentID, by, "completed", string(prevState), "completed",
			fmt.Sprintf("Task completed by agent %q", a.task.AgentID))
	}
	a.log.Info("completed")

	// Synchronously ask ProjectActor to fan MsgDepCompleted to ALL epics/stories/tasks.
	// We WAIT for this to complete before returning, so that any caller who reads
	// task state after MsgCompleteTask returns will see the unblocked state immediately.
	fanReplyCh := make(chan ReplySimple, 1)
	ctx.Send(a.projectPID, &MsgFanDepCompleted{CompletedTaskID: a.task.ID, ReplyCh: fanReplyCh})
	<-fanReplyCh

	// Then notify parent StoryActor for state rollup (async is fine for rollup).
	ctx.Send(a.storyPID, &MsgTaskCompleted{TaskID: a.task.ID})
	return nil
}

// ownerPhrase names who does a human ticket, for refusals.
func (a *TaskActor) ownerPhrase() string {
	if a.task.OwnerUserID == nil {
		return "it has no owner yet; assign the person who does it"
	}
	return fmt.Sprintf("its owner (user %d) does it", *a.task.OwnerUserID)
}

func (a *TaskActor) handleBlock(reason string, by *int) error {
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

func (a *TaskActor) handleUpdate(req domain.UpdateTaskRequest) error {
	clr, err := domain.ParseClears(req.Clear, domain.TaskClearable)
	if err != nil {
		return err
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

	if err := a.repo.UpdateTask(context.Background(), req); err != nil {
		return fmt.Errorf("db update: %w", err)
	}
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

func (a *TaskActor) handleSetState(state domain.TaskState, reason string, by *int) error {
	from := string(a.task.State)
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
		Actor:     "manager",
		Action:    "state_change",
		FromState: from,
		ToState:   string(state),
		Detail:    reason,
		// Who overrode the state, when known.
		ActorUserID: by,
	}); err != nil {
		a.log.Error("failed to log state_change activity", "err", err)
	}

	a.publish(domain.EventTaskUpdated, a.withBlockedBy())
	return nil
}

func (a *TaskActor) handleSetTaskStartAt(startAt *time.Time) error {
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
	if err := a.repo.SetTaskOwner(context.Background(), a.task.ID, msg.NewOwner); err != nil {
		return nil, fmt.Errorf("db set owner: %w", err)
	}
	from := ownerString(a.task.OwnerUserID)
	a.task.OwnerUserID = msg.NewOwner
	a.task.UpdatedAt = time.Now()

	detail := fmt.Sprintf("Owner %s → %s", from, ownerString(msg.NewOwner))
	if msg.NewOwner == nil && a.task.State == domain.TaskStateInProgress {
		// Released mid-flight: nobody is working it any more.
		a.task.State = domain.TaskStateBacklog
		a.task.AgentID = ""
		a.task.DispatchedAt = nil
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
	t := a.withBlockedBy()
	a.publish(domain.EventTaskAssigned, t)
	a.log.Info(action, "owner", ownerString(msg.NewOwner))
	return &t, nil
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
	done, err := a.repo.CompletedAmong(context.Background(), a.task.DependencyIDs)
	if err != nil {
		return fmt.Errorf("read dependency states: %w", err)
	}
	a.completedDeps = done
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

// withBlockedBy returns a copy of the task with BlockedByIDs computed from completedDeps.
func (a *TaskActor) withBlockedBy() domain.Task {
	t := a.task
	t.BlockedByIDs = nil
	for _, depID := range a.task.DependencyIDs {
		if !a.completedDeps[depID] {
			t.BlockedByIDs = append(t.BlockedByIDs, depID)
		}
	}
	return t
}

func (a *TaskActor) publish(evtType domain.EventType, payload interface{}) {
	a.hub.Publish(domain.Event{
		Type:      evtType,
		ProjectID: a.projectID,
		Payload:   payload,
		At:        time.Now(),
	})
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
	done, err := a.repo.CompletedAmong(context.Background(), a.task.DependencyIDs)
	if err != nil {
		return fmt.Errorf("read dependency states: %w", err)
	}
	a.completedDeps = done
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
