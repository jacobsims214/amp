/**
 * A project's epics, stories and tickets, shared by every view of the
 * project (KB design/loading.md, "Shared data").
 *
 * Mission, Board, Flight plan, Graph and Report each used to load the whole
 * project on mount and throw it away on unmount, so every tab switch hit a
 * cold page-level loader. Now one module-level store per project id keeps
 * the last answer:
 *
 * - A view that mounts renders the cached data on its first frame, and the
 *   store re-reads it in the background when it's older than FRESH_MS
 *   (stale-while-revalidate: tier 2, the view stays mounted).
 * - Concurrent loads of one project share one request set, and only the
 *   newest load may land (a slow answer can't overwrite a newer one).
 * - Live events (SSE) patch the store, not a component, so a change seen on
 *   the Board is already there when Mission opens. A patch made while a load
 *   is in flight is replayed on top of that load's answer (the server read it
 *   before the change), so a slow load never undoes a newer event. The
 *   server's "re-read" signals (a ticket deleted, tickets restored from Trash,
 *   a handover) remove the ticket or re-read the project. The store listens while
 *   any view of the project is mounted, and for LINGER_MS after the last
 *   leaves, so a tab switch doesn't drop the stream.
 * - Stories come from GET /api/projects/:id/stories in one request, in
 *   parallel with epics and tickets; an older API without it falls back to
 *   one request per epic.
 */
import { api } from '../api/client'
import { subscribeSSE } from '../hooks/useSSE'
import { withTimeout } from './timeout'
import type { Epic, Story, Task, SSEEvent } from '../types'

/** Cached data younger than this is shown without a re-read on mount. */
export const FRESH_MS = 15_000
/** How long the store keeps listening for live events after the last view of a project leaves. */
export const LINGER_MS = 30_000

export interface BoardSnapshot {
  epics: Epic[]
  stories: Story[]
  tasks: Task[]
  /** the data has been read for this project at least once */
  loaded: boolean
  /** a load is running */
  inFlight: boolean
  error: string | null
}

interface Entry {
  snap: BoardSnapshot
  loadedAt: number
  seq: number
  inflight: Promise<void> | null
  listeners: Set<() => void>
  views: number
  unsubscribe: (() => void) | null
  linger: ReturnType<typeof setTimeout> | null
  /** Patches applied while the current load is in flight, replayed on its answer. */
  replay: Patch[]
}

type Patch = (s: BoardSnapshot) => Partial<BoardSnapshot>

const NONE: never[] = []
const EMPTY: BoardSnapshot = { epics: NONE, stories: NONE, tasks: NONE, loaded: false, inFlight: false, error: null }
const entries = new Map<number, Entry>()

function entry(pid: number): Entry {
  let e = entries.get(pid)
  if (!e) {
    e = { snap: EMPTY, loadedAt: 0, seq: 0, inflight: null, listeners: new Set(), views: 0, unsubscribe: null, linger: null, replay: [] }
    entries.set(pid, e)
  }
  return e
}

function set(pid: number, patch: Partial<BoardSnapshot>) {
  const e = entry(pid)
  e.snap = { ...e.snap, ...patch }
  for (const l of [...e.listeners]) l()
}

/** The project's epics, every story and every ticket: three requests, in parallel. */
export async function fetchBoard(pid: number): Promise<{ epics: Epic[]; stories: Story[]; tasks: Task[] }> {
  const [epics, tasks, projectStories] = await Promise.all([
    api.listEpics(pid),
    api.listTasks(pid),
    Promise.resolve().then(() => api.listProjectStories(pid)).catch(() => null),
  ])
  const stories = projectStories ?? (await Promise.all(epics.map(e => api.listStories(e.id)))).flat()
  return { epics, stories, tasks }
}

/** Read project `pid` again. Returns the running load when one is already in flight and `force` isn't set. */
export function loadBoard(pid: number, { force = false } = {}): Promise<void> {
  const e = entry(pid)
  if (e.inflight && !force) return e.inflight
  const mine = ++e.seq
  e.replay = []
  set(pid, { inFlight: true, error: null })
  // Only this load may land, and only in this entry (a reset store doesn't take a late answer).
  const current = () => mine === e.seq && entries.get(pid) === e
  const run = withTimeout(fetchBoard(pid), 'Loading the board')
    .then(data => {
      if (!current()) return
      e.loadedAt = Date.now()
      // Changes that arrived while this load was in flight are newer than its answer.
      let next: BoardSnapshot = { ...e.snap, ...data, loaded: true }
      for (const p of e.replay) next = { ...next, ...p(next) }
      e.replay = []
      set(pid, next)
    })
    .catch(err => {
      if (!current()) return
      e.replay = []
      const status = (err as { status?: number }).status
      // Gone or no longer visible (403/404): drop what's cached, so it isn't shown any more.
      if (status === 403 || status === 404) {
        e.loadedAt = 0
        set(pid, { epics: NONE, stories: NONE, tasks: NONE, loaded: false, error: err instanceof Error ? err.message : 'Failed to load board' })
        return
      }
      // A failed (or timed-out) refetch keeps the data on screen; the view shows the error.
      set(pid, { error: err instanceof Error ? err.message : 'Failed to load board' })
    })
    .finally(() => {
      if (!current()) return
      e.inflight = null
      set(pid, { inFlight: false })
    })
  e.inflight = run
  return run
}

/** The project's snapshot (empty and not loaded before its first read). */
export function boardSnapshot(pid: number): BoardSnapshot {
  return entries.get(pid)?.snap ?? EMPTY
}

