package actor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
)

// StoryActor owns one story's state machine and supervises its TaskActors.
//
// Responsibilities:
//   - Spawns and supervises one TaskActor per task
//   - Routes task-targeted messages to the right TaskActor
//   - Tracks which tasks are complete to drive story state rollup
//   - Fans MsgDepCompleted to all child TaskActors (for cross-task unblocking)
//   - Notifies parent EpicActor when story transitions state
type StoryActor struct {
	story      domain.Story
	tasks      map[int]*actor.PID // task_id → TaskActor PID
	taskStates map[int]domain.TaskState
	epicPID    *actor.PID // parent EpicActor
	projectPID *actor.PID // ProjectActor (for task registration)
	repo       *repository.Repo
	hub        *hub.Hub
	log        *slog.Logger
}

func NewStoryActor(
	story domain.Story,
	epicPID *actor.PID,
	projectPID *actor.PID,
	repo *repository.Repo,
	h *hub.Hub,
) actor.Actor {
	return &StoryActor{
		story:      story,
		tasks:      make(map[int]*actor.PID),
		taskStates: make(map[int]domain.TaskState),
		epicPID:    epicPID,
		projectPID: projectPID,
		repo:       repo,
		hub:        h,
		log:        slog.Default().With("actor", "story", "story_id", story.ID, "epic_id", story.EpicID),
	}
}

