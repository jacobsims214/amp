// The requester submit page (AMP #3985, build slice J4).
//
// The ticket's acceptance criteria are three, and each is proved here against a
// real rendered page over MSW rather than a mocked client:
//
//  1. **The approved form/success states match, and success shows the server's
//     reference.** The success screen prints the `public_ref` the *server*
//     answered with (not the subject or anything the page could derive), as
//     `REQ-…` with the token's case intact, and says what happens next.
//  2. **A failed response keeps every word, and retrying is safe.** Three
//     failure shapes — a dropped connection, a 500 and a 429 — each leave the
//     whole draft in the form, and the retry sends the *same*
//     `Idempotency-Key`, which is the fact that makes it a replay: the server's
//     idempotency scope is `(org, team, source, submitter)`, so no second request
//     can be created. A changed draft mints a new key.
//  3. **Permission and upload errors are explicit.** A 404 says the sign-in
//     isn't invited (never "not yours" — the surface refuses that distinction),
//     a 403 keeps or supplies a permission sentence, and the attachments section
//     says plainly that files cannot be sent yet rather than offering a picker
//     that could never upload.
//
// Plus the two things the ticket's "mobile" line names: the page holds at a
// 390 px viewport (one column, no sideways page scroll, the primary action full
// width and 44 px tall) and the form is operable by keyboard (labels, the error
// summary's links, Enter to send).
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { RequesterSubmit } from './RequesterSubmit'

interface Call { method: string; path: string; idempotencyKey: string | null; body: Record<string, unknown> | null }
let calls: Call[] = []

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = []; window.sessionStorage.clear() })
afterEach(() => { server.resetHandlers(); window.sessionStorage.clear() })
afterAll(() => server.close())

const TEAM_ID = 4
const TEAM_NAME = 'Northwind support desk'
const PUBLIC_REF = 'Ab-12_cd9efgh76_XyZ' // mixed case on purpose: base64url is case-sensitive
const ME = { name: 'Marta Reyes', email: 'marta.reyes@bluefjord.example' }

/** Record the submit route and answer it however the test wants. */
function submitRoute(answer: (call: Call) => Response | Promise<Response>) {
  return http.post('*/api/teams/4/requests', async ({ request }) => {
    const call: Call = {
      method: request.method,
      path: new URL(request.url).pathname,
      idempotencyKey: request.headers.get('idempotency-key'),
      body: await request.clone().json() as Record<string, unknown>,
    }
    calls.push(call)
    return answer(call)
  })
}

const created = () => HttpResponse.json({
  request: {
    public_ref: PUBLIC_REF,
    state: 'new',
    subject: 'October invoice export',
    team_id: TEAM_ID,
    source_kind: 'web',
    created_at: '2026-11-14T09:41:00Z',
  },
  created: true,
}, { status: 201 })

function mount(props: Partial<Parameters<typeof RequesterSubmit>[0]> = {}) {
  return render(
    <RequesterSubmit teamId={TEAM_ID} teamName={TEAM_NAME} me={ME} {...props} />,
  )
}

/** Fill the form the way a person would: subject, kind, description. */
async function fillForm(user: ReturnType<typeof userEvent.setup>, over: {
  subject?: string; brief?: string; kind?: string; outcome?: string
} = {}) {
  const subject = over.subject ?? 'October invoice export totals don\'t match the ledger'
  const brief = over.brief ?? 'The export is short by about €4,300 across 11 rows.'
  if (subject) await user.type(screen.getByLabelText('Subject', { exact: false }), subject)
  if (over.kind !== '') {
    await user.selectOptions(screen.getByLabelText('Kind of request'), over.kind ?? 'not_working')
  }
  if (brief) await user.type(screen.getByLabelText('What happened / what you need'), brief)
  if (over.outcome) await user.type(screen.getByLabelText('What "done" looks like for you', { exact: false }), over.outcome)
}

const sendButton = () => screen.getByRole('button', { name: /^Send request$/ })

