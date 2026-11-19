import { useCallback, useEffect, useLayoutEffect, useRef, useSyncExternalStore } from 'react'
import type { SSEEvent } from '../types'
import { checkSession, isSessionEnded, onSessionEnded, onSignedInUserChange, signedInUser } from '../lib/session'

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
  // Set while the entry has no subscribers and waits SSE_CLOSE_DELAY_MS to
  // close; a new subscriber cancels it.
  closing: ReturnType<typeof setTimeout> | undefined
  // Set while a stream the browser gave up on waits to be reopened (see
  // scheduleReopen), and how long the next such wait is.
  reopening: ReturnType<typeof setTimeout> | undefined
  backoff: number
}

// One EventSource per URL for the whole app, shared by every useSSE caller
// and closed soon after the last one unsubscribes (SSE_CLOSE_DELAY_MS). A
// board page alone used to open three or four (Board, useBoardData,
// usePlanApproval, the drawer's artifacts), and over HTTP/1.1 the browser
// allows six connections per host, so page loads queued behind the streams.
const registry = new Map<string, Shared>()

// Those six connections are shared by every tab, and every AMP tab of one
// user gets the same /api/events stream. So one visible tab per user and
// browser, the one holding that user's LEADER_LOCK Web Lock, opens it and
// relays every frame on that user's TAB_CHANNEL BroadcastChannel, and the
// user's other tabs deliver what it relays. A tab queues for the lock only
// while it is visible and has subscribers. Without Web Locks or
// BroadcastChannel, each tab opens its own stream.
//
// A stream carries the events of whoever was signed in when it opened, so
// both names end in the user id /api/me gave this tab (#2678): a tab signed
// in as someone else is on another channel and never hears the frames. Until
// the tab knows its user it joins nothing (see listen). The names live only
// in memory, in this browser.
//
// Tabs running different builds talk on this channel: change the messages
// compatibly, or rename the lock and channel together.
const LEADER_LOCK = 'amp-sse-leader'
const TAB_CHANNEL = 'amp-sse'
const lockName = (user: string) => `${LEADER_LOCK}:${user}`
const channelName = (user: string) => `${TAB_CHANNEL}:${user}`

type TabMessage =
  | { kind: 'event'; event: SSEEvent } // a frame from the leader's stream
  | { kind: 'hello' } // a tab started listening: is the stream up?
  | { kind: 'up' } // the leader's answer: its stream said connected
  | { kind: 'down' } // the leader's stream dropped or closed, or a new leader is opening one
  | { kind: 'retry' } // a new subscriber: retry a stream the browser gave up on
  | { kind: 'session-ended' }

interface Tabs {
  // The user whose lock and channel this tab is in (signedInUser when it
  // joined). null while it isn't in any.
  user: string | null
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
  // The stream works again: the next one the browser gives up on is retried soon.
  if (up) shared.backoff = SSE_REOPEN_FIRST_MS
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
  cancelReopen(shared)
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
  // If it didn't, the outage was elsewhere (a 502 while no server was up), so
  // try the stream again after a while (scheduleReopen).
  source.onerror = () => {
    lost(shared)
    if (isSessionEnded()) { source.close(); return }
    if (source.readyState === 2 /* CLOSED */) {
      void checkSession().then(alive => { if (alive) scheduleReopen(url, shared, source) })
    }
  }
  return source
}

// A stream the browser gave up on is retried after SSE_REOPEN_FIRST_MS,
// doubling up to SSE_REOPEN_MAX_MS, each wait jittered by ±20% so tabs and
// browsers don't all come back at once after an outage (as
// remote/useRemoteSession does). The wait starts over once a stream says
// connected (setUp).
export const SSE_REOPEN_FIRST_MS = 1_000
export const SSE_REOPEN_MAX_MS = 30_000

