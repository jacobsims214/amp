export type TaskState = 'backlog' | 'in_progress' | 'completed' | 'blocked' | 'scheduled'
export type Priority = '0' | '1' | '2' | '3'

export interface Project {
  id: number
  name: string
  code: string
  description: string
  state: 'active' | 'archived'
  org_id?: number | null
  /** set on project lists: the org's name, even when the caller isn't a member */
  org_name?: string
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
  /** done tickets with an unmerged PR hold their dependents until the merge is recorded (default on) */
  require_merge?: boolean
  require_merge_since?: string
  /** a dispatched ticket no worker starts is flagged after this many minutes; 0 = off (AMP #2801) */
  not_started_flag_minutes?: number
  /** a dispatched ticket no worker starts is returned to ready after this many minutes; 0 = off (AMP #2801) */
  not_started_return_minutes?: number
  /** verification receipts (#126): '' uses the org's default */
  receipts_mode?: '' | ReceiptsMode
  /** what a Require ticket must reach before it completes */
  receipts_min_level?: ReceiptMinLevel
  /** a person's ticket this many minutes past its decide_by is escalated once; 0 = off, the default (#146) */
  decision_escalation_minutes?: number
  /** the escalation also raises the ticket's priority one step (#146) */
  decision_escalation_raise_priority?: boolean
  /** the escalation hands the ticket to this user; null = nobody (#146) */
  decision_backup_decider?: number | null
}

// ---- Verification receipts (#126) ----

export type ReceiptsMode = 'off' | 'inform' | 'require'
export type ReceiptMinLevel = 'self_checked' | 'independently_verified'
export type VerificationStatus = 'unverified' | 'self_checked' | 'independently_verified' | 'human_accepted'

export interface Receipt {
  id: number
  task_id: number
  project_id: number
  run_id?: number
  state: 'draft' | 'final'
  mode: 'inform' | 'require'
  held: boolean
  agent_id: string
  summary: string
  files_changed: { path: string; added: number; removed: number }[]
  commit_sha: string
  tree_sha: string
  branch: string
  base_branch: string
  pr_url: string
  kept: boolean
  created_at: string
  finalized_at?: string
}

export interface ReceiptCheck {
  id: number
  receipt_id: number
  role: 'worker' | 'verifier' | 'human'
  kind: 'command' | 'test' | 'ci' | 'visual' | 'manual' | 'verdict'
  verdict: 'pass' | 'fail' | 'cant_tell'
  criterion_ref: string
  command?: string
  exit_code?: number
  duration_ms?: number
  note?: string
  evidence_key?: string
  checked_by: string
  user_id?: number
  created_at: string
}

export interface ReceiptEvidence {
  id: number
  receipt_id: number
  key: string
  name: string
  /** the revision it was recorded on */
  tree_sha?: string
  content_type: string
  bytes: number
  sha256?: string
  duration_ms?: number
  frames?: number
  status: 'pending' | 'stored' | 'expired'
  created_at: string
  stored_at?: string
  /** when unkept evidence is (or was) deleted; unset once kept (merged or accepted) */
  expires_at?: string
}

export interface ReceiptSettings {
  mode: ReceiptsMode
  min_level: ReceiptMinLevel
  /** the project's own value ('' = the org's) */
  project_mode: '' | ReceiptsMode
  org_mode: ReceiptsMode
}

export interface ReceiptView {
  receipt: Receipt
  checks: ReceiptCheck[]
  evidence: ReceiptEvidence[]
  status: VerificationStatus
  held: boolean
  /** why a held receipt is below its minimum */
  hold_reason?: string
}

/** GET /api/tasks/:id/receipt: the latest final receipt, or receipt: null. */
export type TaskReceipt =
  | (ReceiptView & { attempts: number; settings: ReceiptSettings })
  | { receipt: null; mode: ReceiptsMode; settings: ReceiptSettings }

export interface ReceiptSummaryRow {
  task_id: number
  status: VerificationStatus
  held: boolean
  has_media: boolean
  /** why a held one waits for a person */
  reason?: string
}

export interface ReceiptSummary extends ReceiptSettings {
  tasks: ReceiptSummaryRow[]
  would_hold_last_7_days: number
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

/**
 * One answer a person can choose (the human-decisions spec's AskOption, #125;
 * agent questions reuse it, #130). consequence is '' when the agent gave none.
 */
export interface AskOption {
  label: string
  consequence: string
}

/** An agent question's options (#130): the question itself is the ticket's block_reason. */
export interface TaskQuestion {
  options: AskOption[]
  /** index into options; only marks a badge, never chooses */
  recommended?: number | null
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
  /** the options an agent gave with its question (#130); absent for a question in words only */
  question?: TaskQuestion | null
  start_at?: string       // RFC3339 UTC timestamp for scheduled tasks
  created_by?: number | null     // AMP user who created the ticket (null for legacy rows)
  owner_user_id?: number | null  // the one human owner; null = unassigned ("pool")
  /** who does the work: an agent (dispatched), or a person (its owner, in AMP) */
  performer?: 'agent' | 'human'
  /** why a person does it (see HUMAN_REASONS) */
  human_reason?: HumanReason
  /** when it last became ready: ready → started is time waiting to be picked up */
  ready_at?: string
  /** the current dispatch token (d_…); empty when it isn't dispatched (AMP #2801) */
  dispatch_id?: string
  /** when a worker started it (a claim, or an older plugin's first activity); null on an in_progress ticket = dispatched but not started (AMP #2801) */
  started_at?: string | null
  /** the agent that started it, "inferred" (an older plugin's first activity), or "backfill" (AMP #2801) */
  started_by?: string
  /** when the watchdog (or amp_return_task) sent it back to ready, and why; cleared by the next dispatch (AMP #2801) */
  returned_at?: string | null
  returned_reason?: string
  /** server-derived, never recomputed here: "not_started" | "started" on an in_progress ticket, absent on any other state (AMP #2801) */
  phase?: TaskPhase
  /** server-derived: minutes a dispatched ticket has waited for a worker; only while phase is not_started (AMP #2801) */
  not_started_minutes?: number
  /** when its scope (name, description, acceptance criteria) was last set; newer than the plan's approval = awaiting approval */
  planned_at?: string
  /** when a person last approved its plan (#2198); approved iff approved_at >= planned_at */
  approved_at?: string
  /** who approved it (null with approved_at set: a migration kept an earlier approval) */
  approved_by?: number | null
  /** set when it completed with a PR or pushed branch while its project requires merges (#2475) */
  merge_required?: boolean
  /** none | needed | merged | abandoned; needed and abandoned hold its dependents (#2475) */
  merge_state?: MergeState
  /** the first merge report: when, and by what (ui, ci or github) */
  merged_at?: string
  merge_source?: string
  /** why it still waits when a dependency is done but not merged: "Waiting on merge of #N (PR …)" */
  waiting_detail?: string
  /** free-form key/value data for integrations (its own, not inherited) */
  metadata?: Metadata
  /** it has a decision doc: its decision is recorded on the doc (Record decision), never by Mark done (#125) */
  has_decision_doc?: boolean
  /** a person's ticket's decide-by date (RFC 3339): set by the person or suggested by an agent (#146) */
  decide_by?: string
  created_at: string
  updated_at: string
}

export type MergeState = 'none' | 'needed' | 'merged' | 'abandoned' | ''

/** Dispatch integrity (AMP #2801): how far an in_progress ticket is. Server-supplied. */
export type TaskPhase = 'not_started' | 'started'

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
  /** unix seconds; absent for docs only the search index has (e.g. the AMP guide) */
  created_at?: number
  /** when an agent last read the doc (amp_kb_get, or a search hit), unix seconds; absent if never */
  last_read_at?: number
  last_read_by?: string
  /** agent reads counted, at most one per reader per 10 minutes */
  read_count?: number
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
  /** unix seconds; absent for docs only the search index has (e.g. the AMP guide) */
  created_at?: number
  /** when an agent last read the doc (amp_kb_get, or a search hit), unix seconds; absent if never */
  last_read_at?: number
  last_read_by?: string
  /** agent reads counted, at most one per reader per 10 minutes */
  read_count?: number
}

