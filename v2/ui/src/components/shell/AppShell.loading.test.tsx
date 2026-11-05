// The sidebar's project group is there before the project list answers, so it
// doesn't pop in and shift the layout (#132).
import { act, render, screen, within } from '@testing-library/react'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../../api/client'
import { AppShell } from './AppShell'
import { resetWaitCounts } from '../../lib/waitCounts'

const never = <T,>() => new Promise<T>(() => {})
beforeEach(() => resetWaitCounts())
afterEach(() => { vi.restoreAllMocks(); resetWaitCounts() })

describe('AppShell project group', () => {
  it('draws the nine project view links before the project list answers', async () => {
    vi.spyOn(api, 'listProjects').mockImplementation(never)
    vi.spyOn(api, 'myWorkCounts').mockImplementation(never)
    vi.spyOn(api, 'getProject').mockImplementation(never)
    const router = createMemoryRouter([{ element: <AppShell />, children: [{ path: '/project/:projectId/board', element: <p>Board</p> }] }],
      { initialEntries: ['/project/3/board'] })
    render(<RouterProvider router={router} />)
    await act(async () => {})
    const nav = screen.getByRole('complementary', { name: 'Main' })
    expect(within(nav).getByText('Project')).toBeInTheDocument()
    for (const label of ['Mission', 'My work', 'Board', 'Flight plan', 'Graph', 'Reports', 'Artifacts', 'Library', 'Settings']) {
      expect(within(nav).getAllByRole('link', { name: label }).some(a => a.getAttribute('href')?.startsWith('/project/3/'))).toBe(true)
    }
  })
})
