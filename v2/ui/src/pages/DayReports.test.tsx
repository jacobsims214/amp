import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { api } from '../api/client'
import { LOAD_TIMEOUT_MS } from '../lib/timeout'
import { DayReports } from './DayReports'
import type { Member } from '../types'

vi.mock('../hooks/useAuth', () => ({ useAuth: () => ({ me: { user_id: 5 } }) }))

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

const never = <T,>() => new Promise<T>(() => {})

describe('DayReports loading', () => {
  it('shows an error with Retry when the people list does not answer, and Retry recovers', async () => {
    const people = vi.spyOn(api, 'reportPeople').mockImplementationOnce(never)
    render(<MemoryRouter><DayReports /></MemoryRouter>)

    await act(() => vi.advanceTimersByTimeAsync(LOAD_TIMEOUT_MS))
    expect(screen.getByRole('alert')).toHaveTextContent('Loading people took longer than 12 seconds')

    people.mockResolvedValue([{ user_id: 6, display_name: 'Ada' } as Member])
    vi.spyOn(api, 'velocity').mockImplementation(never)
    vi.spyOn(api, 'dayReports').mockResolvedValue([])
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Retry' })) })
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.getByText('Ada')).toBeInTheDocument()
  })

  it('times out the saved-reports load with Retry instead of spinning', async () => {
    vi.spyOn(api, 'reportPeople').mockResolvedValue([{ user_id: 6, display_name: 'Ada' } as Member])
    vi.spyOn(api, 'velocity').mockImplementation(never)
    const reports = vi.spyOn(api, 'dayReports').mockImplementationOnce(never)
    render(<MemoryRouter><DayReports /></MemoryRouter>)
    await act(async () => {})
    await act(async () => { fireEvent.click(screen.getByText('Ada')) })

    await act(() => vi.advanceTimersByTimeAsync(LOAD_TIMEOUT_MS))
    expect(screen.getByRole('alert')).toHaveTextContent('Loading saved reports took longer than 12 seconds')

    reports.mockResolvedValue([])
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Retry' })) })
    expect(screen.getByText('No saved reports in the last 30 days.')).toBeInTheDocument()
  })
})

describe('DayReports Your day panel', () => {
  const draft = (markdown: string, summary = '') => ({
    user_id: 5, user_email: '', user_name: 'Me', day: '2026-11-03', tz: 'UTC', summary, saved: false, markdown,
    data: {
      completed: [], started: [], created: [], in_progress: [], blocked: [], touched: [],
      stats: { completed: 0, started: 0, created: 0, in_progress: 0, blocked: 0, projects: 0 },
      generated_at: '2026-11-05T09:30:00Z',
    },
  })
  const open = async (report: ReturnType<typeof draft>, day?: string) => {
    vi.spyOn(api, 'reportPeople').mockResolvedValue([{ user_id: 5, display_name: 'Me' } as Member])
    vi.spyOn(api, 'velocity').mockImplementation(never)
    vi.spyOn(api, 'dayReports').mockResolvedValue([])
    vi.spyOn(api, 'dayReportDraft').mockResolvedValue(report)
    render(<MemoryRouter><DayReports /></MemoryRouter>)
    await act(async () => {})
    await act(async () => { fireEvent.click(screen.getByText('Me')) })
    if (day) await act(async () => { fireEvent.change(document.querySelector('input[type=date]')!, { target: { value: day } }) })
  }

  it('says which sections come from the picked day when it is not today', async () => {
    await open(draft(''), '2020-01-02')
    const note = screen.getByTestId('past-day-note')
    expect(note).toHaveTextContent('Completed, Started, Created and Touched')
    expect(note).toHaveTextContent('In progress and Blocked show')
    expect(note).toHaveTextContent('when this report was generated')
  })

  it('leaves the copy unchanged for today', async () => {
    await open(draft(''))
    expect(screen.queryByTestId('past-day-note')).toBeNull()
    expect(screen.getByText(/Built from today's AMP activity/)).toBeInTheDocument()
  })

  it('renders a saved summary GFM table as a table', async () => {
    vi.spyOn(api, 'reportPeople').mockResolvedValue([{ user_id: 6, display_name: 'Ada' } as Member])
    vi.spyOn(api, 'velocity').mockImplementation(never)
    vi.spyOn(api, 'dayReports').mockResolvedValue([{ ...draft(''), saved: true, summary: '| a | b |\n|---|---|\n| 1 | 2 |' }])
    render(<MemoryRouter><DayReports /></MemoryRouter>)
    await act(async () => {})
    await act(async () => { fireEvent.click(screen.getByText('Ada')) })
    expect(screen.getByRole('table')).toBeInTheDocument()
  })
})

describe('DayReports Velocity panel', () => {
  it('shows a quiet empty state instead of crashing when velocity has no days or fails', async () => {
    vi.spyOn(api, 'reportPeople').mockResolvedValue([{ user_id: 6, display_name: 'Ada' } as Member])
    vi.spyOn(api, 'velocity').mockRejectedValue(new Error('boom'))
    vi.spyOn(api, 'dayReports').mockResolvedValue([])
    render(<MemoryRouter><DayReports /></MemoryRouter>)
    await act(async () => {})
    await act(async () => { fireEvent.click(screen.getByText('Ada')) })
    await act(async () => {})
    expect(screen.getByText('No completed tickets to chart yet.')).toBeInTheDocument()
    expect(screen.getByText('No saved reports in the last 30 days.')).toBeInTheDocument()
  })
})

// Who, which day and which saved report are in the URL (#131), so "Tuesday's
// report for Ada" can be shared.
describe('DayReports in the URL', () => {
  const rep = (day: string) => ({
    user_id: 6, user_email: 'ada@x', user_name: 'Ada', day, tz: 'UTC', summary: `Summary for ${day}`, saved: true, markdown: '',
    data: {
      completed: [], started: [], created: [], in_progress: [], blocked: [], touched: [],
      stats: { completed: 1, started: 0, created: 0, in_progress: 0, blocked: 0, projects: 0 }, generated_at: '',
    },
  })

  it('/reports?user=6&open=2026-11-03 shows Ada with the Nov 3 report expanded', async () => {
    vi.spyOn(api, 'reportPeople').mockResolvedValue([{ user_id: 5, display_name: 'Me' } as Member, { user_id: 6, display_name: 'Ada' } as Member])
    vi.spyOn(api, 'velocity').mockImplementation(never)
    vi.spyOn(api, 'dayReports').mockResolvedValue([rep('2026-11-04'), rep('2026-11-03')])
    render(<MemoryRouter initialEntries={['/reports?user=6&open=2026-11-03']}><DayReports /></MemoryRouter>)
    await act(async () => {})
    expect(screen.getByRole('heading', { name: "Ada's reports" })).toBeInTheDocument()
    const expanded = screen.getAllByRole('button', { expanded: true })
    expect(expanded).toHaveLength(1)
    expect(expanded[0]).toHaveTextContent('Nov 3')
  })
})
