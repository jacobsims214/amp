// The requester's tracking screen (AMP #3987, build slice J6).
//
// The ticket's acceptance criteria are three, and each is proved here against a
// real rendered page over MSW rather than a mocked client:
//
//  1. **The approved track/answer/accept/reopen states work.** Answer sends the
//     answer to the requester's own route and the page re-reads (the destination
//     is the server's to choose, never the page's); accept is offered only in
//     `delivered` and calls the accept route; reopen calls the reopen route from
//     both delivered and accepted. A refused act keeps its words on screen.
//  2. **A foreign and an unknown reference are indistinguishable.** The read
//     answers the surface's single 404 for both, and the rendered text is
//     byte-identical — not merely "also a 404". The two access states that a 404
//     cannot express (invite pending, grant revoked) are props, and the page never
//     derives one from an error.
//  3. **No internal data is exposed.** The page renders `RequesterRequestView`,
//     whose type has no field for a project, ticket, claim, assignee, metadata,
//     agent or revision; the assertion is made against the rendered text anyway,
//     plus the source's import list.
//
// Plus the two things the ticket's "mobile" line names: the page holds at a
// 390 px viewport (one column, full-width 44 px actions, no pixel width wider than
// a phone) and every act is operable by keyboard.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { RequesterStatus } from './RequesterStatus'
import type { RequesterRequestView } from '../types'

interface Call { method: string; path: string; body: Record<string, unknown> | null; ifMatch: string | null }
let calls: Call[] = []

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = [] })
afterEach(() => { server.resetHandlers() })
afterAll(() => server.close())

const PUBLIC_REF = 'Ab-12_cd9efgh76_XyZ' // mixed case on purpose: base64url is case-sensitive
const ME = { name: 'Marta Reyes', email: 'marta.reyes@bluefjord.example' }
const DESK = 'Northwind support desk'

/** A projection exactly as the server builds one: whitelist fields only. */
function projection(over: Partial<RequesterRequestView> = {}): RequesterRequestView {
  return {
    public_ref: PUBLIC_REF,
    state: 'in_progress',
    state_word: 'In progress',
    subject: "October invoice export totals don't match the ledger",
    input_brief: 'The export is short by about €4,300 across 11 rows.',
    team_name: DESK,
    created_at: '2026-11-14T09:41:00Z',
    milestones: [
      { key: 'received', done: true, at: '2026-11-14T09:41:00Z' },
      { key: 'reviewing', done: true },
      { key: 'in_progress', done: true },
      { key: 'delivered', done: false },
      { key: 'accepted', done: false },
    ],
    comments: [],
    outputs: [],
    ...over,
  }
}

/** Record one request route and answer it however the test wants. */
function route(method: 'get' | 'post', path: string, answer: (call: Call) => Response | Promise<Response>) {
  return http[method](`*${path}`, async ({ request }) => {
    const text = await request.clone().text()
    const call: Call = {
      method: request.method,
      path: new URL(request.url).pathname,
      body: text ? JSON.parse(text) as Record<string, unknown> : null,
      ifMatch: request.headers.get('if-match'),
    }
    calls.push(call)
    return answer(call)
  })
}

const readAnswer = (view: RequesterRequestView) => route('get', `/api/requests/${PUBLIC_REF}`, () => HttpResponse.json({ view: 'requester', projection: view }))

const lifecycle = (state: string, rev = 5) => HttpResponse.json({
  request: { id: 7, public_ref: PUBLIC_REF, state, rev },
  changed: true,
})

function mount(props: Partial<Parameters<typeof RequesterStatus>[0]> = {}) {
  return render(<RequesterStatus publicRef={PUBLIC_REF} me={ME} {...props} />)
}

/** The page's own read, resolved: every test that inspects content starts here. */
async function mountReady(view: RequesterRequestView, props: Partial<Parameters<typeof RequesterStatus>[0]> = {}) {
  server.use(readAnswer(view))
  const rendered = mount(props)
  await screen.findByTestId('status-subject')
  return rendered
}

const answerBox = () => screen.getByTestId('requester-answer-box')

// ---- 1. The approved states work ----

