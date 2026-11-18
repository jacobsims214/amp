// After the live stream reconnects, "Decided upstream" re-reads its ticket's
// upstream decisions (#2533): a decision.updated sent while the stream was down
// (a dropped connection, or a tab hidden for 30 s) never arrives, so a decision
// made meanwhile would not show until the drawer was reopened.
import { act, render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { DecidedUpstream } from './DecidedUpstream'
import { api } from '../../api/client'
import { subscribeSSE } from '../../hooks/useSSE'
import type { UpstreamDecision } from '../../types'

const decided = (over: Partial<UpstreamDecision> = {}): UpstreamDecision => ({
  task_id: 34, name: 'Confirm: north-star metric', human_reason: 'judgment', option: 'B: Activation', decision: 'B: Activation — Go with B',
  decided_by: 9, decided_by_name: 'Jacob', decided_at: '2026-11-04T10:00:00Z', ...over,
})

/** What the server answers for ticket 41's upstream decisions. */
let upstream: UpstreamDecision[] = []
/** While set, the read waits for it: the strip's first load is in flight. */
let hold: Promise<void> | null = null
const server = setupServer(
  http.get('*/api/tasks/41/decisions', async () => {
    if (hold) await hold
    return HttpResponse.json({ upstream, own: null })
  }),
)

// The real useSSE over a fake stream (jsdom has no navigator.locks, so each
// test's tab opens its own): frames reach the strip as they would live.
let sources: FakeEventSource[] = []
class FakeEventSource {
  constructor() { sources.push(this) }
  onmessage: ((e: { data: string }) => void) | null = null
  onerror = null
  readyState = 1
  close() {}
}

let decisionReads: ReturnType<typeof vi.spyOn>
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
beforeEach(() => {
  upstream = [decided()]
  decisionReads = vi.spyOn(api, 'getTaskDecisions')
})
afterEach(() => {
  hold = null
  sources = []
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

const reads = () => decisionReads.mock.calls.length

function strip(dependencyIds: number[] = [34]) {
  render(
    <MemoryRouter>
      <DecidedUpstream taskId={41} projectId={2} dependencyIds={dependencyIds} ticketHref={id => `/project/2/board?focus=${id}`} />
    </MemoryRouter>,
  )
}

function send(data: unknown) {
  const source = sources.at(-1)!
  act(() => source.onmessage!({ data: JSON.stringify(data) }))
}

/** The server's first frame on every new stream. It carries no project. */
const connect = () => send({ type: 'connected' })

/** Lets any read a frame started reach the server and answer. */
const flush = () => act(() => new Promise(r => setTimeout(r, 20)))

/** Waits for the strip's first read to answer. */
const loaded = () => screen.findByTestId('decided-upstream')

describe('DecidedUpstream re-reads after a stream reconnect (#2533)', () => {
  it('a connected during the first read adds no request', async () => {
    let release!: () => void
    hold = new Promise(r => { release = r })
    strip()
    await waitFor(() => expect(reads()).toBe(1))
    // The opening frame of the stream the strip opened, while its read is in flight.
    connect()
    await flush()
    expect(reads()).toBe(1)

    release()
    await loaded()
    await flush()
    expect(reads()).toBe(1)
  })

  it('a connected after the first read has settled re-reads exactly once, and shows what was decided meanwhile', async () => {
    strip()
    expect(await loaded()).toHaveTextContent('Decided by Jacob on #34')
    expect(reads()).toBe(1)

    // A second dependency was decided while the stream was down.
    upstream = [decided(), decided({ task_id: 35, option: undefined, decision: 'Ship it Friday', decided_by_name: 'Sam' })]
    connect()
    await flush()
    expect(reads()).toBe(2)
    expect(await screen.findAllByTestId('decided-upstream-item')).toHaveLength(2)
    expect(screen.getByTestId('decided-upstream')).toHaveTextContent('Decided by Sam on #35: Ship it Friday')

    // Each later reconnect re-reads once more.
    connect()
    await flush()
    expect(reads()).toBe(3)
  })

  it('keeps the list on screen while the reconnect re-read is in flight', async () => {
    strip()
    await loaded()
    let release!: () => void
    hold = new Promise(r => { release = r })
    connect()
    await flush()
    expect(reads()).toBe(2)
    expect(screen.getByTestId('decided-upstream')).toHaveTextContent('Decided by Jacob on #34')
    release()
    await flush()
  })

  it('a strip that mounts on a stream another view already holds re-reads on the first connected it sees', async () => {
    // The Board holds the stream and heard its opening frame before the drawer opened.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      strip()
      await loaded()
      expect(sources).toHaveLength(1)
      expect(reads()).toBe(1)

      connect()
      await flush()
      expect(reads()).toBe(2)
    } finally {
      unsubscribe()
    }
  })

  it('a failed first read counts as settled, so a reconnect retries it', async () => {
    server.use(http.get('*/api/tasks/41/decisions', () => HttpResponse.json({ error: 'down' }, { status: 500 }), { once: true }))
    strip()
    await waitFor(() => expect(reads()).toBe(1))
    await flush()
    expect(screen.queryByTestId('decided-upstream')).toBeNull()

    connect()
    await loaded()
    expect(reads()).toBe(2)
  })

  it('ordinary events are not reconnects, and a dependency\'s decision.updated still re-reads once', async () => {
    strip()
    await loaded()
    send({ type: 'task.updated', project_id: 2, payload: { task_id: 34 } })
    send({ type: 'decision.updated', project_id: 2, payload: { task_id: 99, version: 2 } })
    await flush()
    expect(reads()).toBe(1)

    send({ type: 'decision.updated', project_id: 2, payload: { task_id: 34, version: 2 } })
    await flush()
    expect(reads()).toBe(2)
  })

  it('a ticket with no dependencies reads nothing, before or after a reconnect', async () => {
    strip([])
    await flush()
    connect()
    await flush()
    expect(reads()).toBe(0)
    expect(screen.queryByTestId('decided-upstream')).toBeNull()
  })
})
