// Project settings: decision escalation (#146), off by default; an owner
// sets the delay, the priority raise and the backup decider, saved together.
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Project, ProjectAccess, ProjectMember } from '../types'

const project: Project = { id: 3, name: 'P', code: 'P', description: '', state: 'active', created_at: '', updated_at: '', require_merge: true }
const members: ProjectMember[] = [
  { user_id: 7, email: 'carol@x', display_name: 'Carol', access: 'member' },
  { user_id: 8, email: 'bot@x', display_name: 'Planner bot', access: 'member', kind: 'agent' },
]
const state = vi.hoisted(() => ({
  access: 'owner' as string,
  project: null as unknown as Project,
  update: vi.fn<(id: number, s: Partial<Project>) => Promise<Project>>(),
}))

vi.mock('../api/client', () => ({
  api: new Proxy({}, {
    get: (_t, name) => (...args: unknown[]) => {
      if (name === 'getProject') return Promise.resolve(state.project)
      if (name === 'listProjectMembers') return Promise.resolve({ members, my_access: state.access })
      if (name === 'kbSources') return Promise.resolve([])
      if (name === 'getGitHubHook') return Promise.resolve({ configured: false, url: '/api/hooks/github/3', deliveries: [], encrypted: true })
      if (name === 'getGitHubCredential') return Promise.resolve({ configured: false, host: 'github.com', encrypted: true })
      if (name === 'updateProjectSettings') return state.update(args[0] as number, args[1] as Partial<Project>)
      return Promise.reject(new Error(`not mocked: ${String(name)}`))
    },
  }),
}))

const { ProjectSettings } = await import('./ProjectSettings')

function renderAs(access: ProjectAccess, p: Project = project) {
  state.access = access
  state.project = p
  return render(
    <MemoryRouter initialEntries={['/project/3/settings']}>
      <Routes><Route path="/project/:projectId/settings" element={<ProjectSettings />} /></Routes>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  state.update.mockReset()
  state.update.mockImplementation((_id, s) => Promise.resolve({ ...state.project, ...s }))
})

describe('ProjectSettings decision escalation', () => {
  it('is off by default and says so', async () => {
    renderAs('owner')
    const minutes = await screen.findByLabelText(/escalate after the decide-by date by/i)
    expect(minutes).toHaveValue('0')
    expect(screen.getByTestId('escalation-summary')).toHaveTextContent(/Off: an overdue decision shows red on My work/)
    expect(screen.getByLabelText('Backup decider')).toBeDisabled()
    expect(screen.getByRole('button', { name: /save escalation/i })).toBeDisabled()
  })

  it('an owner turns it on with a priority raise and a backup decider, people only', async () => {
    const user = userEvent.setup()
    renderAs('owner')
    const minutes = await screen.findByLabelText(/escalate after the decide-by date by/i)
    await vi.waitFor(() => expect(minutes).toBeEnabled())
    await user.clear(minutes)
    await user.type(minutes, '60')
    await user.click(screen.getByRole('checkbox', { name: /raise its priority one step/i }))
    const backup = screen.getByLabelText('Backup decider')
    expect(screen.queryByRole('option', { name: 'Planner bot' })).toBeNull() // agents never decide
    await user.selectOptions(backup, 'Carol')
    expect(screen.getByTestId('escalation-summary')).toHaveTextContent('60 min after its decide-by date, a ticket still open is escalated once: its person is reminded on the ticket, its priority goes up a step, and it is handed to Carol.')
    await user.click(screen.getByRole('button', { name: /save escalation/i }))
    expect(state.update).toHaveBeenLastCalledWith(3, {
      decision_escalation_minutes: 60, decision_escalation_raise_priority: true, decision_backup_decider: 7,
    })
  })

  it('refuses a bad number and never sends it', async () => {
    const user = userEvent.setup()
    renderAs('owner')
    const minutes = await screen.findByLabelText(/escalate after the decide-by date by/i)
    await vi.waitFor(() => expect(minutes).toBeEnabled())
    await user.clear(minutes)
    await user.type(minutes, 'soon')
    expect(screen.getByText(/0 \(off\) to 43200/)).toHaveAttribute('role', 'alert')
    expect(screen.getByRole('button', { name: /save escalation/i })).toBeDisabled()
    expect(state.update).not.toHaveBeenCalled()
  })

  it('is read-only for a lead', async () => {
    renderAs('lead', { ...project, decision_escalation_minutes: 30 })
    const minutes = await screen.findByLabelText(/escalate after the decide-by date by/i)
    await vi.waitFor(() => expect(minutes).toHaveValue('30'))
    expect(minutes).toBeDisabled()
    expect(screen.queryByRole('button', { name: /save escalation/i })).toBeNull()
  })
})
