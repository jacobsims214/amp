export type TaskState = 'backlog' | 'in_progress' | 'completed' | 'blocked'
export type Priority = '0' | '1' | '2' | '3'

export interface Project {
  id: number
  name: string
  code: string
  description: string
  state: 'active' | 'archived'
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
  assigned_to?: string    // set at plan time by manager — who should work this
  agent_id?: string       // set at dispatch time — who is actually working it
  dispatched_at?: string
  completed_at?: string
  block_reason?: string
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

export interface KBDoc {
  id: string
  project_id: number
  path: string
  title: string
  content: string
  tags: string[]
  author: string
  chunk_index: number
  chunk_text: string
  updated_at: number
}

export interface KBDocSummary {
  id: string
  project_id: number
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
}

export interface KBTagCount {
  tag: string
  count: number
}

export interface SSEEvent {
  type:
    | 'connected'
    | 'project.created'
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
  project_id?: number
  payload?: unknown
  at?: string
}
