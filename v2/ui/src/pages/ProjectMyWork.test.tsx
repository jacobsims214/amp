// The project's My work tab (KB decisions/human-work-ux.md §2, artifact #23)
// and the landing rule (/project/:id opens My work when something waits on you).
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { AuthContext } from '../contexts/auth-context'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { ProjectMyWork } from './ProjectMyWork'
import { ProjectLanding } from './ProjectLanding'
import { ProjectNav } from '../components/ProjectNav'
import { fixedTab, landingPath, landingTab } from '../lib/landing'
import { resetWaitCounts } from '../lib/waitCounts'
import { alertMergeItem, doItem, linkedMergeItem, legacyMergeItem, mergeItem, myWork, task } from '../test/myWorkFixture'
import type { Me, MyWorkCounts, ProjectMyWork as Data, ProjectWaitItem, Task } from '../types'

let data: Data = myWork('busy')
let counts: MyWorkCounts = { counts: { waiting: 5, approve: 1, answer: 1, merge: 1, start: 1, do: 1, coming_up: 1 }, projects: { 2: { waiting: 5, approve: 1, answer: 1, merge: 1, start: 1, do: 1, coming_up: 1 } } }
const posts: string[] = []
const loads: string[] = []
let countsCalls = 0
let inFlight = 0

