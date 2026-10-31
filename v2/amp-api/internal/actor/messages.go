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
	Human bool
	// DispatchID is the token this dispatch hands out (AMP #2801). The
	// worker presents it to start the ticket (MsgClaimStart) and to
	// complete it. Empty for a person's own start of a human ticket, which
	// is started on the spot (a person works the ticket they started).
	DispatchID string
	ReplyCh    chan ReplySimple
}

// MsgClaimStart is a worker starting the ticket it was dispatched for
// (amp_claim_task with a dispatch_id), or the fallback start of a plugin
// whose first activity counts as one (AMP #2801).
//
// DispatchID is the token the caller presents, or "" for the fallback.
// Inferred marks the fallback: the ticket is started but nothing is posted
// on it, and there is no claim to refuse. A token that isn't the current
// one is refused ("the ticket was returned and re-dispatched"), so a worker
// from an earlier dispatch can't silently take the new run.
type MsgClaimStart struct {
	TaskID     int
	DispatchID string
	Inferred   bool
	// By names the agent starting it for the record ("" for inferred).
	By     string
	ByName string
	// Post is the comment body to post when the caller isn't inferred ("":
	// none), so the claim shows on the ticket as well as in the history.
	Post    string
	ByUser  *int
	ReplyCh chan ReplyStart
}

// ReplyStart answers MsgClaimStart: the ticket as it is after the start,
// whether this call started it (false: it already was, which is not an
// error), and the dispatch it started under.
type ReplyStart struct {
	Task           *domain.Task
	Started        bool
	DispatchID     string
	AlreadyStarted bool
	Err            error
}

// MsgReturnTask sends a dispatched ticket back to ready because no worker
// started it (AMP #2801), or because the run holding its dispatch gave up: the watchdog's automatic return, or a harness
// releasing a ticket whose worker ended without closing it
// (amp_return_task).
type MsgReturnTask struct {
	TaskID int
	Reason string
	// DispatchID, when it is the ticket's current dispatch, lets that run
	// return its own started ticket (AMP #2909). A stale one is refused.
	DispatchID string
	// IfDispatch makes it a compare-and-return, for the watchdog (AMP
	// #2949): the ticket is returned only while ExpectDispatchID ("" too) is
	// still its dispatch and nobody has started it. A ticket returned and
	// dispatched again since the sweep read it holds a fresh dispatch, which
	// is refused rather than returned.
	IfDispatch       bool
	ExpectDispatchID string
	// By names who returned it in the history ("system" for the watchdog).
	By      *int
	ByName  string
	ReplyCh chan ReplyGetTask
}

type MsgCompleteTask struct {
	TaskID int
	By     *int // acting user, for the activity log
	// ByName names the caller in the activity log (an email, or "local-dev")
	// when the ticket has no agent to name.
	ByName string
	// RequireStarted refuses unless the ticket is in progress (or already
	// completed): agents complete only work that was dispatched. Checked in
	// the actor so a state change racing the caller's check can't slip by.
	RequireStarted bool
	// RequireDeps refuses while any dependency is incomplete (re-read from
	// the database), so a dependency added or reopened after dispatch still
	// holds an agent's completion. Set off the web UI.
	RequireDeps bool
	// MergeRequired: the run that finished it put work on a PR or a pushed
	// branch and its project requires merges, so it completes with
	// merge_state "needed" and its dependents keep waiting until the merge
	// is recorded (AMP #2475). MergeWhere names the PR or branch for the
	// waiting detail. Old replicas ignore both (gob drops unknown fields).
	MergeRequired bool
	MergeWhere    string
	// DispatchID is the dispatch token the caller presents (AMP #2801). The
	// current one makes this the worker's own completion; anything else
	// (none, or a token from an earlier dispatch) is a closeout, which the
	// actor records as such. CloseoutReason is the why, for the record.
	DispatchID     string
	CloseoutReason string
	// CloseoutOK lets a completion of a ticket that was never dispatched
	// through as a closeout: it needs prior work on the ticket (the service
	// checks), which is how a ready ticket with runs is closed out without
	// dispatching it first (#2473/#2494).
	CloseoutOK bool
	// InUI is set when a person completes the ticket in the AMP web UI: that
	// is their own act, recorded as theirs, never as a manager's closeout.
	InUI    bool
	ReplyCh chan ReplyComplete
}

