// The team-inbox API client and its DTOs (AMP #3977, build slice J0).
//
// This file proves the three things the ticket's acceptance criteria name, one
// describe each:
//
//  1. **Endpoint coverage.** Every shipped request route is called at the path
//     the server registers (internal/api/rest.go) with the verb it registers —
//     so a route that moved, or a client function that drifted to a neighbouring
//     path, fails here rather than in a page nobody rendered yet.
//  2. **DTO separation.** The two views are two types: the team record carries
//     the internal fields (org, claim, assignee, contact, rev, metadata) and the
//     requester projection has `type`-level no room for any of them. The
//     assertion is on the types themselves, not on a particular fixture, because
//     the point is that a field cannot be added by accident.
//  3. **Client error/retry mapping.** A 409's `code`/`rev`/`claimUserId` and a
//     429's `retryAfterS` reach the caller, from either spelling the server
//     writes them in, and `requestConflict`/`requestRetryAfterS` read them back.
//
// It is a client test: no components, no storage. Nothing here writes a token or
// looks at localStorage (the ticket's "no token storage" criterion is that this
// slice adds none — `send` still sends no credential, which the last describe
// asserts by looking at the request's headers).
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import type {
  RequesterRequestView, TeamRequest, RequestReadAnswer, RequestQueue, OwnRequestPage,
  RequestComment, RequestEvent, RequestLink, RequestThreadAnswer, RequestEventsAnswer, RequestLinksAnswer,
} from '../types'
import { requestApi, requestConflict, requestRetryAfterS, type HttpError } from './client'

/** Every request a test made, so a test can assert the verb, path, headers and body. */
interface Call { method: string; path: string; url: string; headers: Headers; body: string }
let calls: Call[] = []

/** Record the call and answer it with `answer`. */
function route(method: string, pattern: string, answer: unknown = {}, status = 200) {
  return http[method as 'get'](pattern, async ({ request }) => {
    calls.push({
      method: request.method,
      path: new URL(request.url).pathname,
      url: request.url,
      headers: request.headers,
      body: await request.text(),
    })
    return HttpResponse.json(answer as Record<string, unknown>, { status })
  })
}

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = [] })
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

/** A team-side record with every internal field populated — nothing here may reach a requester. */
const record: TeamRequest = {
  id: 7,
  public_ref: 'a1b2c3d4e5f6g7h8i9j0',
  org_id: 2,
  team_id: 4,
  requester_user_id: 31,
  requester_contact: 'sam@outside.example',
  source_kind: 'web',
  source_external_id: '',
  subject: 'The export is missing November',
  input_brief: 'November never appeared in the nightly export.',
  outcome_brief: '',
  state: 'triaging',
  claim_user_id: 12,
  claimed_at: '2026-11-13T10:00:00Z',
  owner_user_id: 12,
  duplicate_of: null,
  reject_reason: '',
  needs_info_since: null,
  planned_at: null,
  delivered_at: null,
  accepted_at: null,
  accepted_by: null,
  closed_at: null,
  reopened_at: null,
  reopened_count: 0,
  rev: 3,
  metadata: { vendor: 'zendesk' },
  created_at: '2026-11-12T09:00:00Z',
  updated_at: '2026-11-13T10:00:00Z',
}

/** The requester's projection of the same request: no internal field exists on the type. */
const projection: RequesterRequestView = {
  public_ref: 'a1b2c3d4e5f6g7h8i9j0',
  state: 'triaging',
  state_word: 'Being reviewed',
  subject: 'The export is missing November',
  input_brief: 'November never appeared in the nightly export.',
  team_name: 'Northwind support desk',
  created_at: '2026-11-12T09:00:00Z',
  milestones: [{ key: 'received', done: true, at: '2026-11-12T09:00:00Z' }, { key: 'reviewing', done: true }],
  comments: [{ kind: 'comment', from: 'desk', body: 'Looking at it now.', at: '2026-11-13T10:00:00Z' }],
  outputs: [{ source: 'you', name: 'november.csv', size_bytes: 2048, status: 'preparing' }],
}

