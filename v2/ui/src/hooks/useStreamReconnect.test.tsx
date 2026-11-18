import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderHook } from '@testing-library/react'
import { HIDDEN_CLOSE_MS, SSE_CLOSE_DELAY_MS, sseSubscriberCount, subscribeSSE } from './useSSE'
import { useStreamReconnect } from './useStreamReconnect'
import { resetSession } from '../lib/session'
import type { SSEEvent } from '../types'

class FakeEventSource {
  static instances: FakeEventSource[] = []
  url: string
  readyState = 0
  closed = false
  onmessage: ((e: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }
  close() { this.closed = true; this.readyState = 2 }
  emit(event: Partial<SSEEvent>) { this.onmessage?.({ data: JSON.stringify(event) }) }
}

const live = () => FakeEventSource.instances.filter(s => !s.closed)
// The stream dropped and the browser reconnected it: the server sends a new connected.
const reconnect = (es: FakeEventSource) => { es.onerror?.(); es.emit({ type: 'connected' }) }

/** A view that re-reads on reconnect; setReady is its first load settling. */
function renderView(ready: boolean) {
  const onReconnect = vi.fn()
  const view = renderHook((props: { ready: boolean }) => useStreamReconnect(onReconnect, props.ready), {
    initialProps: { ready },
  })
  return { onReconnect, setReady: (next: boolean) => view.rerender({ ready: next }), unmount: view.unmount }
}

// This tab's visibility. useSSE pauses a tab's streams HIDDEN_CLOSE_MS after it is hidden.
let visibility: DocumentVisibilityState = 'visible'
const setVisibility = (state: DocumentVisibilityState) => {
  visibility = state
  document.dispatchEvent(new Event('visibilitychange'))
}

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
  visibility = 'visible'
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  resetSession()
  delete (document as { visibilityState?: DocumentVisibilityState }).visibilityState
})

// jsdom has no Web Locks, so this block runs useSSE's per-tab path: the tab
// opens its own stream.
describe('useStreamReconnect', () => {
  it('calls onReconnect once for each connected that arrives while ready, and for nothing else', () => {
    const view = renderView(true)
    const [es] = live()
    es.emit({ type: 'task.updated', project_id: 1 })
    es.emit({ type: 'project.created' })
    expect(view.onReconnect).not.toHaveBeenCalled()
    reconnect(es)
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    reconnect(es)
    expect(view.onReconnect).toHaveBeenCalledTimes(2)
    view.unmount()
  })

  it('drops a connected that arrives while not ready, such as the opening frame during the first load', () => {
    const view = renderView(false)
    const [es] = live()
    es.emit({ type: 'connected' })
    view.setReady(true) // the first load settled; the dropped frame is not replayed
    expect(view.onReconnect).not.toHaveBeenCalled()
    reconnect(es)
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    view.setReady(false) // reloading, e.g. after a filter change
    reconnect(es)
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    view.unmount()
  })

  it('a view mounted on a stream that is already open is called on the next reconnect', () => {
    const other = subscribeSSE('/api/events', () => {}) // another view holds the stream
    const [es] = live()
    es.emit({ type: 'connected' }) // its opening frame, before this view mounted
    const view = renderView(true)
    expect(FakeEventSource.instances).toHaveLength(1) // joined the open stream
    expect(view.onReconnect).not.toHaveBeenCalled()
    reconnect(es)
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    view.unmount()
    other()
  })

  it('is called once when a tab that was hidden long enough to drop its stream is shown again', () => {
    vi.useFakeTimers()
    const view = renderView(false)
    const [first] = live()
    first.emit({ type: 'connected' })
    view.setReady(true)
    setVisibility('hidden')
    vi.advanceTimersByTime(HIDDEN_CLOSE_MS)
    expect(first.closed).toBe(true)
    setVisibility('visible')
    const [second] = live()
    expect(second).not.toBe(first)
    second.emit({ type: 'connected' })
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    view.unmount()
  })

  it('uses the latest onReconnect and ready without resubscribing', () => {
    const first = vi.fn(), second = vi.fn()
    const view = renderHook(({ fn, ready }) => useStreamReconnect(fn, ready), {
      initialProps: { fn: first, ready: false },
    })
    view.rerender({ fn: second, ready: true })
    const [es] = live()
    reconnect(es)
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
    expect(FakeEventSource.instances).toHaveLength(1)
    expect(sseSubscriberCount()).toBe(1)
    view.unmount()
  })

  it('unsubscribes on unmount', () => {
    vi.useFakeTimers()
    const other = subscribeSSE('/api/events', () => {})
    const view = renderView(true)
    expect(sseSubscriberCount()).toBe(2)
    view.unmount()
    expect(sseSubscriberCount()).toBe(1)
    const [es] = live()
    reconnect(es)
    expect(view.onReconnect).not.toHaveBeenCalled()
    other()
    vi.advanceTimersByTime(SSE_CLOSE_DELAY_MS)
    expect(es.closed).toBe(true) // the last subscriber closed it, after the close delay
  })
})

// --- Several tabs of one browser (useSSE #1582) -----------------------------
// This file's module copy is this tab. Another tab is a fresh copy of useSSE
// with its own document. Both share the browser's Web Locks, BroadcastChannel
// and EventSource fakes, as in useSSE.test.tsx.

