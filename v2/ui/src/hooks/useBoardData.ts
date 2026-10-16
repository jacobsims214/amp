import { useState, useEffect, useCallback } from 'react'
import { api } from '../api/client'
import { useSSE } from './useSSE'
import type { Epic, Story, Task, SSEEvent } from '../types'

export interface BoardData {
  epics: Epic[]
  stories: Story[]
  tasks: Task[]
  loading: boolean
  error: string | null
  refresh: () => void
  /** Merge an updated entity (e.g. from an assign/owner API response) by id. */
  applyTask: (task: Task) => void
  applyEpic: (epic: Epic) => void
  applyStory: (story: Story) => void
}

function mergeById<T extends { id: number }>(item: T) {
  return (prev: T[]) => prev.map(x => x.id === item.id ? { ...x, ...item } : x)
}

export function useBoardData(projectId: number): BoardData {
  const [epics, setEpics] = useState<Epic[]>([])
  const [stories, setStories] = useState<Story[]>([])
  const [tasks, setTasks] = useState<Task[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const [epicList, taskList] = await Promise.all([
        api.listEpics(projectId),
        api.listTasks(projectId),
      ])
      setEpics(epicList)
      const storyLists = await Promise.all(epicList.map(e => api.listStories(e.id)))
      setStories(storyLists.flat())
      setTasks(taskList)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load board')
    } finally {
      setLoading(false)
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
        setTasks(mergeById(task))
        break
      }
    }
  }, []))

  const applyTask = useCallback((task: Task) => setTasks(mergeById(task)), [])
  const applyEpic = useCallback((epic: Epic) => setEpics(mergeById(epic)), [])
  const applyStory = useCallback((story: Story) => setStories(mergeById(story)), [])

  return { epics, stories, tasks, loading, error, refresh: load, applyTask, applyEpic, applyStory }
}
