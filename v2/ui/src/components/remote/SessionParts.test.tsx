import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Composer, WorkerChips } from './SessionParts'
import type { WorkerInfo, WorkerState } from '../../remote/types'

const NOW = 1_000_000_000_000

/** A worker as the harness sends it (protocol 6.4); `ended_at_ms` is optional on older ones. */
function worker(task_id: number, state: WorkerState, extra: Partial<WorkerInfo> = {}): WorkerInfo {
  return {
    task_id, name: `Ticket ${task_id}`, role: 'amp:amp-worker-backend', state, model: 'opus',
    started_at_ms: NOW - 120_000, ended_at_ms: null, ...extra,
  }
}

const renderRoster = (workers: WorkerInfo[]) => render(<WorkerChips workers={workers} now={NOW} />)
const summary = () => screen.getByTestId('roster-summary')
const counts = () => within(screen.getByTestId('roster-counts')).getAllByTestId('roster-count')
const defaultChipIds = () => within(screen.getByLabelText('Workers')).queryAllByTestId('worker-chip').map(c => c.dataset.ticket)

describe('WorkerChips: the default roster is one summary line (#4895)', () => {
  it('counts every state, and keeps rows only for running and blocked workers', () => {
    renderRoster([
      worker(1, 'running'), worker(2, 'running'), worker(3, 'running'),
      worker(4, 'blocked'),
      worker(5, 'done', { ended_at_ms: NOW - 1_000 }),
      worker(6, 'failed', { ended_at_ms: NOW - 2_000 }),
      worker(7, 'stopped', { ended_at_ms: NOW - 3_000 }),
    ])
    // One line, live states first: 3 running · 1 blocked · 1 failed · 1 stopped · 1 done.
    expect(counts().map(c => [c.dataset.state, c.textContent])).toEqual([
      ['running', '3 running'], ['blocked', ' · 1 blocked'], ['failed', ' · 1 failed'], ['stopped', ' · 1 stopped'], ['done', ' · 1 done'],
    ])
    expect(summary()).toHaveAttribute('data-worst', 'blocked')
    // The default rows are the live workers only — running and blocked. The ended ones are counted, not listed.
    expect(defaultChipIds()).toEqual(['4', '1', '2', '3'])
  })

  it('an ended worker does not appear in the default rows, however many of them there are', () => {
    const finished: WorkerInfo[] = Array.from({ length: 8 }, (_, i) => worker(100 + i, 'done', { ended_at_ms: NOW - i * 1_000 }))
    renderRoster([...finished, worker(1, 'running')])
    const roster = screen.getByLabelText('Workers')
    expect(within(roster).getAllByTestId('worker-chip')).toHaveLength(1)
    expect(defaultChipIds()).toEqual(['1'])
    for (const w of finished) expect(within(roster).queryByText(`#${w.task_id}`)).toBeNull()
    expect(counts().map(c => c.textContent)).toEqual(['1 running', ' · 8 done'])
  })

  it('a blocked worker stays visible: it is live, waiting on a person', () => {
    renderRoster([worker(1, 'running'), worker(2, 'blocked')])
    expect(defaultChipIds()).toEqual(['2', '1']) // blocked first: it waits on a person
    expect(within(screen.getByLabelText('Workers')).getAllByTestId('worker-state').map(s => s.textContent))
      .toEqual(['· blocked', '· running 2m00s'])
  })

  it('colours the summary line with the worst state present', () => {
    const { rerender } = render(<WorkerChips workers={[worker(1, 'running')]} now={NOW} />)
    expect(summary()).toHaveAttribute('data-worst', 'running')
    // A failed worker outranks a running one; a blocked worker outranks them all.
    rerender(<WorkerChips workers={[worker(1, 'running'), worker(2, 'failed', { ended_at_ms: NOW - 1 })]} now={NOW} />)
    expect(summary()).toHaveAttribute('data-worst', 'failed')
    rerender(<WorkerChips workers={[worker(1, 'running'), worker(2, 'blocked'), worker(3, 'failed')]} now={NOW} />)
    expect(summary()).toHaveAttribute('data-worst', 'blocked')
  })

  it('renders nothing at all when there are no workers', () => {
    const { container } = renderRoster([])
    expect(container).toBeEmptyDOMElement()
  })

  it('works with the counts of a hand-built list: only some states present', () => {
    renderRoster([worker(1, 'stopped'), worker(2, 'done', { ended_at_ms: NOW })])
    expect(counts().map(c => c.dataset.state)).toEqual(['stopped', 'done'])
    expect(defaultChipIds()).toEqual([])
  })})

