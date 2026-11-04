// Shareable URLs (v2/docs/url-state.md): Board filters, DAG selection and
// drawer, focus across view switches, and missing focus tickets.
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { beforeAll, describe, expect, it, vi } from 'vitest'
import { AuthContext } from '../contexts/auth-context'
import type { Project, Task } from '../types'

const project: Project = {
  id: 3, name: 'P', code: 'P', description: '', state: 'active', created_at: '', updated_at: '',
}
const task = (id: number, over: Partial<Task> = {}): Task => ({
  id, project_id: 3, epic_id: 1, story_id: 1, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_by: 7, owner_user_id: 7,
  created_at: '2026-10-22T11:00:00Z', updated_at: '', ...over,
})
// 11 mine & ready, 12 mine & waiting, 13 someone else's & in progress, 14 mine & in progress
const tasks = [
  task(11), task(12, { state: 'blocked' }),
  task(13, { state: 'in_progress', owner_user_id: 9, created_by: 9 }),
  task(14, { state: 'in_progress', dependency_ids: [11] }),
]

vi.mock('../hooks/useBoardData', () => ({
  useBoardData: () => ({
    epics: [{ id: 1, project_id: 3, name: 'E', description: '', state: 'active', priority: '1', created_at: '', updated_at: '' }],
    stories: [{ id: 1, epic_id: 1, project_id: 3, name: 'S', description: '', acceptance_criteria: '', state: 'active', created_at: '', updated_at: '' }],
    tasks, loading: false, error: null, refresh: vi.fn(), applyTask: vi.fn(), applyEpic: vi.fn(), applyStory: vi.fn(),
  }),
}))
vi.mock('../hooks/usePlanApproval', () => ({
  usePlanApproval: () => ({ project, approving: false, error: null, notice: null, approve: vi.fn() }),
}))
vi.mock('../hooks/useSSE', () => ({ useSSE: () => 'connected' }))
// The drawer is its own PR's business; here it only has to say which ticket it shows.
vi.mock('../components/TaskDrawer', () => ({
  TaskDrawer: ({ task, onClose }: { task: Task | null; onClose: () => void }) => task
    ? <div role="dialog" aria-label={`Ticket ${task.id}`}><button type="button" onClick={onClose}>Close drawer</button></div>
    : null,
}))
vi.mock('../api/client', () => ({
  api: new Proxy({}, {
    get: (_t, name) => (...args: unknown[]) => {
      if (name === 'listProjectMembers') return Promise.resolve({ members: [], my_access: 'lead' })
      if (name === 'getProject') return Promise.resolve(project)
      if (name === 'getTask') {
        const id = args[0] as number
        if (id === 50) return Promise.resolve(task(50, { project_id: 5 }))
        return Promise.reject(new Error('not found'))
      }
      return Promise.reject(new Error(`not mocked: ${String(name)}`))
    },
  }),
}))

beforeAll(() => {
  // React Flow measures its pane.
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})

const { Board } = await import('./Board')
const { DAGView } = await import('./DAG')
const { FlightPlan } = await import('./FlightPlan')

function Where() {
  const loc = useLocation()
  return <span data-testid="url">{loc.pathname + loc.search}</span>
}

function renderAt(path: string) {
  const router = createMemoryRouter([
    { path: '/project/:projectId/board', element: <><Board /><Where /></> },
    { path: '/project/:projectId/dag', element: <><DAGView /><Where /></> },
    { path: '/project/:projectId/plan', element: <><FlightPlan /><Where /></> },
  ], { initialEntries: [path] })
  render(
    <AuthContext.Provider value={{ me: { subject: 's', email: 'jo@example.com', name: 'Jo', roles: [], user_id: 7 }, loading: false, error: null, isAdmin: false }}>
      <RouterProvider router={router} />
    </AuthContext.Provider>,
  )
  return router
}
const url = () => screen.getByTestId('url').textContent
const card = (id: number) => screen.queryByText(`Ticket ${id}`)?.closest('button') ?? null

