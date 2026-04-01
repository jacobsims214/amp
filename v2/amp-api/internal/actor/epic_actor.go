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

// EpicActor owns one epic's state machine and supervises its StoryActors.
//
// Responsibilities:
//   - Spawns and supervises one StoryActor per story
//   - Routes story/task-targeted messages to the right StoryActor
//   - Tracks which stories are complete to drive epic state rollup
//   - Fans MsgDepCompleted to all child StoryActors (for cross-story unblocking)
//   - Notifies parent ProjectActor when epic transitions state
type EpicActor struct {
	epic        domain.Epic
	stories     map[int]*actor.PID // story_id → StoryActor PID
	storyStates map[int]domain.StoryState
	taskToStory map[int]int // task_id → story_id for O(1) routing
	projectPID  *actor.PID  // parent ProjectActor
	repo        *repository.Repo
	hub         *hub.Hub
	log         *slog.Logger
}

func NewEpicActor(
	epic domain.Epic,
	projectPID *actor.PID,
	repo *repository.Repo,
	h *hub.Hub,
) actor.Actor {
	return &EpicActor{
		epic:        epic,
		stories:     make(map[int]*actor.PID),
		storyStates: make(map[int]domain.StoryState),
		taskToStory: make(map[int]int),
		projectPID:  projectPID,
		repo:        repo,
		hub:         h,
		log:         slog.Default().With("actor", "epic", "epic_id", epic.ID, "project_id", epic.ProjectID),
	}
}

