import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, renderHook } from '@testing-library/react'
import { sseSubscriberCount, subscribeSSE, useSSE } from './useSSE'
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

const open = () => FakeEventSource.instances.filter(s => !s.closed)

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})
afterEach(() => vi.unstubAllGlobals())

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
})
