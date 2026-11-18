import type {
  Project, Epic, Story, Task, Comment, ActivityLog, KBDoc, KBDocSummary, KBSearchResult, KBTagCount, KBAnnotation,
  KBRef, KBInfo, KBSource, Metadata, DayReport, VelocityDay, PlanReport, MyWork, ProjectReport,
  CreateEpicRequest, UpdateEpicRequest, CreateStoryRequest, UpdateStoryRequest, CreateTaskRequest, UpdateTaskRequest,
  Me, AmpUser, AuthAdminUser, Org, OrgRole, Team, TeamRole, Member, ProjectMember, ProjectAccess, ProjectTeam,
  ProjectTeamRole, Invite, AddMemberResult, CreateProjectRequest,
  Artifact, ArtifactVersion, APIToken, NewAPIToken, TokenScope, AuditEntry, AuditFilter, AuditVerifyReport, Webhook, WebhookChange, WebhookDelivery, GitHubHookInfo, GitHubCredentialInfo, GitHubCredentialChange, Run, RunView, Usage, UpstreamDecision, DecisionAsk, DecisionDoc, TraceHit, DeletedTask, DeletedGroup, ProjectTrash, GroupRestoreResult, Portfolio,
  ApproveRequest, ApproveResponse, ApproveResult, ApprovalPlan, MergeRecord, ProjectMyWork, MyWorkCounts, PeopleWaiting,
  GettingStarted, ReceiptsMode, ReceiptMinLevel, ReceiptSummary, ReceiptView, TaskReceipt,
  RequestQueueFilter, RequestQueue, RequestQueueSummary, RequestReadAnswer, OwnRequestFilter, OwnRequestPage,
  SubmitRequestInput, RequestSubmissionAnswer, UpdateRequestInput, RequestMutationAnswer, RequestDiscussionInput,
  RequestCommentAnswer, RequestPreconditionInput, RequestAssignInput, RequestLifecycleInput, RequestLifecycleAnswer,
  RequesterAnswerInput, RequestLinkInput, RequestLinkAnswer, RequestHandoffInput, RequestHandoffAnswer,
  RequestReadPageOpts, RequestThreadAnswer, RequestEventsAnswer, RequestLinksAnswer, RequestLinkKind,
  RequestAttachmentPanel, RequestUploadInput, RequestUploadAnswer, RequestAttachmentKeepAnswer, RequestAttachmentDeleteAnswer,
  RequestDownloadLink, RequestGrantList, RequestGrantAnswer, RequestInviteList, RequestInviteAnswer,
} from '../types'
import type { ResolvedRequestLink } from '../lib/requestControls'
import { auditQuery, auditVerifyUrl, type AuditScope } from '../lib/audit'
import { apiErrorMessage, NOT_SIGNED_IN } from '../lib/apiError'
import { isSessionEnded, markSessionEnded, sessionLost } from '../lib/session'

const BASE = '/api'

function kbQuery(ref: KBRef): string {
  if (ref.global) return 'scope=global'
  if (ref.team_id != null) return `team_id=${ref.team_id}`
  if (ref.org_id != null) return `org_id=${ref.org_id}`
  return `project_id=${ref.project_id}`
}

/**
 * A failed request's error carries the HTTP status (404 not found, 403 no access)
 * and, when the server sent them, the facts a caller retries with.
 *
 * `code` is the refusal's stable code — `request.*` on the inbox surface, so a
 * caller branches on the code rather than on a message that may be reworded.
 * `rev` and `claimUserId` are a 409's facts: the revision the row is actually
 * at (retry with it, no re-read) and who holds the claim (so the loser can be
 * told). `retryAfterS` is a 429's wait. All are additive; a caller that reads
 * only `message` and `status` is unaffected.
 */
export type HttpError = Error & {
  status?: number
  code?: string
  rev?: number
  claimUserId?: number
  retryAfterS?: number
}

const sessionEndedError = () => Object.assign(new Error(NOT_SIGNED_IN), { status: 401, sessionEnded: true })

/**
 * A refused response as the error this client throws: the server's reason
 * (lib/apiError.ts) as the message, plus the status and every retry fact the
 * body and headers carry.
 *
 * The 429 handling reads both spellings — the `Retry-After` header, which an
 * HTTP client honours without reading the body, and the body's
 * `retry_after_s` — because the submission route writes the same number to
 * both and a caller should never have to know which one it got.
 */
async function failure(res: Response, method: string, path: string): Promise<HttpError> {
  const body = (await res
    .json()
    .catch(() => (method === 'GET' ? {} : { error: res.statusText }))) as Record<string, unknown>
  const header = Number.parseInt(res.headers.get('retry-after') ?? '', 10)
  const fromBody = typeof body.retry_after_s === 'number' ? body.retry_after_s : undefined
  return Object.assign(
    new Error(apiErrorMessage(res.status, typeof body.error === 'string' ? body.error : undefined, `${method} ${path}: ${res.status}`)),
    {
      status: res.status,
      code: typeof body.code === 'string' ? body.code : undefined,
      rev: typeof body.rev === 'number' ? body.rev : undefined,
      claimUserId: typeof body.claim_user_id === 'number' ? body.claim_user_id : undefined,
      retryAfterS: Number.isFinite(header) ? header : fromBody,
    },
  )
}

/**
 * Every AMP API call goes through here. A response from oauth2-proxy rather
 * than amp-api (the session ended: a 401, a redirect to sign-in, or the
 * sign-in page as a 200 HTML page) marks the session ended app-wide (lib/session.ts) and throws the
 * "sign in again" sentence; any other failure throws the server's reason
 * (lib/apiError.ts) with the status and retry facts set (see HttpError). Once
 * the session has ended nothing is sent at all, so the pages' pollers go quiet
 * behind the prompt.
 *
 * `headers` carries a call's own transport facts — the inbox submission's
 * `Idempotency-Key` is the only one today. It is never a credential: sign-in is
 * the proxy's cookie, and nothing here stores or sends a token.
 *
 * `signal` cancels the request. An aborted call rejects with the signal's
 * reason (fetch's AbortError for a plain `controller.abort()`), never with an
 * HttpError, so a caller can tell "I cancelled this" from "the server refused".
 * That holds even when the abort lands while a refusal's body is being read.
 */
