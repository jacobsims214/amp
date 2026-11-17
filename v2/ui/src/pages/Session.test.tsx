import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { Session } from './Session'
import { AuthContext } from '../contexts/auth-context'
import { RemoteProvider } from '../remote/RemoteProvider'
import type { RemoteDeps } from '../remote/remote-context'
import type { RemoteCrypto } from '../remote/crypto'
import { memoryStore } from '../remote/deviceStore'
import { webRemoteCrypto } from '../remote/webCrypto'
import { httpTransport, RELAY_DOWN_MESSAGE, RelayError, type EventStreamHandlers, type RelayTransport } from '../remote/transport'
import { MOCK_SESSION_ID, MockRelay, mockCrypto, mockTransport, workerScene } from '../remote/mock/mockRelay'
import parityStream from '../remote/mock/parityStream.json'
import { loadVectors } from '../test/vectors'
import type { Me } from '../types'
import type { ChatEvent, SessionDetail, SessionStatus } from '../remote/types'

vi.setConfig({ testTimeout: 10_000 }) // the fake harness streams on real timers

const meWith = (available: boolean): Me => ({
  subject: 's', email: 'jsims@example.com', name: 'Jacob', roles: [], user_id: 1001,
  capabilities: { remote_control: { available, org_ids: available ? [3] : [], reason: available ? null : 'no_org_enabled' } },
})

function renderSession(deps: RemoteDeps, opts: { me?: Me; id?: string } = {}) {
  const me = opts.me ?? meWith(true)
  return render(
    <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: false }}>
      <RemoteProvider deps={deps}>
        <MemoryRouter initialEntries={[`/sessions/${opts.id ?? MOCK_SESSION_ID}`]}>
          <Routes><Route path="/sessions/:sessionId" element={<Session />} /></Routes>
        </MemoryRouter>
      </RemoteProvider>
    </AuthContext.Provider>,
  )
}

/** A current ticket on the status, the way the harness sends one mid-ticket: the header chip needs
 *  a name or a state as well as the id. */
const TICKET_STATUS: Partial<SessionStatus> = {
  ticket_id: 2372, ticket_name: 'M1-5 ChatEvent carries everything the TUI shows', ticket_state: 'in_progress',
  epic: 'Remote control', story: 'M1',
}

function mockSession(opts: { seed?: boolean; online?: boolean; paired?: boolean; status?: Partial<SessionStatus> } = {}) {
  // Each fake run takes ~1.2 s (8 steps), long enough to queue a second prompt behind it.
  const relay = new MockRelay({ stepMs: 150, seed: opts.seed ?? true, online: opts.online, status: opts.status })
  const deps: RemoteDeps = { transport: mockTransport(relay), crypto: mockCrypto({ paired: opts.paired }), mock: false }
  return { relay, deps }
}

/**
 * A session with a jsdom stand-in for `visualViewport`, so the keyboard case can be exercised
 * without a phone. jsdom ships neither a `visualViewport` nor a layout engine: `innerHeight` stays
 * whatever we set it to, and `viewport.resize(height)` moves only the visual viewport — which is
 * exactly the pair of facts a phone keyboard produces (the layout viewport does not change; the
 * visual one shrinks). `layoutWidth` stands in for the `window.innerWidth` that the
 * desktop-pinch-zoom guard reads.
 */
function keyboardSession(layoutWidth = 390) {
  const listeners = new Set<() => void>()
  const vv = {
    height: 844, width: layoutWidth, offsetTop: 0, offsetLeft: 0, pageTop: 0, pageLeft: 0, scale: 1,
    addEventListener: (_: string, fn: () => void) => { listeners.add(fn) },
    removeEventListener: (_: string, fn: () => void) => { listeners.delete(fn) },
    resize(height: number) {
      vv.height = height
      for (const fn of listeners) fn()
    },
  }
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: layoutWidth })
  Object.defineProperty(window, 'innerHeight', { configurable: true, writable: true, value: 844 })
  Object.defineProperty(window, 'visualViewport', { configurable: true, writable: true, value: vv })
  const { relay, deps } = mockSession({ status: TICKET_STATUS })
  return { relay, deps, viewport: vv }
}

