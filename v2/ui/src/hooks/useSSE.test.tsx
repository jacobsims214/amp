import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, renderHook } from '@testing-library/react'
import { HIDDEN_CLOSE_MS, sseSubscriberCount, subscribeSSE, useSSE } from './useSSE'
import type { SSEEvent } from '../types'
import { isSessionEnded, markSessionEnded, resetSession } from '../lib/session'

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

const open = () => FakeEventSource.instances.filter(s => !s.closed)

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})
afterEach(() => { vi.unstubAllGlobals(); resetSession() })

// jsdom has no Web Locks, so this block runs the per-tab path: each tab its
// own stream, as before cross-tab sharing (see 'across tabs' below).
describe('useSSE', () => {
  it('shares one EventSource across every subscriber', () => {
    function Page() {
      useSSE(1, () => {})
      useSSE(1, () => {})
      useSSE(null, () => {})
      return null
    }
    const { unmount } = render(<><Page /><Page /></>)
    expect(FakeEventSource.instances).toHaveLength(1)
    expect(FakeEventSource.instances[0].url).toBe('/api/events')
    expect(sseSubscriberCount()).toBe(6)
    unmount()
  })

  it('closes the stream when the last subscriber leaves, and reopens for the next', () => {
    const a = renderHook(() => useSSE(1, () => {}))
    const b = renderHook(() => useSSE(2, () => {}))
    const [es] = FakeEventSource.instances
    a.unmount()
    expect(es.closed).toBe(false)
    b.unmount()
    expect(es.closed).toBe(true)
    expect(sseSubscriberCount()).toBe(0)

    const c = renderHook(() => useSSE(1, () => {}))
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(open()).toHaveLength(1)
    c.unmount()
  })

  it('fans each event out to every subscriber, filtered by project', () => {
    const p1 = vi.fn(), p1b = vi.fn(), p2 = vi.fn(), all = vi.fn()
    const hooks = [
      renderHook(() => useSSE(1, p1)),
      renderHook(() => useSSE(1, p1b)),
      renderHook(() => useSSE(2, p2)),
      renderHook(() => useSSE(null, all)),
    ]
    const [es] = FakeEventSource.instances
    act(() => {
      es.emit({ type: 'task.updated', project_id: 1 })
      es.emit({ type: 'project.created' })
    })
    expect(p1).toHaveBeenCalledTimes(2)
    expect(p1b).toHaveBeenCalledTimes(2)
    expect(p2).toHaveBeenCalledTimes(1) // only the event without a project
    expect(all).toHaveBeenCalledTimes(2)
    hooks.forEach(h => h.unmount())
  })

  it('uses the latest handler without reconnecting', () => {
    const first = vi.fn(), second = vi.fn()
    const h = renderHook(({ fn }) => useSSE(1, fn), { initialProps: { fn: first } })
    h.rerender({ fn: second })
    const [es] = FakeEventSource.instances
    act(() => es.emit({ type: 'task.updated', project_id: 1 }))
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
    expect(FakeEventSource.instances).toHaveLength(1)
    h.unmount()
  })

  it('keeps one stream when a subscriber changes project', () => {
    const h = renderHook(({ pid }) => useSSE(pid, () => {}), { initialProps: { pid: 1 } })
    const other = renderHook(() => useSSE(1, () => {}))
    h.rerender({ pid: 2 })
    expect(open()).toHaveLength(1)
    h.unmount(); other.unmount()
  })

  it('leaves reconnecting to the browser after an error', () => {
    const fn = vi.fn()
    const h = renderHook(() => useSSE(1, fn))
    const [es] = FakeEventSource.instances
    act(() => es.onerror?.())
    expect(es.closed).toBe(false)
    expect(FakeEventSource.instances).toHaveLength(1)
    act(() => es.emit({ type: 'task.updated', project_id: 1 }))
    expect(fn).toHaveBeenCalledTimes(1)
    h.unmount()
  })

  it('reopens a stream the browser gave up on when someone new subscribes', () => {
    const fn = vi.fn()
    const a = renderHook(() => useSSE(1, fn))
    const [dead] = FakeEventSource.instances
    dead.readyState = 2 // CLOSED: the browser stopped retrying
    const b = renderHook(() => useSSE(1, () => {}))
    expect(FakeEventSource.instances).toHaveLength(2)
    const live = FakeEventSource.instances[1]
    act(() => live.emit({ type: 'task.updated', project_id: 1 }))
    expect(fn).toHaveBeenCalledTimes(1) // the earlier subscriber moved to the new stream
    a.unmount(); b.unmount()
    expect(live.closed).toBe(true)
  })

  it('ignores malformed events and survives a throwing listener', () => {
    const err = vi.spyOn(console, 'error').mockImplementation(() => {})
    const ok = vi.fn()
    const offBad = subscribeSSE('/api/events', () => { throw new Error('boom') })
    const offOk = subscribeSSE('/api/events', ok)
    const [es] = FakeEventSource.instances
    es.onmessage?.({ data: 'not json' })
    es.emit({ type: 'task.updated' })
    expect(ok).toHaveBeenCalledTimes(1)
    expect(err).toHaveBeenCalled()
    offBad(); offBad() // a second call is a no-op
    expect(sseSubscriberCount()).toBe(1)
    offOk()
    expect(es.closed).toBe(true)
  })

  describe('once the session ended (#153)', () => {
    it('closes the stream and does not reopen it for new subscribers', () => {
      const a = renderHook(() => useSSE(1, () => {}))
      const [es] = FakeEventSource.instances
      act(() => markSessionEnded())
      expect(es.closed).toBe(true)
      const b = renderHook(() => useSSE(2, () => {}))
      expect(FakeEventSource.instances).toHaveLength(1) // no reconnect loop
      a.unmount(); b.unmount()
    })

    it('a stream the browser gave up on asks whether the session ended', async () => {
      const fetchMock = vi.fn(async () => new Response('Unauthorized', { status: 401 }))
      vi.stubGlobal('fetch', fetchMock)
      const a = renderHook(() => useSSE(1, () => {}))
      const [es] = FakeEventSource.instances
      es.readyState = 2 // CLOSED: the proxy answered 401, not a stream
      es.onerror?.()
      await vi.waitFor(() => expect(isSessionEnded()).toBe(true))
      expect(fetchMock).toHaveBeenCalledWith('/api/me', expect.objectContaining({ redirect: 'manual' }))
      a.unmount()
    })

    it('a blip the browser retries by itself probes nothing', () => {
      const fetchMock = vi.fn()
      vi.stubGlobal('fetch', fetchMock)
      const a = renderHook(() => useSSE(1, () => {}))
      FakeEventSource.instances[0].onerror?.() // readyState CONNECTING: reconnecting
      expect(fetchMock).not.toHaveBeenCalled()
      expect(isSessionEnded()).toBe(false)
      a.unmount()
    })
  })

  describe('while the tab is hidden', () => {
    let visibility: DocumentVisibilityState = 'visible'
    const setVisibility = (v: DocumentVisibilityState) => {
      visibility = v
      act(() => { document.dispatchEvent(new Event('visibilitychange')) })
    }
    const hide = () => setVisibility('hidden')
    const show = () => setVisibility('visible')
    const wait = (ms: number) => act(() => { vi.advanceTimersByTime(ms) })

    beforeEach(() => {
      vi.useFakeTimers()
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
    })
    afterEach(() => {
      show() // resets the module's paused state for the next test
      vi.useRealTimers()
    })

    it('closes the stream after 30 s, keeping its subscribers and the session', () => {
      const fetchMock = vi.fn()
      vi.stubGlobal('fetch', fetchMock)
      const a = renderHook(() => useSSE(1, () => {}))
      const [es] = FakeEventSource.instances
      hide()
      wait(HIDDEN_CLOSE_MS - 1)
      expect(es.closed).toBe(false)
      wait(1)
      expect(es.closed).toBe(true)
      expect(sseSubscriberCount()).toBe(1)
      es.onerror?.() // a late error from the stream we closed asks nothing
      expect(fetchMock).not.toHaveBeenCalled()
      expect(isSessionEnded()).toBe(false)
      a.unmount()
    })

    it('a quick hide and show closes nothing', () => {
      const a = renderHook(() => useSSE(1, () => {}))
      const [es] = FakeEventSource.instances
      hide()
      wait(HIDDEN_CLOSE_MS / 2)
      show()
      wait(HIDDEN_CLOSE_MS * 2)
      expect(es.closed).toBe(false)
      expect(FakeEventSource.instances).toHaveLength(1)
      // and the timer starts over on the next hide
      hide()
      wait(HIDDEN_CLOSE_MS - 1)
      expect(es.closed).toBe(false)
      a.unmount()
    })

    it('a subscriber arriving after the close opens nothing', () => {
      const a = renderHook(() => useSSE(1, () => {}))
      hide()
      wait(HIDDEN_CLOSE_MS)
      const b = renderHook(() => useSSE(2, () => {})) // joins the paused stream
      expect(open()).toHaveLength(0)
      expect(sseSubscriberCount()).toBe(2)
      a.unmount(); b.unmount()
      const c = renderHook(() => useSSE(1, () => {})) // a fresh stream, still hidden
      expect(open()).toHaveLength(0)
      expect(FakeEventSource.instances).toHaveLength(1)
      c.unmount()
    })

    it('reopens on show for the subscribers that remain, old and new', () => {
      const early = vi.fn(), late = vi.fn()
      const a = renderHook(() => useSSE(1, early))
      hide()
      wait(HIDDEN_CLOSE_MS)
      const b = renderHook(() => useSSE(1, late))
      show()
      expect(FakeEventSource.instances).toHaveLength(2)
      const [live] = open()
      expect(live).toBe(FakeEventSource.instances[1])
      act(() => live.emit({ type: 'task.updated', project_id: 1 }))
      expect(early).toHaveBeenCalledTimes(1)
      expect(late).toHaveBeenCalledTimes(1)
      a.unmount(); b.unmount()
      expect(live.closed).toBe(true)
    })

    it('opens nothing on show when everyone left while hidden', () => {
      const a = renderHook(() => useSSE(1, () => {}))
      hide()
      wait(HIDDEN_CLOSE_MS)
      a.unmount()
      expect(sseSubscriberCount()).toBe(0)
      show()
      expect(FakeEventSource.instances).toHaveLength(1)
    })

    it('does not reopen on show once the session ended', () => {
      const a = renderHook(() => useSSE(1, () => {}))
      hide()
      wait(HIDDEN_CLOSE_MS)
      act(() => markSessionEnded())
      show()
      expect(FakeEventSource.instances).toHaveLength(1)
      const b = renderHook(() => useSSE(1, () => {}))
      expect(FakeEventSource.instances).toHaveLength(1)
      a.unmount(); b.unmount()
    })
  })
})

