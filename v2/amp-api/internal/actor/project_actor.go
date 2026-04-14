// Package actor implements the AMP actor system.
//
// Architecture (Erlang-inspired, built with protoactor-go):
//
//	ActorSystem
//	└── Registry (maps project_id → ProjectActor PID)
//	    └── ProjectActor  (one per project)
//	        └── EpicActor  (one per epic, child of ProjectActor)
//	            └── StoryActor  (one per story, child of EpicActor)
//	                └── TaskActor  (one per task, child of StoryActor)
//
// ProjectActor is the entry point for all external requests (MCP + REST).
// It routes task-targeted messages to the correct EpicActor using an O(1)
// taskID→epicID index, then propagates down through StoryActor to TaskActor.
//
// The hierarchy drives state rollup automatically:
//
//	Task completes → notifies StoryActor → notifies EpicActor → notifies ProjectActor
//	Completed dep → ProjectActor fans MsgDepCompleted to all EpicActors
//	  → each EpicActor fans to all StoryActors
//	    → each StoryActor fans to all TaskActors
//	      → each blocked TaskActor checks its own deps in O(degree)
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

// ProjectActor manages all epics for a single project.
// It holds an O(1) routing index (taskID→epicID) to route task messages.
type ProjectActor struct {
	projectID  int
	epics      map[int]*actor.PID // epic_id → EpicActor PID
	epicStates map[int]domain.EpicState
	taskToEpic map[int]int // task_id → epic_id for O(1) routing
	repo       *repository.Repo
	hub        *hub.Hub
	log        *slog.Logger
}

func NewProjectActor(projectID int, repo *repository.Repo, h *hub.Hub) actor.Actor {
	return &ProjectActor{
		projectID:  projectID,
		epics:      make(map[int]*actor.PID),
		epicStates: make(map[int]domain.EpicState),
		taskToEpic: make(map[int]int),
		repo:       repo,
		hub:        h,
		log:        slog.Default().With("actor", "project", "project_id", projectID),
	}
}

