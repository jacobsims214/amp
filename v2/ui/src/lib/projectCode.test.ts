import { describe, expect, it } from 'vitest'
import { cleanCode, suggestCode } from './projectCode'

describe('project codes', () => {
  it('keeps only A-Z and 0-9, upper-cased, at most 12', () => {
    expect(cleanCode('pel 1')).toBe('PEL1')
    expect(cleanCode('amp-v2_x')).toBe('AMPV2X')
    expect(cleanCode('abcdefghijklmnop')).toBe('ABCDEFGHIJKL')
  })

  it('suggests initials, or the start of a one-word name', () => {
    expect(suggestCode('Pelican One')).toBe('PO')
    expect(suggestCode('Mobile app v2')).toBe('MAV2')
    expect(suggestCode('Release 2027')).toBe('R2027')
    expect(suggestCode('pelican')).toBe('PELI')
    expect(suggestCode('  ')).toBe('')
    expect(suggestCode('Café — ops!')).toBe('CO')
  })
})
