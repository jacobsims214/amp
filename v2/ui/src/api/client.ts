import type { Project, Epic, Story, Task, Comment, ActivityLog, KBDoc, KBDocSummary, KBSearchResult, KBTagCount } from '../types'

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

// Projects
export const api = {
  listProjects: () => get<{ projects: Project[] }>('/projects').then(r => r.projects),
  getProject: (id: number) => get<{ project: Project }>(`/projects/${id}`).then(r => r.project),

  // Epics
  listEpics: (projectId: number) =>
    get<{ epics: Epic[] }>(`/projects/${projectId}/epics`).then(r => r.epics ?? []),
  getEpic: (id: number) => get<{ epic: Epic }>(`/epics/${id}`).then(r => r.epic),

  // Stories
  listStories: (epicId: number) =>
    get<{ stories: Story[] }>(`/epics/${epicId}/stories`).then(r => r.stories ?? []),
  getStory: (id: number) => get<{ story: Story }>(`/stories/${id}`).then(r => r.story),

  // Tasks
  listTasks: (projectId: number) =>
    get<{ tasks: Task[] }>(`/projects/${projectId}/tasks`).then(r => r.tasks ?? []),
  getTask: (id: number) => get<{ task: Task }>(`/tasks/${id}`).then(r => r.task),

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

  // Actions
  dispatchTask: (taskId: number, agentId = 'amp-worker') =>
    post(`/tasks/${taskId}/dispatch`, { agent_id: agentId }),
  completeTask: (taskId: number) =>
    post(`/tasks/${taskId}/complete`),
}
