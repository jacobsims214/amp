export type TaskState = 'backlog' | 'in_progress' | 'completed' | 'blocked' | 'scheduled'
export type Priority = '0' | '1' | '2' | '3'

export interface Project {
  id: number
  name: string
  code: string
  description: string
  state: 'active' | 'archived'
  org_id?: number | null
  created_by?: number | null
  /** any project member may reassign tickets that aren't in progress */
  open_reassign?: boolean
  /** free-form key/value data for integrations (its own, not inherited) */
  metadata?: Metadata
  created_at: string
  updated_at: string
  plan_approved_at?: string
  plan_approved_by?: number
  require_plan_approval?: boolean
}

export interface Epic {
  id: number
  project_id: number
  name: string
  description: string
  state: string
  priority: Priority
  created_by?: number | null
  owner_ids?: number[]
  /** free-form key/value data for integrations (its own, not inherited) */
  metadata?: Metadata
  created_at: string
  updated_at: string
}

export interface Story {
  id: number
  project_id: number
  epic_id: number
  name: string
  description: string
  acceptance_criteria: string
  state: string
  priority: Priority
  created_by?: number | null
  owner_ids?: number[]
  /** free-form key/value data for integrations (its own, not inherited) */
  metadata?: Metadata
  created_at: string
  updated_at: string
}

export interface Task {
  id: number
  project_id: number
  epic_id: number
  story_id: number
  name: string
  description: string
  acceptance_criteria: string
  state: TaskState
  priority: Priority
  dependency_ids: number[]
  blocked_by_ids?: number[]
  assigned_to?: string    // AGENT profile, set at plan time by manager — which agent should work this
  agent_id?: string       // AGENT, set at dispatch time — which agent is actually working it
  dispatched_at?: string
  completed_at?: string
  block_reason?: string
  start_at?: string       // RFC3339 UTC timestamp for scheduled tasks
  created_by?: number | null     // AMP user who created the ticket (null for legacy rows)
  owner_user_id?: number | null  // the one human owner; null = unassigned ("pool")
  /** who does the work: an agent (dispatched), or a person (its owner, in AMP) */
  performer?: 'agent' | 'human'
  /** why a person does it (see HUMAN_REASONS) */
  human_reason?: HumanReason
  /** when it last became ready: ready → started is time waiting to be picked up */
  ready_at?: string
  /** when its scope (name, description, acceptance criteria) was last set; newer than the plan's approval = awaiting approval */
  planned_at?: string
  /** when a person last approved its plan (#2198); approved iff approved_at >= planned_at */
  approved_at?: string
  /** who approved it (null with approved_at set: a migration kept an earlier approval) */
  approved_by?: number | null
  /** free-form key/value data for integrations (its own, not inherited) */
  metadata?: Metadata
  created_at: string
  updated_at: string
}

export type HumanReason = 'review' | 'approval' | 'access' | 'judgment' | 'external' | 'physical' | 'other'

export interface Comment {
  id: number
  task_id: number
  body: string
  author: string
  created_at: string
}

export interface ActivityLog {
  id: number
  task_id: number
  project_id: number
  actor: string
  action: string
  from_state?: string
  to_state?: string
  detail?: string
  /** the AMP user behind the action, when there was one */
  actor_user_id?: number
  created_at: string
}

// ---- Plan approval (POST /projects/:id/approve-plan, #2198) ----

/** What a person approves: see api.approvePlan. */
export interface ApproveRequest {
  taskIds?: number[]
  asOf?: string
  onBehalf?: boolean
}

/** One ticket still waiting, and whether it's the caller's own. */
export interface AwaitingTicket {
  task_id: number
  owner_user_id?: number | null
  yours: boolean
}

/** The server's answer (service.ApproveResult); older servers send only project and awaiting_approval. */
export interface ApproveResponse {
  project: Project
  approved?: number[]
  approved_tickets?: Array<{ task_id: number; owner_user_id?: number | null; on_behalf: boolean }>
  awaiting_approval?: number[]
  still_awaiting?: AwaitingTicket[]
  awaiting_yours?: number[]
  /** ticket id -> why it wasn't approved ("it was planned after what was approved (as_of)") */
  skipped?: Record<string, string>
  on_behalf?: boolean
}

