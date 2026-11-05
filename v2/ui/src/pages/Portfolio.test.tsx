// The Portfolio range is in the URL (#131): /portfolio?org=1&range=90d opens the 90-day view.
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { Portfolio } from './Portfolio'

vi.mock('../hooks/useAuth', () => ({ useAuth: () => ({ me: { user_id: 5, orgs: [{ id: 1, name: 'Acme' }] } }) }))
afterEach(() => vi.restoreAllMocks())

describe('Portfolio range in the URL', () => {
  it('/portfolio?org=1&range=90d loads the 90-day view', async () => {
    const pf = vi.spyOn(api, 'portfolio').mockImplementation(() => new Promise(() => {}))
    render(<MemoryRouter initialEntries={['/portfolio?org=1&range=90d']}><Portfolio /></MemoryRouter>)
    await waitFor(() => expect(pf).toHaveBeenCalled())
    const since = Date.parse(pf.mock.calls[0][1] as string)
    expect(Math.round((Date.now() - since) / 86_400_000)).toBe(90)
    expect(screen.getByRole('button', { name: '90d' })).toHaveAttribute('aria-pressed', 'true')
  })
})
