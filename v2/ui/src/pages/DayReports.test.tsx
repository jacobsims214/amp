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
