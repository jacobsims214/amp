import { describe, expect, it } from 'vitest'
import { agoText, isCloseout, latestRunWith, parseSelfCheck } from './receipt'
import type { Comment, Run } from '../types'

const c = (id: number, body: string, created_at = `2026-11-01T10:0${id}:00Z`): Comment => ({ id, task_id: 1, body, author: 'a', created_at })
const run = (over: Partial<Run>): Run => ({ id: 1, task_id: 1, project_id: 1, outcome: 'completed', created_at: '2026-11-01T10:00:00Z', ...over })

describe('parseSelfCheck', () => {
  it('counts VERIFIED lines', () => {
    const r = parseSelfCheck([c(1, 'Work complete.\n- Tests pass: VERIFIED\n- Lint clean: VERIFIED')])
    expect(r).toMatchObject({ verified: 2, total: 2, unverified: [] })
  })
  it('reads COULD NOT VERIFY with a dash, an em dash or a colon reason', () => {
    const r = parseSelfCheck([c(1, 'Work complete.\n- A: VERIFIED\n- B: COULD NOT VERIFY — no browser\n- C: COULD NOT VERIFY: no db\n- D: COULD NOT VERIFY - slow\n- E: COULD NOT VERIFY')])!
    expect(r.verified).toBe(1)
    expect(r.total).toBe(5)
    expect(r.unverified.map(i => [i.criterion, i.why])).toEqual([['B', 'no browser'], ['C', 'no db'], ['D', 'slow'], ['E', '']])
  })
  it('keeps colons inside a criterion', () => {
    const r = parseSelfCheck([c(1, 'Work complete.\n- GET /a: returns 200: VERIFIED')])!
    expect(r.items[0].criterion).toBe('GET /a: returns 200')
  })
  it('uses the latest "Work complete." comment', () => {
    const r = parseSelfCheck([
      c(1, 'Work complete.\n- A: COULD NOT VERIFY'),
      c(3, 'Work complete.\n- A: VERIFIED'),
      c(2, 'Work complete.\n- A: COULD NOT VERIFY\n- B: COULD NOT VERIFY'),
      c(4, 'Thanks!\n- A: COULD NOT VERIFY'),
    ])!
    expect(r).toMatchObject({ verified: 1, total: 1 })
  })
  it('compares timestamps, not strings, when fractional precision differs', () => {
    // As strings '...:00.5Z' < '...:00Z' ('.' < 'Z'), but 00.5 is the later instant.
    const r = parseSelfCheck([
      c(1, 'Work complete.\n- A: COULD NOT VERIFY', '2026-11-01T10:00:00Z'),
      c(2, 'Work complete.\n- A: VERIFIED', '2026-11-01T10:00:00.5Z'),
    ])!
    expect(r.verified).toBe(1)
    const r2 = parseSelfCheck([
      c(1, 'Work complete.\n- A: VERIFIED', '2026-11-01T10:00:00.123456Z'),
      c(2, 'Work complete.\n- A: COULD NOT VERIFY', '2026-11-01T10:00:00.1Z'),
    ])!
    expect(r2.verified).toBe(1)
  })
  it('is null with no such comment, or no check lines', () => {
    expect(parseSelfCheck([])).toBeNull()
    expect(parseSelfCheck([c(1, 'Started work.\n- A: VERIFIED')])).toBeNull()
    expect(parseSelfCheck([c(1, 'Work complete.\nAll done, shipped.')])).toBeNull()
  })
  it('ignores malformed lines', () => {
    const r = parseSelfCheck([c(1, 'Work complete.\n- no verdict here\nA: VERIFIED\n- B: verified\n- C VERIFIED\n- D: VERIFIED')])!
    expect(r.items.map(i => i.criterion)).toEqual(['D'])
  })
  it('handles CRLF', () => {
    const r = parseSelfCheck([c(1, 'Work complete.\r\n- A: VERIFIED\r\n- B: COULD NOT VERIFY — why\r\n')])!
    expect(r.total).toBe(2)
    expect(r.unverified[0].why).toBe('why')
  })
})

describe('latestRunWith', () => {
  it('returns the newest run with a branch or PR by default', () => {
    const runs = [run({ id: 1, branch: 'a' }), run({ id: 2, pr_url: 'u' }), run({ id: 3 })]
    expect(latestRunWith(runs)?.id).toBe(2)
    expect(latestRunWith([run({ id: 3 })])).toBeUndefined()
    expect(latestRunWith(runs, r => r.id === 1)?.id).toBe(1)
  })
})

describe('isCloseout / agoText', () => {
  it('tags closeout notes', () => {
    expect(isCloseout(run({ note: 'closeout: merged by hand' }))).toBe(true)
    expect(isCloseout(run({ note: 'done' }))).toBe(false)
    expect(isCloseout(undefined)).toBe(false)
  })
  it('formats ages', () => {
    const now = Date.parse('2026-11-04T12:00:00Z')
    expect(agoText('2026-11-04T10:00:00Z', now)).toBe('2h ago')
    expect(agoText('2026-11-04T11:59:50Z', now)).toBe('just now')
    expect(agoText('2026-11-01T12:00:00Z', now)).toBe('3d ago')
  })
})