// --- Several tabs of one browser -------------------------------------------
// Each simulated tab is its own copy of the module, with its own registry,
// session and document (visibility). They share the browser's Web Locks,
// BroadcastChannel and EventSource fakes, so `open()` counts every stream
// the browser holds.

type Grant = (lock: { name: string }) => unknown

/** Web Locks for one exclusive lock name: FIFO, granted asynchronously. */
class FakeLocks {
  private holder: object | null = null
  private waiting: { run: () => void }[] = []
  /** How many requests are waiting (not counting the holder). */
  get queued() { return this.waiting.length }
  get held() { return this.holder !== null }
  request(name: string, options: { signal?: AbortSignal }, callback: Grant): Promise<unknown> {
    return new Promise((resolve, reject) => {
      const aborted = () => new DOMException('The request was aborted.', 'AbortError')
      if (options.signal?.aborted) { reject(aborted()); return }
      const req = {
        run: () => {
          this.holder = req
          // Held until the callback's promise settles.
          Promise.resolve()
            .then(() => callback({ name }))
            .then(resolve, reject)
            .finally(() => { if (this.holder === req) { this.holder = null; this.next() } })
        },
      }
      // Aborting only withdraws a request that is still waiting.
      options.signal?.addEventListener('abort', () => {
        const i = this.waiting.indexOf(req)
        if (i >= 0) { this.waiting.splice(i, 1); reject(aborted()) }
      })
      this.waiting.push(req)
      this.next()
    })
  }
  /** The browser lets go of a closed tab's lock without its code running. */
  drop() { this.holder = null; this.next() }
  private next() { if (!this.holder && this.waiting.length) this.waiting.shift()!.run() }
}

