package actor

import (
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// ============================================================
// External request/reply messages — sent by MCP/REST handlers
// to ProjectActor, which routes them down the hierarchy.
// All have a ReplyCh so the caller can block waiting for the result.
// ============================================================

// ---- Epic ----

type MsgCreateEpic struct {
	Req     domain.CreateEpicRequest
	ReplyCh chan ReplyCreateEpic
}

type MsgGetEpic struct {
	EpicID  int
	ReplyCh chan ReplyGetEpic
}

type MsgListEpicTasks struct {
	EpicID  int
	ReplyCh chan ReplyListTasks
}

// ---- Story ----

type MsgCreateStory struct {
	Req     domain.CreateStoryRequest
	ReplyCh chan ReplyCreateStory
}

type MsgGetStory struct {
	StoryID int
	ReplyCh chan ReplyGetStory
}

type MsgListStoryTasks struct {
	StoryID int
	ReplyCh chan ReplyListTasks
}

// ---- Task ----

type MsgCreateTask struct {
	Req     domain.CreateTaskRequest
	ReplyCh chan ReplyCreateTask
}

type MsgGetTask struct {
	TaskID  int
	ReplyCh chan ReplyGetTask
}

type MsgListTasks struct {
	ProjectID int
	State     string
	ReplyCh   chan ReplyListTasks
}

type MsgDispatchTask struct {
	TaskID  int
	AgentID string
	ReplyCh chan ReplySimple
}

type MsgCompleteTask struct {
	TaskID  int
	ReplyCh chan ReplySimple
}

type MsgBlockTask struct {
	TaskID  int
	Reason  string
	ReplyCh chan ReplySimple
}

type MsgUpdateTask struct {
	Req     domain.UpdateTaskRequest
	ReplyCh chan ReplySimple
}

type MsgAddComment struct {
	Req     domain.AddCommentRequest
	ReplyCh chan ReplyAddComment
}

type MsgGetComments struct {
	TaskID  int
	ReplyCh chan ReplyGetComments
}

type MsgSetTaskState struct {
	TaskID  int
	State   domain.TaskState
	Reason  string
	ReplyCh chan ReplySimple
}

// MsgSetTaskStartAt updates a task's start_at schedule.
// If StartAt is nil, clears the schedule.
type MsgSetTaskStartAt struct {
	TaskID  int
	StartAt *time.Time
	ReplyCh chan ReplySimple
}

// MsgScheduledUnblock is sent by the timer goroutine to the TaskActor
// when a scheduled task's start_at time has been reached.
type MsgScheduledUnblock struct {
	TaskID int
}

// ---- Management ----

// MsgReset clears all children — called after amp_reset_project wipes postgres.
type MsgReset struct {
	ReplyCh chan ReplySimple
}

// MsgDeleteEpic stops the EpicActor and evicts all its tasks from indexes.
type MsgDeleteEpic struct {
	EpicID  int
	ReplyCh chan ReplySimple
}

// MsgDeleteStory removes a story and all its tasks from the actor hierarchy.
type MsgDeleteStory struct {
	StoryID int
	ReplyCh chan ReplySimple
}

// MsgDeleteTask removes a single task from the hierarchy.
type MsgDeleteTask struct {
	TaskID  int
	ReplyCh chan ReplySimple
}

// ============================================================
// Internal notification messages — fire-and-forget, no ReplyCh.
// Flow upward through the hierarchy to trigger state rollup.
// ============================================================

// MsgDepCompleted is broadcast by ProjectActor to ALL EpicActors
// whenever any task completes. Each TaskActor checks its own deps.
type MsgDepCompleted struct {
	CompletedTaskID int
}

// MsgTaskDispatched bubbles up: TaskActor → StoryActor → EpicActor → ProjectActor
// to trigger in_progress rollup at each level.
type MsgTaskDispatched struct {
	TaskID int
}

// MsgTaskCompleted bubbles up: TaskActor → StoryActor → EpicActor → ProjectActor
type MsgTaskCompleted struct {
	TaskID int
}

// MsgFanDepCompleted is sent synchronously from a completing TaskActor to
// ProjectActor. ProjectActor broadcasts MsgDepCompleted to all EpicActors
// (which fan down to StoryActors → TaskActors) and replies only after all
// fan-out messages have been queued. This ensures unblocking is observable
// immediately after MsgCompleteTask returns.
type MsgFanDepCompleted struct {
	CompletedTaskID int
	ReplyCh         chan ReplySimple
}

// MsgStoryCompleted bubbles up: StoryActor → EpicActor → ProjectActor
type MsgStoryCompleted struct {
	StoryID int
}

// MsgEpicCompleted bubbles up: EpicActor → ProjectActor
type MsgEpicCompleted struct {
	EpicID int
}

// MsgRegisterTask lets StoryActor inform ProjectActor of a new task ID→EpicID
// mapping so ProjectActor can route task-targeted messages in O(1).
type MsgRegisterTask struct {
	TaskID int
	EpicID int
}

// MsgDeregisterTask lets StoryActor inform ProjectActor that a task was deleted.
type MsgDeregisterTask struct {
	TaskID int
}

// ============================================================
// Reply types
// ============================================================

type ReplyCreateEpic struct {
	Epic *domain.Epic
	Err  error
}

type ReplyGetEpic struct {
	Epic *domain.Epic
	Err  error
}

type ReplyCreateStory struct {
	Story *domain.Story
	Err   error
}

type ReplyGetStory struct {
	Story *domain.Story
	Err   error
}

type ReplyCreateTask struct {
	Task *domain.Task
	Err  error
}

type ReplyGetTask struct {
	Task *domain.Task
	Err  error
}

type ReplyListTasks struct {
	Tasks []domain.Task
	Err   error
}

type ReplySimple struct {
	Err error
}

type ReplyAddComment struct {
	Comment *domain.Comment
	Err     error
}

type ReplyGetComments struct {
	Comments []domain.Comment
	Err      error
}
