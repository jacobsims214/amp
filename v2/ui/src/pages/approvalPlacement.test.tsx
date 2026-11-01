// Approving a plan happens only in the project's My work tab (decisions/human-work-ux.md §2).
// The Board and Flight plan show a one-line link there and offer no approve button.
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { AuthContext } from '../contexts/auth-context'
import type { Project, Task } from '../types'

const project: Project = {
  id: 3, name: 'P', code: 'P', description: '', state: 'active', created_at: '', updated_at: '',
  require_plan_approval: true, plan_approved_at: '2026-10-21T15:30:00Z', plan_approved_by: 7,
}
const task = (id: number, over: Partial<Task> = {}): Task => ({
  id, project_id: 3, epic_id: 1, story_id: 1, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_by: 7, owner_user_id: 7,
  created_at: '2026-10-22T11:00:00Z', planned_at: '2026-10-22T11:00:00Z', updated_at: '', ...over,
})
const tasks = [task(11), task(12), task(13, { owner_user_id: 9, created_by: 9 })]
const approve = vi.fn()

vi.mock('../hooks/useBoardData', () => ({
  useBoardData: () => ({
    epics: [{ id: 1, project_id: 3, name: 'E', description: '', state: 'active', priority: '1', created_at: '', updated_at: '' }],
    stories: [{ id: 1, epic_id: 1, project_id: 3, name: 'S', description: '', acceptance_criteria: '', state: 'active', created_at: '', updated_at: '' }],
    tasks, loading: false, error: null, refresh: vi.fn(), applyTask: vi.fn(), applyEpic: vi.fn(), applyStory: vi.fn(),
  }),
}))
vi.mock('../hooks/usePlanApproval', () => ({
  usePlanApproval: () => ({ project, approving: false, error: null, notice: null, approve }),
}))
vi.mock('../hooks/useSSE', () => ({ useSSE: () => 'connected' }))
vi.mock('../api/client', () => ({
  // Every call the pages make on load: members and the project resolve; anything else rejects (and is caught).
  api: new Proxy({}, {
    get: (_t, name) => (...args: unknown[]) => {
      void args
      if (name === 'listProjectMembers') return Promise.resolve({ members: [], my_access: 'lead' })
      if (name === 'getProject') return Promise.resolve(project)
      return Promise.reject(new Error(`not mocked: ${String(name)}`))
    },
  }),
}))

const { Board } = await import('./Board')
const { FlightPlan } = await import('./FlightPlan')

function renderAt(path: string) {
  return render(
    <AuthContext.Provider value={{ me: { subject: 's', email: 'jo@example.com', name: 'Jo', roles: [], user_id: 7 }, loading: false, error: null, isAdmin: false }}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/project/:projectId/board" element={<Board />} />
          <Route path="/project/:projectId/plan" element={<FlightPlan />} />
        </Routes>
      </MemoryRouter>
    </AuthContext.Provider>,
  )
}

describe.each([['Board', '/project/3/board'], ['Flight plan', '/project/3/plan']])('%s: plan approval is a link to My work', (_name, path) => {
  it('links "2 tickets await your approval" to My work, focused on the oldest, with no approve button', async () => {
    renderAt(path)
    const link = await screen.findByRole('link', { name: /2 tickets await your approval/ })
    expect(link).toHaveAttribute('href', '/project/3/my-work?focus=11')
    expect(link).toHaveTextContent('My work')
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()
    expect(screen.queryByRole('checkbox', { name: /approve/i })).toBeNull()
    expect(approve).not.toHaveBeenCalled()
  })
})
