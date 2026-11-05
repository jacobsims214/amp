import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { Projects } from './Projects'
import type { Project } from '../types'

let projects: Project[] = []
const server = setupServer(
  http.get('*/api/projects', () => HttpResponse.json({ projects })),
  http.get('*/api/orgs', () => HttpResponse.json({ orgs: [] })),
)
class FakeEventSource { onmessage = null; onerror = null; addEventListener() {} removeEventListener() {} close() {} }
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'bypass' }) })
afterEach(() => { server.resetHandlers(); projects = [] })
afterAll(() => { server.close(); vi.unstubAllGlobals() })

function Where() {
  const loc = useLocation()
  return <span data-testid="where">{loc.pathname + loc.search}</span>
}

function renderAt(url: string) {
  return render(
    <MemoryRouter initialEntries={[url]}>
      <Routes><Route path="/projects" element={<><Projects /><Where /></>} /></Routes>
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
})