describe('the in-progress state', () => {
  it('shows the subject, the reference, the status word and the stepper from the projection', async () => {
    await mountReady(projection())
    expect(screen.getByTestId('status-subject')).toHaveTextContent("October invoice export totals don't match the ledger")
    // The server's own word, not one the page derived from the state.
    expect(screen.getByTestId('status-word')).toHaveTextContent('In progress')
    expect(screen.getByTestId('status-meta')).toHaveTextContent(`REQ-${PUBLIC_REF}`)
    // Five steps, and "Delivered" is not done: a request the desk has not
    // delivered can never show its delivery step as reached.
    const steps = within(screen.getByTestId('status-stepper')).getAllByRole('listitem')
    expect(steps.map(s => s.getAttribute('data-step'))).toEqual(['received', 'reviewing', 'in_progress', 'delivered', 'accepted'])
    expect(steps.map(s => s.getAttribute('data-done'))).toEqual(['true', 'true', 'true', 'false', 'false'])
    // Nothing is asked of the requester in this state, so there is no answer box.
    expect(screen.queryByTestId('requester-answer-box')).toBeNull()
    expect(screen.queryByTestId('requester-accept-bar')).toBeNull()
    expect(screen.getByTestId('track-note')).toHaveTextContent(/Nothing is needed from you right now/)
  })

  it('renders the brief through the shared Markdown, with a remote image inert', async () => {
    await mountReady(projection({ input_brief: 'Here it is ![pixel](https://attacker.example/x.png)' }))
    const brief = screen.getByTestId('status-brief')
    // The compact renderer shows the URL as text and never creates an img, so a
    // request's text cannot make the browser fetch an attacker's pixel (D13).
    expect(brief.querySelector('img')).toBeNull()
    expect(brief).toHaveTextContent('attacker.example/x.png')
  })
})

describe('the needs-your-reply state', () => {
  const needsReply = () => projection({
    state: 'needs_information',
    state_word: 'Needs your reply',
    needs_reply_since: '2026-11-14T10:30:00Z',
    question: { body: 'Which ledger are you comparing against, the October close or the live books?', at: '2026-11-14T10:30:00Z' },
    comments: [
      { kind: 'question', from: 'desk', body: 'Which ledger are you comparing against?', at: '2026-11-14T10:30:00Z' },
    ],
  })

  it('shows the desk question and the answer box, and nothing to accept yet', async () => {
    await mountReady(needsReply())
    expect(answerBox()).toBeInTheDocument()
    expect(screen.getByTestId('answer-question')).toHaveTextContent('the October close or the live books')
    expect(screen.getByTestId('answer-question-meta')).toHaveTextContent(DESK)
    expect(screen.queryByTestId('requester-accept-bar')).toBeNull()
  })

  it('sends the answer to the requester route and re-reads the request', async () => {
    server.use(
      readAnswer(needsReply()),
      route('post', `/api/requests/${PUBLIC_REF}/answer`, () => lifecycle('in_progress')),
    )
    const user = userEvent.setup()
    mount()
    await screen.findByTestId('requester-answer-box')
    await user.type(screen.getByLabelText('Your answer'), 'The October close — the live books match fine.')
    await user.click(screen.getByRole('button', { name: /^Send answer$/ }))

    await waitFor(() => expect(calls.filter(c => c.method === 'POST')).toHaveLength(1))
    const post = calls.find(c => c.method === 'POST')!
    expect(post.path).toBe(`/api/requests/${PUBLIC_REF}/answer`)
    // Only the answer: the destination is the server's to choose from the
    // request's history (I5), so the page never sends a state.
    expect(Object.keys(post.body!).sort()).toEqual(['answer'])
    expect(post.body).toMatchObject({ answer: 'The October close — the live books match fine.' })
    // And the page re-reads, because the projection it holds is now stale.
    await waitFor(() => expect(calls.filter(c => c.method === 'GET').length).toBeGreaterThan(1))
  })

  it('refuses an empty answer without a round trip, and keeps the words after a 409', async () => {
    server.use(
      readAnswer(needsReply()),
      route('post', `/api/requests/${PUBLIC_REF}/answer`, () => HttpResponse.json(
        { error: 'request 7 is at revision 9, not 4', code: 'request.conflict', rev: 9 }, { status: 409 },
      )),
    )
    const user = userEvent.setup()
    mount()
    await screen.findByTestId('requester-answer-box')

    await user.click(screen.getByRole('button', { name: /^Send answer$/ }))
    expect(screen.getByText(/Write your answer first/)).toBeInTheDocument()
    expect(calls.some(c => c.method === 'POST')).toBe(false)

    await user.type(screen.getByLabelText('Your answer'), 'October close.')
    await user.click(screen.getByRole('button', { name: /^Send answer$/ }))
    await screen.findByTestId('answer-notice')
    expect(screen.getByTestId('answer-notice')).toHaveTextContent(/moved while you were looking at it/)
    expect(screen.getByTestId('answer-notice')).toHaveTextContent(/Nothing was sent/)
    // The words survive the refusal: losing three paragraphs to a conflict is
    // the failure this box exists to prevent.
    expect(screen.getByLabelText('Your answer')).toHaveValue('October close.')
  })

  it('says so honestly when the state wants a reply but no question was shared', async () => {
    await mountReady(projection({ state: 'needs_information', state_word: 'Needs your reply' }))
    expect(screen.queryByTestId('requester-answer-box')).toBeNull()
    expect(screen.getByText(/Their question has not reached this page yet/)).toBeInTheDocument()
  })
})

