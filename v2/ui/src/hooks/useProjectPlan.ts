import { useCallback, useEffect, useMemo, useRef, useSyncExternalStore } from 'react'
import { boardIsStale, boardSnapshot, loadBoard, retainBoard, subscribeBoard, type BoardSnapshot } from '../lib/boardStore'
import type { Epic, Story, Task } from '../types'

export interface ProjectPlan {
  tasks: Task[]
  epics: Epic[]
  stories: Story[]
  /** false until the first load answered (or failed) */
  loaded: boolean
}

const EMPTY: ProjectPlan = { tasks: [], epics: [], stories: [], loaded: false }
const noSubscribe = () => () => {}

/**
 * A project's tickets, epics and stories for the ticket drawer: the human
 * panel (what a ticket unblocks, where it sits) and the Details tab (story
 * and dependency pickers). It reads the shared board store (lib/boardStore),
 * so a drawer opened on the Board, Mission or Flight plan uses the data
 * already on screen instead of reading the project again. The drawer stays
 * mounted between tickets, so a new `refreshKey` (the open ticket's id)
 * re-reads the project through the store; the previous read stays on screen
 * meanwhile.
 */
export function useProjectPlan(projectId: number | undefined, refreshKey?: unknown): ProjectPlan {
  const subscribe = useCallback((l: () => void) => (projectId == null ? noSubscribe() : subscribeBoard(projectId, l)), [projectId])
  const snap = useSyncExternalStore<BoardSnapshot | null>(subscribe,
    () => (projectId == null ? null : boardSnapshot(projectId)), () => (projectId == null ? null : boardSnapshot(projectId)))

  useEffect(() => (projectId == null ? undefined : retainBoard(projectId)), [projectId])

  // First look at a project: read it only if the store has nothing fresh.
  // Another ticket opened in the same drawer: re-read it.
  const seen = useRef<{ projectId: number; key: unknown } | null>(null)
  useEffect(() => {
    if (projectId == null) return
    const prev = seen.current
    seen.current = { projectId, key: refreshKey }
    if (prev?.projectId === projectId && prev.key !== refreshKey) void loadBoard(projectId, { force: true })
    else if (boardIsStale(projectId)) void loadBoard(projectId)
  }, [projectId, refreshKey])

  return useMemo(() => (snap
    ? { tasks: snap.tasks, epics: snap.epics, stories: snap.stories, loaded: snap.loaded || snap.error != null }
    : EMPTY), [snap])
}