/** ApproveResponse with defaults filled in. */
export interface ApproveResult {
  project: Project
  approved: number[]
  awaitingApproval: number[]
  awaitingYours: number[]
  skipped: Record<string, string>
  onBehalf: boolean
}

// ---- Plan check (GET /projects/:id/plan-check) ----

export interface PlanIssue {
  kind: 'isolated' | 'wide_start' | 'empty_stories' | 'no_context' | string
  message: string
  task_ids?: number[]
  fix?: string
  /** 'warn' for advice that doesn't make the plan NO-GO */
  severity?: 'warn'
}

export interface PlanReport {
  ok: boolean
  /** open tickets planned since the plan was last approved */
  awaiting_approval?: number[]
  /** the Flight plan page where a person approves, when tickets await approval */
  approve_url?: string
  plan_approved_at?: string
  plan_approved_by?: number
  require_plan_approval?: boolean
  tasks: number
  open_tasks: number
  dependencies: number
  ready_now: number[]
  longest_chain: number
  issues: PlanIssue[]
  repaired_task_ids?: number[]
  summary: string
}

// ---- Metadata ----

export type MetadataValue = string | number | boolean
/** Free-form key/value data on a project, epic, story, or ticket. */
export type Metadata = Record<string, MetadataValue>
/** A change to metadata: a value sets a key, null removes it. */
export type MetadataPatch = Record<string, MetadataValue | null>

// ---- Daily reports ----

export interface DayReportItem {
  task_id: number
  ref: string
  name: string
  state: string
  project_id: number
  project_code: string
  project_name: string
  epic_id: number
  epic_name: string
  story_id: number
  story_name: string
  block_reason?: string
  completed_at?: string
  cycle_time_hours?: number
  metadata: Metadata
}

export interface DayReportData {
  completed: DayReportItem[]
  started: DayReportItem[]
  created: DayReportItem[]
  in_progress: DayReportItem[]
  blocked: DayReportItem[]
  touched: DayReportItem[]
  stats: {
    completed: number
    started: number
    created: number
    in_progress: number
    blocked: number
    projects: number
    avg_cycle_hours?: number
  }
  generated_at: string
}

export interface DayReport {
  user_id: number
  user_email: string
  user_name: string
  day: string
  tz: string
  summary: string
  data: DayReportData
  saved: boolean
  created_at?: string
  updated_at?: string
  markdown: string
  hidden_items?: number
}

export interface VelocityDay {
  day: string
  completed: number
  by_project: Record<string, number>
}

// ---- KB types ----

export interface KBAnnotation {
  author: string
  text: string
  created_at: number
  is_resolved: boolean
  /** who resolved it and when (unix seconds), once worked into the doc */
  resolved_by?: string
  resolved_at?: number
}

/** Names one knowledge base: set exactly one id. */
export interface KBRef {
  project_id?: number
  team_id?: number
  org_id?: number
  /** The AMP guide: read-only docs that ship with AMP. */
  global?: boolean
}

export type KBScope = 'project' | 'team' | 'org' | 'global'

/** A knowledge base and what the viewer may do in it. */
export interface KBInfo {
  scope: KBScope
  scope_id: number
  name: string
  /** read, search, annotate */
  can_read: boolean
  /** write, delete */
  can_write: boolean
}

/** A KB around a project (GET /kb/sources). Teams you're not on have can_read false. */
export interface KBSource extends KBInfo {
  team_role?: 'owner' | 'member'
  docs?: number
}

export interface KBDoc {
  id: string
  project_id: number
  scope?: KBScope
  scope_id?: number
  /** why a team or org doc was placed there */
  reason?: string
  path: string
  title: string
  content: string
  tags: string[]
  author: string
  chunk_index: number
  chunk_text: string
  updated_at: number
  annotations?: KBAnnotation[]
}

export interface KBDocSummary {
  id: string
  project_id: number
  scope?: KBScope
  scope_id?: number
  reason?: string
  path: string
  title: string
  tags: string[]
  author: string
  updated_at: number
}

