import { act, cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { subscribeSSE } from '../hooks/useSSE'
import { resetSession } from '../lib/session'
import type { Artifact } from '../types'

const day = 86_400_000
const art = (id: number, over: Partial<Artifact>): Artifact => ({
  id, project_id: 3, task_id: null, title: `Artifact ${id}`, kind: 'html', status: 'in_review', current_version: 1,
  approved_version: null, reviewed_by: null, reviewed_at: null, created_by: 1, created_at: '', updated_at: `2026-10-0${id}T00:00:00Z`,
  keep: false, kept_by: null, kept_at: null, expires_at: new Date(Date.now() + 30 * day).toISOString(), expiry_dismissed_at: null, ...over,
})
const items = [
  art(1, { title: 'Login mockup', kind: 'html', task_id: 11, task_ref: 'P-11', task_name: 'Build login' }),
  art(2, { title: 'Flow diagram', kind: 'svg', status: 'approved', expires_at: new Date(Date.now() + 2 * day).toISOString() }),
  art(3, { title: 'Spec', kind: 'markdown', keep: true, expires_at: null }),
]

// The list read: counts calls, and a test can hold it in flight (listGate) or fail it (listFails).
let listed = items
let listReads = 0
let listGate: Promise<void> | null = null
let listFails = 0
async function listArtifacts(): Promise<Artifact[]> {
  listReads++
  const gate = listGate
  if (gate) await gate
  if (listFails > 0) { listFails--; throw new Error('amp-api is down') }
  return listed
}
const keepSpy = vi.fn<(id: number, keep: boolean) => Promise<unknown>>(() => Promise.resolve({}))
vi.mock('../api/client', () => ({
  api: { listArtifacts: () => listArtifacts(), artifactContentUrl: () => '/x', keepArtifact: (id: number, keep: boolean) => keepSpy(id, keep) },
}))
vi.mock('../components/ProjectNav', () => ({
  ProjectNav: ({ rightSlot }: { rightSlot?: React.ReactNode }) => <div>{rightSlot}</div>,
}))

const { Artifacts } = await import('./Artifacts')

// The real useSSE, over a fake stream: frames reach the page as they would live.
// jsdom has no navigator.locks, so useSSE opens one stream per tab.
let sources: FakeEventSource[] = []
class FakeEventSource {
  constructor() { sources.push(this) }
  onmessage: ((e: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  readyState = 1
  close() {}
}
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource) })
afterEach(() => { listed = items; listReads = 0; listGate = null; listFails = 0; sources = []; resetSession() })
afterAll(() => { vi.unstubAllGlobals() })

function send(data: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify(data) }))
}
/** The server's first frame on every new stream. It carries no project. */
const connect = () => send({ type: 'connected' })
/** Let pending reads and their state updates (the settled flag included) finish. */
const settle = () => act(async () => { for (let i = 0; i < 50; i++) await Promise.resolve() })
/** Hold the next list read in flight until the returned function is called. */
function holdReads(): () => void {
  let release!: () => void
  listGate = new Promise(r => { release = r })
  return () => { listGate = null; release() }
}
const published = art(4, { title: 'Published while away' })

function Where() {
  const loc = useLocation()
  return <span data-testid="url">{loc.pathname + loc.search}</span>
}
function renderAt(path: string) {
  const router = createMemoryRouter([{ path: '/project/:projectId/artifacts', element: <><Artifacts /><Where /></> }], { initialEntries: [path] })
  render(<RouterProvider router={router} />)
}
const url = () => screen.getByTestId('url').textContent

