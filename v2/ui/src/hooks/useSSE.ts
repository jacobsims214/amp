import { useEffect, useLayoutEffect, useRef } from 'react'
import type { SSEEvent } from '../types'
import { checkSession, isSessionEnded, onSessionEnded } from '../lib/session'

const EVENTS_URL = '/api/events'

type Listener = (event: SSEEvent) => void

interface Shared {
  // null while a hidden tab has let the stream go (see HIDDEN_CLOSE_MS).
  source: EventSource | null
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
  // EventSource reconnects by itself after a dropped stream; we let it. When
  // it gives up (CLOSED: the answer was an error status or not a stream),
  // ask whether the session ended, since the stream can't show its status.
  source.onerror = () => {
    if (isSessionEnded()) { source.close(); return }
    if (source.readyState === 2 /* CLOSED */) void checkSession()
  }
  return source
}

// A lost session (#153) closes every stream: reconnecting would only get the
// proxy's sign-in answer again, over and over.
onSessionEnded(() => { for (const shared of registry.values()) shared.source?.close() })

// The browser's six connections per host are shared by every tab, so a tab
// left in the background lets its streams go after HIDDEN_CLOSE_MS, keeping
// the subscribers, and reopens them when it is shown again. The server says
// {"type":"connected"} on every new stream, which tells the page to catch up
// on what it missed. While paused, nothing opens a stream: not a new
// subscriber, not a retry.
export const HIDDEN_CLOSE_MS = 30_000
let hiddenTimer: ReturnType<typeof setTimeout> | undefined
let paused = false

function pauseStreams() {
  paused = true
  for (const shared of registry.values()) {
    const { source } = shared
    if (!source) continue
    // A close we chose says nothing about the session: never ask.
    source.onerror = null
    source.close()
    shared.source = null
  }
}

function resumeStreams() {
  paused = false
  if (isSessionEnded()) return
  // Every entry has a subscriber: the last one to leave deletes it.
  for (const [url, shared] of registry) {
    if (!shared.source) shared.source = open(url, shared)
  }
}

function onVisibilityChange() {
  clearTimeout(hiddenTimer)
  hiddenTimer = undefined
  if (document.visibilityState === 'hidden') {
    hiddenTimer = setTimeout(pauseStreams, HIDDEN_CLOSE_MS)
  } else if (paused) {
    resumeStreams()
  }
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', onVisibilityChange)
  // A tab opened in the background starts hidden.
  if (document.visibilityState === 'hidden') onVisibilityChange()
}

/**
 * Subscribe to the shared event stream at url. Returns the unsubscribe. The
 * stream opens with the first subscriber and closes with the last.
 */
export function subscribeSSE(url: string, listener: Listener): () => void {
  // No EventSource (a test environment without one): nothing to listen to.
  if (typeof EventSource === 'undefined') return () => {}
  let shared = registry.get(url)
  if (!shared) {
    const listeners = new Set<Listener>()
    // A paused (hidden) tab opens it when it is shown again.
    shared = { listeners, source: paused ? null : open(url, { listeners }) }
    registry.set(url, shared)
  } else if (
    !paused
    && (!shared.source || shared.source.readyState === 2 /* CLOSED */)
    && !isSessionEnded()
  ) {
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
      entry.source?.close()
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
