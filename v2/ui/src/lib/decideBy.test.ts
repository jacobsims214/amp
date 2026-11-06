import { describe, expect, it } from 'vitest'
import { describeDecideBy, fromLocalInput, toLocalInput } from './decideBy'

const now = Date.parse('2026-11-05T12:00:00Z')

describe('describeDecideBy', () => {
  it('says how far off a date is, and how far overdue', () => {
    expect(describeDecideBy('2026-11-07T12:00:00Z', now)?.relative).toBe('in 2d')
    expect(describeDecideBy('2026-11-05T15:00:00Z', now)?.relative).toBe('in 3h')
    expect(describeDecideBy('2026-11-05T12:12:00Z', now)?.relative).toBe('in 12m')
    const late = describeDecideBy('2026-11-05T09:00:00Z', now)
    expect(late).toMatchObject({ relative: '3h overdue', overdue: true })
    expect(describeDecideBy('2026-11-01T12:00:00Z', now)?.relative).toBe('4d overdue')
    expect(describeDecideBy('2026-11-05T11:59:40Z', now)).toMatchObject({ relative: 'due now', overdue: true })
  })

  it('shows the absolute time in the reader\'s zone, with the zone named', () => {
    const utc = describeDecideBy('2026-11-12T17:00:00Z', now, 'UTC')!.absolute
    const ny = describeDecideBy('2026-11-12T17:00:00Z', now, 'America/New_York')!.absolute
    expect(utc).toMatch(/5:00/)
    expect(utc).toMatch(/UTC/)
    expect(ny).toMatch(/12:00/)
    expect(ny).toMatch(/EST|GMT-5/)
  })

  it('is null for no date or an unreadable one', () => {
    expect(describeDecideBy(undefined, now)).toBeNull()
    expect(describeDecideBy('soon', now)).toBeNull()
  })
})

describe('datetime-local round trip', () => {
  it('turns the reader\'s wall-clock time into the instant, and back', () => {
    const iso = fromLocalInput('2026-11-12T17:00')!
    expect(iso).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00Z$/)
    expect(new Date(iso).getHours()).toBe(17) // local hours: the zone the reader typed in
    expect(toLocalInput(iso)).toBe('2026-11-12T17:00')
  })

  it('refuses what is not a date', () => {
    expect(fromLocalInput('')).toBeNull()
    expect(fromLocalInput('2026-02-31T10:00')).toBeNull()
    expect(fromLocalInput('tomorrow')).toBeNull()
    expect(toLocalInput('nope')).toBe('')
  })
})
