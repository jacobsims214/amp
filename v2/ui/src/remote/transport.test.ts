import { http, HttpResponse } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { httpTransport, RelayError, SSEParser, type EventStreamHandlers } from './transport'
import { MOCK_DEVICE_ID, MOCK_HARNESS_ID, MOCK_SESSION_ID, MOCK_SIG, MockRelay } from './mock/mockRelay'
import { relayHandlers } from './mock/handlers'
import type { CommandEnvelope, RelayFrame } from './types'

let relay = new MockRelay({ stepMs: 0 })
const server = setupServer(...relayHandlers(relay))
beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
afterEach(() => { relay = new MockRelay({ stepMs: 0 }); server.resetHandlers(...relayHandlers(relay)) })
afterAll(() => server.close())

function collect() {
  const got = { envelopes: [] as string[], snapshots: [] as string[], relay: [] as RelayFrame[], open: false, closed: null as RelayError | undefined | null }
  const h: EventStreamHandlers = {
    onOpen: () => { got.open = true },
    onActivity: () => {},
    onEnvelope: w => got.envelopes.push(w),
    onSnapshot: w => got.snapshots.push(w),
    onRelay: f => got.relay.push(f),
    onClose: e => { got.closed = e },
  }
  return { got, h }
}

const command = (seq: number): CommandEnvelope => ({
  v: 1, alg: 'ES256', session_id: MOCK_SESSION_ID, harness_id: MOCK_HARNESS_ID, device_id: MOCK_DEVICE_ID,
  seq, ts: Date.now(), nonce: `n${seq}`, op: 'stop', body: {}, sig: MOCK_SIG,
})

describe('httpTransport against the mocked browser leg (protocol 8.3)', () => {
  it('lists and loads sessions', async () => {
    expect((await httpTransport.listSessions()).map(s => s.session_id)).toEqual([MOCK_SESSION_ID])
    expect((await httpTransport.getSession(MOCK_SESSION_ID)).harness_pub).toBeTruthy()
  })

  it('returns not_found for someone else\'s or a missing session', async () => {
    await expect(httpTransport.getSession('rs_AAAAAAAAAAAAAAAAAAAAAA')).rejects.toMatchObject({ status: 404, code: 'not_found' })
  })

  it('streams attach, snapshots and envelopes, from from_seq', async () => {
    relay.chat({ type: 'notice', level: 'info', code: null, text: 'one' })
    relay.chat({ type: 'notice', level: 'info', code: null, text: 'two' })
    const { got, h } = collect()
    const close = httpTransport.openEvents(MOCK_SESSION_ID, 2, h)
    await vi.waitFor(() => expect(got.envelopes).toHaveLength(2))
    expect(got.open).toBe(true)
    expect(got.relay[0]).toMatchObject({ type: 'attached', online: true })
    expect(got.snapshots).toHaveLength(1) // the hello
    expect(got.envelopes.map(w => JSON.parse(w).seq)).toEqual([2, 3])
    // Live events keep coming on the open stream.
    relay.setOnline(false)
    await vi.waitFor(() => expect(got.relay.at(-1)).toEqual({ type: 'offline' }))
    close()
  })

  it('posts commands, and a 409 for an offline laptop comes back as a RelayError', async () => {
    await httpTransport.postCommand(MOCK_SESSION_ID, command(Date.now()))
    expect(relay.received).toHaveLength(1)
    relay.setOnline(false)
    await expect(httpTransport.postCommand(MOCK_SESSION_ID, command(Date.now() + 1))).rejects.toMatchObject({ status: 409, code: 'harness_offline' })
  })

  it('reads a proxy 502/503/504 without a relay code as the relay service being down', async () => {
    for (const status of [502, 503, 504]) {
      server.use(http.get('*/api/sessions', () => new HttpResponse('no healthy upstream', { status })))
      await expect(httpTransport.listSessions()).rejects.toMatchObject({
        status, code: 'relay_down', message: "Remote control is unavailable right now (the relay service isn't running).",
      })
    }
    // A 503 the relay itself sends keeps its code and message.
    server.use(http.get('*/api/sessions', () => HttpResponse.json({ code: 'relay_unavailable', error: 'the replica holding this session is unreachable; try again' }, { status: 503 })))
    await expect(httpTransport.listSessions()).rejects.toMatchObject({ code: 'relay_unavailable', message: 'the replica holding this session is unreachable; try again' })
    // Other failures are unchanged.
    server.use(http.get('*/api/sessions', () => new HttpResponse('boom', { status: 500 })))
    await expect(httpTransport.listSessions()).rejects.toMatchObject({ code: 'http_500', message: 'Request failed (500)' })
  })

  it('disconnects', async () => {
    await httpTransport.disconnect(MOCK_SESSION_ID)
    expect(relay.ended).toBe('disconnected')
  })
})

describe('SSEParser', () => {
  it('parses named events, multi-line data and comments across chunk boundaries', () => {
    const out: [string, string][] = []
    const p = new SSEParser((n, d) => out.push([n, d]))
    p.push(': ka\n\nretry: 3000\n\nid: 7\nevent: envel')
    p.push('ope\ndata: {"a":1}\r')
    p.push('\n\r\ndata: x\ndata: y\n\n')
    expect(out).toEqual([['envelope', '{"a":1}'], ['message', 'x\ny']])
  })
})