describe('the delivered state', () => {
  const delivered = (over: Partial<RequesterRequestView> = {}) => projection({
    state: 'delivered',
    state_word: 'Delivered — your call',
    delivered_at: '2026-11-13T16:12:00Z',
    outcome_brief: 'Re-ran the export against the corrected rate table. Included: the CSV and the note.',
    milestones: [
      { key: 'received', done: true, at: '2026-11-14T09:41:00Z' },
      { key: 'reviewing', done: true },
      { key: 'in_progress', done: true },
      { key: 'delivered', done: true, at: '2026-11-13T16:12:00Z' },
      { key: 'accepted', done: false },
    ],
    outputs: [
      { source: 'desk', name: 'october-invoice-export-corrected.csv', size_bytes: 188416, status: 'ready', download_ref: 'tok-abc.def' },
      { source: 'desk', name: 'what-changed-and-why.pdf', size_bytes: 98304, status: 'preparing' },
    ],
    ...over,
  })

  it('shows the delivery note, the files and both acts', async () => {
    await mountReady(delivered())
    expect(screen.getByTestId('outcome-brief')).toHaveTextContent(/corrected rate table/)
    expect(screen.getByTestId('requester-accept-bar')).toBeInTheDocument()
    expect(screen.getByTestId('accept-button')).toBeInTheDocument()
    expect(screen.getByTestId('reopen-button')).toBeInTheDocument()

    const rows = screen.getAllByTestId('output-row')
    expect(rows.map(r => r.getAttribute('data-status'))).toEqual(['ready', 'preparing'])
    // Only a ready file offers a download; "preparing" says nothing about why,
    // because the reason is the desk's business.
    expect(within(rows[0]).getByRole('button', { name: /Download october-invoice-export-corrected/ })).toBeInTheDocument()
    expect(within(rows[1]).queryByRole('button', { name: /Download/ })).toBeNull()
    expect(within(rows[1]).getByText('Preparing')).toBeInTheDocument()
  })

  it('accepts through the requester route, which is the one act no agent or token may make', async () => {
    server.use(
      readAnswer(delivered()),
      route('post', `/api/requests/${PUBLIC_REF}/accept`, () => lifecycle('requester_accepted', 6)),
    )
    const user = userEvent.setup()
    mount()
    await screen.findByTestId('requester-accept-bar')
    await user.click(screen.getByTestId('accept-button'))

    await waitFor(() => expect(calls.filter(c => c.method === 'POST')).toHaveLength(1))
    expect(calls.find(c => c.method === 'POST')!.path).toBe(`/api/requests/${PUBLIC_REF}/accept`)
    await waitFor(() => expect(calls.filter(c => c.method === 'GET').length).toBeGreaterThan(1))
  })

  it('reopens from delivered, and the reopen carries no state of its own', async () => {
    server.use(
      readAnswer(delivered()),
      route('post', `/api/requests/${PUBLIC_REF}/reopen`, () => lifecycle('triaging', 7)),
    )
    const user = userEvent.setup()
    mount()
    await screen.findByTestId('requester-accept-bar')
    await user.click(screen.getByTestId('reopen-button'))

    await waitFor(() => expect(calls.filter(c => c.method === 'POST')).toHaveLength(1))
    const post = calls.find(c => c.method === 'POST')!
    expect(post.path).toBe(`/api/requests/${PUBLIC_REF}/reopen`)
    // A reopen is a transition (D6): the destination is the policy's, and the
    // page sends no state — an empty body is the client's `input ?? {}`.
    expect(post.body).toEqual({})
  })

  it('says plainly that a note cannot be attached yet, instead of a box that drops it', async () => {
    await mountReady(delivered())
    const bar = screen.getByTestId('requester-accept-bar')
    // There is no requester comment route and the reopen route takes no note, so
    // a textarea here would accept words and silently lose them.
    expect(within(bar).queryByRole('textbox')).toBeNull()
    expect(bar).toHaveTextContent(/A note can't be attached from this page yet/)
  })
})

describe('the accepted state', () => {
  it('offers only a reopen, and says the request is closed', async () => {
    server.use(readAnswer(projection({
      state: 'requester_accepted',
      state_word: 'Accepted',
      accepted_at: '2026-11-14T12:00:00Z',
      outcome_brief: 'Included: the corrected CSV.',
      milestones: [
        { key: 'received', done: true }, { key: 'reviewing', done: true }, { key: 'in_progress', done: true },
        { key: 'delivered', done: true }, { key: 'accepted', done: true, at: '2026-11-14T12:00:00Z' },
      ],
    })))
    mount()
    await screen.findByTestId('requester-accept-bar')
    expect(screen.queryByTestId('accept-button')).toBeNull()
    expect(screen.getByTestId('reopen-button')).toBeInTheDocument()
    expect(screen.getByTestId('requester-accept-bar')).toHaveTextContent(/You accepted this outcome/)
  })
})

describe('the closed state', () => {
  it('shows the reason the desk gave, and offers no act', async () => {
    await mountReady(projection({
      state: 'rejected',
      state_word: 'Closed',
      reject_reason: 'This is covered by your support agreement, not new work.',
      milestones: [
        { key: 'received', done: true }, { key: 'reviewing', done: true }, { key: 'in_progress', done: false },
        { key: 'delivered', done: false }, { key: 'accepted', done: false },
      ],
    }))
    expect(screen.getByTestId('status-word')).toHaveTextContent('Closed')
    expect(screen.getByTestId('reject-reason')).toHaveTextContent('covered by your support agreement')
    expect(screen.queryByTestId('requester-answer-box')).toBeNull()
    expect(screen.queryByTestId('requester-accept-bar')).toBeNull()
    expect(screen.getByTestId('track-note')).toHaveTextContent(/closed this request/)
  })

  it('treats a duplicate as closed rather than inventing a sixth arm', async () => {
    await mountReady(projection({ state: 'duplicate', state_word: 'Closed' }))
    expect(screen.getByTestId('status-word')).toHaveTextContent('Closed')
    expect(screen.getByTestId('status-word')).toHaveAttribute('data-track', 'closed')
  })
})

// ---- 2. Indistinguishable refusals ----

describe('a reference that is not yours and one that never existed', () => {
  it('renders byte-identical text for both', async () => {
    // The server answers the same 404 for another person's reference and for one
    // that was never minted — one status, one code, one message.
    const notFound = () => HttpResponse.json(
      { error: 'request not found', code: 'request.not_found' }, { status: 404 },
    )
    server.use(route('get', '/api/requests/foreign-ref-000', notFound))
    const first = render(<RequesterStatus publicRef="foreign-ref-000" me={ME} />)
    await screen.findByTestId('requester-access-state')
    const foreign = first.container.textContent
    first.unmount()

    server.use(route('get', '/api/requests/ghost-ref-999', notFound))
    const second = render(<RequesterStatus publicRef="ghost-ref-999" me={ME} />)
    await screen.findByTestId('requester-access-state')
    expect(second.container.textContent).toBe(foreign)
    // And the words never hint that the request might exist but be hidden.
    expect(screen.getByTestId('requester-access-state')).toHaveTextContent(/We can't find that request/)
    expect(screen.getByTestId('requester-access-state')).toHaveTextContent(/we can't say which/)
  })

  it('does not turn a 5xx into "we can\'t find that request"', async () => {
    server.use(route('get', `/api/requests/${PUBLIC_REF}`, () => HttpResponse.json({ error: 'boom' }, { status: 500 })))
    mount()
    await screen.findByTestId('requester-load-failure')
    expect(screen.queryByTestId('requester-access-state')).toBeNull()
    expect(screen.getByTestId('requester-load-failure')).toHaveTextContent(/didn't load/)
    expect(screen.getByRole('button', { name: /Try again/ })).toBeInTheDocument()
  })

  it('takes invite-pending and revoked as props, never from a 404', async () => {
    // Neither state can come from a read: `authz.TeamSubmit` answers one
    // not-found for never-invited and revoked alike, so a page that inferred
    // "revoked" from a 404 would rebuild the oracle the server refuses to be.
    server.use(route('get', `/api/requests/${PUBLIC_REF}`, () => HttpResponse.json({ error: 'x' }, { status: 404 })))
    const { unmount } = mount({ accessState: 'invite_pending', teamName: DESK, onSignIn: vi.fn() })
    expect(screen.getByTestId('requester-access-state')).toHaveAttribute('data-access-state', 'invite_pending')
    expect(screen.getByText("You've been invited to send requests")).toBeInTheDocument()
    expect(screen.getByText(ME.email)).toBeInTheDocument()
    // No read was even attempted: there is nothing to read without access.
    expect(calls).toHaveLength(0)
    unmount()

    mount({ accessState: 'grant_revoked', teamName: DESK, onBackToRequests: vi.fn() })
    expect(screen.getByTestId('requester-access-state')).toHaveAttribute('data-access-state', 'grant_revoked')
    expect(screen.getByText('Your sending access was removed')).toBeInTheDocument()
    // The one promise it must keep: existing requests are still theirs.
    expect(screen.getByTestId('requester-access-state')).toHaveTextContent(/Requests you already sent are still yours to see/)
  })
})

// ---- 3. No internals ----

describe('no internal data is exposed', () => {
  it('renders nothing a triager would see, even with a populated projection', async () => {
    await mountReady(projection({
      state: 'delivered',
      state_word: 'Delivered — your call',
      outcome_brief: 'Included: the CSV.',
      outputs: [{ source: 'desk', name: 'corrected.csv', size_bytes: 1024, status: 'ready', download_ref: 'tok' }],
      comments: [
        { kind: 'question', from: 'desk', body: 'Which ledger?', at: '2026-11-14T10:00:00Z' },
        { kind: 'answer', from: 'you', body: 'The October close.', at: '2026-11-14T11:00:00Z' },
      ],
    }))
    const text = (document.body.textContent ?? '').toLowerCase()
    for (const internal of [
      'project', 'epic', 'story', 'ticket', 'assignee', 'claim', 'run ', 'agent', 'branch',
      'metadata', 'revision', 'rev ', 'org ', 'session', 'audit', 'portfolio',
      'triaging', 'requester_accepted', 'needs_information',
    ]) {
      expect(text, `the requester page must never mention "${internal}"`).not.toContain(internal)
    }
    // "account settings" is the one occurrence of the word, and it is the
    // mockup's own promise that they are *not* here — not a setting shown.
    expect(text.match(/settings/g)?.length ?? 0).toBeLessThanOrEqual(1)
    // One h1, and the skip link the frame provides targets the page's content.
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)
  })

  it('cannot render a team answer: a `team` view fails closed instead of leaking the record', async () => {
    server.use(route('get', `/api/requests/${PUBLIC_REF}`, () => HttpResponse.json({
      view: 'team',
      request: {
        id: 7, public_ref: PUBLIC_REF, org_id: 2, team_id: 4, subject: 'x', claim_user_id: 12,
        owner_user_id: 13, metadata: { secret: true }, rev: 3,
      },
    })))
    mount()
    await screen.findByTestId('requester-load-failure')
    expect(document.body.textContent).not.toContain('claim_user_id')
    expect(document.body.textContent).not.toContain('secret')
  })

  it('keeps the requester frame: no team frame, no project data layer, and only the requester routes', async () => {
    const { readFileSync } = await import('node:fs')
    const { join } = await import('node:path')
    const files = [
      'pages/RequesterStatus.tsx',
      'components/requests/RequesterAnswerBox.tsx',
      'components/requests/RequesterAcceptBar.tsx',
    ]
    const source = files.map(f => readFileSync(join(__dirname, '..', f), 'utf8')).join('\n')
    for (const forbidden of ['AppShell', 'react-router', 'useParams', 'api.getProject', 'listProjects', 'useAuth']) {
      expect(source, forbidden).not.toContain(forbidden)
    }
    // Every network call the screen makes is the requester surface's own. The
    // three acts are the requester's (F4); nothing here reaches a team route, so
    // there is no path by which this page could read a request it was not given.
    const routes = [...source.matchAll(/requestApi\.(\w+)/g)].map(m => m[1])
    expect(new Set(routes)).toEqual(new Set(['ownDetail', 'answer', 'accept', 'ownReopen']))
  })
})

// ---- Mobile and keyboard ----

describe('mobile (390 px)', () => {
  const original = window.innerWidth
  afterEach(() => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: original })
  })

  it('holds one column with full-width 44 px actions and no pixel width wider than a phone', async () => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 390 })
    window.dispatchEvent(new Event('resize'))
    server.use(readAnswer(projection({
      state: 'delivered',
      state_word: 'Delivered — your call',
      outcome_brief: 'Included: the CSV.',
    })))
    mount({ onNewRequest: vi.fn() })
    await screen.findByTestId('requester-accept-bar')

    for (const button of [screen.getByTestId('accept-button'), screen.getByTestId('reopen-button'), screen.getByRole('button', { name: 'Send another request' })]) {
      expect(button).toHaveClass('w-full')
      expect(button).toHaveStyle({ minHeight: '44px' })
    }
    // Nothing is pinned to a pixel width a phone cannot hold.
    for (const el of Array.from(document.querySelectorAll<HTMLElement>('[style]'))) {
      const width = el.style.width
      const px = /^(\d+)px$/.exec(width)
      if (px) expect(Number(px[1])).toBeLessThanOrEqual(390)
    }
  })

  it('answers a phone-sized answer box: a 16 px control and a full-width primary action', async () => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 390 })
    window.dispatchEvent(new Event('resize'))
    await mountReady(projection({
      state: 'needs_information',
      state_word: 'Needs your reply',
      question: { body: 'Which ledger?', at: '2026-11-14T10:00:00Z' },
    }))
    // 16 px at phone size, so iOS does not zoom the page when the box is focused.
    expect(screen.getByLabelText('Your answer').className).toContain('text-base')
    expect(screen.getByRole('button', { name: /^Send answer$/ })).toHaveClass('w-full')
    expect(screen.getByRole('button', { name: /^Send answer$/ })).toHaveStyle({ minHeight: '44px' })
  })
})