export interface KBSearchResult {
  path: string
  title: string
  tags: string[]
  excerpt: string
  author: string
  updated_at: number
  score: number
  annotation_count?: number
  latest_annotation?: string
}

export interface KBTagCount {
  tag: string
  count: number
}

export interface SSEEvent {
  type:
    | 'connected'
    | 'project.created'
    | 'project.archived'
    | 'project.restored'
    | 'project.updated'
    | 'epic.created'
    | 'epic.state_changed'
    | 'story.created'
    | 'story.state_changed'
    | 'task.created'
    | 'task.updated'
    | 'task.dispatched'
    | 'task.completed'
    | 'task.blocked'
    | 'task.unblocked'
    | 'comment.added'
    | 'task.assigned'
    | 'epic.updated'
    | 'story.updated'
    | 'artifact.updated'
  project_id?: number
  payload?: unknown
  at?: string
}

// ---- CRUD request types ----

export interface CreateEpicRequest {
  name: string
  description?: string
  priority?: string
  metadata?: Metadata
}

export interface UpdateEpicRequest {
  name?: string
  description?: string
  priority?: string
  metadata?: MetadataPatch
  /** Fields to blank; an empty value otherwise means "keep". */
  clear?: string[]
}

export interface CreateStoryRequest {
  name: string
  description?: string
  acceptance_criteria?: string
  priority?: string
  metadata?: Metadata
}

export interface UpdateStoryRequest {
  name?: string
  description?: string
  acceptance_criteria?: string
  priority?: string
  metadata?: MetadataPatch
  /** Fields to blank; an empty value otherwise means "keep". */
  clear?: string[]
}

export interface CreateTaskRequest {
  epic_id: number
  story_id: number
  name: string
  description?: string
  acceptance_criteria?: string
  assigned_to?: string
  priority?: string
  dependency_ids?: number[]
  start_at?: string
  /** omit = me; "none" = unassigned; or a project member's email / user id */
  owner?: string
  metadata?: Metadata
  performer?: 'agent' | 'human'
  human_reason?: HumanReason
}

export interface UpdateTaskRequest {
  name?: string
  description?: string
  acceptance_criteria?: string
  assigned_to?: string
  priority?: string
  start_at?: string | null
  metadata?: MetadataPatch
  /** Fields to blank; an empty value otherwise means "keep". */
  clear?: string[]
}

// ---- Auth / users ----

export interface Me {
  subject: string
  email: string
  name: string
  roles: string[]
  user_id?: number
  orgs?: Org[]
  teams?: { id: number; name: string; org_id: number; role: string }[]
  kind?: UserKind
  via_token?: boolean
  /** Feature flags for this caller. Absent on older APIs (treat as unavailable). */
  capabilities?: {
    remote_control?: { available: boolean; org_ids?: number[]; reason?: string | null }
  }
}

export type UserKind = 'human' | 'agent'

export interface APIToken {
  id: number
  user_id: number
  name: string
  prefix: string
  created_by?: number
  created_at: string
  expires_at?: string
  last_used_at?: string
  revoked_at?: string
}

/** A token as just created: the only time `token` is shown. */
export interface NewAPIToken extends APIToken {
  token: string
}

export interface AuditEntry {
  id: number
  at: string
  actor_user_id?: number
  actor_email: string
  via_token?: boolean
  org_id?: number
  project_id?: number
  action: string
  target_type?: string
  target_id?: string
  summary: string
  details?: Record<string, unknown>
}

export type WebhookFormat = 'json' | 'slack'

export interface Webhook {
  id: number
  org_id?: number
  project_id?: number
  name: string
  url: string
  format: WebhookFormat
  /** Only present right after create or rotate. */
  secret?: string
  events: string[]
  active: boolean
  created_by?: number
  created_at: string
  updated_at: string
  last_delivery_at?: string
  last_status?: number
  last_error?: string
  consecutive_failures: number
}

export interface WebhookChange {
  name?: string
  url?: string
  format?: WebhookFormat
  events?: string[]
  active?: boolean
}

