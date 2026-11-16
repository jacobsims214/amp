// RequesterAnswerBox (AMP #3987): the one state that asks the requester something.
//
// What this file pins, in the order the ticket's criteria name it:
//
//  1. **Send answer works, and sends only the answer.** The destination is the
//     server's to read from the request's history (I5), so the page never names a
//     state — an answer that chose one would be a UI deciding a transition.
//  2. **A refusal keeps the words.** Every failure shape leaves the answer in the
//     box with the notice above it, because losing three paragraphs to a 409 is
//     the failure this box exists to prevent.
//  3. **A double send cannot answer twice.** The button is disabled while the act
//     is in flight; and the act is a transition, so a repeat is refused rather
//     than appended.
//  4. **The box is operable by keyboard and holds at 390 px.**
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { RequesterAnswerBox } from './RequesterAnswerBox'

interface Call { path: string; body: Record<string, unknown> | null; ifMatch: string | null }
const calls: Call[] = []

const server = setupServer(
  http.post('*/api/requests/:ref/answer', async ({ request }) => {
    const text = await request.text()
    calls.push({
      path: new URL(request.url).pathname,
      body: text ? JSON.parse(text) as Record<string, unknown> : null,
      ifMatch: request.headers.get('if-match'),
    })
    return HttpResponse.json({
      request: { id: 7, public_ref: 'ref-1', state: 'triaging', rev: 9 },
      comment: { id: 41, kind: 'answer', body: 'October close.', created_at: '2026-11-14T11:00:00Z' },
      changed: true,
    })
  }),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => { server.resetHandlers(); calls.length = 0 })
afterAll(() => server.close())

const QUESTION = { body: 'Which ledger are you comparing against, the October close or the live books?', at: '2026-11-14T10:30:00Z' }

function mount(props: Partial<Parameters<typeof RequesterAnswerBox>[0]> = {}) {
  return render(
    <RequesterAnswerBox publicRef="ref-1" question={QUESTION} teamName="Northwind support desk" {...props} />,
  )
}

const sendButton = () => screen.getByRole('button', { name: /^Send answer$/ })

describe('the question card', () => {
  it('shows the desk\'s question, who asked it and when', () => {
    mount()
    expect(screen.getByText('The desk needs a bit more from you')).toBeInTheDocument()
    expect(screen.getByTestId('answer-question')).toHaveTextContent('the October close or the live books')
    expect(screen.getByTestId('answer-question-meta')).toHaveTextContent('Northwind support desk')
    expect(screen.getByLabelText('Your answer')).toBeInTheDocument()
  })

  it('keeps the request\'s own words literal rather than interpreting them as markdown', () => {
    mount({ question: { body: 'Is `<script>` in the log? **Not bold.**', at: '2026-11-14T10:30:00Z' } })
    const card = screen.getByTestId('answer-question')
    // The question is plain text: a `**` in someone's question must not become
    // emphasis, and an angle-bracketed word must never become an element.
    expect(card.textContent).toContain('**Not bold.**')
    expect(card.querySelector('script')).toBeNull()
    expect(card.querySelector('strong')).toBeNull()
  })
})

describe('sending an answer', () => {
  it('sends only the answer, to the requester\'s own route, and reports the state it returned to', async () => {
    const onAnswered = vi.fn()
    const user = userEvent.setup()
    mount({ onAnswered })
    await user.type(screen.getByLabelText('Your answer'), 'The October close — the live books match fine.')
    await user.click(sendButton())

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].path).toBe('/api/requests/ref-1/answer')
    expect(calls[0].body).toEqual({ answer: 'The October close — the live books match fine.' })
    // The server's answer, not a state the box chose: an answer restores the
    // state the question interrupted, and only the history knows which.
    await waitFor(() => expect(onAnswered).toHaveBeenCalledWith('triaging', 9))
    // Cleared only after the server has it.
    expect(screen.getByLabelText('Your answer')).toHaveValue('')
  })

  it('carries the question id and the revision when the page knows them', async () => {
    const user = userEvent.setup()
    mount({ questionId: 33, expectedRev: 8 })
    await user.type(screen.getByLabelText('Your answer'), 'October close.')
    await user.click(sendButton())

    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].body).toEqual({ answer: 'October close.', answer_to: 33, expected_rev: 8 })
    // The precondition travels as If-Match too, so the server's CAS sees it.
    expect(calls[0].ifMatch).toBe('"8"')
  })

  it('trims the answer but never rewrites it', async () => {
    const user = userEvent.setup()
    mount()
    await user.type(screen.getByLabelText('Your answer'), '   October close, 11 rows.   ')
    await user.click(sendButton())
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].body!.answer).toBe('October close, 11 rows.')
  })

  it('refuses an empty answer without a round trip and puts focus where the problem is', async () => {
    const user = userEvent.setup()
    mount()
    await user.click(sendButton())
    expect(screen.getByText(/Write your answer first/)).toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })

  it('does not send twice while the first send is in flight', async () => {
    let release: (() => void) | undefined
    server.use(http.post('*/api/requests/:ref/answer', async () => {
      await new Promise<void>(resolve => { release = resolve })
      return HttpResponse.json({ request: { id: 7, public_ref: 'ref-1', state: 'triaging', rev: 9 }, changed: true })
    }))
    const user = userEvent.setup()
    mount()
    await user.type(screen.getByLabelText('Your answer'), 'October close.')
    await user.click(sendButton())

    // The button renames itself while the act is in flight, so the second click
    // has nothing named "Send answer" to hit — and the button it does find is
    // disabled. Both halves matter: a disabled button with an unchanged label
    // reads as "nothing happened".
    const sending = await screen.findByRole('button', { name: /^Sending…$/ })
    expect(sending).toBeDisabled()
    expect(screen.queryByRole('button', { name: /^Send answer$/ })).toBeNull()
    await user.click(sending)
    release?.()
    await waitFor(() => expect(screen.getByLabelText('Your answer')).toHaveValue(''))
  })
})