const teamAnswer: RequestReadAnswer = {
  view: 'team',
  request: record,
  outcome: {
    phase: 'in_progress',
    items: [{ kind: 'task', id: 21, state: 'open' }],
    total: 1, done: 0, open: 1, blocked: 0, gated: 0, held: 0, deleted: 0, unverified: 0, rework: 0,
    linked: 1, routed_project_id: 3, outcome_brief_set: false, no_artifact: false,
    delivered_eligible: false, blocked_reason: 'the request is not delivered yet',
  },
}

const requesterAnswer: RequestReadAnswer = { view: 'requester', projection }

const queue: RequestQueue = {
  items: [record],
  count: 1,
  totals: {
    total: 1,
    states: {
      new: 0, triaging: 1, needs_information: 0, accepted: 0, in_progress: 0,
      delivered: 0, requester_accepted: 0, rejected: 0, duplicate: 0,
    },
  },
}

describe('endpoint coverage: every function calls the route the server registers', () => {
  it('the reads', async () => {
    server.use(
      route('get', '*/api/teams/4/requests', queue),
      route('get', '*/api/teams/4/requests/summary', { totals: queue.totals }),
      route('get', '*/api/teams/4/requests/7', teamAnswer),
      route('get', '*/api/requests', { items: [projection], count: 1, total: 1 } satisfies OwnRequestPage),
      route('get', '*/api/requests/a1b2c3d4e5f6g7h8i9j0', requesterAnswer),
    )
    await requestApi.queue(4)
    await requestApi.queueSummary(4)
    await requestApi.detail(4, 7)
    await requestApi.own()
    await requestApi.ownDetail('a1b2c3d4e5f6g7h8i9j0')
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual([
      'GET /api/teams/4/requests',
      'GET /api/teams/4/requests/summary',
      'GET /api/teams/4/requests/7',
      'GET /api/requests',
      'GET /api/requests/a1b2c3d4e5f6g7h8i9j0',
    ])
  })

  it('the queue filter reaches the server as the query the adapter parses', async () => {
    server.use(route('get', '*/api/teams/4/requests', queue))
    await requestApi.queue(4, {
      state: ['new', 'triaging'], submitter: 'me', source_kind: 'web', attached: true,
      q: 'export', before_id: 90, limit: 25, mine: true, mine_limit: 5,
    })
    const q = new URL(calls[0].url).searchParams
    // Repeated state values, which the server's requestQueryStates reads (it also
    // accepts a comma list; this client writes the repeated form).
    expect(q.getAll('state')).toEqual(['new', 'triaging'])
    expect(q.get('submitter')).toBe('me')
    expect(q.get('source_kind')).toBe('web')
    expect(q.get('attached')).toBe('true')
    expect(q.get('q')).toBe('export')
    expect(q.get('before_id')).toBe('90')
    expect(q.get('limit')).toBe('25')
    expect(q.get('mine')).toBe('true')
    expect(q.get('mine_limit')).toBe('5')
  })

  it('the submission carries its retry key as Idempotency-Key, never in the body', async () => {
    server.use(route('post', '*/api/teams/4/requests', { request: { public_ref: 'a1b2c3d4e5f6g7h8i9j0', state: 'new' }, created: true }, 201))
    const answer = await requestApi.submit(4, { subject: 'Export is short', input_brief: 'November is missing.' }, 'submit-1')
    expect(answer.created).toBe(true)
    expect(calls[0].headers.get('idempotency-key')).toBe('submit-1')
    expect(JSON.parse(calls[0].body)).toEqual({ subject: 'Export is short', input_brief: 'November is missing.' })
  })

  it('the triage writes', async () => {
    const written = { request: { id: 7, public_ref: record.public_ref, state: 'triaging', rev: 4 }, changed: true }
    server.use(
      route('patch', '*/api/teams/4/requests/7', written),
      route('post', '*/api/teams/4/requests/7/comments', { comment: { id: 11, kind: 'comment', body: 'ok', created_at: '2026-11-13T11:00:00Z' }, wrote: true }, 201),
      route('post', '*/api/teams/4/requests/7/claim', written),
      route('post', '*/api/teams/4/requests/7/release', { ...written, changed: false }),
      route('post', '*/api/teams/4/requests/7/assign', written),
    )
    await requestApi.update(4, 7, { subject: 'Export is short', expected_rev: 3 })
    await requestApi.comment(4, 7, { kind: 'comment', body: 'ok' })
    await requestApi.claim(4, 7, { expected_rev: 3 })
    await requestApi.release(4, 7)
    await requestApi.assign(4, 7, { assignee: 'me', expected_rev: 4 })
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual([
      'PATCH /api/teams/4/requests/7',
      'POST /api/teams/4/requests/7/comments',
      'POST /api/teams/4/requests/7/claim',
      'POST /api/teams/4/requests/7/release',
      'POST /api/teams/4/requests/7/assign',
    ])
    // The precondition travels as If-Match (the strong spelling the server accepts)
    // and also in the body where the route's own body type has the field.
    expect(calls[0].headers.get('if-match')).toBe('"3"')
    expect(JSON.parse(calls[3].body)).toEqual({})
    expect(calls[3].headers.get('if-match')).toBeNull()
  })

  it('the team lifecycle routes', async () => {
    const acted = { request: { id: 7, public_ref: record.public_ref, state: 'rejected', rev: 5 }, changed: true }
    server.use(
      route('post', '*/api/teams/4/requests/7/transition', acted),
      route('post', '*/api/teams/4/requests/7/needs-info', acted),
      route('post', '*/api/teams/4/requests/7/reject', acted),
      route('post', '*/api/teams/4/requests/7/duplicate', acted),
      route('post', '*/api/teams/4/requests/7/reopen', acted),
      route('post', '*/api/teams/4/requests/7/deliver', acted),
    )
    await requestApi.transition(4, 7, { to: 'in_progress' })
    await requestApi.needsInfo(4, 7, 'Which export?', 4)
    await requestApi.reject(4, 7, 'Duplicate of a live request', 4)
    await requestApi.duplicate(4, 7, 9, 4)
    await requestApi.reopen(4, 7)
    await requestApi.deliver(4, 7, { outcome_brief: 'Re-ran November.', no_artifact: false })
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual([
      'POST /api/teams/4/requests/7/transition',
      'POST /api/teams/4/requests/7/needs-info',
      'POST /api/teams/4/requests/7/reject',
      'POST /api/teams/4/requests/7/duplicate',
      'POST /api/teams/4/requests/7/reopen',
      'POST /api/teams/4/requests/7/deliver',
    ])
    // The named routes send their own evidence field and no `to` — the server
    // refuses a `to` on them, because the destination is the route's.
    expect(JSON.parse(calls[1].body)).toEqual({ question: 'Which export?', expected_rev: 4 })
    expect(JSON.parse(calls[2].body)).toEqual({ reject_reason: 'Duplicate of a live request', expected_rev: 4 })
    expect(JSON.parse(calls[3].body)).toEqual({ canonical_request_id: 9, expected_rev: 4 })
  })

  it('the requester surface addresses a request by its reference, not a team+serial', async () => {
    const acted = { request: { id: 7, public_ref: record.public_ref, state: 'requester_accepted', rev: 6 }, changed: true }
    const ref = 'a1b2c3d4e5f6g7h8i9j0'
    server.use(
      route('post', `*/api/requests/${ref}/answer`, acted),
      route('post', `*/api/requests/${ref}/accept`, acted),
      route('post', `*/api/requests/${ref}/reopen`, acted),
    )
    await requestApi.answer(ref, { answer: 'November, in the nightly job.', answer_to: 11 })
    await requestApi.accept(ref)
    await requestApi.ownReopen(ref, { expected_rev: 5 })
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual([
      `POST /api/requests/${ref}/answer`,
      `POST /api/requests/${ref}/accept`,
      `POST /api/requests/${ref}/reopen`,
    ])
    expect(JSON.parse(calls[0].body)).toEqual({ answer: 'November, in the nightly job.', answer_to: 11 })
  })

  it('lineage, the handoff and the attachment panel', async () => {
    server.use(
      route('post', '*/api/teams/4/requests/7/links', { link: { id: 3, request_id: 7, link_kind: 'routes_to', target_kind: 'project', target_id: 3, created_by: 1, created_at: '2026-11-13T11:00:00Z' }, changed: true }),
      route('post', '*/api/teams/4/requests/7/handoff', { project_id: 3, items: [], created: 0, already: 0 }),
      route('get', '*/api/teams/4/requests/7/attachments', { request_id: 7, public_ref: record.public_ref, items: [], count: 0, limits: { max_file_bytes: 26214400, max_files: 5, max_total_bytes: 62914560, retention_days: 90 } }),
      route('post', '*/api/teams/4/requests/7/attachments', { attachment: { id: 5 }, held: false, next: 'stored' }, 201),
      route('post', '*/api/teams/4/requests/7/attachments/5/keep', { attachment: { id: 5 }, keep: true }),
      route('delete', '*/api/teams/4/requests/7/attachments/5', { deleted: true, attachment_id: 5 }),
      route('post', '*/api/teams/4/requests/7/attachments/5/download', { download: { url: '/api/request-downloads/tok', expires_at: '2026-11-13T12:00:00Z', attachment_id: 5, request_id: 7, file_name: 'november.csv', size_bytes: 2048 } }),
    )
    await requestApi.link(4, 7, { link_kind: 'routes_to', target_kind: 'project', target_id: 3 })
    await requestApi.handoff(4, 7, { handoff_key: 'plan-1', items: [{ ref: 'fix', story_id: 2, name: 'Re-run November', plan: 'p', acceptance_criteria: 'a' }] })
    await requestApi.attachments(4, 7)
    await requestApi.uploadAttachment(4, 7, { file_name: 'november.csv', content_base64: 'bm92ZW1iZXI=' })
    await requestApi.keepAttachment(4, 7, 5, true)
    await requestApi.deleteAttachment(4, 7, 5)
    const link = await requestApi.downloadLink(4, 7, 5)
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual([
      'POST /api/teams/4/requests/7/links',
      'POST /api/teams/4/requests/7/handoff',
      'GET /api/teams/4/requests/7/attachments',
      'POST /api/teams/4/requests/7/attachments',
      'POST /api/teams/4/requests/7/attachments/5/keep',
      'DELETE /api/teams/4/requests/7/attachments/5',
      'POST /api/teams/4/requests/7/attachments/5/download',
    ])
    // The mint answers `{download: {...}}` and the client unwraps it, because
    // every caller wants the link, not the envelope.
    expect(link.url).toBe('/api/request-downloads/tok')
    expect(link.attachment_id).toBe(5)
  })

  it('the access administration', async () => {
    server.use(
      route('get', '*/api/teams/4/request-grants', { team_id: 4, grants: [{ team_id: 4, org_id: 2, user_id: 31, granted_at: '2026-11-13T10:00:00Z', email: 'sam@outside.example' }] }),
      route('post', '*/api/teams/4/request-grants', { team_id: 4, grant: { team_id: 4, org_id: 2, user_id: 31, granted_at: '2026-11-13T10:00:00Z' }, active: true }),
      route('delete', '*/api/teams/4/request-grants/31', { team_id: 4, grant: { team_id: 4, org_id: 2, user_id: 31, granted_at: '2026-11-13T10:00:00Z', revoked_at: '2026-11-13T11:00:00Z' }, active: false }),
      route('get', '*/api/teams/4/request-invites', { team_id: 4, invites: [] }),
      route('post', '*/api/teams/4/request-invites', { team_id: 4, invite: { id: 8, email: 'sam@outside.example' } }, 201),
      route('delete', '*/api/teams/4/request-invites/8', { team_id: 4, invite_id: 8, revoked: true }),
    )
    expect(await requestApi.listGrants(4)).toHaveLength(1)
    expect((await requestApi.grantSubmit(4, 'sam@outside.example')).active).toBe(true)
    expect((await requestApi.revokeSubmit(4, 31)).active).toBe(false)
    expect(await requestApi.listInvites(4)).toEqual([])
    expect((await requestApi.inviteRequester(4, 'sam@outside.example')).invite.id).toBe(8)
    expect((await requestApi.revokeInvite(4, 8)).revoked).toBe(true)
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual([
      'GET /api/teams/4/request-grants',
      'POST /api/teams/4/request-grants',
      'DELETE /api/teams/4/request-grants/31',
      'GET /api/teams/4/request-invites',
      'POST /api/teams/4/request-invites',
      'DELETE /api/teams/4/request-invites/8',
    ])
    // A grant names the person and nothing else: the route refuses a role.
    expect(JSON.parse(calls[1].body)).toEqual({ user: 'sam@outside.example' })
  })
})