/** Whether a view mounting now should re-read the project. */
export function boardIsStale(pid: number, now = Date.now()): boolean {
  const e = entries.get(pid)
  return !e || (!e.inflight && (!e.snap.loaded || now - e.loadedAt > FRESH_MS))
}

/** Subscribe to project `pid`'s snapshot (for useSyncExternalStore). */
export function subscribeBoard(pid: number, listener: () => void): () => void {
  const e = entry(pid)
  e.listeners.add(listener)
  return () => { e.listeners.delete(listener) }
}

// ─── Patches (from API answers and live events) ──────────────────────────────

// The server leaves out empty optional ticket fields (no blockers, no block
// reason, no schedule), so a ticket update must clear them rather than keep
// old values when merged.
function fullTask(task: Task): Task {
  return { ...task, blocked_by_ids: task.blocked_by_ids ?? [], block_reason: task.block_reason ?? '', start_at: task.start_at }
}
const mergeById = <T extends { id: number }>(list: T[], item: T) => list.map(x => (x.id === item.id ? { ...x, ...item } : x))
const addOnce = <T extends { id: number }>(list: T[], item: T) => (list.some(x => x.id === item.id) ? list : [...list, item])

/** Apply `p` now, and again on top of the in-flight load's answer if there is one. */
function patch(pid: number, p: Patch) {
  const e = entry(pid)
  if (e.inflight) e.replay.push(p)
  // Before the first answer there's nothing on screen to patch; the replay covers it.
  if (e.snap.loaded) set(pid, p(e.snap))
}

export function applyTask(pid: number, task: Task) { patch(pid, s => ({ tasks: mergeById(s.tasks, fullTask(task)) })) }
export function applyEpic(pid: number, epic: Epic) { patch(pid, s => ({ epics: mergeById(s.epics, epic) })) }
export function applyStory(pid: number, story: Story) { patch(pid, s => ({ stories: mergeById(s.stories, story) })) }

/**
 * Something changed that the store can't patch (tickets restored from Trash,
 * a handover, a project import): re-read the project now if a view shows it,
 * else on the next view mount. Mutation sites outside the board views call this.
 */
export function invalidateBoard(pid: number) {
  const e = entries.get(pid)
  if (!e) return
  e.loadedAt = 0
  if (e.views > 0) void loadBoard(pid, { force: true })
}

/** A task.updated whose payload isn't a ticket: the server's "this changed, re-read" signals. */
function applySignal(pid: number, payload: Record<string, unknown>) {
  const taskId = typeof payload.task_id === 'number' ? payload.task_id : null
  if (payload.deleted === true && taskId != null) {
    patch(pid, s => ({ tasks: s.tasks.filter(t => t.id !== taskId) }))
    return
  }
  // A run recorded on a ticket: runs aren't board data.
  if (taskId != null && 'run' in payload) return
  // restored, handover, a deleted story or epic ({story_id|epic_id, task_ids, deleted}), anything else: re-read.
  invalidateBoard(pid)
}

/** Apply one live event to project `pid`'s cached data. */
export function applyEvent(pid: number, event: SSEEvent) {
  if (!event.project_id || event.project_id !== pid) return
  const s = boardSnapshot(pid)
  // Nothing cached to patch: the first load reads it all (an event during it
  // is still replayed on its answer).
  if (!s.loaded && !entries.get(pid)?.inflight) return
  const payload = event.payload as Record<string, unknown> | null
  if (event.type === 'task.updated' && (payload == null || typeof payload.id !== 'number')) {
    applySignal(pid, payload ?? {})
    return
  }
  switch (event.type) {
    case 'epic.created': patch(pid, x => ({ epics: addOnce(x.epics, event.payload as Epic) })); break
    // Epic or story state changed (in_progress or completed), or owners/fields edited: update in place.
    case 'epic.state_changed':
    case 'epic.updated': applyEpic(pid, event.payload as Epic); break
    case 'story.created': patch(pid, x => ({ stories: addOnce(x.stories, event.payload as Story) })); break
    case 'story.state_changed':
    case 'story.updated': applyStory(pid, event.payload as Story); break
    case 'task.created': patch(pid, x => ({ tasks: addOnce(x.tasks, event.payload as Task) })); break
    // task.unblocked: a scheduled ticket went from blocked to backlog.
    // task.assigned carries the full ticket with its new owner_user_id.
    case 'task.updated':
    case 'task.dispatched':
    case 'task.completed':
    case 'task.blocked':
    case 'task.unblocked':
    case 'task.assigned': applyTask(pid, event.payload as Task); break
  }
}

/**
 * A view of project `pid` mounted: listen for its live events (once per
 * project, however many views). Returns the matching release; the store
 * keeps listening for LINGER_MS after the last release.
 */
export function retainBoard(pid: number): () => void {
  const e = entry(pid)
  e.views++
  if (e.linger) { clearTimeout(e.linger); e.linger = null }
  if (!e.unsubscribe) e.unsubscribe = subscribeSSE('/api/events', ev => applyEvent(pid, ev))
  let released = false
  return () => {
    if (released) return
    released = true
    e.views--
    if (e.views > 0) return
    e.linger = setTimeout(() => {
      e.linger = null
      if (e.views === 0 && e.unsubscribe) { e.unsubscribe(); e.unsubscribe = null }
    }, LINGER_MS)
  }
}

/** For tests: forget every project and stop listening. */
export function resetBoardStore() {
  for (const e of entries.values()) {
    if (e.linger) clearTimeout(e.linger)
    e.unsubscribe?.()
  }
  entries.clear()
}

// Test setup resets every shared store after each test (src/test/setup.ts).
;((globalThis as { __ampStoreResets?: Set<() => void> }).__ampStoreResets ??= new Set()).add(resetBoardStore)
