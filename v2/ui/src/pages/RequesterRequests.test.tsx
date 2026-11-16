// The requester's own-requests list (AMP #3986, build slice J5).
//
// The ticket's three acceptance criteria, one `describe` each, proven by a real
// rendered page over MSW rather than a mocked component:
//
//  1. **Own-only list and the approved states render.** Only the caller's rows
//     arrive — the page reads `GET /api/requests`, which the server scopes to the
//     caller's requester column in SQL, and the page applies no filter of its own
//     — and the mockup's list, loading and empty states each render with the
//     mockup's own words.
//  2. **Paging and error behaviour are tested.** A `next_before_id` asks for the
//     rows below it and appends them (never re-fetches the first page, never
//     shifts a row under the reader); a failed load says so and Retry recovers; a
//     filtered empty says "clear the filter" rather than "nothing here yet".
//  3. **No project or internal fields.** The fixture carries the internal fields
//     a team-side record has, under the keys a careless page would read, and the
//     rendered tree is searched for every one of them.
//
// Plus the 390 px rule the ticket's "no routing/chrome edits" neighbours set: the
// pill strip scrolls rather than overflowing the page, and every control is
// keyboard-reachable.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { RequesterRequests } from './RequesterRequests'
import type { OwnRequestPage, RequesterRequestView } from '../types'

interface Call { method: string; path: string; search: string }
let calls: Call[] = []

/** Record the call and answer it with `answer`. */
function route(pattern: string, answer: unknown = {}, status = 200) {
  return http.get(pattern, async ({ request }) => {
    calls.push({ method: request.method, path: new URL(request.url).pathname, search: new URL(request.url).search })
    return HttpResponse.json(answer as Record<string, unknown>, { status })
  })
}

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { calls = [] })
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

/** The desk's public name — a prop, never a fetch (a requester cannot read a team route). */
const TEAM = 'Northwind support desk'
const ME = { name: 'Marta Reyes', email: 'marta.reyes@bluefjord.example' }

/** One row of the own list. Only whitelisted keys; a test adds internals on purpose. */
function row(over: Partial<RequesterRequestView> = {}): RequesterRequestView {
  return {
    public_ref: '8f3k2xq7',
    state: 'in_progress',
    state_word: 'In progress',
    subject: "October invoice export totals don't match the ledger",
    input_brief: 'The export is short by about €4,300 across 11 rows.',
    team_name: TEAM,
    created_at: '2026-11-04T14:00:00Z',
    milestones: [],
    comments: [],
    outputs: [],
    ...over,
  }
}

/** A page answer around `items`; `total` is the server's cursor-free number. */
function page(items: RequesterRequestView[], over: Partial<OwnRequestPage> = {}): OwnRequestPage {
  return { items, count: items.length, total: items.length, ...over }
}

const mount = (props: Partial<React.ComponentProps<typeof RequesterRequests>> = {}) =>
  render(<RequesterRequests teamName={TEAM} me={ME} {...props} />)

/**
 * Wait for the list to settle: a rendered list or one of the two empty states.
 * Not "the loader is gone" — the skeleton is deliberately delayed by 250 ms, so a
 * missing loader is true for a moment before any data has arrived.
 */
const settled = async () => {
  await screen.findByRole('heading', { name: 'Your requests' })
  await waitFor(() => {
    const done = screen.queryByRole('list', { name: 'Your requests' })
      ?? screen.queryByRole('heading', { name: 'Nothing here yet' })
      ?? screen.queryByRole('heading', { name: /None of your requests are/ })
    expect(done).not.toBeNull()
  })
}

