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
      case 'epic.state_changed': {
        // Epic state changed (in_progress or completed) — update in place.
        const epic = event.payload as Epic
        setEpics(prev => prev.map(e => e.id === epic.id ? { ...e, ...epic } : e))
        break
      }
      case 'story.created': {
        const story = event.payload as Story
        setStories(prev => prev.find(s => s.id === story.id) ? prev : [...prev, story])
        break
      }
      case 'story.state_changed': {
        // Story state changed — update in place.
        const story = event.payload as Story
        setStories(prev => prev.map(s => s.id === story.id ? { ...s, ...story } : s))
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
      case 'task.unblocked': {
        // task.unblocked is the new dedicated event type (was task.updated before).
        const task = event.payload as Task
        setTasks(prev => prev.map(t => t.id === task.id ? { ...t, ...task } : t))
        break
      }
    }
  }, []))

  return { epics, stories, tasks, loading, error, refresh: load }
}