func (a *EpicActor) Receive(ctx actor.Context) {
	switch msg := ctx.Message().(type) {

	case *actor.Started:
		a.loadAndSpawnStories(ctx)

	case *MsgCreateStory:
		story, err := a.handleCreateStory(ctx, msg.Req)
		msg.ReplyCh <- ReplyCreateStory{Story: story, Err: err}

	case *MsgGetStory:
		pid, ok := a.stories[msg.StoryID]
		if !ok {
			msg.ReplyCh <- ReplyGetStory{Err: fmt.Errorf("story %d not found in epic %d", msg.StoryID, a.epic.ID)}
			return
		}
		replyCh := make(chan ReplyGetStory, 1)
		ctx.Send(pid, &MsgGetStory{StoryID: msg.StoryID, ReplyCh: replyCh})
		msg.ReplyCh <- <-replyCh

	case *MsgListEpicTasks:
		tasks := a.collectAllTasks(ctx)
		msg.ReplyCh <- ReplyListTasks{Tasks: tasks}

	case *MsgCreateTask:
		pid, ok := a.stories[msg.Req.StoryID]
		if !ok {
			msg.ReplyCh <- ReplyCreateTask{Err: fmt.Errorf("story %d not found in epic %d", msg.Req.StoryID, a.epic.ID)}
			return
		}
		replyCh := make(chan ReplyCreateTask, 1)
		ctx.Send(pid, &MsgCreateTask{Req: msg.Req, ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err == nil && reply.Task != nil {
			// Register new task in routing index.
			a.taskToStory[reply.Task.ID] = msg.Req.StoryID
		}
		msg.ReplyCh <- reply

	case *MsgGetTask:
		a.routeToStory(ctx, msg.TaskID, msg)

	case *MsgDispatchTask:
		a.routeToStory(ctx, msg.TaskID, msg)

	case *MsgCompleteTask:
		a.routeToStory(ctx, msg.TaskID, msg)

	case *MsgBlockTask:
		a.routeToStory(ctx, msg.TaskID, msg)

	case *MsgUpdateTask:
		a.routeToStory(ctx, msg.Req.TaskID, msg)

	case *MsgSetTaskState:
		a.routeToStory(ctx, msg.TaskID, msg)

	case *MsgAddComment:
		a.routeToStory(ctx, msg.Req.TaskID, msg)

	case *MsgGetComments:
		a.routeToStory(ctx, msg.TaskID, msg)

	case *MsgDeleteTask:
		a.routeToStory(ctx, msg.TaskID, msg)
		delete(a.taskToStory, msg.TaskID)

	case *MsgDepCompleted:
		// Fan out to all child StoryActors.
		for _, pid := range a.stories {
			ctx.Send(pid, msg)
		}

	case *MsgTaskDispatched:
		// A task in this epic was dispatched — roll up epic state.
		a.onTaskDispatched(ctx)

	case *MsgStoryCompleted:
		a.storyStates[msg.StoryID] = domain.StoryStateCompleted
		a.rollupState(ctx)

	case *MsgDeleteEpic:
		// Stop all children.
		for storyID, pid := range a.stories {
			ctx.Stop(pid)
			delete(a.stories, storyID)
		}
		a.storyStates = make(map[int]domain.StoryState)
		msg.ReplyCh <- ReplySimple{}

	case *MsgReset:
		for storyID, pid := range a.stories {
			replyCh := make(chan ReplySimple, 1)
			ctx.Send(pid, &MsgReset{ReplyCh: replyCh})
			<-replyCh
			ctx.Stop(pid)
			delete(a.stories, storyID)
		}
		a.storyStates = make(map[int]domain.StoryState)
		a.taskToStory = make(map[int]int)
		msg.ReplyCh <- ReplySimple{}
	}
}

// ---- Helpers ----

func (a *EpicActor) loadAndSpawnStories(ctx actor.Context) {
	stories, err := a.repo.ListStories(context.Background(), a.epic.ID)
	if err != nil {
		a.log.Error("failed to load stories", "err", err)
		return
	}
	for _, s := range stories {
		pid := a.spawnStoryActor(ctx, s)
		a.stories[s.ID] = pid
		a.storyStates[s.ID] = s.State
	}
	// Build task→story index from DB (one cheap query per epic on startup).
	taskIDs, err := a.repo.ListTaskIDsByEpic(context.Background(), a.epic.ID)
	if err != nil {
		a.log.Error("failed to load task IDs", "err", err)
		return
	}
	for taskID, storyID := range taskIDs {
		a.taskToStory[taskID] = storyID
	}
	a.log.Info("loaded stories", "count", len(stories), "tasks", len(taskIDs))
}

func (a *EpicActor) spawnStoryActor(ctx actor.Context, s domain.Story) *actor.PID {
	props := actor.PropsFromProducer(func() actor.Actor {
		return NewStoryActor(s, ctx.Self(), a.projectPID, a.repo, a.hub)
	})
	name := fmt.Sprintf("epic-%d-story-%d", a.epic.ID, s.ID)
	pid, err := ctx.SpawnNamed(props, name)
	if err != nil {
		pid = ctx.ActorSystem().NewLocalPID(name)
		a.log.Warn("story actor already exists, reusing pid", "story_id", s.ID)
	}
	return pid
}

func (a *EpicActor) handleCreateStory(ctx actor.Context, req domain.CreateStoryRequest) (*domain.Story, error) {
	story, err := a.repo.CreateStory(context.Background(), req)
	if err != nil {
		return nil, fmt.Errorf("db create story: %w", err)
	}
	pid := a.spawnStoryActor(ctx, *story)
	a.stories[story.ID] = pid
	a.storyStates[story.ID] = story.State

	a.hub.Publish(domain.Event{
		Type:      domain.EventStoryCreated,
		ProjectID: a.epic.ProjectID,
		Payload:   story,
		At:        time.Now(),
	})
	a.log.Info("story created", "story_id", story.ID)
	return story, nil
}

// routeToStory routes a task-targeted message to the correct StoryActor using O(1) lookup.
func (a *EpicActor) routeToStory(ctx actor.Context, taskID int, msg interface{}) {
	storyID, ok := a.taskToStory[taskID]
	if !ok {
		a.log.Warn("task not found in routing index, broadcasting to all stories", "task_id", taskID)
		// Fallback: broadcast — the correct story will handle it.
		for _, pid := range a.stories {
			ctx.Send(pid, msg)
		}
		return
	}
	pid, ok := a.stories[storyID]
	if !ok {
		a.log.Error("story pid not found", "story_id", storyID, "task_id", taskID)
		return
	}
	ctx.Send(pid, msg)
}

func (a *EpicActor) collectAllTasks(ctx actor.Context) []domain.Task {
	var out []domain.Task
	for _, pid := range a.stories {
		replyCh := make(chan ReplyListTasks, 1)
		ctx.Send(pid, &MsgListStoryTasks{ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err == nil {
			out = append(out, reply.Tasks...)
		}
	}
	return out
}

func (a *EpicActor) onTaskDispatched(ctx actor.Context) {
	if a.epic.State == domain.EpicStateInProgress || a.epic.State == domain.EpicStateCompleted {
		return
	}
	a.epic.State = domain.EpicStateInProgress
	if err := a.repo.SetEpicState(context.Background(), a.epic.ID, domain.EpicStateInProgress); err != nil {
		a.log.Error("failed to persist epic in_progress", "err", err)
		return
	}
	a.hub.Publish(domain.Event{
		Type:      domain.EventEpicStateChanged,
		ProjectID: a.epic.ProjectID,
		Payload:   a.epic,
		At:        time.Now(),
	})
	a.log.Info("epic moved to in_progress")
	ctx.Send(a.projectPID, &MsgTaskDispatched{TaskID: 0})
}

func (a *EpicActor) rollupState(ctx actor.Context) {
	if len(a.storyStates) == 0 {
		return
	}
	allComplete := true
	for _, s := range a.storyStates {
		if s != domain.StoryStateCompleted {
			allComplete = false
			break
		}
	}
	if !allComplete || a.epic.State == domain.EpicStateCompleted {
		return
	}

	a.epic.State = domain.EpicStateCompleted
	if err := a.repo.SetEpicState(context.Background(), a.epic.ID, domain.EpicStateCompleted); err != nil {
		a.log.Error("failed to persist epic completed", "err", err)
		return
	}
	a.hub.Publish(domain.Event{
		Type:      domain.EventEpicStateChanged,
		ProjectID: a.epic.ProjectID,
		Payload:   a.epic,
		At:        time.Now(),
	})
	a.log.Info("epic completed")
	ctx.Send(a.projectPID, &MsgEpicCompleted{EpicID: a.epic.ID})
}
