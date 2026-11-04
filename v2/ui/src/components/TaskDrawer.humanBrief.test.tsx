// The drawer leads with the brief: a human ticket says what to do, what done
// means and what it unblocks, the tab is in the URL, and nothing waits on the
// history read except the History tab itself.
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { useState } from 'react'
import { createMemoryRouter, MemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { TaskDrawer } from './TaskDrawer'
import { useFocusParam } from '../hooks/useFocusParam'
import type { ActivityLog, ProjectAccess, ProjectMember, Task } from '../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 34, project_id: 2, epic_id: 1, story_id: 3, name: 'Confirm: north-star targets', description: '', acceptance_criteria: '',
  state: 'in_progress', priority: '1', dependency_ids: [], created_at: '2026-10-27T10:00:00Z', updated_at: '2026-10-27T10:00:00Z',
  planned_at: '2026-10-27T10:00:00Z', approved_at: '2026-10-27T10:30:00Z', approved_by: 9, owner_user_id: 9, created_by: 4,
  dispatched_at: '2026-10-29T08:00:00Z', ...over,
})
const human = (over: Partial<Task> = {}) => task({ performer: 'human', human_reason: 'judgment', agent_id: 'human', ...over })
const members: ProjectMember[] = [
  { user_id: 9, email: 'jo@example.com', display_name: 'Jo Owner', access: 'member' as ProjectAccess },
  { user_id: 4, email: 'pat@example.com', display_name: 'Pat Planner', access: 'lead' as ProjectAccess },
]
const access = 'member' as unknown as ProjectAccess

