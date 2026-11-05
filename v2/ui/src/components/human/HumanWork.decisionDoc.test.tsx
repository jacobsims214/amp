// A ticket with a decision doc (has_decision_doc) is decided on its doc: the
// drawer never falls back to Mark done for it, a doc that fails to load shows
// the error with Retry, and only a confirmed "no doc" (404) on a judgment
// ticket shows the J3 Mark done. "Decided upstream" refreshes when a
// dependency's decision changes (final review fixes, #125).
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { HumanWork } from './HumanWork'
import { DecidedUpstream } from './DecidedUpstream'
import type { DecisionDoc, SSEEvent, Task, UpstreamDecision } from '../../types'

class FakeEventSource {
  static instances: FakeEventSource[] = []
  readyState = 1
  onmessage: ((e: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  constructor() { FakeEventSource.instances.push(this) }
  close() { this.readyState = 2 }
  static emit(event: Partial<SSEEvent>) {
    for (const s of FakeEventSource.instances) if (s.readyState !== 2) s.onmessage?.({ data: JSON.stringify(event) })
  }
}

const task = (over: Partial<Task> = {}): Task => ({
  id: 34, project_id: 2, epic_id: 1, story_id: 3, name: 'Confirm: north-star targets', description: 'Pick the metric',
  acceptance_criteria: '- [ ] Metric chosen', state: 'backlog', priority: '1', dependency_ids: [],
  created_at: '2026-11-01T10:00:00Z', updated_at: '2026-11-01T10:00:00Z', ready_at: '2026-11-02T10:00:00Z', owner_user_id: 9,
  performer: 'human', human_reason: 'judgment', ...over,
})
const doc: DecisionDoc = {
  task_id: 34, artifact_id: 7, current_version: 1, author_kind: 'agent',
  ask: { question: 'Which metric?', options: [{ label: 'A', consequence: 'a' }, { label: 'B', consequence: 'b' }], recommended: 1 },
  text: 'Context\n', decided_version: null, chosen_option: null, decided_by: null, recording_started_at: null, decided_at: null, versions: [],
}

let decision: () => Response
let decisionCalls = 0
let upstream: UpstreamDecision[] = []
let upstreamCalls = 0
const server = setupServer(
  http.get('*/api/tasks/:id/decision', () => { decisionCalls++; return decision() }),
  http.get('*/api/tasks/:id/decisions', () => { upstreamCalls++; return HttpResponse.json({ upstream, own: null }) }),
)
beforeAll(() => { vi.stubGlobal('EventSource', FakeEventSource); server.listen({ onUnhandledFrame: 'error' }) })
beforeEach(() => { FakeEventSource.instances = []; decisionCalls = 0; upstreamCalls = 0; upstream = [] })
afterEach(() => server.resetHandlers())
afterAll(() => { server.close(); vi.unstubAllGlobals() })

const plan = (tasks: Task[] = []) => ({ tasks, epics: [], stories: [], loaded: true })
function work(t: Task, tasks: Task[] = []) {
  return render(
    <MemoryRouter>
      <HumanWork task={t} meId={9} people={new Map([[9, { id: 9, name: 'Jo Owner', email: 'jo@example.com' }]])} plan={plan(tasks)}
        canPlan={false} onEdit={() => {}} ticketHref={id => `/project/2/board?focus=${id}`} onChanged={() => {}} />
    </MemoryRouter>,
  )
}

describe('HumanWork with a decision doc', () => {
  it('never offers Mark done while the doc loads, then shows the card with "blocks N · waiting Xd"', async () => {
    let release!: () => void
    const gate = new Promise<void>(r => { release = r })
    decision = () => HttpResponse.json(doc)
    server.use(http.get('*/api/tasks/:id/decision', async () => { decisionCalls++; await gate; return HttpResponse.json(doc) }))
    work(task({ has_decision_doc: true }), [
      task({ id: 41, performer: 'agent', human_reason: undefined, state: 'blocked', dependency_ids: [34] }),
      task({ id: 44, performer: 'agent', human_reason: undefined, state: 'blocked', dependency_ids: [41] }),
    ])
    expect(screen.getByTestId('decision-loading')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /mark done/i })).toBeNull()
    release()
    expect(await screen.findByTestId('decision-card')).toBeInTheDocument()
    expect(screen.getByTestId('decision-wait')).toHaveTextContent(/^blocks 2 · waiting \d+d$/)
    expect(screen.queryByRole('button', { name: /mark done/i })).toBeNull()
  })

  it('a transient load error shows the error with Retry, not Mark done; Retry shows the card', async () => {
    decision = () => HttpResponse.json({ error: 'database unavailable' }, { status: 503 })
    work(task({ has_decision_doc: true }))
    const err = await screen.findByTestId('decision-load-error')
    expect(err).toHaveTextContent("Couldn't load the decision doc: database unavailable.")
    expect(screen.queryByRole('button', { name: /mark done/i })).toBeNull()
    decision = () => HttpResponse.json(doc)
    await userEvent.click(within(err).getByRole('button', { name: 'Retry' }))
    expect(await screen.findByTestId('decision-card')).toBeInTheDocument()
    expect(screen.queryByTestId('decision-load-error')).toBeNull()
  })

  it('a judgment ticket the server says has no doc keeps J3: a confirmed 404 shows Mark done…', async () => {
    decision = () => HttpResponse.json({ error: 'ticket 34 has no decision doc' }, { status: 404 })
    work(task())
    await waitFor(() => expect(decisionCalls).toBe(1))
    expect(screen.getByRole('button', { name: /mark done/i })).toBeInTheDocument()
    expect(screen.queryByTestId('decision-load-error')).toBeNull()
  })

  it('a judgment ticket whose doc read fails shows the error, not Mark done', async () => {
    decision = () => HttpResponse.json({ error: 'timeout' }, { status: 500 })
    work(task())
    expect(await screen.findByTestId('decision-load-error')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /mark done/i })).toBeNull()
  })
})

