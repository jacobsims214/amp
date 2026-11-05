import { render, screen } from '@testing-library/react'
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
})