func (a *StoryActor) Receive(ctx actor.Context) {
	switch msg := ctx.Message().(type) {

	case *actor.Started:
		a.loadAndSpawnTasks(ctx)

	case *MsgCreateTask:
		task, err := a.handleCreateTask(ctx, msg.Req)
		msg.ReplyCh <- ReplyCreateTask{Task: task, Err: err}

	case *MsgGetTask:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplyGetTask{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		// Forward the message to the TaskActor and relay its reply.
		replyCh := make(chan ReplyGetTask, 1)
		ctx.Send(pid, &MsgGetTask{TaskID: msg.TaskID, ReplyCh: replyCh})
		reply := <-replyCh
		msg.ReplyCh <- reply

	case *MsgListStoryTasks:
		tasks := a.collectAllTasks(ctx)
		msg.ReplyCh <- ReplyListTasks{Tasks: tasks}

	case *MsgDispatchTask:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplySimple, 1)
		fwd := *msg
		fwd.ReplyCh = replyCh
		ctx.Send(pid, &fwd)
		msg.ReplyCh <- <-replyCh

	case *MsgAssignTask:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplyGetTask{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		ctx.Send(pid, msg)

	case *MsgCompleteTask:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplySimple, 1)
		fwd := *msg
		fwd.ReplyCh = replyCh
		ctx.Send(pid, &fwd)
		msg.ReplyCh <- <-replyCh

	case *MsgBlockTask:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplySimple, 1)
		fwd := *msg
		fwd.ReplyCh = replyCh
		ctx.Send(pid, &fwd)
		msg.ReplyCh <- <-replyCh

	case *MsgSetTaskDeps:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplyGetTask{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplyGetTask, 1)
		fwd := *msg
		fwd.ReplyCh = replyCh
		ctx.Send(pid, &fwd)
		msg.ReplyCh <- <-replyCh

	case *MsgReconcileDeps:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{}
			return
		}
		replyCh := make(chan ReplySimple, 1)
		ctx.Send(pid, &MsgReconcileDeps{TaskID: msg.TaskID, ReplyCh: replyCh})
		msg.ReplyCh <- <-replyCh

	case *MsgAdoptTask:
		t, err := a.adoptTask(ctx, msg.TaskID)
		msg.ReplyCh <- ReplyGetTask{Task: t, Err: err}

	case *MsgUpdateTask:
		pid, ok := a.tasks[msg.Req.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("task %d not found in story %d", msg.Req.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplySimple, 1)
		ctx.Send(pid, &MsgUpdateTask{Req: msg.Req, KeepHuman: msg.KeepHuman, ReplyCh: replyCh})
		msg.ReplyCh <- <-replyCh

	case *MsgSetTaskState:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplySimple, 1)
		fwd := *msg
		fwd.ReplyCh = replyCh
		ctx.Send(pid, &fwd)
		msg.ReplyCh <- <-replyCh

	case *MsgSetTaskStartAt:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		ctx.Send(pid, msg)

	case *MsgApproveTask:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplyApproveTask{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		ctx.Send(pid, msg)

	case *MsgSetMergeState:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplyMergeState{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		ctx.Send(pid, msg)

	case *MsgAddComment:
		pid, ok := a.tasks[msg.Req.TaskID]
		if !ok {
			msg.ReplyCh <- ReplyAddComment{Err: fmt.Errorf("task %d not found in story %d", msg.Req.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplyAddComment, 1)
		ctx.Send(pid, &MsgAddComment{Req: msg.Req, ReplyCh: replyCh})
		msg.ReplyCh <- <-replyCh

	case *MsgGetComments:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplyGetComments{Err: fmt.Errorf("task %d not found in story %d", msg.TaskID, a.story.ID)}
			return
		}
		replyCh := make(chan ReplyGetComments, 1)
		ctx.Send(pid, &MsgGetComments{TaskID: msg.TaskID, ReplyCh: replyCh})
		msg.ReplyCh <- <-replyCh

	case *MsgDeleteTask:
		pid, ok := a.tasks[msg.TaskID]
		if !ok {
			msg.ReplyCh <- ReplySimple{}
			return
		}
		// Ask TaskActor to stop itself (it will deregister from ProjectActor).
		replyCh := make(chan ReplySimple, 1)
		ctx.Send(pid, &MsgDeleteTask{TaskID: msg.TaskID, Detach: msg.Detach, ReplyCh: replyCh})
		<-replyCh
		delete(a.tasks, msg.TaskID)
		delete(a.taskStates, msg.TaskID)
		a.rollupState(ctx)
		msg.ReplyCh <- ReplySimple{}

	case *MsgDeleteStory:
		// Stop all child TaskActors and deregister from ProjectActor.
		for taskID, pid := range a.tasks {
			ctx.Stop(pid)
			// Deregister from ProjectActor so its taskToEpic index stays clean.
			ctx.Send(a.projectPID, &MsgDeregisterTask{TaskID: taskID, EpicID: a.story.EpicID})
			delete(a.tasks, taskID)
		}
		a.taskStates = make(map[int]domain.TaskState)
		msg.ReplyCh <- ReplySimple{}

	case *MsgUpdateStory:
		clr, err := domain.ParseClears(msg.Req.Clear, domain.StoryClearable)
		if err != nil {
			msg.ReplyCh <- ReplySimple{Err: err}
			return
		}
		if err := a.repo.UpdateStory(context.Background(), msg.Req); err != nil {
			msg.ReplyCh <- ReplySimple{Err: fmt.Errorf("update story: %w", err)}
			return
		}
		if msg.Req.Metadata != nil {
			meta, err := a.repo.MergeMetadata(context.Background(), "story", msg.Req.StoryID, msg.Req.Metadata)
			if err != nil {
				msg.ReplyCh <- ReplySimple{Err: err}
				return
			}
			a.story.Metadata = meta
		}
		if msg.Req.Name != "" {
			a.story.Name = msg.Req.Name
		}
		a.story.Description = clr.Apply("description", msg.Req.Description, a.story.Description)
		a.story.AcceptanceCriteria = clr.Apply("acceptance_criteria", msg.Req.AcceptanceCriteria, a.story.AcceptanceCriteria)
		if msg.Req.Priority != "" {
			a.story.Priority = msg.Req.Priority
		}
		msg.ReplyCh <- ReplySimple{}

	case *MsgDepCompleted:
		// Fan out to all child TaskActors — each checks its own deps independently.
		for _, pid := range a.tasks {
			ctx.Send(pid, msg)
		}

	case *MsgTaskDispatched:
		// A task moved to in_progress — roll up story state if needed.
		a.taskStates[msg.TaskID] = domain.TaskStateInProgress
		a.rollupState(ctx)

	case *MsgTaskCompleted:
		// A task completed — update our tracking, check for full story completion.
		a.taskStates[msg.TaskID] = domain.TaskStateCompleted
		a.rollupState(ctx)

	case *MsgReset:
		// Stop all child TaskActors.
		for taskID, pid := range a.tasks {
			ctx.Stop(pid)
			delete(a.tasks, taskID)
		}
		a.taskStates = make(map[int]domain.TaskState)
		msg.ReplyCh <- ReplySimple{}
	}
}

// ---- Helpers ----

func (a *StoryActor) loadAndSpawnTasks(ctx actor.Context) {
	tasks, err := a.repo.ListTasksByStory(context.Background(), a.story.ID)
	if err != nil {
		a.log.Error("failed to load tasks", "err", err)
		return
	}

	// Which of these tasks' dependencies are already complete — anywhere in
	// the project, not just in this story.
	var allDeps []int
	for _, t := range tasks {
		allDeps = append(allDeps, t.DependencyIDs...)
	}
	completedSet, waitSet, err := a.repo.DepStates(context.Background(), allDeps)
	if err != nil {
		a.log.Error("failed to load dependency states", "err", err)
		completedSet, waitSet = map[int]bool{}, map[int]string{}
	}

	for _, t := range tasks {
		// Build this task's completedDeps from the completed set.
		completedDeps := make(map[int]bool)
		mergeWaits := make(map[int]string)
		for _, depID := range t.DependencyIDs {
			if completedSet[depID] {
				completedDeps[depID] = true
			} else if w, ok := waitSet[depID]; ok {
				mergeWaits[depID] = w
			}
		}
		pid := a.spawnTaskActor(ctx, t, completedDeps, mergeWaits)
		a.tasks[t.ID] = pid
		a.taskStates[t.ID] = t.State
	}
	a.log.Info("loaded tasks", "count", len(tasks))
}

// adoptTask starts managing a task just moved into this story: load it,
// work out which of its dependencies are done, and spawn its actor.
func (a *StoryActor) adoptTask(ctx actor.Context, taskID int) (*domain.Task, error) {
	t, err := a.repo.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("load moved task %d: %w", taskID, err)
	}
	if t.DependencyIDs, err = a.repo.TaskDependencies(context.Background(), taskID); err != nil {
		return nil, err
	}
	done, waits, err := a.repo.DepStates(context.Background(), t.DependencyIDs)
	if err != nil {
		return nil, err
	}
	a.tasks[t.ID] = a.spawnTaskActor(ctx, *t, done, waits)
	a.taskStates[t.ID] = t.State
	for _, d := range t.DependencyIDs {
		if !done[d] {
			t.BlockedByIDs = append(t.BlockedByIDs, d)
		}
	}
	a.rollupState(ctx)
	return t, nil
}

