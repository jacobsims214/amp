// Flight plan check highlight (#156): a plan-check row lights the tickets it is about, in the URL, Escape clears.
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Task } from '../types'

const task = (id: number, epic: number, story: number, over: Partial<Task> = {}): Task => ({
  id, project_id: 3, epic_id: epic, story_id: story, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_by: 7, owner_user_id: 7, created_at: '', updated_at: '', ...over,
})
const epic = (id: number, name: string) => ({ id, project_id: 3, name, description: '', state: 'active', priority: '1', created_at: '', updated_at: '' })
const story = (id: number, epic_id: number, name: string) => ({ id, epic_id, project_id: 3, name, description: '', acceptance_criteria: '', state: 'active', created_at: '', updated_at: '' })
const at = (d: number) => `2026-01-${String(d).padStart(2, '0')}T00:00:00Z`
// Epic 1 (oldest) is finished; epic 2 is only queued; epic 3 has a story in flight and a question.
const tasks = [
  task(1, 1, 1, { state: 'completed', updated_at: at(1) }),
  task(2, 1, 2, { updated_at: at(2) }), // metadata drift: story 2 is in epic 2, its ticket says epic 1
  task(3, 3, 3, { state: 'completed', updated_at: at(9) }), task(4, 3, 4, { state: 'in_progress', updated_at: at(3) }),
  task(5, 3, 5, { state: 'blocked', block_reason: 'which db?', updated_at: at(10) }),
  task(6, 3, 6, { updated_at: at(8) }), // a queued story updated after the one in flight
]
vi.mock('../hooks/useBoardData', () => ({
  useBoardData: () => ({
    epics: [epic(1, 'Done epic'), epic(2, 'Queued epic'), epic(3, 'Live epic'), epic(4, 'Empty epic')],
    stories: [story(1, 1, 'Done story'), story(2, 2, 'Queued story'), story(3, 3, 'Finished story'), story(4, 3, 'Flight story'), story(5, 3, 'Question story'),
      story(6, 3, 'Newer queued story'), story(7, 3, 'Empty story')],
    tasks, loading: false, error: null, refresh: vi.fn(), applyTask: vi.fn(),
  }),
}))
vi.mock('../hooks/usePlanApproval', () => ({ usePlanApproval: () => ({ project: null, approving: false, error: null, notice: null, approve: vi.fn() }) }))
vi.mock('../hooks/useSSE', () => ({ useSSE: () => 'connected' }))
vi.mock('../components/TaskDrawer', () => ({ TaskDrawer: () => null }))
vi.mock('../api/client', () => ({
  api: new Proxy({}, { get: (_t, name) => () => name === 'listProjectMembers'
    ? Promise.resolve({ members: [], my_access: 'lead' })
    : name === 'planCheck' ? Promise.resolve({ ok: false, summary: 'NO-GO', ready_now: [], open_tasks: 4, dependencies: 0, issues: [
      { kind: 'isolated', message: 'Two tickets are not wired in.', task_ids: [4, 6], fix: 'Link them.' },
      { kind: 'empty_stories', message: 'Stories with no tickets.', story_ids: [7] },
      { kind: 'wide_start', message: 'Too wide.', task_ids: [999] },
      { kind: 'metadata_drift', message: 'Epics differ.', epic_ids: [4, 99] },
      { kind: 'no_context', message: 'KB is empty.', severity: 'warn' }] })
      : Promise.reject(new Error('not mocked')) }),
}))

const { FlightPlan } = await import('./FlightPlan')
function Where() { return <span data-testid="url">{useLocation().search}</span> }
function renderAt(path: string) {
  render(<RouterProvider router={createMemoryRouter([{ path: '/project/:projectId/plan', element: <><FlightPlan /><Where /></> }], { initialEntries: [path] })} />)
}
afterEach(cleanup)
const chipIds = () => [...document.querySelectorAll('tbody button[aria-pressed]')].map(b => b.textContent)

describe('Flight plan: a plan-check row highlights its tickets', () => {
  it('clicking a row narrows the grid to its tickets, lists them all, and puts the check in the URL; clicking again clears', async () => {
    Element.prototype.scrollIntoView = vi.fn()
    const user = userEvent.setup()
    renderAt('/project/3/plan')
    await user.click(await screen.findByRole('button', { name: 'Every ticket wired in' }))
    expect(screen.getByTestId('url').textContent).toBe('?check=isolated')
    // Only the stories holding tickets 4 and 6 stay; the flagged chips are marked beyond color.
    expect(screen.queryByText('Question story')).toBeNull()
    expect(screen.getByText('Flight story')).toBeInTheDocument()
    expect(screen.getByText('Newer queued story')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Ticket 4:.*flagged by the plan check/ })).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('showing only what this check flagged')
    expect(Element.prototype.scrollIntoView).toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Every ticket wired in' }))
    expect(screen.getByTestId('url').textContent).toBe('')
    expect(screen.getByText('Question story')).toBeInTheDocument()
  })

  it('?check= is linkable and Escape clears it', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/plan?check=isolated')
    await screen.findByText('Flight story')
    expect(chipIds()).toContain('4')
    expect(screen.queryByText('Question story')).toBeNull()
    await user.keyboard('{Escape}')
    expect(screen.getByTestId('url').textContent).toBe('')
    expect(screen.getByText('Question story')).toBeInTheDocument()
  })

  it('an empty-stories check lights the stories, even though hiding done would drop them', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/plan')
    await user.click(await screen.findByRole('button', { name: 'Every story has tickets' }))
    expect(screen.getByText('Empty story')).toBeInTheDocument()
    expect(screen.getByText(/Flagged by the plan check/)).toBeInTheDocument()
    expect(screen.queryByText('Flight story')).toBeNull()
  })

  it('an epic-level check marks the epic header, keeps empty epics, and never claims everything is done', async () => {
    Element.prototype.scrollIntoView = vi.fn()
    const user = userEvent.setup()
    renderAt('/project/3/plan')
    await user.click(await screen.findByRole('button', { name: 'Epics carry the same metadata' }))
    expect(screen.getByText('Empty epic')).toBeInTheDocument()
    expect(screen.getByText(/Flagged by the plan check/)).toBeInTheDocument()
    const name = screen.getByText('Empty epic')
    expect(name).toHaveClass('text-ink', 'underline')
    expect(name.className).not.toContain('$')
    expect(screen.queryByText('Live epic')).toBeNull()
    expect(screen.queryByText(/Everything here is done/)).toBeNull()
    expect(Element.prototype.scrollIntoView).toHaveBeenCalled()
  })

  it('a check whose ids are not in the loaded plan says so instead of claiming everything is done', async () => {
    renderAt('/project/3/plan?check=wide_start')
    expect(await screen.findByText(/Nothing this check flagged is in the loaded plan/)).toBeInTheDocument()
    expect(screen.queryByText(/Everything here is done/)).toBeNull()
  })

  it('a check with nothing to point at is not a button', async () => {
    renderAt('/project/3/plan')
    await screen.findByText('Context in the KB')
    expect(screen.queryByRole('button', { name: 'Context in the KB' })).toBeNull()
  })
})