class FakeBroadcastChannel {
  static all: FakeBroadcastChannel[] = []
  static sent: { kind: string }[] = []
  onmessage: ((e: { data: unknown }) => void) | null = null
  closed = false
  name: string
  constructor(name: string) {
    this.name = name
    FakeBroadcastChannel.all.push(this)
  }
  postMessage(data: unknown) {
    if (this.closed) throw new DOMException('Channel is closed', 'InvalidStateError')
    FakeBroadcastChannel.sent.push(data as { kind: string })
    // Every other open channel of that name, never the sender, in order.
    for (const ch of FakeBroadcastChannel.all) {
      if (ch === this || ch.closed || ch.name !== this.name) continue
      const copy = structuredClone(data)
      void Promise.resolve().then(() => { if (!ch.closed) ch.onmessage?.({ data: copy }) })
    }
  }
  close() { this.closed = true }
}

type SSEModule = typeof import('./useSSE')
type SessionModule = typeof import('../lib/session')

interface Tab {
  sse: SSEModule
  session: SessionModule
  /** What this tab's subscribers received. */
  events: SSEEvent[]
  /** The channels this tab opened (they close with the tab). */
  channels: FakeBroadcastChannel[]
  subscribe(url?: string): () => void
  hide(): void
  show(): void
}

