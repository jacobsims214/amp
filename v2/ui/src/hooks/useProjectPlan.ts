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
 * A project's tickets, epics and stories, read once for the ticket drawer:
 * its human panel (what a ticket unblocks, where it sits) and its Details
 * tab (story and dependency pickers) share this one read.
 */
export function useProjectPlan(projectId: number | undefined): ProjectPlan {
  const [plan, setPlan] = useState<ProjectPlan & { projectId?: number }>(EMPTY)

  useEffect(() => {
    if (projectId == null) return
    let live = true
    Promise.all([api.listTasks(projectId), api.listEpics(projectId)])
      .then(async ([tasks, epics]) => {
        const stories = (await Promise.all(epics.map(e => api.listStories(e.id)))).flat()
        if (live) setPlan({ projectId, tasks, epics, stories, loaded: true })
      })
      .catch(() => { if (live) setPlan({ ...EMPTY, projectId, loaded: true }) })
    return () => { live = false }
  }, [projectId])

  // A plan read for another project is never shown for this one.
  return plan.projectId === projectId ? plan : EMPTY
}