/** Web Locks for one exclusive lock name: FIFO, granted asynchronously. */
class FakeLocks {
  private holder: object | null = null
  private waiting: { run: () => void }[] = []
  get held() { return this.holder !== null }
  request(name: string, options: { signal?: AbortSignal }, callback: (lock: { name: string }) => unknown) {
    return new Promise((resolve, reject) => {
      const aborted = () => new DOMException('The request was aborted.', 'AbortError')
      if (options.signal?.aborted) { reject(aborted()); return }
      const req = {
        run: () => {
          this.holder = req
          Promise.resolve()
            .then(() => callback({ name }))
            .then(resolve, reject)
            .finally(() => { if (this.holder === req) { this.holder = null; this.next() } })
        },
      }
      options.signal?.addEventListener('abort', () => {
        const i = this.waiting.indexOf(req)
        if (i >= 0) { this.waiting.splice(i, 1); reject(aborted()) }
      })
      this.waiting.push(req)
      this.next()
    })
  }
  private next() { if (!this.holder && this.waiting.length) this.waiting.shift()!.run() }
}

class FakeBroadcastChannel {
  static all: FakeBroadcastChannel[] = []
  onmessage: ((e: { data: unknown }) => void) | null = null
  closed = false
  name: string
  constructor(name: string) {
    this.name = name
    FakeBroadcastChannel.all.push(this)
  }
  postMessage(data: unknown) {
    if (this.closed) throw new DOMException('Channel is closed', 'InvalidStateError')
    for (const ch of FakeBroadcastChannel.all) {
      if (ch === this || ch.closed || ch.name !== this.name) continue
      const copy = structuredClone(data)
      void Promise.resolve().then(() => { if (!ch.closed) ch.onmessage?.({ data: copy }) })
    }
  }
  close() { this.closed = true }
}

/** Another visible tab of this browser: a fresh copy of useSSE. */
async function openOtherTab() {
  const doc = new EventTarget()
  Object.defineProperty(doc, 'visibilityState', { get: () => 'visible' })
  const realDocument = Object.getOwnPropertyDescriptor(globalThis, 'document')!
  vi.resetModules()
  Object.defineProperty(globalThis, 'document', { configurable: true, get: () => doc })
  try {
    return await import('./useSSE')
  } finally {
    Object.defineProperty(globalThis, 'document', realDocument)
  }
}

// Let the fakes' promise hops (lock grants, channel messages) run.
const settle = async () => { for (let i = 0; i < 100; i++) await Promise.resolve() }

describe('useStreamReconnect across tabs (Web Locks + BroadcastChannel)', () => {
  let locks: FakeLocks
  beforeEach(() => {
    locks = new FakeLocks()
    Object.defineProperty(navigator, 'locks', { configurable: true, value: locks })
    FakeBroadcastChannel.all = []
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
  })
  afterEach(() => { delete (navigator as { locks?: unknown }).locks })

  /** Another tab leads: it holds the lock and the browser's only stream, already up. */
  async function leaderTab() {
    const other = await openOtherTab()
    const stop = other.subscribeSSE('/api/events', () => {})
    await settle()
    const [es] = live()
    es.emit({ type: 'connected' })
    return { es, stop }
  }

  it('a follower tab is called for the reconnect its leader relays, not for its joining connected', async () => {
    const leader = await leaderTab()
    const seen: string[] = []
    const probe = subscribeSSE('/api/events', e => { seen.push(e.type) })
    const view = renderView(false) // this tab's first load is in flight
    await settle()
    expect(seen).toEqual(['connected']) // this tab joined the open stream and was told so
    expect(view.onReconnect).not.toHaveBeenCalled()
    view.setReady(true)
    reconnect(leader.es)
    await settle()
    expect(seen).toEqual(['connected', 'connected'])
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    expect(FakeEventSource.instances).toHaveLength(1) // relayed: this tab opened no stream
    view.unmount(); probe(); leader.stop()
  })

  it('a view mounted on a follower tab that is already listening is called on the next reconnect', async () => {
    const leader = await leaderTab()
    const other = subscribeSSE('/api/events', () => {}) // another view in this tab
    await settle()
    const view = renderView(true)
    await settle()
    expect(view.onReconnect).not.toHaveBeenCalled()
    reconnect(leader.es)
    await settle()
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    view.unmount(); other(); leader.stop()
  })

  it('a follower tab that comes back after being hidden is called exactly once', async () => {
    const leader = await leaderTab()
    vi.useFakeTimers()
    const view = renderView(false)
    await settle()
    view.setReady(true)
    setVisibility('hidden')
    vi.advanceTimersByTime(HIDDEN_CLOSE_MS) // this tab stops listening
    await settle()
    leader.es.emit({ type: 'task.updated', project_id: 1 }) // missed while away
    setVisibility('visible')
    await settle()
    expect(view.onReconnect).toHaveBeenCalledTimes(1) // the leader's "up", delivered as connected
    await settle()
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    expect(FakeEventSource.instances).toHaveLength(1)
    view.unmount(); leader.stop()
  })

  it('works in the tab that leads', async () => {
    vi.useFakeTimers()
    const view = renderView(false)
    await settle()
    expect(locks.held).toBe(true) // the only tab, so it took the lock and opened the stream
    const [es] = live()
    es.emit({ type: 'connected' }) // the opening frame, during the first load
    view.setReady(true)
    expect(view.onReconnect).not.toHaveBeenCalled()
    reconnect(es)
    expect(view.onReconnect).toHaveBeenCalledTimes(1)
    view.unmount()
    vi.advanceTimersByTime(SSE_CLOSE_DELAY_MS)
    await settle()
    expect(locks.held).toBe(false)
  })
})