// Whether this tab should reopen the dead stream now: the entry is still the
// live one with subscribers, nothing has replaced the stream, the tab can be
// seen and isn't paused, the session is alive, and this tab owns the stream
// (it leads, or keeps a stream of its own). A follower never opens one; it
// hears the leader's new stream, connected included.
function mayReopen(url: string, shared: Shared, dead: EventSource): boolean {
  return registry.get(url) === shared
    && shared.listeners.size > 0
    && shared.source === dead
    && dead.readyState === 2 /* CLOSED */
    && !hidden() && !paused && !isSessionEnded()
    && (!shared.tabs || shared.tabs.leading)
}

function scheduleReopen(url: string, shared: Shared, dead: EventSource) {
  if (shared.reopening !== undefined || !mayReopen(url, shared, dead)) return
  const delay = shared.backoff * (0.8 + Math.random() * 0.4)
  shared.backoff = Math.min(shared.backoff * 2, SSE_REOPEN_MAX_MS)
  shared.reopening = setTimeout(() => {
    shared.reopening = undefined
    if (mayReopen(url, shared, dead)) shared.source = open(url, shared)
  }, delay)
}

function cancelReopen(shared: Shared) {
  clearTimeout(shared.reopening)
  shared.reopening = undefined
}

function closeSource(shared: Shared) {
  cancelReopen(shared)
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

// Start hearing the user's other tabs: open the channel, ask whether a
// stream is already up, and queue for the lock if the tab is visible.
function listen(shared: Shared, tabs: Tabs) {
  const user = signedInUser()
  if (user === null) {
    // Who is signed in isn't known yet (the first subscribe runs before
    // AuthProvider's /api/me answers). Join nothing until it is: ask
    // /api/me, and the answer (onSignedInUserChange) calls listen again.
    learnUser()
    return
  }
  if (!tabs.channel) {
    const channel = new BroadcastChannel(channelName(user))
    channel.onmessage = (e: MessageEvent) => onTabMessage(shared, tabs, e.data)
    tabs.channel = channel
    tabs.user = user
  }
  // A tab whose session ended keeps only the channel, to answer hellos.
  if (isSessionEnded()) return
  tabs.joining = true
  post(tabs, { kind: 'hello' })
  if (!hidden()) queue(shared, tabs)
}

function queue(shared: Shared, tabs: Tabs) {
  // A tab whose session ended never leads again, and one checking whether
  // its session ended (checkingElsewhere) waits for the answer.
  if (tabs.leading || tabs.queued || isSessionEnded() || checkingElsewhere) return
  const { user } = tabs
  if (user === null) return
  const queued = new AbortController()
  tabs.queued = queued
  navigator.locks.request(lockName(user), { signal: queued.signal }, () => {
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
  if (!paused && !isSessionEnded()) shared.source = open(EVENTS_URL, shared)
  // Held until stepDown. The browser lets go by itself when the tab closes.
  return new Promise(resolve => { tabs.release = resolve })
}

// Leave the lock queue; a leader also closes its stream and lets go of the
// lock, so a waiting visible tab takes over.
function stepDown(shared: Shared, tabs: Tabs) {
  cancelReopen(shared)
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
  tabs.user = null
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
  if (isSessionEnded()) {
    // This tab hears nothing more. It tells a tab that starts listening that
    // its own session ended, and that tab asks /api/me about its own.
    if (message.kind === 'hello') post(tabs, { kind: 'session-ended' })
    return
  }
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
      if (shared.up && message.kind === 'hello') post(tabs, { kind: 'up' })
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
      checkElsewhere()
      return
  }
}

// Ask /api/me who is signed in, once at a time. checkSession records the
// answer, and onSignedInUserChange below starts listening. If it can't tell
// (offline, an older API without user_id), this tab can't know whose tabs to
// share with, so it keeps a stream of its own, as without Web Locks.
let learningUser = false
function learnUser() {
  if (learningUser) return
  learningUser = true
  void checkSession().finally(() => {
    learningUser = false
    if (signedInUser() !== null || isSessionEnded()) return
    const shared = registry.get(EVENTS_URL)
    if (shared?.tabs && !shared.tabs.channel && !paused && shared.closing === undefined) goSolo(shared)
  })
}

// /api/me named a user: the first answer, or a different user than before
// (someone signed in as another user in this browser, and this tab asked
// again). A tab in the old user's election leaves it, closing the stream it
// led, and joins the new user's. One waiting for its first answer joins now.
onSignedInUserChange(() => {
  const shared = registry.get(EVENTS_URL)
  const tabs = shared?.tabs
  if (!shared || !tabs || tabs.user === signedInUser()) return
  if (tabs.channel) leave(shared, tabs)
  if (paused || isSessionEnded()) return
  // Nobody subscribes any more: the entry was only waiting to close.
  if (shared.closing !== undefined) { closeEntry(EVENTS_URL, shared); return }
  listen(shared, tabs)
})

// Another tab's session ended. That is news about its cookie when it looked,
// and this tab may have signed in since (#2630), so ask /api/me rather than
// believe it. Until the answer, this tab leaves the lock queue, so it doesn't
// take over from an ended leader and open a stream the answer would close. A
// session that ends meanwhile is not broadcast again: the tab that told us
// told every listening tab, and each asks for itself.
let checkingElsewhere = false
function checkElsewhere() {
  if (checkingElsewhere) return
  checkingElsewhere = true
  for (const shared of registry.values()) if (shared.tabs) withdraw(shared.tabs)
  void checkSession().finally(() => {
    checkingElsewhere = false
    // Still signed in: back in the queue (queue skips an ended session).
    if (paused || hidden()) return
    for (const shared of registry.values()) {
      if (shared.tabs?.channel) queue(shared, shared.tabs)
    }
  })
}

// A lost session (#153) closes every stream: reconnecting would only get the
// proxy's sign-in answer again, over and over. The other tabs are told too,
// since a follower has no stream of its own to notice it with. The leader
// steps down and lets go of the lock, so a tab that has signed in since can
// lead (#2630). It keeps its channel to answer hellos (see onTabMessage).
onSessionEnded(() => {
  // Tell them before letting go of the lock, so a follower leaves the queue
  // to ask /api/me before it could be granted the lock.
  if (!checkingElsewhere) tellTabs({ kind: 'session-ended' })
  for (const shared of registry.values()) {
    cancelReopen(shared)
    if (shared.tabs) {
      stepDown(shared, shared.tabs)
      shared.tabs.joining = false
    } else shared.source?.close()
    setUp(shared, false)
  }
})

function tellTabs(message: TabMessage) {
  if (!canShareTabs()) return
  const tabs = registry.get(EVENTS_URL)?.tabs
  if (tabs?.channel) { post(tabs, message); return }
  // Only this user's tabs share a stream with this one.
  const user = signedInUser()
  if (user === null) return
  try {
    const channel = new BroadcastChannel(channelName(user))
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
  for (const [url, shared] of registry) {
    // Nobody is waiting for it: a closing entry goes now, not on show.
    if (shared.closing !== undefined) closeEntry(url, shared)
    else if (shared.tabs) leave(shared, shared.tabs)
    else closeSource(shared)
  }
}

function resumeStreams() {
  paused = false
  if (isSessionEnded()) return
  // Every entry has a subscriber: the last one to leave deletes it (at once
  // while paused, and pauseStreams closed the entries that were closing).
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
    // grace period, so a quick tab switch hands nothing over. A hidden tab
    // doesn't retry a dead stream; it tries again when shown.
    for (const shared of registry.values()) {
      cancelReopen(shared)
      if (shared.tabs) withdraw(shared.tabs)
    }
  } else if (paused) {
    resumeStreams()
  } else {
    for (const [url, shared] of registry) {
      if (shared.tabs?.channel) queue(shared, shared.tabs)
      if (shared.source) scheduleReopen(url, shared, shared.source)
    }
  }
}

if (doc) {
  doc.addEventListener('visibilitychange', onVisibilityChange)
  // A tab opened in the background starts hidden.
  if (hidden()) onVisibilityChange()
}

// When the last subscriber leaves, the stream stays open this long before it
// closes (#2624). A route change (Projects to a project page, ArtifactView's
// project switch) unsubscribes one view and subscribes the next in the same
// commit. Closing at once made the leader close and reopen /api/events, and
// every tab heard a new connected and re-read its data. In the delay the
// entry stays exactly as it was (stream, lock, up), so the next subscriber
// joins it and nothing is sent to anyone. Not used while paused or once the
// session ended: there is no stream to keep then, so the entry goes at once.
export const SSE_CLOSE_DELAY_MS = 1_000

// The last subscriber's leaving, carried out: close the stream (and leave the
// cross-tab election) and drop the entry.
function closeEntry(url: string, shared: Shared) {
  clearTimeout(shared.closing)
  shared.closing = undefined
  cancelReopen(shared)
  if (registry.get(url) !== shared) return
  registry.delete(url)
  if (shared.tabs) leave(shared, shared.tabs)
  else { shared.source?.close(); setUp(shared, false) }
}

// Close every entry that is waiting to close, now.
function closeWaiting() {
  for (const [url, shared] of registry) if (shared.closing !== undefined) closeEntry(url, shared)
}

// A page going away (unload, or into the back-forward cache) has no next
// subscriber to wait for.
doc?.defaultView?.addEventListener('pagehide', closeWaiting)

// Test setup runs this after each test, after the stores' resets let go of
// their streams (src/test/setup.ts), so a stream left waiting by one test
// isn't joined by the next.
;((globalThis as { __ampStreamResets?: Set<() => void> }).__ampStreamResets ??= new Set()).add(closeWaiting)

/**
 * Subscribe to the shared event stream at url. Returns the unsubscribe. The
 * stream opens with the first subscriber and closes SSE_CLOSE_DELAY_MS after
 * the last one leaves, unless another subscribes first.
 */
export function subscribeSSE(url: string, listener: Listener): () => void {
  // No EventSource (a test environment without one): nothing to listen to.
  if (typeof EventSource === 'undefined') return () => {}
  let shared = registry.get(url)
  if (shared?.closing !== undefined) {
    // It was waiting to close: keep it, as if the last one had never left.
    clearTimeout(shared.closing)
    shared.closing = undefined
  }
  if (!shared) {
    shared = {
      listeners: new Set(), source: null, up: false, tabs: null, closing: undefined,
      reopening: undefined, backoff: SSE_REOPEN_FIRST_MS,
    }
    if (url === EVENTS_URL && canShareTabs()) {
      shared.tabs = { user: null, channel: null, leading: false, queued: null, release: null, joining: false }
    }
    registry.set(url, shared)
    // A paused (hidden) tab starts when it is shown again.
    if (!paused) {
      if (shared.tabs) listen(shared, shared.tabs)
      else shared.source = open(url, shared)
    }
  } else if (!paused && !isSessionEnded()) {
    const { tabs } = shared
    // Still out of the election (it doesn't know its user yet): try again.
    if (tabs && !tabs.channel) listen(shared, tabs)
    else if (tabs && !tabs.leading) post(tabs, { kind: 'retry' })
    else reopenIfDead(url, shared)
  }
  shared.listeners.add(listener)
  const entry = shared
  let done = false
  return () => {
    if (done) return
    done = true
    entry.listeners.delete(listener)
    if (entry.listeners.size === 0 && registry.get(url) === entry && entry.closing === undefined) {
      if (paused || isSessionEnded()) closeEntry(url, entry)
      else entry.closing = setTimeout(() => closeEntry(url, entry), SSE_CLOSE_DELAY_MS)
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