describe('DecidedUpstream', () => {
  const strip = (deps: number[]) => render(
    <MemoryRouter>
      <DecidedUpstream taskId={41} projectId={2} dependencyIds={deps} ticketHref={id => `/project/2/board?focus=${id}`} />
    </MemoryRouter>,
  )

  it('reloads when a dependency\'s decision changes, and only then', async () => {
    strip([34])
    await waitFor(() => expect(upstreamCalls).toBe(1))
    expect(screen.queryByTestId('decided-upstream')).toBeNull()
    upstream = [{ task_id: 34, name: 'Confirm', human_reason: 'judgment', option: 'B: Activation', decision: 'B: Activation — Go with B',
      decided_by: 9, decided_by_name: 'Jacob', decided_at: '2026-11-04T10:00:00Z' }]
    act(() => FakeEventSource.emit({ type: 'decision.updated', project_id: 2, payload: { task_id: 99, version: 2 } }))
    act(() => FakeEventSource.emit({ type: 'task.updated', project_id: 2, payload: { task_id: 34 } }))
    await new Promise(r => setTimeout(r, 20))
    expect(upstreamCalls).toBe(1)
    act(() => FakeEventSource.emit({ type: 'decision.updated', project_id: 2, payload: { task_id: 34, version: 2 } }))
    const shown = await screen.findByTestId('decided-upstream')
    expect(upstreamCalls).toBe(2)
    // The label once, then the decided text: "Decided by Jacob on #34: B — …".
    expect(shown).toHaveTextContent('Decided by Jacob on #34: B: Activation — Go with B')
    expect(shown.textContent).not.toContain('B: Activation — B: Activation')
  })

  it('says the label alone when the decision only repeats it (Accept recommendation)', async () => {
    upstream = [{ task_id: 34, name: 'Confirm', human_reason: 'judgment', option: 'B: Activation', decision: 'B: Activation',
      decided_by: 9, decided_by_name: 'Jacob', decided_at: '2026-11-04T10:00:00Z' }]
    strip([34])
    const shown = await screen.findByTestId('decided-upstream')
    expect(within(shown).getByTestId('decided-upstream-item').textContent).toBe('Decided by Jacob on #34: B: Activation')
  })
})
