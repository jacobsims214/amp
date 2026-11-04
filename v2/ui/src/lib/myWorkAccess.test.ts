import { describe, expect, it } from 'vitest'
import { isNoUserError, noMyWorkReason } from './myWorkAccess'

describe('noMyWorkReason', () => {
  it('a signed-in AMP user has My work', () => {
    expect(noMyWorkReason({ subject: 'oidc|1', email: 'jo@x.io', name: 'Jo', roles: [], user_id: 7 })).toBeNull()
  })
  it('the local-dev identity (sign-in off) has none', () => {
    expect(noMyWorkReason({ subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: [] })).toBe('local-dev')
  })
  it('any other identity without an AMP user is not signed in', () => {
    expect(noMyWorkReason({ subject: 'oidc|1', email: 'jo@x.io', name: 'Jo', roles: [] })).toBe('no-user')
  })
  it('an unknown identity (loading, or /api/me failed) is not judged', () => {
    expect(noMyWorkReason(null)).toBeNull()
  })
})

describe('isNoUserError', () => {
  it("matches the server's message", () => {
    expect(isNoUserError('My work needs a signed-in AMP user')).toBe(true)
    expect(isNoUserError('boom')).toBe(false)
    expect(isNoUserError(null)).toBe(false)
  })
})
