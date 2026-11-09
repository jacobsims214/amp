import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { Projects } from './Projects'
import type { Project } from '../types'

let projects: Project[] = []
let created: Record<string, unknown> | null = null
const server = setupServer(
  http.get('*/api/projects', () => HttpResponse.json({ projects })),
  http.post('*/api/projects', async ({ request }) => {
    created = await request.json() as Record<string, unknown>
    return HttpResponse.json({ id: 41, ...created })
  }),
  http.get('*/api/orgs', () => HttpResponse.json({ orgs: [] })),
)
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'bypass' }) })
afterEach(() => { server.resetHandlers(); projects = []; created = null })
afterAll(() => { server.close(); vi.unstubAllGlobals() })

function Where() {
  const loc = useLocation()
  return <span data-testid="where">{loc.pathname + loc.search}</span>
}

function renderAt(url: string) {
  return render(
    <MemoryRouter initialEntries={[url]}>
      <Routes><Route path="/projects" element={<><Projects /><Where /></>} /><Route path="/project/:id" element={<Where />} /></Routes>
    </MemoryRouter>,
  )
}

describe('Projects', () => {
  it('/projects?new=1 opens New project at once and drops the flag from the URL', async () => {
    renderAt('/projects?new=1')
    expect(await screen.findByRole('heading', { name: 'New Project' })).toBeInTheDocument()
    expect(await screen.findByText('/projects', { exact: true })).toHaveAttribute('data-testid', 'where') // ?new dropped
  })

  it('with no projects, leads with Create your first project and links the manager agent', async () => {
    renderAt('/projects')
    const create = await screen.findByRole('button', { name: 'Create your first project' })
    expect(screen.getByRole('link', { name: 'manager agent' })).toHaveAttribute('href', '/guide?doc=setup/connect-your-agent.md')
    expect(screen.queryByText(/amp-manager/)).toBeNull()
    await userEvent.click(create)
    expect(screen.getByRole('heading', { name: 'New Project' })).toBeInTheDocument()
  })

  it('New project: the code follows the name, keeps only A-Z0-9, and Enter creates it', async () => {
    renderAt('/projects?new=1')
    const name = await screen.findByLabelText('Name *')
    const code = screen.getByLabelText('Code *')
    await userEvent.type(name, 'Pelican One')
    expect(code).toHaveValue('PO')
    await userEvent.clear(code)
    await userEvent.type(code, 'pel 1')
    expect(code).toHaveValue('PEL1')
    await userEvent.type(name, 's') // a code you typed stays yours
    expect(code).toHaveValue('PEL1')
    await userEvent.type(name, '{Enter}')
    await waitFor(() => expect(screen.getByTestId('where')).toHaveTextContent('/project/41'))
    expect(created).toMatchObject({ name: 'Pelican Ones', code: 'PEL1' })
  })

  it('New project: missing fields are flagged at the field and cleared as you type', async () => {
    renderAt('/projects?new=1')
    const name = await screen.findByLabelText('Name *')
    await userEvent.type(name, '{Enter}')
    expect(name).toHaveAttribute('aria-invalid', 'true')
    expect(name).toHaveAccessibleDescription('Give the project a name.')
    expect(screen.getByLabelText('Code *')).toHaveAccessibleDescription('Give it a code: letters A-Z and digits.')
    expect(created).toBeNull()
    await userEvent.type(name, 'Ops')
    expect(name).toHaveAttribute('aria-invalid', 'false')
    expect(screen.queryByText('Give the project a name.')).toBeNull()
    expect(screen.queryByText('Give it a code: letters A-Z and digits.')).toBeNull() // the suggestion filled it
    expect(screen.getByLabelText('Code *')).toHaveValue('OPS')
  })
})