// ---- The record's three sub-reads: discussion, history, lineage (AMP #4273) ----
//
// These are the reads the detail page's panels were waiting on (#4270/#4271/#4272 on
// the server). Each case asserts the exact verb and path the server registers, that
// the rows arrive as the UI's ALREADY EXISTING types (no client-side reshaping), and
// that a refusal surfaces as this client's `HttpError` rather than as an empty list —
// "the read failed" and "there is nothing there" must never be one answer.
//
// The fixtures are the real answers' wire shapes, written out field for field:
// comments carries the stored row (author, author_user_id, request_id), events the
// append-only history row, links the `RequestLink` the rail resolves a target onto.

/** One stored discussion row, as the team's arm answers it. */
const commentRow: RequestComment = {
  id: 9180,
  request_id: 7,
  kind: 'comment',
  body: 'Looking at the November run now.',
  author: 'dana@simstech.cloud',
  author_user_id: 12,
  created_at: '2026-11-13T11:00:00Z',
}

/** The same thread as a requester reads it: no id, no author, no user id. */
const sharedCommentRow = {
  kind: 'question' as const,
  from: 'desk' as const,
  body: 'Which export file do you mean?',
  at: '2026-11-13T10:30:00Z',
}

const teamThread: RequestThreadAnswer = {
  view: 'team', request_id: 7, count: 1, next_after_id: 9180, comments: [commentRow],
}