describe('Session page', () => {
  it('renders every ChatEvent type, the status bar and presence', async () => {
    const { deps } = mockSession()
    const { container } = renderSession(deps)
    await screen.findByText('run the tests and tell me what fails')
    expect(screen.getByText('typed on the laptop')).toBeInTheDocument() // user_message (local)
    expect(screen.getByText(/^Thought for \d+s$/).closest('details')).not.toHaveAttribute('open') // thinking_delta, folded
    expect(screen.getByText(/Running the/)).toBeInTheDocument() // message_start + text_delta (Markdown)
    expect(screen.getAllByTestId('tool-row')).toHaveLength(1) // tool_start + tool_end
    expect(screen.getByText(/Full output \(178 KB\) is on the laptop/)).toBeInTheDocument()
    // retry: shown only while it counts down (the TUI's status slot), so not once the run went on.
    expect(screen.queryByText(/Retrying/)).toBeNull()
    expect(within(screen.getByLabelText('Session status')).getByTitle(/25,154 in/)).toBeInTheDocument() // message_end usage
    expect(screen.queryByTestId('run-end')).toBeNull() // run_end: a completed run draws nothing, as in the TUI
    expect(screen.getByText(/context7.*reconnected/)).toBeInTheDocument() // notice
    expect(within(screen.getByLabelText('Workers')).getByText('RC-M1 harness link')).toBeInTheDocument() // worker_status
    // Hostile model output: no images, no script, link with its full URL.
    expect(container.querySelector('img, script, iframe')).toBeNull()
    expect(screen.getByRole('link', { name: /the CI run \(https:\/\/github\.com\/jacobsims214\/amp\/actions\)/ })).toBeInTheDocument()

    const status = screen.getByLabelText('Session status')
    for (const t of ['claude-opus-5-5', '37%', '$1.83']) expect(within(status).getByText(t)).toBeInTheDocument()
    expect(screen.getByTestId('header-facts')).toHaveTextContent('AMP #2 · ticket #2372 · remote-m1-2372 · macos')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('jacobs-mbp')
    expect(screen.getByText('Laptop online')).toBeInTheDocument()
  })

  it('sends a prompt, shows the reply, and queues a second one while the agent is busy', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    const box = screen.getByLabelText('Message')
    await user.type(box, 'please fix it{Enter}')
    await waitFor(() => expect(relay.received.map(c => c.op)).toEqual(['prompt']))
    const sent = relay.received[0]
    expect(sent.body).toMatchObject({ text: 'please fix it', client_msg_id: expect.stringMatching(/^[A-Za-z0-9_-]{1,64}$/) })
    expect(sent.sig).toBeTruthy()
    expect(sent.seq).toBeGreaterThan(1_700_000_000_000)
    await screen.findByText(/Chrome on macOS \(this device\)/)
    // While the run is open, the next message is queued.
    await user.type(box, 'and then the docs{Enter}')
    const strip = await screen.findByLabelText('Queued messages')
    expect(within(strip).getByText('and then the docs')).toBeInTheDocument()
    expect(within(strip).getByText('you')).toBeInTheDocument()
    expect(relay.received[1].seq).toBeGreaterThan(relay.received[0].seq)
    // It runs when the first one finishes.
    await waitFor(() => expect(screen.queryByLabelText('Queued messages')).toBeNull(), { timeout: 3000 })
    await waitFor(() => expect(screen.getAllByText(/You said: "and then the docs"/)).toHaveLength(1), { timeout: 3000 })
  })

  it('sends slash text as a literal prompt, never as a command or another op (R-M6, #2379)', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    await user.type(screen.getByLabelText('Message'), '/remote off{Enter}')
    await waitFor(() => expect(relay.received).toHaveLength(1))
    expect(relay.received[0].op).toBe('prompt')
    expect(relay.received[0].body).toMatchObject({ text: '/remote off' })
    // Still connected: the page didn't treat it as Disconnect.
    expect(screen.getByText('Laptop online')).toBeInTheDocument()
    expect(relay.received.map(c => c.op)).toEqual(['prompt'])
  })

  it('Stop sends a signed stop and the harness confirms it', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    await user.type(screen.getByLabelText('Message'), 'long job{Enter}')
    await screen.findByText(/Chrome on macOS \(this device\)/)
    await user.click(screen.getByRole('button', { name: 'Stop the agent' }))
    await waitFor(() => expect(relay.received.map(c => c.op)).toEqual(['prompt', 'stop']))
    await screen.findByText('Stopped.')
    // The shell's "Stopped." says it once: run_end adds no second line.
    expect(screen.getAllByText('Stopped.')).toHaveLength(1)
  })

  it('cancelling the queue stops the run (M1 has no per-message cancel)', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    const box = screen.getByLabelText('Message')
    await user.type(box, 'first{Enter}')
    await screen.findByText(/Chrome on macOS \(this device\)/)
    await user.type(box, 'second{Enter}')
    await screen.findByLabelText('Queued messages')
    await user.click(screen.getByRole('button', { name: 'Cancel queued' }))
    await user.click(screen.getByRole('button', { name: 'Stop and clear' }))
    await waitFor(() => expect(relay.received.at(-1)?.op).toBe('stop'))
    await waitFor(() => expect(screen.queryByLabelText('Queued messages')).toBeNull())
  })

  it('the composer row holds Send and an inline Stop, and the transparent region keeps the space', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    const { container } = renderSession(deps)
    await screen.findByText('Laptop online')

    // The dock order (#4896, audit F2): the transcript is the only flexible region, and every row
    // that holds a control is `shrink-0`, so a tall dock squeezes the chat, never the reverse.
    const transcript = screen.getByTestId('transcript')
    expect(transcript).toHaveClass('flex-1', 'min-h-0', 'overflow-y-auto')
    const dockRows = [screen.getByTestId('composer'), screen.getByTestId('status-line')]
    for (const row of dockRows) expect(row).toHaveClass('shrink-0')
    expect(container.querySelectorAll('[data-testid="transcript"]')).toHaveLength(1)

    // Send and Stop share the composer's row, and Stop is in it — not a full-width bar below.
    const composer = screen.getByTestId('composer')
    const form = composer.querySelector('form')!
    const send = within(form).getByRole('button', { name: 'Send' })
    const stop = within(form).getByRole('button', { name: 'Stop the agent' })
    expect(form).toContainElement(stop)
    expect(stop).toHaveAttribute('data-testid', 'stop-inline')
    expect(stop).not.toHaveClass('w-full')
    expect(send.compareDocumentPosition(stop) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()

    // No full-width Stop bar survives anywhere on the page.
    const fullWidthStops = [...container.querySelectorAll('button[aria-label="Stop the agent"]')].filter(b => b.className.includes('w-full'))
    expect(fullWidthStops).toEqual([])

    // The textarea can never be dragged: `resize-none` is the only resize utility on it.
    const box = screen.getByLabelText('Message')
    expect(box).toHaveClass('resize-none')
    // No `resize`/`resize-x`/`resize-y` class on it, whatever the width (same guard as
    // SessionParts.test.tsx). Split into class names and test each one: a double-escaped
    // `\\b` inside the regex literal would match a literal backslash and could never fail.
    expect(box.className.split(/\s+/).filter(c => /^resize(-x|-y)?$/.test(c))).toEqual([])

    // Nothing to explain before a run: no reason line, and a plain placeholder.
    expect(screen.queryByTestId('composer-reason')).toBeNull()
    expect(box).toHaveAttribute('placeholder', 'Message the agent…')

    // Mid-run Stop stays reachable (R-M7): it is enabled while the agent works, and the reason
    // for the next message sits one line above the row. A run_start is pushed straight in, so this
    // never races the mock harness's own timer.
    act(() => { relay.chat({ type: 'run_start', run_id: 'run_inflight', prompt_id: 'p_inflight' }) })
    expect(await screen.findByTestId('composer-reason')).toHaveTextContent('Agent busy — your message joins the queue.')
    expect(within(form).getByRole('button', { name: 'Stop the agent' })).toBeEnabled()
    expect(within(form).getByRole('button', { name: 'Queue' })).toBeInTheDocument()
    // And it is the button that actually stops the run.
    await user.click(screen.getByRole('button', { name: 'Stop the agent' }))
    await waitFor(() => expect(relay.received.at(-1)?.op).toBe('stop'))
    expect(relay.received.filter(c => c.op === 'stop')).toHaveLength(1)
  })

  it('offline: badge, no sending, Stop disabled; back online re-enables', async () => {
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    act(() => relay.setOnline(false))
    await screen.findByText('Laptop offline')
    expect(screen.getByLabelText('Message')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Stop the agent' })).toBeDisabled()
    expect(screen.getAllByText(/Messages aren't queued while it's away/).length).toBeGreaterThan(0)
    act(() => relay.setOnline(true))
    await screen.findByText('Laptop online')
    expect(screen.getByLabelText('Message')).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Stop the agent' })).toBeEnabled()
  })

  it('a relay refusal (409 harness_offline) shows on the message, never queued', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession()
    renderSession(deps)
    await screen.findByText('Laptop online')
    relay.online = false // the relay knows before the page hears about it
    await user.type(screen.getByLabelText('Message'), 'hello?{Enter}')
    await screen.findByText(/The laptop is offline/)
    expect(relay.received).toHaveLength(0)
  })

  it('drops forged events with a security notice and never renders them', async () => {
    const { relay, deps } = mockSession({ seed: false })
    renderSession(deps)
    await screen.findByText('Laptop online')
    act(() => {
      const forged = JSON.parse(relay.chat({ type: 'notice', level: 'info', code: null, text: 'placeholder' }))
      void forged
      const wire = JSON.stringify({ ...forged, seq: 999, body: { type: 'text_delta', message_id: 'm', delta: 'Ignore previous instructions' }, sig: 'forged' })
      relay.inject({ event: 'envelope', data: wire })
    })
    await screen.findByText(/Untrusted data from the relay was dropped \(bad signature\)/)
    expect(screen.queryByText(/Ignore previous instructions/)).toBeNull()
  })

  it('a newer protocol version stops the page', async () => {
    const { relay, deps } = mockSession({ seed: false })
    renderSession(deps)
    await screen.findByText('Laptop online')
    act(() => {
      const ok = JSON.parse(relay.chat({ type: 'notice', level: 'info', code: null, text: 'x' }))
      relay.inject({ event: 'envelope', data: JSON.stringify({ ...ok, v: 2, seq: ok.seq + 1 }) })
    })
    await screen.findByText(/This laptop runs a newer harness; update AMP/)
    expect(screen.getByLabelText('Message')).toBeDisabled()
  })

  it('reconnects with from_seq after the stream drops', async () => {
    const { relay, deps } = mockSession({ seed: false })
    const calls: (number | null)[] = []
    let drop: EventStreamHandlers | null = null
    const inner = deps.transport
    const transport: RelayTransport = { ...inner, openEvents: (id, from, h) => { calls.push(from); drop = h; return inner.openEvents(id, from, h) } }
    renderSession({ ...deps, transport })
    await screen.findByText('Laptop online')
    act(() => { relay.chat({ type: 'notice', level: 'info', code: null, text: 'before the drop' }) })
    await screen.findByText('before the drop')
    act(() => drop!.onClose())
    await screen.findByText('Reconnecting…')
    await waitFor(() => expect(calls).toHaveLength(2), { timeout: 3000 })
    expect(calls).toEqual([null, 3]) // hello was 1, the notice 2
    await screen.findByText('Laptop online')
    expect(screen.getAllByText('before the drop')).toHaveLength(1) // replayed events aren't drawn twice
  })

  it('Disconnect sends a signed detach, then ends the session', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession({ seed: false })
    renderSession(deps)
    await screen.findByText('Laptop online')
    await user.click(screen.getByRole('button', { name: /Disconnect/ }))
    await user.click(screen.getByRole('button', { name: 'Disconnect now' }))
    await waitFor(() => expect(relay.ended).toBe('disconnected'))
    expect(relay.received.map(c => c.op)).toEqual(['detach'])
    await screen.findAllByText(/You disconnected this session/)
    expect(screen.getByLabelText('Message')).toBeDisabled()
  })

  it('an unpaired browser sees pairing instructions and nothing from the relay', async () => {
    const { deps } = mockSession({ paired: false })
    renderSession(deps)
    await screen.findByText('Pair this device from the laptop')
    expect(screen.queryByText('run the tests and tell me what fails')).toBeNull()
  })

  it('respects the capability flag', async () => {
    const { deps } = mockSession()
    renderSession(deps, { me: meWith(false) })
    expect(await screen.findByText('Remote control isn\'t available')).toBeInTheDocument()
  })

  it('treats a missing or foreign session as not found', async () => {
    const { deps } = mockSession()
    renderSession(deps, { id: 'rs_ZZZZZZZZZZZZZZZZZZZZZZ' })
    expect(await screen.findByText('No such session')).toBeInTheDocument()
    renderSession(deps, { id: 'not-a-session' })
    expect((await screen.findAllByText('No such session')).length).toBeGreaterThan(0)
  })

  it('a phone header is one row plus the ticket chip, and Disconnect lives in the overflow menu (#4897)', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession({ status: TICKET_STATUS })
    renderSession(deps)
    await screen.findByText('Laptop online')
    const header = document.querySelector('header')!
    const ticket = screen.getByTestId('ticket-chip')

    // One header: the ticket chip lives inside it on a phone, not as a second bar above the chat
    // (audit #3026 F4 — the header wrapped to three rows and the ticket added a fourth).
    expect(header).toContainElement(ticket)
    // The desktop strip is still there for `sm` and up, and it is a separate node: the two layouts
    // never fight over one element's display.
    const strip = screen.getByTestId('ticket-line')
    expect(header).not.toContainElement(strip)
    expect(strip).toHaveClass('hidden', 'sm:flex')
    // The chip keeps the state as well as the id and the name: the one thing a person checking in
    // from a phone needs at a glance never drops out at any width.
    expect(ticket).toHaveTextContent('In progress')
    expect(ticket).toHaveTextContent('#2372')
    expect(ticket).toHaveTextContent('ChatEvent carries everything')

    // The header's right side is on row 1 with back and host, not wrapped onto rows of its own.
    const row1 = header.firstElementChild as HTMLElement
    expect(row1).toHaveClass('w-full', 'sm:contents')
    expect(within(row1).getByRole('link', { name: 'All sessions' })).toBeInTheDocument()
    expect(within(row1).getByRole('heading', { level: 1 })).toHaveTextContent('jacobs-mbp')
    expect(within(row1).getByText('Laptop online')).toBeInTheDocument()

    // Disconnect is reachable from the overflow menu (the phone's only way to it: the desktop
    // button is `hidden sm:block`, so it is not on a phone's header), and it still takes two steps:
    // the menu's Disconnect arms the confirm, Disconnect now does the detach.
    expect(screen.getByRole('button', { name: /^Disconnect$/ }).parentElement).toHaveClass('hidden', 'sm:block')
    expect(within(header).getByTestId('session-menu')).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByTestId('session-menu-panel')).toBeNull()
    await user.click(within(header).getByRole('button', { name: 'Session menu' }))
    expect(within(header).getByTestId('session-menu')).toHaveAttribute('aria-expanded', 'true')
    const panel = within(header).getByTestId('session-menu-panel')
    expect(panel).toHaveAttribute('role', 'menu')
    await user.click(within(panel).getByRole('menuitem', { name: /Disconnect/ }))
    // The desktop header's own Disconnect is still there and untouched: the menu is a second way in.
    await user.click(within(panel).getByRole('button', { name: 'Disconnect now' }))
    await waitFor(() => expect(relay.ended).toBe('disconnected'))
    expect(relay.received.map(c => c.op)).toEqual(['detach'])
    await screen.findAllByText(/You disconnected this session/)
    // Nothing left to disconnect once it has ended: no menu, no button.
    expect(screen.queryByTestId('session-menu')).toBeNull()
  })

  it('the overflow menu closes on a tap outside and on Escape, without disconnecting', async () => {
    const user = userEvent.setup()
    const { relay, deps } = mockSession({ seed: false })
    renderSession(deps)
    await screen.findByText('Laptop online')
    const open = () => user.click(screen.getByRole('button', { name: 'Session menu' }))
    await open()
    await user.click(document.body)
    await waitFor(() => expect(screen.queryByTestId('session-menu-panel')).toBeNull())
    await open()
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByTestId('session-menu-panel')).toBeNull())
    expect(relay.received.map(c => c.op)).toEqual([])
  })

  it('with a phone keyboard up the status line, ticket chip and dock extras yield to the chat (#4897)', async () => {
    const { viewport, deps } = keyboardSession()
    try {
      renderSession(deps)
      await screen.findByText('Laptop online')

      // Idle: the whole dock is there and the status line carries the phone's safe-area padding.
      expect(screen.getByTestId('status-line')).toBeInTheDocument()
      expect(screen.getByTestId('ticket-chip')).toBeInTheDocument()
      expect(screen.getByTestId('composer')).not.toHaveClass('pb-[calc(0.625rem+env(safe-area-inset-bottom))]')
      expect(screen.getByTestId('status-line').parentElement).toHaveClass('pb-[env(safe-area-inset-bottom)]')

      // The keyboard opens: visualViewport shrinks to 55% while the layout viewport is unchanged,
      // which is exactly how a phone reports a soft keyboard (window resize never fires).
      act(() => viewport.resize(464))

      // The status line and the ticket chip yield — but the transcript and composer keep the height.
      await waitFor(() => expect(screen.queryByTestId('status-line')).toBeNull())
      expect(screen.queryByTestId('ticket-chip')).toBeNull()
      expect(screen.queryByLabelText('Workers')).toBeNull()
      expect(screen.getByTestId('transcript')).toHaveClass('flex-1', 'min-h-0', 'overflow-y-auto')
      expect(screen.getByLabelText('Message')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Send' })).toBeInTheDocument()
      // The header's first row stays: back, host, presence, the menu.
      const header = document.querySelector('header')!
      expect(within(header).getByRole('heading', { level: 1 })).toHaveTextContent('jacobs-mbp')
      expect(within(header).getByTestId('session-menu')).toBeInTheDocument()
      // F6: the safe-area inset moves from the (now hidden) status line onto the composer dock, or
      // it slides under the home indicator.
      expect(screen.getByTestId('composer')).toHaveClass('pb-[calc(0.625rem+env(safe-area-inset-bottom))]')

      // The keyboard goes away and everything comes back.
      act(() => viewport.resize(844))
      await waitFor(() => expect(screen.getByTestId('status-line')).toBeInTheDocument())
      expect(screen.getByTestId('ticket-chip')).toBeInTheDocument()
      expect(screen.getByTestId('composer')).not.toHaveClass('pb-[calc(0.625rem+env(safe-area-inset-bottom))]')
    } finally {
      delete (window as { visualViewport?: unknown }).visualViewport
    }
  })

  it('a desktop pinch-zoom is not a keyboard: the status line stays', async () => {
    const { viewport, deps } = keyboardSession(1200)
    try {
      renderSession(deps)
      await screen.findByText('Laptop online')
      expect(screen.getByTestId('status-line')).toBeInTheDocument()
      act(() => viewport.resize(700))
      // A wide layout viewport has no soft keyboard, so the shrunken visual viewport is a zoom.
      await new Promise(r => setTimeout(r, 0))
      expect(screen.getByTestId('status-line')).toBeInTheDocument()
    } finally {
      delete (window as { visualViewport?: unknown }).visualViewport
    }
  })
})