describe('WorkerChips: the sheet holds the full roster (#4895)', () => {
  const everyone = [
    worker(1, 'running'), worker(2, 'blocked'),
    worker(3, 'done', { ended_at_ms: NOW - 60_000 }), worker(4, 'stopped', { ended_at_ms: NOW - 90_000 }),
    worker(5, 'failed', { ended_at_ms: NOW - 30_000 }),
  ]

  it('opens from the summary and lists ended workers with their state and elapsed time', async () => {
    const user = userEvent.setup()
    renderRoster(everyone)
    expect(screen.queryByTestId('roster-sheet')).toBeNull()
    await user.click(summary())
    const sheet = screen.getByTestId('roster-sheet')
    expect(summary()).toHaveAttribute('aria-expanded', 'true')
    // Live first, then the most recently finished.
    expect(within(sheet).getAllByTestId('roster-row').map(r => r.dataset.ticket)).toEqual(['2', '1', '5', '3', '4'])
    const done = within(sheet).getAllByTestId('roster-row').find(r => r.dataset.ticket === '3')!
    expect(within(done).getByTestId('worker-state')).toHaveTextContent('completed after 1m00s')
    const stopped = within(sheet).getAllByTestId('roster-row').find(r => r.dataset.ticket === '4')!
    expect(within(stopped).getByTestId('worker-state')).toHaveTextContent('stopped after 30s')
    expect(within(sheet).getByTestId('roster-sheet-counts')).toHaveTextContent('1 running · 1 blocked · 1 failed · 1 stopped · 1 done')
  })

  it('closes on the scrim', async () => {
    const user = userEvent.setup()
    renderRoster(everyone)
    await user.click(summary())
    await user.click(screen.getByTestId('roster-scrim'))
    expect(screen.queryByTestId('roster-sheet')).toBeNull()
    expect(summary()).toHaveAttribute('aria-expanded', 'false')
  })

  it('closes on Escape', async () => {
    const user = userEvent.setup()
    renderRoster(everyone)
    await user.click(summary())
    await user.keyboard('{Escape}')
    expect(screen.queryByTestId('roster-sheet')).toBeNull()
  })

  it('closes on the close control', async () => {
    const user = userEvent.setup()
    renderRoster(everyone)
    await user.click(summary())
    await user.click(within(screen.getByTestId('roster-sheet')).getByRole('button', { name: 'Close roster' }))
    expect(screen.queryByTestId('roster-sheet')).toBeNull()
  })

  it('is a dialog named for a reader, with the summary as its opener', async () => {
    const user = userEvent.setup()
    renderRoster(everyone)
    expect(summary()).toHaveAccessibleName(/1 running.*1 blocked/) // the counts are the button's name
    expect(summary()).toHaveAttribute('aria-expanded', 'false')
    await user.click(summary())
    expect(screen.getByRole('dialog', { name: 'All workers' })).toBe(screen.getByTestId('roster-sheet'))
    expect(summary()).toHaveAttribute('aria-expanded', 'true')
    expect(summary()).toHaveAttribute('aria-controls', screen.getByTestId('roster-sheet').id)
  })

  it('orders the finished workers without ended_at_ms, as an older harness sends them', async () => {
    const user = userEvent.setup()
    renderRoster([
      worker(1, 'done', { started_at_ms: null }),
      worker(2, 'stopped', { started_at_ms: null, ended_at_ms: null }),
      worker(3, 'running'),
    ])
    await user.click(summary())
    const rows = within(screen.getByTestId('roster-sheet')).getAllByTestId('roster-row')
    // The running worker first; the two the harness sent no times for still both appear.
    expect(rows.map(r => r.dataset.ticket)).toEqual(['3', '1', '2'])
    // With no start it can still say the state, just not how long it ran.
    expect(within(rows[1]).getByTestId('worker-state')).toHaveTextContent('· completed')
    expect(within(rows[2]).getByTestId('worker-state')).toHaveTextContent('· stopped')
  })
})

