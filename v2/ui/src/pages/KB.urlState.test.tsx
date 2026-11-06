// Library URL state: ?doc, ?q and ?tag survive a reload; the tag cloud is capped.
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const doc = (path: string) => ({
  id: path, project_id: 3, path, title: `Title of ${path}`, content: 'Body text here', tags: [], author: 'a',
  chunk_index: 0, chunk_text: '', updated_at: 1, annotations: [],
})
const summary = (path: string) => ({ id: path, path, title: `Title of ${path}`, tags: [], updated_at: 1 })
const tagList = Array.from({ length: 15 }, (_, i) => ({ tag: `tag${i}`, count: 15 - i }))
tagList[0] = { tag: 'a-very-long-tag-name-that-should-be-cut-off', count: 15 }

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
  return router
}
const url = () => screen.getByTestId('url').textContent

beforeEach(() => {
  kb.kbInfo.mockResolvedValue({ scope: 'project', can_write: true })
  kb.kbList.mockResolvedValue([summary('docs/a.md'), summary('docs/b.md')])
  kb.kbTags.mockResolvedValue(tagList)
  kb.kbSearch.mockResolvedValue([])
  kb.kbSources.mockResolvedValue([])
  kb.kbGet.mockImplementation((_r: unknown, p: string) => Promise.resolve(doc(p)))
})
afterEach(() => { cleanup(); vi.clearAllMocks() })

describe('Library URL state', () => {
  it('?doc deep link opens the doc', async () => {
    renderAt('/project/3/kb?doc=docs%2Fb.md')
    await screen.findByText('Body text here')
    expect(kb.kbGet).toHaveBeenCalledWith({ project_id: 3 }, 'docs/b.md')
  })

  it('opening a doc sets ?doc and Back returns to the browse list', async () => {
    const user = userEvent.setup()
    const router = renderAt('/project/3/kb')
    await user.click(await screen.findByText('Title of docs/a.md'))
    await screen.findByText('Body text here')
    expect(url()).toBe('/project/3/kb?doc=docs%2Fa.md')
    await router.navigate(-1)
    await screen.findByText('Title of docs/b.md')
    expect(screen.queryByText('Body text here')).toBeNull()
    expect(url()).toBe('/project/3/kb')
  })

  it('?q runs the search on load', async () => {
    renderAt('/project/3/kb?q=approval')
    expect(await screen.findByPlaceholderText('Search docs…')).toHaveValue('approval')
    await vi.waitFor(() => expect(kb.kbSearch).toHaveBeenCalledWith({ project_id: 3 }, 'approval'))
  })

  it('?tag filters the list', async () => {
    renderAt('/project/3/kb?tag=tag3')
    await vi.waitFor(() => expect(kb.kbList).toHaveBeenCalledWith({ project_id: 3 }, 'tag3'))
  })

  it('shows the top 12 tags and a +N more toggle that expands; long tags carry a title', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/kb')
    await screen.findByText('tag11')
    expect(screen.queryByText('tag12')).toBeNull()
    expect(screen.getByTitle('a-very-long-tag-name-that-should-be-cut-off')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '+3 more' }))
    expect(screen.getByText('tag14')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'show fewer' }))
    expect(screen.queryByText('tag14')).toBeNull()
  })

  it('clicking a tag writes ?tag', async () => {
    const user = userEvent.setup()
    renderAt('/project/3/kb')
    await user.click(await screen.findByText('tag2'))
    expect(url()).toBe('/project/3/kb?tag=tag2')
  })
})

