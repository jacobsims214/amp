// RequesterAcceptBar (AMP #3987): accept or reopen, the two acts the lifecycle
// offers from `delivered`, and the one it offers from `requester_accepted`.
//
// The criteria this file proves:
//
//  1. **Accept is the requester's act, from `delivered` only.** The route is the
//     requester surface's, and the bar offers no accept from `requester_accepted`
//     — the transition table has no such row, so a button would only produce a
//     refusal.
//  2. **Reopen works from both states, and sends no state of its own.** A reopen
//     is a transition (D6), not a tenth state: the destination is the policy's.
//  3. **No note box.** The reopen route takes no note and the requester surface
//     has no comment route, so the bar says the note cannot be attached yet
//     rather than accepting words it would silently drop.
//  4. **A refusal is reported with the act's own verb and nothing is claimed.**
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { RequesterAcceptBar } from './RequesterAcceptBar'

interface Call { path: string; body: unknown; ifMatch: string | null }
let calls: Call[] = []

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = [] })
afterEach(() => { server.resetHandlers() })
afterAll(() => server.close())

function record(method: 'post', path: string, answer: () => Response) {
  return http[method](`*${path}`, async ({ request }) => {
    const text = await request.clone().text()
    calls.push({
      path: new URL(request.url).pathname,
      body: text ? JSON.parse(text) : undefined,
      ifMatch: request.headers.get('if-match'),
    })
    return answer()
  })
}

const accepted = () => HttpResponse.json({
  request: { id: 7, public_ref: 'ref-1', state: 'requester_accepted', rev: 10 },
  changed: true,
})
const reopened = () => HttpResponse.json({
  request: { id: 7, public_ref: 'ref-1', state: 'triaging', rev: 11 },
  changed: true,
})

function mount(props: Partial<Parameters<typeof RequesterAcceptBar>[0]> = {}) {
  return render(<RequesterAcceptBar publicRef="ref-1" state="delivered" {...props} />)
}

describe('the delivered state', () => {
  it('offers both acts, and says only the requester can close it', () => {
    mount()
    expect(screen.getByText('Does this answer your request?')).toBeInTheDocument()
    expect(screen.getByTestId('accept-button')).toHaveTextContent('Yes — accept the outcome')
    expect(screen.getByTestId('reopen-button')).toHaveTextContent("Something's missing — reopen")
    expect(screen.getByTestId('requester-accept-bar')).toHaveTextContent(/Only you can close this/)
  })

  it('accepts through the requester route, carrying the revision when the page has one', async () => {
    server.use(record('post', '/api/requests/ref-1/accept', accepted))
    const onChanged = vi.fn()
    const user = userEvent.setup()
    mount({ expectedRev: 9, onChanged })
    await user.click(screen.getByTestId('accept-button'))

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].path).toBe('/api/requests/ref-1/accept')
    // The accept body is a precondition and nothing else: no state, no on-behalf.
    expect(calls[0].body).toEqual({ expected_rev: 9 })
    expect(calls[0].ifMatch).toBe('"9"')
    await waitFor(() => expect(onChanged).toHaveBeenCalledWith('requester_accepted', 10))
  })

  it('accepts with no revision when the page never had one', async () => {
    server.use(record('post', '/api/requests/ref-1/accept', accepted))
    const user = userEvent.setup()
    mount()
    await user.click(screen.getByTestId('accept-button'))
    await waitFor(() => expect(calls).toHaveLength(1))
    // The route accepts an absent precondition ("whatever the row is now"), which
    // is what a page holding only the projection can honestly send.
    expect(calls[0].body).toEqual({})
    expect(calls[0].ifMatch).toBeNull()
  })

  it('reopens from delivered, sending no state of its own', async () => {
    server.use(record('post', '/api/requests/ref-1/reopen', reopened))
    const onChanged = vi.fn()
    const user = userEvent.setup()
    mount({ onChanged })
    await user.click(screen.getByTestId('reopen-button'))

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].path).toBe('/api/requests/ref-1/reopen')
    // A reopen is a transition (D6): the destination is the policy's answer, and
    // the bar names none.
    expect(calls[0].body).toEqual({})
    await waitFor(() => expect(onChanged).toHaveBeenCalledWith('triaging', 11))
  })
})