export interface WebhookDelivery {
  id: number
  webhook_id: number
  event_type: string
  body?: string
  status: 'pending' | 'delivered' | 'failed'
  attempts: number
  next_attempt_at: string
  last_status?: number
  last_error?: string
  created_at: string
  delivered_at?: string
}

export interface AmpUser {
  id: number
  subject: string
  email: string
  display_name: string
  roles: string[]
  created_at: string
  last_seen_at: string
  kind: UserKind
  managed_by?: number
  disabled_at?: string
}

export interface AuthAdminUser {
  email: string
  username: string
}


// ---- Orgs / teams / access ----

export type OrgRole = 'owner' | 'admin' | 'member'
export type TeamRole = 'lead' | 'member'
export type ProjectTeamRole = 'owner' | 'member'
export type ProjectAccess = 'member' | 'lead' | 'owner' | 'org_admin' | 'global_admin'

export interface Org {
  id: number
  name: string
  slug: string
  created_by: number | null
  created_at: string
  /** caller's role in the org; "" when not a member (e.g. a global admin browsing) */
  my_role?: OrgRole | ''
}

export interface Team {
  id: number
  org_id: number
  name: string
  created_by: number | null
  created_at: string
}

/** A user's membership in an org (role: OrgRole) or team (role: TeamRole). */
export interface Member {
  user_id: number
  email: string
  display_name: string
  role: string
  kind?: UserKind
  disabled?: boolean
}

export interface ProjectMember {
  user_id: number
  email: string
  display_name: string
  access: ProjectAccess
  kind?: UserKind
  disabled?: boolean
}

export interface ProjectTeam {
  project_id: number
  team_id: number
  team_name: string
  role: ProjectTeamRole
}

export interface Invite {
  id: number
  email: string
  org_id: number
  org_role: string
  team_id?: number | null
  team_role: string
  invited_by: number | null
  created_at: string
  accepted_at?: string | null
}

/** Result of adding a member by email/id: either added directly or invited. */
export interface AddMemberResult {
  invited?: boolean
  invite?: Invite
  added?: boolean
  user_id?: number
  role?: string
}

export interface CreateProjectRequest {
  name: string
  code: string
  description?: string
  org_id?: number
  team_id?: number
}

// ---- Home page (GET /my-work) ----

/** A ticket with its project, for views that span projects. */
export type WorkTask = Task & {
  project_code: string
  project_name: string
  /** latest activity on the ticket (state change, comment, dispatch) */
  last_activity_at?: string
}

export interface MyAgent {
  user: AmpUser
  working: WorkTask[]
  done_24h: number
}

export interface MyWork {
  owned: WorkTask[]
  flags: WorkTask[]
  agents: MyAgent[]
  done_24h: number
}

// ---- Project report (GET /projects/:id/report) ----

export interface ReportStates {
  ready: number; in_progress: number; waiting: number; flagged: number; scheduled: number; done: number
}

export interface ReportGroup {
  key: string
  label?: string
  tickets: number
  states: ReportStates
  completed_in_range: number
  median_cycle_hours?: number
  /** token spend of runs recorded in range on this group's tickets */
  usage?: Usage
}

/** Token spend totalled over runs. */
export interface Usage {
  runs: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost_usd: number
  /** runs that reported no token counts */
  unreported: number
  /** runs that died or were stopped (rework) */
  abandoned?: number
  models?: string[]
}

/** One agent run on a ticket: what it used, and where its work went. */
export interface Run {
  id: number
  task_id: number
  project_id: number
  user_id?: number
  agent_id?: string
  outcome: 'completed' | 'blocked' | 'progress' | 'abandoned'
  model?: string
  input_tokens?: number
  output_tokens?: number
  cache_read_tokens?: number
  cache_write_tokens?: number
  cost_usd?: number
  branch?: string
  commit?: string
  pr_url?: string
  pushed?: boolean
  merged?: boolean
  note?: string
  /** a person's reported time on a human ticket */
  hours_worked?: number
  created_at: string
}

/** A ticket a commit, branch, or PR leads back to. */
export interface TraceHit {
  ref: string
  task: Task
  project: string
  runs: Run[]
}