export interface KBSearchResult {
  path: string
  title: string
  tags: string[]
  excerpt: string
  /** the whole matched chunk; the excerpt is its first 120 characters */
  chunk_text?: string
  /** position of the matched chunk within the doc */
  chunk_index?: number
  /** semantic similarity in [0,1]; present when the hit came from the vector half of a hybrid search */
  vector_score?: number
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
    | 'receipt.updated'
    | 'decision.updated'
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
  /** a person's ticket's decide-by date (RFC 3339); clear: ['decide_by'] removes it (#146) */
  decide_by?: string
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
  /**
   * Sign-in is off on this AMP (no OIDC_ISSUER_URL): every request is the
   * Local Dev user, an AMP admin provisioned at startup (#124). Absent on
   * older APIs and with sign-in on.
   */
  local_dev?: boolean
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
  /**
   * How much of the account this token carries: `full`, or `submit` for a
   * credential attenuated to one team's inbox (AMP #3772). Every token minted
   * before scopes existed is `full`. Absent on an older API — read it as full.
   */
  scope?: TokenScope
  /** The one team a `submit` credential may send requests to; absent on a full-scope token. */
  scope_team_id?: number | null
  created_by?: number
  created_at: string
  expires_at?: string
  last_used_at?: string
  revoked_at?: string
}

/** A token's reach (AMP #3772): the whole account, or submission to one inbox. */
export type TokenScope = 'full' | 'submit'

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
  // Audit log v2: all optional; entries from before v2 have schema_version 1 (or none) and none of these.
  schema_version?: number
  actor_type?: AuditActorType
  auth_method?: string
  token_id?: number
  session_id?: string
  on_behalf_of_user_id?: number
  agent_profile?: string
  target_name?: string
  outcome?: AuditOutcome
  reason?: string
  source_ip?: string
  user_agent?: string
  client?: AuditClient
  request_id?: string
  trace_id?: string
  before?: Record<string, unknown>
  after?: Record<string, unknown>
  chain_key?: string
  chain_seq?: number
  hash?: string
}

export type AuditActorType = 'human' | 'agent' | 'system' | 'anonymous'
export type AuditOutcome = 'success' | 'denied' | 'failed'
export type AuditClient = 'web' | 'mcp' | 'harness' | 'api' | 'relay' | 'cli' | 'system'

/** What the audit log is narrowed to; empty fields don't filter. Same names as GET /api/audit. */
export interface AuditFilter {
  action?: string
  actor?: string
  actor_type?: AuditActorType | ''
  target_type?: string
  target_id?: string
  ip?: string
  outcome?: AuditOutcome | ''
  client?: AuditClient | ''
  project_id?: number
  /** ISO times; to is exclusive. */
  from?: string
  to?: string
}

/** GET /api/audit/verify */
export type AuditVerifyStatus = 'ok' | 'unproven' | 'failed'