describe('Session page with the protocol test vectors', () => {
  interface V { name: string; wire: string; expect: string; receiver: { session_id: string; pinned_harness: { harness_id: string; harness_pub: string } } }
  const vectors = loadVectors<{ vectors: V[] }>('events.json').vectors
  const rx = vectors[0].receiver
  it('renders the valid vectors and drops the forged ones', async () => {
    // The real WebCrypto implementation, with the vectors' harness pinned in an in-memory store.
    const store = memoryStore()
    await store.putPin({ ...rx.pinned_harness, pinned_until_ms: Date.now() + 86_400_000 })
    const crypto: RemoteCrypto = webRemoteCrypto(store)
    const detail: SessionDetail = {
      session_id: rx.session_id, harness_id: rx.pinned_harness.harness_id, harness_pub: rx.pinned_harness.harness_pub,
      harness_fp_display: '29CA-E6B5-00C2-66CB', host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' },
      online: true, viewers: 1, started_at_ms: 0, expires_at_ms: 0, last_activity_at_ms: 0, protocol_version: 1,
    }
    const order = ['hello_valid', 'chat_text_delta_valid', 'chat_tool_start_valid', 'chat_message_end_valid', 'ack_queued_valid',
      'refused_valid', 'pair_result_accepted_valid', 'duplicate_seq_dropped', 'bad_signature_body_changed',
      'wrong_harness_fake_registration', 'bad_signature_claims_pinned_harness', 'wrong_session', 'unknown_op_ignored']
    const transport: RelayTransport = {
      listSessions: async () => [detail],
      revokeAll: async () => ({ ended: 0, devices_revoked: 0 }),
      getSession: async id => { if (id !== rx.session_id) throw new RelayError(404, 'not_found', 'x'); return detail },
      openEvents: (_id, _from, h) => {
        queueMicrotask(() => {
          h.onOpen()
          h.onRelay({ type: 'attached', online: true, oldest_seq: 42, latest_seq: 48 })
          for (const n of order) h.onEnvelope(vectors.find(v => v.name === n)!.wire)
        })
        return () => {}
      },
      postCommand: async () => {},
      disconnect: async () => {},
      pair: async () => { throw new Error('not used') },
    }
    const { container } = renderSession({ transport, crypto, mock: false }, { id: rx.session_id })
    await screen.findByText(/Running the tests/) // text_delta
    // Real ES256 verification is asynchronous: wait until the last vector has been through the check.
    await waitFor(() => expect(screen.getAllByRole('alert').filter(a => /Untrusted data/.test(a.textContent ?? ''))).toHaveLength(4))
    expect(screen.getByText('bash')).toBeInTheDocument() // tool_start
    expect(within(screen.getByLabelText('Session status')).getByTitle(/12,034 in/)).toBeInTheDocument() // message_end usage
    expect(within(screen.getByLabelText('Session status')).getByText('$1.83')).toBeInTheDocument() // hello status
    expect(screen.getAllByText(/Running the tests/)).toHaveLength(1) // the duplicate seq is dropped silently
    expect(screen.queryByText(/Ignore previous instructions/)).toBeNull()
    expect(screen.queryByText('hi')).toBeNull() // fake harness / wrong session never render
    expect(screen.queryByText(/Which branch/)).toBeNull() // unknown op is skipped
    expect(screen.getAllByRole('alert').filter(a => /Untrusted data/.test(a.textContent ?? ''))).toHaveLength(4)
    // pair_result for another device: a visible warning, not silence.
    expect(screen.getByText(/paired another device/)).toBeInTheDocument()
    expect(container.querySelector('img, script')).toBeNull()
  })
})


