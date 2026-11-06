// Flight plan visibility (#70): done work hidden by default (whole stories and epics), newest update first, switch in the URL.
import { cleanup, render, screen, within } from '@testing-library/react'
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
    ? Promise.resolve({ members: [], my_access: 'lead' }) : Promise.reject(new Error('not mocked')) }),
}))

const { FlightPlan } = await import('./FlightPlan')
function Where() { return <span data-testid="url">{useLocation().search}</span> }
function renderAt(path: string) {
  render(<RouterProvider router={createMemoryRouter([{ path: '/project/:projectId/plan', element: <><FlightPlan /><Where /></> }], { initialEntries: [path] })} />)
}
afterEach(cleanup)
const order = () => [...document.querySelectorAll('tbody tr')].map(r => r.textContent ?? '')

describe('Flight plan visibility', () => {
  it('hides finished stories and epics by default and orders what is left by last update', async () => {
    renderAt('/project/3/plan')
    await screen.findByText('Live epic')
    expect(screen.queryByText('Done epic')).toBeNull()
    expect(screen.getByText(/2 finished epics hidden/)).toBeInTheDocument()
    expect(screen.queryByText('Finished story')).toBeNull()
    const text = order().join('|')
    // Last updated first: a newer queued story outranks the older one in flight.
    expect(text.indexOf('Question story')).toBeLessThan(text.indexOf('Newer queued story'))
    expect(text.indexOf('Newer queued story')).toBeLessThan(text.indexOf('Flight story'))
    expect(text.indexOf('Live epic')).toBeLessThan(text.indexOf('Queued epic'))
    // Nothing to do (no tickets) is hidden too.
    // Drifted ticket: its epic is still shown by its story, with the open work counted there.
    expect(screen.getByText('Queued epic')).toBeInTheDocument()
    expect(screen.getByText('Queued story')).toBeInTheDocument()
    expect(screen.queryByText('Empty story')).toBeNull()
    expect(screen.queryByText('Empty epic')).toBeNull()
    expect(screen.getByText('3 open of 4 tickets')).toBeInTheDocument()
    expect(screen.getByTestId('url').textContent).toBe('')
    expect(screen.getByRole('button', { name: /Jump to active/ })).toBeEnabled()
  })

  it('"Hide done" off shows everything, puts finished work last, and lives in ?done=show', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/plan')
    await user.click(await screen.findByRole('checkbox', { name: /Hide done/ }))
    expect(screen.getByTestId('url').textContent).toBe('?done=show')
    const text = order().join('|')
    expect(text.indexOf('Queued epic')).toBeLessThan(text.indexOf('Done epic'))
    expect(text.indexOf('Flight story')).toBeLessThan(text.indexOf('Finished story'))
  })

  it('?focus= on a finished ticket keeps its story, epic and chip visible', async () => {
    renderAt('/project/3/plan?focus=1')
    await screen.findByText('Live epic')
    expect(screen.getByText('Done epic')).toBeInTheDocument()
    expect(screen.getByText('Done story')).toBeInTheDocument()
    expect(screen.queryByText('Finished story')).toBeNull()
    expect(document.body.textContent).toContain('Ticket 1')
  })

  it('?done=show opens with done work visible; stage headers count open tickets', async () => {
    renderAt('/project/3/plan?done=show')
    await screen.findByText('Done epic')
    const head = within(document.querySelector('thead')!).getByText(/open/)
    expect(head).toHaveTextContent('4 open')
  })
})