export interface AuditVerifyReport {
  /** True only when status is ok. */
  ok: boolean
  /** ok: verified and signed; unproven: no problems, but nothing signed proves it; failed: tampering or loss. Missing from older servers. */
  status?: AuditVerifyStatus
  checked_at: string
  signing: { configured: boolean; key_id?: string }
  chains: {
    chain_key: string
    ok: boolean
    status?: AuditVerifyStatus
    rows: number
    head_seq: number
    anchor_seq?: number
    checkpoints_matched: number
    signed_checkpoints: number
    last_signed_seq?: number
    problems: { kind: string; seq?: number; audit_id?: number; detail: string }[]
    warnings?: string[]
    /** Rows read by this check: with incremental, only those written since the last one (#2441). Older servers omit it. */
    rows_read?: number
    incremental?: boolean
    /** When every row of the chain was last re-read in full. */
    full_check_at?: string
  }[]
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
  /** The extra header sent with each delivery (its value is never shown). */
  auth_header_name?: string
  auth_header_set?: boolean
  /** A webhook of the whole AMP: audit entries only, all of them. */
  instance?: boolean
  /** Audit entries carry their request context (IP, user agent, session and token ids, before/after). Opt-in; on by default for instance webhooks. */
  include_context?: boolean
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
  /** "Name: value" sets it, "" removes it, absent keeps it. */
  auth_header?: string
  include_context?: boolean
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

/** A project's inbound GitHub merge webhook (GET /projects/:id/github-hook). */
export interface GitHubHook {
  project_id: number
  created_by?: number
  created_at: string
  rotated_at: string
  rejected_count: number
  last_rejected_at?: string
}

/** One signed delivery GitHub made to a project's hook. */
export interface GitHubHookDelivery {
  delivery_id: string
  event: string
  action?: string
  status: number
  result: string
  task_ids: number[] | null
  received_at: string
}

export interface GitHubHookInfo {
  configured: boolean
  /** where GitHub posts; relative when the server has no public URL configured */
  url: string
  hook?: GitHubHook
  deliveries: GitHubHookDelivery[] | null
  /** only in the answer to generating or rotating the secret: shown once */
  secret?: string
  /** the secret is stored encrypted (the server has an encryption key) */
  encrypted: boolean
  setup?: { payload_url?: string; content_type?: string; events?: string }
}

/**
 * A project's GitHub API credential as the API can report it (AMP #3154/#3155).
 * There is deliberately no token field: the token is write-only, so nothing
 * here could carry one even if the server started sending it. `rotated_at`
 * is null on a first attach and set when the token was replaced.
 */
export interface GitHubCredential {
  id: number
  project_id: number
  host: string
  label: string
  created_by?: number
  created_at: string
  rotated_at?: string
}

/**
 * Why a project's GitHub credential is unusable or unconfigurable right now,
 * as a class from a closed list rather than a message — the token never
 * appears in one. Unknown values are rendered as a generic notice, never
 * echoed as a server message.
 *
 * `no_encryption_key`: this server has no AMP_SECRETS_KEY, so a token can't
 * be attached and one already stored can't be opened.
 */
export type GitHubCredentialError = 'no_encryption_key'

/**
 * What an attach (PUT) or rotate (POST) sends. The token is the only secret
 * field; it lives in this request body for one call and is never kept.
 */
export interface GitHubCredentialChange {
  token: string
  label?: string
  host?: string
}

/** GET /projects/:id/github-credential (project owners). */
export interface GitHubCredentialInfo {
  configured: boolean
  host: string
  credential?: GitHubCredential
  /** this server has a key to seal a token with */
  encrypted: boolean
  /** absent or '' when nothing is wrong; see GitHubCredentialError */
  last_error?: string
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
  /** the org's default verification receipts mode (#126) */
  receipts_mode?: ReceiptsMode
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
  /* The "waiting on you" summary (service.MyWorkSummary, AMP #2478). Optional:
     older servers don't send it. KB architecture/waiting-on-you.md. */
  counts?: WaitCounts
  /** projects with anything waiting or coming up, most waiting first */
  projects?: ProjectWait[]
  projects_total?: number
  projects_offset?: number
  projects_limit?: number
  projects_more?: boolean
  rows_per_project?: number
  /** the person's projects with nothing for them */
  clear_projects?: ProjectRef[]
  agents_in_progress?: number
  owned_truncated?: boolean
  flags_truncated?: boolean
}

// ---- Waiting on you (service.WaitingOn; v2/kb/working/human-work.md) ----

export type WaitKind = 'approve' | 'answer' | 'verify' | 'merge' | 'start' | 'do'

/** Waiting is approve + answer + verify + merge + start + do; coming_up is shown, never counted. */
export interface WaitCounts {
  waiting: number
  approve: number
  answer: number
  /** a completion Require holds for you: its receipt is below the minimum (#126); absent from older servers */
  verify?: number
  merge: number
  /** a ticket of yours dispatched to an agent that no worker has started (AMP #2801) */
  start: number
  do: number
  coming_up: number
}

/** GET /my-work/counts: the badges. `projects` holds only projects with something waiting or coming up. */
export interface MyWorkCounts {
  counts: WaitCounts
  projects: Record<string, WaitCounts>
}

/** One step of the Get started checklist: done (and since when), and the project it's about. */
export interface GettingStartedStep {
  done: boolean
  at?: string
  project_id?: number
  /** How the agent connected: mcp, harness (AMP Harness) or token (an API token). */
  client?: string
  /** The ticket the step's link goes to: the one carrying the first receipt. */
  task_id?: number
}

/** GET /api/my-work/getting-started (#128, plus the receipt step #149 owes it). */
export interface GettingStarted {
  agent: GettingStartedStep
  project: GettingStartedStep
  plan: GettingStartedStep
  receipt: GettingStartedStep
}

/** One item in a person's Waiting on people row: a ticket, or for approve, an epic's tickets awaiting them. */
export interface PersonWaitItem {
  kind: WaitKind
  task_ids: number[]
  /** approve only */
  epic_id?: number
  epic_name?: string
  since: string
  /** start: the agent it was dispatched to, and how long it has waited (AMP #2801) */
  agent_id?: string
  not_started_minutes?: number
}

/** One person in GET /projects/:id/waiting-on-people (service.WaitingOn for them). */
export interface PersonWaiting {
  user_id: number
  counts: WaitCounts
  /** approve, answer, merge, do; oldest first within each */
  items: PersonWaitItem[]
  /** ticket ids of their Coming up */
  coming_up: number[]
}

/** GET /projects/:id/waiting-on-people: Mission's Waiting on people, every person with something waiting or coming up. */
export interface PeopleWaiting {
  project_id: number
  people: PersonWaiting[]
}

/** One thing waiting on you (Waiting on you): "approve:<epic id>" (one per epic) or "task:<ticket id>". */
export interface WaitItem extends MergeClassifier {
  kind: WaitKind | 'coming_up'
  key: string
  project_id: number
  project_code: string
  project_name: string
  /** the ticket, for every kind but approve */
  task_id?: number
  /** every ticket the item contains, so ?focus=<any of them> finds it */
  task_ids: number[]
  /** the ticket's name, or the epic's for approve */
  name: string
  state?: string
  since: string
  performer?: string
  human_reason?: string
  owner_user_id?: number
  /** the ticket has a decision doc: it is decided in its drawer, not by Mark done (#125) */
  has_decision_doc?: boolean
  /** a person's ticket's decide-by date (RFC 3339), and whether it has passed (#146) */
  decide_by?: string
  overdue?: boolean
  epic_id?: number
  epic_name?: string
  /** approve: how many tickets await */
  count?: number
  question?: string
  /** verify: why the receipt is below its minimum (#126) */
  hold_reason?: string
  merge_state?: string
  pr_url?: string
  branch?: string
  completed_at?: string
  /** start: the agent it was dispatched to, and how long it has waited (AMP #2801) */
  agent_id?: string
  not_started_minutes?: number
  /** merge, do, answer: open tickets downstream of it, transitively */
  holding_up?: number[]
  /**
   * merge (AMP #3011): the branch the merge would land on, as a run reported it.
   * Absent is *unknown*, never "main" — read target_source, not this, to decide.
   */
  base_branch?: string
  /** merge: where base_branch came from — "run" when a run reported it, "unknown" when nothing did */
  target_source?: MergeTargetSource
  /** merge: open dependents that opted out with waits_for_merge: false — named, never counted */
  not_waiting?: number[]
  /** merge: the plan's human merge step folded into this card (its merge_for names this ticket) */
  tracked_by?: MergeStepFold
  awaiting_approval?: boolean
  start_at?: string
}

/**
 * Where a Merge card's target branch came from (service.MergeTargetRun /
 * MergeTargetDetected / MergeTargetUnknown). "detected" is GitHub's own pull
 * read: it outranks a run's report, because what someone typed or the harness
 * guessed is not what the repository says (AMP #3158).
 */
export type MergeTargetSource = 'run' | 'detected' | 'unknown'

/**
 * The merge-alert classifier's arms (service.MergeAlert*, AMP #3133/#3158):
 * default_branch and rule ask the person for a real reason; unknown is the
 * honest watch (evidence missing, stale, partial or ambiguous); internal is
 * agent organization and asks nobody; none is work already merged.
 */
export type MergeAlertKind = 'default_branch' | 'rule' | 'internal' | 'unknown' | 'none'

/**
 * The merge-alert classifier's fields on a merge item (REST `WaitItem` and MCP
 * `PersonWait` both carry them). Absent on a pre-#3158 server
 * (`classification_made: false` there): then the card keeps its older,
 * explicitly unverified behaviour instead of guessing an arm.
 */
export interface MergeClassifier {
  /** which arm decided this item reaches the person; absent when nothing was classified */
  alert?: MergeAlertKind
  /** one line saying why, in the terms the card shows (artifact #27 v2 states 3 and 9) */
  alert_reason?: string
  /** the rule-box detail for a rule alert: which ruleset demands what, and why it is this person */
  rule?: string
  /** where the person acts on a rule alert: the PR on GitHub, where the review happens */
  review_url?: string
  /** the repository's detected default branch this was classified against; "" when it could not be read */
  default_branch?: string
}

/**
 * The plan's human merge step folded into a Merge card (AMP #3009's `merge_for`,
 * service.MergeStepFold). `ready` is computed by the backend's own bridge guards,
 * so the card never offers Mark merged for a step it would refuse to close.
 */
export interface MergeStepFold {
  task_id: number
  name: string
  state: string
  /** Mark merged may be offered: every prerequisite is met and no guard refuses it */
  ready: boolean
  /** the prerequisites still holding it (its dependencies that aren't satisfied yet) */
  waiting_on?: number[]
  /** why it isn't ready, in one sentence; empty when it is */
  reason?: string
}

/** One project's card in the top-level My work summary. */
export interface ProjectWait {
  project_id: number
  project_code: string
  project_name: string
  counts: WaitCounts
  /** the first items in display order, at most rows_per_project */
  rows: WaitItem[]
  /** how many more items there are in the project */
  more: number
  agents_in_progress: number
}

export interface ProjectRef {
  id: number
  code: string
  name: string
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
  /** the branch the work is meant to merge into */
  base_branch?: string
  pr_url?: string
  pushed?: boolean
  merged?: boolean
  note?: string
  /** a person's reported time on a human ticket */
  hours_worked?: number
  /** the remote-control session the run happened in (links to /sessions/<id>) */
  session_id?: string
  created_at: string
}

/** GET /api/runs/:id: a run and the ticket it belongs to. */
export interface RunView {
  run: Run
  task_id: number
  project_id: number
  /** the ticket's ref, CODE-<id> */
  ref: string
  /** the ticket's canonical link, /t/<ref> */
  url: string
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
  /** 'decision' for a ticket's decision doc */
  role?: string
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

// ---- The project's My work tab and its Approve view (AMP #2478, #2483) ----

/** A ticket outside the approval set that the set depends on, or that depends on it. */
export interface DepSummary {
  id: number
  project_id: number
  epic_id: number
  story_id: number
  name: string
  state: string
  block_reason?: string
  performer: string
  owner_user_id?: number | null
  merge_state: string
  /** the tickets after it may start (done and, while merges are required, merged) */
  satisfied: boolean
  waiting_detail?: string
}

export interface OutsideTicket extends DepSummary {
  epic_name: string
  project_code?: string
  project_name?: string
  relation: 'dependency' | 'dependent' | 'both'
}

/** A human ticket in the plan and how much open work waits on it (amp_check_plan). */
export interface HumanGate {
  id: number
  name: string
  reason: string
  owner_user_id?: number | null
  holding_up: number
}

/** A ticket as it was when last approved (stored at approval, migration 040). */
export interface ApprovedSnapshot {
  name: string
  description: string
  acceptance_criteria: string
  dependency_ids: number[]
}

export interface ChangedSince {
  at: string
  by?: string
  last_approved_at?: string
  /** absent for tickets approved before AMP stored snapshots */
  previous?: ApprovedSnapshot
}

export interface PlanEpic {
  id: number
  name: string
  description: string
  caller_last_approved_at?: string
  ticket_count?: number
  awaiting_ids?: number[]
}

export interface PlanStory {
  id: number
  epic_id?: number
  name: string
  description: string
  acceptance_criteria: string
  task_ids: number[]
  awaiting_ids: number[]
}

/** One open ticket in an approval plan, in full. */
export interface PlanTicket {
  id: number
  epic_id?: number
  story_id: number
  name: string
  description: string
  acceptance_criteria: string
  priority: Priority
  state: TaskState
  block_reason?: string
  performer: 'agent' | 'human' | string
  human_reason?: string
  assigned_to?: string
  owner_user_id?: number | null
  owner_name: string
  created_by?: number | null
  planned_at: string
  approved_at?: string
  approved_by?: number | null
  approved_for?: number | null
  merge_state: string
  /** not approved as planned now */
  awaiting: boolean
  /** the caller approves it as their own */
  yours: boolean
  change: 'new' | 'changed' | null
  changed_since?: ChangedSince
  dependency_ids: number[]
  dependent_ids: number[]
  /** dependencies not satisfied now */
  waits_on: number[]
  starts_on_approval: boolean
}

/** GET /projects/:id/my-work?approve=<epic ids|all> → {plan}. */
export interface ApprovalPlan {
  /** the first epic of the set (older servers: the only one) */
  epic: PlanEpic
  /** every epic in the set; older servers send only `epic` */
  epics?: PlanEpic[]
  stories: PlanStory[]
  done_stories?: PlanStory[]
  /** the newest planned_at returned: send it back verbatim as approve-plan's as_of */
  as_of?: string
  caller_last_approved_at?: string
  require_plan_approval: boolean
  can_approve_on_behalf: boolean
  tickets: PlanTicket[]
  tickets_total: number
  offset: number
  limit: number
  more: boolean
  awaiting_ids: number[]
  yours_ids: number[]
  for_user_ids: number[]
  new_ids: number[]
  changed_ids: number[]
  starts_on_approval: number[]
  human_gates: HumanGate[]
  issues: PlanIssue[]
  outside: OutsideTicket[]
}

/** A ticket's merge state after Mark merged / Not needed (service.MergeRecord). */
export interface MergeRecord {
  task_id: number
  merge_state: MergeState
  merge_source?: string
  merged_at?: string
  /** an earlier report recorded the merge; this one changed nothing */
  already_recorded: boolean
  message: string
  run?: Run
  task?: Task
  /** the plan's human merge step tracking this merge: what the report did to it (AMP #3009) */
  linked_step?: MergeStepRecord
}

/**
 * What a merge report did to the plan's human step whose `merge_for` names the
 * ticket: completed it (one act, not two), found it already done, or left it
 * open because its owner started it or a prerequisite is unfinished.
 */
export interface MergeStepRecord {
  task_id: number
  state: string
  completed: boolean
  already_done: boolean
  message: string
}

/** A ticket's place in a project's My work list (WaitItem with its detail). */
export interface ProjectWaitItem extends WaitItem {
  task?: Task
  plan?: ApprovalPlan
  waits_on?: DepSummary[]
}

export interface DoneItem {
  kind: WaitKind
  task_id?: number
  task_ids: number[]
  epic_id?: number
  name: string
  at: string
  count: number
  merge_source?: string
  merge_state?: string
}

/** GET /projects/:id/my-work */
export interface ProjectMyWork {
  project_id: number
  user_id: number
  /** a lead reading someone else's list: the actions are theirs */
  read_only: boolean
  kinds: WaitKind[]
  counts: WaitCounts
  /** verify is absent from servers that predate receipts (#126) */
  items: Record<Exclude<WaitKind, 'verify'>, ProjectWaitItem[]> & { verify?: ProjectWaitItem[] }
  coming_up: ProjectWaitItem[]
  done_today: DoneItem[]
  agents: { project_id: number; in_progress: number; ready: number; waiting: number; done_today: number }
  day: string
}

/** What a person decided on a completed human ticket that another ticket depends on (GET /tasks/:id/decisions). */
export interface UpstreamDecision {
  task_id: number
  name: string
  human_reason: string
  /** the note, or option + decided text */
  decision: string
  option?: string
  doc_url?: string
  version?: number
  decided_by: number | null
  decided_by_name: string
  decided_at: string
}

/** One answer a person can choose on a judgment ticket (the agent questions of #130 reuse it). */
export interface AskOption {
  label: string
  consequence: string
}

/** A decision doc version's options: its stored metadata. */
export interface DecisionAsk {
  question: string
  options: AskOption[]
  /** index into options; only marks a badge, never chooses */
  recommended: number
}

/** A judgment ticket's ask as tool input (it drafts the doc; not stored on the ticket). */
export interface Ask extends DecisionAsk {
  recommendation_reason: string
  evidence?: string[]
}

export interface DecisionDocVersion {
  version: number
  author_kind: 'person' | 'agent'
  text: string
  ask: DecisionAsk | null
  created_by: number | null
  created_at: string
}

/** A judgment ticket's decision doc (GET /tasks/:id/decision, and /decisions' own). */
export interface DecisionDoc {
  task_id: number
  artifact_id: number
  doc_url?: string
  current_version: number
  /** who wrote the current version */
  author_kind: 'person' | 'agent'
  /** the current version's options */
  ask: DecisionAsk | null
  /** the current version's markdown (prose only) */
  text: string
  decided_version: number | null
  chosen_option: number | null
  decision_text?: string
  decided_by: number | null
  decided_by_name?: string
  recording_started_at: string | null
  decided_at: string | null
  versions?: DecisionDocVersion[]
}

// ---- Team inbox requests (epic 202, story 549; AMP #3977, build slice J0) ----
//
// The DTOs for the team-request surface every UI slice (J1–J6) builds on. They
// mirror the API's own answers exactly (internal/domain/request.go,
// requester_view.go and internal/api/request_*.go), and two rules from those
// files are carried into the types themselves rather than left to call sites:
//
//  1. **The two views are different types, not one type with optional fields.**
//     `TeamRequest` is the stored record a triager works from — org, claim,
//     assignee, contact snapshot, revision, metadata, reject reason. A requester
//     gets `RequesterRequestView`, the whitelist projection, which has no field
//     for any of those. Keeping them apart here is what stops a component from
//     rendering an internal field on the requester surface: there is nothing to
//     read. Never widen `RequesterRequestView` with an internal field — the
//     server refuses to send one (writeRequestRead in request_reads.go), so a
//     field added here could only ever be `undefined` at runtime.
//
//  2. **A write answers with the act's facts, never the record.** Every write
//     route answers `{request: {id, public_ref, state, rev}, changed}`, or the
//     lifecycle/discussion variants of that shape. That is deliberate on the
//     server: the edit and requester routes are reachable by a customer, so one
//     envelope for every caller is the safe one. A triager's UI re-reads the
//     record it is rendering — which it must do anyway, since the claim and
//     assignee it shows are two fields the write answer does not carry.
//
// Bounds are the server's real ones (domain/request.go); the UI checks them
// before a submit so a person finds out in the form rather than after a round
// trip, but the server is the authority.

/**
 * A request's lifecycle state. Nine values, and reopen is a *transition*, not a
 * state: a reopened request goes back to `triaging`/`needs_information` with
 * `reopened_count` raised, so no component has to special-case a tenth.
 */
export type RequestStatus =
  | 'new'
  | 'triaging'
  | 'needs_information'
  | 'accepted'
  | 'in_progress'
  | 'delivered'
  | 'requester_accepted'
  | 'rejected'
  | 'duplicate'

/** The nine states in lifecycle order (the order the queue's pills use). */
export const REQUEST_STATUSES: readonly RequestStatus[] = [
  'new', 'triaging', 'needs_information', 'accepted', 'in_progress',
  'delivered', 'requester_accepted', 'rejected', 'duplicate',
]

/** How a request arrived. Only a connector carries a vendor object id. */
export type RequestSourceKind = 'web' | 'mcp' | 'api' | 'connector'

/** The four intake sources. */
export const REQUEST_SOURCE_KINDS: readonly RequestSourceKind[] = ['web', 'mcp', 'api', 'connector']

/**
 * What one discussion row is. A `question` is the desk asking the requester
 * something (`needs_information` obliges a preceding one) and an `answer` is
 * what returns the request to where it was.
 */
export type RequestCommentKind = 'comment' | 'question' | 'answer' | 'system'

/** The four discussion kinds. */
export const REQUEST_COMMENT_KINDS: readonly RequestCommentKind[] = ['comment', 'question', 'answer', 'system']

/** What one append-only history row records. */
export type RequestEventAction =
  | 'submit' | 'transition' | 'claim' | 'release' | 'assign'
  | 'link' | 'unlink' | 'edit'

/**
 * The durable relation a link carries. Never metadata: `routes_to` is the one
 * project a request is routed to, `planned_as` the plan items serving it,
 * `delivered_by` the tasks/artifacts/receipts that prove delivery, and the last
 * two point at another request only.
 */
export type RequestLinkKind = 'routes_to' | 'planned_as' | 'delivered_by' | 'duplicate_of' | 'relates_to'

/** What a link may point at. No foreign key: a link outlives its target (tombstone). */
export type RequestTargetKind = 'project' | 'epic' | 'story' | 'task' | 'artifact' | 'receipt' | 'request'

/**
 * Which target kinds each link kind may point at — the server's own matrix
 * (requestLinkTargets), so a UI picker cannot offer a pair the API will refuse.
 */
export const REQUEST_LINK_TARGETS: Readonly<Record<RequestLinkKind, readonly RequestTargetKind[]>> = {
  routes_to: ['project'],
  planned_as: ['epic', 'story', 'task'],
  delivered_by: ['task', 'artifact', 'receipt'],
  duplicate_of: ['request'],
  relates_to: ['request'],
}

/** A private upload's scan verdict. `unsupported` is *unknown*, not clean. */
export type RequestScanState = 'pending' | 'clean' | 'infected' | 'unsupported'

/**
 * Why a file may not be downloaded right now (empty means it may). It is the
 * server's single decision (domain.RequestAttachmentDownloadRefusal), which both
 * the mint and the byte-serving route call, so a panel and a download cannot
 * disagree.
 */
export type RequestAttachmentRefusal = '' | 'scan_infected' | 'scan_pending' | 'held_for_release' | 'purged'

/** One request in a team's inbox: the stored record a triager works from. */
export interface TeamRequest {
  id: number
  /** The opaque reference a requester quotes (D1) — never a guessable sequence. */
  public_ref: string
  org_id: number
  team_id: number

