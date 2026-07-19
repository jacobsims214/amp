import type { Project, Epic, Story, Task, Comment, ActivityLog, KBDoc, KBDocSummary, KBSearchResult, KBTagCount, KBAnnotation, CreateEpicRequest, UpdateEpicRequest, CreateStoryRequest, UpdateStoryRequest, CreateTaskRequest, UpdateTaskRequest } from '../types'

const BASE = '/api'

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`)
  if (!res.ok) throw new Error(`GET ${path}: ${res.status}`)
  return res.json()
}

async function post<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || `POST ${path}: ${res.status}`)
  }
  return res.json()
}

async function patch<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || `PATCH ${path}: ${res.status}`)
  }
  return res.json()
}

async function del(path: string): Promise<void> {
  const res = await fetch(`${BASE}${path}`, { method: 'DELETE' })
  if (!res.ok) throw new Error(`DELETE ${path}: ${res.status}`)
}

// Projects
export const api = {
  listProjects: () => get<{ projects: Project[] }>('/projects').then(r => r.projects),
  getProject: (id: number) => get<{ project: Project }>(`/projects/${id}`).then(r => r.project),
  archiveProject: (id: number) => post<Project>(`/projects/${id}/archive`),
  restoreProject: (id: number) => post<Project>(`/projects/${id}/restore`),

  // Epics
  listEpics: (projectId: number) =>
    get<{ epics: Epic[] }>(`/projects/${projectId}/epics`).then(r => r.epics ?? []),
  getEpic: (id: number) => get<{ epic: Epic }>(`/epics/${id}`).then(r => r.epic),

  // Stories
  listStories: (epicId: number) =>
    get<{ stories: Story[] }>(`/epics/${epicId}/stories`).then(r => r.stories ?? []),
  getStory: (id: number) => get<{ story: Story }>(`/stories/${id}`).then(r => r.story),

  // Epic CRUD
  createEpic: (projectId: number, req: CreateEpicRequest) =>
    post<Epic>(`/projects/${projectId}/epics`, req),
  updateEpic: (id: number, req: UpdateEpicRequest) =>
    patch<{ epic: Epic }>(`/epics/${id}`, req).then(r => r.epic),
  deleteEpic: (id: number) =>
    del(`/epics/${id}`),

  // Story CRUD
  createStory: (epicId: number, req: CreateStoryRequest) =>
    post<Story>(`/epics/${epicId}/stories`, req),
  updateStory: (id: number, req: UpdateStoryRequest) =>
    patch<{ story: Story }>(`/stories/${id}`, req).then(r => r.story),
  deleteStory: (id: number) =>
    del(`/stories/${id}`),

  // Tasks
  listTasks: (projectId: number) =>
    get<{ tasks: Task[] }>(`/projects/${projectId}/tasks`).then(r => r.tasks ?? []),
  getTask: (id: number) => get<{ task: Task }>(`/tasks/${id}`).then(r => r.task),

  // Task CRUD
  createTask: (projectId: number, req: CreateTaskRequest) =>
    post<Task>(`/projects/${projectId}/tasks`, req),
  updateTask: (id: number, req: UpdateTaskRequest) =>
    patch<{ task: Task }>(`/tasks/${id}`, req).then(r => r.task),
  deleteTask: (id: number) =>
    del(`/tasks/${id}`),
  setTaskStartAt: (id: number, startAt: string | null) =>
    post<{ task_id: number; start_at: string | null }>(`/tasks/${id}/start_at`, { start_at: startAt }),

  // Comments
  getComments: (taskId: number) =>
    get<{ comments: Comment[] }>(`/tasks/${taskId}/comments`).then(r => r.comments ?? []),
  addComment: (taskId: number, body: string, author = 'user') =>
    post<Comment>(`/tasks/${taskId}/comments`, { body, author }),

  // History
  getHistory: (taskId: number) =>
    get<{ history: ActivityLog[]; count: number }>(`/tasks/${taskId}/history`).then(r => r.history ?? []),

  // Project activity log (for report dashboard)
  getProjectActivity: (projectId: number, since?: string, until?: string) => {
    const params = new URLSearchParams()
    if (since) params.set('since', since)
    if (until) params.set('until', until)
    const qs = params.toString()
    return get<{ activity: ActivityLog[]; count: number }>(
      `/projects/${projectId}/activity${qs ? '?' + qs : ''}`
    ).then(r => r.activity ?? [])
  },

  // KB
  kbList: (projectId: number, tag?: string) =>
    get<{ docs: KBDocSummary[]; count: number }>(`/kb/list?project_id=${projectId}${tag ? '&tag=' + encodeURIComponent(tag) : ''}`).then(r => r.docs ?? []),
  kbSearch: (projectId: number, query: string, limit = 10) =>
    get<{ results: KBSearchResult[]; count: number }>(`/kb/search?project_id=${projectId}&q=${encodeURIComponent(query)}&limit=${limit}`).then(r => r.results ?? []),
  kbGet: (projectId: number, path: string) =>
    get<KBDoc>(`/kb/doc?project_id=${projectId}&path=${encodeURIComponent(path)}`),
  kbWrite: (projectId: number, path: string, title: string, content: string, tags: string[], author = 'user') =>
    post<KBDocSummary>(`/kb/doc?project_id=${projectId}`, { path, title, content, tags, author }),
  kbDelete: async (projectId: number, path: string) => {
    const res = await fetch(`${BASE}/kb/doc?project_id=${projectId}&path=${encodeURIComponent(path)}`, { method: 'DELETE' })
    if (!res.ok) throw new Error(`DELETE kb doc: ${res.status}`)
  },
  kbTags: (projectId: number) =>
    get<{ tags: KBTagCount[] }>(`/kb/tags?project_id=${projectId}`).then(r => r.tags ?? []),
  kbConfig: (projectId: number) =>
    get<{ typesense_url: string; api_key: string; collection: string }>(`/kb/config?project_id=${projectId}`),
  kbAnnotate: (projectId: number, path: string, text: string, author = 'user') =>
    post<KBAnnotation>(`/kb/annotate?project_id=${projectId}`, { path, text, author }),

  // Actions
  dispatchTask: (taskId: number, agentId = 'amp-worker') =>
    post(`/tasks/${taskId}/dispatch`, { agent_id: agentId }),
  completeTask: (taskId: number) =>
    post(`/tasks/${taskId}/complete`),

  // Export
  exportProject: async (projectId: number): Promise<{ blob: Blob; filename: string }> => {
    const res = await fetch(`${BASE}/projects/${projectId}/export`)
    if (!res.ok) throw new Error(`Export failed: ${res.status}`)
    
    // Get filename from Content-Disposition header or construct it
    const contentDisposition = res.headers.get('content-disposition')
    let filename = `amp-export-${projectId}-${new Date().toISOString().split('T')[0]}.json`
    
    if (contentDisposition) {
      const match = contentDisposition.match(/filename="?([^"]+)"?/)
      if (match) filename = match[1]
    }
    
    const blob = await res.blob()
    return { blob, filename }
  },

  // Import
  importProject: async (bundleText: string): Promise<Project> => {
    const bundle = JSON.parse(bundleText)
    return post<Project>('/projects/import', bundle)
  },
}
