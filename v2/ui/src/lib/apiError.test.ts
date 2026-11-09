import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import { api } from '../api/client'
import { errMsg } from './adminStyles'
import { apiErrorMessage, friendlyError, isBareStatus, NO_PERMISSION, NOT_SIGNED_IN } from './apiError'

describe('apiErrorMessage (#124)', () => {
  it('turns a bare 403 into a sentence naming what is missing', () => {
    for (const bare of ['forbidden', 'Forbidden', 'FORBIDDEN\n', '', undefined, '403', '403 Forbidden', 'GET /users: 403']) {
      const msg = apiErrorMessage(403, bare)
      expect(msg).toBe(NO_PERMISSION)
      expect(msg.trim().toLowerCase()).not.toBe('forbidden')
    }
  })
  it('turns a bare 401 into "sign in again"', () => {
    expect(apiErrorMessage(401, 'Unauthorized')).toBe(NOT_SIGNED_IN)
    expect(apiErrorMessage(401, 'not authenticated')).toBe(NOT_SIGNED_IN)
  })
  it("keeps the server's reason when it is a sentence", () => {
    const why = 'only project leads, owners and admins can read another person\'s My work in project 1; you can read your own'
    expect(apiErrorMessage(403, why)).toBe(why)
    expect(apiErrorMessage(500, 'database is down')).toBe('database is down')
  })
  it('uses the fallback for other bare failures', () => {
    expect(apiErrorMessage(500, '', 'GET /x: 500')).toBe('GET /x: 500')
  })
  it('isBareStatus / friendlyError', () => {
    expect(isBareStatus('forbidden')).toBe(true)
    expect(isBareStatus('you are not on team 3')).toBe(false)
    expect(friendlyError('forbidden')).toBe(NO_PERMISSION)
    expect(friendlyError('Unauthorized')).toBe(NOT_SIGNED_IN)
    expect(friendlyError('boom')).toBe('boom')
    expect(errMsg(new Error('forbidden'), 'x')).toBe(NO_PERMISSION)
  })
})

describe('the API client never surfaces a bare "forbidden"', () => {
  const server = setupServer(
    http.get('*/api/users', () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })),
    http.post('*/api/agents', () => new HttpResponse('Forbidden', { status: 403, statusText: 'Forbidden' })),
    http.get('*/api/projects', () => new HttpResponse(null, { status: 401, statusText: 'Unauthorized' })),
  )
  beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
  afterEach(() => server.resetHandlers())
  afterAll(() => server.close())

  it.each([
    ['GET /api/users (JSON {"error":"forbidden"})', () => api.listUsers(), NO_PERMISSION],
    ['POST /api/agents (text "Forbidden")', () => api.createAgent('a', []), NO_PERMISSION],
    ['GET /api/projects (401, no body)', () => api.listProjects(), NOT_SIGNED_IN],
  ])('%s', async (_, call, want) => {
    const err = await call().then(() => null, (e: unknown) => e as Error & { status?: number })
    expect(err).not.toBeNull()
    expect(err!.message).toBe(want)
    expect(err!.message.toLowerCase()).not.toMatch(/^forbidden$/)
  })
})
