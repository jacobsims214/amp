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

			if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateBlocked, a.task.BlockReason); err != nil {
				a.log.Error("failed to persist scheduled block state", "err", err)
			}
		}

		a.log.Info("started", "state", a.task.State, "deps", len(a.task.DependencyIDs))

	case *actor.Stopped:
		// Deregister from ProjectActor routing index.
		ctx.Send(a.projectPID, &MsgDeregisterTask{TaskID: a.task.ID})

	case *MsgGetTask:
		t := a.withBlockedBy()
		msg.ReplyCh <- ReplyGetTask{Task: &t}

	case *MsgDispatchTask:
		err := a.handleDispatch(msg.AgentID, ctx)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgCompleteTask:
		err := a.handleComplete(ctx)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgBlockTask:
		err := a.handleBlock(msg.Reason)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgUpdateTask:
		err := a.handleUpdate(msg.Req)
		msg.ReplyCh <- ReplySimple{Err: err}

	case *MsgSetTaskState:
		// Cancel timer if armed before applying state change
		if a.scheduleTimer != nil {
			a.scheduleTimer.Stop()
			a.scheduleTimer = nil
		}
		err := a.handleSetState(msg.State, msg.Reason)
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
		// Parent is handling DB deletion; we just stop.
		ctx.Stop(ctx.Self())
		msg.ReplyCh <- ReplySimple{}
	}
}

// ---- Handlers ----

func (a *TaskActor) handleDispatch(agentID string, ctx actor.Context) error {
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
	a.task.State = domain.TaskStateInProgress
	a.task.AgentID = agentID
	a.task.DispatchedAt = &now
	a.task.UpdatedAt = now

	if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateInProgress, ""); err != nil {
		return fmt.Errorf("db dispatch: %w", err)
	}
	if err := a.repo.SetTaskAgent(context.Background(), a.task.ID, agentID, &now); err != nil {
		return fmt.Errorf("db set agent: %w", err)
	}

	a.publish(domain.EventTaskDispatched, a.withBlockedBy())
	a.logActivity(agentID, "dispatched", "backlog", "in_progress",
		fmt.Sprintf("Dispatched to agent %q", agentID))
	a.log.Info("dispatched", "agent", agentID)

	// Notify parent StoryActor so it can roll up to in_progress.
	ctx.Send(a.storyPID, &MsgTaskDispatched{TaskID: a.task.ID})
	return nil
}

func (a *TaskActor) handleComplete(ctx actor.Context) error {
	if a.task.State == domain.TaskStateCompleted {
		return nil // idempotent
	}

	prevState := a.task.State // capture before mutation for accurate activity log
	now := time.Now()
	a.task.State = domain.TaskStateCompleted
	a.task.CompletedAt = &now
	a.task.UpdatedAt = now

	if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateCompleted, ""); err != nil {
		return fmt.Errorf("db complete: %w", err)
	}

	a.publish(domain.EventTaskCompleted, a.withBlockedBy())
	a.logActivity(a.task.AgentID, "completed", string(prevState), "completed",
		fmt.Sprintf("Task completed by agent %q", a.task.AgentID))
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

func (a *TaskActor) handleBlock(reason string) error {
	a.task.State = domain.TaskStateBlocked
	a.task.BlockReason = reason
	a.task.UpdatedAt = time.Now()

	if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateBlocked, reason); err != nil {
		return fmt.Errorf("db block: %w", err)
	}

	a.publish(domain.EventTaskBlocked, a.withBlockedBy())
	a.logActivity("system", "blocked", "", "blocked", reason)
	a.log.Info("blocked", "reason", reason)
	return nil
}

