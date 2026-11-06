// People & agents never shows a bare "forbidden" (#124): a 403 the server
// gives no reason for becomes a sentence saying what is missing.
import { render, screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { MemoryRouter } from 'react-router-dom'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { Users } from './Users'
import { AuthContext } from '../contexts/auth-context'
import type { Me } from '../types'

const server = setupServer(
  http.get('*/api/users', () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })),
  http.get('*/authadmin/users', () => new HttpResponse('Not Found', { status: 404 })),
)
beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const me: Me = { subject: 'local-dev', email: 'local-dev@amp.local', name: 'Local Dev', roles: ['admin'], user_id: 1, local_dev: true }

function open() {
  return render(
    <AuthContext.Provider value={{ me, loading: false, error: null, isAdmin: true }}>
      <MemoryRouter><Users /></MemoryRouter>
    </AuthContext.Provider>,
  )
}

describe('People & agents', () => {
  it('a 403 renders a sentence, never "forbidden"', async () => {
    open()
    expect(await screen.findByText(/doesn't have permission for this/)).toBeInTheDocument()
    expect(screen.queryByText(/^\s*forbidden\s*$/i)).toBeNull()
    expect(document.body.textContent?.toLowerCase()).not.toMatch(/\bforbidden\b/)
  })
  it('in local mode, says sign-in is off instead of "the sign-in service didn\'t answer"', async () => {
    open()
    expect(await screen.findByText(/Sign-in is off on this AMP \(local mode\)/)).toBeInTheDocument()
  })
})
