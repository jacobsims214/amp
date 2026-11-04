import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Epic, Story, Task } from '../types'

export interface ProjectPlan {
  tasks: Task[]
  epics: Epic[]
  stories: Story[]
  /** false until the first load answered (or failed) */
  loaded: boolean
}

const EMPTY: ProjectPlan = { tasks: [], epics: [], stories: [], loaded: false }

/**
 * A project's tickets, epics and stories, read once per opened ticket: the
 * drawer's human panel (what a ticket unblocks, where it sits) and its
 * Details tab (story and dependency pickers) share this one read. The drawer
 * stays mounted between tickets, so `refreshKey` (the open ticket's id)
 * rereads it when another ticket opens; the previous read stays on screen
 * meanwhile.
 */
export function useProjectPlan(projectId: number | undefined, refreshKey?: unknown): ProjectPlan {
  const [plan, setPlan] = useState<ProjectPlan & { projectId?: number }>(EMPTY)

  useEffect(() => {
    if (projectId == null) return
    let live = true
    Promise.all([api.listTasks(projectId), api.listEpics(projectId)])
      .then(async ([tasks, epics]) => {
        const stories = (await Promise.all(epics.map(e => api.listStories(e.id)))).flat()
        if (live) setPlan({ projectId, tasks, epics, stories, loaded: true })
      })
      .catch(() => { if (live) setPlan(prev => prev.projectId === projectId ? { ...prev, loaded: true } : { ...EMPTY, projectId, loaded: true }) })
    return () => { live = false }
  }, [projectId, refreshKey])

  // A plan read for another project is never shown for this one.
  return plan.projectId === projectId ? plan : EMPTY
}
