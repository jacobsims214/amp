import { describe, expect, it } from 'vitest'
import { DEFAULT_FILTER, parseBoardFilter, serializeBoardFilter, type FilterState } from './boardFilter'
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
    [{ state: 'blocked' }, 'status=waiting'],
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
    const f: FilterState = { query: 'x', state: 'blocked', epicId: 2, owner: 9 }
    expect(roundTrip(f)).toEqual(f)
  })

  it('reads junk as the default', () => {
    expect(parseBoardFilter(new URLSearchParams('status=nope&epic=-1&owner=zz'))).toEqual(DEFAULT_FILTER)
  })

  it('accepts owner=everyone as an alias of all', () => {
    expect(parseBoardFilter(new URLSearchParams('owner=everyone')).owner).toBe('everyone')
  })
})
