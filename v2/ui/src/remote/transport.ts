/**
 * How the page talks to the relay's browser leg (protocol 8.3). Same-origin
 * only: the page never takes an endpoint from its URL (T-UI-8).
 *
 * The event stream is read with fetch + a small SSE parser rather than
 * EventSource, so the page sees keepalive comments (EventSource hides them and
 * can't notice a silent stream) and controls `from_seq` itself on reconnect.
 */
import type { PairMessage } from './protocol'
import type { CommandEnvelope, RelayFrame, SessionDetail, SessionSummary } from './types'

export class RelayError extends Error {
  status: number
  code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'RelayError'
    this.status = status
    this.code = code
  }
}

export interface EventStreamHandlers {
  /** The stream is open (headers received). */
  onOpen(): void
  /** Any bytes arrived, keepalives included. */
  onActivity(): void
  /** `event: envelope` (`id` is the SSE id, the seq). */
  onEnvelope(wire: string): void
  /** `event: snapshot`. */
  onSnapshot(wire: string): void
  /** `event: relay`: unsigned status from AMP. */
  onRelay(frame: RelayFrame): void
  /** The stream ended or failed; `err` is set for HTTP errors. */
  onClose(err?: RelayError): void
}

export interface RelayTransport {
  listSessions(): Promise<SessionSummary[]>
  getSession(id: string): Promise<SessionDetail>
  /** Opens the event stream; returns a function that closes it. */
  openEvents(id: string, fromSeq: number | null, h: EventStreamHandlers): () => void
  /** POSTs one signed command. Resolves on 202; rejects with RelayError otherwise. */
  postCommand(id: string, env: CommandEnvelope): Promise<void>
  disconnect(id: string): Promise<void>
  /** Ends every live session of the caller and forgets their devices (protocol 8.3). */
  revokeAll(): Promise<{ ended: number; devices_revoked: number }>
  /** POSTs a pair request (protocol 7.3). Resolves on 202 with the pairing window's expiry. */
  pair(id: string, msg: PairMessage): Promise<{ expires_at_ms: number | null }>
}

/** Shown when the relay service isn't running (amp-relay scaled to 0 or not deployed). */
export const RELAY_DOWN_MESSAGE = "Remote control is unavailable right now (the relay service isn't running)."

/**
 * The error for a failed relay call. A 502/503/504 without a relay `code` is
 * the proxy in front of AMP answering for a relay that isn't there (Envoy's
 * "no healthy upstream"), the same rule the harness uses (relay.rs). A 503
 * that the relay itself sends carries a code (relay_unavailable,
 * audit_unavailable) and keeps its own message.
 */
async function errorFrom(res: Response): Promise<RelayError> {
  const raw = await res.text().catch(() => '')
  let body: { error?: string; code?: string } = {}
  try {
    const parsed: unknown = JSON.parse(raw)
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) body = parsed as typeof body
  } catch { /* not JSON: a proxy's text page */ }
  if (!body.code && (res.status === 502 || res.status === 503 || res.status === 504)) {
    return new RelayError(res.status, 'relay_down', RELAY_DOWN_MESSAGE)
  }
  return new RelayError(res.status, body.code ?? `http_${res.status}`, body.error ?? `Request failed (${res.status})`)
}

const enc = encodeURIComponent

/** Feeds SSE text and emits complete events (event name, data, id). */
export class SSEParser {
  private buf = ''
  private event = ''
  private data: string[] = []
  private onEvent: (name: string, data: string) => void
  constructor(onEvent: (name: string, data: string) => void) { this.onEvent = onEvent }

  push(chunk: string) {
    this.buf += chunk
    let nl: number
    while ((nl = this.buf.search(/\r\n|\n|\r/)) >= 0) {
      if (nl === this.buf.length - 1 && this.buf.endsWith('\r')) break // maybe half of a CRLF split across chunks
      const line = this.buf.slice(0, nl)
      this.buf = this.buf.slice(nl + (this.buf.startsWith('\r\n', nl) ? 2 : 1))
      this.line(line)
    }
  }

  private line(line: string) {
    if (line === '') {
      if (this.data.length) this.onEvent(this.event || 'message', this.data.join('\n'))
      this.event = ''
      this.data = []
      return
    }
    if (line.startsWith(':')) return // comment (keepalive)
    const i = line.indexOf(':')
    const field = i < 0 ? line : line.slice(0, i)
    let value = i < 0 ? '' : line.slice(i + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') this.event = value
    else if (field === 'data') this.data.push(value)
    // `id` and `retry` are handled by the caller (seq tracking and backoff)
  }
}

export const httpTransport: RelayTransport = {
  async listSessions() {
    const res = await fetch('/api/sessions', { headers: { Accept: 'application/json' } })
    if (!res.ok) throw await errorFrom(res)
    const body = await res.json() as { sessions?: SessionSummary[] }
    return body.sessions ?? []
  },

  async getSession(id) {
    const res = await fetch(`/api/sessions/${enc(id)}`, { headers: { Accept: 'application/json' } })
    if (!res.ok) throw await errorFrom(res)
    return res.json()
  },

  openEvents(id, fromSeq, h) {
    const ctl = new AbortController()
    const url = `/api/sessions/${enc(id)}/events${fromSeq != null ? `?from_seq=${fromSeq}` : ''}`
    const parser = new SSEParser((name, data) => {
      if (name === 'envelope') h.onEnvelope(data)
      else if (name === 'snapshot') h.onSnapshot(data)
      else if (name === 'relay') {
        try { h.onRelay(JSON.parse(data) as RelayFrame) } catch { /* not a frame we know */ }
      }
    })
    let closed = false
    const finish = (err?: RelayError) => { if (!closed) { closed = true; h.onClose(err) } }
    ;(async () => {
      try {
        const res = await fetch(url, { headers: { Accept: 'text/event-stream' }, signal: ctl.signal, cache: 'no-store' })
        if (!res.ok || !res.body) { finish(await errorFrom(res)); return }
        h.onOpen()
        const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
        for (;;) {
          const { value, done } = await reader.read()
          if (done) break
          h.onActivity()
          parser.push(value)
        }
        finish()
      } catch {
        finish()
      }
    })()
    return () => { closed = true; ctl.abort() }
  },

  async postCommand(id, env) {
    const res = await fetch(`/api/sessions/${enc(id)}/commands`, {
      method: 'POST', headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: JSON.stringify(env),
    })
    if (res.status !== 202 && !res.ok) throw await errorFrom(res)
  },

  async disconnect(id) {
    const res = await fetch(`/api/sessions/${enc(id)}/disconnect`, { method: 'POST' })
    if (!res.ok) throw await errorFrom(res)
  },

  async revokeAll() {
    const res = await fetch('/api/sessions/revoke-all', { method: 'POST' })
    if (!res.ok) throw await errorFrom(res)
    const body = await res.json().catch(() => ({})) as { ended?: number; devices_revoked?: number }
    return { ended: body.ended ?? 0, devices_revoked: body.devices_revoked ?? 0 }
  },

  async pair(id, msg) {
    const res = await fetch(`/api/sessions/${enc(id)}/pair`, {
      method: 'POST', headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: JSON.stringify(msg),
    })
    if (!res.ok) throw await errorFrom(res)
    const body = await res.json().catch(() => ({})) as { expires_at_ms?: number }
    return { expires_at_ms: typeof body.expires_at_ms === 'number' ? body.expires_at_ms : null }
  },
}