// ReplyComplete answers MsgCompleteTask: the ticket after the completion and
// whether it was recorded as a closeout (the caller wasn't its worker).
type ReplyComplete struct {
	Task     *domain.Task
	Closeout bool
	Err      error
}

// MsgSetMergeState changes a ticket's merge state in its actor (AMP #2475):
// a merge recorded, a PR abandoned, a merge marked not needed, or a PR
// reported after completion. When the change releases or holds the
// ticket's dependents, the actor tells them, as completion does.
type MsgSetMergeState struct {
	TaskID int
	State  string // domain.MergeNone / MergeNeeded / MergeMerged / MergeAbandoned
	// Required sets merge_required (nil keeps it).
	Required *bool
	// Source and At record who reported a merge and when (State merged).
	Source string
	At     *time.Time
	// Where names the PR or branch, for the waiting detail when it gates.
	Where string
	// From applies the change only while the ticket's merge state is one of
	// these (empty: any). The reply says whether it changed.
	From   []string
	By     *int
	ByName string
	// Detail is the activity-log line.
	Detail  string
	ReplyCh chan ReplyMergeState
}

// ReplyMergeState is the ticket after MsgSetMergeState, and whether its
// merge state changed.
type ReplyMergeState struct {
	Task    *domain.Task
	Changed bool
	Err     error
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
	// ByLead is set when the person making the change is a project lead or
	// above acting in the AMP web UI (not over MCP or an API token, AMP
	// #2223): their reassignment keeps the ticket's plan approval. Anyone
	// else's drops it when the ticket stops being the person it was approved
	// for (domain.Task.DropsApprovalOn, AMP #2218).
	ByLead  bool
	By      *int
	ByName  string
	ReplyCh chan ReplyGetTask
}

type MsgUpdateTask struct {
	Req domain.UpdateTaskRequest
	// KeepHuman, when set, refuses (with this message) switching a person's
	// ticket to agent work. The task actor checks it atomically with the
	// update; a string so it survives remote encoding.
	KeepHuman string
	ReplyCh   chan ReplySimple
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
	TaskID int
	State  domain.TaskState
	Reason string
	By     *int
	// DispatchID is the dispatch token the caller presents (AMP #2801): set,
	// the caller claims to be the ticket's worker, so its completion to
	// completed isn't recorded as a closeout.
	DispatchID string
	// InUI is set when a person changes the state in the AMP web UI: their
	// own act, never a manager's closeout.
	InUI bool
	// ByName names who changed it in the activity log (default "manager").
	ByName string
	// RequireStarted refuses unless the ticket is in progress (see
	// MsgCompleteTask).
	RequireStarted bool
	// RequireDeps (set off the web UI) keeps a ticket with an incomplete
	// dependency from being made ready or done by hand: to completed or
	// in_progress it's refused; to backlog it's refused from backlog or
	// blocked, and work moved back from in progress or completed lands
	// blocked on its dependencies instead.
	RequireDeps bool
	ReplyCh     chan ReplySimple
}

// MsgSetTaskStartAt updates a task's start_at schedule.
// If StartAt is nil, clears the schedule.
type MsgSetTaskStartAt struct {
	TaskID  int
	StartAt *time.Time
	ReplyCh chan ReplySimple
}

// MsgApproveTask records a person's approval of one ticket's plan (AMP
// #2198). The task actor approves it only if, at that moment, it is still
// open, not already approved as planned now, planned at or before AsOf
// (when set: what the person saw), and owned by ExpectOwner (whom the
// service checked the approver may approve for). Otherwise it replies with
// Approved false and Why. ByName/OnBehalfOf word the history entry.
type MsgApproveTask struct {
	TaskID      int
	AsOf        *time.Time
	ExpectOwner *int
	By          *int
	ByName      string
	OnBehalfOf  string
	ReplyCh     chan ReplyApproveTask
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
	// MergeWait, when set, means the task is done but its merge isn't
	// recorded: dependents keep waiting, and show this as why.
	MergeWait string
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
	MergeWait       string // see MsgDepCompleted
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

// ReplyApproveTask answers MsgApproveTask: the ticket as it is now, whether
// this call approved it, and why not when it didn't.
type ReplyApproveTask struct {
	Task     *domain.Task
	Approved bool
	Why      string
	Err      error
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
