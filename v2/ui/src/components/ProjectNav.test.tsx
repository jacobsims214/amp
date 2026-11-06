import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { ProjectNav } from './ProjectNav'

vi.mock('../api/client', () => ({
  api: { getProject: vi.fn(() => new Promise(() => {})) },
}))
vi.mock('../lib/waitCounts', () => ({
  useWaitCounts: () => null,
  projectCounts: () => undefined,
}))

describe('ProjectNav while the project is unresolved', () => {
  it('shows a title skeleton, not "Loading…", and renders the nav links', () => {
    const { container } = render(
      <MemoryRouter><ProjectNav projectId={7} currentView="board" /></MemoryRouter>,
    )
    expect(screen.queryByText('Loading…')).toBeNull()
    const busy = container.querySelector('[aria-busy="true"]')
    expect(busy).not.toBeNull()
    expect(busy!.querySelector('.amp-skel')).not.toBeNull()
    expect(screen.getByRole('link', { name: /Board/ }).getAttribute('href')).toContain('7')
    expect(screen.getByRole('link', { name: /Mission/ })).toBeTruthy()
  })
})
