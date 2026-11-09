// Tier 1 (#132): Mission and Flight plan draw their header at once and
// content-shaped skeletons after 250 ms, never the full-page band loader.
import { act, render, screen } from '@testing-library/react'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('../hooks/useSSE', () => ({ useSSE: () => {}, subscribeSSE: () => () => {} }))
vi.mock('../api/client', () => ({
  api: new Proxy({}, { get: () => () => new Promise(() => {}) }),
}))
const { Mission } = await import('./Mission')
const { FlightPlan } = await import('./FlightPlan')

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

function at(path: string, route: string, el: React.ReactNode) {
  render(<RouterProvider router={createMemoryRouter([{ path: route, element: el }], { initialEntries: [path] })} />)
}

describe.each([
  ['Mission', '/project/3/mission', '/project/:projectId/mission', <Mission />, 'Loading mission'],
  ['Flight plan', '/project/3/plan', '/project/:projectId/plan', <FlightPlan />, 'Loading the flight plan'],
] as const)('%s first load', (_name, path, route, el, label) => {
  it('draws the header at once, skeletons after 250 ms, and no band loader', async () => {
    at(path, route, el)
    expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()
    expect(screen.queryByRole('status', { name: label })).toBeNull()
    await act(() => vi.advanceTimersByTimeAsync(260))
    expect(screen.getByRole('status', { name: label })).toHaveAttribute('aria-busy', 'true')
    expect(document.querySelector('.amp-band-loader')).toBeNull()
  })
})
