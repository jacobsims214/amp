// DecisionCard (docs/specs/human-decision-docs.md §5): the recommended badge,
// Accept recommendation, Something else, Decided means, Record decision, Edit
// doc (a 409 keeps the draft), provenance marks, and the read-only state.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, useState } from 'react'
import { DecisionCard } from './DecisionCard'
import type { DecisionDoc, Task } from '../../types'

const task = (over: Partial<Task> = {}): Task => ({
  id: 34, project_id: 2, epic_id: 1, story_id: 1, name: 'Confirm: north-star targets', description: 'Pick the metric',
  acceptance_criteria: '- [ ] Metric chosen\n- [ ] Target and floor set\n- [ ] Trial length confirmed', state: 'backlog',
  priority: '1', dependency_ids: [], created_at: '2026-11-02T10:00:00Z', updated_at: '2026-11-02T10:00:00Z', owner_user_id: 9,
  performer: 'human', human_reason: 'judgment', ...over,
})

const doc: DecisionDoc = {
  task_id: 34, artifact_id: 7, current_version: 2, author_kind: 'agent',
  ask: {
    question: 'Which north-star metric do we commit to for the next 90 days?',
    options: [
      { label: 'A: Weekly active teams', consequence: 'Easy to measure; rewards seats over value' },
      { label: 'B: Activation rate (40%, floor 30%)', consequence: 'Needs the onboarding event (#41)' },
      { label: 'C: Net revenue retention', consequence: 'Lags 90 days' },
    ],
    recommended: 1,
  },
  text: '## Why we recommend B: Activation rate (40%, floor 30%)\n\nB is the only one we can move.\n',
  decided_version: null, chosen_option: null, decided_by: null, recording_started_at: null, decided_at: null,
}

const { done, save, getTask, getDoc } = vi.hoisted(() => ({ done: vi.fn(), save: vi.fn(), getTask: vi.fn(), getDoc: vi.fn() }))
vi.mock('../../api/client', () => ({ api: { recordDecision: done, saveDecisionDoc: save, getTask, getTaskDecision: getDoc, startHumanTask: vi.fn() } }))
beforeEach(() => {
  done.mockReset(); save.mockReset(); getTask.mockReset(); getDoc.mockReset()
  done.mockResolvedValue({ ...doc, decided_version: 2, chosen_option: 1, decided_at: '2026-11-04T12:00:00Z' })
  getTask.mockResolvedValue(task({ state: 'completed' }))
})

const recorded: DecisionDoc = {
  ...doc, decided_version: 2, chosen_option: 1, decision_text: 'Go with B; keep trial at 14 days', decided_by: 9,
  decided_by_name: 'Jacob', recording_started_at: '2026-11-04T12:00:00Z', decided_at: '2026-11-04T12:00:05Z',
}

