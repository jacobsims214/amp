import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LAUNCH_MS, launchMode, markLaunched, SEEN_KEY, shouldLaunch, stations, summary } from './launch'
import type { Me } from '../types'

const at = (pathname: string, search = '') => ({ pathname, search })
const me: Me = { subject: 'local-dev', email: 'dev@local', name: 'Local Dev', roles: ['admin'] }

describe('shouldLaunch', () => {
  beforeEach(() => sessionStorage.clear())

  it('plays on a cold start at /', () => {
    expect(shouldLaunch(at('/'))).toBe(true)
  })

  it('skips the second load in the same session', () => {
    expect(shouldLaunch(at('/'))).toBe(true)
    markLaunched()
    expect(sessionStorage.getItem(SEEN_KEY)).toBe('1')
    expect(shouldLaunch(at('/'))).toBe(false)
  })

  it.each([
    '/project/3/board',
    '/project/3',
    '/projects',
    '/artifacts/1',
    '/sessions/abc',
  ])('skips a deep link to %s, even on a cold start', path => {
    expect(shouldLaunch(at(path, '?focus=7'))).toBe(false)
  })

  it('still honours ?launch= for demos', () => {
    markLaunched()
    expect(shouldLaunch(at('/', '?launch=full'))).toBe(true)
    expect(shouldLaunch(at('/project/3', '?launch=short'))).toBe(true)
    expect(shouldLaunch(at('/', '?launch=bogus'))).toBe(false)
  })

  it('does not play when sessionStorage is blocked', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked') })
    expect(shouldLaunch(at('/'))).toBe(false)
  })
})

describe('stations', () => {
  const unchecked = /PLAN CHECK|AGENTS|KNOWLEDGE BASE|TELEMETRY/

  it('never lists a system it did not check', () => {
    for (const s of [
      { me: null, loading: true, error: null },
      { me, loading: false, error: null },
      { me: null, loading: false, error: 'Failed to load identity (502)' },
      { me: null, loading: false, error: null },
    ]) {
      for (const line of stations(s)) expect(line.label).not.toMatch(unchecked)
    }
  })

  it('does not say GO while /api/me is still in flight', () => {
    const lines = stations({ me: null, loading: true, error: null })
    expect(lines).toEqual([{ label: 'AMP API', value: '', tone: 'pending' }])
    expect(summary(lines).text).not.toMatch(/GO/)
  })

  it('says GO only once /api/me answered, and names who is signed in', () => {
    const lines = stations({ me, loading: false, error: null })
    expect(lines).toEqual([
      { label: 'AMP API', value: 'GO', tone: 'go' },
      { label: 'SIGNED IN AS', value: 'LOCAL DEV', tone: 'info' },
    ])
    expect(summary(lines)).toEqual({ text: 'ALL STATIONS GO', tone: 'go' })
  })

  it('does not call a signed-out redirect a failure', () => {
    const lines = stations({ me: null, loading: false, error: null })
    expect(lines).toEqual([{ label: 'AMP API', value: '', tone: 'pending' }])
    expect(summary(lines).text).toBe('STANDING BY')
  })

  it('reports a failed /api/me as NO ANSWER, not GO', () => {
    const lines = stations({ me: null, loading: false, error: 'Failed to load identity (502)' })
    expect(lines).toEqual([{ label: 'AMP API', value: 'NO ANSWER', tone: 'nogo' }])
    expect(lines.some(l => l.value === 'GO')).toBe(false)
    expect(summary(lines).tone).toBe('nogo')
    expect(summary(lines).text).not.toMatch(/ALL STATIONS GO/)
  })
})

// Tier 0 (KB design/loading.md): the decision recorded there, in code.
describe('launchMode and timings', () => {
  const env = (over: Partial<Parameters<typeof launchMode>[0]> = {}) => ({ reducedMotion: false, search: '', referrer: '', ...over })

  it('plays the short launch on a cold start, and the full one only right after sign-in', () => {
    expect(launchMode(env())).toBe('short')
    expect(launchMode(env({ referrer: 'https://amp.example.com/oauth2/callback?code=x' }))).toBe('full')
    expect(launchMode(env({ referrer: 'https://amp.example.com/project/3/board' }))).toBe('short')
  })

  it('reduced motion wins; ?launch= forces a version', () => {
    expect(launchMode(env({ reducedMotion: true, search: '?launch=full' }))).toBe('reduced')
    expect(launchMode(env({ search: '?launch=full' }))).toBe('full')
    expect(launchMode(env({ search: '?launch=short', referrer: '/oauth2/x' }))).toBe('short')
  })

  it('short is a 600 ms floor that waits for /api/me up to 2 s; full is 5 s', () => {
    expect(LAUNCH_MS).toMatchObject({ short: 600, shortMax: 2000, full: 5000 })
  })
})