describe('Composer: Send and Stop in one row, a fixed box (#4896)', () => {
  const renderComposer = (props: Partial<Parameters<typeof Composer>[0]> = {}) => {
    const onSend = vi.fn(), onStop = vi.fn()
    const view = render(<Composer disabled={false} disabledReason={null} busy={false} online onSend={onSend} onStop={onStop} {...props} />)
    return { ...view, onSend, onStop }
  }
  const form = () => screen.getByTestId('composer').querySelector('form')!

  it('puts Stop inside the composer row, beside Send, never as a full-width bar', () => {
    const { container } = renderComposer()
    const row = form()
    const stop = within(row).getByRole('button', { name: 'Stop the agent' })
    expect(row).toContainElement(stop)
    expect(stop).toHaveAttribute('data-testid', 'stop-inline')
    // Not the old full-width bar, and not the same box as Send: a 42px square.
    expect(stop).not.toHaveClass('w-full')
    expect(stop).toHaveClass('w-[42px]', 'h-[42px]')
    // Send comes first in the row; bar-style buttons elsewhere would have `w-full`.
    const send = within(row).getByRole('button', { name: 'Send' })
    expect(send.compareDocumentPosition(stop) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect([...container.querySelectorAll('button[aria-label="Stop the agent"]')].filter(b => b.className.includes('w-full'))).toEqual([])
    // Only one Stop on the page: no second, larger copy survives below the row.
    expect(container.querySelectorAll('button[aria-label="Stop the agent"]')).toHaveLength(1)
  })

  it('keeps Stop enabled while a run is in flight (R-M7), and only offline disables it', () => {
    const { rerender, onStop } = renderComposer({ busy: true })
    const stop = within(form()).getByRole('button', { name: 'Stop the agent' })
    expect(stop).toBeEnabled()
    stop.click()
    expect(onStop).toHaveBeenCalledTimes(1)
    // Offline is the one state that can reach the laptop no longer.
    rerender(<Composer disabled={false} disabledReason={null} busy online={false} onSend={vi.fn()} onStop={onStop} />)
    expect(within(form()).getByRole('button', { name: 'Stop the agent' })).toBeDisabled()
  })

  it('cannot be resized by the user: the only resize utility is resize-none', () => {
    renderComposer()
    const box = screen.getByLabelText('Message')
    expect(box).toHaveClass('resize-none')
    // No `resize-y`/`resize-x`/`resize` anywhere on it, whatever the width.
    expect(box.className.split(/\s+/).filter(c => /^resize(-x|-y)?$/.test(c))).toEqual([])
    // It grows with the text instead (`field-sizing-content`), capped rather than draggable.
    expect(box).toHaveClass('field-sizing-content')
  })

  it('says one line above the row why a message will queue, and nothing when it will send', () => {
    const { rerender } = renderComposer()
    expect(screen.queryByTestId('composer-reason')).toBeNull()

    rerender(<Composer disabled={false} disabledReason={null} busy queued={2} online onSend={vi.fn()} onStop={vi.fn()} />)
    const reason = screen.getByTestId('composer-reason')
    expect(reason).toHaveTextContent('Agent busy — your message joins the queue (2 ahead).')
    // Above the row, not inside it.
    expect(form()).not.toContainElement(reason)
    expect(within(form()).getByRole('button', { name: 'Queue' })).toBeInTheDocument()
  })

  it('explains a blocked composer once, above the row (offline, ended, unpaired)', () => {
    const why = "The laptop is offline. Messages aren't queued while it's away."
    const { container } = renderComposer({ disabled: true, disabledReason: why })
    expect(screen.getByTestId('composer-reason')).toHaveTextContent(why)
    // The reason is not repeated below the row.
    expect(within(container).getAllByText(why)).toHaveLength(1)
    expect(screen.getByLabelText('Message')).toBeDisabled()
  })
})
