// The Board for someone new to it (#128): the empty state leads with the
// manual action and names the manager agent with a link, and an empty epic
// or story shows its add control in full.
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
const createStory = vi.fn((...args: unknown[]) => { void args; return Promise.resolve({}) })

// The board store subscribes to /api/events for live updates (#132), so the double
// needs subscribeSSE too; it returns an unsubscribe fn like the real one.
vi.mock('../hooks/useSSE', () => ({ useSSE: () => 'connected', subscribeSSE: () => () => {} }))
vi.mock('../hooks/usePlanApproval', () => ({
  usePlanApproval: () => ({ project: null, approving: false, error: null, notice: null, approve: vi.fn() }),
}))
vi.mock('../api/client', () => ({
  api: new Proxy({}, {
    get: (_t, name) => (...args: unknown[]) => {
      if (name === 'listEpics') return Promise.resolve(epics)
      if (name === 'listTasks') return Promise.resolve(tasks)
      // Must be nullish (not []) so fetchBoard falls back to per-epic listStories.
      if (name === 'listProjectStories') return Promise.resolve(null)
      if (name === 'listStories') return Promise.resolve(stories.filter(s => s.epic_id === args[0]))
      if (name === 'listProjectMembers') return Promise.resolve({ members: [], my_access: 'lead' })
      if (name === 'getProject') return Promise.resolve({ id: 3, metadata: {} })
      if (name === 'inheritedMetadata') return Promise.resolve({})
      if (name === 'createEpic') return createEpic(...args)
      if (name === 'createStory') return createStory(...args)
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

const epic = (id: number, name: string): Epic => ({ id, project_id: 3, name, description: '', state: 'active', priority: '1', owner_ids: [7], created_at: '', updated_at: '' }) as Epic
const story = (id: number, epicId: number, name: string): Story => ({ id, epic_id: epicId, project_id: 3, name, description: '', acceptance_criteria: '', state: 'active', owner_ids: [7], created_at: '', updated_at: '' }) as Story
const task = (id: number, storyId: number): Task => ({
  id, project_id: 3, epic_id: 1, story_id: storyId, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_by: 7, owner_user_id: 7, created_at: '', planned_at: '', updated_at: '',
}) as Task

beforeEach(() => { epics = []; stories = []; tasks = []; createEpic.mockClear(); createStory.mockClear() })

describe('Board empty state', () => {
  it('leads with Create your first epic and links the manager agent', async () => {
    renderBoard()
    const create = await screen.findByRole('button', { name: 'Create your first epic' })
    expect(screen.getByRole('link', { name: 'manager agent' })).toHaveAttribute('href', '/guide?doc=setup/connect-your-agent.md')
    await userEvent.click(create)
    expect(screen.getByRole('heading', { name: 'New Epic' })).toBeInTheDocument()
  })
})

describe('Board add controls', () => {
  it('an empty epic shows + Story and an empty story + Task; the rest keep a labelled icon', async () => {
    epics = [epic(1, 'Launch'), epic(2, 'Empty epic')]
    stories = [story(10, 1, 'Has tickets'), story(11, 1, 'No tickets')]
    tasks = [task(100, 10)]
    renderBoard()
    const addStoryEmpty = await screen.findByRole('button', { name: 'Add a story to Empty epic' })
    expect(addStoryEmpty).toHaveTextContent('Story')
    expect(addStoryEmpty).toHaveAttribute('title', 'Add a story to this epic')
    const addStoryFull = screen.getByRole('button', { name: 'Add a story to Launch' })
    expect(addStoryFull).toHaveTextContent('')
    expect(addStoryFull).toHaveAttribute('title', 'Add a story to this epic')

    expect(screen.getByRole('button', { name: 'Add a task to No tickets' })).toHaveTextContent('Task')
    expect(screen.getByRole('button', { name: 'Add a task to Has tickets' })).toHaveTextContent('')
    expect(screen.getByRole('button', { name: 'Story actions: Has tickets' })).toHaveAttribute('title', 'Edit or delete this story')
    expect(screen.getByRole('button', { name: 'Epic actions: Launch' })).toHaveAttribute('title', 'Edit or delete this epic')

    await userEvent.click(screen.getByRole('button', { name: 'Add a task to No tickets' }))
    expect(screen.getByRole('heading', { name: 'New Task' })).toBeInTheDocument()
  })
})

describe('Board dialogs', () => {
  it('New Epic: Enter in Name saves; an empty Name is flagged at the field', async () => {
    renderBoard()
    await userEvent.click(await screen.findByRole('button', { name: 'Create your first epic' }))
    expect(screen.getByRole('dialog', { name: 'New Epic' })).toBeInTheDocument()
    const name = screen.getByLabelText('Name *')
    await userEvent.type(name, '{Enter}')
    expect(createEpic).not.toHaveBeenCalled()
    expect(name).toHaveAccessibleDescription('Give it a name.')
    await userEvent.type(name, 'Launch{Enter}')
    expect(createEpic).toHaveBeenCalledWith(3, expect.objectContaining({ name: 'Launch' }))
  })

  it('New Epic: Escape with a name typed asks before discarding it', async () => {
    renderBoard()
    await userEvent.click(await screen.findByRole('button', { name: 'Create your first epic' }))
    await userEvent.type(screen.getByLabelText('Name *'), 'Half a thought')
    await userEvent.keyboard('{Escape}')
    expect(screen.getByRole('alertdialog', { name: 'Discard changes?' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Discard' }))
    expect(screen.queryByRole('dialog', { name: 'New Epic' })).toBeNull()
    expect(createEpic).not.toHaveBeenCalled()
  })

  it('New Story: Enter in Name saves it, and focus goes back to + Story', async () => {
    epics = [epic(2, 'Empty epic')]
    renderBoard()
    const add = await screen.findByRole('button', { name: 'Add a story to Empty epic' })
    await userEvent.click(add)
    expect(screen.getByRole('dialog', { name: 'New Story' })).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Name *'), 'Landing page{Enter}')
    expect(createStory).toHaveBeenCalledWith(2, expect.objectContaining({ name: 'Landing page' }))
    expect(await screen.findByRole('button', { name: 'Add a story to Empty epic' })).toHaveFocus()
  })
})
