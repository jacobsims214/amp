import { describe, expect, it } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { flagParam, idListParam, idParam, patchParams, usePatchSearchParams, useSearchParamState } from './useSearchParamState'

const HIDE = flagParam(true, 'hide', 'show')

function Probe() {
  const [sel, setSel] = useSearchParamState('sel', idParam)
  const [hide, setHide] = useSearchParamState('done', HIDE)
  const [open] = useSearchParamState('open', idListParam)
  const patch = usePatchSearchParams()
  const loc = useLocation()
  return (
    <div>
      <output data-testid="sel">{sel ?? 'none'}</output>
      <output data-testid="hide">{String(hide)}</output>
      <output data-testid="open">{open.join('|')}</output>
      <output data-testid="search">{loc.search}</output>
      <button type="button" onClick={() => setSel(5, { push: true })}>select 5</button>
      <button type="button" onClick={() => setSel(null)}>clear</button>
      <button type="button" onClick={() => setHide(!hide)}>toggle</button>
      <button type="button" onClick={() => { patch({ a: '1' }); patch({ b: '2' }) }}>two writes</button>
    </div>
  )
}

function setup(url: string) {
  const router = createMemoryRouter([{ path: '*', element: <Probe /> }], { initialEntries: [url] })
  render(<RouterProvider router={router} />)
  return router
}
const text = (id: string) => screen.getByTestId(id).textContent

describe('patchParams', () => {
  it('sets, removes on null or empty, and keeps the rest', () => {
    const next = patchParams(new URLSearchParams('focus=3&q=x&epic=2'), { q: null, epic: '', status: 'done' })
    expect(next.toString()).toBe('focus=3&status=done')
  })
})

describe('useSearchParamState', () => {
  it('reads defaults when absent and parses values when present', () => {
    setup('/b?sel=7&done=show&open=3,4,3,x')
    expect(text('sel')).toBe('7')
    expect(text('hide')).toBe('false')
    expect(text('open')).toBe('3|4')
  })

  it('falls back to the default for unusable values', () => {
    setup('/b?sel=abc&done=maybe')
    expect(text('sel')).toBe('none')
    expect(text('hide')).toBe('true')
  })

  it('leaves out a value equal to its default and keeps other params', async () => {
    const user = userEvent.setup()
    setup('/b?focus=9&x=1')
    await user.click(screen.getByText('toggle'))
    expect(text('search')).toBe('?focus=9&x=1&done=show')
    await user.click(screen.getByText('toggle'))
    expect(text('search')).toBe('?focus=9&x=1')
  })

  it('replaces by default and pushes when asked', async () => {
    const user = userEvent.setup()
    const router = setup('/b')
    await user.click(screen.getByText('toggle'))
    expect(router.state.historyAction).toBe('REPLACE')
    await user.click(screen.getByText('select 5'))
    expect(router.state.historyAction).toBe('PUSH')
    expect(text('search')).toBe('?done=show&sel=5')
    await act(() => router.navigate(-1))
    expect(text('sel')).toBe('none')
  })

  it('two writes before a re-render both land', async () => {
    const user = userEvent.setup()
    setup('/b?keep=1')
    await user.click(screen.getByText('two writes'))
    expect(text('search')).toBe('?keep=1&a=1&b=2')
  })

  it('does not navigate when nothing changes', async () => {
    const user = userEvent.setup()
    const router = setup('/b')
    const before = router.state.location.key
    await user.click(screen.getByText('clear'))
    expect(router.state.location.key).toBe(before)
  })
})
