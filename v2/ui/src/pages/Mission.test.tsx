// Mission is informational (v2/kb/working/human-work.md, artifact #23):
// a Waiting-on-you strip into My work, Merge needed and "for you" readouts,
// outlined squares, and Waiting on people. It has no action buttons.
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { Mission } from './Mission'
import { AuthContext } from '../contexts/auth-context'
import { resetWaitCounts } from '../lib/waitCounts'
import type { Me, MyWorkCounts, PeopleWaiting, ProjectAccess, Task } from '../types'

const ME = 7
const at = '2026-10-27T10:00:00Z'
function task(id: number, over: Partial<Task> = {}): Task {
  return {
    id, project_id: 2, epic_id: 172, story_id: 413, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
    state: 'completed', priority: '1', dependency_ids: [], created_at: at, updated_at: at, planned_at: at, approved_at: at, ...over,
  }
}
const tasks: Task[] = [
  task(2470),
  task(2471, { owner_user_id: ME, merge_state: 'needed' }),
  task(2462, { owner_user_id: 8, merge_state: 'needed' }),
  task(2473, { owner_user_id: ME, state: 'blocked', block_reason: 'Which copy?' }),
  task(2477, { owner_user_id: 8, state: 'blocked', block_reason: 'Which API?' }),
  task(2481, { owner_user_id: ME, performer: 'human', state: 'backlog' }),
  task(2490, { owner_user_id: 9, performer: 'human', state: 'blocked', dependency_ids: [2481] }),
  task(2491, { owner_user_id: 8, state: 'in_progress', agent_id: 'amp:amp-worker-frontend', dispatched_at: at }),
]
let counts: MyWorkCounts = { counts: { waiting: 3, approve: 0, answer: 1, merge: 1, start: 0, do: 1, coming_up: 0 }, projects: { 2: { waiting: 3, approve: 0, answer: 1, merge: 1, start: 0, do: 1, coming_up: 0 } } }
// Waiting on people comes from the server (Service.WaitingOn per person), in
// user id order; the panel orders it (you first, then most waiting).
const wc = (o: Partial<MyWorkCounts['counts']>) => ({ waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0, ...o })
const PEOPLE: PeopleWaiting = { project_id: 2, people: [
  { user_id: ME, counts: wc({ waiting: 3, answer: 1, merge: 1, start: 0, do: 1 }), coming_up: [], items: [
    { kind: 'answer', task_ids: [2473], since: at }, { kind: 'merge', task_ids: [2471], since: at }, { kind: 'do', task_ids: [2481], since: at }] },
  { user_id: 8, counts: wc({ waiting: 2, answer: 1, merge: 1 }), coming_up: [], items: [
    { kind: 'answer', task_ids: [2477], since: at }, { kind: 'merge', task_ids: [2462], since: at }] },
  { user_id: 9, counts: wc({ coming_up: 1 }), coming_up: [2490], items: [] },
] }
let peopleWaiting: PeopleWaiting = PEOPLE
let peopleCalls = 0
let access: ProjectAccess = 'lead'
let inFlight = 0

