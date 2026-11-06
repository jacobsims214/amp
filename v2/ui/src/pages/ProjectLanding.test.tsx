// /project/:id lands on its tab straight from cached wait counts (#132).
import { render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import { ProjectLanding } from './ProjectLanding'
import { resetWaitCounts } from '../lib/waitCounts'
import type { MyWorkCounts } from '../types'

const z = { waiting: 0, approve: 0, answer: 0, merge: 0, start: 0, do: 0, coming_up: 0 }
const waiting: MyWorkCounts = { counts: { ...z, waiting: 2 }, projects: { 2: { ...z, waiting: 2 } } }
let countsCalls = 0
let inFlight = 0

const server = setupServer(
  http.get('*/api/my-work/counts', () => { countsCalls++; return HttpResponse.json(waiting) }),
)
server.events.on('request:start', () => { inFlight++ })
server.events.on('request:end', () => { inFlight-- })
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { resetWaitCounts(); countsCalls = 0 })
afterEach(async () => { await waitFor(() => expect(inFlight).toBe(0)); server.resetHandlers() })
afterAll(() => server.close())

function Where() { const l = useLocation(); return <div data-testid="where">{l.pathname}{l.search}</div> }

function mount(initial = '/project/2') {
  const router = createMemoryRouter([
    { path: '/project/:projectId', element: <ProjectLanding /> },
    { path: '/project/:projectId/:tab', element: <Where /> },
  ], { initialEntries: [initial] })
  return render(<RouterProvider router={router} />)
}

describe('ProjectLanding', () => {
  it('with warm counts goes straight to the tab, never shows the loader, and refreshes in the background', () => {
    resetWaitCounts(waiting)
    mount()
    // Synchronous: no waiting on the network.
    expect(screen.queryByRole('status', { name: 'Opening project' })).toBeNull()
    expect(screen.getByTestId('where').textContent).toBe('/project/2/my-work')
    return waitFor(() => expect(countsCalls).toBe(1))
  })

  it('with cold counts shows the loader, then navigates', async () => {
    mount('/project/2?x=1')
    expect(screen.getByRole('status', { name: 'Opening project' })).toBeTruthy()
    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/project/2/my-work?x=1'))
  })
})
