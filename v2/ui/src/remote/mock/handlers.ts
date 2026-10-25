/**
 * MSW handlers for the relay's browser leg (protocol 8.3), backed by a
 * MockRelay. They let the real HTTP transport (fetch + SSE parsing) be tested
 * end to end before amp-api has the routes.
 */
import { http, HttpResponse } from 'msw'
import { RelayError } from '../transport'
import type { CommandEnvelope } from '../types'
import type { PairMessage } from '../protocol'
import type { MockRelay } from './mockRelay'

const err = (e: unknown) => {
  if (e instanceof RelayError) return HttpResponse.json({ error: e.message, code: e.code }, { status: e.status })
  throw e
}

const sse = (frame: { event: string; data: string; id?: number }) =>
  `${frame.id != null ? `id: ${frame.id}\n` : ''}event: ${frame.event}\ndata: ${frame.data}\n\n`

export function relayHandlers(relay: MockRelay) {
  const notFound = () => HttpResponse.json({ error: 'Not found', code: 'not_found' }, { status: 404 })
  return [
    http.get('*/api/sessions', () => HttpResponse.json({ sessions: relay.ended ? [] : [relay.summary()] })),

    http.get('*/api/sessions/:id', ({ params }) => {
      if (params.id !== relay.sessionId) return notFound()
      if (relay.ended) return HttpResponse.json({ error: 'Ended', code: 'session_ended', reason: relay.ended }, { status: 410 })
      return HttpResponse.json(relay.summary())
    }),

    http.get('*/api/sessions/:id/events', ({ params, request }) => {
      if (params.id !== relay.sessionId) return notFound()
      const from = new URL(request.url).searchParams.get('from_seq')
      const enc = new TextEncoder()
      let unsub = () => {}
      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(enc.encode('retry: 3000\n\n: ka\n\n'))
          unsub = relay.subscribe(from ? Number(from) : null, f => {
            const id = f.event === 'envelope' ? (JSON.parse(f.data) as { seq: number }).seq : undefined
            try { controller.enqueue(enc.encode(sse({ ...f, id }))) } catch { unsub() }
          })
          request.signal.addEventListener('abort', () => { unsub(); try { controller.close() } catch { /* closed */ } })
        },
        cancel() { unsub() },
      })
      return new HttpResponse(stream, { headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' } })
    }),

    http.post('*/api/sessions/:id/commands', async ({ params, request }) => {
      if (params.id !== relay.sessionId) return notFound()
      try {
        relay.receive(await request.json() as CommandEnvelope)
        return HttpResponse.json({ status: 'forwarded' }, { status: 202 })
      } catch (e) { return err(e) }
    }),

    http.post('*/api/sessions/:id/pair', async ({ params, request }) => {
      if (params.id !== relay.sessionId) return notFound()
      const msg = await request.json() as PairMessage
      relay.emit('pair_result', { pairing_id: msg.request.pairing_id, result: 'accepted', code: null, tries_left: null,
        pinned_until_ms: Date.now() + 30 * 86_400_000 }, msg.request.device_id)
      return HttpResponse.json({ status: 'pending', expires_at_ms: Date.now() + 300_000 }, { status: 202 })
    }),

    http.post('*/api/sessions/:id/disconnect', ({ params }) => {
      if (params.id !== relay.sessionId) return notFound()
      relay.end('disconnected')
      return new HttpResponse(null, { status: 204 })
    }),
  ]
}
