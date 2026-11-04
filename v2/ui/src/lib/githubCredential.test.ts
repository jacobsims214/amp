// The GitHub token form's pure decisions (AMP #3161).
import { describe, expect, it } from 'vitest'
import { credentialProblem, httpStatus } from './githubCredential'

describe('credentialProblem', () => {
  it('says nothing when the server reports nothing wrong', () => {
    expect(credentialProblem({})).toBeNull()
    expect(credentialProblem({ last_error: '' })).toBeNull()
  })

  it('turns the no-encryption-key class into the reason and what an admin does', () => {
    const p = credentialProblem({ last_error: 'no_encryption_key' })!
    expect(p).toMatch(/has no encryption key/i)
    expect(p).toMatch(/AMP_SECRETS_KEY/)
  })

  it('reports an unknown class as unknown, never as the raw value', () => {
    const p = credentialProblem({ last_error: 'token_refused_by_github' })!
    expect(p).toMatch(/does not recognise/i)
    expect(p).not.toContain('token_refused_by_github')
  })
})

describe('httpStatus', () => {
  it('reads the status the API client attaches, and nothing from other errors', () => {
    expect(httpStatus(Object.assign(new Error('forbidden'), { status: 403 }))).toBe(403)
    expect(httpStatus(new Error('plain'))).toBeUndefined()
    expect(httpStatus(null)).toBeUndefined()
    expect(httpStatus({ status: '403' })).toBeUndefined()
  })
})