async function openTab({ hidden = false } = {}): Promise<Tab> {
  let visibility: DocumentVisibilityState = hidden ? 'hidden' : 'visible'
  const doc = new EventTarget()
  Object.defineProperty(doc, 'visibilityState', { get: () => visibility })
  const realDocument = Object.getOwnPropertyDescriptor(globalThis, 'document')!
  vi.resetModules()
  Object.defineProperty(globalThis, 'document', { configurable: true, get: () => doc })
  let sse!: SSEModule, session!: SessionModule
  try {
    sse = await import('./useSSE')
    session = await import('../lib/session')
  } finally {
    Object.defineProperty(globalThis, 'document', realDocument)
  }
  const events: SSEEvent[] = []
  const channels: FakeBroadcastChannel[] = []
  // Channels opened during a call into this tab are this tab's.
  const track = <T,>(fn: () => T): T => {
    const before = FakeBroadcastChannel.all.length
    const result = fn()
    channels.push(...FakeBroadcastChannel.all.slice(before))
    return result
  }
  const setVisibility = (v: DocumentVisibilityState) => {
    visibility = v
    track(() => doc.dispatchEvent(new Event('visibilitychange')))
  }
  return {
    sse, session, events, channels,
    subscribe: (url = '/api/events') => track(() => sse.subscribeSSE(url, e => { events.push(e) })),
    hide: () => setVisibility('hidden'),
    show: () => setVisibility('visible'),
  }
}

// Let the fakes' promise hops (lock grants, channel messages) run.
const settle = async () => { for (let i = 0; i < 100; i++) await Promise.resolve() }
const connectedCount = (t: Tab) => t.events.filter(e => e.type === 'connected').length
const types = (events: SSEEvent[]) => events.map(e => e.type)