describe('own-only list and the approved states', () => {
  it('lists the caller’s own requests from the own-list route, and nothing else', async () => {
    const rows = [
      row({ public_ref: 'reqAAAA1', state: 'needs_information', state_word: 'Needs your reply', subject: 'October invoice export totals don’t match the ledger', needs_reply_since: new Date(Date.now() - 2 * 3_600_000).toISOString() }),
      row({ public_ref: 'reqBBBB2', state: 'delivered', state_word: 'Delivered — your call', delivered_at: new Date(Date.now() - 3 * 86_400_000).toISOString(), subject: 'Add a second approver to our onboarding checklist' }),
      row({ public_ref: 'reqCCCC3', state: 'in_progress', state_word: 'In progress', subject: 'SSO login loops back to the sign-in page on Safari' }),
      row({ public_ref: 'reqDDDD4', state: 'requester_accepted', state_word: 'Accepted', accepted_at: '2026-10-21T10:00:00Z', closed_at: '2026-10-21T10:00:00Z', subject: 'Rename our workspace' }),
    ]
    server.use(route('*/api/requests', page(rows, { total: 4 })))
    mount()
    await settled()

    const list = screen.getByRole('list', { name: 'Your requests' })
    const items = within(list).getAllByRole('listitem')
    expect(items).toHaveLength(4)

    // The route the page reads is the requester's own list, never a team queue.
    const listCalls = calls.filter(c => c.path === '/api/requests' && !c.search.includes('limit=1'))
    expect(listCalls.length).toBeGreaterThan(0)
    expect(calls.some(c => c.path.startsWith('/api/teams/'))).toBe(false)

    // Every row shows the server's state word, the reference, and its own line.
    expect(within(items[0]).getByText('Needs your reply')).toBeInTheDocument()
    expect(within(items[0]).getByText('REQ-reqAAAA1', { exact: false })).toBeInTheDocument()
    expect(within(items[0]).getByText(/The desk asked you a question 2h ago/)).toBeInTheDocument()
    expect(within(items[1]).getByText('Delivered — your call')).toBeInTheDocument()
    expect(within(items[1]).getByText(/Delivered 3d ago — check the outcome/)).toBeInTheDocument()
    expect(within(items[2]).getByText('In progress')).toBeInTheDocument()
    expect(within(items[3]).getByText('Accepted')).toBeInTheDocument()
    expect(within(items[3]).getByText(/You accepted the outcome on Oct 21/)).toBeInTheDocument()

    // The header states the rule plainly, and names the address it is scoped to.
    expect(screen.getByText(/You see only your own requests/)).toBeInTheDocument()
    // The address it is scoped to appears in the intro; the frame's identity
    // chip carries it too, which is why this is not a single-element query.
    expect(screen.getAllByText(new RegExp(ME.email.replace(/\./g, '\\.'))).length).toBeGreaterThan(0)
    // The count is the server's total.
    expect(screen.getByText('4 requests')).toBeInTheDocument()
  })

  it('filters by pill through the server, not over the rows it happens to hold', async () => {
    // The page's job here is to ask the server the right question; a client-side
    // filter over one loaded page could not describe a filter's whole set.
    server.use(
      route('*/api/requests', page([row()], { total: 1 })),
    )
    mount()
    await settled()
    calls = []

    await userEvent.click(screen.getByRole('button', { name: /Needs your reply/ }))
    await waitFor(() => expect(calls.some(c => c.search.includes('state=needs_information'))).toBe(true))
    const call = calls.find(c => c.search.includes('state=needs_information'))!
    expect(call.path).toBe('/api/requests')
    // …and the previous pill's rows do not linger under the new pill.
    await waitFor(() => expect(screen.getByRole('button', { name: /Needs your reply/ })).toHaveAttribute('aria-pressed', 'true'))
  })

  it('renders the loading state with the mockup’s shape, then the list', async () => {
    // A load that never settles: the skeleton is what the page shows while it
    // waits, and it is announced rather than being a silent pile of boxes.
    let release!: () => void
    const held = new Promise<void>(res => { release = res })
    server.use(http.get('*/api/requests', async ({ request }) => {
      const search = new URL(request.url).search
      calls.push({ method: 'GET', path: new URL(request.url).pathname, search })
      await held
      return HttpResponse.json(page([row()]) as unknown as Record<string, unknown>)
    }))

    mount()
    // The indicator waits ~250 ms (useDelayedFlag), so it appears on a slow load
    // and never flashes on a fast one.
    const loading = await screen.findByLabelText('Loading your requests', {}, { timeout: 3000 })
    expect(loading).toHaveAttribute('aria-busy', 'true')
    expect(within(loading).getByText('Loading your requests…')).toBeInTheDocument()
    release()
    await screen.findByRole('list', { name: 'Your requests' })
    expect(screen.queryByLabelText('Loading your requests')).toBeNull()
  })

  it('offers "send your first request" only when the person has sent nothing at all', async () => {
    server.use(route('*/api/requests', page([], { total: 0 })))
    const onNewRequest = () => { opened++ }
    let opened = 0
    mount({ onNewRequest })
    await settled()

    expect(screen.getByRole('heading', { name: 'Nothing here yet' })).toBeInTheDocument()
    expect(screen.getByText(/Requests you send to Northwind support desk show up here/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Send your first request' }))
    expect(opened).toBe(1)
  })

  it('opens a request by its public reference — the only address a requester holds', async () => {
    server.use(route('*/api/requests', page([row({ public_ref: 'AbC-123_xYz' })])))
    const seen: string[] = []
    mount({ onOpenRequest: ref => seen.push(ref) })
    await settled()

    await userEvent.click(screen.getByRole('button', { name: /invoice export/ }))
    // Exactly the server's token, case preserved: the status screen resolves it
    // with an equality, so a folded reference would be a 404.
    expect(seen).toEqual(['AbC-123_xYz'])
  })
})

describe('paging', () => {
  it('pages with the keyset cursor and appends, never re-reading the first page', async () => {
    const first = [row({ public_ref: 'reqPAGE1', subject: 'First page row' })]
    const older = [row({ public_ref: 'reqPAGE2', subject: 'Older row from the second page' })]
    server.use(http.get('*/api/requests', ({ request }) => {
      const search = new URL(request.url).search
      calls.push({ method: 'GET', path: new URL(request.url).pathname, search })
      const before = new URL(request.url).searchParams.get('before_id')
      if (before) {
        return HttpResponse.json(page(older, { total: 2 }) as unknown as Record<string, unknown>)
      }
      return HttpResponse.json(page(first, { total: 2, next_before_id: 41 }) as unknown as Record<string, unknown>)
    }))
    mount()
    await settled()

    // The cursor is the last row's id, and the page asks for the default page.
    const paging = screen.getByRole('button', { name: /Older requests/ })
    calls = []
    await userEvent.click(paging)

    await screen.findByText('Older row from the second page')
    const asked = calls.filter(c => c.path === '/api/requests' && !c.search.includes('limit=1'))
    expect(asked).toHaveLength(1)
    expect(asked[0].search).toContain('before_id=41')
    // Both pages are on screen: appending, not replacing.
    expect(screen.getByText('First page row')).toBeInTheDocument()
    expect(screen.getByText('Older row from the second page')).toBeInTheDocument()
    // The server says there is no further page, so no control offers one.
    expect(screen.queryByRole('button', { name: /Older requests/ })).toBeNull()
  })

  it('shows how many of the total are on screen while a filter has more', async () => {
    server.use(route('*/api/requests', page([row()], { total: 12, next_before_id: 7 })))
    mount()
    await settled()
    expect(screen.getByText('showing 1 of 12')).toBeInTheDocument()
  })
})

describe('error and empty behaviour', () => {
  it('says the list did not load and recovers on Retry', async () => {
    let fail = true
    server.use(http.get('*/api/requests', ({ request }) => {
      const search = new URL(request.url).search
      if (search.includes('limit=1')) return HttpResponse.json(page([], { total: 0 }) as unknown as Record<string, unknown>)
      return fail
        ? HttpResponse.json({ error: 'The request list did not come back.' }, { status: 503 })
        : HttpResponse.json(page([row()], { total: 1 }) as unknown as Record<string, unknown>)
    }))
    mount()
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/did not come back/))

    fail = false
    await userEvent.click(within(screen.getByRole('alert')).getByRole('button', { name: /Retry/ }))
    await screen.findByRole('list', { name: 'Your requests' })
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('distinguishes "no matches for this filter" from "you have sent nothing"', async () => {
    // An account with rows, filtered to a pill that holds none of them.
    server.use(http.get('*/api/requests', ({ request }) => {
      const search = new URL(request.url).search
      const filtered = search.includes('state=')
      const items = filtered ? [] : [row()]
      return HttpResponse.json(page(items, { total: items.length }) as unknown as Record<string, unknown>)
    }))
    mount()
    await screen.findByRole('list', { name: 'Your requests' })

    await userEvent.click(screen.getByRole('button', { name: /Delivered/ }))
    await screen.findByRole('heading', { name: /None of your requests are delivered/ })
    // Not the invitation copy: nothing is wrong with the account.
    expect(screen.queryByRole('heading', { name: 'Nothing here yet' })).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: /Clear the filter/ }))
    await screen.findByRole('list', { name: 'Your requests' })
    expect(screen.getByRole('button', { name: /^All/ })).toHaveAttribute('aria-pressed', 'true')
  })

  it('leaves a pill without a number rather than inventing a zero when its count fails', async () => {
    // The count reads are best-effort: a failed one must not report "0 delivered"
    // for a filter that might hold something.
    server.use(http.get('*/api/requests', ({ request }) => {
      const search = new URL(request.url).search
      if (search.includes('limit=1') && search.includes('delivered')) {
        return HttpResponse.json({ error: 'nope' }, { status: 503 })
      }
      return HttpResponse.json(page([row()], { total: 1 }) as unknown as Record<string, unknown>)
    }))
    mount()
    await settled()
    const delivered = screen.getByRole('button', { name: /Delivered/ })
    expect(delivered.textContent).toBe('Delivered')
    expect(screen.getByRole('button', { name: /All/ }).textContent).toBe('All')
  })
})

