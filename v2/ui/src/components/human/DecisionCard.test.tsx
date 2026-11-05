// DecisionCard (docs/specs/human-decision-docs.md §5): the recommended badge,
// Accept recommendation, Something else, Mark done's note, and Decided means.
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
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

const { done } = vi.hoisted(() => ({ done: vi.fn() }))
vi.mock('../../api/client', () => ({ api: { completeHumanTask: done, startHumanTask: vi.fn() } }))
beforeEach(() => { done.mockReset(); done.mockResolvedValue(task({ state: 'completed' })) })

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
    expect(done).not.toHaveBeenCalled() // the person still clicks Mark done
  })

  it('Mark done sends "<option label> — <decision text>"', async () => {
    const onChanged = vi.fn()
    render(<DecisionCard task={task()} doc={doc} canAct onChanged={onChanged} />)
    await userEvent.click(screen.getAllByRole('radio')[1])
    await userEvent.type(screen.getByLabelText('Your decision'), 'Go with B; keep trial at 14 days')
    await userEvent.click(screen.getByRole('button', { name: /mark done/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(done).toHaveBeenCalledWith(34, { note: 'B: Activation rate (40%, floor 30%) — Go with B; keep trial at 14 days' })
  })

  it('Something else sends the decision text alone', async () => {
    const onChanged = vi.fn()
    render(<DecisionCard task={task()} doc={doc} canAct onChanged={onChanged} />)
    await userEvent.click(screen.getByRole('radio', { name: 'Something else' }))
    await userEvent.type(screen.getByLabelText('Your decision'), 'Retention, but measured weekly')
    await userEvent.click(screen.getByRole('button', { name: /mark done/i }))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(done).toHaveBeenCalledWith(34, { note: 'Retention, but measured weekly' })
  })

  it('needs an option and the decision text before Mark done', async () => {
    render(<DecisionCard task={task()} doc={doc} canAct />)
    await userEvent.type(screen.getByLabelText('Your decision'), 'B')
    await userEvent.click(screen.getByRole('button', { name: /mark done/i }))
    expect(screen.getByRole('alert')).toHaveTextContent('Pick an option, or Something else')
    await userEvent.click(screen.getAllByRole('radio')[0])
    await userEvent.clear(screen.getByLabelText('Your decision'))
    await userEvent.click(screen.getByRole('button', { name: /mark done/i }))
    expect(screen.getByRole('alert')).toHaveTextContent('Write what you decided')
    expect(done).not.toHaveBeenCalled()
  })

  it('is read-only for someone else, and says who decides', () => {
    render(<DecisionCard task={task()} doc={doc} canAct={false} ownerName="Jacob" />)
    expect(screen.queryByRole('button', { name: /mark done/i })).toBeNull()
    for (const r of screen.getAllByRole('radio')) expect(r).toBeDisabled()
    expect(screen.getByText('Only Jacob decides this.')).toBeInTheDocument()
  })
})