function stubLocks(locks: unknown) {
  Object.defineProperty(navigator, 'locks', { configurable: true, value: locks })
}

describe('across tabs (Web Locks + BroadcastChannel)', () => {
  let locks: FakeLocks
  beforeEach(() => {
    vi.useFakeTimers()
    locks = new FakeLocks()
    stubLocks(locks)
    FakeBroadcastChannel.all = []
    FakeBroadcastChannel.sent = []
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
  })
  afterEach(() => {
    delete (navigator as { locks?: unknown }).locks
    vi.useRealTimers()
  })
  const wait = async (ms: number) => { vi.advanceTimersByTime(ms); await settle() }

  it('opens exactly one EventSource for N visible tabs and relays every frame to the others', async () => {
    const tabs = [await openTab(), await openTab(), await openTab(), await openTab()]
    tabs.forEach(t => t.subscribe())
    await settle()
    expect(open()).toHaveLength(1)
    const [es] = open()
    es.emit({ type: 'connected' })
    es.emit({ type: 'task.updated', project_id: 1 })
    await settle()
    for (const t of tabs) expect(types(t.events)).toEqual(['connected', 'task.updated'])
    // More subscribers in any tab share it too.
    tabs[2].subscribe()
    tabs[0].subscribe()
    await settle()
    expect(FakeEventSource.instances).toHaveLength(1)
    expect(locks.queued).toBe(3) // the other visible tabs wait their turn
  })

  it('a tab that joins a stream that is already up gets one connected, and nobody else does', async () => {
    const a = await openTab()
    a.subscribe()
    await settle()
    const [es] = open()
    es.emit({ type: 'connected' })
    const b = await openTab()
    b.subscribe()
    await settle()
    expect(connectedCount(b)).toBe(1) // the leader's "up", delivered as connected
    expect(connectedCount(a)).toBe(1)
    const c = await openTab()
    c.subscribe()
    await settle()
    expect(connectedCount(c)).toBe(1)
    expect(connectedCount(b)).toBe(1) // c's answer isn't b's
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('hands the stream to a waiting visible tab when the leader leaves', async () => {
    const a = await openTab(), b = await openTab(), c = await openTab()
    const offA = a.subscribe()
    b.subscribe()
    c.subscribe()
    await settle()
    const [first] = open()
    first.emit({ type: 'connected' })
    await settle()
    offA()
    await settle()
    expect(first.closed).toBe(true)
    expect(open()).toHaveLength(1)
    const [second] = open()
    second.emit({ type: 'connected' })
    second.emit({ type: 'task.updated', project_id: 1 })
    await settle()
    // One connected per stream: the second is the catch-up after the gap.
    for (const t of [b, c]) {
      expect(connectedCount(t)).toBe(2)
      expect(t.events.at(-1)?.type).toBe('task.updated')
    }
  })

  it('takes over when the browser closes the leader tab', async () => {
    const a = await openTab(), b = await openTab()
    a.subscribe()
    b.subscribe()
    await settle()
    const [first] = open()
    first.emit({ type: 'connected' })
    await settle()
    // The browser ends a's stream, channel and lock; none of a's code runs.
    first.close()
    a.channels.forEach(ch => ch.close())
    locks.drop()
    await settle()
    expect(open()).toHaveLength(1)
    const [second] = open()
    expect(second).not.toBe(first)
    second.emit({ type: 'connected' })
    await settle()
    expect(connectedCount(b)).toBe(2)
  })

  it('a hidden leader keeps the stream through the grace period, then hands it over', async () => {
    const a = await openTab(), b = await openTab()
    a.subscribe()
    b.subscribe()
    await settle()
    const [first] = open()
    first.emit({ type: 'connected' })
    a.hide()
    await wait(HIDDEN_CLOSE_MS - 1)
    expect(open()).toEqual([first])
    first.emit({ type: 'task.updated', project_id: 1 })
    await settle()
    expect(b.events.at(-1)?.type).toBe('task.updated') // still relayed while hidden
    await wait(1)
    expect(first.closed).toBe(true)
    expect(open()).toHaveLength(1) // b, the visible tab, took over
    expect(locks.queued).toBe(0) // and hidden a isn't waiting to lead again
    const [second] = open()
    second.emit({ type: 'connected' })
    await settle()
    expect(connectedCount(b)).toBe(2)
  })

  it('a quick hide and show of the leader hands nothing over', async () => {
    const a = await openTab(), b = await openTab()
    a.subscribe()
    b.subscribe()
    await settle()
    const [first] = open()
    a.hide()
    await wait(HIDDEN_CLOSE_MS / 2)
    a.show()
    await wait(HIDDEN_CLOSE_MS * 2)
    expect(open()).toEqual([first])
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('a hidden follower costs nothing after 30 s and resyncs once when shown', async () => {
    const a = await openTab(), b = await openTab()
    a.subscribe()
    b.subscribe()
    await settle()
    const [es] = open()
    es.emit({ type: 'connected' })
    await settle()
    expect(locks.queued).toBe(1)
    b.hide()
    await settle()
    expect(locks.queued).toBe(0) // a hidden tab doesn't queue to lead
    es.emit({ type: 'task.updated', project_id: 1 })
    await settle()
    expect(b.events.at(-1)?.type).toBe('task.updated') // delivered during the grace period
    await wait(HIDDEN_CLOSE_MS)
    expect(b.channels.every(ch => ch.closed)).toBe(true) // no channel, no lock, no stream
    const seen = b.events.length
    es.emit({ type: 'task.updated', project_id: 1 })
    await settle()
    expect(b.events).toHaveLength(seen) // delivers nothing while paused
    b.show()
    await settle()
    expect(types(b.events.slice(seen))).toEqual(['connected']) // exactly one, so its stores resync
    es.emit({ type: 'task.updated', project_id: 1 })
    await settle()
    expect(types(b.events.slice(seen))).toEqual(['connected', 'task.updated'])
    expect(open()).toEqual([es]) // still the leader's one stream
    expect(locks.queued).toBe(1) // b waits its turn again
  })

  it('a returning follower whose leader left opens the stream itself, with one connected', async () => {
    const a = await openTab(), b = await openTab()
    const offA = a.subscribe()
    b.subscribe()
    await settle()
    open()[0].emit({ type: 'connected' })
    b.hide()
    await wait(HIDDEN_CLOSE_MS)
    offA()
    await settle()
    expect(open()).toHaveLength(0)
    const seen = b.events.length
    b.show()
    await settle()
    expect(open()).toHaveLength(1)
    open()[0].emit({ type: 'connected' })
    await settle()
    expect(types(b.events.slice(seen))).toEqual(['connected']) // its own stream's, no extra
  })

  it('a hidden tab never takes over; it opens the stream when shown', async () => {
    const a = await openTab(), b = await openTab()
    const offA = a.subscribe()
    b.subscribe()
    await settle()
    b.hide()
    offA()
    await settle()
    expect(open()).toHaveLength(0)
    b.show()
    await settle()
    expect(open()).toHaveLength(1)
  })

  it('a tab opened in the background waits until it is shown', async () => {
    const a = await openTab({ hidden: true })
    a.subscribe()
    await settle()
    expect(open()).toHaveLength(0)
    expect(locks.held).toBe(false)
    a.show()
    await settle()
    expect(open()).toHaveLength(1)
  })

  it('a new subscriber in a follower tab gets the leader to retry a stream the browser gave up on', async () => {
    const a = await openTab(), b = await openTab()
    a.subscribe()
    b.subscribe()
    await settle()
    const [dead] = FakeEventSource.instances
    dead.readyState = 2 // CLOSED: the server answered with an error status
    b.subscribe()
    await settle()
    expect(FakeEventSource.instances).toHaveLength(2)
    const live = FakeEventSource.instances[1]
    live.emit({ type: 'task.updated', project_id: 1 })
    await settle()
    expect(b.events.at(-1)?.type).toBe('task.updated')
  })

  it('only /api/events is shared: another URL keeps a stream per tab', async () => {
    const a = await openTab(), b = await openTab()
    a.subscribe('/api/other')
    b.subscribe('/api/other')
    await settle()
    expect(open()).toHaveLength(2)
  })

  describe('when the session ends (#153)', () => {
    it('ending in the leader tab ends it in every tab, and no tab opens a stream after', async () => {
      const a = await openTab(), b = await openTab(), c = await openTab()
      const offA = a.subscribe()
      b.subscribe()
      c.subscribe()
      await settle()
      const [es] = open()
      a.session.markSessionEnded() // e.g. a's stream gave up and /api/me said signed out
      await settle()
      expect(es.closed).toBe(true)
      expect(b.session.isSessionEnded()).toBe(true)
      expect(c.session.isSessionEnded()).toBe(true)
      offA() // the next leader opens nothing either
      await settle()
      expect(locks.held).toBe(true)
      expect(FakeEventSource.instances).toHaveLength(1)
    })

    it('a follower that finds it ended tells the leader, which closes the stream', async () => {
      const a = await openTab(), b = await openTab()
      a.subscribe()
      b.subscribe()
      await settle()
      const [es] = open()
      b.session.markSessionEnded() // e.g. an API call came back signed out
      await settle()
      expect(a.session.isSessionEnded()).toBe(true)
      expect(es.closed).toBe(true)
      expect(FakeBroadcastChannel.sent.filter(m => m.kind === 'session-ended')).toHaveLength(1) // a doesn't echo it
    })

    it('a follower paused at the time learns it when shown', async () => {
      const a = await openTab(), b = await openTab()
      a.subscribe()
      b.subscribe()
      await settle()
      b.hide()
      await wait(HIDDEN_CLOSE_MS)
      a.session.markSessionEnded()
      await settle()
      expect(b.session.isSessionEnded()).toBe(false) // its channel was closed
      b.show()
      await settle()
      expect(b.session.isSessionEnded()).toBe(true) // the leader answered its hello
      expect(FakeEventSource.instances).toHaveLength(1)
    })
  })
})

describe('without Web Locks or BroadcastChannel (each tab keeps its own stream)', () => {
  afterEach(() => { delete (navigator as { locks?: unknown }).locks })

  it('without Web Locks, every tab opens its own stream and nothing is broadcast', async () => {
    FakeBroadcastChannel.all = []
    FakeBroadcastChannel.sent = []
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    const a = await openTab(), b = await openTab()
    a.subscribe()
    b.subscribe()
    expect(open()).toHaveLength(2)
    const [esA] = open()
    esA.emit({ type: 'task.updated', project_id: 1 })
    a.session.markSessionEnded()
    await settle()
    expect(a.events).toHaveLength(1)
    expect(b.events).toHaveLength(0)
    expect(FakeBroadcastChannel.all).toHaveLength(0)
    expect(FakeBroadcastChannel.sent).toHaveLength(0)
  })

  it('without BroadcastChannel, every tab opens its own stream and takes no lock', async () => {
    const locks = new FakeLocks()
    stubLocks(locks)
    vi.stubGlobal('BroadcastChannel', undefined)
    const a = await openTab(), b = await openTab()
    a.subscribe()
    b.subscribe()
    await settle()
    expect(open()).toHaveLength(2)
    expect(locks.held).toBe(false)
  })

  it('a tab whose lock request is refused keeps its own stream', async () => {
    stubLocks({ request: () => Promise.reject(new DOMException('The request was denied.', 'SecurityError')) })
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    const a = await openTab()
    a.subscribe()
    await settle()
    expect(open()).toHaveLength(1)
    const [es] = open()
    es.emit({ type: 'task.updated', project_id: 1 })
    expect(a.events).toHaveLength(1)
  })
})