describe('no project or internal fields', () => {
  it('renders nothing from the team-side record, whatever the answer carries', async () => {
    // A maximally internal object, with the team record's own keys. A page that
    // read the record rather than the projection would paint at least one.
    const leaky = {
      ...row({ public_ref: 'reqLEAK1', subject: 'Ledger mismatch' }),
      id: 4242,
      org_id: 77,
      team_id: 88,
      project_id: 99,
      requester_user_id: 31,
      requester_contact: 'SENTINEL-contact@example.com',
      source_kind: 'connector',
      source_external_id: 'SENTINEL-gh-88',
      claim_user_id: 12,
      claimed_at: '2026-11-13T10:00:00Z',
      owner_user_id: 12,
      duplicate_of: 19,
      reject_reason: 'SENTINEL-reject-reason',
      needs_info_since: null,
      rev: 7,
      metadata: { jira_key: 'SENTINEL-JIRA-1', branch: 'SENTINEL-branch' },
      accepted_by: 12,
      reopened_count: 3,
    }
    server.use(route('*/api/requests', page([leaky])))
    mount()
    await settled()

    // The list is scoped because the page's pill row legitimately uses two of
    // these words ("In progress", "Delivered" are the mockup's own filter
    // labels); the leak question is about the row's content.
    const list = screen.getByRole('list', { name: 'Your requests' })
    const text = list.textContent ?? ''
    for (const word of [
      'SENTINEL', 'project', 'epic', 'story', 'ticket', 'agent', 'branch', 'metadata',
      'claim', 'assignee', 'org', 'session', 'portfolio', 'audit', 'settings', 'rev 7',
      'connector', 'jira',
    ]) {
      expect(text.toLowerCase(), word).not.toContain(word.toLowerCase())
    }
    // What it does show is the whitelisted row.
    expect(within(list).getByText('Ledger mismatch')).toBeInTheDocument()
    expect(within(list).getByText('In progress')).toBeInTheDocument()
    expect(within(list).getByText('REQ-reqLEAK1', { exact: false })).toBeInTheDocument()
  })

  it('shows only the whitelisted fields on a row, in the row’s own accessible name', async () => {
    server.use(route('*/api/requests', page([row({ subject: 'Ledger mismatch' })])))
    mount({ onOpenRequest: () => {} })
    await settled()

    const target = within(screen.getByRole('list', { name: 'Your requests' })).getByRole('button')
    expect(target).toHaveAccessibleName('Ledger mismatch, In progress')
    // And the desk's name is a public name, not an id: no numeric team/org id,
    // and no internal noun, anywhere in the list.
    expect(screen.getByRole('list', { name: 'Your requests' }).textContent).not.toMatch(/\b(org|team id|#88)\b/i)
  })
})

describe('mobile and keyboard (390 px)', () => {
  const original = window.innerWidth
  afterEach(() => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: original })
  })

  it('scrolls the pill strip at 390 px and keeps every control reachable', async () => {
    Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 390 })
    window.dispatchEvent(new Event('resize'))
    server.use(route('*/api/requests', page([row()], { total: 1 })))
    mount({ onOpenRequest: () => {}, onNewRequest: () => {} })
    await settled()

    // The mockup's 390 px rule: five pills are wider than a phone, so the strip
    // scrolls instead of pushing the page sideways.
    const pills = screen.getByRole('group', { name: 'Filter requests' })
    expect(pills.className).toMatch(/overflow-x-auto/)

    // Nothing is display:none'd away: the tag, the row and both actions survive.
    const list = screen.getByRole('list', { name: 'Your requests' })
    expect(within(list).getByText('In progress')).toBeVisible()
    const item = within(list).getByRole('listitem')
    expect(item).toBeVisible()
    expect(screen.getByRole('button', { name: 'New request' })).toBeVisible()
    // A row's target is a real button, so it is in the tab order and activates
    // on Enter.
    expect(within(item).getByRole('button').tagName).toBe('BUTTON')
  })

  it('operates the pills and a row by keyboard alone', async () => {
    const user = userEvent.setup()
    let opened: string | null = null
    server.use(http.get('*/api/requests', ({ request }) => {
      const search = new URL(request.url).search
      calls.push({ method: 'GET', path: new URL(request.url).pathname, search })
      return HttpResponse.json(page([row({ public_ref: 'reqKEY1' })], { total: 1 }) as unknown as Record<string, unknown>)
    }))
    mount({ onOpenRequest: ref => { opened = ref } })
    await settled()

    // A row opens from the keyboard: it is a button, so this needs no handler.
    const target = within(screen.getByRole('list', { name: 'Your requests' })).getByRole('button')
    target.focus()
    expect(target).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(opened).toBe('reqKEY1')

    // Tab to a pill and press Enter: a real button, so this needs no handler either.
    const delivered = screen.getByRole('button', { name: /Delivered/ })
    delivered.focus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(delivered).toHaveAttribute('aria-pressed', 'true'))
    expect(calls.some(c => c.search.includes('state=delivered'))).toBe(true)
  })
})