func (a *StoryActor) spawnTaskActor(ctx actor.Context, t domain.Task, completedDeps map[int]bool, mergeWaits map[int]string) *actor.PID {
	props := actor.PropsFromProducer(func() actor.Actor {
		ta := NewTaskActorFromTask(t, completedDeps, ctx.Self(), a.projectPID, a.repo, a.hub).(*TaskActor)
		ta.mergeWaits = mergeWaits
		return ta
	})
	name := fmt.Sprintf("story-%d-task-%d", a.story.ID, t.ID)
	pid, err := ctx.SpawnNamed(props, name)
	if err != nil {
		// Already exists (restart scenario) — look up by local address.
		pid = ctx.ActorSystem().NewLocalPID(name)
		a.log.Warn("task actor already exists, reusing pid", "task_id", t.ID)
	}
	return pid
}

func (a *StoryActor) handleCreateTask(ctx actor.Context, req domain.CreateTaskRequest) (*domain.Task, error) {
	// Persist first.
	task, err := a.repo.CreateTask(context.Background(), req)
	if err != nil {
		return nil, fmt.Errorf("db create task: %w", err)
	}

	// Derive initial state from deps — which are already completed, wherever
	// they are in the project.
	completedDeps, mergeWaits, err := a.repo.DepStates(context.Background(), req.DependencyIDs)
	if err != nil {
		return nil, fmt.Errorf("db dependency states: %w", err)
	}
	hasIncomplete := false
	for _, depID := range req.DependencyIDs {
		if !completedDeps[depID] {
			hasIncomplete = true
			break
		}
	}
	if hasIncomplete {
		task.State = domain.TaskStateBlocked
		if err := a.repo.SetTaskState(context.Background(), task.ID, domain.TaskStateBlocked, ""); err != nil {
			return nil, fmt.Errorf("db block new task: %w", err)
		}
	}

	// Spawn TaskActor.
	pid := a.spawnTaskActor(ctx, *task, completedDeps, mergeWaits)
	a.tasks[task.ID] = pid
	a.taskStates[task.ID] = task.State

	// Build BlockedByIDs for the response.
	t := *task
	if hasIncomplete {
		for _, depID := range req.DependencyIDs {
			if !completedDeps[depID] {
				t.BlockedByIDs = append(t.BlockedByIDs, depID)
			}
		}
	}

	// Publish SSE + log.
	a.hub.Publish(domain.Event{
		Type:      domain.EventTaskCreated,
		ProjectID: a.story.ProjectID,
		Payload:   t,
		At:        time.Now(),
	})
	_ = a.repo.LogActivity(context.Background(), domain.ActivityLog{
		TaskID:      task.ID,
		ProjectID:   a.story.ProjectID,
		Actor:       "system",
		Action:      "created",
		ToState:     string(task.State),
		ActorUserID: req.CreatedBy,
		Detail:      fmt.Sprintf("Created in story %q (#%d) as %s.", a.story.Name, a.story.ID, task.State),
	})
	a.log.Info("task created", "task_id", task.ID, "state", task.State)
	return &t, nil
}

