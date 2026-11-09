import { describe, expect, it } from 'vitest'
import { commitURL, sessionPath } from './runLinks'

describe('run links', () => {
  it('links a commit to the repository its PR is in', () => {
    expect(commitURL({ commit: 'abc1234def', pr_url: 'https://github.com/o/r/pull/7' })).toBe('https://github.com/o/r/commit/abc1234def')
    expect(commitURL({ commit: 'abc1234', pr_url: 'https://gitlab.example/g/sub/r/-/merge_requests/3' })).toBe('https://gitlab.example/g/sub/r/-/commit/abc1234')
  })
  it('leaves the commit unlinked without a PR or a real SHA', () => {
    expect(commitURL({ commit: 'abc1234' })).toBeNull()
    expect(commitURL({ commit: 'not a sha', pr_url: 'https://github.com/o/r/pull/7' })).toBeNull()
    expect(commitURL({ pr_url: 'https://github.com/o/r/pull/7' })).toBeNull()
  })
  it('links a run to its session', () => {
    expect(sessionPath({ session_id: 'rs_abc' })).toBe('/sessions/rs_abc')
    expect(sessionPath({})).toBeNull()
    expect(sessionPath({ session_id: '../x/y' })).toBeNull()
  })
})
