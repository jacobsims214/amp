import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render } from '@testing-library/react'
import { LaunchSequence } from './LaunchSequence'
import { shouldLaunch } from '../../lib/launch'
import type { AuthState } from '../../contexts/auth-context'

let auth: AuthState
vi.mock('../../hooks/useAuth', () => ({ useAuth: () => auth }))

describe('LaunchSequence', () => {
  beforeEach(() => {
    sessionStorage.clear()
    // jsdom has no canvas; the overlay still mounts, it just has nothing to draw on.
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null)
  })

  it('marks the session once /api/me answers, so a reload at / does not replay it', () => {
    auth = { me: null, loading: true, error: null, isAdmin: false }
    expect(shouldLaunch({ pathname: '/', search: '' })).toBe(true)
    const { container, rerender } = render(<LaunchSequence />)
    expect(container.querySelector('.amp-launch')).not.toBeNull()
    auth = { me: { subject: 'u', email: 'u@x', name: 'U', roles: [] }, loading: false, error: null, isAdmin: false }
    rerender(<LaunchSequence />)
    expect(shouldLaunch({ pathname: '/', search: '' })).toBe(false)
  })

  it('does not mark the session when signed out, so the launch plays after sign-in brings the tab back to /', () => {
    // Signed out: /api/me redirected, AuthProvider is sending the tab to sign-in.
    auth = { me: null, loading: false, error: null, isAdmin: false }
    render(<LaunchSequence />)
    expect(shouldLaunch({ pathname: '/', search: '' })).toBe(true)
  })
})