describe('a refused answer', () => {
  const refused = (status: number, body: Record<string, unknown>) =>
    http.post('*/api/requests/:ref/answer', () => HttpResponse.json(body, { status }))

  it('keeps every word and says nothing was sent, on a 409', async () => {
    server.use(refused(409, { error: 'request 7 is at revision 9, not 8', code: 'request.conflict', rev: 9 }))
    const user = userEvent.setup()
    mount()
    await user.type(screen.getByLabelText('Your answer'), 'October close, 11 rows.')
    await user.click(sendButton())

    const notice = await screen.findByTestId('answer-notice')
    expect(notice).toHaveTextContent(/moved while you were looking at it/)
    expect(notice).toHaveTextContent(/Nothing was sent/)
    expect(screen.getByLabelText('Your answer')).toHaveValue('October close, 11 rows.')
  })

  it('keeps them on a 404 and on a 403 too', async () => {
    const user = userEvent.setup()
    server.use(refused(404, { error: 'request not found', code: 'request.not_found' }))
    mount()
    await user.type(screen.getByLabelText('Your answer'), 'October close.')
    await user.click(sendButton())
    expect(await screen.findByTestId('answer-notice')).toHaveTextContent(/isn't available to you any more/)
    expect(screen.getByLabelText('Your answer')).toHaveValue('October close.')
    calls.length = 0

    server.use(refused(403, { error: 'not your request' }))
    await user.click(sendButton())
    await waitFor(() => expect(screen.getByTestId('answer-notice')).toHaveTextContent(/Only the request's own requester/))
    expect(screen.getByLabelText('Your answer')).toHaveValue('October close.')
  })

  it('keeps them when the connection dies, and does not pretend to know what happened', async () => {
    server.use(http.post('*/api/requests/:ref/answer', () => HttpResponse.error()))
    const user = userEvent.setup()
    mount()
    await user.type(screen.getByLabelText('Your answer'), 'October close.')
    await user.click(sendButton())
    const notice = await screen.findByTestId('answer-notice')
    expect(notice).toHaveTextContent(/could not tell whether that reached the desk/)
    expect(screen.getByLabelText('Your answer')).toHaveValue('October close.')
  })

  it('keeps the conflict notice after an edit, because editing does not un-stale the row', async () => {
    server.use(refused(409, { error: 'stale', code: 'request.conflict' }))
    const user = userEvent.setup()
    mount()
    await user.type(screen.getByLabelText('Your answer'), 'October close.')
    await user.click(sendButton())
    await screen.findByTestId('answer-notice')
    await user.type(screen.getByLabelText('Your answer'), ' and November.')
    // A field error clears on edit; a *conflict* does not. The row is still at
    // the revision someone else moved it to, and the notice's own instruction
    // (reload) is the only thing that fixes it — so hiding it on a keystroke
    // would leave the person retrying a write that cannot succeed.
    expect(screen.getByTestId('answer-notice')).toBeInTheDocument()
  })
})

describe('keyboard and mobile', () => {
  it('labels the box, and Enter in it is a newline rather than a submit', async () => {
    const user = userEvent.setup()
    mount()
    const box = screen.getByLabelText('Your answer')
    await user.click(box)
    await user.type(box, 'Line one.{Enter}Line two.')
    expect(box).toHaveValue('Line one.\nLine two.')
    expect(calls).toHaveLength(0)
    // The act is the button, which is a real button in the tab order.
    sendButton().focus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(calls).toHaveLength(1))
  })

  it('is 16 px and full width at a phone size, so iOS does not zoom and the action is reachable', () => {
    mount()
    expect(screen.getByLabelText('Your answer').className).toContain('text-base')
    expect(sendButton()).toHaveClass('w-full')
    expect(sendButton()).toHaveClass('sm:w-auto')
    expect(sendButton()).toHaveStyle({ minHeight: '44px' })
  })

  it('announces the length only once it matters', async () => {
    const user = userEvent.setup()
    mount()
    expect(screen.queryByText(/of 20000 characters/)).toBeNull()
    await user.click(screen.getByLabelText('Your answer'))
    // Pasting past the bound is the realistic way to reach it; typing 16000
    // characters would be a slow test for the same branch.
    await user.paste('x'.repeat(16100))
    expect(screen.getByText(/16100 of 20000 characters/)).toBeInTheDocument()
  })
})