describe('the accepted state', () => {
  it('offers only a reopen: there is no acceptance row out of requester_accepted', () => {
    mount({ state: 'requester_accepted' })
    expect(screen.queryByTestId('accept-button')).toBeNull()
    expect(screen.getByTestId('reopen-button')).toBeInTheDocument()
    expect(screen.getByText('You accepted this outcome')).toBeInTheDocument()
  })

  it('reopens from accepted too', async () => {
    server.use(record('post', '/api/requests/ref-1/reopen', reopened))
    const user = userEvent.setup()
    mount({ state: 'requester_accepted' })
    await user.click(screen.getByTestId('reopen-button'))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].path).toBe('/api/requests/ref-1/reopen')
  })
})

describe('a refused act', () => {
  it('names the requester-only rule on a 403, and claims nothing was recorded', async () => {
    server.use(record('post', '/api/requests/ref-1/accept', () => HttpResponse.json(
      { error: 'a requester act is the requester\'s alone (I1)' }, { status: 403 },
    )))
    const onChanged = vi.fn()
    const user = userEvent.setup()
    mount({ onChanged })
    await user.click(screen.getByTestId('accept-button'))

    const notice = await screen.findByTestId('accept-notice')
    expect(notice).toHaveTextContent(/Only the request's own requester/)
    expect(notice).toHaveTextContent(/Nothing was recorded/)
    expect(onChanged).not.toHaveBeenCalled()
  })

  it('reports a conflict on a reopen and tells the person to reload', async () => {
    server.use(record('post', '/api/requests/ref-1/reopen', () => HttpResponse.json(
      { error: 'request 7 is at revision 12', code: 'request.conflict', rev: 12 }, { status: 409 },
    )))
    const user = userEvent.setup()
    mount()
    await user.click(screen.getByTestId('reopen-button'))
    const notice = await screen.findByTestId('accept-notice')
    expect(notice).toHaveTextContent(/moved while you were looking at it/)
    expect(notice).toHaveTextContent(/Reload the page/)
    // The verb is the reopen's, so it never claims an answer was sent.
    expect(notice).toHaveTextContent(/Nothing was recorded/)
  })
})

describe('the missing note', () => {
  it('offers no note box and says why, instead of accepting words it would drop', () => {
    mount()
    const bar = screen.getByTestId('requester-accept-bar')
    // `POST /api/requests/{ref}/reopen` takes a precondition only, and the
    // requester surface has no comment route, so a textarea here would be a
    // field whose contents nothing ever reads.
    expect(within(bar).queryByRole('textbox')).toBeNull()
    expect(bar).toHaveTextContent(/A note can't be attached from this page yet/)
    expect(bar).toHaveTextContent(/they will ask you what is missing/)
  })
})

describe('keyboard and mobile', () => {
  it('has both acts as real buttons in the tab order, full width and 44 px at a phone size', async () => {
    mount()
    const accept = screen.getByTestId('accept-button')
    const reopen = screen.getByTestId('reopen-button')
    for (const button of [accept, reopen]) {
      expect(button.tagName).toBe('BUTTON')
      expect(button).toHaveClass('w-full')
      expect(button).toHaveClass('sm:w-auto')
      expect(button).toHaveStyle({ minHeight: '44px' })
    }
    accept.focus()
    await userEvent.tab()
    expect(reopen).toHaveFocus()
  })

  it('activates each act from the keyboard', async () => {
    server.use(
      record('post', '/api/requests/ref-1/accept', accepted),
      record('post', '/api/requests/ref-1/reopen', reopened),
    )
    const user = userEvent.setup()
    mount()
    screen.getByTestId('accept-button').focus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(calls.map(c => c.path)).toEqual(['/api/requests/ref-1/accept']))
    screen.getByTestId('reopen-button').focus()
    await user.keyboard(' ')
    await waitFor(() => expect(calls.map(c => c.path)).toEqual(['/api/requests/ref-1/accept', '/api/requests/ref-1/reopen']))
  })

  it('disables both while one act is in flight, so the second cannot race it', async () => {
    let release: (() => void) | undefined
    server.use(http.post('*/api/requests/:ref/accept', async () => {
      await new Promise<void>(resolve => { release = resolve })
      return accepted()
    }))
    const user = userEvent.setup()
    mount()
    await user.click(screen.getByTestId('accept-button'))
    const accepting = await screen.findByRole('button', { name: /^Accepting…$/ })
    expect(accepting).toBeDisabled()
    expect(screen.getByTestId('reopen-button')).toBeDisabled()
    release?.()
    await waitFor(() => expect(screen.getByTestId('accept-button')).toBeEnabled())
  })
})
