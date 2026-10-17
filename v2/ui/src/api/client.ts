import type {
  Project, Epic, Story, Task, Comment, ActivityLog, KBDoc, KBDocSummary, KBSearchResult, KBTagCount, KBAnnotation,
  KBRef, KBInfo, KBSource, Metadata, DayReport, VelocityDay, PlanReport, MyWork, ProjectReport,
  CreateEpicRequest, UpdateEpicRequest, CreateStoryRequest, UpdateStoryRequest, CreateTaskRequest, UpdateTaskRequest,
  Me, AmpUser, AuthAdminUser, Org, OrgRole, Team, TeamRole, Member, ProjectMember, ProjectAccess, ProjectTeam,
  ProjectTeamRole, Invite, AddMemberResult, CreateProjectRequest,
  APIToken, NewAPIToken, AuditEntry, Webhook, WebhookChange, WebhookDelivery,
} from '../types'

const BASE = '/api'

function kbQuery(ref: KBRef): string {
  if (ref.team_id != null) return `team_id=${ref.team_id}`
  if (ref.org_id != null) return `org_id=${ref.org_id}`
  return `project_id=${ref.project_id}`
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`)
  if (!res.ok) {
    const err = await res.json().catch(() => ({}))
    throw new Error(err.error || `GET ${path}: ${res.status}`)
  }
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

const webhookBase = (scope: { orgId?: number; projectId?: number }) =>
  scope.orgId ? `/orgs/${scope.orgId}/webhooks` : `/projects/${scope.projectId}/webhooks`

// The API accepts users as an email or numeric id, always sent as a string.
const userRef = (user: string | number) => String(user)

// Projects
export const api = {
  listProjects: () => get<{ projects: Project[] }>('/projects').then(r => r.projects),
  getProject: (id: number) => get<{ project: Project }>(`/projects/${id}`).then(r => r.project),
  updateProjectSettings: (id: number, settings: { name?: string; description?: string; open_reassign?: boolean; metadata?: Record<string, string | number | boolean | null> }) =>
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
  /** the created task, with warnings about likely planning mistakes (e.g. no dependencies) */
  createTask: (projectId: number, req: CreateTaskRequest) =>
    post<Task & { warnings?: string[] }>(`/projects/${projectId}/tasks`, req),
  /** dependency_ids is the complete new list */
  setTaskDependencies: (id: number, dependencyIds: number[]) =>
    put<{ task: Task; warnings?: string[] | null }>(`/tasks/${id}/dependencies`, { dependency_ids: dependencyIds }),
  moveTask: (id: number, storyId: number) =>
    post<{ task: Task }>(`/tasks/${id}/move`, { story_id: storyId }).then(r => r.task),
  moveStory: (id: number, epicId: number) =>
    post<{ story: Story }>(`/stories/${id}/move`, { epic_id: epicId }).then(r => r.story),
  projectReport: (projectId: number, opts: { since?: string; until?: string; groupBy?: string }) => {
    const q = new URLSearchParams()
    if (opts.since) q.set('since', opts.since)
    if (opts.until) q.set('until', opts.until)
    if (opts.groupBy) q.set('group_by', opts.groupBy)
    return get<ProjectReport>(`/projects/${projectId}/report?${q}`)
  },
  planCheck: (projectId: number) => get<PlanReport>(`/projects/${projectId}/plan-check`),
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

  // KB: every call names one knowledge base (a project's, a team's, or an org's).
  kbInfo: (ref: KBRef) => get<KBInfo>(`/kb/info?${kbQuery(ref)}`),
  kbSources: (projectId: number) =>
    get<{ sources: KBSource[] }>(`/kb/sources?project_id=${projectId}`).then(r => r.sources ?? []),
  kbList: (ref: KBRef, tag?: string) =>
    get<{ docs: KBDocSummary[]; count: number }>(`/kb/list?${kbQuery(ref)}${tag ? '&tag=' + encodeURIComponent(tag) : ''}`).then(r => r.docs ?? []),
  kbSearch: (ref: KBRef, query: string, limit = 10) =>
    get<{ results: KBSearchResult[]; count: number }>(`/kb/search?${kbQuery(ref)}&q=${encodeURIComponent(query)}&limit=${limit}`).then(r => r.results ?? []),
  kbGet: (ref: KBRef, path: string) =>
    get<KBDoc>(`/kb/doc?${kbQuery(ref)}&path=${encodeURIComponent(path)}`),
  kbWrite: (ref: KBRef, doc: { path: string; title: string; content: string; tags: string[]; reason?: string }) =>
    post<KBDocSummary>(`/kb/doc?${kbQuery(ref)}`, doc),
  kbDelete: (ref: KBRef, path: string) =>
    del(`/kb/doc?${kbQuery(ref)}&path=${encodeURIComponent(path)}`),
  kbTags: (ref: KBRef) =>
    get<{ tags: KBTagCount[] }>(`/kb/tags?${kbQuery(ref)}`).then(r => r.tags ?? []),
  kbAnnotate: (ref: KBRef, path: string, text: string) =>
    post<KBAnnotation>(`/kb/annotate?${kbQuery(ref)}`, { path, text }),
  /** index: the annotation's position in doc.annotations (oldest first) */
  kbResolveAnnotation: (ref: KBRef, path: string, index: number, resolved: boolean) =>
    post<KBDoc>(`/kb/resolve?${kbQuery(ref)}`, { path, index, resolved }),

  // Metadata
  inheritedMetadata: (kind: 'epic' | 'story' | 'task', id: number) =>
    get<{ inherited: Metadata }>(`/metadata/inherited?kind=${kind}&id=${id}`).then(r => r.inherited ?? {}),

  // Daily reports
  dayReports: (opts: { user?: string; from?: string; to?: string; tz?: string }) => {
    const q = new URLSearchParams(Object.entries(opts).filter(([, v]) => v) as [string, string][])
    return get<{ reports: DayReport[] | null }>(`/day-reports?${q}`).then(r => r.reports ?? [])
  },
  dayReportDraft: (date: string, tz: string) =>
    get<{ report: DayReport }>(`/day-reports/draft?date=${date}&tz=${encodeURIComponent(tz)}`).then(r => r.report),
  saveDayReport: (date: string, tz: string, summary: string) =>
    put<{ report: DayReport }>(`/day-reports/${date}?tz=${encodeURIComponent(tz)}`, { summary }).then(r => r.report),
  reportPeople: () =>
    get<{ people: Member[] | null }>('/day-reports/people').then(r => r.people ?? []),
  velocity: (opts: { user?: string; from?: string; to?: string; tz?: string }) => {
    const q = new URLSearchParams(Object.entries(opts).filter(([, v]) => v) as [string, string][])
    return get<{ days: VelocityDay[] | null }>(`/velocity?${q}`).then(r => r.days ?? [])
  },

  // Actions
  dispatchTask: (taskId: number, agentId = 'amp-worker') =>
    post(`/tasks/${taskId}/dispatch`, { agent_id: agentId }),
  completeTask: (taskId: number) =>
    post(`/tasks/${taskId}/complete`),
  /** Answer an agent's flag (a ticket blocked with a reason): posts the
   *  answer as a comment and puts the ticket back to ready (or waiting). */
  answerFlag: (taskId: number, answer: string) =>
    post<{ task: Task }>(`/tasks/${taskId}/unblock`, { answer }).then(r => r.task),

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
  deactivateUser: (userId: number) => post<{ user: AmpUser }>(`/users/${userId}/deactivate`).then(r => r.user),
  reactivateUser: (userId: number) => post<{ user: AmpUser }>(`/users/${userId}/reactivate`).then(r => r.user),

  // Agent accounts and API tokens ("me" is the caller)
  myWork: () => get<MyWork>('/my-work'),
  listAgents: () => get<{ agents: AmpUser[] }>('/agents').then(r => r.agents),
  createAgent: (name: string, teamIds: number[]) =>
    post<{ agent: AmpUser }>('/agents', { name, team_ids: teamIds }).then(r => r.agent),
  listTokens: (user: number | 'me') => get<{ tokens: APIToken[] | null }>(`/users/${user}/tokens`).then(r => r.tokens ?? []),
  createToken: (user: number | 'me', name: string, expiresInDays = 0) =>
    post<{ token: NewAPIToken }>(`/users/${user}/tokens`, { name, expires_in_days: expiresInDays }).then(r => r.token),
  revokeToken: (tokenId: number) => del(`/tokens/${tokenId}`),

  // Audit log: global admins any; org admins pass orgId; project owners projectId
  audit: (q: { orgId?: number; projectId?: number; actorId?: number; action?: string; before?: number; limit?: number }) => {
    const p = new URLSearchParams()
    if (q.orgId) p.set('org_id', String(q.orgId))
    if (q.projectId) p.set('project_id', String(q.projectId))
    if (q.actorId) p.set('actor_id', String(q.actorId))
    if (q.action) p.set('action', q.action)
    if (q.before) p.set('before', String(q.before))
    if (q.limit) p.set('limit', String(q.limit))
    return get<{ entries: AuditEntry[] | null }>(`/audit?${p}`).then(r => r.entries ?? [])
  },

  // Webhooks
  listWebhooks: (scope: { orgId?: number; projectId?: number }) =>
    get<{ webhooks: Webhook[] | null }>(webhookBase(scope)).then(r => r.webhooks ?? []),
  createWebhook: (scope: { orgId?: number; projectId?: number }, change: WebhookChange) =>
    post<{ webhook: Webhook }>(webhookBase(scope), change).then(r => r.webhook),
  updateWebhook: (id: number, change: WebhookChange) => patch<{ webhook: Webhook }>(`/webhooks/${id}`, change).then(r => r.webhook),
  deleteWebhook: (id: number) => del(`/webhooks/${id}`),
  rotateWebhookSecret: (id: number) => post<{ webhook: Webhook }>(`/webhooks/${id}/rotate-secret`).then(r => r.webhook),
  testWebhook: (id: number) => post<{ delivery_id: number }>(`/webhooks/${id}/test`),
  listDeliveries: (id: number, limit = 30) =>
    get<{ deliveries: WebhookDelivery[] | null }>(`/webhooks/${id}/deliveries?limit=${limit}`).then(r => r.deliveries ?? []),

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
