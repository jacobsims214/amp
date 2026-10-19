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

type MsgUpdateEpic struct {
	Req     domain.UpdateEpicRequest
	ReplyCh chan ReplySimple
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

type MsgUpdateStory struct {
	Req     domain.UpdateStoryRequest
	ReplyCh chan ReplySimple
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
	// By is the AMP user dispatching. An unassigned ticket is claimed by
	// them in the same step; a ticket owned by someone else is refused
	// unless Override is set (org admins). nil = no ownership rules (local dev).
	By       *int
	Override bool
	// Human is set when a person starts their own human ticket (the UI's
	// Start). Agents never start human tickets, and people don't start
	// agent tickets this way.
	Human   bool
	ReplyCh chan ReplySimple
}

type MsgCompleteTask struct {
	TaskID  int
	By      *int // acting user, for the activity log
	ReplyCh chan ReplySimple
}

type MsgBlockTask struct {
	TaskID  int
	Reason  string
	By      *int
	ReplyCh chan ReplySimple
}

// MsgAssignTask changes a ticket's single owner as a compare-and-set: it is
// applied only if the current owner still equals ExpectOwner — the owner the
// caller was authorised against — so two concurrent claims can't both win.
// NewOwner nil releases the ticket; an in-progress ticket goes back to backlog.
type MsgAssignTask struct {
	TaskID      int
	ExpectOwner *int
	NewOwner    *int
	// RequireIdle refuses the change if the ticket is in progress (set when
	// only the project's open-reassign setting allows it).
	RequireIdle bool
	By          *int
	ByName      string
	ReplyCh     chan ReplyGetTask
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
	By      *int
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

// MsgPing asks the project actor to answer once it is running.
type MsgPing struct {
	ReplyCh chan ReplySimple
}

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
	// Detach stops the task's actor for a move rather than a delete: the
	// task stays in the routing index, which the mover updates itself.
	Detach bool
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

// MsgDeregisterTask lets a stopping TaskActor tell ProjectActor to drop it
// from the routing index. EpicID is the epic it was in: a task that moved
// to another epic has already been re-registered there, and a late
// deregistration from its old home must not undo that.
type MsgDeregisterTask struct {
	TaskID int
	EpicID int
}

// MsgSetTaskDeps replaces a task's dependencies (already validated) and
// re-derives whether it is waiting on any.
type MsgSetTaskDeps struct {
	TaskID        int
	DependencyIDs []int
	ReplyCh       chan ReplyGetTask
}

// MsgReconcileDeps makes a task re-read its dependencies from the database
// (e.g. after one of them was deleted) and re-derive whether it is waiting.
type MsgReconcileDeps struct {
	TaskID  int
	ReplyCh chan ReplySimple
}

// MsgRestoreTask brings a ticket restored from the trash (already back in
// the database) into its story's actors.
type MsgRestoreTask struct {
	TaskID  int
	EpicID  int
	StoryID int
	ReplyCh chan ReplyGetTask
}

// MsgRestoreEpic brings an epic restored from the trash (already back in
// the database, with its stories) into the project's actors. Its tickets
// follow with MsgRestoreTask.
type MsgRestoreEpic struct {
	EpicID  int
	ReplyCh chan ReplySimple
}

// MsgRestoreStory brings a story restored from the trash (already back in
// the database) into its epic's actors. Its tickets follow with
// MsgRestoreTask.
type MsgRestoreStory struct {
	EpicID  int
	StoryID int
	ReplyCh chan ReplySimple
}

// MsgMoveTask moves a task to another story (in the same or another epic).
type MsgMoveTask struct {
	TaskID     int
	FromEpicID int
	ToEpicID   int
	ToStoryID  int
	ReplyCh    chan ReplyGetTask
}

// MsgAdoptTask tells a story (via its epic) to start managing a task that
// was just moved into it.
type MsgAdoptTask struct {
	TaskID  int
	StoryID int
	ReplyCh chan ReplyGetTask
}

// MsgMoveStory moves a story, with its tasks, to another epic.
type MsgMoveStory struct {
	StoryID    int
	FromEpicID int
	ToEpicID   int
	ReplyCh    chan ReplySimple
}

// MsgAdoptStory tells an epic to start managing a story just moved into it.
type MsgAdoptStory struct {
	StoryID int
	ReplyCh chan ReplyAdoptStory
}

type ReplyAdoptStory struct {
	TaskIDs []int
	Err     error
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
