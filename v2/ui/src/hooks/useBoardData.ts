import { useState, useEffect, useCallback, useRef } from 'react'
import { api } from '../api/client'
import { useSSE } from './useSSE'
import type { Epic, Story, Task, SSEEvent } from '../types'

export interface BoardData {
  epics: Epic[]
  stories: Story[]
  tasks: Task[]
  /** The first load: nothing of this project's board is on screen yet. */
  loading: boolean
  /** A refetch while the loaded board stays on screen (refresh(), after a mutation). */
  refreshing: boolean
  error: string | null
  refresh: () => void
  /** Merge an updated entity (e.g. from an assign/owner API response) by id. */
  applyTask: (task: Task) => void
  applyEpic: (epic: Epic) => void
  applyStory: (story: Story) => void
}

// The server leaves out empty optional ticket fields (no blockers, no block
// reason, no schedule), so a ticket update must clear them rather than keep old values
// when merged.
function fullTask(task: Task): Task {
  return { ...task, blocked_by_ids: task.blocked_by_ids ?? [], block_reason: task.block_reason ?? '', start_at: task.start_at }
}

const NONE: never[] = []

function mergeById<T extends { id: number }>(item: T) {
  return (prev: T[]) => prev.map(x => x.id === item.id ? { ...x, ...item } : x)
}

export function useBoardData(projectId: number): BoardData {
  const [epics, setEpics] = useState<Epic[]>([])
  const [stories, setStories] = useState<Story[]>([])
  const [tasks, setTasks] = useState<Task[]>([])
  const [inFlight, setInFlight] = useState(true)
  // The project the arrays hold, so a project switch shows a first load, not the old board.
  const [loadedFor, setLoadedFor] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  // Only the latest load may land: a slow earlier refetch must not overwrite a newer one.
  const seq = useRef(0)

  const load = useCallback(async () => {
    const mine = ++seq.current
    setInFlight(true)
    setError(null)
    try {
      const [epicList, taskList] = await Promise.all([
        api.listEpics(projectId),
        api.listTasks(projectId),
      ])
      const storyLists = await Promise.all(epicList.map(e => api.listStories(e.id)))
      if (mine !== seq.current) return
      setEpics(epicList)
      setStories(storyLists.flat())
      setTasks(taskList)
      setLoadedFor(projectId)
    } catch (e) {
      // A failed refetch keeps the board that is on screen; the caller shows the error.
      if (mine === seq.current) setError(e instanceof Error ? e.message : 'Failed to load board')
    } finally {
      if (mine === seq.current) setInFlight(false)
    }
  }, [projectId])

  useEffect(() => { load() }, [load])

  useSSE(projectId, useCallback((event: SSEEvent) => {
    if (!event.project_id) return

    switch (event.type) {
      case 'epic.created': {
        const epic = event.payload as Epic
        setEpics(prev => prev.find(e => e.id === epic.id) ? prev : [...prev, epic])
        break
      }
      case 'epic.state_changed':
      case 'epic.updated': {
        // Epic state changed (in_progress or completed) or owners/fields
        // edited — update in place.
        const epic = event.payload as Epic
        setEpics(mergeById(epic))
        break
      }
      case 'story.created': {
        const story = event.payload as Story
        setStories(prev => prev.find(s => s.id === story.id) ? prev : [...prev, story])
        break
      }
      case 'story.state_changed':
      case 'story.updated': {
        // Story state changed or owners/fields edited — update in place.
        const story = event.payload as Story
        setStories(mergeById(story))
        break
      }
      case 'task.created': {
        const task = event.payload as Task
        setTasks(prev => prev.find(t => t.id === task.id) ? prev : [...prev, task])
        break
      }
      case 'task.updated':
      case 'task.dispatched':
      case 'task.completed':
      case 'task.blocked':
      case 'task.unblocked':
      case 'task.assigned': {
        // task.unblocked is the new dedicated event type (was task.updated before).
        // When a scheduled task is unblocked, it transitions from blocked to backlog state.
        // task.assigned carries the full task with its new owner_user_id.
        const task = event.payload as Task
        setTasks(mergeById(fullTask(task)))
        break
      }
    }
  }, []))

  const applyTask = useCallback((task: Task) => setTasks(mergeById(fullTask(task))), [])
  const applyEpic = useCallback((epic: Epic) => setEpics(mergeById(epic)), [])
  const applyStory = useCallback((story: Story) => setStories(mergeById(story)), [])

  const loaded = loadedFor === projectId
  return {
    epics: loaded ? epics : NONE, stories: loaded ? stories : NONE, tasks: loaded ? tasks : NONE,
    // Not loaded and no error means a first load is due or in flight: that covers the
    // render right after a project switch, before the load effect has run.
    loading: !loaded && !error, refreshing: inFlight && loaded,
    error, refresh: load, applyTask, applyEpic, applyStory,
  }
}
