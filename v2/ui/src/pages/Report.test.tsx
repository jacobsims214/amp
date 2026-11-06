import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { api } from '../api/client'
import { Report } from './Report'
import type { ProjectReport, Task } from '../types'

const task = { id: 42, project_id: 1, name: 'Sign the contract', state: 'ready' } as unknown as Task

vi.mock('../hooks/useBoardData', () => ({
  useBoardData: () => ({ tasks: [task], epics: [], stories: [], loading: false, refresh: () => {}, applyTask: () => {} }),
}))
vi.mock('../hooks/useProjectPeople', () => ({
  useProjectPeople: () => ({ members: [], myAccess: null, openReassign: false, people: new Map() }),
}))
vi.mock('../components/StandupSheet', () => ({ StandupSheet: () => null }))
vi.mock('../components/ProjectNav', () => ({ ProjectNav: ({ rightSlot }: { rightSlot?: React.ReactNode }) => <div>{rightSlot}</div> }))
vi.mock('../components/TaskDrawer', () => ({
  TaskDrawer: ({ task, onClose }: { task: Task | null; onClose: () => void }) =>
    task ? <div role="dialog">Drawer {task.name}<button type="button" onClick={onClose}>Close drawer</button></div> : null,
}))

const states = { ready: 0, in_progress: 0, waiting: 0, flagged: 0, scheduled: 0, done: 0 }
const perf = { tickets: 1, open: 1, median_wait_hours: 1, median_cycle_hours: 1, hours_worked: 0 }
const report = {
  completed: 0, dispatched: 0, created: 0, tickets: 1, states, agents: [], epics: [], metadata_keys: [], people: [],
  work: {
    agent: perf, human: perf, human_by_reason: [],
    slowest_human_tickets: [{ id: 42, name: 'Sign the contract', reason: 'approval', state: 'ready', wait_hours: 1, cycle_hours: 1, hours_worked: 0, holding_up: 2 }],
  },
} as unknown as ProjectReport

function Where() {
  const l = useLocation()
  return <output data-testid="where">{l.pathname}{l.search}</output>
}
function setup(url: string) {
  const router = createMemoryRouter(
    [{ path: '/project/:projectId/report', element: <><Report /><Where /></> }, { path: '*', element: <Where /> }],
    { initialEntries: ['/start', url] })
  render(<RouterProvider router={router} />)
  return router
}
const where = () => screen.getByTestId('where').textContent

beforeEach(() => {
  vi.spyOn(api, 'projectReport').mockResolvedValue(report)
  vi.spyOn(api, 'getProjectActivity').mockResolvedValue([])
  vi.spyOn(api, 'getTask').mockResolvedValue(task)
})

describe('Report URL state', () => {
  it('opens a ticket in a drawer over the report with ?focus=, and Back closes it', async () => {
    const user = userEvent.setup()
    const router = setup('/project/1/report')
    await user.click(await screen.findByText('Sign the contract'))
    expect(where()).toBe('/project/1/report?focus=42')
    expect(screen.getByRole('dialog')).toHaveTextContent('Sign the contract')
    await router.navigate(-1)
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(where()).toBe('/project/1/report')
  })

  it('Close removes focus', async () => {
    const user = userEvent.setup()
    setup('/project/1/report?focus=42')
    await user.click(await screen.findByRole('button', { name: 'Close drawer' }))
    expect(where()).toBe('/project/1/report')
  })

  it('selects the range from ?range= on load', async () => {
    setup('/project/1/report?range=30d')
    expect(await screen.findByRole('button', { name: '30 days' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('falls back to 7 days for inherited-property names in ?range=', async () => {
    setup('/project/1/report?range=constructor')
    expect(await screen.findByRole('button', { name: '7 days' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('writes a non-default range and leaves the default out of the URL', async () => {
    const user = userEvent.setup()
    setup('/project/1/report')
    expect(await screen.findByRole('button', { name: '7 days' })).toHaveAttribute('aria-pressed', 'true')
    expect(where()).toBe('/project/1/report')
    await user.click(screen.getByRole('button', { name: '30 days' }))
    expect(where()).toBe('/project/1/report?range=30d')
    await user.click(screen.getByRole('button', { name: '7 days' }))
    expect(where()).toBe('/project/1/report')
  })
})
