import { describe, expect, it } from 'vitest'
import { mismatchedRef, parseTicketRef, runPath, ticketPath, ticketRef } from './ticketRef'

describe('ticket refs', () => {
  it('builds the ref and link from a code and id', () => {
    expect(ticketRef('ba', 34)).toBe('BA-34')
    expect(ticketPath('BA', 34)).toBe('/t/BA-34')
    expect(ticketPath('', 34)).toBe('/t/34')
    expect(ticketPath(null, 34)).toBe('/t/34')
    expect(runPath(12)).toBe('/r/12')
    // A code with a URL character can't break the link.
    expect(ticketPath('a?b', 34)).toBe('/t/A%3FB-34')
    expect(parseTicketRef('A%3FB-34')).toEqual({ code: 'A?B', id: 34 })
  })

  it('reads BA-34, ba-34, 34 and #34 as ticket 34', () => {
    expect(parseTicketRef('BA-34')).toEqual({ code: 'BA', id: 34 })
    expect(parseTicketRef('ba-34')).toEqual({ code: 'BA', id: 34 })
    expect(parseTicketRef('34')).toEqual({ code: null, id: 34 })
    expect(parseTicketRef('#34')).toEqual({ code: null, id: 34 })
    expect(parseTicketRef('MCT-1730-34')).toEqual({ code: 'MCT-1730', id: 34 })
  })

  it('rejects anything without a positive trailing id', () => {
    expect(parseTicketRef('BA')).toBeNull()
    expect(parseTicketRef('')).toBeNull()
    expect(parseTicketRef('0')).toBeNull()
    expect(parseTicketRef('34x')).toBeNull()
  })

  it('quotes a typed ref only when it is a ref for this ticket in another code', () => {
    expect(mismatchedRef('xy-34', 34, 'BA')).toBe('XY-34')
    expect(mismatchedRef('BA-34', 34, 'ba')).toBeNull() // the ticket's own ref
    expect(mismatchedRef('XY-35', 34, 'BA')).toBeNull() // another ticket's number
    expect(mismatchedRef('Call support now-34', 34, 'BA')).toBeNull() // not a ref
    expect(mismatchedRef(`${'X'.repeat(40)}-34`, 34, 'BA')).toBeNull() // too long
    expect(mismatchedRef('XY-34', 34, undefined)).toBeNull() // code not known yet
    expect(mismatchedRef(null, 34, 'BA')).toBeNull()
  })
})