func (a *TaskActor) handleUpdate(req domain.UpdateTaskRequest) error {
	if req.Name != "" {
		a.task.Name = req.Name
	}
	if req.Description != "" {
		a.task.Description = req.Description
	}
	if req.AssignedTo != "" {
		a.task.AssignedTo = req.AssignedTo
	}
	if req.AgentID != "" {
		a.task.AgentID = req.AgentID
	}
	a.task.UpdatedAt = time.Now()

	if err := a.repo.UpdateTask(context.Background(), req); err != nil {
		return fmt.Errorf("db update: %w", err)
	}

	a.publish(domain.EventTaskUpdated, a.withBlockedBy())
	return nil
}

func (a *TaskActor) handleSetState(state domain.TaskState, reason string) error {
	from := string(a.task.State)
	a.task.State = state
	a.task.UpdatedAt = time.Now()

	if err := a.repo.SetTaskState(context.Background(), a.task.ID, state, reason); err != nil {
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

		if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateBlocked, a.task.BlockReason); err != nil {
			return fmt.Errorf("db set scheduled block state: %w", err)
		}

		a.log.Info("scheduled task", "start_at", startAt)
	} else if startAt == nil {
		// Clearing the schedule — if task was blocked by schedule, unblock it
		if a.task.State == domain.TaskStateBlocked && strings.HasPrefix(a.task.BlockReason, "scheduled:") {
			a.task.State = domain.TaskStateBacklog
			a.task.BlockReason = ""

			if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateBacklog, ""); err != nil {
				return fmt.Errorf("db unblock task: %w", err)
			}

			a.log.Info("cleared schedule, unblocked task")
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
	a.logActivity(req.Author, "comment", "", "", req.Body)
	return comment, nil
}

// checkUnblock is called after every MsgDepCompleted.
// O(degree) — only checks this task's own dep list.
func (a *TaskActor) checkUnblock(ctx actor.Context) {
	if a.task.State != domain.TaskStateBlocked {
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

	if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateBacklog, ""); err != nil {
		a.log.Error("failed to persist unblock", "err", err)
		return
	}

	a.publish(domain.EventTaskUnblocked, a.withBlockedBy())
	a.logActivity("system", "unblocked", "blocked", "backlog",
		"All dependencies completed — task moved to backlog")
	a.log.Info("auto-unblocked")
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
	if err := a.repo.LogActivity(context.Background(), domain.ActivityLog{
		TaskID:    a.task.ID,
		ProjectID: a.projectID,
		Actor:     actorName,
		Action:    action,
		FromState: fromState,
		ToState:   toState,
		Detail:    detail,
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

// handleScheduledUnblock is called when the scheduled timer fires.
// It transitions the task from blocked to backlog and clears the start_at.
func (a *TaskActor) handleScheduledUnblock(ctx actor.Context) {
	// Guard against spurious fires — verify start_at is set and time has passed
	if a.task.StartAt == nil || a.task.StartAt.After(time.Now()) {
		a.log.Warn("spurious scheduled unblock", "start_at", a.task.StartAt)
		return
	}

	// Only unblock if currently in blocked state
	if a.task.State != domain.TaskStateBlocked {
		a.log.Warn("scheduled unblock but task not blocked", "state", a.task.State)
		return
	}

	// Transition to backlog
	a.task.State = domain.TaskStateBacklog
	a.task.BlockReason = ""
	a.task.UpdatedAt = time.Now()

	// Persist state change
	if err := a.repo.SetTaskState(context.Background(), a.task.ID, domain.TaskStateBacklog, ""); err != nil {
		a.log.Error("failed to persist scheduled unblock state", "err", err)
		return
	}

	// Clear start_at in DB — once the schedule fires, it's consumed
	if err := a.repo.SetTaskStartAt(context.Background(), a.task.ID, nil); err != nil {
		a.log.Error("failed to clear start_at", "err", err)
		return
	}

	// Clear the timer reference
	a.scheduleTimer = nil

	// Publish SSE event
	a.publish(domain.EventTaskUnblocked, a.withBlockedBy())

	// Log activity
	a.logActivity("system", "unblocked", "blocked", "backlog",
		"Scheduled start time reached")

	a.log.Info("scheduled unblock complete")
}
