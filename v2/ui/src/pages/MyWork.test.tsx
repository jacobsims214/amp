// Top-level My work is a summary (KB decisions/human-work-ux.md §3): no action
// buttons, every row a link into /project/:id/my-work?focus=<ticket id>.
import { render, screen, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { MyWork } from './MyWork'
import { AuthContext } from '../contexts/auth-context'
import { RemoteProvider } from '../remote/RemoteProvider'
import { httpTransport } from '../remote/transport'
import { unpairedCrypto } from '../remote/crypto'
import type { MyWork as MyWorkData, WaitCounts, WaitItem, WorkTask } from '../types'

const hourAgo = new Date(Date.now() - 3_600_000).toISOString()
const counts = (c: Partial<WaitCounts>): WaitCounts => {
  const base = { approve: 0, answer: 0, merge: 0, do: 0, coming_up: 0, ...c }
  return { ...base, waiting: base.approve + base.answer + base.merge + base.do }
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
const server = setupServer(
  http.get('*/api/my-work', () => HttpResponse.json(payload)),
  http.get('*/api/velocity', () => HttpResponse.json({ days: [] })),
  http.get('*/api/day-reports/*', () => HttpResponse.json({ error: 'none' }, { status: 404 })),
  http.get('*/api/artifacts/expiring', () => HttpResponse.json({ artifacts: [
    { id: 23, title: 'Human work UX mockup', kind: 'html', status: 'in_review', expires_at: new Date(Date.now() + 5 * 86_400_000).toISOString() },
  ] })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
afterEach(() => { payload = busy })
afterAll(() => server.close())

function renderMyWork() {
  return render(
    <AuthContext.Provider value={{ me: { subject: 's', email: 'jo@example.com', name: 'Jo', roles: [], user_id: 7 }, loading: false, error: null, isAdmin: false }}>
      <RemoteProvider deps={{ transport: httpTransport, crypto: unpairedCrypto, mock: false }}>
        <MemoryRouter><MyWork /></MemoryRouter>
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

  it('still renders against an older server without the summary', async () => {
    payload = { owned: [], flags: [], agents: [], done_24h: 0 }
    renderMyWork()
    expect(await screen.findByText('No project needs you right now.')).toBeInTheDocument()
  })
})