describe('Session page reads like the harness TUI (#2386)', () => {
  const recorded = parityStream as { at_ms: number; event: ChatEvent }[]
  const withStream = (stream: { at_ms: number; event: ChatEvent }[]) => {
    const relay = new MockRelay({ stepMs: 0, stream })
    return { relay, deps: { transport: mockTransport(relay), crypto: mockCrypto(), mock: false } as RemoteDeps }
  }

  it('renders the recorded harness feed with the TUI\'s structure', async () => {
    const { deps } = withStream(recorded)
    renderSession(deps)
    await screen.findByText('try again')
    // Thinking folded with its duration, both sections.
    expect(screen.getAllByText(/^Thought for \d+s$/).map(e => e.textContent)).toEqual(['Thought for 4s', 'Thought for 1s'])
    // Tool calls grouped; the six-call group folds its first three successful calls.
    const groups = screen.getAllByTestId('tool-group')
    expect(groups).toHaveLength(3)
    const fold = within(groups[1]).getByTestId('tool-fold')
    expect(fold).toHaveTextContent('3 calls·create_epic, create_story, create_tasks')
    const failed = within(groups[1]).getAllByTestId('tool-row').find(r => r.dataset.state === 'failed')!
    expect(failed).toHaveTextContent('assign_task #526')
    expect(within(failed).getByTestId('tool-summary')).toHaveTextContent('"me" needs a signed-in AMP user')
    expect(failed).toHaveTextContent('6 ms')
    expect(within(groups[2]).getByTestId('tool-summary')).toHaveTextContent('30 lines')
    // The remote turns carry their device; the local ones say so.
    expect(screen.getAllByTestId('remote-tag').map(t => t.textContent)).toEqual(['RemoteSafari on iPhone', 'RemoteSafari on iPhone'])
    // Stopped once; the failed run's two error lines; no extra run-end lines.
    expect(screen.getAllByText('Stopped.')).toHaveLength(1)
    expect(screen.getByText('✗ Error: 429 Too Many Requests: rate limit exceeded for this key')).toBeInTheDocument()
    expect(screen.getByText('✗ Error: No more faux responses queued')).toBeInTheDocument()
    expect(screen.queryByTestId('run-end')).toBeNull()
    // Markdown table and the link's full URL.
    expect(screen.getByRole('table')).toHaveTextContent('Research JetStream APIs')
    expect(screen.getByRole('link', { name: /amp\.example\.test\/project\/158\/plan/ })).toBeInTheDocument()
  })

  it('opens a tool call to its arguments and pretty-printed result', async () => {
    const user = userEvent.setup()
    const { deps } = withStream(recorded)
    renderSession(deps)
    await screen.findByText('try again')
    const row = screen.getAllByTestId('tool-row').find(r => r.textContent?.includes('get_task #514'))!
    await user.click(within(row).getByText('get_task #514'))
    expect(row).toHaveAttribute('open')
    expect(within(row).getByText(/"comments_total": 3/)).toBeInTheDocument()
  })

  it('shows a run in flight: working timer, running call, and the dispatched workers', async () => {
    const t0 = Date.now() - 3_400
    const { deps } = withStream(workerScene(t0))
    renderSession(deps)
    await screen.findByText('dispatch both')
    expect(screen.getByTestId('working')).toHaveTextContent(/Working… 3s/)
    const rows = screen.getAllByTestId('tool-row')
    expect(rows.map(r => r.dataset.state)).toEqual(['running', 'running'])
    expect(within(rows[0]).getByTestId('worker-live')).toHaveTextContent('Add the clamp helper with a fairly long ticket name to test cuts · worker-backend (wf-1) · running 2s')
    const chips = within(screen.getByLabelText('Workers')).getAllByTestId('worker-chip')
    expect(chips.map(c => c.textContent)).toEqual([
      '#218Add the clamp helper with a fairly long ticket name to test cuts· worker-backend (wf-1)· running 2s',
      '#219Write the README line· worker-backend (wf-1)· running 2s',
    ])
    expect(screen.getByText(/▶ worker started #218/)).toBeInTheDocument()
  })

  it('counts a retry down in the working line while it is pending', async () => {
    const now = Date.now()
    const { deps } = withStream([
      { at_ms: now - 500, event: { type: 'user_message', prompt_id: 'p_1', text: 'go', origin: 'local', device_id: null, device_name: null } },
      { at_ms: now - 500, event: { type: 'run_start', run_id: 'run_1', prompt_id: 'p_1' } },
      { at_ms: now - 400, event: { type: 'retry', attempt: 1, max_retries: 10, delay_ms: 2000, error: '429 Too Many Requests' } },
    ])
    renderSession(deps)
    await screen.findByText('go')
    expect(screen.getByTestId('working')).toHaveTextContent(/Retrying \(1\/10\) in 2s…429 Too Many RequestsStop to cancel/)
  })
})

describe('Session page shows what the TUI shows besides (#2388)', () => {
  interface V { name: string; wire: string; receiver: { session_id: string; pinned_harness: { harness_id: string; harness_pub: string } } }
  const vectors = loadVectors<{ vectors: V[] }>('events.json').vectors
  const rx = vectors[0].receiver
  it('renders the #2388 vectors: ticket line, board, AMP connection, worker chips, compaction, error sentence, refused line', async () => {
    const store = memoryStore()
    await store.putPin({ ...rx.pinned_harness, pinned_until_ms: Date.now() + 86_400_000 })
    const detail: SessionDetail = {
      session_id: rx.session_id, harness_id: rx.pinned_harness.harness_id, harness_pub: rx.pinned_harness.harness_pub,
      harness_fp_display: '29CA-E6B5-00C2-66CB', host: 'jacobs-mbp', os: 'macos', harness_version: '0.4.0', project: { id: 2, name: 'AMP' },
      online: true, viewers: 1, started_at_ms: 0, expires_at_ms: 0, last_activity_at_ms: 0, protocol_version: 1,
    }
    const order = ['hello_valid', 'status_tui_facts_valid', 'chat_worker_status_tui_facts_valid', 'chat_compaction_valid',
      'chat_message_end_error_summary_valid', 'chat_tool_end_refused_valid']
    const transport: RelayTransport = {
      listSessions: async () => [detail],
      revokeAll: async () => ({ ended: 0, devices_revoked: 0 }),
      getSession: async () => detail,
      openEvents: (_id, _from, h) => {
        queueMicrotask(() => {
          h.onOpen()
          h.onRelay({ type: 'attached', online: true, oldest_seq: 42, latest_seq: 53 })
          for (const n of order) h.onEnvelope(vectors.find(v => v.name === n)!.wire)
        })
        return () => {}
      },
      postCommand: async () => {},
      disconnect: async () => {},
      pair: async () => { throw new Error('not used') },
    }
    renderSession({ transport, crypto: webRemoteCrypto(store), mock: false }, { id: rx.session_id })
    // The last vector (refused line) has been through the real ES256 check.
    const refused = await screen.findByTestId('tool-summary')
    expect(refused).toHaveTextContent('AMP refused: this needs a person in the AMP web UI (below)')
    expect(refused.closest('[data-testid=tool-row]')).toHaveAttribute('data-state', 'failed')
    // Gap 2: the TUI's ticket line, header facts and status line.
    const ticket = screen.getByTestId('ticket-line')
    expect(ticket).toHaveTextContent('In progress')
    expect(ticket).toHaveTextContent('#2388')
    expect(ticket).toHaveTextContent('RC-M1 harness + UI: ChatEvent carries everything the TUI shows')
    expect(within(ticket).getByTestId('ticket-epic')).toHaveTextContent('Remote control › M1')
    expect(screen.getByTestId('header-facts')).toHaveTextContent('AMP #2 · amp (remote-m1-2388) · manager · macos')
    const status = screen.getByLabelText('Session status')
    expect(within(status).getByText('• high')).toBeInTheDocument()
    expect(within(status).getByText('1.0M')).toBeInTheDocument() // the harness's totals, not this page's sum
    expect(within(status).getByTitle(/120,340 in · 8,311 out · 901,200 cached, the session's totals on the laptop/)).toBeInTheDocument()
    expect(within(status).getByText('$2.10')).toBeInTheDocument()
    const board = screen.getByTestId('board-line')
    expect(board).toHaveTextContent('1 question')
    expect(board).toHaveTextContent('3 In progress')
    expect(board).toHaveTextContent('5 Ready')
    expect(within(board).getByTestId('plan-approval')).toHaveTextContent('plan: 2 waiting for your approval')
    // Gap 6: the harness's own AMP connection.
    expect(screen.getByTestId('amp-connection')).toHaveTextContent('slow 812 ms')
    // Gap 1: worker chips with epic › story, tokens, context and cost. Only the running worker
    // gets a default row; the finished one is counted on the summary line and listed in the
    // sheet (#4895).
    const roster = screen.getByLabelText('Workers')
    const chips = within(roster).getAllByTestId('worker-chip')
    expect(chips).toHaveLength(1)
    expect(chips[0]).toHaveTextContent('#2390')
    expect(within(roster).getByTestId('roster-counts')).toHaveTextContent('1 running · 1 done')
    expect(within(chips[0]).getByTestId('worker-epic')).toHaveTextContent('QA3d epic › Watch workers')
    expect(within(chips[0]).getByTestId('worker-tokens')).toHaveTextContent('7.8k tok')
    expect(within(chips[0]).getByTestId('worker-ctx')).toHaveTextContent('ctx 8%')
    expect(within(chips[0]).getByTestId('worker-cost')).toHaveTextContent('$0.04')
    await userEvent.setup().click(within(roster).getByTestId('roster-summary'))
    const rows = within(screen.getByTestId('roster-sheet')).getAllByTestId('roster-row')
    expect(rows).toHaveLength(2)
    expect(within(rows[1]).getByTestId('worker-state')).toHaveTextContent('completed after 3m10s')
    // Gap 3: compaction in the working line.
    expect(screen.getByTestId('working')).toHaveTextContent('Auto-compacting…')
    // Gap 4: the TUI's one sentence, the raw provider text one click away.
    expect(screen.getByTestId('reply-error')).toHaveTextContent('✗ Error: OpenRouter rate-limited the request (429): Rate limit exceeded: free-models-per-day. Wait a minute and try again, or pick another model.')
    expect(within(screen.getByTestId('reply-error-detail')).getByText(/^http error 429:/)).toBeInTheDocument()
  })
})

describe('Session page while amp-relay is down', () => {
  /** A transport whose getSession fails with `err` for the first `failures` calls, then works. */
  function outage(failures: number, err: () => Error) {
    const { relay, deps } = mockSession()
    const calls = { n: 0 }
    const inner = deps.transport
    const transport: RelayTransport = {
      ...inner,
      getSession: async id => { calls.n++; if (calls.n <= failures) throw err(); return inner.getSession(id) },
    }
    return { relay, deps: { ...deps, transport }, calls }
  }

  it('a page opened during the outage opens the session by itself once the relay is back (#2455)', async () => {
    const { deps, calls } = outage(2, () => new RelayError(503, 'relay_down', RELAY_DOWN_MESSAGE))
    renderSession(deps)
    expect(await screen.findByText('Waiting to open the session')).toBeInTheDocument()
    expect(screen.getByText(RELAY_DOWN_MESSAGE)).toBeInTheDocument()
    expect(screen.getByText(/keeps trying and opens the session as soon as it can/)).toBeInTheDocument()
    expect(screen.queryByText("Couldn't open the session")).toBeNull()
    // No reload: the backoff (about 1 s, then about 2 s) tries again and the session opens.
    expect(await screen.findByText('Laptop online', {}, { timeout: 6_000 })).toBeInTheDocument()
    expect(calls.n).toBe(3)
    expect(screen.queryByText('Waiting to open the session')).toBeNull()
  })

  it('a network failure is retried too, and Try now tries at once', async () => {
    const user = userEvent.setup()
    const { deps, calls } = outage(1, () => new TypeError('Failed to fetch'))
    renderSession(deps)
    await screen.findByText('Waiting to open the session')
    await user.click(screen.getByRole('button', { name: 'Try now' }))
    expect(await screen.findByText('Laptop online', {}, { timeout: 500 })).toBeInTheDocument()
    expect(calls.n).toBe(2)
  })

  it('a refusal is final: no retry loop', async () => {
    const { deps, calls } = outage(99, () => new RelayError(403, 'forbidden', 'Not allowed.'))
    renderSession(deps)
    expect(await screen.findByText("Couldn't open the session")).toBeInTheDocument()
    expect(screen.getByText('Not allowed.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Try now' })).toBeNull()
    await new Promise(r => setTimeout(r, 1_500))
    expect(calls.n).toBe(1)
  })

  it('says remote control is unavailable instead of a raw 503', async () => {
    const fetchMock = vi.fn(async () => new Response('no healthy upstream', { status: 503, headers: { 'Content-Type': 'text/plain' } }))
    vi.stubGlobal('fetch', fetchMock)
    try {
      renderSession({ transport: httpTransport, crypto: mockCrypto(), mock: false })
      expect(await screen.findByText("Remote control is unavailable right now (the relay service isn't running).")).toBeInTheDocument()
      expect(screen.queryByText(/Request failed/)).toBeNull()
      expect(fetchMock).toHaveBeenCalled()
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
