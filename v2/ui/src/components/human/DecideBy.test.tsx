// DecideBy (#146): a person's ticket's decide-by date on its decision card:
// relative and absolute, red once overdue; its person sets, changes or
// removes it.
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DecideBy } from './DecideBy'
import type { Task } from '../../types'

const { update } = vi.hoisted(() => ({ update: vi.fn() }))
vi.mock('../../api/client', () => ({ api: { updateTask: update } }))
beforeEach(() => { update.mockReset() })

const now = () => Date.parse('2026-11-05T12:00:00Z')
const task = (over: Partial<Task> = {}): Task => ({
  id: 34, project_id: 2, epic_id: 1, story_id: 1, name: 'Confirm: north-star targets', description: 'd', acceptance_criteria: 'a',
  state: 'backlog', priority: '1', dependency_ids: [], created_at: '2026-11-02T10:00:00Z', updated_at: '2026-11-02T10:00:00Z',
  owner_user_id: 9, performer: 'human', human_reason: 'judgment', ...over,
})

describe('DecideBy', () => {
  it('shows a future date relative and absolute, not overdue', () => {
    render(<DecideBy task={task({ decide_by: '2026-11-07T12:00:00Z' })} canEdit={false} now={now} />)
    const line = screen.getByTestId('decide-by')
    expect(line).toHaveTextContent(/Decide by .+ · in 2d/)
    expect(line.querySelector('time')).toHaveAttribute('datetime', '2026-11-07T12:00:00Z')
    expect(line.querySelector('[data-overdue]')).toBeNull()
    expect(screen.queryByRole('button')).toBeNull() // not the person's: read-only
  })

  it('is red once overdue', () => {
    render(<DecideBy task={task({ decide_by: '2026-11-05T09:00:00Z' })} canEdit={false} now={now} />)
    expect(screen.getByText(/3h overdue/)).toHaveAttribute('data-overdue', 'true')
  })

  it('shows nothing to someone who cannot set one when there is none', () => {
    const { container } = render(<DecideBy task={task()} canEdit={false} now={now} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('sets a date as the instant of the reader\'s wall-clock time', async () => {
    const user = userEvent.setup()
    const changed = vi.fn()
    update.mockResolvedValue(task({ decide_by: '2026-11-12T17:00:00Z' }))
    render(<DecideBy task={task()} canEdit now={now} onChanged={changed} />)
    expect(screen.getByText('No decide-by date')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Set a date' }))
    const input = screen.getByLabelText('Decide by')
    await user.clear(input)
    await user.type(input, '2026-11-12T17:00')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    const sent = update.mock.calls[0][1].decide_by as string
    expect(update).toHaveBeenCalledWith(34, { decide_by: sent })
    expect(new Date(sent).getHours()).toBe(17)
    expect(new Date(sent).getDate()).toBe(12)
    expect(changed).toHaveBeenCalled()
  })

  it('removes the date with a clear', async () => {
    const user = userEvent.setup()
    update.mockResolvedValue(task())
    render(<DecideBy task={task({ decide_by: '2026-11-07T12:00:00Z' })} canEdit now={now} />)
    await user.click(screen.getByRole('button', { name: 'Remove' }))
    expect(update).toHaveBeenCalledWith(34, { clear: ['decide_by'] })
  })

  it('keeps the editor open and says why when the server refuses', async () => {
    const user = userEvent.setup()
    update.mockRejectedValue(new Error('decide_by is for a person\'s ticket'))
    render(<DecideBy task={task({ decide_by: '2026-11-07T12:00:00Z' })} canEdit now={now} />)
    await user.click(screen.getByRole('button', { name: 'Change' }))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByRole('alert')).toHaveTextContent("decide_by is for a person's ticket")
    expect(screen.getByTestId('decide-by-editor')).toBeInTheDocument()
  })
})