async function send(
  method: string,
  path: string,
  body?: unknown,
  base = BASE,
  headers?: Record<string, string>,
  signal?: AbortSignal,
): Promise<Response> {
  if (isSessionEnded()) throw sessionEndedError()
  const res = await fetch(`${base}${path}`, {
    method,
    // Don't follow the proxy to the sign-in page: see it as a redirect.
    redirect: 'manual',
    headers: {
      ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      ...headers,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal,
  })
  if (sessionLost(res)) {
    markSessionEnded()
    throw sessionEndedError()
  }
  if (!res.ok) {
    const err = await failure(res, method, path)
    // failure() swallows a body that won't parse; an abort is not that.
    signal?.throwIfAborted()
    throw err
  }
  return res
}

/** A cancellable read's option: the signal that aborts it. */
export interface ReadOpts { signal?: AbortSignal }

const get = <T>(path: string, signal?: AbortSignal): Promise<T> =>
  send('GET', path, undefined, BASE, undefined, signal).then(r => r.json())
const post = <T>(path: string, body?: unknown, headers?: Record<string, string>): Promise<T> =>
  send('POST', path, body, BASE, headers).then(r => r.json())
const patch = <T>(path: string, body?: unknown, headers?: Record<string, string>): Promise<T> =>
  send('PATCH', path, body, BASE, headers).then(r => r.json())
const put = <T>(path: string, body?: unknown): Promise<T> => send('PUT', path, body).then(r => r.json())

// del resolves to the parsed JSON body when there is one (several RBAC
// endpoints return the updated resource), or undefined for empty bodies.
const del = <T = void>(path: string): Promise<T> =>
  send('DELETE', path).then(r => r.json().catch(() => undefined) as Promise<T>)

// The credential host travels in the query string; the token never does.
const credentialHostQuery = (host?: string) => host ? `?host=${encodeURIComponent(host)}` : ''

// Org, project, or (neither) instance webhooks.
const webhookBase = (scope: { orgId?: number; projectId?: number }) =>
  scope.orgId ? `/orgs/${scope.orgId}/webhooks` : scope.projectId ? `/projects/${scope.projectId}/webhooks` : '/webhooks'

// The API accepts users as an email or numeric id, always sent as a string.
const userRef = (user: string | number) => String(user)

// Projects
export const api = {
  listProjects: () => get<{ projects: Project[] }>('/projects').then(r => r.projects),
  getProject: (id: number) => get<{ project: Project }>(`/projects/${id}`).then(r => r.project),
  updateProjectSettings: (id: number, settings: { name?: string; description?: string; open_reassign?: boolean; require_plan_approval?: boolean; require_merge?: boolean; not_started_flag_minutes?: number; not_started_return_minutes?: number; receipts_mode?: '' | ReceiptsMode; receipts_min_level?: ReceiptMinLevel; decision_escalation_minutes?: number; decision_escalation_raise_priority?: boolean; decision_backup_decider?: number; metadata?: Record<string, string | number | boolean | null> }) =>
    patch<{ project: Project }>(`/projects/${id}`, settings).then(r => r.project),
  createProject: (req: CreateProjectRequest) => post<Project>('/projects', req),
  archiveProject: (id: number) => post<Project>(`/projects/${id}/archive`),
  restoreProject: (id: number) => post<Project>(`/projects/${id}/restore`),

  // Epics
  listEpics: (projectId: number) =>
    get<{ epics: Epic[] }>(`/projects/${projectId}/epics`).then(r => r.epics ?? []),
  getEpic: (id: number) => get<{ epic: Epic }>(`/epics/${id}`).then(r => r.epic),

  // Stories
  /**
   * Every story in the project, in one request. Rejects when the API doesn't
   * have the endpoint yet (an older server), so callers fall back to
   * listStories per epic.
   */
  listProjectStories: (projectId: number) =>
    get<{ stories?: Story[] | null }>(`/projects/${projectId}/stories`).then(r => {
      if (!r || !('stories' in r)) throw new Error('GET /projects/:id/stories: not supported')
      return r.stories ?? []
    }),
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
  /** Put the project in an org (or move it); links to the old org's teams are removed. */
  setProjectOrg: (projectId: number, orgId: number, ownerTeamId?: number) =>
    post<{ project: Project; unlinked: ProjectTeam[] | null }>(`/projects/${projectId}/org`, { org_id: orgId, owner_team_id: ownerTeamId ?? 0 }),
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
  /**
   * Approve tickets' plans (#2198). Every field is optional: taskIds are exactly
   * the tickets to approve (omitted: all of the caller's own that wait, or
   * everyone's with onBehalf); asOf is the newest planned_at among them,
   * verbatim from the API (#2192); onBehalf allows other people's tickets
   * (project leads and above). The server answers 403 naming the ids when
   * on_behalf is missing or the caller isn't a lead.
   */
  approvePlan: (projectId: number, req: ApproveRequest = {}) => {
    const body: Record<string, unknown> = {}
    if (req.taskIds) body.task_ids = req.taskIds
    if (req.asOf) body.as_of = req.asOf
    if (req.onBehalf) body.on_behalf = true
    return post<ApproveResponse>(`/projects/${projectId}/approve-plan`, body).then((r): ApproveResult => ({
      project: r.project,
      approved: r.approved ?? [],
      awaitingApproval: r.awaiting_approval ?? [],
      // An older server (one project-wide approval) has no owners: treat what waits as the caller's.
      awaitingYours: r.awaiting_yours ?? r.awaiting_approval ?? [],
      skipped: r.skipped ?? {},
      onBehalf: !!r.on_behalf,
    }))
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

  // Artifacts
  listArtifacts: (projectId: number, taskId?: number) =>
    get<{ artifacts: Artifact[] }>(`/projects/${projectId}/artifacts${taskId != null ? `?task_id=${taskId}` : ''}`).then(r => r.artifacts ?? []),
  getArtifact: (id: number) => get<{ artifact: Artifact; versions: ArtifactVersion[] }>(`/artifacts/${id}`),
  reviewArtifact: (id: number, review: { approve: boolean; note?: string; version?: number }) =>
    post<{ artifact: Artifact }>(`/artifacts/${id}/review`, review).then(r => r.artifact),
  /** Who published, reviewed, kept and viewed it. Throws for people who may not see it. */
  artifactActivity: (id: number) => get<{ activity: AuditEntry[] }>(`/artifacts/${id}/activity`).then(r => r.activity ?? []),
  keepArtifact: (id: number, keep: boolean) => post<{ artifact: Artifact }>(`/artifacts/${id}/keep`, { keep }).then(r => r.artifact),
  letArtifactExpire: (id: number) => post<{ artifact: Artifact }>(`/artifacts/${id}/let-expire`).then(r => r.artifact),
  /** My artifacts (made by me, or on my tickets) that expire within `days` and that nobody kept. */
  expiringArtifacts: (days = 14) => get<{ artifacts: Artifact[] }>(`/artifacts/expiring?days=${days}`).then(r => r.artifacts ?? []),
  /** Where a version's content is served (sandboxed). */
  artifactContentUrl: (id: number, version?: number) => `${BASE}/artifacts/${id}/content${version ? `?version=${version}` : ''}`,
  /** a receipt evidence file (#126): served with Range, so a video seeks */
  receiptEvidenceUrl: (key: string) => `${BASE}/receipts/${key.split('/').map(encodeURIComponent).join('/')}`,

  // Comments
  getComments: (taskId: number) =>
    get<{ comments: Comment[] }>(`/tasks/${taskId}/comments`).then(r => r.comments ?? []),
  addComment: (taskId: number, body: string, author = 'user') =>
    post<Comment>(`/tasks/${taskId}/comments`, { body, author }),

  // History
  getRun: (runId: number) => get<RunView>(`/runs/${runId}`),
  taskRuns: (taskId: number) => get<{ runs: Run[] | null; usage: Usage }>(`/tasks/${taskId}/runs`).then(r => ({ runs: r.runs ?? [], usage: r.usage })),
  trace: (q: { commit?: string; branch?: string; pr?: string }) => {
    const p = new URLSearchParams()
    if (q.commit) p.set('commit', q.commit)
    if (q.branch) p.set('branch', q.branch)
    if (q.pr) p.set('pr', q.pr)
    return get<{ tickets: TraceHit[] | null }>(`/trace?${p}`).then(r => r.tickets ?? [])
  },
  projectDayReports: (projectId: number, date: string, tz: string) =>
    get<{ day: string; people: { user_id: number; email: string; name: string; report?: DayReport }[] }>(
      `/projects/${projectId}/day-reports?date=${encodeURIComponent(date)}&tz=${encodeURIComponent(tz)}`),
  portfolio: (orgId: number, since: string, splitBy: string) =>
    get<Portfolio>(`/orgs/${orgId}/portfolio?since=${encodeURIComponent(since)}&split_by=${encodeURIComponent(splitBy)}`),
  handover: (projectId: number, body: { from: string; to: string; task_ids?: number[]; include_in_progress?: boolean; reason?: string }) =>
    post<{ results: { task_id: number; name: string; moved: boolean; error?: string }[] }>(`/projects/${projectId}/handover`, body),
  unmerged: (projectId: number) =>
    get<{ waiting_to_merge: Run[] | null }>(`/projects/${projectId}/unmerged`).then(r => r.waiting_to_merge ?? []),
  recordRun: (taskId: number, run: Partial<Run>) => post<Run>(`/tasks/${taskId}/runs`, run),
  projectTrash: (projectId: number) =>
    get<{ deleted: DeletedTask[] | null; groups?: DeletedGroup[] | null }>(`/projects/${projectId}/trash`)
      .then((r): ProjectTrash => ({ tickets: r.deleted ?? [], groups: r.groups ?? [] })),
  restoreTasks: (projectId: number, taskIds: number[], groups: { epicIds?: number[]; storyIds?: number[] } = {}) =>
    post<{ results: { task_id: number; restored: boolean; error?: string }[]; groups?: GroupRestoreResult[] }>(
      `/projects/${projectId}/trash/restore`, { task_ids: taskIds, epic_ids: groups.epicIds ?? [], story_ids: groups.storyIds ?? [] }),
  getHistory: (taskId: number) =>
    get<{ history: ActivityLog[]; count: number }>(`/tasks/${taskId}/history`).then(r => r.history ?? []),

  // Project activity log (for report dashboard). `limit` caps the rows (the
  // newest N); `signal` cancels the load. Neither is sent when not given.
  getProjectActivity: (projectId: number, since?: string, until?: string, opts: ReadOpts & { limit?: number } = {}) => {
    const params = new URLSearchParams()
    if (since) params.set('since', since)
    if (until) params.set('until', until)
    if (opts.limit != null) params.set('limit', String(opts.limit))
    const qs = params.toString()
    return get<{ activity: ActivityLog[]; count: number }>(
      `/projects/${projectId}/activity${qs ? '?' + qs : ''}`, opts.signal,
    ).then(r => r.activity ?? [])
  },
  // Newest activity time of each running (in_progress) ticket, keyed by ticket
  // id as a string. A running ticket that has logged nothing is absent.
  getLastActivity: (projectId: number, opts: ReadOpts = {}) =>
    get<{ last_activity: Record<string, string> | null }>(`/projects/${projectId}/last-activity`, opts.signal)
      .then(r => r.last_activity ?? {}),

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
  /** A person sends a dispatched ticket no worker has started back to ready
   *  (AMP #2801). The watchdog does it by itself at the project's return
   *  timeout; this is the earlier, knowing version. */
  returnTask: (taskId: number, reason: string) =>
    post<{ task: Task }>(`/tasks/${taskId}/return`, { reason }).then(r => r.task),
  /** Completes an agent ticket; where the project requires a receipt, a
   *  completion below its minimum comes back held (with why) instead. */
  completeTask: (taskId: number) =>
    post<{ task_id: number; state: string; held?: boolean; reason?: string }>(`/tasks/${taskId}/complete`),

  // Verification receipts (#126)
  getTaskReceipt: (taskId: number) => get<TaskReceipt>(`/tasks/${taskId}/receipt`),
  listTaskReceipts: (taskId: number) =>
    get<{ receipts: ReceiptView[] | null }>(`/tasks/${taskId}/receipts`).then(r => r.receipts ?? []),
  /** A person accepts the ticket's latest receipt (a held ticket completes). */
  acceptReceipt: (taskId: number, note?: string) => post<ReceiptView>(`/tasks/${taskId}/receipt/accept`, { note: note ?? '' }),
  /** A person rejects it, with a note (a held ticket goes back to ready). */
  rejectReceipt: (taskId: number, note: string) => post<ReceiptView>(`/tasks/${taskId}/receipt/reject`, { note }),
  receiptSummary: (projectId: number) => get<ReceiptSummary>(`/projects/${projectId}/receipts/summary`),
  setOrgReceiptsMode: (orgId: number, mode: ReceiptsMode) =>
    put<{ mode: ReceiptsMode; org: Org }>(`/orgs/${orgId}/receipts`, { mode }).then(r => r.org),
  /** A person starts their own human ticket. */
  startHumanTask: (taskId: number) =>
    post<{ task: Task }>(`/tasks/${taskId}/start`).then(r => r.task),
  /** A person marks their human ticket done, with what they want on the record. */
  completeHumanTask: (taskId: number, done: { note?: string; pr_url?: string; hours_worked?: number }) =>
    post<{ task: Task }>(`/tasks/${taskId}/complete`, done).then(r => r.task),
  /** What people decided on this ticket's dependencies, and its own decision doc (null without one). */
  getTaskDecisions: (taskId: number) =>
    get<{ upstream: UpstreamDecision[] | null; own: DecisionDoc | null }>(`/tasks/${taskId}/decisions`)
      .then(r => ({ upstream: r.upstream ?? [], own: r.own ?? null })),
  /** A ticket's decision doc with every version and its options (404 without one). */
  getTaskDecision: (taskId: number) =>
    get<DecisionDoc>(`/tasks/${taskId}/decision`),
  /** A person's edit of a decision doc: its markdown and options as one new
   *  version, made on base_version (409 when the doc changed meanwhile). */
  saveDecisionDoc: (taskId: number, body: { base_version: number; content: string; ask: DecisionAsk }) =>
    put<DecisionDoc>(`/tasks/${taskId}/decision/doc`, body),
  /** Record decision: freezes the version, completes the ticket, and posts the decision. */
  recordDecision: (taskId: number, body: { version: number; chosen_option: number | null; decision_text: string }) =>
    post<DecisionDoc>(`/tasks/${taskId}/decision/record`, body),
  /** Answer an agent's flag (a ticket blocked with a reason): posts the
   *  answer as a comment and puts the ticket back to ready (or waiting). */
  /**
   * Answer a question. option chooses one of its options (#130), answer then
   * being a note: its index and the label shown, so the server refuses (409)
   * if the question changed since it was read.
   */
  answerFlag: (taskId: number, answer: string, option?: { index: number; label: string }) =>
    post<{ task: Task }>(`/tasks/${taskId}/unblock`,
      option == null ? { answer } : { answer, option: option.index, option_label: option.label }).then(r => r.task),

  // Export
  exportProject: async (projectId: number): Promise<{ blob: Blob; filename: string }> => {
    const res = await send('GET', `/projects/${projectId}/export`)
    
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
  /** The sidebar and tab badges: what waits on the caller, in total and per project (only projects with something). */
  myWorkCounts: () => get<MyWorkCounts>('/my-work/counts'),
  /** The Get started checklist on an empty My work (#128): each step from server state. */
  gettingStarted: () => get<GettingStarted>('/my-work/getting-started'),
  /** Mission's Waiting on people: each person's counts by kind and ticket ids (any member). */
  waitingOnPeople: (projectId: number) => get<PeopleWaiting>(`/projects/${projectId}/waiting-on-people`),
  /** The project's My work tab: what waits on the caller (or `user`, for a lead), each approve item with its plan. */
  projectMyWork: (projectId: number, opts: { user?: string; tz?: string } = {}) => {
    const q = new URLSearchParams()
    if (opts.user) q.set('user', opts.user)
    q.set('tz', opts.tz ?? Intl.DateTimeFormat().resolvedOptions().timeZone)
    return get<ProjectMyWork>(`/projects/${projectId}/my-work?${q}`)
  },
  /**
   * The Approve view's plan: one or more epics together (one as_of), or
   * 'all' = every epic awaiting the caller's approval, oldest first.
   */
  approvalPlan: (projectId: number, epics: number[] | 'all', opts: { user?: string; offset?: number; limit?: number } = {}) => {
    const q = new URLSearchParams({ approve: epics === 'all' ? 'all' : epics.join(',') })
    if (opts.user) q.set('user', opts.user)
    if (opts.offset) q.set('plan_offset', String(opts.offset))
    if (opts.limit) q.set('plan_limit', String(opts.limit))
    return get<{ plan: ApprovalPlan }>(`/projects/${projectId}/my-work?${q}`).then(r => r.plan)
  },
  /** Mark merged: recorded once; a repeat answers already_recorded with the first record. */
  markMerged: (taskId: number, mergeCommit?: string) =>
    post<MergeRecord>(`/tasks/${taskId}/merge`, mergeCommit ? { merge_commit: mergeCommit } : undefined),
  /** Not needed: the work won't be merged; what waits on it is released. */
  mergeNotNeeded: (taskId: number) => post<MergeRecord>(`/tasks/${taskId}/merge/not-needed`),
  /** Reopen a done ticket (a PR closed without merging): back to ready, or waiting on its dependencies. */
  reopenTask: (taskId: number) => post<{ task: Task }>(`/tasks/${taskId}/reopen`).then(r => r.task),
  listAgents: () => get<{ agents: AmpUser[] }>('/agents').then(r => r.agents),
  createAgent: (name: string, teamIds: number[]) =>
    post<{ agent: AmpUser }>('/agents', { name, team_ids: teamIds }).then(r => r.agent),
  listTokens: (user: number | 'me') => get<{ tokens: APIToken[] | null }>(`/users/${user}/tokens`).then(r => r.tokens ?? []),
  /**
   * Mint a token. `scope` narrows it (AMP #3772): `submit` makes a credential
   * that may only send requests to `scopeTeamId`'s inbox. Omitting it mints a
   * full-scope token, exactly as before.
   *
   * The secret is in the answer and nowhere else — there is no route that
   * returns one (`service.ListTokens` never carries it), so a caller that drops
   * it has to mint another.
   */
  createToken: (user: number | 'me', name: string, expiresInDays = 0, scope?: { scope: TokenScope; scopeTeamId?: number }) =>
    post<{ token: NewAPIToken }>(`/users/${user}/tokens`, {
      name, expires_in_days: expiresInDays,
      ...(scope ? { scope: scope.scope, ...(scope.scopeTeamId != null ? { scope_team_id: scope.scopeTeamId } : {}) } : {}),
    }).then(r => r.token),
  revokeToken: (tokenId: number) => del(`/tokens/${tokenId}`),

  // Audit log: global admins any; org admins pass orgId; project owners projectId.
  // `q.limit` caps the rows; `opts.signal` cancels the load.
  audit: (q: AuditScope & { filter?: AuditFilter; action?: string; before?: number; limit?: number }, opts: ReadOpts = {}) => {
    const f: AuditFilter = { ...q.filter }
    if (q.action && !f.action) f.action = q.action
    const p = auditQuery(q, f, { before: q.before, limit: q.limit })
    return get<{ entries: AuditEntry[] | null }>(`/audit?${p}`, opts.signal).then(r => r.entries ?? [])
  },
  // The hash chain check: org admins pass orgId; global admins may omit it (every chain)
  auditVerify: (orgId?: number) => get<AuditVerifyReport>(auditVerifyUrl(orgId).slice(BASE.length)),

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

  // The project's inbound GitHub merge webhook (project owners). The secret
  // comes back only from rotateGitHubHookSecret, once.
  getGitHubHook: (projectId: number) => get<GitHubHookInfo>(`/projects/${projectId}/github-hook`),
  rotateGitHubHookSecret: (projectId: number) => post<GitHubHookInfo>(`/projects/${projectId}/github-hook/secret`),
  deleteGitHubHook: (projectId: number) => del<{ deleted: boolean }>(`/projects/${projectId}/github-hook`),

  // The project's GitHub API token: write-only (AMP #3155). The answers to
  // every call here are GitHubCredentialInfo, which has no token field, and
  // the token only ever travels in a PUT/POST body — never a URL, which is
  // written to every proxy and access log on the way in.
  getGitHubCredential: (projectId: number, host?: string) =>
    get<GitHubCredentialInfo>(`/projects/${projectId}/github-credential${credentialHostQuery(host)}`),
  attachGitHubCredential: (projectId: number, body: GitHubCredentialChange) =>
    put<GitHubCredentialInfo>(`/projects/${projectId}/github-credential`, body),
  rotateGitHubCredential: (projectId: number, body: GitHubCredentialChange) =>
    post<GitHubCredentialInfo>(`/projects/${projectId}/github-credential/rotate`, body),
  deleteGitHubCredential: (projectId: number, host?: string) =>
    del<{ deleted: boolean; host: string }>(`/projects/${projectId}/github-credential${credentialHostQuery(host)}`),

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

// ---- Team inbox requests (epic 202, story 549; AMP #3977, build slice J0) ----
//
// Thin functions over every shipped request route — nothing here decides
// anything. Who may read what, the tenant, the keyset cursor, the requester
// whitelist and every 404 are the server's decisions; these functions name a
// path, send a body and return the answer's shape. That is deliberate: the
// server's adapters are thin over one authorized service path each, and a
// client that re-implemented a rule would be a second place for it to drift.
//
// Three facts about this surface are easy to get wrong, so they are stated once
// here instead of in each call site's comment:
//
//  1. **A write addresses a request by its SERIAL; a read accepts either the
//     serial or the public reference.** The team routes are under a team
//     (`/teams/{id}/requests/{serial}`) because the service loads the team first
//     for the org half of the tenant; a public reference on a team write is
//     refused by name rather than looked up, so a second definition of what a
//     reference may name cannot appear in an adapter. The exception is the
//     requester surface (`/requests/{ref}/…`), where the reference is the only
//     thing a customer holds — that is why those calls take `request: number |
//     string` and the team ones take `number`.
//
//  2. **A write answers the act, never the record** (see `RequestMutationFacts`).
//     A component that shows a claim or an assignee after a write must re-read
//     the record; the write answer does not carry them.
//
//  3. **A 409 carries the facts a retry needs**, and a 429 carries its wait, on
//     the thrown `HttpError`: `code` (`request.conflict`), `rev` (the revision
//     the row is actually at) and `claimUserId` (who holds the claim), or
//     `retryAfterS`. Nothing here swallows them.

/**
 * The filters a queue call may carry, as a query string. Repeated `state`
 * values are the server's own spelling (`?state=new&state=triaging`), which it
 * also accepts comma-separated; this writes the repeated form.
 */
function requestQueueQuery(filter: RequestQueueFilter = {}): string {
  const q = new URLSearchParams()
  for (const st of filter.state ?? []) q.append('state', st)
  if (filter.submitter !== undefined) q.set('submitter', String(filter.submitter))
  if (filter.source_kind) q.set('source_kind', filter.source_kind)
  if (filter.created_after) q.set('created_after', filter.created_after)
  if (filter.created_before) q.set('created_before', filter.created_before)
  if (filter.attached !== undefined) q.set('attached', String(filter.attached))
  if (filter.q) q.set('q', filter.q)
  if (filter.before_id) q.set('before_id', String(filter.before_id))
  if (filter.limit) q.set('limit', String(filter.limit))
  if (filter.mine) q.set('mine', 'true')
  if (filter.mine_before_id) q.set('mine_before_id', String(filter.mine_before_id))
  if (filter.mine_limit) q.set('mine_limit', String(filter.mine_limit))
  const s = q.toString()
  return s ? `?${s}` : ''
}

/** The requester's own-list query: smaller than the queue's, by the server's own rule. */
function ownRequestQuery(filter: OwnRequestFilter = {}): string {
  const q = new URLSearchParams()
  for (const st of filter.state ?? []) q.append('state', st)
  if (filter.before_id) q.set('before_id', String(filter.before_id))
  if (filter.limit) q.set('limit', String(filter.limit))
  const s = q.toString()
  return s ? `?${s}` : ''
}

/**
 * The forward cursor a discussion or history read carries (`?after_id=N&limit=N`).
 *
 * Forward, not `before_id`, because both tables are append-only: a thread and a
 * history are read from where they began. The server clamps `limit` itself, so
 * this writes whatever the caller asked for and never re-derives the bound — a
 * second copy of the ceiling is a second thing to get out of step with it.
 * Zero means "unset" in both spellings, as on every other query here.
 */
function requestReadPageQuery(opts: RequestReadPageOpts = {}): string {
  const q = new URLSearchParams()
  if (opts.after_id) q.set('after_id', String(opts.after_id))
  if (opts.limit) q.set('limit', String(opts.limit))
  const s = q.toString()
  return s ? `?${s}` : ''
}

/**
 * The lineage read's `kind` filter, in the repeated form the server also
 * accepts (`?kind=planned_as&kind=delivered_by`) — the queue's `state`
 * convention, so one spelling is written here and the comma form is the
 * server's business. No filter writes no query at all.
 */
function requestLinksQuery(kind?: RequestLinkKind | readonly RequestLinkKind[]): string {
  const kinds: readonly RequestLinkKind[] = kind == null ? [] : typeof kind === 'string' ? [kind] : kind
  if (kinds.length === 0) return ''
  const q = new URLSearchParams()
  for (const k of kinds) q.append('kind', k)
  return `?${q.toString()}`
}

/**
 * A request write's precondition: the revision the caller read, in whichever of
 * the two spellings the caller chose.
 *
 * The server accepts `If-Match: "7"` and `If-Match: 7` (both mean the same), and
 * it refuses `If-Match: *` and weak validators — they claim a guarantee HTTP
 * does not give a strong comparison. `expected_rev` in the body is the same
 * precondition; sending both is allowed only when they agree. This helper
 * prefers the header and never invents a revision, so an omitted `rev` stays
 * omitted and the act stays replay-tolerant.
 */
function preconditionHeaders(rev?: number): Record<string, string> | undefined {
  return rev !== undefined && rev > 0 ? { 'If-Match': `"${rev}"` } : undefined
}

/**
 * The team inbox surface: the shared queue, one request's record, triage,
 * lifecycle, lineage, attachments and the access administration.
 *
 * Every function names the route it calls, so a reader can match it against
 * `internal/api/rest.go`'s registrations without opening the server. The route
 * list is deliberately complete: a UI slice that needs a call should find it
 * here rather than adding one beside its component, which is what keeps this
 * file the single place the surface is described.
 */
export const requestApi = {
  // ---- Submission (F1's submit half) ----
  /**
   * Submit a request to a team's inbox, with no project named.
   *
   * `idempotencyKey` is the caller's retry token and goes in the
   * `Idempotency-Key` header, never in the body: a body field for it could be
   * read as content, and the key's scope is `(org, team, source, submitter)`, so
   * a browser's key and an API client's key are different keys.
   *
   * `created` is false when this call answered a replay with the request the key
   * already produced (the server answers 200 rather than 201 in that case).
   */
  submit: (teamId: number, input: SubmitRequestInput, idempotencyKey?: string) =>
    post<RequestSubmissionAnswer>(`/teams/${teamId}/requests`, input,
      idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : undefined),

  // ---- Reads (F1's list half and F2's read half) ----
  /** The team's queue: one keyset page, its per-state counts, and (when asked) the caller's own slice. */
  queue: (teamId: number, filter?: RequestQueueFilter) =>
    get<RequestQueue>(`/teams/${teamId}/requests${requestQueueQuery(filter)}`),
  /** The same counts without a page — My work's inbox panels read this. */
  queueSummary: (teamId: number, filter?: Omit<RequestQueueFilter, 'before_id' | 'limit'>) =>
    get<RequestQueueSummary>(`/teams/${teamId}/requests/summary${requestQueueQuery(filter as RequestQueueFilter)}`),
  /** One request's team view: the stored record plus its delivery aggregate. `ref` is a serial or a public reference. */
  detail: (teamId: number, ref: number | string) =>
    get<RequestReadAnswer>(`/teams/${teamId}/requests/${encodeURIComponent(String(ref))}`),
  /** The caller's own requests, projected — never the record, and never another requester's. */
  own: (filter?: OwnRequestFilter) => get<OwnRequestPage>(`/requests${ownRequestQuery(filter)}`),
  /** One of the caller's own requests. Another person's reference is the same 404 as an unknown one. */
  ownDetail: (ref: number | string) =>
    get<RequestReadAnswer>(`/requests/${encodeURIComponent(String(ref))}`),

  // ---- The record's three sub-reads (F2's discussion, F3's history, F5's lineage) ----
  /**
   * One request's discussion: every stored row, in append order.
   *
   * `ref` is a serial or the requester's public reference, as on every request
   * read (the server decodes it with the detail read's own decoder).
   *
   * The answer is returned whole, not unwrapped to one array, because the two
   * arms are two different lists: a team answer carries `comments` (the stored
   * `RequestComment` rows, attribution included) while a requester answer carries
   * `shared_comments` (`RequesterComment`: no id, no author, no user id) and
   * **omits `comments` entirely**. Returning `comments ?? []` would hand a
   * requester an empty thread where they have a real one — the bug
   * `servedReads` in `lib/requestDetail.ts` exists to prevent — so narrow on
   * `view` and the right list follows.
   *
   * `opts` is the forward cursor. The thread is an append-only table read from
   * where it began, so there is no `before_id`; the server clamps `limit` (50
   * default, 200 ceiling) and answers `next_after_id` only when another page
   * really exists, so a caller never asks for an empty page to learn it is done.
   * A requester's answer carries no cursor at all.
   */
  listComments: (teamId: number, ref: number | string, opts?: RequestReadPageOpts) =>
    get<RequestThreadAnswer>(`/teams/${teamId}/requests/${encodeURIComponent(String(ref))}/comments${requestReadPageQuery(opts)}`),
  /**
   * One request's history: the append-only `request_events` rows, in id order.
   *
   * One view, one list. The history is an internal fact — a row names the acting
   * person, the internal state names and the ids of what it touched — so the
   * server refuses a requester here (404, the surface's one answer) rather than
   * filtering; a page that rendered this for a requester would be inventing a
   * whitelist nothing consumes.
   *
   * Returned as its page, so `next_after_id` reaches the caller and a long
   * history is a caller's decision rather than a silent truncation at the
   * server's 50-row default. `events` is the existing `RequestEvent`.
   */
  listEvents: (teamId: number, ref: number | string, opts?: RequestReadPageOpts) =>
    get<RequestEventsAnswer>(`/teams/${teamId}/requests/${encodeURIComponent(String(ref))}/events${requestReadPageQuery(opts)}`),
  /**
   * One request's linked work: the `request_links` rows in attach order, so the
   * planning rail and the deliver panel's evidence list read the lineage itself
   * rather than reconstructing it from the outcome aggregate.
   *
   * `kind` narrows the answer to one kind, or to a union of them (the server
   * takes the repeated form this writes, and also a comma list). An unknown kind
   * is a 400 `request.invalid`.
   *
   * Rows, not a page: a request's lineage is bounded by what planning and
   * delivery attached, so there is no cursor and nothing to truncate. Each row is
   * the rail's own `ResolvedRequestLink` — `RequestLink` plus an OPTIONAL
   * `target`, which this answer never carries: the server does not resolve a
   * target, and an absent one is the truthful state rather than a placeholder.
   * The page fills a title (or `gone`/`approved`/`merged`) from the target's own
   * read.
   */
  listLinks: (teamId: number, ref: number | string, kind?: RequestLinkKind | readonly RequestLinkKind[]): Promise<ResolvedRequestLink[]> =>
    get<RequestLinksAnswer>(`/teams/${teamId}/requests/${encodeURIComponent(String(ref))}/links${requestLinksQuery(kind)}`)
      .then(r => r.links ?? []),

  // ---- Triage writes (F2's write half) ----
  /** The submitter's versioned edit, while the request is still untriaged. `expected_rev` is required. */
  update: (teamId: number, requestId: number, input: UpdateRequestInput) =>
    patch<RequestMutationAnswer>(`/teams/${teamId}/requests/${requestId}`, input,
      preconditionHeaders(input.expected_rev)),
  /** One row of discussion in the caller's own voice. 201 when it wrote, 200 when it answered a retry. */
  comment: (teamId: number, requestId: number, input: RequestDiscussionInput) =>
    post<RequestCommentAnswer>(`/teams/${teamId}/requests/${requestId}/comments`, input),
  /** Take the triage claim. A lost race is a 409 carrying `claimUserId` — the winner. */
  claim: (teamId: number, requestId: number, input?: RequestPreconditionInput) =>
    post<RequestMutationAnswer>(`/teams/${teamId}/requests/${requestId}/claim`, input ?? {},
      preconditionHeaders(input?.expected_rev)),
  /** Give the claim back. Idempotent: releasing a claim nobody holds is `changed: false`. */
  release: (teamId: number, requestId: number, input?: RequestPreconditionInput) =>
    post<RequestMutationAnswer>(`/teams/${teamId}/requests/${requestId}/release`, input ?? {},
      preconditionHeaders(input?.expected_rev)),
  /** A team manager's explicit transfer to an assignee ('me', an id, an email, or 'none'). Clears any claim. */
  assign: (teamId: number, requestId: number, input: RequestAssignInput) =>
    post<RequestMutationAnswer>(`/teams/${teamId}/requests/${requestId}/assign`, input,
      preconditionHeaders(input.expected_rev)),

  // ---- Team lifecycle (F3) ----
  /** The plain working step — the one generic route, to whichever state the caller names. */
  transition: (teamId: number, requestId: number, input: RequestLifecycleInput) =>
    post<RequestLifecycleAnswer>(`/teams/${teamId}/requests/${requestId}/transition`, input,
      preconditionHeaders(input.expected_rev)),
  /** Ask the requester a question; the request moves to needs_information (I5). */
  needsInfo: (teamId: number, requestId: number, question: string, expectedRev?: number) =>
    post<RequestLifecycleAnswer>(`/teams/${teamId}/requests/${requestId}/needs-info`, { question, expected_rev: expectedRev },
      preconditionHeaders(expectedRev)),
  /** Decline, with a reason (I3). */
  reject: (teamId: number, requestId: number, rejectReason: string, expectedRev?: number) =>
    post<RequestLifecycleAnswer>(`/teams/${teamId}/requests/${requestId}/reject`, { reject_reason: rejectReason, expected_rev: expectedRev },
      preconditionHeaders(expectedRev)),
  /** Fold into a canonical request (I3/D7). */
  duplicate: (teamId: number, requestId: number, canonicalRequestId: number, expectedRev?: number) =>
    post<RequestLifecycleAnswer>(`/teams/${teamId}/requests/${requestId}/duplicate`, { canonical_request_id: canonicalRequestId, expected_rev: expectedRev },
      preconditionHeaders(expectedRev)),
  /** The team reopens a closed request (D6). */
  reopen: (teamId: number, requestId: number, input?: RequestPreconditionInput) =>
    post<RequestLifecycleAnswer>(`/teams/${teamId}/requests/${requestId}/reopen`, input ?? {},
      preconditionHeaders(input?.expected_rev)),
  /**
   * State that the outcome exists (I2).
   *
   * The server refuses a delivery whose evidence is missing before the lifecycle
   * write runs, and its refusal names the request's public reference — so a
   * Deliver panel disables its button on `outcome.delivered_eligible` (the
   * aggregate's gate) rather than trying and translating the error.
   */
  deliver: (teamId: number, requestId: number, input: RequestLifecycleInput) =>
    post<RequestLifecycleAnswer>(`/teams/${teamId}/requests/${requestId}/deliver`, input,
      preconditionHeaders(input.expected_rev)),

  // ---- The requester's own acts (F4) ----
  /** Answer the desk's question. The destination is the state the question interrupted, never the caller's choice. */
  answer: (ref: number | string, input: RequesterAnswerInput) =>
    post<RequestLifecycleAnswer>(`/requests/${encodeURIComponent(String(ref))}/answer`, input,
      preconditionHeaders(input.expected_rev)),
  /**
   * Accept a delivery — the one act no team member, lead, agent, token or
   * connector may perform (I1/D5).
   *
   * The server refuses a non-person caller before it resolves the reference, so
   * an agent or a service credential gets identical bytes for a real, a foreign
   * and an unknown reference. There is no on-behalf path in v1, and this client
   * adds none.
   */
  accept: (ref: number | string, input?: RequestPreconditionInput) =>
    post<RequestLifecycleAnswer>(`/requests/${encodeURIComponent(String(ref))}/accept`, input ?? {},
      preconditionHeaders(input?.expected_rev)),
  /** The requester reopens after a delivery (D6). */
  ownReopen: (ref: number | string, input?: RequestPreconditionInput) =>
    post<RequestLifecycleAnswer>(`/requests/${encodeURIComponent(String(ref))}/reopen`, input ?? {},
      preconditionHeaders(input?.expected_rev)),

  // ---- Lineage and the planning handoff (F5) ----
  /**
   * Attach one durable relation. The relation is its own idempotency key: a
   * replay answers `changed: false` and writes no row, history entry or audit.
   *
   * A second active `routes_to` is a 409 naming the project the request is on
   * now; `supersede_route` moves it deliberately, appending the new routing and
   * keeping the old one as history.
   */
  link: (teamId: number, requestId: number, input: RequestLinkInput) =>
    post<RequestLinkAnswer>(`/teams/${teamId}/requests/${requestId}/links`, input),
  /**
   * Plan a batch of tickets for the request, durably and idempotently.
   *
   * The created tickets await a person's approval: nothing on this path approves
   * or dispatches anything. A retried partial batch plans only what is missing
   * and reports the rest as `already`.
   */
  handoff: (teamId: number, requestId: number, input: RequestHandoffInput) =>
    post<RequestHandoffAnswer>(`/teams/${teamId}/requests/${requestId}/handoff`, input),

  // ---- Private attachments (F5/G3) ----
  /** The file panel: metadata only, never bytes, plus AMP's upload bounds. */
  attachments: (teamId: number, requestId: number) =>
    get<RequestAttachmentPanel>(`/teams/${teamId}/requests/${requestId}/attachments`),
  /** One file onto the request. 201 always: an upload is not idempotent, so there is no replay to answer. */
  uploadAttachment: (teamId: number, requestId: number, input: RequestUploadInput) =>
    post<RequestUploadAnswer>(`/teams/${teamId}/requests/${requestId}/attachments`, input),
  /** Pin a file past its retention window, or let the request's own clock apply again. */
  keepAttachment: (teamId: number, requestId: number, attachmentId: number, keep: boolean) =>
    post<RequestAttachmentKeepAnswer>(`/teams/${teamId}/requests/${requestId}/attachments/${attachmentId}/keep`, { keep }),
  /** Delete a file. A second delete is a 404, not `{deleted: false}` — the row is read before it is removed. */
  deleteAttachment: (teamId: number, requestId: number, attachmentId: number) =>
    del<RequestAttachmentDeleteAnswer>(`/teams/${teamId}/requests/${requestId}/attachments/${attachmentId}`),
  /**
   * Mint a short-lived, single-use download link for one file.
   *
   * Minting is one thing and serving the bytes is another: the link is an
   * additional capability, never a way around signing in, and it is the only way
   * a private file's bytes are served.
   */
  downloadLink: (teamId: number, requestId: number, attachmentId: number) =>
    post<{ download: RequestDownloadLink }>(
      `/teams/${teamId}/requests/${requestId}/attachments/${attachmentId}/download`,
    ).then(r => r.download),

  // ---- Who may submit to this inbox (D14) and requester invitations ----
  /** The team's submit grants: in force first, then revoked. */
  listGrants: (teamId: number) =>
    get<RequestGrantList>(`/teams/${teamId}/request-grants`).then(r => r.grants ?? []),
  /** Grant submit access to 'me', a user id or an email. Idempotent, so there is no `changed`. */
  grantSubmit: (teamId: number, user: string | number) =>
    post<RequestGrantAnswer>(`/teams/${teamId}/request-grants`, { user: String(user) }),
  /** Revoke it. A revoked grant fails the very next check, with no cache to clear. */
  revokeSubmit: (teamId: number, user: string | number) =>
    del<RequestGrantAnswer>(`/teams/${teamId}/request-grants/${encodeURIComponent(String(user))}`),
  /** The inbox's pending requester invitations. */
  listInvites: (teamId: number) =>
    get<RequestInviteList>(`/teams/${teamId}/request-invites`).then(r => r.invites ?? []),
  /** Invite someone as a requester: it provisions a customer identity and grants no role at all. */
  inviteRequester: (teamId: number, email: string) =>
    post<RequestInviteAnswer>(`/teams/${teamId}/request-invites`, { email }),
  revokeInvite: (teamId: number, inviteId: number) =>
    del<{ team_id: number; invite_id: number; revoked: boolean }>(`/teams/${teamId}/request-invites/${inviteId}`),
}

/**
 * A 409 on this surface, as the facts a caller retries with.
 *
 * Every conflict the inbox answers — a stale revision, a lost claim race, a
 * refused re-route, a duplicate that cannot be folded — is one status and one
 * code, and what differs is which extra fact the body carries: `rev` for a
 * stale write, `claimUserId` for a lost claim. A caller that needs to tell them
 * apart reads the body's message; a caller that needs to *act* reads these.
 */
export function requestConflict(err: unknown): { rev?: number; claimUserId?: number; code?: string } | null {
  const e = err as HttpError | undefined
  if (!e || e.status !== 409) return null
  return { rev: e.rev, claimUserId: e.claimUserId, code: e.code }
}

/**
 * A 429's wait in seconds, from either spelling the server writes it in.
 *
 * The submission route puts the same number in `Retry-After` and in
 * `retry_after_s`, and `send` reads both, so this is the one place a caller
 * looks. Used by the submit form's "try again in Ns" copy.
 */
export function requestRetryAfterS(err: unknown): number | null {
  const e = err as HttpError | undefined
  return e?.status === 429 && typeof e.retryAfterS === 'number' ? e.retryAfterS : null
}

// ---- amp-authadmin — Dex credential CRUD (routed via Envoy at /authadmin/*) ----
const AUTHADMIN_BASE = '/authadmin'

async function authadminReq<T>(path: string, method: string, body?: unknown): Promise<T> {
  return send(method, path, body, AUTHADMIN_BASE).then(r => r.json())
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
