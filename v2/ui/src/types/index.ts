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
  created_at: string
  updated_at: string
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
  created_at: string
  updated_at: string
}

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
  created_at: string
}

// ---- KB types ----

export interface KBAnnotation {
  author: string
  text: string
  created_at: number
  is_resolved: boolean
}

/** Names one knowledge base: set exactly one id. */
export interface KBRef {
  project_id?: number
  team_id?: number
  org_id?: number
}

export type KBScope = 'project' | 'team' | 'org'

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
  project_id?: number
  payload?: unknown
  at?: string
}

// ---- CRUD request types ----

export interface CreateEpicRequest {
  name: string
  description?: string
  priority?: string
}

export interface UpdateEpicRequest {
  name?: string
  description?: string
  priority?: string
}

export interface CreateStoryRequest {
  name: string
  description?: string
  acceptance_criteria?: string
  priority?: string
}

export interface UpdateStoryRequest {
  name?: string
  description?: string
  acceptance_criteria?: string
  priority?: string
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
}

export interface UpdateTaskRequest {
  name?: string
  description?: string
  acceptance_criteria?: string
  assigned_to?: string
  priority?: string
  start_at?: string | null
}

// ---- Auth / users ----

export interface Me {
  subject: string
  email: string
  name: string
  roles: string[]
  user_id?: number
  orgs?: Org[]
}

export interface AmpUser {
  id: number
  subject: string
  email: string
  display_name: string
  roles: string[]
  created_at: string
  last_seen_at: string
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
}

export interface ProjectMember {
  user_id: number
  email: string
  display_name: string
  access: ProjectAccess
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
