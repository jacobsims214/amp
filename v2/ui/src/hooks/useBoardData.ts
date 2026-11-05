import { useCallback, useEffect, useSyncExternalStore } from 'react'
import {
  applyEpic as storeEpic, applyStory as storeStory, applyTask as storeTask,
  boardIsStale, boardSnapshot, loadBoard, retainBoard, subscribeBoard,
} from '../lib/boardStore'
import type { Epic, Story, Task } from '../types'

export interface BoardData {
  epics: Epic[]
  stories: Story[]
  tasks: Task[]
  /** The first load: nothing of this project's board is on screen yet. */
  loading: boolean
  /** A refetch while the loaded board stays on screen (refresh(), after a mutation, a revisit). */
  refreshing: boolean
  error: string | null
  refresh: () => void
  /** Merge an updated entity (e.g. from an assign/owner API response) by id. */
  applyTask: (task: Task) => void
  applyEpic: (epic: Epic) => void
  applyStory: (story: Story) => void
}

/**
 * A project's epics, stories and tickets from the shared store
 * (lib/boardStore.ts): cached data renders on the first frame, a stale or
 * missing project is read in the background, and live events keep it
 * current for every view of the project.
 */
export function useBoardData(projectId: number): BoardData {
  const subscribe = useCallback((l: () => void) => subscribeBoard(projectId, l), [projectId])
  const snap = useSyncExternalStore(subscribe, () => boardSnapshot(projectId), () => boardSnapshot(projectId))

  useEffect(() => {
    const release = retainBoard(projectId)
    if (boardIsStale(projectId)) void loadBoard(projectId)
    return release
  }, [projectId])

  const refresh = useCallback(() => { void loadBoard(projectId, { force: true }) }, [projectId])
  const applyTask = useCallback((t: Task) => storeTask(projectId, t), [projectId])
  const applyEpic = useCallback((e: Epic) => storeEpic(projectId, e), [projectId])
  const applyStory = useCallback((s: Story) => storeStory(projectId, s), [projectId])

  return {
    epics: snap.epics, stories: snap.stories, tasks: snap.tasks,
    // Not loaded and no error means a first load is due or in flight: that covers the
    // render right after a project switch, before the load effect has run.
    loading: !snap.loaded && !snap.error, refreshing: snap.inFlight && snap.loaded,
    error: snap.error, refresh, applyTask, applyEpic, applyStory,
  }
}