func (a *StoryActor) collectAllTasks(ctx actor.Context) []domain.Task {
	var out []domain.Task
	for taskID, pid := range a.tasks {
		replyCh := make(chan ReplyGetTask, 1)
		ctx.Send(pid, &MsgGetTask{TaskID: taskID, ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err == nil && reply.Task != nil {
			out = append(out, *reply.Task)
		}
	}
	return out
}

// rollupState checks task states and drives the story state machine.
func (a *StoryActor) rollupState(ctx actor.Context) {
	if len(a.taskStates) == 0 {
		return
	}

	allComplete := true
	anyInProgress := false
	for _, s := range a.taskStates {
		if s != domain.TaskStateCompleted {
			allComplete = false
		}
		if s == domain.TaskStateInProgress {
			anyInProgress = true
		}
	}

	var newState domain.StoryState
	if allComplete {
		newState = domain.StoryStateCompleted
	} else if anyInProgress || a.story.State == domain.StoryStateInProgress {
		newState = domain.StoryStateInProgress
	} else {
		return // no change
	}

	if newState == a.story.State {
		return
	}

	a.story.State = newState
	if err := a.repo.SetStoryState(context.Background(), a.story.ID, newState); err != nil {
		a.log.Error("failed to persist story state", "state", newState, "err", err)
		return
	}

	a.hub.Publish(domain.Event{
		Type:      domain.EventStoryStateChanged,
		ProjectID: a.story.ProjectID,
		Payload:   a.story,
		At:        time.Now(),
	})
	a.log.Info("story state changed", "state", newState)

	if newState == domain.StoryStateCompleted {
		ctx.Send(a.epicPID, &MsgStoryCompleted{StoryID: a.story.ID})
	} else if newState == domain.StoryStateInProgress && a.story.State != domain.StoryStateInProgress {
		ctx.Send(a.epicPID, &MsgTaskDispatched{TaskID: 0}) // signal epic to go in_progress
	}
}