let projectTasks: Task[] = []
let listTasksCalls = 0
let historyRows: ActivityLog[] = []
let historyHangs = false
// "Hanging" reads wait here until the test ends, so no request outlives the server.
let held: (() => void)[] = []
const hold = async (body: Record<string, unknown>) => { await new Promise<void>(r => held.push(r)); return HttpResponse.json(body) }
const server = setupServer(
  http.get('*/api/projects/2/tasks', () => { listTasksCalls++; return HttpResponse.json({ tasks: projectTasks }) }),
  http.get('*/api/projects/2/epics', () => HttpResponse.json({ epics: [{ id: 1, project_id: 2, name: 'Scorecard launch' }] })),
  http.get('*/api/epics/1/stories', () => HttpResponse.json({ stories: [{ id: 3, epic_id: 1, name: 'Targets agreed' }] })),
  http.get('*/api/tasks/:id/history', () => historyHangs ? hold({ history: historyRows, count: historyRows.length }) : HttpResponse.json({ history: historyRows, count: historyRows.length })),
  http.get('*/api/tasks/:id/comments', () => historyHangs ? hold({ comments: [] }) : HttpResponse.json({ comments: [] })),
  http.get('*/api/tasks/:id/runs', () => HttpResponse.json({ runs: [], usage: { runs: 0 } })),
  http.get('*/api/projects/2/artifacts', () => HttpResponse.json({ artifacts: [] })),
  http.get('*/api/*', () => HttpResponse.json({})),
)
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
let inFlight = 0
server.events.on('request:start', () => { inFlight++ })
server.events.on('request:end', () => { inFlight-- })
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
afterEach(async () => {
  held.forEach(r => r()); held = []
  await waitFor(() => expect(inFlight).toBe(0))
  server.resetHandlers()
  projectTasks = []; listTasksCalls = 0; historyRows = []; historyHangs = false
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

function open(t: Task, url = '/project/2/board?focus=34') {
  const router = createMemoryRouter([{ path: '*', element: <TaskDrawer task={t} onClose={() => {}} members={members} meId={9} myAccess={access} onTaskUpdated={() => {}} /> }], { initialEntries: [url] })
  render(<RouterProvider router={router} />)
  return router
}
const panel = () => screen.getByTestId('human-work')
const tabButton = (name: 'history' | 'details') => screen.getByRole('button', { name })

describe('human ticket brief', () => {
  it('shows the description in the human panel, above Mark done…, on the Details tab', () => {
    open(human({ description: 'Pick **one** of the targets in the doc.' }))
    const p = panel()
    expect(within(p).getByText('What you need to do')).toBeInTheDocument()
    const desc = within(p).getByText('one')
    expect(desc.tagName).toBe('STRONG')
    const done = within(p).getByRole('button', { name: /mark done…/i })
    expect(desc.compareDocumentPosition(done) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(tabButton('details')).toHaveAttribute('aria-pressed', 'true')
    // Details doesn't repeat the brief the panel already leads with.
    expect(screen.queryByText('Description')).toBeNull()
  })

  it('shows acceptance criteria as checklist rows', () => {
    open(human({ description: 'd', acceptance_criteria: '- Targets confirmed\n- [x] Doc linked' }))
    const rows = within(panel()).getAllByTestId('checklist-item')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('Targets confirmed')
    expect(rows[1]).toHaveAttribute('data-done', 'true')
    expect(within(panel()).getByText('Done when')).toBeInTheDocument()
  })

  it('says the planner left no instructions, naming the creator, with Edit for a planner', async () => {
    open(human())
    const empty = within(panel()).getByTestId('human-brief-empty')
    expect(empty).toHaveTextContent('The planner left no instructions. Ask Pat Planner on the ticket, or edit it to add them.')
    await userEvent.click(within(empty).getByRole('button', { name: 'Edit' }))
    expect(screen.getByRole('button', { name: /save changes/i })).toBeInTheDocument()
  })

  it('heads the panel with what to do, keeps the rules in a tooltip, and says Started once', () => {
    open(human({ owner_user_id: 7 }))
    const heading = within(panel()).getByRole('heading', { level: 3 })
    expect(heading.textContent).toMatch(/^Decide/)
    expect(screen.queryByText(/Agents never pick it up/)).toBeNull()
    expect(heading).toHaveAttribute('title', expect.stringContaining('Agents never pick it up'))
    expect(screen.getAllByText(/^Started /)).toHaveLength(1)
    // Someone else's ticket: one owner-only line.
    expect(screen.getAllByText(/starts and finishes it/)).toHaveLength(1)
  })

  it('lists the open tickets that wait on it, as links to them', async () => {
    projectTasks = [
      task({ id: 35, name: 'Build scorecard', dependency_ids: [34], agent_id: 'amp-worker', state: 'blocked' }),
      task({ id: 36, name: 'Publish targets', dependency_ids: [34, 12], state: 'blocked', performer: 'human', owner_user_id: 4 }),
      task({ id: 37, name: 'Already done', dependency_ids: [34], state: 'completed' }),
      task({ id: 38, name: 'Unrelated', dependency_ids: [12] }),
    ]
    open(human())
    const row = await screen.findByTestId('human-unblocks')
    expect(row).toHaveTextContent('Unblocks 2')
    expect(within(row).getByRole('link', { name: '#35' })).toHaveAttribute('href', '/project/2/board?focus=35')
    expect(within(row).getByRole('link', { name: '#36' })).toHaveAttribute('href', '/project/2/board?focus=36')
    expect(row).toHaveTextContent('Build scorecard · amp-worker')
    expect(row).toHaveTextContent('Publish targets · Pat Planner')
    expect(row).not.toHaveTextContent('Already done')
    expect(row).not.toHaveTextContent('Unrelated')
  })

  it('says when nothing waits on it, and where it sits', async () => {
    open(human())
    expect(await screen.findByTestId('human-unblocks')).toHaveTextContent('Nothing waits on this')
    expect(screen.getByTestId('human-context')).toHaveTextContent('Scorecard launch › Targets agreed · Judgment · planned by Pat Planner')
  })

  it('reads the project tickets once per drawer, whichever tab is open', async () => {
    open(human())
    await screen.findByTestId('human-unblocks')
    await userEvent.click(tabButton('history'))
    await userEvent.click(tabButton('details'))
    await waitFor(() => expect(inFlight).toBe(0))
    expect(listTasksCalls).toBe(1)
  })

  it('rereads the project tickets when the drawer moves to another ticket', async () => {
    // Following a "#N" link swaps the ticket without closing the drawer.
    function Switch() {
      const [id, setId] = useState(34)
      return <><button type="button" onClick={() => setId(40)}>switch</button>
        <TaskDrawer task={human({ id })} onClose={() => {}} members={members} meId={9} myAccess={access} /></>
    }
    render(<MemoryRouter initialEntries={['/project/2/board?focus=34']}><Switch /></MemoryRouter>)
    expect(await screen.findByTestId('human-unblocks')).toHaveTextContent('Nothing waits on this')
    // A ticket that depends on #40 was planned meanwhile.
    projectTasks = [task({ id: 41, name: 'Build scorecard', state: 'blocked', dependency_ids: [40], agent_id: 'amp-worker' })]
    await userEvent.click(screen.getByRole('button', { name: 'switch' }))
    expect(await screen.findByRole('link', { name: '#41' })).toBeInTheDocument()
    expect(listTasksCalls).toBe(2)
  })
})

describe('drawer tab', () => {
  it('keeps an agent ticket in progress on History by default', async () => {
    open(task({ agent_id: 'amp-worker' }))
    expect(tabButton('history')).toHaveAttribute('aria-pressed', 'true')
    await waitFor(() => expect(inFlight).toBe(0))
  })

  it('opens the tab the URL names, even for a ticket in progress', async () => {
    open(task({ agent_id: 'amp-worker', description: 'The agent brief' }), '/project/1/board?focus=34&tab=details')
    expect(tabButton('details')).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByText('The agent brief')).toBeInTheDocument()
    await waitFor(() => expect(inFlight).toBe(0))
  })

  it('writes the tab to the URL with replace, and a reload keeps it', async () => {
    const router = open(human())
    await userEvent.click(tabButton('history'))
    expect(router.state.location.search).toBe('?focus=34&tab=history')
    expect(router.state.historyAction).toBe('REPLACE')
    await waitFor(() => expect(inFlight).toBe(0))
    const url = router.state.location.pathname + router.state.location.search
    act(() => router.dispose())
    cleanup()
    open(human(), url)
    expect(tabButton('history')).toHaveAttribute('aria-pressed', 'true')
  })

  it('copies an absolute link to the ticket and its tab', async () => {
    const writeText = vi.fn(() => Promise.resolve())
    const user = userEvent.setup()
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    open(human())
    await user.click(screen.getByRole('button', { name: 'Copy link' }))
    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/project/2/board?focus=34&tab=details`)
    expect(await screen.findByRole('status')).toHaveTextContent('Copied')
  })

  it('links each ticket it waits on', () => {
    open(task({ state: 'blocked', blocked_by_ids: [12, 13], dependency_ids: [12, 13] }))
    expect(screen.getByRole('link', { name: '#12' })).toHaveAttribute('href', '/project/2/board?focus=12')
    expect(screen.getByRole('link', { name: '#13' })).toHaveAttribute('href', '/project/2/board?focus=13')
  })

  function FocusHost({ t }: { t: Task }) {
    const [focus, setFocus] = useFocusParam()
    const loc = useLocation()
    return <><output data-testid="search">{loc.search}</output>
      <TaskDrawer task={focus === t.id ? t : null} onClose={() => setFocus(null)} members={members} meId={9} myAccess={access} /></>
  }
  function LocalHost({ t }: { t: Task }) {
    const [open, setOpen] = useState(true)
    const loc = useLocation()
    return <><output data-testid="search">{loc.search}</output>
      <TaskDrawer task={open ? t : null} onClose={() => setOpen(false)} members={members} meId={9} myAccess={access} /></>
  }

  it('drops focus and tab from the URL on close', async () => {
    render(<MemoryRouter initialEntries={['/project/2/board?focus=34&tab=history']}><FocusHost t={human()} /></MemoryRouter>)
    await userEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(screen.getByTestId('search')).toHaveTextContent(/^$/)
  })

  it('drops the tab on close where the view keeps the open ticket itself', async () => {
    render(<MemoryRouter initialEntries={['/project/2/dag?tab=history']}><LocalHost t={human()} /></MemoryRouter>)
    await userEvent.click(screen.getByRole('button', { name: 'Close' }))
    await waitFor(() => expect(screen.getByTestId('search')).toHaveTextContent(/^$/))
  })
})

describe('loading', () => {
  it('shows the brief and actions before history answers, and a skeleton only on History', async () => {
    historyHangs = true
    open(human({ description: 'Pick the targets.' }))
    expect(screen.getByText('Pick the targets.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /mark done…/i })).toBeInTheDocument()
    expect(document.querySelector('.animate-spin')).toBeNull()
    await userEvent.click(tabButton('history'))
    expect(screen.getByTestId('history-skeleton')).toBeInTheDocument()
  })

  it('keeps the history on screen, with no spinner, when the ticket changes state', async () => {
    historyRows = [{ id: 1, task_id: 34, project_id: 2, actor: 'pat@example.com', action: 'created', created_at: '2026-10-27T10:00:00Z' }]
    function StateHost() {
      const [t, setT] = useState(task({ agent_id: 'amp-worker' }))
      return <><button type="button" onClick={() => setT(x => ({ ...x, state: 'blocked' }))}>change state</button>
        <TaskDrawer task={t} onClose={() => {}} members={members} meId={9} myAccess={access} /></>
    }
    render(<MemoryRouter initialEntries={['/project/2/board?focus=34']}><StateHost /></MemoryRouter>)
    expect(await screen.findByText('created')).toBeInTheDocument()
    historyHangs = true
    await userEvent.click(screen.getByRole('button', { name: 'change state' }))
    expect(screen.getByText('created')).toBeInTheDocument()
    expect(screen.queryByTestId('history-skeleton')).toBeNull()
    expect(document.querySelector('.animate-spin')).toBeNull()
  })
})

describe('ticket text as markdown', () => {
  it("renders an agent's question as markdown inside the quote", () => {
    open(task({ state: 'blocked', block_reason: '1. **A**\n2. B', agent_id: 'amp-worker' }))
    const quote = document.querySelector('blockquote')!
    const items = within(quote).getAllByRole('listitem')
    expect(quote.querySelector('ol')).not.toBeNull()
    expect(items).toHaveLength(2)
    expect(quote.querySelector('strong')).toHaveTextContent('A')
  })

  it('renders comments as markdown, keeping their line breaks', async () => {
    historyRows = [
      { id: 1, task_id: 34, project_id: 2, actor: 'pat@example.com', action: 'comment', detail: 'see https://x.example', created_at: '2026-10-27T10:00:00Z' },
      { id: 2, task_id: 34, project_id: 2, actor: 'pat@example.com', action: 'comment', detail: 'line1\nline2', created_at: '2026-10-27T11:00:00Z' },
    ]
    open(task({ agent_id: 'amp-worker' }))
    const comments = await screen.findAllByTestId('history-comment')
    expect(within(comments[0]).getByRole('link', { name: 'https://x.example' })).toHaveAttribute('href', 'https://x.example')
    expect(comments[1].querySelector('br')).not.toBeNull()
    expect(comments[1]).toHaveTextContent(/line1\s*line2/)
  })

  it("renders a run's note as markdown", async () => {
    server.use(http.get('*/api/tasks/:id/runs', () => HttpResponse.json({ runs: [
      { id: 1, task_id: 34, project_id: 2, outcome: 'completed', note: 'Ran `npm test`', created_at: '2026-10-27T10:00:00Z' },
    ], usage: { runs: 1 } })))
    open(task({ state: 'backlog' }))
    const note = await screen.findByTestId('run-note')
    expect(note.querySelector('code')).toHaveTextContent('npm test')
  })
})
