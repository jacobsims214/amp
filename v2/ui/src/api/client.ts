import type {
  Project, Epic, Story, Task, Comment, ActivityLog, KBDoc, KBDocSummary, KBSearchResult, KBTagCount, KBAnnotation,
  CreateEpicRequest, UpdateEpicRequest, CreateStoryRequest, UpdateStoryRequest, CreateTaskRequest, UpdateTaskRequest,
  Me, AmpUser, AuthAdminUser, Org, OrgRole, Team, TeamRole, Member, ProjectMember, ProjectAccess, ProjectTeam,
  ProjectTeamRole, Invite, AddMemberResult, CreateProjectRequest,
} from '../types'

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

async function put<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || `PUT ${path}: ${res.status}`)
  }
  return res.json()
}

// del resolves to the parsed JSON body when there is one (several RBAC
// endpoints return the updated resource), or undefined for empty bodies.
async function del<T = void>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`, { method: 'DELETE' })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || `DELETE ${path}: ${res.status}`)
  }
  return res.json().catch(() => undefined) as Promise<T>
}

// The API accepts users as an email or numeric id, always sent as a string.
const userRef = (user: string | number) => String(user)

// Projects
export const api = {
  listProjects: () => get<{ projects: Project[] }>('/projects').then(r => r.projects),
  getProject: (id: number) => get<{ project: Project }>(`/projects/${id}`).then(r => r.project),
  updateProjectSettings: (id: number, settings: { open_reassign: boolean }) =>
    patch<{ project: Project }>(`/projects/${id}`, settings).then(r => r.project),
  createProject: (req: CreateProjectRequest) => post<Project>('/projects', req),
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

  addEpicOwner: (id: number, user: string | number) =>
    post<{ epic: Epic }>(`/epics/${id}/owners`, { user: userRef(user) }).then(r => r.epic),
  removeEpicOwner: (id: number, userId: number) =>
    del<{ epic: Epic }>(`/epics/${id}/owners/${userId}`).then(r => r.epic),

  // Story CRUD
  createStory: (epicId: number, req: CreateStoryRequest) =>
    post<Story>(`/epics/${epicId}/stories`, req),
  updateStory: (id: number, req: UpdateStoryRequest) =>
    patch<{ story: Story }>(`/stories/${id}`, req).then(r => r.story),
  deleteStory: (id: number) =>
    del(`/stories/${id}`),

  addStoryOwner: (id: number, user: string | number) =>
    post<{ story: Story }>(`/stories/${id}/owners`, { user: userRef(user) }).then(r => r.story),
  removeStoryOwner: (id: number, userId: number) =>
    del<{ story: Story }>(`/stories/${id}/owners/${userId}`).then(r => r.story),

  // Project access
  listProjectMembers: (projectId: number) =>
    get<{ members: ProjectMember[] | null; my_access: ProjectAccess }>(`/projects/${projectId}/members`)
      .then(r => ({ members: r.members ?? [], my_access: r.my_access })),
  listProjectTeams: (projectId: number) =>
    get<{ teams: ProjectTeam[] | null }>(`/projects/${projectId}/teams`).then(r => r.teams ?? []),
  setProjectTeam: (projectId: number, teamId: number, role: ProjectTeamRole) =>
    put<{ teams: ProjectTeam[] | null }>(`/projects/${projectId}/teams/${teamId}`, { role }).then(r => r.teams ?? []),
  removeProjectTeam: (projectId: number, teamId: number) =>
    del<{ teams: ProjectTeam[] | null }>(`/projects/${projectId}/teams/${teamId}`).then(r => r.teams ?? []),

  // Tasks
  // owner: 'any' = every ticket in the project (the board filters client-side);
  // omitted server-side default is "my work" only.
  listTasks: (projectId: number, owner = 'any') =>
    get<{ tasks: Task[] }>(`/projects/${projectId}/tasks?owner=${encodeURIComponent(owner)}`).then(r => r.tasks ?? []),
  getTask: (id: number) => get<{ task: Task }>(`/tasks/${id}`).then(r => r.task),

  // Task CRUD
  createTask: (projectId: number, req: CreateTaskRequest) =>
    post<Task>(`/projects/${projectId}/tasks`, req),
  updateTask: (id: number, req: UpdateTaskRequest) =>
    patch<{ task: Task }>(`/tasks/${id}`, req).then(r => r.task),
  deleteTask: (id: number) =>
    del(`/tasks/${id}`),
  /** user: member email/id, 'me' to claim, or 'none' to release */
  assignTask: (id: number, user: string | number) =>
    post<{ task: Task }>(`/tasks/${id}/assign`, { user: userRef(user) }).then(r => r.task),
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
  importProject: async (bundleText: string, home?: { org_id?: number; team_id?: number }): Promise<Project> => {
    const bundle = JSON.parse(bundleText)
    const params = new URLSearchParams()
    if (home?.org_id) params.set('org_id', String(home.org_id))
    if (home?.team_id) params.set('team_id', String(home.team_id))
    const qs = params.toString()
    return post<Project>(`/projects/import${qs ? '?' + qs : ''}`, bundle)
  },

  // Auth / identity
  me: () => get<Me>('/me'),
  listUsers: () => get<{ users: AmpUser[] }>('/users').then(r => r.users),
  setUserRole: (userId: number, role: string, grant: boolean) =>
    patch<{ user_id: number }>(`/users/${userId}/role`, { role, grant }),
  deleteUser: (userId: number) => del(`/users/${userId}`),

  // Orgs
  listOrgs: () => get<{ orgs: Org[] | null }>('/orgs').then(r => r.orgs ?? []),
  getOrg: (id: number) => get<{ org: Org }>(`/orgs/${id}`).then(r => r.org),
  createOrg: (req: { name: string; slug?: string; owner?: string }) =>
    post<{ org: Org; owner_invite?: Invite }>('/orgs', req),
  renameOrg: (id: number, name: string) => patch<{ org: Org }>(`/orgs/${id}`, { name }).then(r => r.org),
  deleteOrg: (id: number) => del(`/orgs/${id}`),
  listOrgMembers: (orgId: number) =>
    get<{ members: Member[] | null }>(`/orgs/${orgId}/members`).then(r => r.members ?? []),
  addOrgMember: (orgId: number, user: string | number, role: OrgRole) =>
    post<AddMemberResult>(`/orgs/${orgId}/members`, { user: userRef(user), role }),
  setOrgMemberRole: (orgId: number, userId: number, role: OrgRole) =>
    put(`/orgs/${orgId}/members/${userId}`, { role }),
  removeOrgMember: (orgId: number, userId: number) => del(`/orgs/${orgId}/members/${userId}`),
  listOrgTeams: (orgId: number) =>
    get<{ teams: Team[] | null }>(`/orgs/${orgId}/teams`).then(r => r.teams ?? []),
  createTeam: (orgId: number, name: string) =>
    post<{ team: Team }>(`/orgs/${orgId}/teams`, { name }).then(r => r.team),
  listOrgInvites: (orgId: number) =>
    get<{ invites: Invite[] | null }>(`/orgs/${orgId}/invites`).then(r => r.invites ?? []),
  createInvite: (orgId: number, req: { email: string; org_role?: OrgRole; team_id?: number; team_role?: TeamRole }) =>
    post<AddMemberResult>(`/orgs/${orgId}/invites`, req),
  revokeInvite: (orgId: number, inviteId: number) => del(`/orgs/${orgId}/invites/${inviteId}`),

  // Teams
  getTeam: (id: number) =>
    get<{ team: Team; members: Member[] | null }>(`/teams/${id}`).then(r => ({ team: r.team, members: r.members ?? [] })),
  renameTeam: (id: number, name: string) => patch<{ team: Team }>(`/teams/${id}`, { name }).then(r => r.team),
  deleteTeam: (id: number) => del(`/teams/${id}`),
  addTeamMember: (teamId: number, user: string | number, role: TeamRole) =>
    post<AddMemberResult>(`/teams/${teamId}/members`, { user: userRef(user), role }),
  setTeamMemberRole: (teamId: number, userId: number, role: TeamRole) =>
    put(`/teams/${teamId}/members/${userId}`, { role }),
  removeTeamMember: (teamId: number, userId: number) => del(`/teams/${teamId}/members/${userId}`),
}

// ---- amp-authadmin — Dex credential CRUD (routed via Envoy at /authadmin/*) ----
const AUTHADMIN_BASE = '/authadmin'

async function authadminReq<T>(path: string, method: string, body?: unknown): Promise<T> {
  const res = await fetch(`${AUTHADMIN_BASE}${path}`, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || `${method} ${path}: ${res.status}`)
  }
  return res.json()
}

export const authAdminApi = {
  listCredentials: () => authadminReq<{ users: AuthAdminUser[] }>('/users', 'GET').then(r => r.users),
  createCredential: (email: string, password: string, displayName?: string) =>
    authadminReq<AuthAdminUser>('/users', 'POST', { email, password, display_name: displayName }),
  resetPassword: (email: string, password: string) =>
    authadminReq<{ email: string }>(`/users/${encodeURIComponent(email)}`, 'PATCH', { password }),
  deleteCredential: (email: string) =>
    authadminReq<{ email: string }>(`/users/${encodeURIComponent(email)}`, 'DELETE'),
}
