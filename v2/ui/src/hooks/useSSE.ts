import { useCallback, useEffect, useLayoutEffect, useRef, useSyncExternalStore } from 'react'
import type { SSEEvent } from '../types'
import { checkSession, isSessionEnded, markSessionEnded, onSessionEnded } from '../lib/session'

const EVENTS_URL = '/api/events'

type Listener = (event: SSEEvent) => void

interface Shared {
  listeners: Set<Listener>
  // This tab's own stream. null while another tab leads (see LEADER_LOCK),
  // or while a hidden tab has let the stream go (see HIDDEN_CLOSE_MS).
  source: EventSource | null
  // The stream this tab hears said {"type":"connected"} and hasn't failed
  // since: its own source, or in a follower tab the leader's. What
  // useSSEConnected reports. Change it only through setUp.
  up: boolean
  // How this tab shares the stream with the other tabs; null when it doesn't.
  tabs: Tabs | null
}

// One EventSource per URL for the whole app, shared by every useSSE caller
// and closed when the last one unsubscribes. A board page alone used to open
// three or four (Board, useBoardData, usePlanApproval, the drawer's
// artifacts), and over HTTP/1.1 the browser allows six connections per host,
// so page loads queued behind the streams.
const registry = new Map<string, Shared>()

// Those six connections are shared by every tab, and every AMP tab gets the
// same per-user /api/events stream. So one visible tab per browser, the one
// holding the LEADER_LOCK Web Lock, opens it and relays every frame on the
// TAB_CHANNEL BroadcastChannel, and the other tabs deliver what it relays.
// A tab queues for the lock only while it is visible and has subscribers.
// Without Web Locks or BroadcastChannel, each tab opens its own stream.
// Tabs running different builds talk on this channel: change the messages
// compatibly, or rename the lock and channel together.
const LEADER_LOCK = 'amp-sse-leader'
const TAB_CHANNEL = 'amp-sse'

type TabMessage =
  | { kind: 'event'; event: SSEEvent } // a frame from the leader's stream
  | { kind: 'hello' } // a tab started listening: is the stream up?
  | { kind: 'up' } // the leader's answer: its stream said connected
  | { kind: 'down' } // the leader's stream dropped or closed, or a new leader is opening one
  | { kind: 'retry' } // a new subscriber: retry a stream the browser gave up on
  | { kind: 'session-ended' }

interface Tabs {
  // null while the tab is paused (hidden for HIDDEN_CLOSE_MS).
  channel: BroadcastChannel | null
  leading: boolean
  // This tab's place in the lock queue, if it has one.
  queued: AbortController | null
  // Lets go of the lock (it is held until this promise settles).
  release: (() => void) | null
  // Listening, and not yet told the stream is up: the next "up" or relayed
  // connected is this tab's connected, as if it had opened the stream itself.
  joining: boolean
}

function canShareTabs(): boolean {
  return typeof BroadcastChannel !== 'undefined'
    && typeof navigator !== 'undefined'
    && typeof navigator.locks?.request === 'function'
}

// The tab's document, read once (tests give each simulated tab its own).
const doc = typeof document === 'undefined' ? undefined : document
const hidden = () => doc?.visibilityState === 'hidden'

// Told whenever an entry's up changes, so useSSEConnected re-reads it.
const watchers = new Set<() => void>()

function setUp(shared: Shared, up: boolean) {
  if (shared.up === up) return
  shared.up = up
  for (const watcher of [...watchers]) watcher()
}

// The stream this tab hears is down. A leader tells the other tabs, whose
// streams are its stream, so their Live indicators go back to Connecting.
function lost(shared: Shared) {
  const { tabs } = shared
  if (tabs?.leading && shared.up) post(tabs, { kind: 'down' })
  setUp(shared, false)
}

function deliver(shared: Shared, event: SSEEvent) {
  // Copy so a listener that unsubscribes mid-dispatch doesn't skip another.
  for (const listener of [...shared.listeners]) {
    try { listener(event) } catch (err) { console.error('useSSE listener failed', err) }
  }
}

function post(tabs: Tabs, message: TabMessage) {
  try { tabs.channel?.postMessage(message) } catch { /* closed meanwhile */ }
}

function open(url: string, shared: Shared): EventSource {
  const source = new EventSource(url)
  setUp(shared, false)
  source.onmessage = (e) => {
    let event: SSEEvent
    try {
      event = JSON.parse(e.data)
    } catch {
      return // ignore malformed events
    }
    if (event.type === 'connected') setUp(shared, true)
    // The leader relays every frame, connected included, to the other tabs.
    if (shared.tabs?.leading) post(shared.tabs, { kind: 'event', event })
    deliver(shared, event)
  }
  // EventSource reconnects by itself after a dropped stream; we let it. When
  // it gives up (CLOSED: the answer was an error status or not a stream),
  // ask whether the session ended, since the stream can't show its status.
  source.onerror = () => {
    lost(shared)
    if (isSessionEnded()) { source.close(); return }
    if (source.readyState === 2 /* CLOSED */) void checkSession()
  }
  return source
}

