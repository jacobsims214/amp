import { describe, expect, it } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation, useNavigate } from 'react-router-dom'
import { useFocusParam } from './useFocusParam'
import { projectLink } from '../lib/projectLink'

function Probe() {
  const [focusId, setFocus] = useFocusParam()
  const loc = useLocation()
  const navigate = useNavigate()
  return (
    <div>
      <output data-testid="focus">{focusId ?? 'none'}</output>
      <output data-testid="search">{loc.search}</output>
      <button type="button" onClick={() => setFocus(7)}>open 7</button>
      <button type="button" onClick={() => setFocus(8)}>open 8</button>
      <button type="button" onClick={() => setFocus(null)}>dismiss</button>
      <button type="button" onClick={() => navigate(-1)}>back</button>
    </div>
  )
}

function setup(url: string) {
  const router = createMemoryRouter([{ path: '/project/:projectId/*', element: <Probe /> }], { initialEntries: [url] })
  render(<RouterProvider router={router} />)
  return router
}

const focus = () => screen.getByTestId('focus').textContent
const search = () => screen.getByTestId('search').textContent

describe('useFocusParam', () => {
  it('reads ?focus=', () => {
    setup('/project/2/board?focus=42')
    expect(focus()).toBe('42')
    expect(search()).toBe('?focus=42')
  })

  it.each([['ticket'], ['task']])('rewrites the ?%s= alias to ?focus= with replace', name => {
    const router = setup(`/project/2?${name}=42&tab=x`)
    expect(focus()).toBe('42')
    expect(search()).toBe('?tab=x&focus=42')
    // replace, not push: there's no extra history entry to go back through
    expect(router.state.historyAction).toBe('REPLACE')
  })

  it('prefers ?focus= over an alias and drops the alias', () => {
    setup('/project/2?task=5&focus=9')
    expect(focus()).toBe('9')
    expect(search()).toBe('?focus=9')
  })

  it('ignores ids that are not positive integers', () => {
    setup('/project/2?focus=abc')
    expect(focus()).toBe('none')
  })

  it('persists in the URL across a reload until dismissed', async () => {
    const user = userEvent.setup()
    const router = setup('/project/2/board')
    await user.click(screen.getByRole('button', { name: 'open 7' }))
    expect(search()).toBe('?focus=7')
    // A reload is a fresh router on the same URL.
    const url = router.state.location.pathname + router.state.location.search
    act(() => router.dispose())
    cleanup()
    setup(url)
    expect(focus()).toBe('7')
  })

  it('pushes on open so back returns to the previous ticket', async () => {
    const user = userEvent.setup()
    setup('/project/2/board')
    await user.click(screen.getByRole('button', { name: 'open 7' }))
    await user.click(screen.getByRole('button', { name: 'open 8' }))
    expect(focus()).toBe('8')
    await user.click(screen.getByRole('button', { name: 'back' }))
    expect(focus()).toBe('7')
    await user.click(screen.getByRole('button', { name: 'back' }))
    expect(focus()).toBe('none')
  })

  it('dismiss removes the param with replace and keeps other params', async () => {
    const user = userEvent.setup()
    const router = setup('/project/2/board?view=x&focus=7')
    await user.click(screen.getByRole('button', { name: 'dismiss' }))
    expect(focus()).toBe('none')
    expect(search()).toBe('?view=x')
    expect(router.state.historyAction).toBe('REPLACE')
  })
})

describe('projectLink', () => {
  it('builds every project route with ?focus=', () => {
    expect(projectLink(2)).toBe('/project/2')
    expect(projectLink(2, 'mission')).toBe('/project/2/mission')
    expect(projectLink(2, 'mission', 5)).toBe('/project/2/mission?focus=5')
    expect(projectLink(2, 'board', 5)).toBe('/project/2/board?focus=5')
    expect(projectLink(2, 'plan', 5)).toBe('/project/2/plan?focus=5')
    expect(projectLink(2, 'my-work', 5)).toBe('/project/2/my-work?focus=5')
    expect(projectLink('7', 'board')).toBe('/project/7/board')
    expect(projectLink(2, 'board', null)).toBe('/project/2/board')
  })
})