describe('Artifacts filters', () => {
  it('keeps an expiring artifact in one click and links versions', async () => {
    renderAt('/project/3/artifacts')
    await screen.findByText('Login mockup')
    expect(screen.queryByRole('button', { name: 'Keep Login mockup' })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Keep Flow diagram' }))
    expect(keepSpy).toHaveBeenCalledWith(2, true)
    expect(screen.getAllByRole('link', { name: 'v1' }).map(l => l.getAttribute('href'))).toContain('/artifacts/1?v=1')
  })

  it('shows an error when keep fails, then recovers on the next try', async () => {
    keepSpy.mockClear()
    keepSpy.mockRejectedValueOnce(new Error('Network down'))
    renderAt('/project/3/artifacts')
    await screen.findByText('Login mockup')
    const card = screen.getByText('Flow diagram').closest('.amp-panel') as HTMLElement
    const keepBtn = () => within(card).getByRole('button', { name: 'Keep Flow diagram' })
    await userEvent.click(keepBtn())
    expect(await within(card).findByRole('alert')).toHaveTextContent('Network down')
    expect(keepBtn()).toBeEnabled()
    await userEvent.click(keepBtn())
    expect(keepSpy).toHaveBeenCalledTimes(2)
    expect(within(card).queryByRole('alert')).toBeNull()
  })

  it('sorts by ticket and writes it to the URL', async () => {
    renderAt('/project/3/artifacts')
    await screen.findByText('Login mockup')
    await userEvent.selectOptions(screen.getByLabelText('Sort'), 'ticket')
    expect(url()).toContain('sort=ticket')
    expect(screen.getAllByRole('link', { name: /^(Login mockup|Flow diagram|Spec)$/ })[0]).toHaveAccessibleName('Login mockup')
    const sec = screen.getByRole('region', { name: 'P-11 Build login' })
    expect(within(sec).getByText('Login mockup')).toBeInTheDocument()
    expect(within(sec).queryByText('Spec')).toBeNull()
    expect(within(screen.getByRole('region', { name: 'No ticket' })).getByText('Spec')).toBeInTheDocument()
    expect(within(sec).getByRole('link', { name: 'P-11 Build login' })).toHaveAttribute('href', expect.stringContaining('11'))
  })

  it('reads kind and expiry from the URL', async () => {
    renderAt('/project/3/artifacts?kind=svg')
    expect(await screen.findByText('Flow diagram')).toBeInTheDocument()
    expect(screen.queryByText('Login mockup')).toBeNull()
    expect(screen.getByText('1 of 3')).toBeInTheDocument()
  })

  it('filters expiring soon and kept, and writes them to the URL', async () => {
    renderAt('/project/3/artifacts')
    await screen.findByText('Login mockup')
    await userEvent.selectOptions(screen.getByLabelText('Expiry'), 'soon')
    expect(url()).toBe('/project/3/artifacts?expiry=soon')
    expect(screen.getByText('Flow diagram')).toBeInTheDocument()
    expect(screen.queryByText('Spec')).toBeNull()
    await userEvent.selectOptions(screen.getByLabelText('Expiry'), 'kept')
    expect(screen.getByText('Spec')).toBeInTheDocument()
    expect(screen.queryByText('Flow diagram')).toBeNull()
  })

  it('finds by ticket ref and links to the owning ticket', async () => {
    renderAt('/project/3/artifacts')
    await screen.findByText('Login mockup')
    await userEvent.type(screen.getByLabelText('Find artifacts'), 'p-11')
    expect(screen.queryByText('Spec')).toBeNull()
    const link = within(screen.getByText('Login mockup').closest('.amp-panel') as HTMLElement).getByRole('link', { name: 'P-11' })
    expect(link).toHaveAttribute('href', '/project/3/board?focus=11')
  })

  it('ignores junk values and clears filters', async () => {
    renderAt('/project/3/artifacts?kind=bogus&status=approved')
    expect(await screen.findByText('Flow diagram')).toBeInTheDocument()
    await userEvent.click(screen.getByText('Clear filters'))
    expect(url()).toBe('/project/3/artifacts')
    expect(screen.getByText('3 of 3')).toBeInTheDocument()
  })
})

describe('Artifacts after a stream reconnect', () => {
  it('drops a connected that arrives while the first read is in flight', async () => {
    const release = holdReads()
    renderAt('/project/3/artifacts')
    expect(listReads).toBe(1)
    connect() // the new stream's opening frame: the read in flight is already fresh
    expect(listReads).toBe(1)
    release()
    expect(await screen.findByText('Login mockup')).toBeInTheDocument()
    await settle()
    expect(listReads).toBe(1) // dropped, not saved for later
  })

  it('re-reads the list exactly once on each connected after the first read settles', async () => {
    renderAt('/project/3/artifacts')
    expect(await screen.findByText('Login mockup')).toBeInTheDocument()
    await settle()
    expect(listReads).toBe(1)

    // Published while the stream was down: no artifact.updated ever arrives.
    listed = [...items, published]
    const release = holdReads()
    connect()
    expect(listReads).toBe(2)
    // A quiet re-read: the list stays on screen while it is in flight.
    expect(screen.getByText('Login mockup')).toBeInTheDocument()
    release()
    expect(await screen.findByText('Published while away')).toBeInTheDocument()
    await settle()
    expect(listReads).toBe(2)

    connect()
    expect(listReads).toBe(3)
    await settle()
    expect(listReads).toBe(3)
  })

  it('a page that mounts on a stream another view holds re-reads on the first connected it sees', async () => {
    // Another view opened the stream and heard its opening frame before this page mounted.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      renderAt('/project/3/artifacts')
      expect(await screen.findByText('Login mockup')).toBeInTheDocument()
      await settle()
      expect(sources).toHaveLength(1)
      expect(listReads).toBe(1)

      listed = [...items, published]
      connect()
      expect(listReads).toBe(2)
      expect(await screen.findByText('Published while away')).toBeInTheDocument()
      await settle()
      expect(listReads).toBe(2)
    } finally {
      cleanup()
      unsubscribe()
    }
  })

  it('retries a failed first read on the next connected', async () => {
    listFails = 1
    renderAt('/project/3/artifacts')
    expect(await screen.findByText('No artifacts yet')).toBeInTheDocument()
    await settle()
    connect()
    expect(listReads).toBe(2)
    expect(await screen.findByText('Login mockup')).toBeInTheDocument()
  })

  it('re-reads on artifact.updated, and not on other events', async () => {
    renderAt('/project/3/artifacts')
    expect(await screen.findByText('Login mockup')).toBeInTheDocument()
    await settle()
    send({ type: 'task.updated', project_id: 3 })
    send({ type: 'artifact.updated', project_id: 9 })
    expect(listReads).toBe(1)
    send({ type: 'artifact.updated', project_id: 3 })
    expect(listReads).toBe(2)
  })
})
