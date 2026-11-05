import { describe, it, expect } from 'vitest'
import { stripMarkdown } from './stripMarkdown'

describe('stripMarkdown', () => {
  it('handles the combined example', () => {
    expect(stripMarkdown('**Pick** [one](https://x) `code`')).toBe('Pick one code')
  })
  it('removes bold and italic', () => {
    expect(stripMarkdown('**bold** and __bold2__')).toBe('bold and bold2')
    expect(stripMarkdown('*it* and _it2_')).toBe('it and it2')
  })
  it('keeps link text only', () => {
    expect(stripMarkdown('see [docs](https://a.b/c_d) now')).toBe('see docs now')
  })
  it('removes inline code ticks', () => {
    expect(stripMarkdown('run `npm test` first')).toBe('run npm test first')
  })
  it('removes list markers', () => {
    expect(stripMarkdown('- one\n- two\n1. three')).toBe('one two three')
    expect(stripMarkdown('* a\n+ b')).toBe('a b')
  })
  it('removes headings', () => {
    expect(stripMarkdown('## Title\nbody')).toBe('Title body')
  })
  it('returns empty for empty input', () => {
    expect(stripMarkdown('')).toBe('')
  })
})
