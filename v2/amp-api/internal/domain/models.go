package domain

import "time"

// ---- Enums ----

type TaskState string

const (
	TaskStateBacklog    TaskState = "backlog"
	TaskStateInProgress TaskState = "in_progress"
	TaskStateCompleted  TaskState = "completed"
	TaskStateBlocked    TaskState = "blocked"
)

type ProjectState string

const (
	ProjectStateActive   ProjectState = "active"
	ProjectStateArchived ProjectState = "archived"
)

type EpicState string

const (
	EpicStateBacklog    EpicState = "backlog"
	EpicStateInProgress EpicState = "in_progress"
	EpicStateCompleted  EpicState = "completed"
	EpicStateBlocked    EpicState = "blocked"
)

type StoryState string

const (
	StoryStateBacklog    StoryState = "backlog"
	StoryStateInProgress StoryState = "in_progress"
	StoryStateCompleted  StoryState = "completed"
	StoryStateBlocked    StoryState = "blocked"
)

// ---- Core models ----

type Project struct {
	ID          int          `json:"id"`
	Name        string       `json:"name"`
	Code        string       `json:"code"`
	Description string       `json:"description"`
	State       ProjectState `json:"state"`
	// OrgID is the org the project belongs to. Only that org's teams can be
	// linked to it. Nil only transiently for rows predating orgs.
	OrgID     *int `json:"org_id"`
	CreatedBy *int `json:"created_by"`
	// OpenReassign lets any project member take or reassign a ticket that
	// isn't in progress. Otherwise only its owner or a lead can.
	OpenReassign bool `json:"open_reassign"`
	// Plan approval: when a person last approved the plan, and whether
	// tickets created after that need a new approval before dispatch.
	PlanApprovedAt      *time.Time `json:"plan_approved_at,omitempty"`
	PlanApprovedBy      *int       `json:"plan_approved_by,omitempty"`
	RequirePlanApproval bool       `json:"require_plan_approval"`
	// RequireMerge (default on): a ticket that completes with a PR or a
	// pushed branch holds the tickets that depend on it until its merge is
	// recorded (Task.MergeState). Off, dependents start on completion.
	// RequireMergeSince is when it last went on: tickets completed before
	// then are never gated.
	RequireMerge      bool       `json:"require_merge"`
	RequireMergeSince *time.Time `json:"require_merge_since,omitempty"`
	// Metadata is free-form key/value data for integrations (see Metadata).
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AwaitsApproval reports whether ticket t still waits for a person to
// approve it. Since AMP #2198 approval is per ticket (Task.Approved): the
// project's PlanApprovedAt is only when its last approval happened and no
// longer decides. The project receiver is kept so callers read the same.
func (p *Project) AwaitsApproval(t *Task) bool {
	return !t.Approved()
}

// ProjectSettings is a change to a project's settings; nil fields are kept.
type ProjectSettings struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	OpenReassign *bool   `json:"open_reassign"`
	// RequirePlanApproval: tickets planned after the last approval can't be
	// dispatched until someone approves the plan again.
	RequirePlanApproval *bool `json:"require_plan_approval"`
	// RequireMerge: done tickets with an unmerged PR hold their dependents.
	RequireMerge *bool `json:"require_merge"`
	// Metadata sets keys; null removes one (see MetadataPatch).
	Metadata MetadataPatch `json:"metadata,omitempty"`
}