const requesterThread: RequestThreadAnswer = {
  view: 'requester', count: 1, shared_comments: [sharedCommentRow],
}

/** One appended history row, from the act that moved the request into triage. */
const eventRow: RequestEvent = {
  id: 9181,
  request_id: 7,
  org_id: 2,
  team_id: 4,
  action: 'transition',
  from_status: 'new',
  to_status: 'triaging',
  actor: 'dana@simstech.cloud',
  actor_user_id: 12,
  detail: '{"source_kind":"web"}',
  created_at: '2026-11-13T10:00:00Z',
}

const history: RequestEventsAnswer = { view: 'team', request_id: 7, count: 1, events: [eventRow] }

/** One lineage row: the route the request is on. */
const linkRow: RequestLink = {
  id: 3,
  request_id: 7,
  link_kind: 'routes_to',
  target_kind: 'project',
  target_id: 3,
  created_by: 12,
  created_at: '2026-11-13T11:00:00Z',
}

const lineage: RequestLinksAnswer = { view: 'team', request_id: 7, count: 1, links: [linkRow] }

/** Call a route that fails, and return the thrown error as this client's type. */
const failure = async (call: () => Promise<unknown>): Promise<HttpError> => {
  const err = await call().then(() => null, (e: unknown) => e as HttpError)
  expect(err).not.toBeNull()
  return err!
}