const server = setupServer(
  http.get('*/api/projects/2/my-work', ({ request }) => {
    const q = new URL(request.url).searchParams
    loads.push(q.toString())
    return q.get('approve') ? HttpResponse.json({ plan: data.items.approve[0]?.plan }) : HttpResponse.json(data)
  }),
  http.get('*/api/my-work/counts', () => { countsCalls++; return HttpResponse.json(counts) }),
  http.get('*/api/projects/2/members', () => HttpResponse.json({ members: [], my_access: 'lead' })),
  http.get('*/api/projects/:id', () => HttpResponse.json({ project: { id: 2, name: 'AMP v2', code: 'AMPV2' } })),
  http.post('*/api/tasks/2471/merge', ({ request }) => {
    posts.push(new URL(request.url).pathname)
    return HttpResponse.json({ task_id: 2471, merge_state: 'merged', already_recorded: false, message: 'merged' })
  }),
)
server.events.on('request:start', () => { inFlight++ })
server.events.on('request:end', () => { inFlight-- })
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
beforeEach(() => { resetWaitCounts() })
afterEach(async () => {
  await waitFor(() => expect(inFlight).toBe(0))
  server.resetHandlers(); posts.length = 0; loads.length = 0; countsCalls = 0; data = myWork('busy')
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

function Where() {
  const l = useLocation()
  return <span data-testid="where">{l.pathname + l.search}</span>
}

function at(url: string, me: Me | null = null, loading = false) {
  const router = createMemoryRouter([
    { path: '/project/:projectId', element: <><ProjectLanding /><Where /></> },
    { path: '/project/:projectId/my-work', element: <><ProjectMyWork /><Where /></> },
    { path: '/project/:projectId/mission', element: <><ProjectNav projectId={2} currentView="mission" /><p>Mission page</p><Where /></> },
    { path: '/project/:projectId/board', element: <><p>Board page</p><Where /></> },
  ], { initialEntries: [url] })
  render(<AuthContext.Provider value={{ me, loading, error: null, isAdmin: false }}><RouterProvider router={router} /></AuthContext.Provider>)
  return router
}
const where = () => screen.getByTestId('where').textContent

describe('My work tab: states', () => {
  it('busy: readouts, the items in kind order with their actions, and the side panels', async () => {
    at('/project/2/my-work')
    const list = await screen.findByRole('region', { name: 'Waiting on you' })
    const ro = screen.getByRole('group', { name: 'Waiting on you in AMP v2' })
    expect(within(ro).getByText('Waiting on you').parentElement).toHaveClass('alarm')
    expect(within(ro).getByText('Waiting on you').nextSibling).toHaveTextContent('5')
    expect(within(ro).getByText(/^Oldest: 3h, the plan approval$/)).toBeInTheDocument()
    expect(within(ro).getByText('plan · 5 tickets')).toBeInTheDocument()

    const kinds = Array.from(list.querySelectorAll('[data-item-key]')).map(e => e.getAttribute('data-item-key'))
    expect(kinds).toEqual(['approve:172', 'task:2473', 'task:2471', 'task:2803', 'task:2481'])
    expect(within(list).getByRole('button', { name: 'Review and approve 5' })).toBeInTheDocument()
    expect(within(list).getByRole('button', { name: /send answer/i })).toBeInTheDocument()
    expect(within(list).getByRole('button', { name: /mark merged/i })).toBeInTheDocument()
    // The merge card's contract (AMP #3013): the PR's route, and only the
    // dependents that actually wait — never every dependent.
    expect(within(list).getByTestId('merge-route')).toHaveTextContent('PR #88 · fix/board-mine → main')
    expect(within(list).getByTestId('merge-waiters')).toHaveTextContent('2 tickets wait on this merge: #2490, #2491.')
    expect(within(list).getByRole('button', { name: /return to ready/i })).toBeInTheDocument()
    expect(within(list).getByRole('button', { name: /mark done…/i })).toBeInTheDocument()

    const coming = screen.getByRole('region', { name: 'Coming up' })
    expect(within(coming).getByText('1 · not counted yet')).toBeInTheDocument()
    expect(within(coming).getByText(/After #2130 M5-MERGE \(a person, in progress\)/)).toBeInTheDocument()
    const agents = screen.getByRole('region', { name: 'Your agents here' })
    expect(within(agents).getByRole('link', { name: /4 in progress/ })).toHaveAttribute('href', '/project/2/board')
    expect(within(screen.getByRole('region', { name: 'Done today' })).getByText('Merged #2470: My Work: questions counted once')).toBeInTheDocument()
  })

  it('empty: "You\'re clear in AMP v2" with the legend', async () => {
    data = myWork('empty')
    at('/project/2/my-work')
    expect(await screen.findByRole('heading', { name: "You're clear in AMP v2." })).toBeInTheDocument()
    expect(screen.getByText('Answer a question')).toBeInTheDocument()
    const ro = screen.getByRole('group', { name: 'Waiting on you in AMP v2' })
    expect(within(ro).getByText('Waiting on you').parentElement).toHaveClass('calm')
    expect(within(ro).getByText('All clear')).toBeInTheDocument()
    expect(screen.getByText('None of your tickets are waiting on other work.')).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Done today' })).toBeNull()
  })

  it('done: "You cleared N things today" with the cleared list', async () => {
    data = myWork('done')
    at('/project/2/my-work')
    expect(await screen.findByRole('heading', { name: 'You cleared 2 things in AMP v2 today.' })).toBeInTheDocument()
    const ro = screen.getByRole('group', { name: 'Waiting on you in AMP v2' })
    expect(within(ro).getByText('Cleared today').nextSibling).toHaveTextContent('2')
    expect(screen.getByText('Approved 9 tickets: Human work UX')).toBeInTheDocument()
    expect(screen.getByText('Recorded by GitHub')).toBeInTheDocument()
  })

  it('an action reloads the list and the badges', async () => {
    at('/project/2/my-work')
    await userEvent.click(await screen.findByRole('button', { name: /mark merged/i }))
    await waitFor(() => expect(posts).toEqual(['/api/tasks/2471/merge']))
    await waitFor(() => expect(loads.length).toBe(2))
    await waitFor(() => expect(countsCalls).toBeGreaterThan(0))
  })

  it('a lead reading someone else\'s list sees no actions', async () => {
    data = myWork('busy', { read_only: true })
    at('/project/2/my-work?user=7')
    expect(await screen.findByRole('button', { name: 'Review' })).toBeInTheDocument()
    expect(loads[0]).toContain('user=7')
    expect(screen.queryByRole('button', { name: /mark merged/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /send answer/i })).toBeNull()
  })

  it('names whose list it is for screen readers, not "you"', async () => {
    data = myWork('busy', { read_only: true, user_id: 7 })
    server.use(http.get('*/api/projects/2/members', () => HttpResponse.json({
      members: [{ user_id: 7, email: 'riley@example.com', display_name: 'Riley Lane' }], my_access: 'lead' })))
    at('/project/2/my-work?user=7')
    expect(await screen.findByRole('group', { name: 'Waiting on Riley Lane in AMP v2' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Waiting on Riley Lane' })).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Waiting on you' })).toBeNull()
    expect(screen.queryByRole('group', { name: /waiting on you/i })).toBeNull()
  })

  it('shows an error with Try again', async () => {
    server.use(http.get('*/api/projects/2/my-work', () => HttpResponse.json({ error: 'boom' }, { status: 500 })))
    at('/project/2/my-work')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent("Couldn't load My work: boom")
    expect(alert).toHaveTextContent('no counts are shown')
    expect(screen.queryByRole('status', { name: 'Loading My work' })).toBeNull()
    expect(screen.queryByRole('group', { name: /waiting on you/i })).toBeNull()
    server.use(http.get('*/api/projects/2/my-work', () => HttpResponse.json(data)))
    await userEvent.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('region', { name: 'Waiting on you' })).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('no AMP user, sign-in off: explains local dev and points at the Board, with no Try again', async () => {
    server.use(http.get('*/api/projects/2/my-work', () => HttpResponse.json({ error: 'My work needs a signed-in AMP user' }, { status: 400 })))
    at('/project/2/my-work', { subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: [] })
    const panel = await screen.findByRole('region', { name: 'My work is off: sign-in is turned off' })
    expect(panel).toHaveTextContent('Sign-in is turned off on this AMP')
    expect(panel).toHaveTextContent('OIDC_ISSUER_URL')
    expect(panel).not.toHaveTextContent('needs a signed-in AMP user')
    expect(within(panel).getByRole('link', { name: /Open the Board/ })).toHaveAttribute('href', '/project/2/board')
    expect(within(panel).queryByRole('link', { name: /Sign in/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('no AMP user while the identity is loading: keeps the loader, never the raw error or a Sign in guess', async () => {
    server.use(http.get('*/api/projects/2/my-work', () => HttpResponse.json({ error: 'My work needs a signed-in AMP user' }, { status: 400 })))
    let hits = 0
    server.events.on('response:mocked', ({ request }) => { if (new URL(request.url).pathname === '/api/projects/2/my-work') hits++ })
    at('/project/2/my-work', null, true)
    await waitFor(() => expect(hits).toBeGreaterThan(0))
    await new Promise(r => setTimeout(r, 20))
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByRole('link', { name: /Sign in/ })).toBeNull()
    expect(screen.queryByText(/needs a signed-in AMP user/)).toBeNull()
    server.events.removeAllListeners()
  })

  it('no AMP user, signed out: a Sign in link that comes back here', async () => {
    server.use(http.get('*/api/projects/2/my-work', () => HttpResponse.json({ error: 'My work needs a signed-in AMP user' }, { status: 400 })))
    at('/project/2/my-work', { subject: 'oidc|abc', email: 'jo@example.com', name: 'Jo', roles: [] })
    const panel = await screen.findByRole('region', { name: 'My work: not signed in' })
    expect(panel).toHaveTextContent('you are signed in as jo@example.com')
    expect(panel).toHaveTextContent('Ask an AMP admin to invite this email')
    expect(within(panel).getByRole('link', { name: /Sign in/ }).getAttribute('href')).toMatch(/^\/oauth2\/start\?rd=/)
    expect(within(panel).getByRole('link', { name: /Open the Board/ })).toHaveAttribute('href', '/project/2/board')
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
  })
})

describe('My work tab: ?focus=', () => {
  it('outlines the item that contains the ticket', async () => {
    at('/project/2/my-work?focus=2471')
    await screen.findByRole('region', { name: 'Waiting on you' })
    const item = document.querySelector('[data-item-key="task:2471"]')!
    expect(item).toHaveClass('focus')
    expect(within(item as HTMLElement).getByText('Opened from a link')).toBeInTheDocument()
    expect(document.querySelectorAll('.pmw-item.focus')).toHaveLength(1)
  })

  it('an approval contains its tickets: focusing #4 outlines the Approve card', async () => {
    at('/project/2/my-work?focus=4')
    await screen.findByRole('region', { name: 'Waiting on you' })
    expect(document.querySelector('[data-item-key="approve:172"]')).toHaveClass('focus')
  })

  it('outlines a Coming up row, and says when the ticket waits on nobody here', async () => {
    at('/project/2/my-work?focus=2131')
    await screen.findByRole('region', { name: 'Waiting on you' })
    expect(document.querySelector('a[data-item-key="task:2131"]')).toHaveClass('focus')
  })

  it('a ticket not in the list gets a note with a Board link; dismiss clears the param', async () => {
    at('/project/2/my-work?focus=999')
    expect(await screen.findByText(/#999 isn't waiting on you here/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Open #999 on the Board' })).toHaveAttribute('href', '/project/2/board?focus=999')
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(where()).toBe('/project/2/my-work')
  })

  it('the old ?ticket= alias is rewritten and focuses too', async () => {
    at('/project/2/my-work?ticket=2481')
    await screen.findByRole('region', { name: 'Waiting on you' })
    await waitFor(() => expect(where()).toBe('/project/2/my-work?focus=2481'))
    expect(document.querySelector('[data-item-key="task:2481"]')).toHaveClass('focus')
  })

  it('Review and approve opens the Approve view in place, and Back returns to the list', async () => {
    const router = at('/project/2/my-work')
    await userEvent.click(await screen.findByRole('button', { name: 'Review and approve 5' }))
    expect(router.state.location.search).toBe('?approve=172&focus=1')
    expect(await screen.findByRole('heading', { level: 1, name: /Human work UX/ })).toBeInTheDocument()
    await userEvent.click(screen.getAllByRole('button', { name: /my work/i })[0])
    expect(await screen.findByRole('region', { name: 'Waiting on you' })).toBeInTheDocument()
  })
})

describe('landing rule', () => {
  it('opens My work when something waits on you in the project', async () => {
    at('/project/2')
    await waitFor(() => expect(where()).toBe('/project/2/my-work'))
  })

  it('opens Mission when nothing waits on you there', async () => {
    counts = { counts: { waiting: 3, approve: 0, answer: 3, merge: 0, start: 0, do: 0, coming_up: 0 }, projects: { 5: { waiting: 3, approve: 0, answer: 3, merge: 0, start: 0, do: 0, coming_up: 0 } } }
    at('/project/2')
    expect(await screen.findByText('Mission page')).toBeInTheDocument()
    expect(where()).toBe('/project/2/mission')
    counts = { counts: { waiting: 5, approve: 1, answer: 1, merge: 1, start: 1, do: 1, coming_up: 1 }, projects: { 2: { waiting: 5, approve: 1, answer: 1, merge: 1, start: 1, do: 1, coming_up: 1 } } }
  })

  it('an explicit tab always wins', async () => {
    at('/project/2/mission')
    expect(await screen.findByText('Mission page')).toBeInTheDocument()
    expect(where()).toBe('/project/2/mission')
  })

  it('pure rule: tabs, old ticket links and ?approve=', () => {
    expect(landingTab('', 2)).toBe('my-work')
    expect(landingTab('', 0)).toBe('mission')
    expect(landingTab('', null)).toBe('mission')
    expect(landingTab('?focus=5', 2)).toBe('mission')
    expect(landingTab('?ticket=5', 2)).toBe('mission')
    expect(landingTab('?approve=172', 0)).toBe('my-work')
    expect(fixedTab('')).toBeNull()
    expect(landingPath(2, '?focus=5', 'mission')).toBe('/project/2/mission?focus=5')
    expect(landingPath(2, '', 'my-work')).toBe('/project/2/my-work')
  })
})

describe('badges', () => {
  it('the My work tab carries the project\'s count, hot when above 0', async () => {
    at('/project/2/mission')
    const { refreshWaitCounts } = await import('../lib/waitCounts')
    await refreshWaitCounts()
    const tab = await screen.findByRole('link', { name: 'My work, 5 waiting on you' })
    expect(tab).toHaveAttribute('href', '/project/2/my-work')
    expect(tab.querySelector('span')).toHaveClass('bg-accent')
    expect(screen.getByRole('link', { name: 'Mission' })).toHaveAttribute('href', '/project/2/mission')
  })
})

// The Merge card's states (AMP #3013, approved artifact #27 v2). One card per
// PR: the linked/lapsed-legacy/external cases the ticket names, each rendering
// exactly one action, with no duplicate and no acknowledgement.
describe('My work tab: merge card states', () => {
  const mergeList = () => screen.findByRole('region', { name: 'Waiting on you' })

  it('linked and gated: prerequisites on the card, "tracked by", and NO Mark merged', async () => {
    // The plan's step #2304 gates this PR: #2303 and #2131 are still open.
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [linkedMergeItem()], start: [], do: [] },
      counts: { waiting: 1, approve: 0, answer: 0, merge: 1, start: 0, do: 0, coming_up: 1 },
    })
    at('/project/2/my-work')
    const list = await mergeList()
    const card = within(list).getByRole('article', { name: /Merge: #2302/ })
    // The plan step is folded in: named, with its open prerequisites listed.
    expect(within(card).getByText(/Tracked by/)).toBeInTheDocument()
    expect(within(card).getAllByText(/#2304/).length).toBeGreaterThan(0)
    const prereqs = within(card).getByRole('list', { name: 'Merge prerequisites' })
    expect(within(prereqs).getByText('#2303')).toBeInTheDocument()
    expect(within(prereqs).getByText('#2131')).toBeInTheDocument()
    // One action only: Review. Mark merged is absent, not disabled — and it is
    // not offered anywhere else on the card either (never a duplicate).
    expect(within(list).queryAllByRole('button', { name: /mark merged/i })).toHaveLength(0)
    expect(within(card).getByRole('link', { name: /review pr #88/i })).toBeInTheDocument()
    // The card is inert while gated.
    expect(card).toHaveClass('inert')
  })

  it('linked and ready: one Mark merged, which records the merge and completes the step', async () => {
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [linkedMergeItem({ tracked_by: { task_id: 2304, name: 'Merge PR', state: 'backlog', ready: true } })], start: [], do: [] },
      counts: { waiting: 1, approve: 0, answer: 0, merge: 1, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    const list = await mergeList()
    const card = within(list).getByRole('article', { name: /Merge: #2302/ })
    expect(card).not.toHaveClass('inert')
    expect(within(card).getByText(/completes/)).toHaveTextContent(/Mark merged records the merge and completes #2304 in one step/)
    expect(within(list).getAllByRole('button', { name: /mark merged/i })).toHaveLength(1)
  })

  it('legacy (no contract): no invented target, and Mark merged stays the ordinary action', async () => {
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [legacyMergeItem()], start: [], do: [] },
      counts: { waiting: 1, approve: 0, answer: 0, merge: 1, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    const list = await mergeList()
    const card = within(list).getByRole('article', { name: /Merge: #2600/ })
    // A pre-contract item has no provenance, so no target is claimed at all.
    expect(within(card).getByTestId('merge-route')).toHaveTextContent('PR #77 · fix/older · target not verified')
    expect(within(card).queryByText(/→ main/)).toBeNull()
    // An unreported waiter set must not read as "nothing waits".
    expect(within(card).queryByText(/Nothing waits on this merge/)).toBeNull()
    expect(within(card).getByRole('button', { name: /mark merged/i })).toHaveClass('amp-btn-primary')
  })

  it('external: an already-recorded merge is in Done today, never in the list', async () => {
    // The record happened elsewhere (GitHub's webhook), so the card is gone:
    // that absence is the design, and nothing is owed.
    data = myWork('done', {
      done_today: [{ kind: 'merge', task_id: 2302, task_ids: [2302], name: 'Open PR: harness on the public home page', at: new Date().toISOString(), count: 1, merge_source: 'github', merge_state: 'merged' }],
      items: { approve: [], answer: [], merge: [], start: [], do: [] },
      counts: { waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    expect(await screen.findByText('Merged #2302: Open PR: harness on the public home page')).toBeInTheDocument()
    expect(screen.getByText('Recorded by GitHub')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /mark merged/i })).toBeNull()
    expect(screen.queryByRole('region', { name: 'Waiting on you' })).toBeNull()
  })

  it('abandoned and nothing-waits: the two states with no primary merge action', async () => {
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [
        mergeItem({ key: 'task:2471', merge_state: 'abandoned', holding_up: [],
          task: task(2471, { state: 'completed', completed_at: new Date().toISOString(), merge_state: 'abandoned' }) }),
        mergeItem({ key: 'task:2472', task_id: 2472, name: 'Quiet PR', holding_up: [], not_waiting: [2492] }),
      ], start: [], do: [] },
      counts: { waiting: 2, approve: 0, answer: 0, merge: 2, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    const list = await mergeList()
    expect(within(list).getAllByText(/PR closed without merging/).length).toBeGreaterThan(0)
    // Closed PR: Reopen and Not needed, never Mark merged.
    expect(within(list).getAllByRole('button', { name: /mark merged/i })).toHaveLength(1)
    // The quiet one keeps Mark merged, demoted, because a merge still has to be
    // recorded somewhere — but it is not the headline action.
    const quiet = within(list).getByRole('article', { name: /Merge: #2472/ })
    expect(within(quiet).getByRole('button', { name: /mark merged/i })).not.toHaveClass('amp-btn-primary')
    expect(within(quiet).getByTestId('merge-waiters')).toHaveTextContent('Nothing waits on this merge')
    expect(within(quiet).getByText(/#2492 opted out/)).toBeInTheDocument()
  })
})

// The classifier's arms end to end (AMP #3158, approved artifact #27 v2). The
// server decides the arm; the page must render it and never invent one: no
// default branch from a branch name, no Mark merged where a rule demands an
// approval on GitHub, and no internal work as a merge chore.
describe('My work tab: the merge classifier\'s arms', () => {
  const mergeList = () => screen.findByRole('region', { name: 'Waiting on you' })
  const pr = 'https://github.com/o/r/pull/96'

  it('default branch: the reason and its source, and the ordinary Mark merged', async () => {
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [alertMergeItem({ pr_url: pr, branch: 'feat/usage-caps' })], start: [], do: [] },
      counts: { waiting: 1, approve: 0, answer: 0, merge: 1, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    const card = within(await mergeList()).getByRole('article', { name: /Merge: #2471/ })
    expect(within(card).getByTestId('merge-why')).toHaveTextContent("the PR targets main, the repository's default branch")
    expect(within(card).getByTestId('merge-chip')).toHaveTextContent('default: main · detected from GitHub')
    expect(within(card).getByTestId('merge-chip')).not.toHaveClass('warn')
    expect(within(card).getByTestId('merge-route')).toHaveTextContent('PR #96 · feat/usage-caps → main')
    expect(within(card).getByRole('button', { name: /mark merged/i })).toHaveClass('amp-btn-primary')
    expect(within(card).queryByTestId('merge-rule')).toBeNull()
  })

  it('rule: the rule box, the GitHub review as the primary action, and NO Mark merged anywhere', async () => {
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [alertMergeItem({
        pr_url: pr, branch: 'amp-2955', base_branch: 'feat/billing',
        alert: 'rule', alert_reason: 'the PR targets feat/billing, and a rule requires your approval',
        rule: 'organization ruleset billing: 1 approving review, a code owner\'s approval — you are a code owner of the changed paths',
        review_url: pr,
      })], start: [], do: [] },
      counts: { waiting: 1, approve: 0, answer: 0, merge: 1, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    const list = await mergeList()
    const card = within(list).getByRole('article', { name: /Merge: #2471/ })
    expect(within(card).getByTestId('merge-rule')).toHaveTextContent('you are a code owner of the changed paths')
    const review = within(card).getByTestId('merge-review')
    expect(review).toHaveTextContent('Review and approve PR #96')
    expect(review).toHaveClass('amp-btn-primary')
    // The ticket says it outright: rule cards lack Mark merged.
    expect(within(list).queryAllByRole('button', { name: /mark merged/i })).toHaveLength(0)
    expect(within(card).queryByRole('button', { name: /not needed/i })).toBeNull()
    // And the card is not inert: it is the person's, it is just a review.
    expect(card).not.toHaveClass('inert')
  })

  it('unknown: the warning chip and the card as built, never hidden', async () => {
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [alertMergeItem({
        pr_url: pr, branch: 'amp-2960', base_branch: 'trunk', target_source: 'unknown',
        alert: 'unknown', alert_reason: "the repository's default branch could not be read", default_branch: '',
        holding_up: [2975],
      })], start: [], do: [] },
      counts: { waiting: 1, approve: 0, answer: 0, merge: 1, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    const card = within(await mergeList()).getByRole('article', { name: /Merge: #2471/ })
    expect(within(card).getByTestId('merge-why')).toHaveTextContent("the repository's default branch could not be read")
    expect(within(card).getByTestId('merge-chip')).toHaveClass('warn')
    expect(within(card).getByTestId('merge-chip')).toHaveTextContent('default branch & rules: unknown')
    // The run's hint never becomes a claimed target on an unknown verdict.
    expect(within(card).getByTestId('merge-route')).toHaveTextContent('target not verified')
    expect(within(card).queryByText(/→ trunk/)).toBeNull()
    expect(within(card).getByTestId('merge-waiters')).toHaveTextContent('1 ticket waits on this merge: #2975.')
    expect(within(card).getByRole('button', { name: /mark merged/i })).toBeInTheDocument()
  })

  it('internal: an inert card with no action, never a merge chore', async () => {
    // The server drops internal items; this is the belt-and-braces case where
    // one arrives anyway. It must offer nothing a person could click.
    data = myWork('busy', {
      items: { approve: [], answer: [], merge: [alertMergeItem({
        pr_url: pr, branch: 'amp-2968', base_branch: 'feat/harness-home-page',
        alert: 'internal', alert_reason: "feat/harness-home-page is not the repository's default branch (main), and no rule requires your approval",
        holding_up: [2302],
      })], start: [], do: [] },
      counts: { waiting: 1, approve: 0, answer: 0, merge: 1, start: 0, do: 0, coming_up: 0 },
    })
    at('/project/2/my-work')
    const list = await mergeList()
    const card = within(list).getByRole('article', { name: /Merge: #2471/ })
    expect(card).toHaveClass('inert')
    expect(within(card).queryByRole('button', { name: /mark merged/i })).toBeNull()
    expect(within(card).queryByRole('button', { name: /not needed/i })).toBeNull()
    expect(within(card).queryByTestId('merge-chip')).toBeNull()
    expect(within(card).queryByTestId('merge-rule')).toBeNull()
  })
})

// main's #136/#140: the list loads in bands, and each card says in one line
// what the item needs from you.
describe('My work tab: loading', () => {
  it('draws the project nav at once and the list skeleton only once the load runs past 250ms', async () => {
    let release!: () => void
    const gate = new Promise<void>(r => { release = r })
    server.use(http.get('*/api/projects/2/my-work', async () => { await gate; return HttpResponse.json(data) }))
    at('/project/2/my-work')
    expect(screen.getByRole('link', { name: 'Mission' })).toBeInTheDocument()
    const region = screen.getByRole('status', { name: 'Loading My work' })
    expect(region).toHaveAttribute('aria-busy', 'true')
    expect(document.querySelector('.amp-band-loader')).toBeNull()
    expect(region.querySelector('.amp-skel')).toBeNull()
    await waitFor(() => expect(region.querySelector('.amp-skel')).not.toBeNull())
    release()
    await screen.findByRole('region', { name: 'Waiting on you' })
    expect(screen.queryByRole('status', { name: 'Loading My work' })).toBeNull()
  })

  it('shows no skeleton for a fast load', async () => {
    at('/project/2/my-work')
    await screen.findByRole('region', { name: 'Waiting on you' })
    expect(document.querySelector('.amp-skel')).toBeNull()
  })
})

// Each card says, in one line under its title, what the item needs from you.
describe('My work tab: what each item needs from you', () => {
  const withDo = (over: Partial<Task>) => {
    const d = myWork('busy')
    d.items.do = [{ ...doItem(), human_reason: 'judgment', task: task(2481, { performer: 'human', human_reason: 'judgment', ...over }) }]
    return d
  }
  const needsOf = (key: string) => document.querySelector(`[data-item-key="${key}"]`)!.querySelectorAll('[data-needs]')

  it('every non-approve card has exactly one needs line; the Approve card has none', async () => {
    at('/project/2/my-work')
    await screen.findByRole('region', { name: 'Waiting on you' })
    expect(needsOf('approve:172')).toHaveLength(0)
    const lines = Object.fromEntries(['task:2473', 'task:2471', 'task:2803', 'task:2481'].map(k => {
      const found = needsOf(k)
      expect(found).toHaveLength(1)
      return [k, found[0].textContent]
    }))
    expect(lines).toEqual({
      'task:2473': 'Answer: Approved as is, or request changes?',
      'task:2471': 'Merge PR #88, 2 tickets wait',
      'task:2803': 'Launch its run, or return #2803 to ready',
      'task:2481': 'Do with your access: d',
    })
  })

  it('a judgment ticket says "Decide" with the first sentence of its brief, and folds the brief', async () => {
    data = withDo({ description: 'Pick the Q1 targets. Options are in the planning doc.', acceptance_criteria: 'Three targets recorded' })
    at('/project/2/my-work')
    await screen.findByRole('region', { name: 'Waiting on you' })
    const card = document.querySelector('[data-item-key="task:2481"]') as HTMLElement
    expect(within(card).getByText('Decide: Pick the Q1 targets.')).toBeInTheDocument()
    const brief = card.querySelector('details')!
    expect(brief).not.toHaveAttribute('open')
    await userEvent.click(within(card).getByText('Show brief'))
    expect(brief).toHaveAttribute('open')
    expect(within(brief).getByText('Pick the Q1 targets. Options are in the planning doc.')).toBeVisible()
    expect(within(brief).getByText('Three targets recorded')).toBeVisible()
  })

  it('a ticket with no description says so, with no brief to show', async () => {
    data = withDo({ description: '', acceptance_criteria: '' })
    at('/project/2/my-work')
    await screen.findByRole('region', { name: 'Waiting on you' })
    const card = document.querySelector('[data-item-key="task:2481"]') as HTMLElement
    expect(within(card).getByText('No instructions: open the ticket')).toBeInTheDocument()
    expect(card.querySelector('details')).toBeNull()
  })
})

describe('My work tab: held receipts (#126)', () => {
  it('shows a verify item from the server, with why and a link to Accept or Reject on the ticket', async () => {
    const held: ProjectWaitItem = {
      project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2', kind: 'verify', key: 'task:77', task_id: 77, task_ids: [77],
      name: 'Settings page', since: new Date(Date.now() - 3_600_000).toISOString(), state: 'in_progress', agent_id: 'amp-worker',
      hold_reason: 'no checks were recorded', task: task(77, { state: 'in_progress', agent_id: 'amp-worker' }),
    }
    data = myWork('empty')
    data.items.verify = [held]
    data.counts = { ...data.counts, verify: 1, waiting: 1 }
    at('/project/2/my-work')
    const card = await screen.findByRole('article', { name: /Verify: #77 Settings page/ })
    expect(card).toHaveTextContent("Couldn't verify #77: no checks were recorded. Accept or reject it on the ticket")
    expect(within(card).getByRole('link', { name: 'Open #77 to accept or reject' })).toHaveAttribute('href', '/project/2/board?focus=77')
  })
})