type Epic struct {
	ID          int       `json:"id"`
	ProjectID   int       `json:"project_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	State       EpicState `json:"state"`
	Priority    string    `json:"priority"`
	// CreatedBy is the AMP user who created it; nil for rows predating
	// attribution. OwnerIDs are the users who own this shared grouping.
	CreatedBy *int  `json:"created_by"`
	OwnerIDs  []int `json:"owner_ids"`
	// Metadata is free-form key/value data for integrations (see Metadata).
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Story struct {
	ID                 int        `json:"id"`
	ProjectID          int        `json:"project_id"`
	EpicID             int        `json:"epic_id"`
	Name               string     `json:"name"`
	Description        string     `json:"description"`
	AcceptanceCriteria string     `json:"acceptance_criteria"`
	State              StoryState `json:"state"`
	Priority           string     `json:"priority"`
	CreatedBy          *int       `json:"created_by"`
	OwnerIDs           []int      `json:"owner_ids"`
	// Metadata is free-form key/value data for integrations (see Metadata).
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Task struct {
	ID                 int       `json:"id"`
	ProjectID          int       `json:"project_id"`
	EpicID             int       `json:"epic_id"`  // required — every task belongs to an epic
	StoryID            int       `json:"story_id"` // required — every task belongs to a story
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	AcceptanceCriteria string    `json:"acceptance_criteria"`
	State              TaskState `json:"state"`
	Priority           string    `json:"priority"`
	// DependencyIDs are the task IDs that must complete before this task can run.
	// The actor derives state: empty → backlog, any incomplete dep → blocked.
	// Agents never set state directly on create.
	DependencyIDs []int `json:"dependency_ids"`
	// BlockedByIDs is the subset of DependencyIDs not yet completed.
	// Computed on every read — never stored.
	BlockedByIDs []int `json:"blocked_by_ids,omitempty"`
	// AssignedTo is set by the manager at planning time — who should work this ticket.
	// This is free text (e.g. "amp-worker", "amp-worker-frontend").
	// It is shown on the board before dispatch so the user can review and correct it.
	AssignedTo string `json:"assigned_to,omitempty"`
	// AgentID is set at dispatch time by the actor — who is actually working it now.
	AgentID      string     `json:"agent_id,omitempty"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	BlockReason  string     `json:"block_reason,omitempty"`
	StartAt      *time.Time `json:"start_at,omitempty"`
	// CreatedBy is the AMP user who created the ticket (nil for legacy rows).
	CreatedBy *int `json:"created_by"`
	// OwnerUserID is the one AMP user who owns the work; nil = unassigned,
	// available for anyone on the project (or their agents) to claim.
	// Distinct from AssignedTo/AgentID, which name the agent profile.
	OwnerUserID *int `json:"owner_user_id"`
	// Performer is who does the work: "agent" (dispatched to an agent) or
	// "human" (its owner does it and tracks it in AMP). A human ticket sits
	// in the dependency graph like any other: what depends on it waits for
	// the person.
	Performer string `json:"performer"`
	// HumanReason says why a person does it (see HumanReasons).
	HumanReason string `json:"human_reason,omitempty"`
	// ReadyAt is when the ticket last became ready (backlog): ready → started
	// is time spent waiting for someone to pick it up.
	ReadyAt *time.Time `json:"ready_at,omitempty"`
	// Metadata is free-form key/value data for integrations (see Metadata).
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// PlannedAt is when the ticket's scope was last set: its creation, its
	// restore from the trash, or the last change to its name, description,
	// or acceptance criteria. A plan approval covers tickets planned before
	// it (Project.AwaitsApproval); comments and dependency changes don't
	// move it.
	PlannedAt time.Time `json:"planned_at"`
	// ApprovedAt is when a person last approved the ticket's plan, and
	// ApprovedBy who (nil: approved by a migration, or the person's account
	// is gone). The ticket is approved iff ApprovedAt is at or after
	// PlannedAt (Approved): re-planning it makes it wait again.
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	ApprovedBy *int       `json:"approved_by,omitempty"`
	// ApprovedFor is whose ticket it was when it was approved: its owner,
	// or for an unassigned ticket the person who planned it (nil: nobody's,
	// a legacy row). When a Member moves the ticket to someone else, the
	// approval is dropped; a project lead's reassignment and dispatch's
	// automatic claim keep it (AMP #2218, Task.DropsApprovalOn).
	ApprovedFor *int `json:"approved_for,omitempty"`
	// Merge signal (AMP #2475). MergeRequired is set when the ticket
	// completes with a PR or pushed branch while its project requires
	// merges; MergeState is then "needed" until the merge is recorded, and
	// the tickets that depend on it wait (see MergeGates). MergedAt and
	// MergeSource are the first merge report (ui, ci or github).
	MergeRequired bool       `json:"merge_required"`
	MergeState    string     `json:"merge_state"`
	MergedAt      *time.Time `json:"merged_at,omitempty"`
	MergeSource   string     `json:"merge_source,omitempty"`
	// WaitingDetail says why a ticket still waits when a dependency it
	// waits on is done but not merged ("Waiting on merge of #N (PR …)").
	// Computed by its actor, never stored.
	WaitingDetail string `json:"waiting_detail,omitempty"`
}