describe('Board filters in the URL', () => {
  it('with no params behaves as before: my tickets, no state filter, a clean URL', async () => {
    renderAt('/project/3/board')
    await screen.findByText('Ticket 11')
    expect(card(13)).toBeNull()
    expect(screen.getByRole('button', { name: /^All/ })).toHaveAttribute('aria-pressed', 'true')
    expect(url()).toBe('/project/3/board')
  })

  it('clicking "Waiting & questions" pushes status=waiting, and a reload keeps it', async () => {
    const user = userEvent.setup()
    const router = renderAt('/project/3/board?focus=11')
    await user.click(await screen.findByRole('button', { name: /Waiting & questions/ }))
    expect(url()).toBe('/project/3/board?focus=11&status=waiting')
    expect(router.state.historyAction).toBe('PUSH')
    expect(card(11)).toHaveClass('opacity-35')
    expect(card(12)).not.toHaveClass('opacity-35')

    // Reload: a fresh render at the same URL.
    const shared = url()!
    cleanup()
    renderAt(shared)
    expect(await screen.findByRole('button', { name: /Waiting & questions/ })).toHaveAttribute('aria-pressed', 'true')
    expect(card(11)).toHaveClass('opacity-35')
    expect(card(12)).not.toHaveClass('opacity-35')
    // ?focus= works alongside the filter.
    expect(screen.getByRole('dialog', { name: 'Ticket 11' })).toBeInTheDocument()
  })

  it('typing "north" writes q=north without a history entry per keystroke', async () => {
    const user = userEvent.setup()
    const router = renderAt('/project/3/board')
    await screen.findByText('Ticket 11')
    const before = router.state.historyAction
    await user.type(screen.getByPlaceholderText(/Search tasks/), 'north')
    expect(screen.getByPlaceholderText(/Search tasks/)).toHaveValue('north')
    await waitFor(() => expect(url()).toBe('/project/3/board?q=north'))
    expect(before).toBe('POP')
    expect(router.state.historyAction).toBe('REPLACE')
  })

  it('opening ?owner=all&status=in_progress shows everyone\'s in-progress cards', async () => {
    renderAt('/project/3/board?owner=all&status=in_progress')
    await screen.findByText('Ticket 13')
    expect(card(13)).not.toHaveClass('opacity-35')
    expect(card(14)).not.toHaveClass('opacity-35')
    expect(card(11)).toHaveClass('opacity-35')
  })

  it('Back undoes a pill', async () => {
    const user = userEvent.setup()
    const router = renderAt('/project/3/board')
    await user.click(await screen.findByRole('button', { name: /^Done/ }))
    expect(url()).toBe('/project/3/board?status=done')
    await act(() => router.navigate(-1))
    expect(url()).toBe('/project/3/board')
    expect(screen.getByRole('button', { name: /^All/ })).toHaveAttribute('aria-pressed', 'true')
  })
})

describe('?focus= naming a ticket that is not here', () => {
  it('redirects to the same view in the ticket\'s own project', async () => {
    renderAt('/project/3/board?focus=50')
    await waitFor(() => expect(url()).toBe('/project/5/board?focus=50'))
  })

  it('says so when the ticket does not exist, and Dismiss clears focus', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/board?focus=999999')
    expect(await screen.findByRole('status')).toHaveTextContent('Ticket #999999 was not found')
    await user.click(screen.getByRole('button', { name: /Dismiss/ }))
    expect(url()).toBe('/project/3/board')
    expect(screen.queryByText(/was not found/)).toBeNull()
  })

  it('works on the Flight plan too', async () => {
    renderAt('/project/3/plan?focus=999999')
    expect(await screen.findByText(/Ticket.*was not found/)).toBeInTheDocument()
  })
})

describe('switching views keeps ?focus=', () => {
  it('the project view tabs carry focus to views that honor it, and nothing else', async () => {
    renderAt('/project/3/board?focus=11&status=waiting')
    const tabs = await screen.findByRole('navigation', { name: 'Views' })
    const link = (name: string) => [...tabs.querySelectorAll('a')].find(a => a.textContent?.startsWith(name))!
    expect(link('Flight plan')).toHaveAttribute('href', '/project/3/plan?focus=11')
    expect(link('Graph')).toHaveAttribute('href', '/project/3/dag?focus=11')
    expect(link('Mission')).toHaveAttribute('href', '/project/3/mission?focus=11')
    expect(link('Settings')).toHaveAttribute('href', '/project/3/settings')
  })
})

describe('DAG in the URL', () => {
  it('?focus=14 opens the drawer for ticket 14, and closing it removes focus', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/dag?focus=14&epic=1')
    await user.click(await screen.findByRole('button', { name: 'Close drawer' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(url()).toBe('/project/3/dag?epic=1')
  })

  it('?sel=14&chain=1 selects 14 and shows only its chain', async () => {
    renderAt('/project/3/dag?sel=14&chain=1')
    const bar = await screen.findByTitle('Show only this chain')
    expect(bar.closest('div')).toHaveTextContent('#14')
    expect(bar.closest('div')).toHaveTextContent('waits on 1')
    // 11 is upstream of 14; 12 and 13 are not on its chain, so they are not drawn.
    expect(await screen.findByText('Ticket 11')).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByText('Ticket 12')).toBeNull())
    expect(screen.queryByText('Ticket 13')).toBeNull()
  })

  it('selecting a node pushes sel, and Back clears it', async () => {
    const router = renderAt('/project/3/dag')
    // A plain click: d3-zoom's mousedown handler needs a real window.
    fireEvent.click(await screen.findByText('Ticket 12'))
    expect(url()).toBe('/project/3/dag?sel=12')
    expect(router.state.historyAction).toBe('PUSH')
    await act(() => router.navigate(-1))
    expect(url()).toBe('/project/3/dag')
    expect(screen.queryByTitle('Show only this chain')).toBeNull()
  })

  it('clearing the selection removes sel and chain', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/dag?sel=14&chain=1&done=hide')
    await user.click(await screen.findByTitle('Clear'))
    expect(url()).toBe('/project/3/dag?done=hide')
  })
})