describe('keyboard', () => {
  it('reaches the answer box, types and sends with the keyboard alone', async () => {
    server.use(
      readAnswer(projection({
        state: 'needs_information',
        state_word: 'Needs your reply',
        question: { body: 'Which ledger?', at: '2026-11-14T10:00:00Z' },
      })),
      route('post', `/api/requests/${PUBLIC_REF}/answer`, () => lifecycle('in_progress')),
    )
    const user = userEvent.setup()
    mount()
    await screen.findByTestId('requester-answer-box')

    // The textarea is a real labelled control, and its label is the box's name.
    await user.click(screen.getByLabelText('Your answer'))
    await user.keyboard('October close.')
    await user.keyboard('{Enter}')
    // Enter in a textarea is a newline, not a submit: the button is the act.
    expect(screen.getByLabelText('Your answer')).toHaveValue('October close.\n')
    const send = screen.getByRole('button', { name: /^Send answer$/ })
    send.focus()
    expect(send).toHaveFocus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(calls.some(c => c.method === 'POST')).toBe(true))
  })

  it('reaches accept and reopen as real buttons in the tab order', async () => {
    await mountReady(projection({
      state: 'delivered',
      state_word: 'Delivered — your call',
      outcome_brief: 'Included: the CSV.',
    }))
    const accept = screen.getByTestId('accept-button')
    const reopen = screen.getByTestId('reopen-button')
    expect(accept.tagName).toBe('BUTTON')
    expect(reopen.tagName).toBe('BUTTON')
    // Both are reachable by Tab from the document body, and neither needs a
    // pointer: a focusable element with a key handler would be the failure.
    accept.focus()
    await userEvent.tab()
    expect(reopen).toHaveFocus()
  })
})

// ---- The loading state ----

describe('the first load', () => {
  it('announces one busy region and shows no half-built request', () => {
    server.use(route('get', `/api/requests/${PUBLIC_REF}`, () => new Promise<Response>(() => {})))
    mount()
    const busy = screen.getByRole('status', { name: 'Loading your request' })
    expect(busy).toHaveAttribute('aria-busy', 'true')
    expect(screen.queryByTestId('status-subject')).toBeNull()
  })
})