describe('the approved form', () => {
  it('renders the mockup\'s sections, the contact taken from the sign-in, and no way to change it', () => {
    mount()
    expect(screen.getByRole('heading', { level: 1, name: 'New request' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'What you need' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Attachments' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Getting back to you' })).toBeInTheDocument()

    // Both contact inputs are read-only and hold the signed-in identity, so a
    // request can never claim to come from another address.
    const email = screen.getByLabelText('Email') as HTMLInputElement
    expect(email.value).toBe(ME.email)
    expect(email.readOnly).toBe(true)
    expect(email).toHaveAttribute('aria-readonly', 'true')
    const name = screen.getByLabelText('Your name') as HTMLInputElement
    expect(name.value).toBe(ME.name)
    expect(name.readOnly).toBe(true)
  })

  it('is the requester\'s frame only: no project, epic, ticket, claim or assignee anywhere', () => {
    mount()
    const text = (document.body.textContent ?? '').toLowerCase()
    for (const internal of ['project', 'epic', 'story', 'ticket', 'assignee', 'claim', 'run ', 'agent', 'branch']) {
      expect(text, `the requester page must never mention "${internal}"`).not.toContain(internal)
    }
    // One h1, and the skip link the frame provides targets the page's content.
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1)
  })

  it('offers the mockup\'s kind and urgency choices, and marks the pressed urgency', () => {
    mount()
    const kind = screen.getByLabelText('Kind of request') as HTMLSelectElement
    expect(Array.from(kind.options).map(o => o.textContent)).toEqual([
      'Choose…',
      "Something isn't working",
      'New work or a change',
      'A question about our agreement',
      'Access or an account',
    ])
    const scale = screen.getByRole('group', { name: 'How much is this blocking you?' })
    expect(within(scale).getAllByRole('button').map(b => b.textContent)).toEqual([
      'Not urgent', 'Slowing us down', 'Work stopped',
    ])
    expect(within(scale).getByRole('button', { name: 'Slowing us down' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('says files cannot be sent yet instead of offering a picker that could never upload', () => {
    mount()
    // The upload route is team-addressed by serial and the requester projection
    // carries neither, so no requester upload exists. A picker here would accept
    // files it could never send.
    expect(screen.queryByRole('button', { name: /browse/i })).toBeNull()
    const section = screen.getByRole('heading', { name: 'Attachments' }).closest('section')!
    expect(section).toHaveTextContent(/can’t be sent with a new request from this page yet/i)
    expect(section).toHaveTextContent(/put it in the description above/i)
  })
})

describe('sending', () => {
  it('sends exactly the route\'s three fields, on the team\'s path, with an idempotency key', async () => {
    server.use(submitRoute(created))
    const user = userEvent.setup()
    mount()
    await fillForm(user, { outcome: 'A corrected October CSV.' })
    await user.click(sendButton())

    await screen.findByRole('heading', { name: new RegExp(`Thanks — ${TEAM_NAME} has your request`) })
    expect(calls).toHaveLength(1)
    expect(calls[0].method).toBe('POST')
    expect(calls[0].path).toBe('/api/teams/4/requests')
    expect(calls[0].idempotencyKey).toMatch(/^[0-9a-f]{36}$/)

    const body = calls[0].body!
    expect(Object.keys(body).sort()).toEqual(['input_brief', 'metadata', 'subject'])
    expect(body).toMatchObject({
      subject: "October invoice export totals don't match the ledger",
      metadata: { request_kind: 'not_working', urgency: 'slowing_us_down' },
    })
    // The desired outcome rides inside the brief under its own heading, and the
    // team's delivery field is never sent.
    expect(body.input_brief).toContain('What "done" looks like:')
    expect(body.input_brief).toContain('A corrected October CSV.')
    expect(body).not.toHaveProperty('outcome_brief')
    // The contact is the server's fact, not a submitted one.
    expect(JSON.stringify(body)).not.toContain(ME.email)
  })

  it('shows the server\'s public reference as REQ-, case preserved, and what happens next', async () => {
    server.use(submitRoute(created))
    const user = userEvent.setup()
    mount()
    await fillForm(user)
    await user.click(sendButton())

    const ref = await screen.findByTestId('success-ref')
    // The server's token, not a serial and not something the page built.
    expect(ref).toHaveTextContent(`REQ-${PUBLIC_REF}`)
    expect(screen.getByRole('heading', { name: /Thanks — Northwind support desk has your request/ })).toBeInTheDocument()
    expect(screen.getByText('What happens next')).toBeInTheDocument()
    expect(screen.getByText(/Triage\./)).toBeInTheDocument()
    expect(screen.getByText(/Delivery\./)).toBeInTheDocument()
    // AMP sends no email, so the mockup's "we emailed a confirmation" must not
    // appear: it would be a promise nothing in the product keeps.
    expect(document.body.textContent).not.toMatch(/we emailed|emailed a confirmation/i)
    expect(document.body.textContent).toMatch(/Nothing about it was emailed to you/i)
  })

  it('forgets the retry token once the server answered, so the next send is a new request', async () => {
    server.use(submitRoute(created))
    const user = userEvent.setup()
    mount()
    await fillForm(user)
    await user.click(sendButton())
    await screen.findByTestId('success-ref')
    expect(window.sessionStorage.getItem('amp.requester-submit.4')).toBeNull()
  })

  it('copies the reference it displays, and says so when the clipboard refuses', async () => {
    server.use(submitRoute(created))
    // `userEvent.setup()` installs its own clipboard stub (the browser's is not
    // available in jsdom), so the assertion reads the stub's text back rather
    // than spying on a write that setup would have replaced.
    const user = userEvent.setup()
    mount()
    await fillForm(user)
    await user.click(sendButton())
    await screen.findByTestId('success-ref')

    await user.click(screen.getByRole('button', { name: 'Copy the reference' }))
    await screen.findByText('Reference copied.')
    expect(await navigator.clipboard.readText()).toBe(`REQ-${PUBLIC_REF}`)

    const deny = vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValueOnce(new Error('denied'))
    await user.click(screen.getByRole('button', { name: 'Copy the reference' }))
    await screen.findByText(/select the reference above and copy it/i)
    deny.mockRestore()
  })

  it('offers Track / All requests / Send another only when the page was given them', async () => {
    server.use(submitRoute(created))
    const user = userEvent.setup()
    const onOpenRequest = vi.fn()
    const onViewRequests = vi.fn()
    mount({ onOpenRequest, onViewRequests })
    await fillForm(user)
    await user.click(sendButton())
    await screen.findByTestId('success-ref')

    await user.click(screen.getByRole('button', { name: 'Track this request' }))
    expect(onOpenRequest).toHaveBeenCalledWith(PUBLIC_REF)
    await user.click(screen.getByRole('button', { name: 'All your requests' }))
    expect(onViewRequests).toHaveBeenCalled()
    // Not passed in this mount, so not rendered: no dead button.
    expect(screen.queryByRole('button', { name: 'Send another request' })).toBeNull()
  })
})

describe('validation', () => {
  it('refuses an empty form, links every error to its field, and sends nothing', async () => {
    server.use(submitRoute(created))
    const user = userEvent.setup()
    mount()
    await user.click(sendButton())

    const summary = await screen.findByTestId('submit-error-summary')
    expect(summary).toHaveAttribute('role', 'alert')
    expect(summary).toHaveTextContent('3 things to fix before sending')
    expect(summary).toHaveFocus()

    const links = within(summary).getAllByRole('button')
    expect(links).toHaveLength(3)
    // Each one is a link to its field, not a bare sentence.
    await user.click(links[1])
    expect(screen.getByLabelText('Kind of request')).toHaveFocus()

    // Each bad field is marked for assistive tech and described by its error.
    const subject = screen.getByLabelText('Subject', { exact: false })
    expect(subject).toHaveAttribute('aria-invalid', 'true')
    expect(subject).toHaveAccessibleDescription(/give your request a subject/i)
    expect(screen.getByLabelText('Kind of request')).toHaveAttribute('aria-invalid', 'true')

    expect(calls).toHaveLength(0)
  })

  it('keeps what was typed and clears a field\'s error as soon as it is fixed', async () => {
    server.use(submitRoute(created))
    const user = userEvent.setup()
    mount()
    // Subject and description typed, kind left unset: one error.
    await fillForm(user, { kind: '' })
    await user.click(sendButton())

    const summary = await screen.findByTestId('submit-error-summary')
    expect(summary).toHaveTextContent('1 thing to fix before sending')
    expect(summary).not.toHaveTextContent(/subject/i)

    // The words are still there after the refusal.
    expect(screen.getByLabelText('Subject', { exact: false })).toHaveValue('October invoice export totals don\'t match the ledger')
    expect(screen.getByLabelText('What happened / what you need'))
      .toHaveValue('The export is short by about €4,300 across 11 rows.')

    await user.selectOptions(screen.getByLabelText('Kind of request'), 'access')
    expect(screen.getByLabelText('Kind of request')).toHaveAttribute('aria-invalid', 'false')
    // Fixing the only bad field does not silently re-send; the next press does.
    expect(calls).toHaveLength(0)
  })
})

describe('a failed send', () => {
  const typedSubject = 'October invoice export totals don\'t match the ledger'
  const typedBrief = 'The export is short by about €4,300 across 11 rows.'

  async function sendAndFail(answer: Parameters<typeof submitRoute>[0]) {
    server.use(submitRoute(answer))
    const user = userEvent.setup()
    const page = mount()
    await fillForm(user)
    await user.click(sendButton())
    // Every failure keeps the form: the notice sits above it, and the fields
    // still hold what was typed.
    const notice = await screen.findByTestId('submit-failure')
    expect(screen.getByLabelText('Subject', { exact: false })).toHaveValue(typedSubject)
    expect(screen.getByLabelText('What happened / what you need')).toHaveValue(typedBrief)
    return { user, notice, page }
  }

  it('keeps every word after a dropped connection, and retries on the same key', async () => {
    let attempt = 0
    const { user, notice } = await sendAndFail(() => {
      attempt += 1
      if (attempt === 1) return HttpResponse.error()
      return created()
    })

    expect(notice).toHaveTextContent(/Sending didn't finish/i)
    expect(notice).toHaveTextContent(/Nothing you wrote was lost/i)
    expect(notice).toHaveTextContent(/cannot be sent twice/i)

    await user.click(within(notice).getByRole('button', { name: 'Try again' }))
    await screen.findByTestId('success-ref')

    expect(calls).toHaveLength(2)
    // THE point: a retry is a replay of the same submission, not a second one.
    expect(calls[1].idempotencyKey).toBe(calls[0].idempotencyKey)
    expect(calls[1].body).toEqual(calls[0].body)
  })

  it('survives a reload mid-request: the retry still carries the stored key', async () => {
    // A 503 leaves the draft in the page; the token is in sessionStorage, so a
    // re-mount (what a reload amounts to) replays rather than duplicating.
    let attempt = 0
    const answer = () => { attempt += 1; return attempt === 1 ? HttpResponse.json({ error: 'restarting' }, { status: 503 }) : created() }
    const first = await sendAndFail(answer)
    const firstKey = calls[0].idempotencyKey
    expect(firstKey).toMatch(/^[0-9a-f]{36}$/)
    // The reload: the page's state goes away entirely.
    first.page.unmount()

    // A fresh page has an empty draft but the same token in sessionStorage.
    mount()
    const u2 = userEvent.setup()
    await fillForm(u2)
    await u2.click(screen.getByRole('button', { name: /^Send request$/ }))
    await waitFor(() => expect(screen.getByTestId('success-ref')).toBeInTheDocument())
    expect(calls.at(-1)!.idempotencyKey).toBe(firstKey)
  })

  it('mints a new key when the draft changed, because the same key with a new payload is a 409', async () => {
    const { user } = await sendAndFail(() => HttpResponse.error())
    const firstKey = calls[0].idempotencyKey

    // Change one field, then retry through Send (not the notice's retry, which
    // re-sends the submitted draft as it was).
    const brief = screen.getByLabelText('What happened / what you need')
    await user.clear(brief)
    await user.type(brief, 'A different sentence, with the order number added.')
    server.use(submitRoute(created))
    await user.click(sendButton())
    await screen.findByTestId('success-ref')

    expect(calls.at(-1)!.idempotencyKey).not.toBe(firstKey)
  })

  it('carries a 429\'s wait and sends nothing', async () => {
    const { notice } = await sendAndFail(() =>
      HttpResponse.json({ error: 'too many requests', code: 'request.rate_limited', retry_after_s: 60 }, {
        status: 429, headers: { 'Retry-After': '60' },
      }))
    expect(notice).toHaveTextContent(/a lot of requests/i)
    expect(notice).toHaveTextContent(/Nothing was sent/i)
    expect(notice).toHaveTextContent(/60 seconds/)
    // It counts as one attempt only: a refusal wrote nothing.
    expect(calls).toHaveLength(1)
  })

  it('turns a 404 into "your sign-in can\'t send here", never "not yours"', async () => {
    const { notice } = await sendAndFail(() =>
      HttpResponse.json({ error: 'request not found', code: 'request.not_found' }, { status: 404 }))
    expect(notice).toHaveTextContent(/can't send to Northwind support desk/i)
    expect(notice).toHaveTextContent(/Nothing was sent/i)
    expect(notice).toHaveTextContent(/invited/i)
    // The API refuses to distinguish unknown from foreign; the page must not
    // invent the distinction it refused.
    expect(notice.textContent).not.toMatch(/not yours|belongs to another|does not exist/i)
  })

  it('states the permission refusal in a sentence, not a status word', async () => {
    const { notice } = await sendAndFail(() =>
      HttpResponse.json({ error: 'Forbidden' }, { status: 403 }))
    // The API client already replaces a bare "Forbidden" with a sentence; either
    // way the page never shows a bare status word.
    expect(notice.textContent).not.toMatch(/^forbidden$/i)
    expect(notice).toHaveTextContent(/permission|can't send requests/i)
  })

  it('keeps the server\'s reason on a coded 400, and says nothing was sent', async () => {
    const { notice } = await sendAndFail(() =>
      HttpResponse.json({ error: 'subject is over 200 characters (got 260)', code: 'request.invalid' }, { status: 400 }))
    expect(notice).toHaveTextContent('subject is over 200 characters')
    expect(notice).toHaveTextContent(/Nothing was sent/i)
  })

  it('dismisses the notice without losing the draft', async () => {
    const { user, notice } = await sendAndFail(() => HttpResponse.error())
    await user.click(within(notice).getByRole('button', { name: 'Keep editing' }))
    await waitFor(() => expect(screen.queryByTestId('submit-failure')).toBeNull())
    expect(screen.getByLabelText('Subject', { exact: false })).toHaveValue(typedSubject)
  })
})

describe('keyboard', () => {
  it('sends with Enter in a field, and every control is reachable and labelled', async () => {
    server.use(submitRoute(created))
    const user = userEvent.setup()
    mount()

    const subject = screen.getByLabelText('Subject', { exact: false })
    subject.focus()
    await user.type(subject, 'A keyboard-only request')
    await user.type(screen.getByLabelText('What happened / what you need'), 'Typed without touching the mouse.')
    await user.selectOptions(screen.getByLabelText('Kind of request'), 'access')
    // Enter inside a single-line input submits the form, like any HTML form (it
    // does not from a textarea, where Enter is a newline — which is why the
    // focus is put back in the subject first).
    subject.focus()
    await user.keyboard('{Enter}')

    await screen.findByTestId('success-ref')
    expect(calls).toHaveLength(1)
    expect(calls[0].body).toMatchObject({ subject: 'A keyboard-only request' })
  })

  it('moves the focus into the error summary so a screen reader hears it', async () => {
    const user = userEvent.setup()
    mount()
    await user.click(sendButton())
    await waitFor(() => expect(screen.getByTestId('submit-error-summary')).toHaveFocus())
  })
})

describe('mobile (390 px)', () => {
  const original = window.innerWidth
  afterEach(() => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: original })
  })

  it('holds one column, full-width 44 px actions, and no sideways overflow', async () => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 390 })
    window.dispatchEvent(new Event('resize'))
    mount()

    // The primary action is full width until `sm`, tall enough to tap, and
    // pinned to the bottom edge so it is reachable from the last field rather
    // than two screens below it.
    expect(sendButton()).toHaveClass('w-full')
    expect(sendButton()).toHaveStyle({ minHeight: '44px' })
    expect(sendButton().closest('.sticky')).not.toBeNull()
    expect(sendButton().closest('.sticky')!.className).toMatch(/sticky bottom-0/)
    // The two-column field pairs collapse: a grid that is one column below `sm`.
    expect(screen.getByLabelText('Kind of request').closest('.grid')).toHaveClass('sm:grid-cols-2')
    // Nothing is pinned to a pixel width wider than a phone.
    const form = document.querySelector('form')!
    expect(form.className).not.toMatch(/w-\[\d{3,}px\]/)
    // Inputs are 16 px at phone size, so iOS does not zoom on focus.
    expect(screen.getByLabelText('Subject', { exact: false }).className).toContain('text-base')
  })

  it('keeps the success screen single-column with a full-width primary action', async () => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 390 })
    window.dispatchEvent(new Event('resize'))
    server.use(submitRoute(created))
    const user = userEvent.setup()
    mount({ onOpenRequest: vi.fn() })
    await fillForm(user)
    await user.click(sendButton())

    const track = await screen.findByRole('button', { name: 'Track this request' })
    expect(track).toHaveClass('w-full')
    expect(track).toHaveStyle({ minHeight: '44px' })
    // The reference is on one line and selectable rather than clipped.
    expect(screen.getByTestId('success-ref').className).toContain('select-all')
  })
})