describe('DecisionCard', () => {
  it('shows the question and options, with the badge on the recommended one and nothing chosen', () => {
    render(<DecisionCard task={task()} doc={doc} canAct />)
    expect(screen.getByTestId('decision-question')).toHaveTextContent(doc.ask!.question)
    const options = screen.getAllByTestId('option')
    expect(options).toHaveLength(3)
    expect(within(options[1]).getByTestId('recommended-badge')).toHaveTextContent('Recommended')
    expect(screen.getAllByTestId('recommended-badge')).toHaveLength(1)
    // The recommendation never pre-selects.
    for (const r of screen.getAllByRole('radio')) expect(r).not.toBeChecked()
    expect(screen.getByLabelText('Something else')).toBeInTheDocument()
    expect(screen.getByTestId('decision-meta')).toHaveTextContent('drafted by an agent · v2')
  })

  it('lists the acceptance criteria as Decided means', () => {
    render(<DecisionCard task={task()} doc={doc} canAct />)
    const items = screen.getAllByTestId('decided-means-item').map(li => li.textContent)
    expect(items).toEqual(['Metric chosen', 'Target and floor set', 'Trial length confirmed'])
  })

  it('Accept recommendation picks the recommended option and fills the decision with its label', async () => {
    render(<DecisionCard task={task()} doc={doc} canAct />)
    await userEvent.click(screen.getByRole('button', { name: /accept recommendation/i }))
    expect(screen.getAllByRole('radio')[1]).toBeChecked()
    expect(screen.getByLabelText('Your decision')).toHaveValue('B: Activation rate (40%, floor 30%)')
    expect(done).not.toHaveBeenCalled() // the person still clicks Record decision
  })

  it('Record decision sends the current version, the chosen option and the text', async () => {
    const onChanged = vi.fn(), onDocChanged = vi.fn()
    render(<DecisionCard task={task()} doc={doc} canAct onChanged={onChanged} onDocChanged={onDocChanged} />)
    expect(screen.queryByRole('button', { name: /mark done/i })).toBeNull()
    await userEvent.click(screen.getAllByRole('radio')[1])
    await userEvent.type(screen.getByLabelText('Your decision'), 'Go with B; keep trial at 14 days')
    await userEvent.click(screen.getByRole('button', { name: /record decision/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalledWith(expect.objectContaining({ state: 'completed' })))
    expect(done).toHaveBeenCalledWith(34, { version: 2, chosen_option: 1, decision_text: 'Go with B; keep trial at 14 days' })
    expect(onDocChanged).toHaveBeenCalledWith(expect.objectContaining({ decided_version: 2 }))
  })

  it('Something else records no option, the text alone', async () => {
    const onChanged = vi.fn()
    render(<DecisionCard task={task()} doc={doc} canAct onChanged={onChanged} />)
    await userEvent.click(screen.getByRole('radio', { name: 'Something else' }))
    await userEvent.type(screen.getByLabelText('Your decision'), 'Retention, but measured weekly')
    await userEvent.click(screen.getByRole('button', { name: /record decision/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(done).toHaveBeenCalledWith(34, { version: 2, chosen_option: null, decision_text: 'Retention, but measured weekly' })
  })

  it('needs an option and the decision text before Record decision', async () => {
    render(<DecisionCard task={task()} doc={doc} canAct />)
    await userEvent.type(screen.getByLabelText('Your decision'), 'B')
    await userEvent.click(screen.getByRole('button', { name: /record decision/i }))
    expect(screen.getByRole('alert')).toHaveTextContent('Pick an option, or Something else')
    await userEvent.click(screen.getAllByRole('radio')[0])
    await userEvent.clear(screen.getByLabelText('Your decision'))
    await userEvent.click(screen.getByRole('button', { name: /record decision/i }))
    expect(screen.getByRole('alert')).toHaveTextContent('Write what you decided')
    expect(done).not.toHaveBeenCalled()
  })

  it('Edit doc saves the markdown and the options as one version on the current base', async () => {
    const onDocChanged = vi.fn()
    save.mockResolvedValue({ ...doc, current_version: 3 })
    render(<DecisionCard task={task()} doc={doc} canAct onDocChanged={onDocChanged} />)
    await userEvent.click(screen.getByRole('button', { name: /edit doc/i }))
    const label = screen.getByLabelText('Option 2 label')
    await userEvent.clear(label)
    await userEvent.type(label, 'B: Activation rate (45%, floor 30%)')
    await userEvent.type(screen.getByLabelText('Doc text'), 'Trial stays at 14 days.')
    await userEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await waitFor(() => expect(onDocChanged).toHaveBeenCalled())
    const [id, body] = save.mock.calls[0]
    expect(id).toBe(34)
    expect(body.base_version).toBe(2)
    expect(body.content).toBe(doc.text + 'Trial stays at 14 days.')
    expect(body.ask.options[1]).toEqual({ label: 'B: Activation rate (45%, floor 30%)', consequence: 'Needs the onboarding event (#41)' })
    expect(body.ask.recommended).toBe(1)
    expect(screen.queryByTestId('decision-editor')).toBeNull()
  })

  it('a 409 on save keeps the draft and says the doc changed', async () => {
    save.mockRejectedValue(Object.assign(new Error('The doc changed in another tab. Reload to see it'), { status: 409 }))
    render(<DecisionCard task={task()} doc={doc} canAct />)
    await userEvent.click(screen.getByRole('button', { name: /edit doc/i }))
    await userEvent.type(screen.getByLabelText('Doc text'), 'My careful edit.')
    await userEvent.click(screen.getByRole('button', { name: /^save$/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('The doc changed in another tab. Reload to see it')
    expect(screen.getByLabelText('Doc text')).toHaveValue(doc.text + 'My careful edit.')
  })

  it('the option editor keeps 2 to 4 options and checks them before saving', async () => {
    render(<DecisionCard task={task()} doc={doc} canAct />)
    await userEvent.click(screen.getByRole('button', { name: /edit doc/i }))
    await userEvent.click(screen.getByRole('button', { name: /add option/i }))
    expect(screen.getAllByTestId('option-row')).toHaveLength(4)
    expect(screen.queryByRole('button', { name: /add option/i })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: /^save$/i }))
    expect(screen.getByRole('alert')).toHaveTextContent('Option 4 needs a label')
    await userEvent.click(screen.getByRole('button', { name: 'Remove option 4' }))
    await userEvent.click(screen.getByRole('button', { name: 'Remove option 3' }))
    expect(screen.getByRole('button', { name: 'Remove option 2' })).toBeDisabled()
    expect(save).not.toHaveBeenCalled()
  })

  it('marks the agent\'s lines and options, not the owner\'s', () => {
    const opts = doc.ask!
    const v1 = { version: 1, author_kind: 'agent' as const, text: 'Agent context.\n\nAgent reason.', ask: opts, created_by: 1, created_at: '' }
    const v2 = {
      version: 2, author_kind: 'person' as const, text: 'Agent context.\n\nOwner addition.', created_by: 9, created_at: '',
      ask: { ...opts, options: opts.options.map((o, i) => i === 1 ? { ...o, label: 'B: tighter' } : o) },
    }
    render(<DecisionCard task={task()} doc={{ ...doc, text: v2.text, ask: v2.ask, author_kind: 'person', versions: [v1, v2] }} canAct />)
    const runs = screen.getAllByTestId('doc-run')
    expect(runs.map(r => [r.dataset.author, r.textContent?.replace(/Agent$/, '').trim()])).toEqual([['agent', 'Agent context.'], ['person', 'Owner addition.']])
    expect(runs[0]).toHaveAttribute('title', 'Agent')
    expect(screen.getAllByTestId('option').map(o => o.dataset.author)).toEqual(['agent', 'person', 'agent'])
  })

  it('is read-only after the decision is recorded', () => {
    render(<DecisionCard task={task({ state: 'completed' })} doc={recorded} canAct />)
    expect(screen.getByTestId('decision-recorded')).toHaveTextContent('Decided by Jacob · v2')
    expect(screen.getByTestId('decision-recorded')).toHaveTextContent('Go with B; keep trial at 14 days')
    expect(screen.queryByRole('button', { name: /edit doc/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /record decision/i })).toBeNull()
    expect(screen.queryByLabelText('Your decision')).toBeNull()
    for (const r of screen.getAllByRole('radio')) expect(r).toBeDisabled()
    expect(screen.getAllByRole('radio')[1]).toBeChecked()
  })

  it('a record that did not finish is retried with the stored values once its claim is a minute old', async () => {
    const claimed = { ...recorded, decided_at: null, recording_started_at: '2026-11-04T12:00:00Z' }
    const { unmount } = render(<DecisionCard task={task()} doc={claimed} canAct now={() => Date.parse('2026-11-04T12:00:30Z')} />)
    expect(screen.getByTestId('decision-recording')).toHaveTextContent('Being recorded')
    expect(screen.queryByRole('button', { name: /retry/i })).toBeNull()
    unmount()
    render(<DecisionCard task={task()} doc={claimed} canAct now={() => Date.parse('2026-11-04T12:02:00Z')} />)
    expect(screen.getByTestId('decision-recording')).toHaveTextContent("Recording didn't finish.")
    await userEvent.click(screen.getByRole('button', { name: /retry/i }))
    expect(done).toHaveBeenCalledWith(34, { version: 2, chosen_option: 1, decision_text: 'Go with B; keep trial at 14 days' })
  })

  describe('a record that stops after its claim', () => {
    beforeEach(() => { vi.useFakeTimers({ shouldAdvanceTime: true }) })
    afterEach(() => { vi.useRealTimers() })

    it('reloads into "Being recorded", then offers Retry once the claim is a minute old', async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
      done.mockRejectedValueOnce(Object.assign(new Error("recording didn't finish (disk on fire); retry in a minute"), { status: 500 }))
      getDoc.mockImplementation(async () => ({
        ...doc, decided_version: 2, chosen_option: 1, decision_text: 'Go with B', decided_by: 9,
        recording_started_at: new Date(Date.now()).toISOString(), decided_at: null,
      }))
      function Harness() {
        const [d, setD] = useState(doc)
        return <DecisionCard task={task()} doc={d} canAct onDocChanged={setD} />
      }
      render(<Harness />)
      await user.click(screen.getAllByRole('radio')[1])
      await user.type(screen.getByLabelText('Your decision'), 'Go with B')
      await user.click(screen.getByRole('button', { name: /record decision/i }))
      expect(await screen.findByTestId('decision-recording')).toHaveTextContent('Being recorded')
      expect(screen.queryByLabelText('Your decision')).toBeNull()
      expect(screen.queryByRole('button', { name: /retry/i })).toBeNull()
      await act(async () => { vi.advanceTimersByTime(61_000) })
      expect(screen.getByTestId('decision-recording')).toHaveTextContent("Recording didn't finish.")
      done.mockResolvedValueOnce({ ...doc, decided_version: 2, chosen_option: 1, decided_at: new Date().toISOString() })
      await user.click(screen.getByRole('button', { name: /retry/i }))
      expect(done).toHaveBeenLastCalledWith(34, { version: 2, chosen_option: 1, decision_text: 'Go with B' })
    })
  })

  it('is read-only for someone else, and says who decides', () => {
    render(<DecisionCard task={task()} doc={doc} canAct={false} ownerName="Jacob" />)
    expect(screen.queryByRole('button', { name: /record decision/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /edit doc/i })).toBeNull()
    for (const r of screen.getAllByRole('radio')) expect(r).toBeDisabled()
    expect(screen.getByText('Only Jacob decides this.')).toBeInTheDocument()
  })

  it('says "blocks N · waiting Xd" in its header (J1), and "Nothing waits on it" with none', () => {
    const now = () => Date.parse('2026-11-04T10:00:00Z')
    const { rerender } = render(<DecisionCard task={task()} doc={doc} canAct blocks={5} waitingSince="2026-11-02T10:00:00Z" now={now} />)
    expect(screen.getByTestId('decision-wait')).toHaveTextContent(/^blocks 5 · waiting 2d$/)
    rerender(<DecisionCard task={task()} doc={doc} canAct blocks={0} waitingSince="2026-11-02T10:00:00Z" now={now} />)
    expect(screen.getByTestId('decision-wait')).toHaveTextContent(/^Nothing waits on it · waiting 2d$/)
    // Not known yet (the plan is loading), and not on a done ticket.
    rerender(<DecisionCard task={task()} doc={doc} canAct waitingSince="2026-11-02T10:00:00Z" now={now} />)
    expect(screen.queryByTestId('decision-wait')).toBeNull()
    rerender(<DecisionCard task={task({ state: 'completed' })} doc={recorded} canAct blocks={5} now={now} />)
    expect(screen.queryByTestId('decision-wait')).toBeNull()
  })

  it('offers Edit doc while the ticket waits on its dependencies; recording waits for them', async () => {
    save.mockResolvedValue({ ...doc, current_version: 3, versions: [] })
    render(<DecisionCard task={task({ state: 'blocked', dependency_ids: [12], blocked_by_ids: [12] })} doc={doc} canAct />)
    expect(screen.getByText('Waiting on #12 before it can start.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /record decision/i })).toBeNull()
    for (const r of screen.getAllByRole('radio')) expect(r).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: /edit doc/i }))
    expect(screen.getByTestId('decision-editor')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(save).toHaveBeenCalledWith(34, expect.objectContaining({ base_version: 2 }))
  })

  it('a blocked ticket offers no Edit doc to someone else, or once the record is claimed', () => {
    const blocked = task({ state: 'blocked', dependency_ids: [12], blocked_by_ids: [12] })
    const { rerender } = render(<DecisionCard task={blocked} doc={doc} canAct={false} />)
    expect(screen.queryByRole('button', { name: /edit doc/i })).toBeNull()
    rerender(<DecisionCard task={blocked} doc={{ ...doc, decided_version: 2, chosen_option: 1, recording_started_at: new Date().toISOString() }} canAct />)
    expect(screen.queryByRole('button', { name: /edit doc/i })).toBeNull()
  })

  it('clears the picked option when another tab saves a new version', async () => {
    const { rerender } = render(<DecisionCard task={task()} doc={doc} canAct />)
    await userEvent.click(screen.getAllByRole('radio')[1])
    expect(screen.getAllByRole('radio')[1]).toBeChecked()
    // v3 drops option A: index 1 is now C. The pick is gone, not moved.
    const v3: DecisionDoc = { ...doc, current_version: 3, ask: { ...doc.ask!, options: doc.ask!.options.slice(1), recommended: 0 } }
    rerender(<DecisionCard task={task()} doc={v3} canAct />)
    for (const r of screen.getAllByRole('radio')) expect(r).not.toBeChecked()
    await userEvent.type(screen.getByLabelText('Your decision'), 'Go')
    await userEvent.click(screen.getByRole('button', { name: /record decision/i }))
    expect(done).not.toHaveBeenCalled()
    expect(screen.getByRole('alert')).toHaveTextContent('Pick an option, or Something else')
  })
})
