// The project's My work tab (KB decisions/human-work-ux.md §2, artifact #23)
// and the landing rule (/project/:id opens My work when something waits on you).
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { ProjectMyWork } from './ProjectMyWork'
import { ProjectLanding } from './ProjectLanding'
import { ProjectNav } from '../components/ProjectNav'
import { fixedTab, landingPath, landingTab } from '../lib/landing'
import { resetWaitCounts } from '../lib/waitCounts'
import { myWork } from '../test/myWorkFixture'
import type { MyWorkCounts, ProjectMyWork as Data } from '../types'

let data: Data = myWork('busy')
let counts: MyWorkCounts = { counts: { waiting: 4, approve: 1, answer: 1, merge: 1, do: 1, coming_up: 1 }, projects: { 2: { waiting: 4, approve: 1, answer: 1, merge: 1, do: 1, coming_up: 1 } } }
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

function at(url: string) {
  const router = createMemoryRouter([
    { path: '/project/:projectId', element: <><ProjectLanding /><Where /></> },
    { path: '/project/:projectId/my-work', element: <><ProjectMyWork /><Where /></> },
    { path: '/project/:projectId/mission', element: <><ProjectNav projectId={2} currentView="mission" /><p>Mission page</p><Where /></> },
    { path: '/project/:projectId/board', element: <><p>Board page</p><Where /></> },
  ], { initialEntries: [url] })
  render(<RouterProvider router={router} />)
  return router
}
const where = () => screen.getByTestId('where').textContent

describe('My work tab: states', () => {
  it('busy: readouts, the items in kind order with their actions, and the side panels', async () => {
    at('/project/2/my-work')
    const list = await screen.findByRole('region', { name: 'Waiting on you' })
    const ro = screen.getByRole('group', { name: 'Waiting on you in AMP v2' })
    expect(within(ro).getByText('Waiting on you').parentElement).toHaveClass('alarm')
    expect(within(ro).getByText('Waiting on you').nextSibling).toHaveTextContent('4')
    expect(within(ro).getByText(/^Oldest: 3h, the plan approval$/)).toBeInTheDocument()
    expect(within(ro).getByText('plan · 5 tickets')).toBeInTheDocument()

    const kinds = Array.from(list.querySelectorAll('[data-item-key]')).map(e => e.getAttribute('data-item-key'))
    expect(kinds).toEqual(['approve:172', 'task:2473', 'task:2471', 'task:2481'])
    expect(within(list).getByRole('button', { name: 'Review and approve 5' })).toBeInTheDocument()
    expect(within(list).getByRole('button', { name: /send answer/i })).toBeInTheDocument()
    expect(within(list).getByRole('button', { name: /mark merged/i })).toBeInTheDocument()
    expect(within(list).getByText('2 tickets wait on the merge')).toBeInTheDocument()
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

  it('shows an error with Try again', async () => {
    server.use(http.get('*/api/projects/2/my-work', () => HttpResponse.json({ error: 'boom' }, { status: 500 })))
    at('/project/2/my-work')
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load My work")
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
    counts = { counts: { waiting: 3, approve: 0, answer: 3, merge: 0, do: 0, coming_up: 0 }, projects: { 5: { waiting: 3, approve: 0, answer: 3, merge: 0, do: 0, coming_up: 0 } } }
    at('/project/2')
    expect(await screen.findByText('Mission page')).toBeInTheDocument()
    expect(where()).toBe('/project/2/mission')
    counts = { counts: { waiting: 4, approve: 1, answer: 1, merge: 1, do: 1, coming_up: 1 }, projects: { 2: { waiting: 4, approve: 1, answer: 1, merge: 1, do: 1, coming_up: 1 } } }
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
    const tab = await screen.findByRole('link', { name: 'My work, 4 waiting on you' })
    expect(tab).toHaveAttribute('href', '/project/2/my-work')
    expect(tab.querySelector('span')).toHaveClass('bg-accent')
    expect(screen.getByRole('link', { name: 'Mission' })).toHaveAttribute('href', '/project/2/mission')
  })
})
