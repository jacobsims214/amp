// After the live stream reconnects, a human ticket's panel re-reads its
// decision doc (#2532): a decision.updated sent while the stream was down (a
// dropped connection, or a tab hidden for 30 s) never arrives, so an edit or a
// decision recorded in that gap would otherwise never show.
import { act, render, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { HumanWork } from './HumanWork'
import { api } from '../../api/client'
import { subscribeSSE } from '../../hooks/useSSE'
import type { DecisionDoc, Task } from '../../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 34, project_id: 2, epic_id: 1, story_id: 3, name: 'Confirm: north-star targets', description: 'Pick the metric',
  acceptance_criteria: '- [ ] Metric chosen', state: 'backlog', priority: '1', dependency_ids: [],
  created_at: '2026-11-01T10:00:00Z', updated_at: '2026-11-01T10:00:00Z', ready_at: '2026-11-02T10:00:00Z', owner_user_id: 9,
  performer: 'human', human_reason: 'judgment', has_decision_doc: true, ...over,
})
const docV = (text: string): DecisionDoc => ({
  task_id: 34, artifact_id: 7, current_version: 1, author_kind: 'agent',
  ask: { question: 'Which metric?', options: [{ label: 'A', consequence: 'a' }, { label: 'B', consequence: 'b' }], recommended: 1 },
  text, decided_version: null, chosen_option: null, decided_by: null, recording_started_at: null, decided_at: null, versions: [],
})

/** What the decision read answers with next. */
let doc: DecisionDoc = docV('First draft')
/** While set, the decision read waits for it: a read is in flight. */
let hold: Promise<void> | null = null
const server = setupServer(
  http.get('*/api/tasks/34/decision', async () => {
    if (hold) await hold
    return HttpResponse.json(doc)
  }),
)

// The real useSSE over a fake stream (jsdom has no navigator.locks, so each
// test's tab opens its own): frames reach the panel as they would live.
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
beforeEach(() => { decisionReads = vi.spyOn(api, 'getTaskDecision') })
afterEach(() => {
  doc = docV('First draft')
  hold = null
  sources = []
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => { server.close(); vi.unstubAllGlobals() })

const reads = () => decisionReads.mock.calls.length

function work(t: Task = task()) {
  render(
    <MemoryRouter>
      <HumanWork task={t} meId={9} people={new Map([[9, { id: 9, name: 'Jo Owner', email: 'jo@example.com' }]])}
        plan={{ tasks: [], epics: [], stories: [], loaded: true }} canPlan={false} onEdit={() => {}}
        ticketHref={id => `/project/2/board?focus=${id}`} onChanged={() => {}} />
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

/** Holds the decision read until the returned release is called. */
function holdReads() {
  let release!: () => void
  hold = new Promise(r => { release = r })
  return release
}

describe('HumanWork re-reads its decision doc after a stream reconnect (#2532)', () => {
  it('a connected during the first doc read adds no request', async () => {
    const release = holdReads()
    work()
    await waitFor(() => expect(reads()).toBe(1))
    expect(screen.getByTestId('decision-loading')).toBeInTheDocument()
    // The opening frame of the stream the panel opened, while its read is in flight.
    connect()
    await flush()
    expect(reads()).toBe(1)

    release()
    expect(await screen.findByTestId('decision-card')).toBeInTheDocument()
    await flush()
    expect(reads()).toBe(1)
  })

  it('a connected after the first read has settled re-reads the doc exactly once, and shows what changed', async () => {
    work()
    expect(await screen.findByText('First draft')).toBeInTheDocument()
    expect(reads()).toBe(1)

    // The doc changed while the stream was down; its decision.updated was lost.
    doc = docV('Second draft')
    const release = holdReads()
    connect()
    await flush()
    expect(reads()).toBe(2)
    // The re-read is quiet: the card stays on screen, no loading line.
    expect(screen.getByText('First draft')).toBeInTheDocument()
    expect(screen.queryByTestId('decision-loading')).toBeNull()
    release()
    expect(await screen.findByText('Second draft')).toBeInTheDocument()
    expect(reads()).toBe(2)

    // Each later reconnect re-reads once more.
    connect()
    await flush()
    expect(reads()).toBe(3)
  })

  it('a panel that mounts on a stream another view already holds re-reads on the first connected it sees', async () => {
    // The Board holds the stream and heard its opening frame before the drawer opened.
    const unsubscribe = subscribeSSE('/api/events', () => {})
    try {
      connect()
      work()
      expect(await screen.findByTestId('decision-card')).toBeInTheDocument()
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
    server.use(http.get('*/api/tasks/34/decision', () => HttpResponse.json({ error: 'database unavailable' }, { status: 503 }), { once: true }))
    work()
    expect(await screen.findByTestId('decision-load-error')).toBeInTheDocument()
    expect(reads()).toBe(1)

    connect()
    expect(await screen.findByTestId('decision-card')).toBeInTheDocument()
    expect(screen.queryByTestId('decision-load-error')).toBeNull()
    expect(reads()).toBe(2)
  })

  it('a judgment ticket with no doc yet shows the doc an agent wrote while the stream was down', async () => {
    server.use(http.get('*/api/tasks/34/decision', () => HttpResponse.json({ error: 'no decision doc' }, { status: 404 }), { once: true }))
    work(task({ has_decision_doc: false }))
    // Mark done… shows before the read answers too, so wait for the 404 itself.
    await waitFor(() => expect(reads()).toBe(1))
    await act(() => (decisionReads.mock.results[0].value as Promise<unknown>).catch(() => {}))
    expect(screen.getByRole('button', { name: /mark done/i })).toBeInTheDocument()

    connect()
    expect(await screen.findByTestId('decision-card')).toBeInTheDocument()
    expect(reads()).toBe(2)
  })

  it('ordinary events are not reconnects, and decision.updated for this ticket still re-reads once', async () => {
    work()
    await screen.findByTestId('decision-card')
    send({ type: 'task.updated', project_id: 2, payload: { id: 34, state: 'backlog' } })
    send({ type: 'decision.updated', project_id: 2, payload: { task_id: 99, version: 2 } })
    await flush()
    expect(reads()).toBe(1)

    send({ type: 'decision.updated', project_id: 2, payload: { task_id: 34, version: 2 } })
    await flush()
    expect(reads()).toBe(2)
  })

  it('a person\'s ticket with no doc to read reads nothing, before or after a reconnect', async () => {
    work(task({ has_decision_doc: false, human_reason: 'physical' }))
    await screen.findByTestId('human-work')
    connect()
    await flush()
    expect(reads()).toBe(0)
  })
})
