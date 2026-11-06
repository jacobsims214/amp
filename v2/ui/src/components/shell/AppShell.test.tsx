import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, beforeEach, afterEach, beforeAll, afterAll } from 'vitest'
import { BrowserRouter } from 'react-router-dom'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { AppShell } from './AppShell'
import { AuthContext } from '../../contexts/auth-context'
import { RemoteContext, type RemoteDeps } from '../../remote/remote-context'
import type { Me, Project } from '../../types'
import { resetWaitCounts } from '../../lib/waitCounts'

const me: Me = {
  subject: 'dev@example.com',
  email: 'dev@example.com',
  name: 'Dev User',
  kind: 'human',
  roles: ['admin'],
  orgs: [{ id: 2, name: 'Engineering', slug: 'eng', my_role: 'owner', created_by: null, created_at: '' }],
}

const projects: Project[] = [
  { id: 7, name: 'Home OS', code: 'home-os', state: 'active', description: '', metadata: {}, created_at: '', updated_at: '' },
]

const remoteDeps: RemoteDeps = {
  transport: {} as RemoteDeps['transport'],
  crypto: {} as RemoteDeps['crypto'],
  mock: false,
}

function Wrapper({ children }: { children: React.ReactNode }) {
  return (
    <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: true }}>
      <RemoteContext.Provider value={remoteDeps}>
        <BrowserRouter>{children}</BrowserRouter>
      </RemoteContext.Provider>
    </AuthContext.Provider>
  )
}

const server = setupServer(
  http.get('/api/projects', () => HttpResponse.json({ projects })),
  http.get('/api/my-work/counts', () => HttpResponse.json({ counts: { waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0 }, projects: {} })),
)

describe('AppShell sidebar animations', () => {
  beforeAll(() => server.listen())
  beforeEach(() => {
    resetWaitCounts({ counts: { waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0 }, projects: {} })
  })
  afterEach(() => { server.resetHandlers(); resetWaitCounts() })
  afterAll(() => server.close())

  it('renders the collapse toggle with accessible labels', async () => {
    render(<AppShell />, { wrapper: Wrapper })
    await waitFor(() => expect(screen.getByRole('complementary', { name: 'Main' })).toBeInTheDocument())
    const toggle = screen.getByRole('button', { name: 'Collapse sidebar' })
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    expect(toggle).toHaveAttribute('aria-controls', 'main-nav')
  })

  it('collapses and expands the sidebar, rotating the caret', async () => {
    const user = userEvent.setup()
    render(<AppShell />, { wrapper: Wrapper })
    await waitFor(() => expect(screen.getByRole('complementary', { name: 'Main' })).toBeInTheDocument())

    const toggle = screen.getByRole('button', { name: 'Collapse sidebar' })
    await user.click(toggle)

    await waitFor(() => expect(screen.getByRole('button', { name: 'Expand sidebar' })).toBeInTheDocument())
    expect(toggle).toHaveAttribute('aria-expanded', 'false')

    await user.click(screen.getByRole('button', { name: 'Expand sidebar' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Collapse sidebar' })).toBeInTheDocument())
  })

  it('shows icon-only tooltips when collapsed', async () => {
    const user = userEvent.setup()
    render(<AppShell />, { wrapper: Wrapper })
    await waitFor(() => expect(screen.getByRole('complementary', { name: 'Main' })).toBeInTheDocument())

    await user.click(screen.getByRole('button', { name: 'Collapse sidebar' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Expand sidebar' })).toBeInTheDocument())

    const myWork = screen.getByRole('link', { name: 'My work' })
    expect(myWork).toBeInTheDocument()
    expect(myWork).toHaveAccessibleName('My work')
  })
})
