// The api client's cancellable reads (AMP #1575).
//
// `get` takes an optional AbortSignal; getProjectActivity takes `{ limit, signal }`
// and audit takes `{ signal }` (its limit already rode in the query object).
// Mission's ~500 KB activity loads use them to cap the feed and drop superseded
// loads (#1586). Three things are pinned here:
//
//  1. A caller that passes no options sends exactly the URL it sent before.
//  2. `limit` reaches the query string.
//  3. An aborted read rejects with the AbortError, not with an HttpError, both
//     before the response and while a refusal's body is still being read.
import { delay, http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'

let urls: URL[] = []

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
beforeEach(() => { urls = [] })
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

/** Answer `pattern` with `answer`, recording each request's URL. */
function answer(pattern: string, body: Record<string, unknown>) {
  server.use(http.get(pattern, ({ request }) => {
    urls.push(new URL(request.url))
    return HttpResponse.json(body)
  }))
}

/** Never answer `pattern` before the test aborts (bounded, so a missed abort can't hang the run). */
function stall(pattern: string) {
  server.use(http.get(pattern, async ({ request }) => {
    urls.push(new URL(request.url))
    await delay(5_000)
    return HttpResponse.json({})
  }))
}

/** What a promise rejected with, or a failure if it resolved. */
async function rejection(p: Promise<unknown>): Promise<unknown> {
  try {
    await p
  } catch (err) {
    return err
  }
  throw new Error('expected the call to reject')
}

describe('getProjectActivity', () => {
  it('sends the same URL as before when given no options', async () => {
    answer('*/api/projects/10/activity', { activity: [{ id: 1 }], count: 1 })

    await expect(api.getProjectActivity(10)).resolves.toEqual([{ id: 1 }])
    await api.getProjectActivity(10, '2026-11-01T00:00:00Z')
    await api.getProjectActivity(10, '2026-11-01T00:00:00Z', '2026-11-02T00:00:00Z')

    expect(urls.map(u => u.search)).toEqual([
      '',
      '?since=2026-11-01T00%3A00%3A00Z',
      '?since=2026-11-01T00%3A00%3A00Z&until=2026-11-02T00%3A00%3A00Z',
    ])
  })

  it('sends limit as a query param', async () => {
    answer('*/api/projects/10/activity', { activity: [], count: 0 })

    await api.getProjectActivity(10, '2026-11-01T00:00:00Z', undefined, { limit: 100 })
    await api.getProjectActivity(10, undefined, undefined, { limit: 0 })

    expect(urls[0].searchParams.get('since')).toBe('2026-11-01T00:00:00Z')
    expect(urls[0].searchParams.get('limit')).toBe('100')
    expect(urls[0].searchParams.has('until')).toBe(false)
    expect(urls[1].search).toBe('?limit=0')
  })

  it('rejects with the AbortError when its signal aborts mid-flight', async () => {
    stall('*/api/projects/10/activity')
    const ctl = new AbortController()

    const load = api.getProjectActivity(10, undefined, undefined, { limit: 100, signal: ctl.signal })
    await expect.poll(() => urls.length).toBe(1)
    ctl.abort()

    const err = await rejection(load)
    expect(err).toMatchObject({ name: 'AbortError' })
    expect(err).not.toHaveProperty('status')
  })

  it('rejects with the AbortError when its signal was already aborted', async () => {
    answer('*/api/projects/10/activity', { activity: [], count: 0 })
    const ctl = new AbortController()
    ctl.abort()

    const err = await rejection(api.getProjectActivity(10, undefined, undefined, { signal: ctl.signal }))
    expect(err).toMatchObject({ name: 'AbortError' })
    expect(urls).toHaveLength(0)
  })

  it('rejects with the AbortError, not the refusal, when the abort lands while the error body is read', async () => {
    // fetch here, not MSW: a real socket aborted mid-body trips an undici
    // assertion after the test. The refusal's body read is where the abort
    // lands, which is what fetch does when the signal fires during a read.
    const ctl = new AbortController()
    const refusal = new Response(null, { status: 500 })
    refusal.json = () => {
      ctl.abort()
      return Promise.reject(new DOMException('This operation was aborted', 'AbortError'))
    }
    vi.stubGlobal('fetch', vi.fn(async () => refusal))
    try {
      const err = await rejection(api.getProjectActivity(10, undefined, undefined, { signal: ctl.signal }))
      expect(err).toMatchObject({ name: 'AbortError' })
      expect(err).not.toHaveProperty('status')
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('still throws the HttpError for a refusal it did not cancel', async () => {
    server.use(http.get('*/api/projects/10/activity', () => HttpResponse.json({ error: 'no access' }, { status: 403 })))

    const err = await rejection(api.getProjectActivity(10, undefined, undefined, { signal: new AbortController().signal }))
    expect(err).toMatchObject({ status: 403 })
  })
})

describe('audit', () => {
  it('sends the same URL as before when given no options', async () => {
    answer('*/api/audit', { entries: [{ id: 3 }] })

    await expect(api.audit({ projectId: 10, limit: 200 })).resolves.toEqual([{ id: 3 }])

    expect(urls[0].search).toBe('?project_id=10&limit=200')
  })

  it('treats a null entries list as empty', async () => {
    answer('*/api/audit', { entries: null })

    await expect(api.audit({ projectId: 10 }, { signal: new AbortController().signal })).resolves.toEqual([])
  })

  it('rejects with the AbortError when its signal aborts mid-flight', async () => {
    stall('*/api/audit')
    const ctl = new AbortController()

    const load = api.audit({ projectId: 10, limit: 100 }, { signal: ctl.signal })
    await expect.poll(() => urls.length).toBe(1)
    expect(urls[0].searchParams.get('limit')).toBe('100')
    ctl.abort()

    const err = await rejection(load)
    expect(err).toMatchObject({ name: 'AbortError' })
    expect(err).not.toHaveProperty('status')
  })
})
