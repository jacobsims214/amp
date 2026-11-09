// Library doc metadata (#159): created, updated and last agent read on the doc
// header and list items, and the browse list sorts by least read and stale.
import { cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const day = 86400
const now = Math.floor(Date.now() / 1000)
// Fixed past dates render as "Mon D, YYYY" whatever today is.
const jan2 = Date.UTC(2026, 0, 2, 12) / 1000
const mar4 = Date.UTC(2026, 2, 4, 12) / 1000

const summary = (path: string, extra: Record<string, unknown>) => ({
  id: path, path, title: `Title of ${path}`, tags: [], author: 'a', updated_at: mar4, created_at: jan2, read_count: 0, ...extra,
})
const docs = [
  summary('hot.md', { updated_at: now - day, read_count: 9, last_read_at: now - 3600, last_read_by: 'builder@agents' }),
  summary('old.md', { updated_at: jan2, read_count: 2, last_read_at: now - 3 * day, last_read_by: 'qa@agents' }),
  summary('unread.md', { updated_at: mar4 }),
]
const doc = (path: string) => ({
  ...docs.find(d => d.path === path)!, content: 'Body text here', chunk_index: 0, chunk_text: '', annotations: [],
})

const kb = vi.hoisted(() => ({
  kbInfo: vi.fn(), kbList: vi.fn(), kbTags: vi.fn(), kbSearch: vi.fn(), kbGet: vi.fn(), kbSources: vi.fn(),
}))
vi.mock('../api/client', () => ({ api: kb }))
vi.mock('../components/ProjectNav', () => ({ ProjectNav: () => null }))

const { KB } = await import('./KB')

function Where() {
  const loc = useLocation()
  return <span data-testid="url">{loc.pathname + loc.search}</span>
}
function renderAt(path: string) {
  const router = createMemoryRouter(
    [{ path: '/project/:projectId/kb', element: <><KB /><Where /></> }],
    { initialEntries: [path] },
  )
  render(<RouterProvider router={router} />)
}
const order = () => screen.getAllByRole('heading', { level: 3 }).map(h => h.textContent)

beforeEach(() => {
  kb.kbInfo.mockResolvedValue({ scope: 'project', can_write: true })
  kb.kbList.mockResolvedValue(docs)
  kb.kbTags.mockResolvedValue([])
  kb.kbSearch.mockResolvedValue([])
  kb.kbSources.mockResolvedValue([])
  kb.kbGet.mockImplementation((_r: unknown, p: string) => Promise.resolve(doc(p)))
})
afterEach(() => { cleanup(); vi.clearAllMocks() })

describe('Library read metadata', () => {
  it('the doc header shows created, updated and who last read it', async () => {
    renderAt('/project/3/kb?doc=old.md')
    await screen.findByText('Body text here')
    expect(screen.getByText('Created Jan 2, 2026')).toBeInTheDocument()
    expect(screen.getByText('Updated Jan 2, 2026')).toBeInTheDocument()
    const read = screen.getByText('Last read by qa@agents 3d ago')
    expect(read).toHaveAttribute('title', '2 agent reads counted')
  })

  it('a doc no agent has read says so', async () => {
    renderAt('/project/3/kb?doc=unread.md')
    await screen.findByText('Body text here')
    expect(screen.getByText('Not read by an agent yet')).toBeInTheDocument()
  })

  it('a doc without stored metadata (the AMP guide) shows no read line or sort', async () => {
    kb.kbList.mockResolvedValue([{ id: 'g', path: 'g.md', title: 'Guide', tags: [], author: '', updated_at: mar4 }])
    kb.kbGet.mockResolvedValue({ id: 'g', project_id: 0, path: 'g.md', title: 'Guide', tags: [], author: '', updated_at: mar4,
      content: 'Body text here', chunk_index: 0, chunk_text: '', annotations: [] })
    renderAt('/project/3/kb')
    await screen.findByText('Guide')
    expect(screen.queryByRole('combobox', { name: 'Sort documents' })).toBeNull()
    expect(screen.queryByText(/read by an agent/)).toBeNull()
    cleanup()
    renderAt('/project/3/kb?doc=g.md')
    await screen.findByText('Body text here')
    expect(screen.queryByText(/Last read|Not read/)).toBeNull()
    expect(screen.queryByText(/^Created/)).toBeNull()
  })

  it('list items show the read count, and the list sorts by least read and stale via ?sort', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/kb')
    await screen.findByText('Title of hot.md')
    expect(order()).toEqual(['Title of hot.md', 'Title of old.md', 'Title of unread.md'])
    const item = screen.getByText('Title of unread.md').closest('div.flex-1') as HTMLElement
    expect(within(item).getByText(/never read by an agent/)).toBeInTheDocument()
    expect(screen.getByText(/9 agent reads, last 1h ago/)).toBeInTheDocument()

    const sort = screen.getByRole('combobox', { name: 'Sort documents' })
    await user.selectOptions(sort, 'least-read')
    expect(order()).toEqual(['Title of unread.md', 'Title of old.md', 'Title of hot.md'])
    expect(screen.getByTestId('url').textContent).toBe('/project/3/kb?sort=least-read')

    await user.selectOptions(sort, 'stale')
    expect(order()).toEqual(['Title of old.md', 'Title of unread.md', 'Title of hot.md'])

    await user.selectOptions(sort, 'updated')
    expect(screen.getByTestId('url').textContent).toBe('/project/3/kb')
  })
})
