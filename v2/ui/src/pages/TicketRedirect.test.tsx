// Canonical links: /t/<ref> and /r/<run id> land on the ticket's Board
// drawer, keep their params, and say so when the ticket can't be seen (#131).
import { render, screen } from '@testing-library/react'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { RunRedirect, TicketRedirect } from './TicketRedirect'
import type { Task } from '../types'

const getTask = vi.fn()
const getRun = vi.fn()
const getProject = vi.fn()
vi.mock('../api/client', () => ({ api: { getTask: (id: number) => getTask(id), getRun: (id: number) => getRun(id), getProject: (id: number) => getProject(id) } }))

function Where() {
  const loc = useLocation()
  return <output data-testid="where">{loc.pathname + loc.search}</output>
}

function renderAt(path: string) {
  const router = createMemoryRouter([
    { path: '/t/:ref', element: <TicketRedirect /> },
    { path: '/r/:runId', element: <RunRedirect /> },
    { path: '*', element: <Where /> },
  ], { initialEntries: [path] })
  render(<RouterProvider router={router} />)
  return router
}

const task = (id: number, project_id: number) => ({ id, project_id } as Task)

describe('canonical ticket and run links', () => {
  beforeEach(() => { getTask.mockReset(); getRun.mockReset(); getProject.mockReset().mockResolvedValue({ id: 7, code: 'BA', name: 'Backup Air' }) })

  it.each(['/t/BA-34', '/t/ba-34', '/t/34'])('%s opens ticket 34 on its Board with the drawer open', async path => {
    getTask.mockResolvedValue(task(34, 7))
    const router = renderAt(path)
    expect(await screen.findByTestId('where')).toHaveTextContent('/project/7/board?focus=34')
    expect(getTask).toHaveBeenCalledWith(34)
    // Replaced, so Back doesn't land on the hop.
    expect(router.state.historyAction).toBe('REPLACE')
  })

  it('a code that is not the ticket\'s project\'s still opens it, flagged with ?ref= for the drawer notice', async () => {
    getTask.mockResolvedValue(task(34, 7))
    renderAt('/t/XY-34')
    expect(await screen.findByTestId('where')).toHaveTextContent('/project/7/board?focus=34&ref=XY-34')
  })

  it('keeps tab and the other params', async () => {
    getTask.mockResolvedValue(task(34, 7))
    renderAt('/t/BA-34?tab=details&run=12')
    expect(await screen.findByTestId('where')).toHaveTextContent('/project/7/board?focus=34&tab=details&run=12')
  })

  it('says a ticket that does not exist or is not visible was not found, instead of a blank board', async () => {
    getTask.mockRejectedValue(Object.assign(new Error('task 999999 not found'), { status: 404 }))
    renderAt('/t/999999')
    expect(await screen.findByRole('heading', { name: /Ticket 999999 was not found, or you can't see it/ })).toBeInTheDocument()
  })

  it('says a ref with no ticket id is not a ticket', async () => {
    renderAt('/t/nope')
    expect(await screen.findByRole('heading', { name: /was not found/ })).toBeInTheDocument()
    expect(getTask).not.toHaveBeenCalled()
  })

  it('/r/<id> opens its ticket at the run', async () => {
    getRun.mockResolvedValue({ run: { id: 12 }, task_id: 34, project_id: 7, ref: 'BA-34', url: '/t/BA-34' })
    renderAt('/r/12')
    expect(await screen.findByTestId('where')).toHaveTextContent('/project/7/board?focus=34&run=12')
  })

  it('a run that cannot be seen says so', async () => {
    getRun.mockRejectedValue(Object.assign(new Error('run 5 not found'), { status: 404 }))
    renderAt('/r/5')
    expect(await screen.findByRole('heading', { name: /Run 5 was not found/ })).toBeInTheDocument()
  })
})