function closeSource(shared: Shared) {
  const { source } = shared
  if (source) {
    // A close we chose says nothing about the session: never ask.
    source.onerror = null
    source.close()
  }
  shared.source = null
  setUp(shared, false)
}

// The browser gave up on the stream (e.g. the server answered with an error
// status); a new subscriber is a good moment to try again.
function reopenIfDead(url: string, shared: Shared) {
  if (!shared.source || shared.source.readyState === 2 /* CLOSED */) shared.source = open(url, shared)
}

// Start hearing the other tabs: open the channel, ask whether a stream is
// already up, and queue for the lock if the tab is visible.
function listen(shared: Shared, tabs: Tabs) {
  if (!tabs.channel) {
    const channel = new BroadcastChannel(TAB_CHANNEL)
    channel.onmessage = (e: MessageEvent) => onTabMessage(shared, tabs, e.data)
    tabs.channel = channel
  }
  tabs.joining = true
  post(tabs, { kind: 'hello' })
  if (!hidden()) queue(shared, tabs)
}

function queue(shared: Shared, tabs: Tabs) {
  if (tabs.leading || tabs.queued) return
  const queued = new AbortController()
  tabs.queued = queued
  navigator.locks.request(LEADER_LOCK, { signal: queued.signal }, () => {
    // Withdrawn after the lock was granted: let it go at once.
    if (tabs.queued !== queued) return
    tabs.queued = null
    return lead(shared, tabs)
  }).catch(() => {
    // An AbortError means we withdrew. Anything else (a browser that refuses
    // locks here) means this tab can't take part: it keeps its own stream.
    if (tabs.queued === queued) goSolo(shared)
  })
}

function withdraw(tabs: Tabs) {
  const { queued } = tabs
  tabs.queued = null
  queued?.abort()
}

function lead(shared: Shared, tabs: Tabs): Promise<void> {
  tabs.leading = true
  tabs.joining = false
  // Until this tab's own stream says connected there is none: the last
  // leader may have closed without a word (the browser closed its tab).
  setUp(shared, false)
  post(tabs, { kind: 'down' })
  // Once the session ended, the leader opens nothing but keeps the lock, so
  // no other tab takes over and opens a stream either.
  if (!paused && !isSessionEnded()) shared.source = open(EVENTS_URL, shared)
  // Held until stepDown. The browser lets go by itself when the tab closes.
  return new Promise(resolve => { tabs.release = resolve })
}

// Leave the lock queue; a leader also closes its stream and lets go of the
// lock, so a waiting visible tab takes over.
function stepDown(shared: Shared, tabs: Tabs) {
  withdraw(tabs)
  if (!tabs.leading) return
  lost(shared)
  tabs.leading = false
  closeSource(shared)
  tabs.release?.()
  tabs.release = null
}

// Stop taking part (a paused tab, or the last subscriber left): no stream,
// no place in the queue, no channel.
function leave(shared: Shared, tabs: Tabs) {
  stepDown(shared, tabs)
  tabs.channel?.close()
  tabs.channel = null
  tabs.joining = false
  setUp(shared, false) // a follower no longer hears the leader's stream
}

function goSolo(shared: Shared) {
  if (!shared.tabs) return
  leave(shared, shared.tabs)
  shared.tabs = null
  if (!paused && !isSessionEnded()) reopenIfDead(EVENTS_URL, shared)
}

function onTabMessage(shared: Shared, tabs: Tabs, message: TabMessage) {
  if (!message || typeof message !== 'object') return
  switch (message.kind) {
    case 'event':
      if (message.event.type === 'connected' && !tabs.leading) {
        tabs.joining = false
        setUp(shared, true)
      }
      deliver(shared, message.event)
      return
    case 'hello':
    case 'retry':
      if (!tabs.leading) return
      if (isSessionEnded()) post(tabs, { kind: 'session-ended' })
      else if (shared.up && message.kind === 'hello') post(tabs, { kind: 'up' })
      else reopenIfDead(EVENTS_URL, shared)
      return
    case 'up':
      // Every listening tab hears the answer; only a joining one needs it.
      if (tabs.leading || !tabs.joining) return
      tabs.joining = false
      setUp(shared, true)
      deliver(shared, { type: 'connected' })
      return
    case 'down':
      if (!tabs.leading) setUp(shared, false)
      return
    case 'session-ended':
      endedElsewhere = true
      try { markSessionEnded() } finally { endedElsewhere = false }
      return
  }
}

// A lost session (#153) closes every stream: reconnecting would only get the
// proxy's sign-in answer again, over and over. The other tabs are told too,
// since a follower has no stream of its own to notice it with.
let endedElsewhere = false
onSessionEnded(() => {
  for (const shared of registry.values()) {
    shared.source?.close()
    setUp(shared, false)
  }
  if (!endedElsewhere) tellTabs({ kind: 'session-ended' })
})

