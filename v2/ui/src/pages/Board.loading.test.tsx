// Tiered loading on the Board: the first load draws the page shell and card
// skeletons (never a full-page loader), fast loads show nothing, and a refetch
// (Refresh, or any mutation) keeps the board mounted where it was.
import { act, fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthContext } from '../contexts/auth-context'
import type { Epic, Story, Task } from '../types'

const epic: Epic = { id: 1, project_id: 3, name: 'E', description: '', state: 'active', priority: '1', created_at: '', updated_at: '' } as Epic
const story: Story = { id: 1, epic_id: 1, project_id: 3, name: 'S', description: '', acceptance_criteria: '', state: 'active', created_at: '', updated_at: '' } as Story
const task = (id: number): Task => ({
  id, project_id: 3, epic_id: 1, story_id: 1, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_by: 7, owner_user_id: 7,
  created_at: '', planned_at: '', updated_at: '',
}) as Task

/** A promise the test settles by hand, so it controls how long a load runs. */
function deferred<T>() {
  let resolve!: (v: T) => void, reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

let epicsCall: ReturnType<typeof deferred<Epic[]>>
const listTasks = vi.fn(() => Promise.resolve([task(11)]))
const createTask = vi.fn(() => Promise.resolve(task(12)))

vi.mock('../hooks/useSSE', () => ({ useSSE: () => 'connected' }))
vi.mock('../hooks/usePlanApproval', () => ({
  usePlanApproval: () => ({ project: null, approving: false, error: null, notice: null, approve: vi.fn() }),
}))
vi.mock('../api/client', () => ({
  api: new Proxy({}, {
    get: (_t, name) => (...args: unknown[]) => {
      void args
      if (name === 'listEpics') return epicsCall.promise
      if (name === 'listTasks') return listTasks()
      if (name === 'listStories') return Promise.resolve([story])
      if (name === 'createTask') return createTask()
      if (name === 'listProjectMembers') return Promise.resolve({ members: [], my_access: 'lead' })
      return Promise.reject(new Error(`not mocked: ${String(name)}`))
    },
  }),
}))

const { Board } = await import('./Board')

function renderBoard() {
  return render(
    <AuthContext.Provider value={{ me: { subject: 's', email: 'jo@example.com', name: 'Jo', roles: [], user_id: 7 }, loading: false, error: null, isAdmin: false }}>
      <MemoryRouter initialEntries={['/project/3/board']}>
        <Routes><Route path="/project/:projectId/board" element={<Board />} /></Routes>
      </MemoryRouter>
    </AuthContext.Provider>,
  )
}

const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms) })
const settle = (fn: () => void) => act(async () => { fn(); await vi.advanceTimersByTimeAsync(0) })
const refreshButton = () => screen.getByRole('button', { name: 'Refresh the board' })

/** Load the board and return its first ticket card and the scroll container. */
async function loaded() {
  renderBoard()
  await settle(() => epicsCall.resolve([epic]))
  const card = screen.getByRole('button', { name: /Ticket 11: Ticket 11/ })
  const scroller = card.closest('.overflow-auto') as HTMLElement
  expect(scroller).not.toBeNull()
  return { card, scroller }
}

beforeEach(() => {
  vi.useFakeTimers()
  epicsCall = deferred<Epic[]>()
  listTasks.mockClear(); createTask.mockClear()
})
afterEach(() => { vi.useRealTimers() })

