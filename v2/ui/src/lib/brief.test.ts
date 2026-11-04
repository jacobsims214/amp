import { describe, expect, it } from 'vitest'
import { checklistItems, softBreaks } from './brief'

describe('checklistItems', () => {
  it('reads a markdown list as checklist rows, ticked where the planner wrote [x]', () => {
    expect(checklistItems('- Targets confirmed\n- [ ] Doc updated\n* [x] Owner told\n1. Numbered too')).toEqual([
      { text: 'Targets confirmed', done: false },
      { text: 'Doc updated', done: false },
      { text: 'Owner told', done: true },
      { text: 'Numbered too', done: false },
    ])
  })

  it('skips blank lines between items', () => {
    expect(checklistItems('- a\n\n- b\n')).toHaveLength(2)
  })

  it('returns null for anything that is not one list item per line, so it is shown as markdown', () => {
    expect(checklistItems('The targets are confirmed in the doc.')).toBeNull()
    expect(checklistItems('## Done when\n- a')).toBeNull()
    expect(checklistItems('- a\n  wrapped onto a second line')).toBeNull()
    expect(checklistItems('')).toBeNull()
    expect(checklistItems(undefined)).toBeNull()
  })
})

describe('softBreaks', () => {
  it('turns single newlines into markdown hard breaks', () => {
    expect(softBreaks('line1\nline2')).toBe('line1  \nline2')
  })

  it('leaves paragraph breaks, the last line and fenced code alone', () => {
    expect(softBreaks('a\n\nb')).toBe('a\n\nb')
    expect(softBreaks('a\n```\nx\ny\n```\nb')).toBe('a\n```\nx\ny\n```\nb')
  })
})
