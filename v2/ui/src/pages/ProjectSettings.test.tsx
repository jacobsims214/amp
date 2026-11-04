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

// The dispatch watchdog (AMP #2801, story 444): the minutes after which a
// dispatched ticket no worker started is flagged, and returned to ready.
// 0 turns each off; a person saves them, an agent cannot.
describe('ProjectSettings dispatch watchdog', () => {
  it('shows both timeouts, with the defaults when the project sends none', async () => {
    renderAs('owner')
    expect(await screen.findByRole('heading', { name: 'Dispatch watchdog' })).toBeInTheDocument()
    const flag = await screen.findByLabelText(/flag it for its owner after/i)
    const ret = screen.getByLabelText(/return it to ready after/i)
    expect(flag).toHaveValue('10')
    expect(ret).toHaveValue('30')
    await vi.waitFor(() => expect(flag).toBeEnabled())
  })

  it('saves both minutes together, and only when they changed', async () => {
    state.update.mockImplementation((_id, s) => Promise.resolve({ ...project, ...s }))
    const user = userEvent.setup()
    renderAs('owner')
    const flag = await screen.findByLabelText(/flag it for its owner after/i)
    const save = screen.getByRole('button', { name: /save watchdog/i })
    await vi.waitFor(() => expect(flag).toBeEnabled())
    expect(save).toBeDisabled() // nothing changed yet

    await user.clear(flag)
    await user.type(flag, '5')
    await user.click(save)
    expect(state.update).toHaveBeenLastCalledWith(3, { not_started_flag_minutes: 5, not_started_return_minutes: 30 })
    await vi.waitFor(() => expect(screen.getByText('Saved')).toBeInTheDocument())
  })

  it('0 turns a timeout off and says so', async () => {
    state.update.mockImplementation((_id, s) => Promise.resolve({ ...project, ...s }))
    const user = userEvent.setup()
    renderAs('owner')
    const ret = await screen.findByLabelText(/return it to ready after/i)
    await vi.waitFor(() => expect(ret).toBeEnabled())
    await user.clear(ret)
    await user.type(ret, '0')
    expect(screen.getByText('Off: this never happens.')).toBeInTheDocument()
    expect(screen.getByText(/automatic return is off/i)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /save watchdog/i }))
    expect(state.update).toHaveBeenLastCalledWith(3, { not_started_flag_minutes: 10, not_started_return_minutes: 0 })
  })

  it('refuses text that is not a whole number of minutes, and never sends it', async () => {
    const user = userEvent.setup()
    renderAs('owner')
    const flag = await screen.findByLabelText(/flag it for its owner after/i)
    await vi.waitFor(() => expect(flag).toBeEnabled())
    await user.clear(flag)
    await user.type(flag, '1.5')
    expect(screen.getAllByRole('alert').map(e => e.textContent).join(' ')).toMatch(/whole number of minutes, 0 \(off\) to 1440/i)
    expect(screen.getByRole('button', { name: /save watchdog/i })).toBeDisabled()
    expect(state.update).not.toHaveBeenCalled()
  })

  it('warns when the return timeout is not above the flag, so nothing is ever flagged', async () => {
    const user = userEvent.setup()
    renderAs('owner')
    const flag = await screen.findByLabelText(/flag it for its owner after/i)
    await vi.waitFor(() => expect(flag).toBeEnabled())
    await user.clear(flag)
    await user.type(flag, '40')
    expect(screen.getByText(/never reaches the flag first/i)).toBeInTheDocument()
  })

  it('is read-only for a lead, who cannot change either timeout', async () => {
    renderAs('lead')
    const flag = await screen.findByLabelText(/flag it for its owner after/i)
    expect(flag).toBeDisabled()
    expect(screen.getByLabelText(/return it to ready after/i)).toBeDisabled()
    expect(screen.queryByRole('button', { name: /save watchdog/i })).toBeNull()
  })
})