// Receive is the project actor's message loop — one message at a time.
func (a *ProjectActor) Receive(ctx actor.Context) {
	switch msg := ctx.Message().(type) {

	case *actor.Started:
		a.loadAndSpawnEpics(ctx)

	// ---- Epic operations ----

	case *MsgCreateEpic:
		epic, err := a.handleCreateEpic(ctx, msg.Req)
		msg.ReplyCh <- ReplyCreateEpic{Epic: epic, Err: err}

	case *MsgGetEpic:
		pid, ok := a.epics[msg.EpicID]
		if !ok {
			msg.ReplyCh <- ReplyGetEpic{Err: fmt.Errorf("epic %d not found", msg.EpicID)}
			return
		}
		replyCh := make(chan ReplyGetEpic, 1)
		ctx.Send(pid, &MsgGetEpic{EpicID: msg.EpicID, ReplyCh: replyCh})
		msg.ReplyCh <- <-replyCh

	// ---- Story operations ----

	case *MsgCreateStory:
		pid, ok := a.epics[msg.Req.EpicID]
		if !ok {
			msg.ReplyCh <- ReplyCreateStory{Err: fmt.Errorf("epic %d not found", msg.Req.EpicID)}
			return
		}
		replyCh := make(chan ReplyCreateStory, 1)
		ctx.Send(pid, &MsgCreateStory{Req: msg.Req, ReplyCh: replyCh})
		msg.ReplyCh <- <-replyCh

	case *MsgGetStory:
		// Route to the correct epic — need to find which epic owns this story.
		// We don't have a storyToEpic index, so broadcast and take first reply.
		a.broadcastToEpics(ctx, msg)

	// ---- Task operations — route via taskToEpic index ----

	case *MsgCreateTask:
		pid, ok := a.epics[msg.Req.EpicID]
		if !ok {
			msg.ReplyCh <- ReplyCreateTask{Err: fmt.Errorf("epic %d not found", msg.Req.EpicID)}
			return
		}
		replyCh := make(chan ReplyCreateTask, 1)
		ctx.Send(pid, &MsgCreateTask{Req: msg.Req, ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err == nil && reply.Task != nil {
			a.taskToEpic[reply.Task.ID] = msg.Req.EpicID
		}
		msg.ReplyCh <- reply

	case *MsgGetTask:
		a.routeTaskMsg(ctx, msg.TaskID, msg)

	case *MsgListTasks:
		tasks := a.collectAllTasks(ctx)
		// Apply state filter if provided
		if msg.State != "" {
			filtered := tasks[:0]
			for _, t := range tasks {
				if string(t.State) == msg.State {
					filtered = append(filtered, t)
				}
			}
			tasks = filtered
		}
		msg.ReplyCh <- ReplyListTasks{Tasks: tasks}

	case *MsgDispatchTask:
		a.routeTaskMsg(ctx, msg.TaskID, msg)

	case *MsgCompleteTask:
		// Route to the task, then fan MsgDepCompleted to ALL epics.
		a.routeTaskMsg(ctx, msg.TaskID, msg)
		// Fan-out happens via MsgTaskCompleted notification bubbling up,
		// which triggers ProjectActor to broadcast (see MsgTaskCompleted below).

	case *MsgBlockTask:
		a.routeTaskMsg(ctx, msg.TaskID, msg)

	case *MsgUpdateTask:
		a.routeTaskMsg(ctx, msg.Req.TaskID, msg)

	case *MsgSetTaskState:
		a.routeTaskMsg(ctx, msg.TaskID, msg)

	case *MsgAddComment:
		a.routeTaskMsg(ctx, msg.Req.TaskID, msg)

	case *MsgGetComments:
		a.routeTaskMsg(ctx, msg.TaskID, msg)

	case *MsgDeleteTask:
		a.routeTaskMsg(ctx, msg.TaskID, msg)
		delete(a.taskToEpic, msg.TaskID)

	// ---- Internal routing index maintenance ----

	case *MsgRegisterTask:
		a.taskToEpic[msg.TaskID] = msg.EpicID

	case *MsgDeregisterTask:
		delete(a.taskToEpic, msg.TaskID)

	// ---- Upward notifications from children — drive dep fan-out ----

	case *MsgFanDepCompleted:
		// Synchronous dep fan-out: send MsgDepCompleted to all EpicActors.
		// Each EpicActor fans to StoryActors, each StoryActor fans to TaskActors.
		// We Send (not request/reply) to each epic — the messages are queued and
		// will be processed by the time the caller reads task state, because the
		// caller's next message (MsgGetTask) goes through the same actor mailboxes.
		depMsg := &MsgDepCompleted{CompletedTaskID: msg.CompletedTaskID}
		for _, pid := range a.epics {
			ctx.Send(pid, depMsg)
		}
		msg.ReplyCh <- ReplySimple{}

	case *MsgTaskCompleted:
		// Upward rollup notification (async) — no dep fan-out here.
		// Fan-out was already handled synchronously via MsgFanDepCompleted.

	case *MsgTaskDispatched:
		// A task was dispatched in some epic — no project-level action needed.
		// (Epic handles its own in_progress rollup)

	case *MsgStoryCompleted:
		// Bubbled from EpicActor — no project-level action needed currently.

	case *MsgEpicCompleted:
		a.epicStates[msg.EpicID] = domain.EpicStateCompleted
		a.log.Info("epic completed", "epic_id", msg.EpicID)

	// ---- Management ----

	case *MsgReset:
		for epicID, pid := range a.epics {
			replyCh := make(chan ReplySimple, 1)
			ctx.Send(pid, &MsgReset{ReplyCh: replyCh})
			<-replyCh
			ctx.Stop(pid)
			delete(a.epics, epicID)
		}
		a.epicStates = make(map[int]domain.EpicState)
		a.taskToEpic = make(map[int]int)
		a.log.Info("project reset — all children stopped")
		msg.ReplyCh <- ReplySimple{}

	case *MsgDeleteEpic:
		pid, ok := a.epics[msg.EpicID]
		if !ok {
			msg.ReplyCh <- ReplySimple{}
			return
		}
		// Tell EpicActor to stop its children.
		replyCh := make(chan ReplySimple, 1)
		ctx.Send(pid, &MsgDeleteEpic{EpicID: msg.EpicID, ReplyCh: replyCh})
		<-replyCh
		ctx.Stop(pid)
		delete(a.epics, msg.EpicID)
		delete(a.epicStates, msg.EpicID)
		// Clean up routing index entries for this epic's tasks.
		for taskID, epicID := range a.taskToEpic {
			if epicID == msg.EpicID {
				delete(a.taskToEpic, taskID)
			}
		}
		msg.ReplyCh <- ReplySimple{}

	case *MsgDeleteStory:
		// Find which epic owns this story by broadcasting.
		// (We don't have a storyToEpic index at project level — broadcast to all epics.)
		for _, epicPID := range a.epics {
			replyCh := make(chan ReplySimple, 1)
			ctx.Send(epicPID, &MsgDeleteStory{StoryID: msg.StoryID, ReplyCh: replyCh})
			reply := <-replyCh
			if reply.Err == nil {
				break // found and handled
			}
		}
		msg.ReplyCh <- ReplySimple{}
	}
}

// ---- Helpers ----

func (a *ProjectActor) loadAndSpawnEpics(ctx actor.Context) {
	epics, err := a.repo.ListEpics(context.Background(), a.projectID)
	if err != nil {
		a.log.Error("failed to load epics", "err", err)
		return
	}
	for _, e := range epics {
		pid := a.spawnEpicActor(ctx, e)
		a.epics[e.ID] = pid
		a.epicStates[e.ID] = e.State
	}

	// Build task routing index.
	taskIndex, err := a.repo.ListTaskIDsByProject(context.Background(), a.projectID)
	if err != nil {
		a.log.Error("failed to load task routing index", "err", err)
		return
	}
	a.taskToEpic = taskIndex
	a.log.Info("started", "epics", len(epics), "tasks_indexed", len(taskIndex))
}

func (a *ProjectActor) spawnEpicActor(ctx actor.Context, e domain.Epic) *actor.PID {
	props := actor.PropsFromProducer(func() actor.Actor {
		return NewEpicActor(e, ctx.Self(), a.repo, a.hub)
	})
	name := fmt.Sprintf("project-%d-epic-%d", a.projectID, e.ID)
	pid, err := ctx.SpawnNamed(props, name)
	if err != nil {
		pid = ctx.ActorSystem().NewLocalPID(name)
		a.log.Warn("epic actor already exists, reusing pid", "epic_id", e.ID)
	}
	return pid
}

func (a *ProjectActor) handleCreateEpic(ctx actor.Context, req domain.CreateEpicRequest) (*domain.Epic, error) {
	epic, err := a.repo.CreateEpic(context.Background(), req)
	if err != nil {
		return nil, fmt.Errorf("db create epic: %w", err)
	}
	pid := a.spawnEpicActor(ctx, *epic)
	a.epics[epic.ID] = pid
	a.epicStates[epic.ID] = epic.State

	a.hub.Publish(domain.Event{
		Type:      domain.EventEpicCreated,
		ProjectID: a.projectID,
		Payload:   epic,
		At:        time.Now(),
	})
	a.log.Info("epic created", "epic_id", epic.ID)
	return epic, nil
}

// routeTaskMsg routes a task-targeted message to the correct EpicActor.
func (a *ProjectActor) routeTaskMsg(ctx actor.Context, taskID int, msg interface{}) {
	epicID, ok := a.taskToEpic[taskID]
	if !ok {
		a.log.Warn("task not in routing index, broadcasting to all epics", "task_id", taskID)
		a.broadcastToEpics(ctx, msg)
		return
	}
	pid, ok := a.epics[epicID]
	if !ok {
		a.log.Error("epic pid not found for task", "task_id", taskID, "epic_id", epicID)
		return
	}
	ctx.Send(pid, msg)
}

// broadcastToEpics sends a message to all EpicActors.
// Used as fallback when routing index misses (e.g., first access after restart)
// and for messages like MsgGetStory that don't have an epicID hint.
func (a *ProjectActor) broadcastToEpics(ctx actor.Context, msg interface{}) {
	for _, pid := range a.epics {
		ctx.Send(pid, msg)
	}
}

func (a *ProjectActor) collectAllTasks(ctx actor.Context) []domain.Task {
	var out []domain.Task
	for _, pid := range a.epics {
		replyCh := make(chan ReplyListTasks, 1)
		ctx.Send(pid, &MsgListEpicTasks{ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err == nil {
			out = append(out, reply.Tasks...)
		}
	}
	return out
}

// publish is kept for compatibility — hub is called directly where needed.
func (a *ProjectActor) publish(evtType domain.EventType, payload interface{}) {
	a.hub.Publish(domain.Event{
		Type:      evtType,
		ProjectID: a.projectID,
		Payload:   payload,
		At:        time.Now(),
	})
}
