import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { Projects } from './Projects'
import { subscribeSSE } from '../hooks/useSSE'
import type { Project } from '../types'

let projects: Project[] = []
let created: Record<string, unknown> | null = null
// How many times the page read the list, and a gate that holds a read until opened.
let listReads = 0
let listGate: Promise<void> | null = null
const server = setupServer(
  http.get('*/api/projects', async () => {
    listReads++
    if (listGate) await listGate
    return HttpResponse.json({ projects })
  }),
  http.post('*/api/projects', async ({ request }) => {
    created = await request.json() as Record<string, unknown>
    return HttpResponse.json({ id: 41, ...created })
  }),
  http.get('*/api/orgs', () => HttpResponse.json({ orgs: [] })),
)
// The real useSSE, over a fake stream: frames reach the page as they would live.
// jsdom has no navigator.locks, so useSSE opens one stream per tab.
let sources: FakeEventSource[] = []
class FakeEventSource {
  constructor() { sources.push(this) }
  onmessage: ((e: { data: string }) => void) | null = null
  onerror = null
  readyState = 1
  close() {}
}
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'bypass' }) })
afterEach(() => { server.resetHandlers(); projects = []; created = null; listReads = 0; listGate = null; sources = [] })
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

function project(id: number, name: string, state: Project['state'] = 'active'): Project {
  return { id, name, code: `P${id}`, description: '', state, created_at: '2026-11-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z' }
}

function send(data: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify(data) }))
}

/** The server's first frame on every new stream. It carries no project. */
function connect() {
  send({ type: 'connected' })
}

/** Lets any request the page just made reach the server and render. */
async function settle() {
  await act(async () => { await new Promise(r => setTimeout(r, 50)) })
}

describe('Projects after a stream reconnect', () => {
  it('re-reads the list once on each connected after the first load has settled', async () => {
    projects = [project(1, 'Apollo'), project(2, 'Borealis')]
    renderAt('/projects')
    expect(await screen.findByRole('heading', { name: 'Apollo' })).toBeInTheDocument()
    expect(listReads).toBe(1)

    // An ordinary event is applied as it comes, not taken for a reconnect.
    send({ type: 'project.created', payload: project(3, 'Cygnus') })
    expect(await screen.findByRole('heading', { name: 'Cygnus' })).toBeInTheDocument()
    await settle()
    expect(listReads).toBe(1)

    // While the stream was down, Delta was created and Borealis archived.
    projects = [project(1, 'Apollo'), project(2, 'Borealis', 'archived'), project(3, 'Cygnus'), project(4, 'Delta')]
    connect()
    expect(await screen.findByRole('heading', { name: 'Delta' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Borealis' })).toBeNull()
    expect(screen.getByRole('button', { name: /Archived \(1\)/ })).toBeInTheDocument()
    expect(screen.getByText('live')).toBeInTheDocument()
    await settle()
    expect(listReads).toBe(2)

    connect()
    await waitFor(() => expect(listReads).toBe(3))
    await settle()
    expect(listReads).toBe(3)
  })

  it('a connected during the first load causes no extra request', async () => {
    projects = [project(1, 'Apollo')]
    let open!: () => void
    listGate = new Promise(r => { open = r })
    renderAt('/projects')
    await waitFor(() => expect(listReads).toBe(1))

    // The opening frame of the stream the page just opened: the list is being read.
    connect()
    open()
    expect(await screen.findByRole('heading', { name: 'Apollo' })).toBeInTheDocument()
    await settle()
    expect(listReads).toBe(1)

    // The next connected is a reconnect.
    connect()
    await waitFor(() => expect(listReads).toBe(2))
  })

  it('a page that mounts on a stream another view holds re-reads on the first connected it sees', async () => {
    // Another view (the board store lingers for 30 s) opened the stream and
    // heard its opening frame before this page mounted.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      projects = [project(1, 'Apollo')]
      renderAt('/projects')
      expect(await screen.findByRole('heading', { name: 'Apollo' })).toBeInTheDocument()
      expect(screen.getByText('live')).toBeInTheDocument() // no connected of its own needed (#2540)
      expect(sources).toHaveLength(1)
      expect(listReads).toBe(1)

      projects = [project(1, 'Apollo'), project(2, 'Borealis')]
      connect()
      expect(await screen.findByRole('heading', { name: 'Borealis' })).toBeInTheDocument()
      await settle()
      expect(listReads).toBe(2)
    } finally {
      unsubscribe()
    }
  })
})