// Merge states (Task.MergeState).
const (
	MergeNone      = "none"
	MergeNeeded    = "needed"
	MergeMerged    = "merged"
	MergeAbandoned = "abandoned"
)

// Merge sources: who recorded a merge first.
const (
	MergeSourceUI     = "ui"
	MergeSourceCI     = "ci"
	MergeSourceGitHub = "github"
)

// MergeGates reports whether a done ticket in merge state ms still holds
// the tickets that depend on it: its PR waits to be merged, or was closed
// without merging and nobody has said it isn't needed.
func MergeGates(ms string) bool {
	return ms == MergeNeeded || ms == MergeAbandoned
}

// Approved reports whether a person approved the ticket as it's planned
// now: approved at or after its last planning (AMP #2198).
func (t *Task) Approved() bool {
	return t.ApprovedAt != nil && !t.ApprovedAt.Before(t.Planned())
}

// ApprovesAsOwn reports whether userID approving t approves their own work:
// they own it, or nobody owns it and they created it. Anything else is
// approving on someone's behalf (AMP #2198). A ticket with neither owner
// nor creator (legacy rows, local dev) is nobody's, so only the local-dev
// principal (user 0, no account) approves it as its own.
func (t *Task) ApprovesAsOwn(userID int) bool {
	who := t.OwnerUserID
	if who == nil {
		who = t.CreatedBy
	}
	if who == nil {
		return userID == 0
	}
	return *who == userID
}

// ApprovalOwner is whose approval the ticket needs: its owner, or for an
// unassigned ticket the person who planned it (nil when neither is set).
func (t *Task) ApprovalOwner() *int {
	if t.OwnerUserID != nil {
		return t.OwnerUserID
	}
	return t.CreatedBy
}

// DropsApprovalOn reports whether giving t to newOwner (nil: releasing it)
// drops its plan approval, when the person making the change isn't a
// project lead or above acting in the AMP web UI (byLead false; a lead
// over MCP or an API token counts as not a lead here, AMP #2223). An approval is for one person
// (ApprovedFor): it survives only while the ticket is that person's again
// afterwards, so a Member can't approve someone's ticket by claiming it,
// approving it as their own and handing it back, nor hand their approved
// ticket to someone who never approved it (AMP #2218). A lead's
// reassignment in the web UI keeps it: a lead may approve on anyone's
// behalf there anyway.
func (t *Task) DropsApprovalOn(newOwner *int, byLead bool) bool {
	if t.ApprovedAt == nil || byLead {
		return false
	}
	next := newOwner
	if next == nil {
		next = t.CreatedBy
	}
	if next == nil || t.ApprovedFor == nil {
		return !(next == nil && t.ApprovedFor == nil)
	}
	return *next != *t.ApprovedFor
}

// Planned is when the ticket was last planned (PlannedAt, or CreatedAt for
// a value built without it).
func (t *Task) Planned() time.Time {
	if t.PlannedAt.IsZero() {
		return t.CreatedAt
	}
	return t.PlannedAt
}

// IsHuman reports whether a person, not an agent, does this ticket.
func (t *Task) IsHuman() bool { return t.Performer == PerformerHuman }

type Comment struct {
	ID           int       `json:"id"`
	TaskID       int       `json:"task_id"`
	Body         string    `json:"body"`
	Author       string    `json:"author"`
	AuthorUserID *int      `json:"author_user_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ---- Request types ----

type CreateProjectRequest struct {
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
	// OrgID and TeamID choose where the project lives and which team owns
	// it. Both may be omitted when the caller's choice is unambiguous.
	OrgID     *int     `json:"org_id,omitempty"`
	TeamID    *int     `json:"team_id,omitempty"`
	CreatedBy *int     `json:"-"`
	Metadata  Metadata `json:"metadata,omitempty"`
}

type CreateEpicRequest struct {
	ProjectID   int      `json:"project_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	CreatedBy   *int     `json:"-"` // set server-side from the caller; also made an owner
	Metadata    Metadata `json:"metadata,omitempty"`
}

