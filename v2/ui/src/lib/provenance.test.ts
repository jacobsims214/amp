// Provenance (docs/specs/human-decision-docs.md §3): per-line and per-option
// blame across an agent's v1 and a person's v2.
import { describe, expect, it } from 'vitest'
import { blame, blameRuns } from './provenance'
import type { DecisionDocVersion } from '../types'

const opts = (b: string) => ({
  question: 'Which metric?',
  recommended: 1,
  options: [
    { label: 'A: Weekly active teams', consequence: 'Easy to measure' },
    { label: b, consequence: 'Needs the onboarding event' },
    { label: 'C: Net revenue retention', consequence: 'Lags 90 days' },
  ],
})

const v = (version: number, author_kind: 'agent' | 'person', text: string, b: string): DecisionDocVersion => ({
  version, author_kind, text, ask: opts(b), created_by: 1, created_at: '2026-11-04T10:00:00Z',
})

const v1 = v(1, 'agent', '## Context\n\nNorth star.\n\n## Why\n\nB moves inside the quarter.', 'B: Activation rate (40%)')
const v2 = v(2, 'person', '## Context\n\nNorth star for Q1.\n\n## Why\n\nB moves inside the quarter.\n\nTrial stays at 14 days.', 'B: Activation rate (45%)')

describe('blame', () => {
  it('gives v1 to the agent, line by line and option by option', () => {
    const b = blame([v1])
    expect(b.lines.every(l => l.author === 'agent' && l.version === 1)).toBe(true)
    expect(b.options).toEqual([{ author: 'agent', version: 1 }, { author: 'agent', version: 1 }, { author: 'agent', version: 1 }])
  })

  it('gives the lines and the option the person changed in v2 to the person, the rest to the agent', () => {
    const b = blame([v1, v2])
    expect(b.lines.map(l => `${l.author}:${l.text}`)).toEqual([
      'agent:## Context', 'agent:', 'person:North star for Q1.', 'agent:', 'agent:## Why', 'agent:',
      'agent:B moves inside the quarter.', 'person:', 'person:Trial stays at 14 days.',
    ])
    expect(b.options.map(o => o.author)).toEqual(['agent', 'person', 'agent'])
    expect(b.options[1].version).toBe(2)
  })

  it('keeps the agent on lines a later person version left alone', () => {
    const v3 = v(3, 'person', v2.text.replace('Trial stays at 14 days.', 'Trial stays at 21 days.'), 'B: Activation rate (45%)')
    const b = blame([v1, v2, v3])
    expect(b.lines.find(l => l.text === 'B moves inside the quarter.')).toMatchObject({ author: 'agent', version: 1 })
    expect(b.lines.find(l => l.text === 'North star for Q1.')).toMatchObject({ author: 'person', version: 2 })
    expect(b.lines.find(l => l.text === 'Trial stays at 21 days.')).toMatchObject({ author: 'person', version: 3 })
    expect(b.options[1]).toEqual({ author: 'person', version: 2 })
  })

  it('groups consecutive lines by author', () => {
    const runs = blameRuns(blame([v1, v2]).lines)
    expect(runs.map(r => r.author)).toEqual(['agent', 'person', 'agent', 'person'])
    expect(runs[1].text).toBe('North star for Q1.')
  })
})
