import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
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

const keepSpy = vi.fn<(id: number, keep: boolean) => Promise<unknown>>(() => Promise.resolve({}))
vi.mock('../api/client', () => ({
  api: { listArtifacts: () => Promise.resolve(items), artifactContentUrl: () => '/x', keepArtifact: (id: number, keep: boolean) => keepSpy(id, keep) },
}))
vi.mock('../hooks/useSSE', () => ({ useSSE: () => {} }))
vi.mock('../components/ProjectNav', () => ({
  ProjectNav: ({ rightSlot }: { rightSlot?: React.ReactNode }) => <div>{rightSlot}</div>,
}))

const { Artifacts } = await import('./Artifacts')

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
