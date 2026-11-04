import { useEffect, useLayoutEffect, useRef } from 'react'
import type { SSEEvent } from '../types'

const EVENTS_URL = '/api/events'

type Listener = (event: SSEEvent) => void

interface Shared {
  source: EventSource
  listeners: Set<Listener>
}

// One EventSource per URL for the whole app, shared by every useSSE caller
// and closed when the last one unsubscribes. A board page alone used to open
// three or four (Board, useBoardData, usePlanApproval, the drawer's
// artifacts), and over HTTP/1.1 the browser allows six connections per host,
// so page loads queued behind the streams.
const registry = new Map<string, Shared>()

function open(url: string, shared: Omit<Shared, 'source'>): EventSource {
  const source = new EventSource(url)
  source.onmessage = (e) => {
    let event: SSEEvent
    try {
      event = JSON.parse(e.data)
    } catch {
      return // ignore malformed events
    }
    // Copy so a listener that unsubscribes mid-dispatch doesn't skip another.
    for (const listener of [...shared.listeners]) {
      try { listener(event) } catch (err) { console.error('useSSE listener failed', err) }
    }
  }
  // EventSource reconnects by itself after a dropped stream; we let it.
  source.onerror = () => {}
  return source
}

/**
 * Subscribe to the shared event stream at url. Returns the unsubscribe. The
 * stream opens with the first subscriber and closes with the last.
 */
export function subscribeSSE(url: string, listener: Listener): () => void {
  let shared = registry.get(url)
  if (!shared) {
    const listeners = new Set<Listener>()
    shared = { listeners, source: open(url, { listeners }) }
    registry.set(url, shared)
  } else if (shared.source.readyState === 2 /* CLOSED */) {
    // The browser gave up (e.g. the server answered with an error status);
    // a new subscriber is a good moment to try again.
    shared.source = open(url, shared)
  }
  shared.listeners.add(listener)
  const entry = shared
  let done = false
  return () => {
    if (done) return
    done = true
    entry.listeners.delete(listener)
    if (entry.listeners.size === 0 && registry.get(url) === entry) {
      entry.source.close()
      registry.delete(url)
    }
  }
}

/** Test hook: how many subscribers share url's stream (0 when closed). */
export function sseSubscriberCount(url = EVENTS_URL): number {
  return registry.get(url)?.listeners.size ?? 0
}

/**
 * Receive AMP's live events. projectId filters to one project's events (plus
 * events that carry no project); null receives everything. Every caller in
 * the app shares one /api/events connection.
 */
export function useSSE(
  projectId: number | null,
  onEvent: (event: SSEEvent) => void,
) {
  const onEventRef = useRef(onEvent)
  // Keep the latest handler without resubscribing when it changes.
  useLayoutEffect(() => { onEventRef.current = onEvent })

  useEffect(() => {
    return subscribeSSE(EVENTS_URL, (event) => {
      // Filter to relevant project if specified
      if (projectId !== null && event.project_id !== undefined && event.project_id !== projectId) {
        return
      }
      onEventRef.current(event)
    })
  }, [projectId])
}
