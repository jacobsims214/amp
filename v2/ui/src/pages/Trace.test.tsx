import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Task, TraceHit } from '../types'

const task = (id: number): Task => ({
  id, project_id: 3, epic_id: 1, story_id: 1, name: `Ticket ${id}`, description: `Plan for ${id}`, acceptance_criteria: '',
  state: 'completed', priority: '1', dependency_ids: [], created_by: 7, created_at: '', updated_at: '',
})
const hits: TraceHit[] = [
  { ref: 'P-21', task: task(21), project: 'P', runs: [] },
  { ref: 'P-22', task: task(22), project: 'P', runs: [] },
]
const trace = vi.fn()

vi.mock('../api/client', () => ({
  api: {
    trace: (q: unknown) => trace(q),
    getHistory: (id: number) => Promise.resolve([{ id: 1, task_id: id, actor: 'amp-worker', action: 'state_change', detail: `finished ${id}`, created_at: '2026-10-01T10:00:00Z' }]),
    getComments: () => Promise.resolve([]),
    taskRuns: () => Promise.resolve({ runs: [], usage: null }),
  },
}))

const { Trace } = await import('./Trace')

function Where() {
  const loc = useLocation()
  return <span data-testid="url">{loc.pathname + loc.search}</span>
}
function renderAt(path: string) {
  const router = createMemoryRouter([{ path: '/trace', element: <><Trace /><Where /></> }], { initialEntries: [path] })
  render(<RouterProvider router={router} />)
  return router
}
const url = () => screen.getByTestId('url').textContent

beforeEach(() => {
  trace.mockReset()
  trace.mockResolvedValue(hits)
})

describe('Trace in the URL', () => {
  it('runs ?q= on load with no interaction', async () => {
    renderAt('/trace?q=abc1234')
    expect(await screen.findByText('Ticket 21')).toBeInTheDocument()
    expect(screen.getByText('Ticket 22')).toBeInTheDocument()
    expect(trace).toHaveBeenCalledWith({ commit: 'abc1234' })
    expect(screen.getByLabelText(/Commit SHA/)).toHaveValue('abc1234')
  })

  it('shows nothing and calls nothing without ?q=', () => {
    renderAt('/trace')
    expect(trace).not.toHaveBeenCalled()
  })

  it('?open= renders that story already expanded with its timeline', async () => {
    renderAt('/trace?q=abc1234&open=22')
    expect(await screen.findByText('finished 22')).toBeInTheDocument()
    expect(screen.queryByText('finished 21')).toBeNull()
    const panel = document.getElementById('trace-22')!
    expect(panel.querySelector('[aria-expanded="true"]')).not.toBeNull()
  })

  it('toggling a story adds and removes it in ?open=', async () => {
    const user = userEvent.setup()
    renderAt('/trace?q=abc1234')
    await screen.findByText('Ticket 21')
    const show = () => screen.getAllByRole('button', { name: /how it got here/ })
    await user.click(show()[0])
    expect(url()).toBe('/trace?q=abc1234&open=21')
    await user.click(show()[1])
    expect(url()).toBe('/trace?q=abc1234&open=21%2C22')
    await user.click(show()[0])
    expect(url()).toBe('/trace?q=abc1234&open=22')
  })

  it('a new query keeps unrelated params, clears open, and pushes', async () => {
    const user = userEvent.setup()
    const router = renderAt('/trace?q=abc1234&open=21&x=1')
    await screen.findByText('Ticket 21')
    const input = screen.getByLabelText(/Commit SHA/)
    await user.clear(input)
    await user.type(input, 'feature/x{Enter}')
    expect(url()).toBe('/trace?q=feature%2Fx&x=1')
    expect(router.state.historyAction).toBe('PUSH')
    await waitFor(() => expect(trace).toHaveBeenLastCalledWith({ branch: 'feature/x' }))
    // Back returns to the earlier trace, re-run from the URL.
    await act(() => router.navigate(-1))
    expect(screen.getByLabelText(/Commit SHA/)).toHaveValue('abc1234')
    await waitFor(() => expect(trace).toHaveBeenLastCalledWith({ commit: 'abc1234' }))
  })

  it('each story has a Copy link that reproduces the expanded view', async () => {
    const user = userEvent.setup()
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    renderAt('/trace?q=abc1234')
    await screen.findByText('Ticket 21')
    await user.click(screen.getAllByRole('button', { name: /Copy link/ })[1])
    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/trace?q=abc1234&open=22`)
    expect(await screen.findByText('Copied')).toBeInTheDocument()
  })

  it('"Open ticket" and the ref go to the ticket\'s canonical link', async () => {
    renderAt('/trace?q=abc1234')
    await screen.findByText('Ticket 21')
    expect(screen.getAllByRole('link', { name: 'Open ticket' })[0]).toHaveAttribute('href', '/t/P-21')
    expect(screen.getByRole('link', { name: 'P-21' })).toHaveAttribute('href', '/t/P-21')
  })
})
