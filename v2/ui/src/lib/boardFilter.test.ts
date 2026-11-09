import { describe, expect, it } from 'vitest'
import { boardStatus, DEFAULT_FILTER, parseBoardFilter, serializeBoardFilter, type FilterState } from './boardFilter'
import { patchParams } from '../hooks/useSearchParamState'

const roundTrip = (f: FilterState) => parseBoardFilter(patchParams(new URLSearchParams(), serializeBoardFilter(f)))

describe('board filter in the URL', () => {
  it('leaves every default out', () => {
    expect(patchParams(new URLSearchParams(), serializeBoardFilter(DEFAULT_FILTER)).toString()).toBe('')
    expect(parseBoardFilter(new URLSearchParams())).toEqual(DEFAULT_FILTER)
  })

  it.each<[Partial<FilterState>, string]>([
    [{ state: 'backlog' }, 'status=ready'],
    [{ state: 'in_progress' }, 'status=in_progress'],
    [{ state: 'completed' }, 'status=done'],
    [{ state: 'waiting' }, 'status=waiting'],
    [{ state: 'question' }, 'status=question'],
    [{ query: 'north star' }, 'q=north+star'],
    [{ epicId: 4 }, 'epic=4'],
    [{ owner: 'everyone' }, 'owner=all'],
    [{ owner: 'unowned' }, 'owner=unowned'],
    [{ owner: 12 }, 'owner=12'],
  ])('round-trips %o as %s', (over, query) => {
    const f = { ...DEFAULT_FILTER, ...over }
    expect(patchParams(new URLSearchParams(), serializeBoardFilter(f)).toString()).toBe(query)
    expect(roundTrip(f)).toEqual(f)
  })

  it('round-trips every field at once', () => {
    const f: FilterState = { query: 'x', state: 'question', epicId: 2, owner: 9 }
    expect(roundTrip(f)).toEqual(f)
  })

  it('reads junk as the default', () => {
    expect(parseBoardFilter(new URLSearchParams('status=nope&epic=-1&owner=zz'))).toEqual(DEFAULT_FILTER)
  })

  it('accepts owner=everyone as an alias of all', () => {
    expect(parseBoardFilter(new URLSearchParams('owner=everyone')).owner).toBe('everyone')
  })
})

describe('boardStatus (#129)', () => {
  it('splits a person\'s question from waiting on other tickets', () => {
    expect(boardStatus({ state: 'blocked', block_reason: 'HubSpot or Pipedrive?' })).toBe('question')
    expect(boardStatus({ state: 'blocked', block_reason: '', blocked_by_ids: [3] })).toBe('waiting')
    expect(boardStatus({ state: 'blocked', block_reason: 'scheduled:2026-12-01T00:00:00Z' })).toBeNull()
    expect(boardStatus({ state: 'backlog' })).toBe('backlog')
  })
})
