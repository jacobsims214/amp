import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import type { Project } from '../../types'
import { SIDEBAR_KEY } from '../../lib/sidebarPrefs'
import { ProjectTree } from './ProjectTree'

const mk = (id: number, name: string, org_id: number | null): Project => ({
  id, name, code: `P${id}`, description: '', state: 'active', org_id, created_at: '', updated_at: '',
} as Project)

const orgs = [{ id: 1, name: 'Acme' }, { id: 2, name: 'Globex' }]
const many = Array.from({ length: 12 }, (_, i) => mk(i + 1, `Proj ${i + 1}`, 1))
const small = [mk(1, 'Alpha', 1), mk(2, 'Beta', 1), mk(3, 'Gamma', 2), mk(4, 'Solo', null)]

beforeEach(() => localStorage.clear())

const renderTree = (projects: Project[], currentId?: string, wait: Record<number, number> = {}) =>
  render(<MemoryRouter><ProjectTree projects={projects} orgs={orgs} currentId={currentId} search="" waitIn={id => wait[Number(id)]} /></MemoryRouter>)

describe('ProjectTree', () => {
  it('lists every project (no cap), grouped by org', () => {
    renderTree(many)
    expect(screen.getAllByRole('link', { name: /^Proj \d+/ })).toHaveLength(12)
    renderTree(small)
    expect(screen.getByRole('region', { name: 'Globex' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'No org' })).toBeInTheDocument()
  })

  it('groups a project in an org the caller is not a member of under that org, not "No org"', () => {
    render(<MemoryRouter><ProjectTree projects={[mk(1, 'Alpha', 1), mk(2, 'Shared', 9), mk(3, 'Solo', null)]} orgs={orgs} search="" waitIn={() => undefined} /></MemoryRouter>)
    expect(within(screen.getByRole('region', { name: 'Org #9' })).getByRole('link', { name: /^Shared/ })).toBeInTheDocument()
    expect(within(screen.getByRole('region', { name: 'No org' })).queryByRole('link', { name: /^Shared/ })).toBeNull()
  })

  it('names a non-member org from the project\'s org_name', () => {
    render(<MemoryRouter><ProjectTree projects={[{ ...mk(2, 'Shared', 9), org_name: 'Initech' }]} orgs={orgs} search="" waitIn={() => undefined} /></MemoryRouter>)
    expect(within(screen.getByRole('region', { name: 'Initech' })).getByRole('link', { name: /^Shared/ })).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Org #9' })).toBeNull()
  })

  it('still groups by org_id when me.orgs is empty (auth off)', () => {
    render(<MemoryRouter><ProjectTree projects={[mk(1, 'Alpha', 1), mk(2, 'Beta', 2)]} orgs={[]} search="" waitIn={() => undefined} /></MemoryRouter>)
    expect(screen.getByRole('region', { name: 'Org #1' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Org #2' })).toBeInTheDocument()
  })

  it('collapses an org with aria-expanded and keeps a badge on the collapsed row', async () => {
    const user = userEvent.setup()
    renderTree(small, undefined, { 1: 2, 2: 1 })
    const btn = screen.getByRole('button', { name: /Collapse Acme, 3 waiting on you/ })
    expect(btn).toHaveAttribute('aria-expanded', 'true')
    await user.click(btn)
    const closed = screen.getByRole('button', { name: /Expand Acme, 3 waiting on you/ })
    expect(closed).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByRole('link', { name: /^Alpha/ })).toBeNull()
    expect(JSON.parse(localStorage.getItem(SIDEBAR_KEY)!).collapsedOrgs).toEqual(['org:1'])
  })

  it('expands a project to its views by keyboard; the current one is always open', async () => {
    const user = userEvent.setup()
    renderTree(small, '2')
    expect(screen.getByRole('link', { name: 'Board' })).toBeInTheDocument() // Beta is current
    const alpha = screen.getByRole('button', { name: 'Expand Alpha' })
    alpha.focus()
    await user.keyboard('{Enter}')
    expect(screen.getByRole('button', { name: 'Collapse Alpha' })).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getAllByRole('link', { name: 'Mission' })).toHaveLength(2)
    expect(screen.getByRole('button', { name: 'Collapse Beta' })).toBeDisabled()
  })

  it('pins a project to the top and reorders with the move buttons, persisted', async () => {
    const user = userEvent.setup()
    const { unmount } = renderTree(small)
    await user.click(screen.getByRole('button', { name: 'Pin Beta' }))
    const pinned = screen.getByRole('region', { name: 'Pinned projects' })
    expect(within(pinned).getByRole('link', { name: /^Beta/ })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Move Beta up' }))
    unmount()
    renderTree(small) // a fresh mount reads the saved state
    expect(within(screen.getByRole('region', { name: 'Pinned projects' })).getByRole('link', { name: /^Beta/ })).toBeInTheDocument()
  })

  it('moves a project up within its org', async () => {
    const user = userEvent.setup()
    renderTree(small)
    await user.click(screen.getByRole('button', { name: 'Move Beta up' }))
    const acme = screen.getByRole('region', { name: 'Acme' })
    expect(within(acme).getAllByRole('link').map(l => l.textContent)).toEqual(['BetaP2', 'AlphaP1'])
  })

  it('filters by name once the list is long, ignoring collapse', async () => {
    const user = userEvent.setup()
    renderTree(many)
    await user.type(screen.getByRole('searchbox', { name: 'Filter projects' }), 'proj 12')
    expect(screen.getAllByRole('link', { name: /^Proj/ })).toHaveLength(1)
    await user.clear(screen.getByRole('searchbox', { name: 'Filter projects' }))
    await user.type(screen.getByRole('searchbox', { name: 'Filter projects' }), 'zzz')
    expect(screen.getByText('No project matches.')).toBeInTheDocument()
  })

  it('keeps the current project open while a filter is active', async () => {
    const user = userEvent.setup()
    renderTree(many, '12')
    await user.type(screen.getByRole('searchbox', { name: 'Filter projects' }), 'proj 12')
    expect(screen.getByRole('link', { name: 'Board' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Collapse Proj 12' })).toBeDisabled()
  })

  it('shows no filter box for a short list', () => {
    renderTree(small)
    expect(screen.queryByRole('searchbox')).toBeNull()
  })
})
