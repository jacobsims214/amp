import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render } from '@testing-library/react'
import { LaunchSequence } from './LaunchSequence'
import { shouldLaunch } from '../../lib/launch'

vi.mock('../../hooks/useAuth', () => ({
  useAuth: () => ({ me: null, loading: true, error: null, isAdmin: false }),
}))

describe('LaunchSequence', () => {
  beforeEach(() => {
    sessionStorage.clear()
    // jsdom has no canvas; the overlay still mounts, it just has nothing to draw on.
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null)
  })

  it('marks the session, so a reload at / does not replay it', () => {
    expect(shouldLaunch({ pathname: '/', search: '' })).toBe(true)
    const { container } = render(<LaunchSequence />)
    expect(container.querySelector('.amp-launch')).not.toBeNull()
    expect(shouldLaunch({ pathname: '/', search: '' })).toBe(false)
  })
})