describe('the record sub-reads: discussion, history and lineage', () => {
  it('listComments asks the thread route and returns the team rows unchanged', async () => {
    server.use(route('get', '*/api/teams/4/requests/7/comments', teamThread))
    const answer = await requestApi.listComments(4, 7)
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual(['GET /api/teams/4/requests/7/comments'])
    // The answer narrows on `view`, and the rows are the existing RequestComment:
    // the stored author snapshot travels, and nothing is reshaped client-side.
    expect(answer.view).toBe('team')
    if (answer.view !== 'team') throw new Error('expected the team arm')
    expect(answer.comments).toEqual([commentRow])
    expect(answer.next_after_id).toBe(9180)
    // An unset cursor writes no query at all — the server reads "from the start".
    expect(new URL(calls[0].url).search).toBe('')
  })

  it('listComments reads a requester arm as its own list, never as an empty team thread', async () => {
    server.use(route('get', '*/api/teams/4/requests/a1b2c3d4e5f6g7h8i9j0/comments', requesterThread))
    const answer = await requestApi.listComments(4, 'a1b2c3d4e5f6g7h8i9j0')
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual([
      'GET /api/teams/4/requests/a1b2c3d4e5f6g7h8i9j0/comments',
    ])
    // A requester answer has no `comments` key at all, so this arm is the only way
    // to reach their thread: "is comments an array" would answer false for a real one.
    if (answer.view !== 'requester') throw new Error('expected the requester arm')
    expect(answer.shared_comments).toEqual([sharedCommentRow])
    expect(answer).not.toHaveProperty('comments')
    expect(answer).not.toHaveProperty('request_id')
  })

  it('listComments sends its forward cursor as after_id and limit', async () => {
    server.use(route('get', '*/api/teams/4/requests/7/comments', teamThread))
    await requestApi.listComments(4, 7, { after_id: 9180, limit: 25 })
    const q = new URL(calls[0].url).searchParams
    expect(q.get('after_id')).toBe('9180')
    expect(q.get('limit')).toBe('25')
  })

  it('listComments surfaces a refusal as HttpError rather than an empty thread', async () => {
    server.use(http.get('*/api/teams/4/requests/7/comments', () =>
      HttpResponse.json({ error: 'no such request', code: 'request.not_found' }, { status: 404 })))
    const err = await failure(() => requestApi.listComments(4, 7))
    expect(err.status).toBe(404)
    expect(err.code).toBe('request.not_found')
    expect(err.message).toContain('no such request')
  })

  it('listEvents asks the history route and returns the append-only rows', async () => {
    server.use(route('get', '*/api/teams/4/requests/7/events', history))
    const answer = await requestApi.listEvents(4, 7)
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual(['GET /api/teams/4/requests/7/events'])
    expect(answer.view).toBe('team')
    expect(answer.request_id).toBe(7)
    // A transition row keeps both state names and its detail; the UI's optional
    // fields are exactly the `omitempty` ones the server drops elsewhere.
    expect(answer.events).toEqual([eventRow])
    expect(answer.events[0].from_status).toBe('new')
    expect(answer.events[0].to_status).toBe('triaging')
    expect(answer.next_after_id).toBeUndefined()
  })

  it('listEvents sends its forward cursor as after_id and limit', async () => {
    server.use(route('get', '*/api/teams/4/requests/7/events', history))
    await requestApi.listEvents(4, 7, { after_id: 9181, limit: 50 })
    const q = new URL(calls[0].url).searchParams
    expect(q.get('after_id')).toBe('9181')
    expect(q.get('limit')).toBe('50')
  })

  it('listEvents surfaces a refusal as HttpError rather than an empty history', async () => {
    server.use(http.get('*/api/teams/4/requests/7/events', () =>
      HttpResponse.json({ error: 'no such request', code: 'request.not_found' }, { status: 404 })))
    const err = await failure(() => requestApi.listEvents(4, 7))
    expect(err.status).toBe(404)
    expect(err.code).toBe('request.not_found')
  })

  it('listLinks asks the lineage route and returns rows the rail can carry as-is', async () => {
    server.use(route('get', '*/api/teams/4/requests/7/links', lineage))
    const links = await requestApi.listLinks(4, 7)
    expect(calls.map(c => `${c.method} ${c.path}`)).toEqual(['GET /api/teams/4/requests/7/links'])
    // The row is the rail's `ResolvedRequestLink` — RequestLink plus an OPTIONAL
    // target — and the server resolves no target: an absent one is truthful, and
    // the page fills the title from the target's own read.
    expect(links).toEqual([linkRow])
    expect(links[0].target).toBeUndefined()
    expect(new URL(calls[0].url).search).toBe('')
  })

  it('listLinks writes its kind filter in the repeated form the server accepts', async () => {
    server.use(route('get', '*/api/teams/4/requests/7/links', lineage))
    await requestApi.listLinks(4, 7, 'planned_as')
    await requestApi.listLinks(4, 7, ['planned_as', 'delivered_by'])
    const one = new URL(calls[0].url).searchParams
    const two = new URL(calls[1].url).searchParams
    expect(one.getAll('kind')).toEqual(['planned_as'])
    expect(two.getAll('kind')).toEqual(['planned_as', 'delivered_by'])
  })

  it('listLinks surfaces a refusal as HttpError rather than an empty lineage', async () => {
    server.use(http.get('*/api/teams/4/requests/7/links', () =>
      HttpResponse.json({ error: 'no such request', code: 'request.not_found' }, { status: 404 })))
    const err = await failure(() => requestApi.listLinks(4, 7))
    expect(err.status).toBe(404)
    expect(err.code).toBe('request.not_found')
    expect(requestConflict(err)).toBeNull()
  })

  it('an empty list is [] and not a failure — the panels can tell the two apart', async () => {
    // The server writes `[]` for "nothing attached yet"; the client passes it
    // through, so a rail renders "not routed yet" rather than a failure notice.
    server.use(
      route('get', '*/api/teams/4/requests/7/links', { view: 'team', request_id: 7, count: 0, links: [] }),
      route('get', '*/api/teams/4/requests/7/events', { view: 'team', request_id: 7, count: 0, events: [] }),
    )
    expect(await requestApi.listLinks(4, 7)).toEqual([])
    expect((await requestApi.listEvents(4, 7)).events).toEqual([])
  })
})

