// Project settings: the require_merge toggle and who sees the GitHub merge webhook (AMP #2485).
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Project, ProjectAccess } from '../types'

const project: Project = { id: 3, name: 'P', code: 'P', description: '', state: 'active', created_at: '', updated_at: '', require_merge: true }
const state = vi.hoisted(() => ({ access: 'owner' as string, update: vi.fn<(id: number, s: Partial<Project>) => Promise<Project>>() }))

vi.mock('../api/client', () => ({
  api: new Proxy({}, {
    get: (_t, name) => (...args: unknown[]) => {
      if (name === 'getProject') return Promise.resolve(project)
      if (name === 'listProjectMembers') return Promise.resolve({ members: [], my_access: state.access })
      if (name === 'kbSources') return Promise.resolve([])
      if (name === 'getGitHubHook') return Promise.resolve({ configured: false, url: '/api/hooks/github/3', deliveries: [], encrypted: true })
      if (name === 'updateProjectSettings') return state.update(args[0] as number, args[1] as Partial<Project>)
      return Promise.reject(new Error(`not mocked: ${String(name)}`))
    },
  }),
}))

const { ProjectSettings } = await import('./ProjectSettings')

function renderAs(access: ProjectAccess) {
  state.access = access
  return render(
    <MemoryRouter initialEntries={['/project/3/settings']}>
      <Routes><Route path="/project/:projectId/settings" element={<ProjectSettings />} /></Routes>
    </MemoryRouter>,
  )
}

beforeEach(() => { state.update.mockReset() })

describe('ProjectSettings require_merge', () => {
  it('turns require_merge off and back on for a project owner', async () => {
    state.update.mockImplementation((_id, s) => Promise.resolve({ ...project, ...s }))
    const user = userEvent.setup()
    renderAs('owner')
    const toggle = await screen.findByRole('checkbox', { name: /hold dependent tickets until the work they build on is merged/i })
    await vi.waitFor(() => expect(toggle).toBeEnabled())
    expect(toggle).toBeChecked()

    await user.click(toggle)
    expect(state.update).toHaveBeenLastCalledWith(3, { require_merge: false })
    expect(await screen.findByText(/dependents start as soon as a ticket is done/i)).toBeInTheDocument()
    expect(toggle).not.toBeChecked()

    await user.click(toggle)
    expect(state.update).toHaveBeenLastCalledWith(3, { require_merge: true })
    await vi.waitFor(() => expect(toggle).toBeChecked())
  })

  it('shows the error and keeps the setting when the change is refused', async () => {
    state.update.mockRejectedValue(new Error('project owner access required'))
    const user = userEvent.setup()
    renderAs('owner')
    const toggle = await screen.findByRole('checkbox', { name: /hold dependent tickets/i })
    await vi.waitFor(() => expect(toggle).toBeEnabled())
    await user.click(toggle)
    expect(await screen.findByText('project owner access required')).toBeInTheDocument()
    expect(toggle).toBeChecked()
  })

  it('is read-only for a lead, who also does not see the GitHub merge webhook', async () => {
    renderAs('lead')
    const toggle = await screen.findByRole('checkbox', { name: /hold dependent tickets/i })
    await screen.findByText(/only project owners and org admins can change these settings/i)
    expect(toggle).toBeDisabled()
    expect(screen.queryByRole('heading', { name: 'GitHub merge webhook' })).not.toBeInTheDocument()
  })

  it('shows the GitHub merge webhook section to a project owner', async () => {
    renderAs('owner')
    expect(await screen.findByRole('heading', { name: 'GitHub merge webhook' })).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: /generate secret/i })).toBeInTheDocument()
  })
})