describe('Library search hits', () => {
  const hit = (chunk: string) => ({
    path: 'docs/a.md', title: 'Title of docs/a.md', tags: [], excerpt: chunk.slice(0, 120), chunk_text: chunk,
    author: 'a', updated_at: 1, score: 0.8, vector_score: 0.71, chunk_index: 3, annotation_count: 2, latest_annotation: 'needs a rewrite',
  })

  it('shows the whole matched passage, score and chunk index on demand', async () => {
    const user = userEvent.setup()
    const chunk = 'Approval flow. ' + 'details '.repeat(40) + 'FINAL-WORDS'
    kb.kbSearch.mockResolvedValue([hit(chunk)])
    renderAt('/project/3/kb?q=approval')
    expect(await screen.findByText(/2 annotations/)).toBeInTheDocument()
    expect(screen.getByText(/needs a rewrite/)).toBeInTheDocument()
    expect(screen.queryByText(/FINAL-WORDS/)).toBeNull()
    await user.click(screen.getByRole('button', { name: 'Show passage' }))
    expect(screen.getByLabelText('Matched passage')).toHaveTextContent('FINAL-WORDS')
    expect(screen.getByText(/Score 0\.80 · semantic 0\.71 · chunk 3/)).toBeInTheDocument()
    expect(screen.getByText(/agents see only the short excerpt/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Hide passage' }))
    expect(screen.queryByLabelText('Matched passage')).toBeNull()
  })

  it('offers no toggle when the hit carries no chunk text', async () => {
    kb.kbSearch.mockResolvedValue([{ ...hit('short chunk'), chunk_text: undefined }])
    renderAt('/project/3/kb?q=approval')
    await screen.findByText(/2 annotations/)
    expect(screen.queryByRole('button', { name: 'Show passage' })).toBeNull()
  })
})

describe('Library open-at-match, sidebar and annotations', () => {
  it('opening a hit highlights the matched block and keeps it through re-renders', async () => {
    const user = userEvent.setup()
    kb.kbGet.mockResolvedValue({ ...doc('docs/a.md'), content: 'First para.\n\nThe **matched** sentence lives [here](http://x.test).\n\nLast para.' })
    kb.kbSearch.mockResolvedValue([{
      path: 'docs/a.md', title: 'Title of docs/a.md', tags: [], excerpt: 'unique-excerpt', chunk_text: 'The **matched** sentence lives [here](http://x.test).',
      author: 'a', updated_at: 1, score: 0.8, chunk_index: 2,
    }])
    renderAt('/project/3/kb?q=matched')
    await user.click(await screen.findByText('unique-excerpt'))
    const hit = (await screen.findByText('matched')).closest('p')!
    expect(url()).toBe('/project/3/kb?doc=docs%2Fa.md')
    await vi.waitFor(() => expect(hit).toHaveAttribute('data-chunk-hit'))
    expect(screen.getByText('First para.')).not.toHaveAttribute('data-chunk-hit')
    await user.click(screen.getByRole('button', { name: /Annotations/ }))
    await user.type(screen.getByPlaceholderText(/annotation|note|add/i), 'x') // re-renders DocView
    expect(hit).toBeInTheDocument()
    expect(hit).toHaveAttribute('data-chunk-hit')
  })

  it('highlights paragraphs with markup next to punctuation and snake_case', async () => {
    const user = userEvent.setup()
    const chunk = '**Note**: call `amp_kb_search`) with my_var_name and *a*.'
    kb.kbGet.mockImplementation(async (_r: unknown, path: string) => ({ ...doc(path), content: path === 'docs/a.md' ? `Intro line here.\n\n${chunk}` : `Other intro.\n\n${chunk}` }))
    kb.kbSearch.mockResolvedValue([{ path: 'docs/a.md', title: 'Title of docs/a.md', tags: [], excerpt: 'unique-excerpt', chunk_text: chunk, author: 'a', updated_at: 1, score: 0.8, chunk_index: 1 }])
    renderAt('/project/3/kb?q=note')
    await user.click(await screen.findByText('unique-excerpt'))
    const hit = (await screen.findByText('Note')).closest('p')!
    await vi.waitFor(() => expect(hit).toHaveAttribute('data-chunk-hit'))
    expect(hit.style.background).not.toBe('')
  })

  it('shows an annotation count in the doc header and the panel above the text', async () => {
    const ann = { author: 'sam', text: 'fix this', created_at: 1, is_resolved: false }
    kb.kbGet.mockResolvedValue({ ...doc('docs/a.md'), annotations: [ann, { ...ann, text: 'and this' }] })
    renderAt('/project/3/kb?doc=docs%2Fa.md')
    expect(await screen.findByRole('link', { name: /2 annotations/ })).toBeInTheDocument()
    expect(screen.getByText('fix this')).toBeInTheDocument()
  })

  it('restores the saved sidebar width and clamps it', async () => {
    localStorage.setItem('amp.kb.sidebarWidth', '9999')
    renderAt('/project/3/kb')
    const handle = await screen.findByRole('separator', { name: 'Resize sidebar' })
    expect(handle.parentElement).toHaveStyle({ width: '640px' })
    localStorage.removeItem('amp.kb.sidebarWidth')
  })
})