/** An epic (with its stories) or a story in a project's trash. */
export interface DeletedGroup {
  kind: 'epic' | 'story'
  id: number
  project_id: number
  epic_id: number
  name: string
  task_ids: number[]
  stories: number
  deleted_by?: number
  deleted_at: string
  /** A story can only come back while its epic exists (or comes back with it). */
  epic_exists: boolean
}

export interface ProjectTrash {
  tickets: DeletedTask[]
  groups: DeletedGroup[]
}

export interface GroupRestoreResult {
  kind: 'epic' | 'story'
  id: number
  restored: boolean
  error?: string
  tickets?: { task_id: number; restored: boolean; error?: string }[]
}

/** A ticket in a project's trash. */
export interface DeletedTask {
  task_id: number
  project_id: number
  epic_id: number
  story_id: number
  name: string
  deleted_by?: number
  deleted_at: string
  story_exists: boolean
}

export interface ProjectReport {
  since: string
  until: string
  tickets: number
  states: ReportStates
  completed: number
  dispatched: number
  created: number
  median_cycle_hours?: number
  flag_hours: number
  agents: ReportGroup[]
  epics: ReportGroup[]
  metadata_keys: { key: string; coverage: number }[]
  group_by?: string
  breakdown?: ReportGroup[]
  /** by owner (key: user id, "" unowned) */
  people?: ReportGroup[]
  usage?: Usage
  usage_by_model?: (Usage & { model: string })[]
  /** how the work splits between agents and people */
  work?: WorkReport
}

/** One kind of performer's tickets: wait = ready → started, cycle = started → done. */
export interface PerformerStats {
  tickets: number
  open: number
  completed_in_range: number
  median_wait_hours?: number
  median_cycle_hours?: number
  hours_worked?: number
}

export interface HumanTicketRow {
  id: number
  name: string
  reason: HumanReason
  owner_user_id: number | null
  state: TaskState
  wait_hours?: number
  cycle_hours?: number
  hours_worked?: number
  elapsed_hours: number
  holding_up: number
  downstream: number
}

export interface WorkReport {
  agent: PerformerStats
  human: PerformerStats
  human_by_reason: (PerformerStats & { reason: HumanReason })[]
  slowest_human_tickets: HumanTicketRow[]
}

/** One project in an org's portfolio. */
export interface PortfolioRow {
  project_id: number
  name: string
  code: string
  state: string
  metadata?: Metadata
  tickets: number
  states: ReportStates
  completed: number
  usage: Usage
  /** spend and tickets by value of the split key ("(not set)" for none) */
  split: Record<string, { tickets: number; usage: Usage }>
}

export interface Portfolio {
  org_id: number
  since: string
  until: string
  split_by?: string
  split_values: string[]
  /** metadata keys set on projects (columns) */
  project_keys: string[]
  /** metadata keys on tickets, with coverage (split candidates) */
  ticket_keys: { key: string; coverage: number }[]
  projects: PortfolioRow[]
  total: PortfolioRow
  usage_by_model: (Usage & { model: string })[]
}

// ---- Artifacts: mockups, diagrams, images and documents agents publish for review ----

export type ArtifactKind = 'html' | 'svg' | 'image' | 'markdown' | 'pdf' | 'text'
export type ArtifactStatus = 'in_review' | 'approved' | 'changes_requested'

export interface Artifact {
  id: number
  project_id: number
  task_id: number | null
  task_ref?: string
  task_name?: string
  title: string
  kind: ArtifactKind
  status: ArtifactStatus
  current_version: number
  approved_version: number | null
  reviewed_by: number | null
  reviewed_by_email?: string
  reviewed_at: string | null
  created_by: number | null
  created_by_email?: string
  created_at: string
  updated_at: string
  /** Kept by a person: never expires. Otherwise deleted at expires_at. */
  keep: boolean
  kept_by: number | null
  kept_at: string | null
  expires_at: string | null
  expiry_dismissed_at: string | null
  url?: string
}

export interface ArtifactVersion {
  artifact_id: number
  version: number
  content_type: string
  size_bytes: number
  sha256: string
  note: string
  created_by: number | null
  created_by_email?: string
  created_at: string
}
