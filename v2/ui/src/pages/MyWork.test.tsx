// Top-level My work is a summary (v2/kb/working/human-work.md): no action
// buttons, every row a link into /project/:id/my-work?focus=<ticket id>.
import { render, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { MyWork } from './MyWork'
import { AuthContext } from '../contexts/auth-context'
import { RemoteProvider } from '../remote/RemoteProvider'
import { httpTransport } from '../remote/transport'
import { unpairedCrypto } from '../remote/crypto'
import type { GettingStarted, Me, MyWork as MyWorkData, WaitCounts, WaitItem, WorkTask } from '../types'
import userEvent from '@testing-library/user-event'

const hourAgo = new Date(Date.now() - 3_600_000).toISOString()
const counts = (c: Partial<WaitCounts>): WaitCounts => {
  const base = { approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0, ...c }
  return { ...base, waiting: base.approve + base.answer + base.merge + base.start + base.do }
}
const item = (over: Partial<WaitItem>): WaitItem => ({
  kind: 'answer', key: `task:${over.task_id}`, project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2',
  task_ids: over.task_id ? [over.task_id] : [], name: 'A ticket', since: hourAgo, ...over,
})
const agentTicket = { id: 900, project_id: 5, project_code: 'HOME', project_name: 'Home OS', name: 'Agent work',
  state: 'in_progress', performer: 'agent', dispatched_at: new Date().toISOString(), updated_at: new Date().toISOString() } as unknown as WorkTask

const busy: MyWorkData = {
  owned: [agentTicket], flags: [], agents: [], done_24h: 3,
  counts: counts({ approve: 1, answer: 2, merge: 2, do: 3, coming_up: 1 }),
  projects: [
    {
      project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2', agents_in_progress: 0, more: 2,
      counts: counts({ approve: 1, answer: 1, merge: 2, do: 2 }),
      rows: [
        item({ kind: 'approve', key: 'approve:172', task_ids: [2475, 2477], name: 'Human work UX', epic_id: 172, count: 2 }),
        item({ kind: 'answer', task_id: 2473, name: 'Review the mockup', question: 'Which one?' }),
        item({ kind: 'merge', task_id: 2471, name: 'Board Mine filter', pr_url: 'https://github.com/o/r/pull/88', holding_up: [2480, 2481] }),
        item({ kind: 'merge', task_id: 2470, name: 'My Work questions' }),
        item({ kind: 'do', task_id: 2481, name: 'Rotate the Dex secret', state: 'backlog', performer: 'human', human_reason: 'access' }),
        item({ kind: 'do', task_id: 2482, name: 'Second thing', state: 'in_progress', performer: 'human' }),
      ],
    },
    {
      project_id: 3, project_code: 'CORE', project_name: 'Core API', agents_in_progress: 0, more: 0,
      counts: counts({ answer: 1, do: 1 }),
      rows: [item({ project_id: 3, project_code: 'CORE', project_name: 'Core API', task_id: 1902, name: 'Which FAA snapshot?' }),
        item({ kind: 'do', project_id: 3, project_code: 'CORE', project_name: 'Core API', task_id: 1903, name: 'Call the vendor' })],
    },
    { project_id: 5, project_code: 'HOME', project_name: 'Home OS', agents_in_progress: 1, more: 0, counts: counts({ coming_up: 1 }), rows: [] },
  ],
  projects_total: 3, projects_offset: 0, projects_limit: 50, projects_more: false, rows_per_project: 6,
  clear_projects: Array.from({ length: 10 }, (_, i) => ({ id: 100 + i, code: `P${i}`, name: `Quiet ${i}` })),
  agents_in_progress: 1,
}

let payload: MyWorkData = busy
const DONE = { done: true, at: hourAgo }
const allDone: GettingStarted = { agent: DONE, project: { ...DONE, project_id: 2 }, plan: { ...DONE, project_id: 2 }, receipt: { ...DONE, project_id: 2, task_id: 12 } }
let started: GettingStarted = allDone
const server = setupServer(
  http.get('*/api/my-work', () => HttpResponse.json(payload)),
  http.get('*/api/my-work/getting-started', () => HttpResponse.json(started)),
  http.get('*/api/velocity', () => HttpResponse.json({ days: [] })),
  http.get('*/api/day-reports/*', () => HttpResponse.json({ error: 'none' }, { status: 404 })),
  http.get('*/api/artifacts/expiring', () => HttpResponse.json({ artifacts: [
    { id: 23, title: 'Human work UX mockup', kind: 'html', status: 'in_review', expires_at: new Date(Date.now() + 5 * 86_400_000).toISOString() },
  ] })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
afterEach(() => { payload = busy; started = allDone; localStorage.clear() })
afterAll(() => server.close())

function renderMyWork(me: Me = { subject: 's', email: 'jo@example.com', name: 'Jo', roles: [], user_id: 7 }, url = '/') {
  return render(
    <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: false }}>
      <RemoteProvider deps={{ transport: httpTransport, crypto: unpairedCrypto, mock: false }}>
        <MemoryRouter initialEntries={[url]}><MyWork /></MemoryRouter>
      </RemoteProvider>
    </AuthContext.Provider>,
  )
}

describe('top-level My work summary', () => {
  it('has no action buttons: nothing is a button or styled as one', async () => {
    const { container } = renderMyWork()
    await screen.findByRole('heading', { name: 'AMP v2' })
    await screen.findByText('Human work UX mockup') // the expiring artifact, as a link (no Keep button here)
    expect(screen.queryAllByRole('button')).toHaveLength(0)
    expect(container.querySelectorAll('button, .amp-btn, [role="button"]')).toHaveLength(0)
    for (const name of [/^start$/i, /^mark done/i, /^answer$/i, /^keep$/i]) {
      expect(screen.queryByRole('link', { name })).toBeNull()
    }
  })

  it('cards are ordered most waiting first, with kind counts and coming up', async () => {
    renderMyWork()
    const headings = await screen.findAllByRole('heading', { level: 2 })
    expect(headings.map(h => h.textContent)).toEqual(['AMP v2', 'Core API'])
    const amp = screen.getByRole('article', { name: /AMP v2: 6 waiting on you/ })
    expect(amp.textContent).toMatch(/Approve 1.*Answer 1.*Merge 2.*Do 2/)
    expect(screen.getByRole('article', { name: /Core API: 2 waiting/ }).textContent).toMatch(/Approve 0.*Answer 1/)
    expect(within(amp).getByRole('link', { name: 'Open My work in AMP v2, 6 waiting' })).toHaveAttribute('href', '/project/2/my-work')
  })

  it('every row links into the project My work tab with ?focus=', async () => {
    renderMyWork()
    const amp = await screen.findByRole('article', { name: /AMP v2/ })
    const rows = within(amp).getAllByRole('listitem').map(li => within(li).getByRole('link'))
    const hrefs = rows.map(a => a.getAttribute('href'))
    expect(hrefs.slice(0, 6)).toEqual([
      '/project/2/my-work?focus=2475', // an approval focuses its first ticket
      '/project/2/my-work?focus=2473',
      '/project/2/my-work?focus=2471',
      '/project/2/my-work?focus=2470',
      '/project/2/my-work?focus=2481',
      '/project/2/my-work?focus=2482',
    ])
    expect(within(amp).getByText('Approve the plan: Human work UX (2 tickets)')).toBeInTheDocument()
    expect(within(amp).getByText('#2471 Merge PR #88: Board Mine filter')).toBeInTheDocument()
    expect(within(amp).getByText(/2 tickets wait/)).toBeInTheDocument()
    // No silent truncation.
    expect(within(amp).getByRole('link', { name: '+2 more in AMP v2 ›' })).toHaveAttribute('href', '/project/2/my-work')
    const core = screen.getByRole('article', { name: /Core API/ })
    expect(within(core).getByRole('link', { name: /#1902/ })).toHaveAttribute('href', '/project/3/my-work?focus=1902')
  })

  it('lists projects with nothing for you, compactly', async () => {
    renderMyWork()
    await screen.findByText('Nothing waiting on you')
    expect(screen.getByRole('link', { name: /Home OS.*1 coming up/ })).toHaveAttribute('href', '/project/5/my-work')
    expect(screen.getByRole('link', { name: /Quiet 0/ })).toHaveAttribute('href', '/project/100/my-work')
    expect(screen.queryByText('Quiet 9')).toBeNull()
    expect(screen.getByRole('link', { name: /2 more projects with nothing for you/ })).toBeInTheDocument()
  })

  it('shows a calm empty state when nothing waits', async () => {
    payload = { owned: [], flags: [], agents: [], done_24h: 0, counts: counts({}), projects: [], projects_total: 0,
      clear_projects: [{ id: 2, code: 'AMPV2', name: 'AMP v2' }], agents_in_progress: 0 }
    renderMyWork()
    expect(await screen.findByText('No project needs you right now.')).toBeInTheDocument()
    expect(screen.getByText('in all 1 project')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /AMP v2/ })).toHaveAttribute('href', '/project/2/my-work')
    expect(screen.queryAllByRole('button')).toHaveLength(0)
  })

  it('each row\'s sub-line leads with what it needs from you (needsLine)', async () => {
    renderMyWork()
    const amp = await screen.findByRole('article', { name: /AMP v2/ })
    const sub = (title: string) => within(amp).getByText(title).closest('a')?.lastElementChild?.textContent
    expect(sub('#2473 Review the mockup')).toBe('Answer: Which one? · waiting 1h')
    expect(sub('#2471 Merge PR #88: Board Mine filter')).toBe('Merge PR #88, 2 tickets wait · done 1h ago')
    expect(sub('#2470 Merge: My Work questions')).toBe('Merge its work · done 1h ago')
    expect(sub('#2481 Rotate the Dex secret')).toBe('Do with your access: open the ticket for the brief · ready for 1h')
    expect(sub('#2482 Second thing')).toBe('Do: open the ticket for the brief · started 1h ago')
  })

  it('lists plans waiting for approval above the project cards, across projects (#72)', async () => {
    const plan = (pid: number, code: string, name: string, epic: number, ids: number[]) =>
      item({ kind: 'approve', key: `approve:${epic}`, project_id: pid, project_code: code, project_name: name, task_ids: ids, name: `Plan ${epic}`, epic_id: epic, count: ids.length })
    payload = {
      ...busy,
      counts: counts({ approve: 3, answer: 3 }),
      projects: [
        { project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2', agents_in_progress: 0, more: 0, counts: counts({ answer: 3 }),
          rows: [item({ task_id: 1 }), item({ task_id: 2 }), item({ task_id: 3 })] },
        { project_id: 3, project_code: 'CORE', project_name: 'Core API', agents_in_progress: 0, more: 0, counts: counts({ approve: 2 }),
          rows: [plan(3, 'CORE', 'Core API', 50, [10, 11]), plan(3, 'CORE', 'Core API', 51, [12])] },
      ],
    }
    renderMyWork()
    const panel = await screen.findByRole('region', { name: 'Plans waiting for your approval' })
    const links = within(panel).getAllByRole('link')
    expect(links.map(a => a.getAttribute('href'))).toEqual(['/project/3/my-work?focus=10', '/project/3/my-work?focus=12'])
    expect(within(panel).getByText('Core API: Plan 50')).toBeInTheDocument()
    expect(within(panel).getByText(/^Review and approve 2 tickets · planned/)).toBeInTheDocument()
    // The server counts 3; only 2 are in the cards, so the panel says one is elsewhere.
    expect(within(panel).getByText(/\+1 more plan not listed here/)).toBeInTheDocument()
    // It comes before the project cards, even though Core API's card is second.
    const cards = screen.getByRole('article', { name: /AMP v2/ })
    expect(panel.compareDocumentPosition(cards) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(screen.queryAllByRole('button')).toHaveLength(0)
  })

  it('shows no approval panel when no plan waits on you', async () => {
    payload = { ...busy, counts: counts({ answer: 1 }), projects: [busy.projects![1]] }
    renderMyWork()
    await screen.findByRole('article', { name: /Core API/ })
    expect(screen.queryByRole('region', { name: 'Plans waiting for your approval' })).toBeNull()
  })

  it('still renders against an older server without the summary', async () => {
    payload = { owned: [], flags: [], agents: [], done_24h: 0 }
    renderMyWork()
    expect(await screen.findByText('No project needs you right now.')).toBeInTheDocument()
  })
})

// Dispatch integrity in the summary (AMP #2801, story 444): a ticket of yours
// dispatched to an agent that no worker started is a "start" row, with the
// server's minutes, linking to where the action lives.
describe('top-level My work: a dispatched ticket no worker started', () => {
  it('counts it, names the agent and the minutes, and links into My work', async () => {
    const started = item({
      kind: 'start', task_id: 2803, name: 'UI: not started states', state: 'not_started',
      agent_id: 'amp:amp-worker-frontend', not_started_minutes: 32,
    })
    payload = {
      ...busy,
      counts: counts({ start: 1 }),
      projects: [{ project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2', agents_in_progress: 0, more: 0,
        counts: counts({ start: 1 }), rows: [started] }],
    }
    renderMyWork()
    const amp = await screen.findByRole('article', { name: /AMP v2/ })
    expect(amp.textContent).toMatch(/Start 1/)
    expect(within(amp).getByText('Launch its run, or return #2803 to ready · dispatched to amp:amp-worker-frontend 32 min ago, no worker has started it')).toBeInTheDocument()
    expect(within(amp).getByRole('link', { name: /#2803/ })).toHaveAttribute('href', '/project/2/my-work?focus=2803')
  })
})

describe('top-level My work: a completion held for you (#126)', () => {
  it('counts it as Verify, says why, and links into My work', async () => {
    const held = item({ kind: 'verify', task_id: 77, name: 'Settings page', state: 'in_progress', hold_reason: 'no checks were recorded' })
    payload = {
      ...busy,
      counts: { ...counts({}), verify: 1, waiting: 1 },
      projects: [{ project_id: 2, project_code: 'AMPV2', project_name: 'AMP v2', agents_in_progress: 0, more: 0,
        counts: { ...counts({}), verify: 1, waiting: 1 }, rows: [held] }],
    }
    renderMyWork()
    const amp = await screen.findByRole('article', { name: /AMP v2/ })
    expect(amp.textContent).toMatch(/Verify 1/)
    expect(within(amp).getByText(/Couldn't verify #77: no checks were recorded/)).toBeInTheDocument()
    expect(within(amp).getByRole('link', { name: /#77/ })).toHaveAttribute('href', '/project/2/my-work?focus=77')
  })
})

describe('top-level My work loading', () => {
  it('draws the header at once and the summary skeleton only once the load runs past 250ms', async () => {
    let release!: () => void
    const gate = new Promise<void>(r => { release = r })
    server.use(http.get('*/api/my-work', async () => { await gate; return HttpResponse.json(payload) }))
    const { container } = renderMyWork()
    expect(screen.getByRole('heading', { name: 'My work', level: 1 })).toBeInTheDocument()
    expect(screen.getByRole('status', { name: 'Loading your work' })).toHaveAttribute('aria-busy', 'true')
    expect(container.querySelector('.amp-band-loader')).toBeNull()
    expect(container.querySelector('.amp-skel')).toBeNull()
    await waitFor(() => expect(container.querySelector('.amp-skel')).not.toBeNull())
    release()
    await screen.findByRole('heading', { name: 'AMP v2' })
    expect(screen.queryByRole('status', { name: 'Loading your work' })).toBeNull()
    server.resetHandlers()
  })

  it('shows no skeleton for a fast load', async () => {
    const { container } = renderMyWork()
    await screen.findByRole('heading', { name: 'AMP v2' })
    expect(container.querySelector('.amp-skel')).toBeNull()
  })
})

// The server answers an empty list when there's no AMP user, and a load can
// fail: neither may read as "0 waiting" or "in all 0 projects" (critique #1).
const EMPTY: MyWorkData = { ...busy, owned: [], counts: counts({}), projects: [], clear_projects: [], projects_total: 0, agents_in_progress: 0, done_24h: 0 }

describe('top-level My work: no zeros when there is nothing to count', () => {
  it('sign-in off (local dev): explains it and points at a project, with no counts', async () => {
    payload = EMPTY
    renderMyWork({ subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: [] })
    const panel = await screen.findByRole('region', { name: 'My work is off: sign-in is turned off' })
    expect(panel).toHaveTextContent('Sign-in is turned off on this AMP')
    expect(within(panel).getByRole('link', { name: /Open a project/ })).toHaveAttribute('href', '/projects')
    expect(screen.queryByLabelText('Waiting on you, across your projects')).toBeNull()
    expect(screen.queryByText(/in all 0 projects/)).toBeNull()
    expect(screen.queryByText('No project needs you right now.')).toBeNull()
  })

  it('signed out: says so and offers Sign in', async () => {
    payload = EMPTY
    renderMyWork({ subject: 'oidc|abc', email: 'jo@example.com', name: 'Jo', roles: [] })
    const panel = await screen.findByRole('region', { name: 'My work: not signed in' })
    expect(within(panel).getByRole('link', { name: /Sign in/ }).getAttribute('href')).toMatch(/^\/oauth2\/start\?rd=/)
    expect(screen.queryByLabelText('Waiting on you, across your projects')).toBeNull()
  })

  it('a failed load shows the error and Try again, never the zero tiles', async () => {
    server.use(http.get('*/api/my-work', () => HttpResponse.json({ error: 'database is down' }, { status: 500 }), { once: true }))
    renderMyWork()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent("Couldn't load My work: database is down")
    expect(alert).toHaveTextContent('no counts are shown')
    expect(screen.queryByRole('status', { name: 'Loading your work' })).toBeNull()
    expect(screen.queryByLabelText('Waiting on you, across your projects')).toBeNull()
    expect(screen.queryByText('No project needs you right now.')).toBeNull()
    await userEvent.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await screen.findByLabelText('Waiting on you, across your projects')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('My work day-report panel (#133)', () => {
  afterEach(() => server.resetHandlers())
  it('renders the summary as markdown: a list is a list', async () => {
    server.use(http.get('*/api/day-reports/draft', () => HttpResponse.json({ report: {
      user_id: 7, user_email: 'jo@example.com', user_name: 'Jo', day: '2026-11-05', tz: 'UTC', summary: '- a\n- b', saved: false, markdown: '',
      data: { completed: [], started: [], created: [], in_progress: [], blocked: [], touched: [], generated_at: '',
        stats: { completed: 1, started: 0, created: 0, in_progress: 0, blocked: 0, projects: 1 } } } })))
    renderMyWork()
    const summary = await screen.findByTestId('day-summary')
    const list = summary.querySelector('ul')
    expect(list).not.toBeNull()
    expect(list!.querySelectorAll('li')).toHaveLength(2)
  })
})

// The Get started checklist (#128) sits where the queue will be, on an empty
// My work, until every step that can be done here is done. Its fourth step,
// seeing a receipt, arrived with verification receipts (#149).
describe('top-level My work: Get started', () => {
  const fresh: GettingStarted = { agent: { done: false }, project: { done: false }, plan: { done: false }, receipt: { done: false } }
  const empty: MyWorkData = { owned: [], flags: [], agents: [], done_24h: 0, counts: counts({}), projects: [], projects_total: 0, clear_projects: [], agents_in_progress: 0 }

  it('shows the four steps on a fresh instance, with the MCP address and how to connect', async () => {
    payload = empty
    started = fresh
    renderMyWork()
    const list = await screen.findByRole('region', { name: 'Set up AMP in four steps' })
    expect(within(list).getByText('0 of 4 done')).toBeInTheDocument()
    expect(within(list).getByLabelText('MCP address')).toHaveTextContent(`${window.location.origin}/mcp`)
    expect(within(list).getByRole('button', { name: 'Copy address' })).toBeInTheDocument()
    expect(within(list).getByRole('link', { name: /How to connect your agent/ })).toHaveAttribute('href', '/guide?doc=setup/connect-your-agent.md')
    expect(within(list).getByRole('link', { name: 'Create a project' })).toHaveAttribute('href', '/projects?new=1')
    expect(within(list).getAllByRole('link', { name: 'manager agent' })[0]).toHaveAttribute('href', '/guide?doc=setup/connect-your-agent.md')
    expect(within(list).getByLabelText('Step 4: See your first receipt, to do')).toBeInTheDocument()
    expect(within(list).getByRole('link', { name: /How receipts work/ })).toHaveAttribute('href', '/guide?doc=working/verification-receipts.md')
  })

  it('checks steps from the server and links the plan step to the project', async () => {
    payload = empty
    started = { ...fresh, agent: { done: true, at: hourAgo, client: 'harness' }, project: { done: true, at: hourAgo, project_id: 9 }, plan: { done: false, project_id: 9 } }
    renderMyWork()
    const list = await screen.findByRole('region', { name: 'Set up AMP in four steps' })
    expect(within(list).getByText('2 of 4 done')).toBeInTheDocument()
    expect(within(list).getByLabelText('Step 1: Connect an agent, done')).toHaveTextContent('Connected 1 hour ago through the AMP Harness.')
    expect(within(list).getByLabelText('Step 3: Review and approve the agent\'s plan, to do')).toBeInTheDocument()
    expect(within(list).queryByLabelText('MCP address')).toBeNull()
    expect(within(list).getByRole('link', { name: /Open the project's My work/ })).toHaveAttribute('href', '/project/9/my-work')
    expect(within(list).getByRole('link', { name: /Turn receipts on in Project settings/ })).toHaveAttribute('href', '/project/9/settings')
  })

  it('opens the first receipt from the fourth step once there is one', async () => {
    payload = empty
    // The plan step is the one still open, so the list is on screen with the
    // receipt step checked and linking to the ticket that carries it.
    started = { ...fresh, agent: DONE, project: { ...DONE, project_id: 9 }, plan: { done: false, project_id: 9 },
      receipt: { done: true, at: hourAgo, project_id: 9, task_id: 421 } }
    renderMyWork()
    const list = await screen.findByRole('region', { name: 'Set up AMP in four steps' })
    expect(within(list).getByText('3 of 4 done')).toBeInTheDocument()
    expect(within(list).getByLabelText('Step 4: See your first receipt, done')).toHaveTextContent('You have a receipt, 1 hour ago')
    expect(within(list).getByRole('link', { name: /Open the receipt/ })).toHaveAttribute('href', '/project/9/board?focus=421')
  })

  it('is gone once every step is done', async () => {
    payload = empty
    renderMyWork()
    await screen.findByText('No project needs you right now.')
    expect(screen.queryByRole('region', { name: 'Set up AMP in four steps' })).toBeNull()
  })

  it('Hide keeps it hidden for this person', async () => {
    payload = empty
    started = fresh
    const { unmount } = renderMyWork()
    const list = await screen.findByRole('region', { name: 'Set up AMP in four steps' })
    await userEvent.click(within(list).getByRole('button', { name: 'Hide' }))
    expect(screen.queryByRole('region', { name: 'Set up AMP in four steps' })).toBeNull()
    unmount()
    renderMyWork()
    await screen.findByText('No project needs you right now.')
    expect(screen.queryByRole('region', { name: 'Set up AMP in four steps' })).toBeNull()
  })

  it('shows with sign-in off too, above the explanation', async () => {
    payload = EMPTY
    started = fresh
    renderMyWork({ subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: [] })
    const list = await screen.findByRole('region', { name: 'Set up AMP in four steps' })
    const off = screen.getByRole('region', { name: 'My work is off: sign-in is turned off' })
    expect(list.compareDocumentPosition(off) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('stays out of a busy My work', async () => {
    started = fresh
    renderMyWork()
    await screen.findByRole('heading', { name: 'AMP v2' })
    expect(screen.queryByRole('region', { name: 'Set up AMP in four steps' })).toBeNull()
  })
})

describe('find box (#155)', () => {
  const me: Me = { subject: 's', email: 'jo@example.com', name: 'Jo', roles: [], user_id: 7 }

  it('?q= narrows rows across projects by title, ticket ref or project, and keeps the counts', async () => {
    renderMyWork(me, '/?q=vendor')
    await screen.findByRole('heading', { name: 'Core API' })
    expect(screen.queryByRole('heading', { name: 'AMP v2' })).toBeNull()
    const core = screen.getByRole('article', { name: /Core API/ })
    expect(within(core).getAllByRole('listitem')).toHaveLength(1)
    expect(within(core).getByText('#1903 Call the vendor')).toBeInTheDocument()
    const ro = screen.getByLabelText('Waiting on you, across your projects')
    expect(ro.textContent).toMatch(/Waiting on you8/)
    expect(ro.textContent).toMatch(/in 2 of 13 projects/)
  })

  it('typing filters and writes ?q=; a ticket number matches; a project name keeps all its rows', async () => {
    const user = userEvent.setup()
    renderMyWork()
    await screen.findByRole('heading', { name: 'AMP v2' })
    const box = screen.getByRole('searchbox', { name: 'Find in My work' })
    await user.type(box, '2473')
    expect(screen.getByText('#2473 Review the mockup')).toBeInTheDocument()
    expect(screen.queryByText('#2470 My Work questions')).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Core API' })).toBeNull()
    await user.clear(box)
    await user.type(box, 'core api')
    expect(within(screen.getByRole('article', { name: /Core API/ })).getAllByRole('listitem')).toHaveLength(2)
    expect(screen.queryByRole('heading', { name: 'AMP v2' })).toBeNull()
  })

  it('says so when nothing matches; Clear removes ?q= and brings the rows back', async () => {
    const user = userEvent.setup()
    renderMyWork(me, '/?q=zzzz')
    expect(await screen.findByText('No matches')).toBeInTheDocument()
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByLabelText('Waiting on you, across your projects').textContent).toMatch(/in 2 of 13 projects/)
    await user.click(screen.getByRole('button', { name: 'Clear' }))
    expect(screen.queryByText('No matches')).toBeNull()
    expect(screen.getByRole('searchbox', { name: 'Find in My work' })).toHaveValue('')
    expect(screen.getByRole('searchbox', { name: 'Find in My work' })).toHaveFocus()
    expect(screen.getByRole('heading', { name: 'AMP v2' })).toBeInTheDocument()
  })

  it('Escape clears the box and brings the rows back', async () => {
    const user = userEvent.setup()
    renderMyWork(me, '/?q=vendor')
    const box = await screen.findByRole('searchbox', { name: 'Find in My work' })
    expect(box).toHaveValue('vendor')
    await user.click(box)
    await user.keyboard('{Escape}')
    expect(box).toHaveValue('')
    expect(screen.getByRole('heading', { name: 'AMP v2' })).toBeInTheDocument()
  })

  it('a match only in projects with nothing waiting never shows the all-clear state', async () => {
    renderMyWork(me, '/?q=home')
    await screen.findByText(/No waiting items match/)
    expect(screen.queryByText(/Nothing waits on you/)).toBeNull()
    expect(screen.getByText('No matches')).toBeInTheDocument()
    expect(screen.getByText(/No waiting items match/)).toBeInTheDocument()
    expect(screen.getByLabelText('Waiting on you, across your projects').textContent).toMatch(/Waiting on you8/)
  })

  it('says some rows were not searched, and keeps each card\'s "more" link', async () => {
    renderMyWork(me, '/?q=mockup')
    await screen.findByText('#2473 Review the mockup')
    expect(screen.getByText(/Only the rows listed here are searched\. 2 more items in the cards are not searched/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '+2 more in AMP v2 ›' })).toBeInTheDocument()
  })

  it('"/" focuses the box, but not while typing in it', async () => {
    const user = userEvent.setup()
    renderMyWork()
    const box = await screen.findByRole('searchbox', { name: 'Find in My work' })
    await user.keyboard('/')
    expect(box).toHaveFocus()
    expect(box).toHaveValue('')
  })
})