describe('Board first load', () => {
  it('draws the nav and card skeletons, not a full-page loader, once the load runs past 250ms', async () => {
    const { container } = renderBoard()
    // The header is up at once: its controls are usable while the board loads.
    expect(refreshButton()).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Epic/ })).toBeInTheDocument()
    expect(screen.queryByRole('status', { name: 'Loading the board' })).toBeNull()
    expect(container.querySelector('.amp-band-loader')).toBeNull()
    const region = screen.getByRole('status', { name: 'Loading tickets' })
    expect(region).toHaveAttribute('aria-busy', 'true')

    await tick(249)
    expect(screen.queryByTestId('board-skeleton')).toBeNull()
    await tick(1)
    expect(screen.getByTestId('board-skeleton').querySelectorAll('.amp-skel-card')).toHaveLength(15)

    await settle(() => epicsCall.resolve([epic]))
    expect(screen.queryByRole('status', { name: 'Loading tickets' })).toBeNull()
    expect(screen.getByRole('button', { name: /Ticket 11/ })).toBeInTheDocument()
  })

  it('shows no skeleton when the load resolves under 250ms', async () => {
    renderBoard()
    await tick(120)
    await settle(() => epicsCall.resolve([epic]))
    await tick(500)
    expect(screen.queryByTestId('board-skeleton')).toBeNull()
    expect(screen.getByRole('button', { name: /Ticket 11/ })).toBeInTheDocument()
  })

  it('still shows the error page when the first load fails', async () => {
    renderBoard()
    await settle(() => epicsCall.reject(new Error('Board is down')))
    expect(screen.getByText('Board is down')).toBeInTheDocument()
  })
})

describe('Board refetch', () => {
  it('keeps the columns mounted and marks Refresh busy while the refetch is in flight', async () => {
    const { card, scroller } = await loaded()
    scroller.scrollTop = 120
    expect(refreshButton()).toHaveAttribute('aria-busy', 'false')

    epicsCall = deferred<Epic[]>()
    fireEvent.click(refreshButton())
    expect(refreshButton()).toHaveAttribute('aria-busy', 'true')
    expect(card.isConnected).toBe(true)
    expect(screen.getByRole('button', { name: /Ticket 11/ })).toBe(card)
    expect(screen.queryByTestId('board-skeleton')).toBeNull()

    // The band under the nav and the spinning icon wait out the delay too.
    await tick(249)
    expect(screen.queryByTestId('progress-band')).toBeNull()
    expect(refreshButton().querySelector('.animate-spin')).toBeNull()
    await tick(1)
    expect(screen.getByTestId('progress-band')).toBeInTheDocument()
    expect(refreshButton().querySelector('.animate-spin')).not.toBeNull()

    await settle(() => epicsCall.resolve([epic]))
    expect(refreshButton()).toHaveAttribute('aria-busy', 'false')
    expect(screen.getByRole('button', { name: /Ticket 11/ })).toBe(card)
    expect(scroller.scrollTop).toBe(120)
  })

  it('creating a ticket refreshes in place: same board, same scroll position', async () => {
    const { card, scroller } = await loaded()
    scroller.scrollTop = 240
    epicsCall = deferred<Epic[]>()
    listTasks.mockImplementation(() => Promise.resolve([task(11), task(12)]))

    fireEvent.click(screen.getByRole('button', { name: 'Add a task to S' }))
    fireEvent.change(screen.getByPlaceholderText('Task name'), { target: { value: 'Ticket 12' } })
    await settle(() => fireEvent.click(screen.getByRole('button', { name: 'Save' })))
    expect(createTask).toHaveBeenCalledTimes(1)
    expect(refreshButton()).toHaveAttribute('aria-busy', 'true')
    expect(card.isConnected).toBe(true)
    expect(scroller.isConnected).toBe(true)

    await settle(() => epicsCall.resolve([epic]))
    expect(screen.getByRole('button', { name: /Ticket 12: Ticket 12/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Ticket 11/ })).toBe(card)
    expect(scroller.scrollTop).toBe(240)
  })

  it('a failed refetch keeps the board and says so', async () => {
    const { card } = await loaded()
    epicsCall = deferred<Epic[]>()
    fireEvent.click(refreshButton())
    await settle(() => epicsCall.reject(new Error('Network down')))
    expect(screen.getByRole('alert')).toHaveTextContent("Couldn't refresh the board: Network down")
    expect(card.isConnected).toBe(true)
  })
})
