// The Board for someone new to it (#128): the empty state leads with the
// manual action and names the manager agent with a link.
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthContext } from '../contexts/auth-context'
import type { Epic, Story, Task } from '../types'

let epics: Epic[] = []
let stories: Story[] = []
let tasks: Task[] = []
const createEpic = vi.fn((...args: unknown[]) => { void args; return Promise.resolve({}) })

vi.mock('../hooks/useSSE', () => ({ useSSE: () => 'connected' }))
vi.mock('../hooks/usePlanApproval', () => ({
  usePlanApproval: () => ({ project: null, approving: false, error: null, notice: null, approve: vi.fn() }),
}))
vi.mock('../api/client', () => ({
  api: new Proxy({}, {
    get: (_t, name) => (...args: unknown[]) => {
      if (name === 'listEpics') return Promise.resolve(epics)
      if (name === 'listTasks') return Promise.resolve(tasks)
      if (name === 'listStories') return Promise.resolve(stories)
      if (name === 'listProjectMembers') return Promise.resolve({ members: [], my_access: 'lead' })
      if (name === 'getProject') return Promise.resolve({ id: 3, metadata: {} })
      if (name === 'inheritedMetadata') return Promise.resolve({})
      if (name === 'createEpic') return createEpic(...args)
      return Promise.resolve([])
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

beforeEach(() => { epics = []; stories = []; tasks = []; createEpic.mockClear() })

describe('Board empty state', () => {
  it('leads with Create your first epic and links the manager agent', async () => {
    renderBoard()
    const create = await screen.findByRole('button', { name: 'Create your first epic' })
    expect(screen.getByRole('link', { name: 'manager agent' })).toHaveAttribute('href', '/guide?doc=setup/connect-your-agent.md')
    await userEvent.click(create)
    expect(screen.getByRole('heading', { name: 'New Epic' })).toBeInTheDocument()
  })
})