const server = setupServer(
  http.get('*/api/projects/2/epics', () => HttpResponse.json({ epics: [{ id: 172, project_id: 2, name: 'Human work UX', description: '', priority: '1', created_at: at, updated_at: at }] })),
  http.get('*/api/epics/172/stories', () => HttpResponse.json({ stories: [{ id: 413, epic_id: 172, name: 'Mission', description: '', priority: '1', created_at: at, updated_at: at }] })),
  http.get('*/api/projects/2/tasks', () => HttpResponse.json({ tasks })),
  http.get('*/api/projects/2/members', () => HttpResponse.json({ my_access: access, members: [
    { user_id: ME, email: 'jacob@x.io', display_name: 'Jacob Sims', access: 'owner' },
    { user_id: 8, email: 'sam@x.io', display_name: 'Sam Ortiz', access: 'member' },
    { user_id: 9, email: 'priya@x.io', display_name: 'Priya Kumar', access: 'member' },
  ] })),
  http.get('*/api/projects/2/activity', () => HttpResponse.json({ activity: [], count: 0 })),
  http.get('*/api/audit', () => HttpResponse.json({ entries: [] })),
  http.get('*/api/my-work/counts', () => HttpResponse.json(counts)),
  http.get('*/api/projects/2/waiting-on-people', () => { peopleCalls++; return HttpResponse.json(peopleWaiting) }),
  http.get('*/api/projects/2', () => HttpResponse.json({ project: { id: 2, name: 'AMP v2', code: 'AMPV2', require_plan_approval: true } })),
  http.get('*/api/*', () => HttpResponse.json({})),
)
server.events.on('request:start', () => { inFlight++ })
server.events.on('request:end', () => { inFlight-- })
// Requests made by Mission's background loads since open() rendered it.
const calls = { activity: 0, audit: 0, counts: 0, waiting: 0 }
const CALL_PATHS: Record<string, keyof typeof calls> = {
  '/api/projects/2/activity': 'activity', '/api/audit': 'audit',
  '/api/my-work/counts': 'counts', '/api/projects/2/waiting-on-people': 'waiting',
}
server.events.on('request:start', ({ request }) => {
  const kind = CALL_PATHS[new URL(request.url).pathname]
  if (kind) calls[kind]++
})
const resetCalls = () => { for (const k of Object.keys(calls) as (keyof typeof calls)[]) calls[k] = 0 }
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
beforeEach(() => { resetWaitCounts(); access = 'lead'; peopleWaiting = PEOPLE; peopleCalls = 0 })
afterEach(async () => {
  await waitFor(() => expect(inFlight).toBe(0))
  server.resetHandlers()
  counts = { counts: { waiting: 3, approve: 0, answer: 1, merge: 1, start: 0, do: 1, coming_up: 0 }, projects: { 2: { waiting: 3, approve: 0, answer: 1, merge: 1, start: 0, do: 1, coming_up: 0 } } }
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

async function open(me: Me | null = { user_id: ME, email: 'jacob@x.io' } as never, loading = false) {
  // The strip reads the shared counts store, which AppShell fills; do it here.
  const { refreshWaitCounts } = await import('../lib/waitCounts')
  await refreshWaitCounts()
  resetCalls()
  const router = createMemoryRouter([{ path: '/project/:projectId/mission', element: <Mission /> }], { initialEntries: ['/project/2/mission'] })
  render(
    <AuthContext.Provider value={{ me, loading, error: null, isAdmin: false }}>
      <RouterProvider router={router} />
    </AuthContext.Provider>,
  )
  return await screen.findByText('Waiting on people')
}

describe('Mission (informational)', () => {
  it('shows the Waiting on you strip, linking to My work', async () => {
    await open()
    const strip = await screen.findByRole('link', { name: /^Waiting on you: 3\. Answer 1 · Merge 1 · Do 1\. Open My work$/ })
    expect(strip).toHaveAttribute('href', '/project/2/my-work')
  })

  it('shows the quiet strip when nothing waits on you', async () => {
    counts = { counts: { waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0 }, projects: {} }
    await open()
    expect(await screen.findByText('Nothing waits on you here')).toBeInTheDocument()
    expect(screen.getByText('Nothing waits on you here').closest('a')).toHaveAttribute('href', '/project/2/my-work')
  })

  it('without an AMP user (sign-in off, Local Dev not provisioned): no counts, no green, and where to look instead', async () => {
    counts = { counts: { waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0 }, projects: {} }
    await open({ subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: [] })
    expect(screen.queryByText('Nothing waits on you here')).toBeNull()
    const strip = screen.getByRole('status')
    expect(strip).toHaveTextContent('Waiting on you: not counted')
    expect(strip).toHaveTextContent('Sign-in is off on this AMP')
    expect(within(strip).getByRole('link', { name: /Board/ })).toHaveAttribute('href', '/project/2/board')
    expect(within(strip).getByRole('link', { name: /Why/ })).toHaveAttribute('href', '/project/2/my-work')
    // The readouts keep the project's numbers but never claim "0 for you".
    expect(screen.getByText('Questions').parentElement).not.toHaveTextContent('for you')
    expect(screen.getByText('Merge needed').parentElement).not.toHaveTextContent('yours')
  })

  it('identity still loading: no strip yet, so local dev never flashes green', async () => {
    counts = { counts: { waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0 }, projects: {} }
    await open(null, true)
    expect(screen.queryByText('Nothing waits on you here')).toBeNull()
    expect(screen.queryByText('Waiting on you: not counted')).toBeNull()
    expect(screen.getByText('Questions').parentElement).not.toHaveTextContent('for you')
  })

  it('signed out: says so, not "Nothing waits on you here"', async () => {
    await open({ subject: 'oidc|abc', email: 'jo@example.com', name: 'Jo', roles: [] })
    expect(screen.queryByText('Nothing waits on you here')).toBeNull()
    expect(screen.getByRole('status')).toHaveTextContent('You are not signed in as an AMP user')
  })

  it('readouts: Questions N · M for you, Merge needed N · M yours', async () => {
    await open()
    const q = screen.getByText('Questions').parentElement!
    expect(q).toHaveTextContent('2')
    expect(q).toHaveTextContent('1 for you')
    const m = screen.getByText('Merge needed').parentElement!
    expect(m).toHaveTextContent('2')
    expect(m).toHaveTextContent('1 yours')
  })

  it('outlines the squares that wait on you', async () => {
    await open()
    const epic = await screen.findByRole('region', { name: 'Epic Human work UX' })
    await waitFor(() => expect(epic.querySelectorAll('[data-for-you]')).toHaveLength(3))
    expect(within(epic).getByRole('button', { name: /Ticket 2471: .*waits on you/ })).toBeInTheDocument()
    expect(within(epic).getByRole('button', { name: /Ticket 2477: .*Question$/ })).toBeInTheDocument()
    expect(within(epic).getByText('3 for you')).toBeInTheDocument()
  })

  it('Waiting on people: you first, counts by kind, rows link into each My work', async () => {
    const panel = (await open()).closest('section')!
    const rows = within(panel).getAllByRole('link')
    expect(rows.map(r => r.getAttribute('aria-label'))).toEqual([
      'You: 3 waiting. Answer 1 (#2473) · Merge 1 (#2471) · Do 1 (#2481)',
      'Sam Ortiz: 2 waiting. Answer 1 (#2477) · Merge 1 (#2462)',
      'Priya Kumar: 0 waiting. 1 coming up',
    ])
    expect(rows[0]).toHaveAttribute('href', '/project/2/my-work?focus=2473')
    expect(rows[1]).toHaveAttribute('href', '/project/2/my-work?user=8&focus=2477')
    expect(rows[2]).toHaveAttribute('href', '/project/2/my-work?user=9&focus=2490')
  })

  it('Waiting on people is what the server says, not worked out from the Board', async () => {
    // An approval the Board's tickets alone wouldn't produce (they're all approved).
    peopleWaiting = { project_id: 2, people: [
      { user_id: 9, counts: wc({ waiting: 1, approve: 1 }), coming_up: [], items: [{ kind: 'approve', task_ids: [2470, 2490], epic_id: 172, epic_name: 'Human work UX', since: at }] },
    ] }
    const panel = (await open()).closest('section')!
    await waitFor(() => expect(within(panel).getAllByRole('link').map(r => r.getAttribute('aria-label'))).toEqual([
      'Priya Kumar: 1 waiting. Approve 1 (2 tickets)',
    ]))
    expect(within(panel).getByRole('link')).toHaveAttribute('href', '/project/2/my-work?user=9&focus=2470')
    expect(peopleCalls).toBeGreaterThan(0)
    // Nothing waits on the viewer in it, so no square is outlined.
    const epic = await screen.findByRole('region', { name: 'Epic Human work UX' })
    expect(epic.querySelectorAll('[data-for-you]')).toHaveLength(0)
  })

  it('says so when nothing waits on anyone', async () => {
    peopleWaiting = { project_id: 2, people: [] }
    const panel = (await open()).closest('section')!
    expect(await within(panel).findByText('Nothing waits on anyone here.')).toBeInTheDocument()
  })

  it("for a member, others' rows open the ticket on the Board", async () => {
    access = 'member'
    const panel = (await open()).closest('section')!
    await waitFor(() => expect(within(panel).getAllByRole('link')[1]).toHaveAttribute('href', '/project/2/board?focus=2477'))
  })

  it('has no action buttons: no Answer, Merged, Approve, Start or Mark done', async () => {
    await open()
    await screen.findByRole('region', { name: 'Epic Human work UX' })
    const names = screen.getAllByRole('button').map(b => b.getAttribute('aria-label') || b.textContent || '')
    for (const n of names) expect(n).not.toMatch(/^(Answer|Merged|Mark merged|Approve|Start|Mark done|Send answer)/)
    expect(document.querySelector('textarea')).toBeNull()
    expect(screen.queryByText(/done, not merged/)).toBeNull()
  })
})

// A hidden tab makes no background loads (activity, audit, counts, waiting on
// people). Each one it skipped runs once when the tab is shown again.
describe('Mission in a hidden tab', () => {
  const realSetTimeout = globalThis.setTimeout
  let visibility: DocumentVisibilityState = 'visible'
  const setVisibility = (state: DocumentVisibilityState) => act(() => {
    visibility = state
    document.dispatchEvent(new Event('visibilitychange'))
  })
  const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms))
  // Let any request that was started reach the server, then wait for all of them to finish.
  const quiet = async () => {
    await act(() => new Promise(r => realSetTimeout(r, 50)))
    await waitFor(() => expect(inFlight).toBe(0))
  }
  const none = { activity: 0, audit: 0, counts: 0, waiting: 0 }
  const oneEach = { activity: 1, audit: 1, counts: 1, waiting: 1 }

  beforeEach(() => {
    // Fake time still moves with real time, so findBy and waitFor keep working.
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
    visibility = 'visible'
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
  })
  afterEach(() => {
    cleanup()
    vi.useRealTimers()
    // Drop the override so jsdom's own getter shows through again.
    delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
  })

  it('a tab opened hidden loads none of them, then one of each once shown', async () => {
    visibility = 'hidden'
    await open()
    // The 600 ms and 1200 ms reloads come due, and two activity polls.
    await advance(2 * 60_000 + 2_000)
    await quiet()
    expect(calls).toEqual(none)

    await setVisibility('visible')
    // The poll's catch-up and the held activity reload share one load.
    await waitFor(() => expect(calls).toEqual(oneEach))
    await advance(5_000)
    await quiet()
    expect(calls).toEqual(oneEach)
  })

  it('hidden for less than a poll: the reloads it skipped run once when shown', async () => {
    visibility = 'hidden'
    await open()
    await advance(2_000)
    await quiet()
    expect(calls).toEqual(none)

    await setVisibility('visible')
    await waitFor(() => expect(calls).toEqual(oneEach))
    await advance(5_000)
    await quiet()
    expect(calls).toEqual(oneEach)
  })

  it('polls activity each minute while visible, none while hidden, and catches up once', async () => {
    await open()
    await advance(2_000)
    await quiet()
    resetCalls()

    // Hidden and shown again before anything came due: nothing extra runs.
    await setVisibility('hidden')
    await advance(5_000)
    await setVisibility('visible')
    await quiet()
    expect(calls).toEqual(none)

    await advance(60_000)
    await waitFor(() => expect(calls).toEqual({ ...none, activity: 1, audit: 1 }))
    resetCalls()

    await setVisibility('hidden')
    await advance(3 * 60_000)
    await quiet()
    expect(calls).toEqual(none)

    // Only the activity poll was skipped, so only activity loads, once.
    await setVisibility('visible')
    await waitFor(() => expect(calls).toEqual({ ...none, activity: 1, audit: 1 }))
    await advance(5_000)
    await quiet()
    expect(calls).toEqual({ ...none, activity: 1, audit: 1 })
  })

  it('after unmount, showing the tab loads nothing', async () => {
    visibility = 'hidden'
    await open()
    await advance(2_000)
    cleanup()
    await setVisibility('visible')
    await advance(2_000)
    await quiet()
    expect(calls).toEqual(none)
  })
})