describe('DTO separation: the two views are two types', () => {
  it('the team record carries every internal field', () => {
    // The internal fields a requester must never see, all present on the record.
    for (const key of ['org_id', 'team_id', 'claim_user_id', 'claimed_at', 'owner_user_id',
      'requester_contact', 'rev', 'metadata', 'duplicate_of', 'reject_reason', 'source_external_id']) {
      expect(record, key).toHaveProperty(key)
    }
  })

  it('the requester projection has no room for one', () => {
    // Two halves, and the compile-time half is the one that matters: these
    // directives are consumed only while the fields are absent from the type, so
    // a future commit that adds one to `RequesterRequestView` turns this test
    // file into a type error (an unused @ts-expect-error) rather than leaving a
    // silently-nullable internal field on the requester surface.
    const field = <K extends PropertyKey>() => (k: K): K => k
    // @ts-expect-error a projection carries no org: adding one would be a server bug, not a client option.
    field<keyof RequesterRequestView>()('org_id')
    // @ts-expect-error nor a revision — the write answers carry one instead.
    field<keyof RequesterRequestView>()('rev')
    // @ts-expect-error nor a triage claim.
    field<keyof RequesterRequestView>()('claim_user_id')
    // @ts-expect-error nor the assignee.
    field<keyof RequesterRequestView>()('owner_user_id')
    // @ts-expect-error nor the contact snapshot.
    field<keyof RequesterRequestView>()('requester_contact')
    // @ts-expect-error nor a serial id: a requester's row is named by its public reference only.
    field<keyof RequesterRequestView>()('id')

    // And at runtime, the fixture's own keys agree.
    const herFields = Object.keys(projection)
    for (const key of ['org_id', 'team_id', 'claim_user_id', 'claimed_at', 'owner_user_id',
      'requester_contact', 'rev', 'metadata', 'duplicate_of', 'reject_reason', 'id']) {
      expect(herFields).not.toContain(key)
    }
  })

  it('a read answer is discriminated by `view`, so a caller cannot read the wrong half', () => {
    expect(teamAnswer.view).toBe('team')
    expect(requesterAnswer.view).toBe('requester')
    const asTeam = teamAnswer as Extract<RequestReadAnswer, { view: 'team' }>
    expect(asTeam.request.id).toBe(7)
    expect(asTeam.outcome.delivered_eligible).toBe(false)
    const asRequester = requesterAnswer as Extract<RequestReadAnswer, { view: 'requester' }>
    expect(asRequester.projection.state_word).toBe('Being reviewed')
    // @ts-expect-error a requester answer has no `request` half to reach for: the union's other arm does.
    void requesterAnswer.request
  })
})