function tellTabs(message: TabMessage) {
  if (!canShareTabs()) return
  const tabs = registry.get(EVENTS_URL)?.tabs
  if (tabs?.channel) { post(tabs, message); return }
  try {
    const channel = new BroadcastChannel(TAB_CHANNEL)
    channel.postMessage(message)
    channel.close()
  } catch { /* nobody to tell */ }
}

// The browser's six connections per host are shared by every tab, so a tab
// left in the background lets its streams go after HIDDEN_CLOSE_MS, keeping
// the subscribers, and reopens them when it is shown again. The server says
// {"type":"connected"} on every new stream, which tells the page to catch up
// on what it missed. While paused, nothing opens a stream: not a new
// subscriber, not a retry. A leader hands the stream to a visible tab then;
// a follower stops listening, and when shown it delivers one connected
// (the leader's "up", or its own new stream's) so the page catches up.
export const HIDDEN_CLOSE_MS = 30_000
let hiddenTimer: ReturnType<typeof setTimeout> | undefined
let paused = false

function pauseStreams() {
  paused = true
  for (const shared of registry.values()) {
    if (shared.tabs) leave(shared, shared.tabs)
    else closeSource(shared)
  }
}

function resumeStreams() {
  paused = false
  if (isSessionEnded()) return
  // Every entry has a subscriber: the last one to leave deletes it.
  for (const [url, shared] of registry) {
    if (shared.tabs) listen(shared, shared.tabs)
    else if (!shared.source) shared.source = open(url, shared)
  }
}

function onVisibilityChange() {
  clearTimeout(hiddenTimer)
  hiddenTimer = undefined
  if (hidden()) {
    hiddenTimer = setTimeout(pauseStreams, HIDDEN_CLOSE_MS)
    // Only visible tabs queue to lead. A leader keeps its stream through the
    // grace period, so a quick tab switch hands nothing over.
    for (const shared of registry.values()) if (shared.tabs) withdraw(shared.tabs)
  } else if (paused) {
    resumeStreams()
  } else {
    for (const shared of registry.values()) {
      if (shared.tabs?.channel) queue(shared, shared.tabs)
    }
  }
}

if (doc) {
  doc.addEventListener('visibilitychange', onVisibilityChange)
  // A tab opened in the background starts hidden.
  if (hidden()) onVisibilityChange()
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
    shared = { listeners: new Set(), source: null, up: false, tabs: null }
    if (url === EVENTS_URL && canShareTabs()) {
      shared.tabs = { channel: null, leading: false, queued: null, release: null, joining: false }
    }
    registry.set(url, shared)
    // A paused (hidden) tab starts when it is shown again.
    if (!paused) {
      if (shared.tabs) listen(shared, shared.tabs)
      else shared.source = open(url, shared)
    }
  } else if (!paused && !isSessionEnded()) {
    const { tabs } = shared
    if (tabs && !tabs.leading) post(tabs, { kind: 'retry' })
    else reopenIfDead(url, shared)
  }
  shared.listeners.add(listener)
  const entry = shared
  let done = false
  return () => {
    if (done) return
    done = true
    entry.listeners.delete(listener)
    if (entry.listeners.size === 0 && registry.get(url) === entry) {
      registry.delete(url)
      if (entry.tabs) leave(entry, entry.tabs)
      else { entry.source?.close(); setUp(entry, false) }
    }
  }
}

/** Test hook: how many subscribers share url's stream (0 when closed). */
export function sseSubscriberCount(url = EVENTS_URL): number {
  return registry.get(url)?.listeners.size ?? 0
}

/**
 * Whether the stream at url is connected in this tab: it has said
 * {"type":"connected"} (in a follower tab, the leader's stream has) and
 * hasn't dropped since. False while it opens or reconnects, while a hidden
 * tab has let it go, once the session ended, and when nothing subscribes.
 */
export function isSSEConnected(url = EVENTS_URL): boolean {
  return registry.get(url)?.up ?? false
}

/**
 * isSSEConnected as React state, for a Live indicator. It holds the stream
 * open like useSSE does. A view that mounts on a stream another view already
 * holds gets no connected frame of its own, so it reads the state rather than
 * waiting for one: it shows Live at once.
 */
export function useSSEConnected(url = EVENTS_URL): boolean {
  const subscribe = useCallback((onChange: () => void) => {
    watchers.add(onChange)
    // A listener of its own: the registry's set would merge a shared one.
    const unsubscribe = subscribeSSE(url, () => {})
    return () => {
      watchers.delete(onChange)
      unsubscribe()
    }
  }, [url])
  return useSyncExternalStore(subscribe, () => isSSEConnected(url), () => false)
}

/**
 * Receive AMP's live events. projectId filters to one project's events (plus
 * events that carry no project); null receives everything. Every caller in
 * every tab of the browser shares one /api/events connection.
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
