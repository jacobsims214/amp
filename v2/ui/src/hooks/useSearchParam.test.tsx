import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { useSearchParam } from './useSearchParam'

function Probe() {
  const [tab, setTab] = useSearchParam('tab', 'details')
  const loc = useLocation()
  return (
    <div>
      <output data-testid="tab">{tab}</output>
      <output data-testid="search">{loc.search}</output>
      <button type="button" onClick={() => setTab('history')}>history</button>
      <button type="button" onClick={() => setTab('details')}>details</button>
      <button type="button" onClick={() => setTab(null)}>clear</button>
    </div>
  )
}

function setup(url: string) {
  const router = createMemoryRouter([{ path: '/project/:projectId/*', element: <Probe /> }], { initialEntries: [url] })
  render(<RouterProvider router={router} />)
  return router
}

const tab = () => screen.getByTestId('tab').textContent
const search = () => screen.getByTestId('search').textContent

describe('useSearchParam', () => {
  it('falls back to the default when the param is absent', () => {
    setup('/project/2/board?focus=5')
    expect(tab()).toBe('details')
  })

  it('sets the param with replace and keeps the other params', async () => {
    const router = setup('/project/2/board?focus=5')
    await userEvent.click(screen.getByRole('button', { name: 'history' }))
    expect(tab()).toBe('history')
    expect(search()).toBe('?focus=5&tab=history')
    expect(router.state.historyAction).toBe('REPLACE')
  })

  it('leaves the default (and null) out of the URL', async () => {
    setup('/project/2/board?focus=5&tab=history')
    await userEvent.click(screen.getByRole('button', { name: 'details' }))
    expect(search()).toBe('?focus=5')
    await userEvent.click(screen.getByRole('button', { name: 'history' }))
    await userEvent.click(screen.getByRole('button', { name: 'clear' }))
    expect(search()).toBe('?focus=5')
    expect(tab()).toBe('details')
  })
})