describe('client error/retry mapping', () => {
  it('a 409 carries the code and the revision the row is actually at', async () => {
    server.use(http.patch('*/api/teams/4/requests/7', () =>
      HttpResponse.json({ error: 'request 7 is at rev 9, not 3: it changed since it was read, so reload it and reapply', code: 'request.conflict', rev: 9 }, { status: 409 })))
    const err = await failure(() => requestApi.update(4, 7, { subject: 'x', expected_rev: 3 }))
    expect(err.status).toBe(409)
    expect(err.code).toBe('request.conflict')
    expect(err.rev).toBe(9)
    // The server's sentence is kept — it is a sentence, not a bare status word.
    expect(err.message).toContain('is at rev 9')
    expect(requestConflict(err)).toEqual({ rev: 9, claimUserId: undefined, code: 'request.conflict' })
  })

  it('a lost claim race carries who holds it', async () => {
    server.use(http.post('*/api/teams/4/requests/7/claim', () =>
      HttpResponse.json({ error: 'request 7 is claimed by user 12', code: 'request.conflict', claim_user_id: 12 }, { status: 409 })))
    const err = await failure(() => requestApi.claim(4, 7))
    expect(err.status).toBe(409)
    expect(err.claimUserId).toBe(12)
    expect(requestConflict(err)).toEqual({ rev: undefined, claimUserId: 12, code: 'request.conflict' })
  })

  it('a 429 carries its wait from the header and the body alike', async () => {
    // Both spellings at once — which is what the submission route writes.
    server.use(http.post('*/api/teams/4/requests', () =>
      HttpResponse.json(
        { error: 'too many submissions; try again shortly', code: 'request.rate_limited', retry_after_s: 21 },
        { status: 429, headers: { 'Retry-After': '21' } },
      )))
    const err = await failure(() => requestApi.submit(4, { subject: 'x', input_brief: 'y' }))
    expect(err.status).toBe(429)
    expect(err.code).toBe('request.rate_limited')
    expect(err.retryAfterS).toBe(21)
    expect(requestRetryAfterS(err)).toBe(21)

    // The header alone, or the body alone, reads the same number — a caller
    // never has to know which one the server sent.
    server.use(http.post('*/api/teams/4/requests', () =>
      new HttpResponse(null, { status: 429, headers: { 'Retry-After': '7' } })))
    expect(requestRetryAfterS(await failure(() => requestApi.submit(4, { subject: 'x', input_brief: 'y' })))).toBe(7)

    server.use(http.post('*/api/teams/4/requests', () =>
      HttpResponse.json({ error: 'slow down', code: 'request.rate_limited', retry_after_s: 3 }, { status: 429 })))
    expect(requestRetryAfterS(await failure(() => requestApi.submit(4, { subject: 'x', input_brief: 'y' })))).toBe(3)
  })

  it('a 404 is the surface single not-found: same code for a foreign reference and an unknown one', async () => {
    server.use(http.get('*/api/requests/unknown-reference-here', () =>
      HttpResponse.json({ error: 'no such request', code: 'request.not_found' }, { status: 404 })))
    const unknown = await failure(() => requestApi.ownDetail('unknown-reference-here'))
    server.use(http.get('*/api/requests/someone-elses-ref-000', () =>
      HttpResponse.json({ error: 'no such request', code: 'request.not_found' }, { status: 404 })))
    const foreign = await failure(() => requestApi.ownDetail('someone-elses-ref-000'))
    // Existence never leaks: the two are indistinguishable to a caller.
    expect(unknown.code).toBe('request.not_found')
    expect(foreign.code).toBe(unknown.code)
    expect(foreign.status).toBe(unknown.status)
    expect(foreign.message).toBe(unknown.message)
    // And a 404 is not a conflict, so the retry helpers stay quiet.
    expect(requestConflict(unknown)).toBeNull()
    expect(requestRetryAfterS(unknown)).toBeNull()
  })

  it('a 400 names the mistake with request.invalid', async () => {
    server.use(http.post('*/api/teams/4/requests/7/deliver', () =>
      HttpResponse.json({ error: '"to" is not a field of this route: its destination is fixed (delivered)', code: 'request.invalid' }, { status: 400 })))
    const err = await failure(() => requestApi.deliver(4, 7, { to: 'delivered' }))
    expect(err.status).toBe(400)
    expect(err.code).toBe('request.invalid')
    expect(err.message).toContain('destination is fixed')
  })

  it('a non-request failure keeps the client existing behaviour', async () => {
    // A bare status word still becomes a sentence (lib/apiError.ts, #124), and
    // the newer fields stay absent rather than becoming `undefined` junk.
    server.use(http.get('*/api/teams/4/requests', () => new HttpResponse('Forbidden', { status: 403, statusText: 'Forbidden' })))
    const err = await failure(() => requestApi.queue(4))
    expect(err.status).toBe(403)
    expect(err.message).not.toBe('Forbidden')
    expect(err.message).toMatch(/permission/i)
    expect(err.code).toBeUndefined()
    expect(err.rev).toBeUndefined()
  })
})

describe('no credential is added or stored', () => {
  it('a request carries no Authorization header and looks at no storage', async () => {
    server.use(route('get', '*/api/teams/4/requests', queue))
    await requestApi.queue(4)
    expect(calls[0].headers.get('authorization')).toBeNull()
    // No cookie, no token, no key in the URL: sign-in is the proxy's cookie, and
    // this slice stores nothing of its own.
    expect(new URL(calls[0].url).searchParams.get('token')).toBeNull()
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })
})