type CreateStoryRequest struct {
	ProjectID          int      `json:"project_id"`
	EpicID             int      `json:"epic_id"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Priority           string   `json:"priority"`
	CreatedBy          *int     `json:"-"`
	Metadata           Metadata `json:"metadata,omitempty"`
}

type CreateTaskRequest struct {
	ProjectID          int    `json:"project_id"`
	EpicID             int    `json:"epic_id"`  // required
	StoryID            int    `json:"story_id"` // required
	Name               string `json:"name"`
	Description        string `json:"description"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Priority           string `json:"priority"`
	// AssignedTo: set at planning time by the manager. Who should work this.
	AssignedTo string `json:"assigned_to,omitempty"`
	// DependencyIDs: task IDs that must complete before this one runs.
	// State (backlog vs blocked) is derived by the actor — agent does not set it.
	DependencyIDs []int      `json:"dependency_ids"`
	StartAt       *time.Time `json:"start_at,omitempty"`
	CreatedBy     *int       `json:"-"`
	// OwnerUserID is resolved server-side: the creator by default, or nil
	// for an unassigned ticket.
	OwnerUserID *int     `json:"-"`
	Metadata    Metadata `json:"metadata,omitempty"`
	// Performer: "agent" (default) or "human"; HumanReason with "human".
	Performer   string `json:"performer,omitempty"`
	HumanReason string `json:"human_reason,omitempty"`
}

type UpdateTaskRequest struct {
	TaskID             int        `json:"task_id"`
	Name               string     `json:"name,omitempty"`
	Description        string     `json:"description,omitempty"`
	AcceptanceCriteria string     `json:"acceptance_criteria,omitempty"`
	AssignedTo         string     `json:"assigned_to,omitempty"`
	AgentID            string     `json:"agent_id,omitempty"`
	Priority           string     `json:"priority,omitempty"`
	StartAt            *time.Time `json:"start_at,omitempty"`
	// Performer switches who does it ("agent" or "human"); not while it's in
	// progress. HumanReason: why a person does it.
	Performer   string `json:"performer,omitempty"`
	HumanReason string `json:"human_reason,omitempty"`
	// Metadata sets keys; null removes one (see MetadataPatch).
	Metadata MetadataPatch `json:"metadata,omitempty"`
	// Clear names fields to blank (see TaskClearable); "" otherwise means keep.
	Clear []string `json:"clear,omitempty"`
}

type UpdateEpicRequest struct {
	EpicID      int    `json:"epic_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty"`
	// Metadata sets keys; null removes one (see MetadataPatch).
	Metadata MetadataPatch `json:"metadata,omitempty"`
	// Clear names fields to blank (see EpicClearable).
	Clear []string `json:"clear,omitempty"`
}

