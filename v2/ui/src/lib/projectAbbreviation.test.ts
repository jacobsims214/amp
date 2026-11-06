import { describe, expect, it } from 'vitest'
import { projectAbbreviation } from './projectAbbreviation'

describe('projectAbbreviation', () => {
  it('derives 3-letter abbreviations from project names', () => {
    expect(projectAbbreviation('Home OS')).toBe('HOS')
    expect(projectAbbreviation('simstech Design System')).toBe('SDS')
    expect(projectAbbreviation('amp-api')).toBe('AAP')
    expect(projectAbbreviation('Northwind Commerce')).toBe('NCO')
  })

  it('handles short names by repeating the last available initial', () => {
    expect(projectAbbreviation('UX')).toBe('UXX')
    expect(projectAbbreviation('A')).toBe('AAA')
    expect(projectAbbreviation('Go')).toBe('GOO')
  })

  it('ignores digits and punctuation', () => {
    expect(projectAbbreviation('Project 42')).toBe('PRO')
    expect(projectAbbreviation('My--Cool App!')).toBe('MCA')
    expect(projectAbbreviation('   spaced   out   name   ')).toBe('SON')
  })

  it('uppercases the result', () => {
    expect(projectAbbreviation('lower case words')).toBe('LCW')
  })

  it('falls back to AMP for names with no letters', () => {
    expect(projectAbbreviation('123')).toBe('AMP')
    expect(projectAbbreviation('!!!')).toBe('AMP')
    expect(projectAbbreviation('   ')).toBe('AMP')
  })
})