  /** The signed-in requester, `null` for a service or connector submission. */
  requester_user_id: number | null
  /** The contact snapshot taken at submit; survives user deletion. */
  requester_contact: string

  source_kind: RequestSourceKind
  /** The vendor's immutable id (connector only); it, not a key, dedups a replay. */
  source_external_id: string

  subject: string
  input_brief: string
  /** The team's delivery note. Empty until the delivery transition writes it. */
  outcome_brief: string

  state: RequestStatus

  /** The triage claim: "I am looking at it right now", one at a time, never expired. */
  claim_user_id: number | null
  claimed_at: string | null
  /** The assignee who owns the outcome. A transfer clears the claim — they are never one field. */
  owner_user_id: number | null

  /** The canonical request once this one is flattened; a canonical is never a duplicate. */
  duplicate_of: number | null
  reject_reason: string

  needs_info_since: string | null
  planned_at: string | null
  delivered_at: string | null
  /** Acceptance is the requester's alone (I1/D5): nothing else may set these. */
  accepted_at: string | null
  accepted_by: number | null
  closed_at: string | null
  reopened_at: string | null
  reopened_count: number

  /** The optimistic-concurrency token every write carries as If-Match / expected_rev. */
  rev: number

  /** Connector labels and routing hints only; durable relations live in link rows. */
  metadata: Metadata
  created_at: string
  updated_at: string
}

/** One row of a request's discussion: append-only, so the thread is also the record. */
export interface RequestComment {
  id: number
  request_id: number
  kind: RequestCommentKind
  body: string
  /** The display name at write time; `author_user_id` is the AMP user behind it, when there is one. */
  author: string
  author_user_id: number | null
  created_at: string
}

/** One row of a request's history: append-only, exactly one per act. */
export interface RequestEvent {
  id: number
  request_id: number
  org_id: number
  team_id: number
  action: RequestEventAction
  /** Both set for a `transition` only. */
  from_status?: RequestStatus
  to_status?: RequestStatus
  actor: string
  actor_user_id: number | null
  /** JSON explaining the act (a transfer's previous assignee, a link's target). */
  detail?: string
  created_at: string
}

/** A durable relation from a request to planned or delivered work. */
export interface RequestLink {
  id: number
  request_id: number
  link_kind: RequestLinkKind
  target_kind: RequestTargetKind
  target_id: number
  created_by: number | null
  created_at: string
}

// ---- The queue and its counts ----

/** How many requests a filter matches in each state, plus their sum. */
export interface RequestStateTotals {
  /** The sum of `states` — the filter's whole queue. */
  total: number
  /** One entry per lifecycle state (all nine are always present). */
  states: Record<RequestStatus, number>
}

/** One person's slice of one filtered queue: what they are triaging and what they own. */
export interface RequestOwnership {
  user_id: number
  /** Rows whose triage claim this user holds. Never expired or stolen (D4). */
  claimed_ids: number[]
  /** Rows this user owns the outcome of. */
  assigned_ids: number[]
  /** `claimed_ids` or `assigned_ids`, each id once, newest first — a personal panel renders this. */
  ids: number[]
  count: number
  /** The cursor for the next page, absent on the last one. */
  next_before_id?: number
  /** How many distinct requests the ownership + filter match, cursor-free. */
  total: number
}

/** The team's queue: one keyset page, the per-state counts beside it, and (when asked) the caller's own slice. */
export interface RequestQueue {
  /** The page, newest first. */
  items: TeamRequest[]
  /** How many rows this page holds (not the filtered total; `totals` carries that). */
  count: number
  /** The cursor for the next page, absent on the last one. */
  next_before_id?: number
  /** The whole filter's per-state counts — what each queue pill shows. */
  totals: RequestStateTotals
  /** The caller's claimed/assigned slice, absent when it was not asked for. */
  mine?: RequestOwnership
}

/** The queue's counts without its page — My work's inbox panels read this. */
export interface RequestQueueSummary {
  totals: RequestStateTotals
  mine?: RequestOwnership
}

/**
 * The filters the team queue accepts. Every term is passed to the server, which
 * refuses an unknown value rather than matching nothing: a typo that returned an
 * empty queue would read as "the team has no requests", the one answer a queue
 * must never invent.
 */
export interface RequestQueueFilter {
  /** Any of these states; empty means every state. */
  state?: RequestStatus[]
  /** A user id, or 'me'. */
  submitter?: number | 'me'
  source_kind?: RequestSourceKind
  /** RFC 3339 timestamps. */
  created_after?: string
  created_before?: string
  /** Only requests that carry at least one attachment. */
  attached?: boolean
  q?: string
  /** Keyset paging: the `next_before_id` of the page before. */
  before_id?: number
  limit?: number
  /** Also ask for the caller's own claimed/assigned slice. */
  mine?: boolean
  mine_before_id?: number
  mine_limit?: number
}

/** The filters the requester's own list accepts — deliberately smaller than the queue's. */
export interface OwnRequestFilter {
  state?: RequestStatus[]
  before_id?: number
  limit?: number
}

// ---- The requester's whitelist projection (never the record) ----

/**
 * One step of a request's public progress. Five steps and no ticket, project or
 * phase name among them — the labels are the UI's copy, not the server's.
 */
export type RequesterMilestoneKey = 'received' | 'reviewing' | 'in_progress' | 'delivered' | 'accepted'

/** The closed stepper vocabulary, in order. */
export const REQUESTER_MILESTONE_KEYS: readonly RequesterMilestoneKey[] = [
  'received', 'reviewing', 'in_progress', 'delivered', 'accepted',
]

/** One public step: whether it happened, when, and one optional sentence. */
export interface RequesterMilestone {
  key: RequesterMilestoneKey
  done: boolean
  /** Present only for a done step. */
  at?: string
  /** A public sentence about the step, already sanitized by the server. */
  note?: string
}

/** Who wrote a shared comment, reduced to the only two attributions a requester may see. */
export type RequesterAuthor = 'you' | 'desk'

/** One comment the team chose to share: no id, no user id, no display name. */
export interface RequesterComment {
  kind: RequestCommentKind
  from: RequesterAuthor
  body: string
  at: string
}

/** The desk's open question — the "needs a bit more from you" card. */
export interface RequesterQuestion {
  body: string
  at: string
}

/** Whose file an output is: the requester's own upload, or something the team shared. */
export type RequesterOutputSource = 'you' | 'desk'

/** The only status word a requester sees about a file — three words, no domain state. */
export type RequesterOutputStatus = 'ready' | 'preparing' | 'unavailable'

/** One file the requester may see on their request. */
export interface RequesterOutput {
  source: RequesterOutputSource
  name: string
  size_bytes: number
  status: RequesterOutputStatus
  at?: string
  /** The opaque download handle, present only for a `ready` file — never a row id. */
  download_ref?: string
}

/**
 * One request as its own requester sees it. The whitelist is a *type*, not a
 * filter: there is no field here for a project, a claim, an assignee, a
 * revision, a metadata key, an agent identity or an audit row, so a component
 * built on it cannot leak one even by accident.
 */
export interface RequesterRequestView {
  public_ref: string
  state: RequestStatus
  /** The word the approved requester mockup shows for `state` — render this, branch on `state`. */
  state_word: string
  subject: string
  input_brief: string
  /** Present only once the request is delivered; a brief written mid-flight stays hidden. */
  outcome_brief?: string
  reject_reason?: string
  /** The desk's display name — the one team fact the mockup shows. */
  team_name?: string
  created_at: string
  needs_reply_since?: string
  delivered_at?: string
  accepted_at?: string
  closed_at?: string
  milestones: RequesterMilestone[]
  comments: RequesterComment[]
  outputs: RequesterOutput[]
  /** The open question, absent when the team shared none. */
  question?: RequesterQuestion
}

/** One requester's list: their own requests only, by construction. */
export interface OwnRequestPage {
  items: RequesterRequestView[]
  count: number
  next_before_id?: number
  /** How many requests the caller has in total, cursor-free. */
  total: number
}

// ---- The delivery aggregate (team view only) ----

/** What one piece of delivery evidence is. */
export type RequestOutcomeItemKind = 'task' | 'artifact' | 'receipt'

/** One piece of evidence's honest state, in order of "how far from delivered". */
export type RequestOutcomeItemState =
  | 'open' | 'blocked' | 'done' | 'merge_gated' | 'held' | 'deleted' | 'unverified'

/** The one word for the team rail. `delivered` is the only phase that says the outcome exists. */
export type RequestPhase = 'unlinked' | 'not_started' | 'in_progress' | 'blocked' | 'delivered' | 'closed'

/** One piece of linked evidence. */
export interface RequestOutcomeItem {
  kind: RequestOutcomeItemKind
  id: number
  state: RequestOutcomeItemState
  /** Set only for a merge-gated task, so a rail can say which gate it is. */
  merge_state?: string
  /** The tombstone date when the target is gone and the trash still remembers it. */
  deleted_at?: string
  /** A receipt's derived status; set only for a receipt. */
  verification_status?: string
  /** How many of the piece's runs died or were stopped. */
  rework?: number
}

/**
 * What a request's linked work actually amounts to, plus the delivery gate.
 *
 * `delivered_eligible` is the one decision that is not a count, and it is
 * deliberately NOT `done === total`: a request whose tickets are all complete is
 * reported as such and is explicitly not delivered (I8). Nothing about ticket
 * state can make it true.
 */
export interface RequestOutcome {
  phase: RequestPhase
  items: RequestOutcomeItem[]
  total: number
  done: number
  open: number
  blocked: number
  gated: number
  held: number
  deleted: number
  unverified: number
  /** The sum of the items' abandoned runs — "5 of 5 done" is not the same delivery with 3 of them. */
  rework: number
  /** `planned_as` epics/stories/tickets plus the one `routes_to` project: "is this planned at all". */
  linked: number
  /** The active routes_to target, absent for none. */
  routed_project_id?: number
  /** Whether the delivery note is set (I2's first half) — kept apart from its text. */
  outcome_brief_set: boolean
  /** Whether the delivery was the explicit "there is nothing to link" record (I2's second half). */
  no_artifact: boolean
  /** The delivery gate for these facts. What a Deliver panel disables its button on. */
  delivered_eligible: boolean
  /** What is missing before a delivery could be made; absent when eligible or already delivered. */
  blocked_reason?: string
}

// ---- Write answers (the act's facts, never the record) ----

/**
 * What every write answers with: which request, where it now stands, at what
 * revision. No org, no claim, no assignee, no contact, no metadata.
 *
 * `rev` is here for both views and it is the one internal-looking number a
 * requester gets: an edit is a compare-and-set, so a form saved twice needs the
 * revision the first save produced.
 */
export interface RequestMutationFacts {
  id: number
  public_ref: string
  state: RequestStatus
  rev: number
}

/** One write's answer. `changed` is false for a replay ("already was", not "done"). */
export interface RequestMutationAnswer {
  request: RequestMutationFacts
  changed: boolean
}

/** One discussion row as a write answers it. There is deliberately no author. */
export interface RequestCommentFacts {
  id: number
  kind: RequestCommentKind
  body: string
  created_at: string
}

/** One discussion write's answer. `wrote` distinguishes a new row (201) from a retried one (200). */
export interface RequestCommentAnswer {
  comment: RequestCommentFacts
  wrote: boolean
}

/** One lifecycle act's answer: the request's facts, the row the move appended (if any) and whether it wrote. */
export interface RequestLifecycleAnswer {
  request: RequestMutationFacts
  /** The question or answer the move appended; absent when the act carried none. Never an author. */
  comment?: RequestCommentFacts
  changed: boolean
}

// ---- The two read answers ----

/**
 * One request's answer on either surface. `view` says which of the two objects
 * is set, and exactly one ever is: a team answer carries `request` (plus
 * `outcome`), a requester answer carries `projection` and nothing else.
 */
export type RequestReadAnswer =
  | { view: 'team'; request: TeamRequest; outcome: RequestOutcome }
  | { view: 'requester'; projection: RequesterRequestView }

/** What a submission answers with — a whitelist, not the record. */
export interface SubmittedRequest {
  /** The opaque reference the requester will quote. */
  public_ref: string
  state: RequestStatus
  subject: string
  team_id: number
  source_kind: RequestSourceKind
  created_at: string
}

/** A submission's answer. `created` is false for a replay of the same `Idempotency-Key`. */
export interface RequestSubmissionAnswer {
  request: SubmittedRequest
  created: boolean
}

// ---- Request bodies ----

/** `POST /api/teams/:id/requests`. The caller's retry token goes in the `Idempotency-Key` header. */
export interface SubmitRequestInput {
  subject: string
  input_brief: string
  /** Connector labels and routing hints only — never an authorization decision. */
  metadata?: Metadata
}

/** `PATCH /api/teams/:id/requests/:request`. The fields a submitter may move, and its precondition. */
export interface UpdateRequestInput {
  subject?: string
  input_brief?: string
  /** Answered by name (refused): the delivery note is the delivery transition's. */
  outcome_brief?: string
  /** Answered by name (refused): metadata is the team's and its integrations'. */
  metadata?: MetadataPatch
  /** Required by the server: an edit is a compare-and-set. */
  expected_rev?: number
}

/** `POST …/comments`. */
export interface RequestDiscussionInput {
  kind: RequestCommentKind
  body: string
  /** The id a retried post is answering, so the retry returns the row the first attempt wrote. */
  retry_of_comment_id?: number
}

/** `POST …/assign`. */
export interface RequestAssignInput {
  /** A user id, an email, 'me', or 'none'/'unassigned'. */
  assignee: string
  expected_rev?: number
}

/**
 * The evidence fields a lifecycle route may send. Every one is present so the
 * *policy* refuses a field the destination does not read (an outcome brief on a
 * reject, a question on a plain step) rather than the decoder dropping it.
 */
export interface RequestLifecycleInput {
  /** The generic `transition` route's destination. */
  to?: RequestStatus
  /** What the desk asks (needs-info, I5). */
  question?: string
  /** Why the request is declined (reject, I3). */
  reject_reason?: string
  /** The request a duplicate folds into (duplicate, I3/D7). */
  canonical_request_id?: number
  /** What is and is not included (deliver, I2). */
  outcome_brief?: string
  /** The explicit "there is nothing to link" record (deliver, I2). */
  no_artifact?: boolean
  /** The revision the caller read; omitted means "the revision this call reads". */
  expected_rev?: number
}

/** `POST /api/requests/:ref/answer`. There is no field for a state or an actor. */
export interface RequesterAnswerInput {
  answer: string
  /** The question comment being answered; 0 answers the question currently open. */
  answer_to?: number
  expected_rev?: number
}

/** `…/claim`, `…/release`, `…/accept` and `…/reopen` take only a precondition. */
export interface RequestPreconditionInput {
  /** The revision the caller read; omitted means "whatever the row is now". */
  expected_rev?: number
}

// ---- Lineage: links and the planning handoff ----

/** `POST …/links`: one relation, and nothing else. */
export interface RequestLinkInput {
  link_kind: RequestLinkKind
  target_kind: RequestTargetKind
  target_id: number
  /** Allows a `routes_to` attach to move the request to a different project (D9), keeping the old route as history. */
  supersede_route?: boolean
}

/** What a link's target resolved to, read from the target's own row. Absent on a replay of a deleted target. */
export interface RequestLinkTargetFacts {
  kind: RequestTargetKind
  id: number
  project_id: number
}

/** One attach's answer. `changed: false` is a replay: nothing was written at all. */
export interface RequestLinkAnswer {
  link: RequestLink
  changed: boolean
  target?: RequestLinkTargetFacts
}

/**
 * One ticket the caller wants planned. Every field is a *plan* decision; the
 * ones a caller may not make (assignee, owner, start_at, performer, metadata,
 * approval) have no field at all — request text decides nothing.
 */
export interface RequestHandoffItem {
  /** This item's key within the batch, stable across attempts of the same handoff. */
  ref: string
  /** Where the ticket goes; it must be in the request's routed project. */
  story_id: number
  name: string
  /** The plan author's instruction, written outside the untrusted fence. */
  plan: string
  acceptance_criteria: string
  /** 0 low, 1 normal, 2 high, 3 critical. */
  priority?: Priority
  /** Refs in this batch, or existing ticket ids — as a string or a number. */
  depends_on?: (string | number)[]
}

/** `POST …/handoff`: the caller's retry token and the batch. */
export interface RequestHandoffInput {
  /** Idempotency token for THIS batch: the same key retries, a new key is a deliberate re-plan. */
  handoff_key: string
  items: RequestHandoffItem[]
}

/** One ref's outcome in a handoff. */
export interface RequestHandoffItemResult {
  ref: string
  /** The ticket serving this ref — created by this call, or by an earlier attempt of the same handoff. */
  task_id: number
  /** True when THIS call created the ticket; false is "it already existed". */
  created: boolean
  /** True when this call wrote the `planned_as` relation; false is a replay. */
  linked: boolean
}

/** One handoff attempt: where the batch was planned, what each ref became, and the counts a retry reads. */
export interface RequestHandoffAnswer {
  project_id: number
  items: RequestHandoffItemResult[]
  created: number
  already: number
  warnings?: string[]
}

// ---- Attachments ----

/** What the download gate would say about one file right now. */
export interface RequestAttachmentDownloadState {
  /** True exactly when the file may be served. Says nothing about the caller. */
  ready: boolean
  /** Why not, when it is not ready. */
  refusal?: RequestAttachmentRefusal
  /** The derived restricted-file hold — shown as such even while "held" is the reason. */
  held: boolean
}

/** One file as a listing answers it: the metadata a panel shows, never the bytes. */
export interface RequestAttachmentFacts {
  id: number
  request_id: number
  file_name: string
  /** What AMP sniffed from the bytes, never the client's header. */
  content_type: string
  size_bytes: number
  sha256: string
  scan_state: RequestScanState
  uploaded_by: number | null
  created_at: string
  /** Whether a keep pins the file past the retention window. */
  keep: boolean
  kept_by: number | null
  kept_at: string | null
  expires_at: string | null
  released_at: string | null
  released_by: number | null
  /** The restricted-file hold: waiting for a team lead to release it. */
  held: boolean
  state: RequestAttachmentDownloadState
}

/** AMP's upload bounds, travelling with a listing so a caller can say why a sixth upload would fail. */
export interface RequestAttachmentLimits {
  max_file_bytes: number
  max_files: number
  max_total_bytes: number
  retention_days: number
}

/** The file panel: one request's identity and its files. */
export interface RequestAttachmentPanel {
  request_id: number
  public_ref: string
  items: RequestAttachmentFacts[]
  count: number
  limits: RequestAttachmentLimits
}

/** One upload's answer. `held` is at the top level too: it is the fact a UI branches on first. */
export interface RequestUploadAnswer {
  attachment: RequestAttachmentFacts
  held: boolean
  /** One sentence about what the caller can and cannot do next. */
  next: string
}

/** One keep or unkeep's answer: the file's retention columns as they now stand. */
export interface RequestAttachmentKeepAnswer {
  attachment: RequestAttachmentFacts
  keep: boolean
}

/** One delete's answer. A retry of a delete that already happened is the surface's 404, not a false. */
export interface RequestAttachmentDeleteAnswer {
  deleted: boolean
  attachment_id: number
}

/** `POST …/attachments`: the file's display name and its bytes. No content type, no scan state, no location. */
export interface RequestUploadInput {
  file_name: string
  /** The bytes, base64-encoded. AMP never fetches a location a caller names. */
  content_base64: string
}

/** One minted download link. */
export interface RequestDownloadLink {
  url: string
  expires_at: string
  attachment_id: number
  request_id: number
  file_name: string
  size_bytes: number
}

// ---- Who may submit to an inbox (D14), and requester invitations ----

/**
 * A revocable grant of submit access on one team's inbox. Never membership: it
 * reaches no project, so the holder can submit and nothing else.
 */
export interface TeamSubmitGrant {
  team_id: number
  org_id: number
  user_id: number
  granted_by?: number
  granted_at: string
  /** Set once revoked; a revoked grant fails the very next check with no cache to clear. */
  revoked_by?: number
  revoked_at?: string
}

/** A grant with the recipient's identity, for the administration listing. */
export interface TeamSubmitGrantee extends TeamSubmitGrant {
  email: string
  display_name?: string
  kind?: string
}

/** One grant as the administration view gets it. Granting is idempotent, so there is no `changed`. */
export interface RequestGrantAnswer {
  team_id: number
  grant: TeamSubmitGrant
  active: boolean
}

/** A team's grants: in force first, then revoked. */
export interface RequestGrantList {
  team_id: number
  grants: TeamSubmitGrantee[]
}

/** One pending requester invitation. Its kind is always 'requester' and it grants no role. */
export interface RequestInviteAnswer {
  team_id: number
  invite: Invite
}

/** An inbox's pending requester invitations. */
export interface RequestInviteList {
  team_id: number
  invites: Invite[]
}

// ---- The stable codes a client branches on ----

/**
 * The four `request.*` codes every refusal on this surface carries, so a client
 * branches on a code rather than on a message that may be reworded. `not_found`
 * is deliberately indistinguishable between "no such request" and "not yours":
 * existence never leaks.
 */
export type RequestErrorCode =
  | 'request.not_found'
  | 'request.conflict'
  | 'request.invalid'
  | 'request.rate_limited'

/**
 * What a 429 submission answer carries: the same wait in the body as in
 * `Retry-After`, so a client that reads only one still honours it.
 */
export interface RequestRateLimitedBody {
  error: string
  code: 'request.rate_limited'
  retry_after_s: number
}

/** The bound on a one-line subject, in characters (the server's own limit). */
export const MAX_REQUEST_SUBJECT_LEN = 200
/** The bound on a brief (`input_brief` / `outcome_brief`), in characters. */
export const MAX_REQUEST_BRIEF_LEN = 20000
/** The bound on a reject reason, in characters. */
export const MAX_REQUEST_REJECT_REASON_LEN = 2000