type UpdateStoryRequest struct {
	StoryID            int    `json:"story_id"`
	Name               string `json:"name,omitempty"`
	Description        string `json:"description,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	Priority           string `json:"priority,omitempty"`
	// Metadata sets keys; null removes one (see MetadataPatch).
	Metadata MetadataPatch `json:"metadata,omitempty"`
	// Clear names fields to blank (see StoryClearable).
	Clear []string `json:"clear,omitempty"`
}

type DispatchTaskRequest struct {
	TaskID  int    `json:"task_id"`
	AgentID string `json:"agent_id"`
}

type AddCommentRequest struct {
	TaskID       int    `json:"task_id"`
	Body         string `json:"body"`
	Author       string `json:"author"`
	AuthorUserID *int   `json:"-"`
}

// ActivityLog is the immutable audit trail for a task.
// Every state transition and comment produces an entry.
// This is what amp_get_ticket_history returns — the full story of a ticket.
type ActivityLog struct {
	ID        int    `json:"id"`
	TaskID    int    `json:"task_id"`
	ProjectID int    `json:"project_id"`
	Actor     string `json:"actor"`  // "manager", "amp-worker", "system", agent ID
	Action    string `json:"action"` // "created", "dispatched", "completed", "blocked", "comment", "state_change"
	FromState string `json:"from_state,omitempty"`
	ToState   string `json:"to_state,omitempty"`
	Detail    string `json:"detail,omitempty"` // human-readable summary or comment body
	// ActorUserID is the AMP user behind the action, when there was one.
	ActorUserID *int      `json:"actor_user_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ---- Events (broadcast to SSE clients) ----

type EventType string

const (
	EventProjectCreated    EventType = "project.created"
	EventProjectArchived   EventType = "project.archived"
	EventProjectRestored   EventType = "project.restored"
	EventProjectUpdated    EventType = "project.updated"
	EventEpicCreated       EventType = "epic.created"
	EventEpicStateChanged  EventType = "epic.state_changed"
	EventStoryCreated      EventType = "story.created"
	EventStoryStateChanged EventType = "story.state_changed"
	EventTaskCreated       EventType = "task.created"
	EventTaskUpdated       EventType = "task.updated"
	EventTaskDispatched    EventType = "task.dispatched"
	EventTaskCompleted     EventType = "task.completed"
	EventTaskBlocked       EventType = "task.blocked"
	EventTaskUnblocked     EventType = "task.unblocked"
	EventCommentAdded      EventType = "comment.added"
	EventTaskAssigned      EventType = "task.assigned"
	EventEpicUpdated       EventType = "epic.updated"
	EventStoryUpdated      EventType = "story.updated"
	EventArtifactUpdated   EventType = "artifact.updated"
)

// AllEventTypes lists every event the hub publishes.
var AllEventTypes = []EventType{
	EventProjectCreated, EventProjectArchived, EventProjectRestored, EventProjectUpdated,
	EventEpicCreated, EventEpicStateChanged, EventEpicUpdated,
	EventStoryCreated, EventStoryStateChanged, EventStoryUpdated,
	EventTaskCreated, EventTaskUpdated, EventTaskDispatched, EventTaskCompleted,
	EventTaskBlocked, EventTaskUnblocked, EventTaskAssigned, EventCommentAdded,
	EventArtifactUpdated,
}

type Event struct {
	Type      EventType   `json:"type"`
	ProjectID int         `json:"project_id"`
	Payload   interface{} `json:"payload"`
	At        time.Time   `json:"at"`
}

// ---- Export types ----

// ExportBundle captures everything needed to fully reconstruct a project.
// It is the source of truth for the import flow and must be self-contained.
type ExportBundle struct {
	Version    string        `json:"version"` // "1"
	ExportedAt time.Time     `json:"exported_at"`
	Project    Project       `json:"project"`
	Epics      []Epic        `json:"epics"`
	Stories    []Story       `json:"stories"`
	Tasks      []ExportTask  `json:"tasks"`
	KBDocs     []ExportKBDoc `json:"kb_docs"`
}

// ExportTask wraps a Task with its dependency IDs for export.
// DependencyIDs use the ORIGINAL task IDs — the import step will remap them.
type ExportTask struct {
	Task
	DependencyIDs []int `json:"dependency_ids"`
}

// ExportKBDoc represents a knowledge base document for export.
type ExportKBDoc struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Content     string         `json:"content"`
	Tags        []string       `json:"tags"`
	Author      string         `json:"author"`
	Annotations []KBAnnotation `json:"annotations,omitempty"`
	// Reason, CreatedAt and UpdatedAt (unix seconds) were added with the
	// Postgres KB store. Exports made before then lack them; an import treats
	// a zero date as "now".
	Reason    string `json:"reason,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// KBAnnotation is a note left on a KB doc (kb.Annotation).
type KBAnnotation struct {
	Author     string `json:"author"`
	Text       string `json:"text"`
	CreatedAt  int64  `json:"created_at"`
	IsResolved bool   `json:"is_resolved"`
	// Who resolved it and when, once the note has been worked into the doc.
	ResolvedBy string `json:"resolved_by,omitempty"`
	ResolvedAt int64  `json:"resolved_at,omitempty"`
}

// ---- Auth / users ----

// User is a JIT-provisioned identity, keyed off the Dex OIDC "sub" claim.
// Dex remains the source of truth for credentials — this row exists purely
// so amp-api can attribute activity and assign roles.
type User struct {
	ID          int       `json:"id"`
	Subject     string    `json:"subject"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Roles       []string  `json:"roles"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	// Kind is UserKindHuman (signs in through Dex) or UserKindAgent (an
	// account for an autonomous agent, which uses API tokens).
	Kind string `json:"kind"`
	// ManagedBy is the person responsible for an agent account.
	ManagedBy *int `json:"managed_by,omitempty"`
	// DisabledAt is set while the account is deactivated: every request
	// it makes is refused, and its work and history stay as they are.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

// AuditEntry is one row of the audit log: a change to access, settings,
// or a deletion, and who made it.
type AuditEntry struct {
	ID          int64          `json:"id"`
	At          time.Time      `json:"at"`
	ActorUserID *int           `json:"actor_user_id,omitempty"`
	ActorEmail  string         `json:"actor_email"`
	ViaToken    bool           `json:"via_token,omitempty"` // made with an API token
	OrgID       *int           `json:"org_id,omitempty"`
	ProjectID   *int           `json:"project_id,omitempty"`
	Action      string         `json:"action"` // e.g. "org.member.set", "task.delete"
	TargetType  string         `json:"target_type,omitempty"`
	TargetID    string         `json:"target_id,omitempty"`
	Summary     string         `json:"summary"`
	Details     map[string]any `json:"details,omitempty"`

	// Audit log v2 (KB decisions/audit-log-v2.md). Every field is new and
	// omitempty, so consumers of the v1 shape see only additions. Rows
	// written before v2 read SchemaVersion 1 with these empty.
	SchemaVersion    int            `json:"schema_version,omitempty"`
	ActorType        string         `json:"actor_type,omitempty"`  // AuditActor*
	AuthMethod       string         `json:"auth_method,omitempty"` // dex_session, oauth_mcp, api_token, cluster, local_dev, none
	TokenID          *int64         `json:"token_id,omitempty"`    // api_tokens.id, never the token
	SessionID        string         `json:"session_id,omitempty"`
	OnBehalfOfUserID *int           `json:"on_behalf_of_user_id,omitempty"`
	AgentProfile     string         `json:"agent_profile,omitempty"`
	TargetName       string         `json:"target_name,omitempty"`
	Outcome          string         `json:"outcome,omitempty"` // AuditOutcome*
	Reason           string         `json:"reason,omitempty"`
	SourceIP         string         `json:"source_ip,omitempty"`
	UserAgent        string         `json:"user_agent,omitempty"`
	Client           string         `json:"client,omitempty"` // AuditClient*
	RequestID        string         `json:"request_id,omitempty"`
	TraceID          string         `json:"trace_id,omitempty"`
	Before           map[string]any `json:"before,omitempty"`
	After            map[string]any `json:"after,omitempty"`

	// Tamper evidence (#2429): the row's place in its chain ("org:<id>" or
	// "global") and its hash, hex. A copy kept elsewhere (a SIEM via
	// webhooks) can later prove what the row said. Empty on an entry not
	// yet read back from the database.
	ChainKey string `json:"chain_key,omitempty"`
	ChainSeq int64  `json:"chain_seq,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

// AuditSchemaVersion is what writers put in AuditEntry.SchemaVersion.
const AuditSchemaVersion = 2

// Who made an audited change (AuditEntry.ActorType).
const (
	AuditActorHuman     = "human"
	AuditActorAgent     = "agent"
	AuditActorSystem    = "system"
	AuditActorAnonymous = "anonymous"
)

// The result of an audited action (AuditEntry.Outcome).
const (
	AuditOutcomeSuccess = "success"
	AuditOutcomeDenied  = "denied"
	AuditOutcomeFailed  = "failed"
)

// The surface an audited change came through (AuditEntry.Client).
const (
	AuditClientWeb     = "web"
	AuditClientMCP     = "mcp"
	AuditClientHarness = "harness"
	AuditClientAPI     = "api"
	AuditClientRelay   = "relay"
	AuditClientCLI     = "cli"
	AuditClientSystem  = "system"
)

// AuditFilter selects audit entries; zero fields don't filter.
type AuditFilter struct {
	OrgID       int // entries for the org or any of its projects
	ProjectID   int
	ActorUserID int
	Action      string // exact action, or a prefix ending in "." (e.g. "org.")
	BeforeID    int64  // page backwards from here
	Limit       int

	// Audit log v2 filters (#2431).
	Actor      string     // actor email, case-insensitive substring
	ActorType  string     // AuditActor*
	TargetType string     // exact
	TargetID   string     // exact
	IP         string     // an address or a CIDR prefix (source_ip <<= IP)
	Outcome    string     // AuditOutcome*
	Client     string     // AuditClient*
	From       *time.Time // at >= From
	To         *time.Time // at < To
}

// Values is the filter as it was asked for, without the paging fields:
// what an audit.export row records.
func (f AuditFilter) Values() map[string]any {
	out := map[string]any{}
	set := func(k string, v any, zero bool) {
		if !zero {
			out[k] = v
		}
	}
	set("org_id", f.OrgID, f.OrgID == 0)
	set("project_id", f.ProjectID, f.ProjectID == 0)
	set("actor_user_id", f.ActorUserID, f.ActorUserID == 0)
	set("action", f.Action, f.Action == "")
	set("actor", f.Actor, f.Actor == "")
	set("actor_type", f.ActorType, f.ActorType == "")
	set("target_type", f.TargetType, f.TargetType == "")
	set("target_id", f.TargetID, f.TargetID == "")
	set("ip", f.IP, f.IP == "")
	set("outcome", f.Outcome, f.Outcome == "")
	set("client", f.Client, f.Client == "")
	if f.From != nil {
		out["from"] = f.From.UTC().Format(time.RFC3339)
	}
	if f.To != nil {
		out["to"] = f.To.UTC().Format(time.RFC3339)
	}
	return out
}

// IsAgent reports whether u is an agent account.
func (u *User) IsAgent() bool { return u != nil && u.Kind == UserKindAgent }

const (
	UserKindHuman = "human"
	UserKindAgent = "agent"
)

// APIToken is a long-lived credential for MCP (and API) access, for agent
// accounts and for people running headless agents. Only a hash is stored;
// the token itself is shown once, when it is created.
type APIToken struct {
	ID         int        `json:"id"`
	UserID     int        `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"` // first characters, to recognise it
	CreatedBy  *int       `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// ---- Orgs, teams, access ----

const (
	OrgRoleOwner  = "owner"
	OrgRoleAdmin  = "admin"
	OrgRoleMember = "member"

	TeamRoleLead   = "lead"
	TeamRoleMember = "member"

	ProjectTeamOwner  = "owner"
	ProjectTeamMember = "member"
)

type Org struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedBy *int      `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// MyRole is the caller's role in the org ("" when not a member, e.g. a
	// global admin browsing). Filled by list endpoints only.
	MyRole string `json:"my_role,omitempty"`
}

type Team struct {
	ID        int       `json:"id"`
	OrgID     int       `json:"org_id"`
	Name      string    `json:"name"`
	CreatedBy *int      `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Member is a user's membership in an org or team.
type Member struct {
	UserID      int    `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Kind        string `json:"kind,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

// ProjectTeam is a team's link to a project.
type ProjectTeam struct {
	ProjectID int    `json:"project_id"`
	TeamID    int    `json:"team_id"`
	TeamName  string `json:"team_name"`
	Role      string `json:"role"`
}

// ProjectMember is a user with access to a project, and their effective
// access level there (see authz.Level).
type ProjectMember struct {
	UserID      int    `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Access      string `json:"access"`
	Kind        string `json:"kind,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

type Invite struct {
	ID         int        `json:"id"`
	Email      string     `json:"email"`
	OrgID      int        `json:"org_id"`
	OrgRole    string     `json:"org_role"`
	TeamID     *int       `json:"team_id,omitempty"`
	TeamRole   string     `json:"team_role"`
	InvitedBy  *int       `json:"invited_by"`
	CreatedAt  time.Time  `json:"created_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
}

// HasRole reports whether the user holds a global role.
func (u *User) HasRole(role string) bool {
	if u == nil {
		return false
	}
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}
