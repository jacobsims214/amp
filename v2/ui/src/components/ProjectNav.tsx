import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Project } from '../types'
import { PageHeader } from './ui/PageHeader'
import { projectCounts, useWaitCounts } from '../lib/waitCounts'

export type ProjectView = 'mission' | 'my-work' | 'board' | 'plan' | 'kb' | 'dag' | 'report' | 'artifacts' | 'settings'

interface ProjectNavProps {
  projectId: number
  currentView: ProjectView
  /** Page-specific controls on the right of the header */
  rightSlot?: React.ReactNode
  /** The project, when the page already has a fresher copy (e.g. after a rename) */
  project?: Project | null
  /** A line under the title */
  meta?: React.ReactNode
}

const TABS: { view: ProjectView; label: string; path: (id: number) => string }[] = [
  { view: 'mission',  label: 'Mission',     path: id => `/project/${id}/mission` },
  { view: 'my-work',  label: 'My work',     path: id => `/project/${id}/my-work` },
  { view: 'board',    label: 'Board',       path: id => `/project/${id}/board` },
  { view: 'plan',     label: 'Flight plan', path: id => `/project/${id}/plan` },
  { view: 'dag',      label: 'Graph',       path: id => `/project/${id}/dag` },
  { view: 'report',   label: 'Reports',     path: id => `/project/${id}/report` },
  { view: 'artifacts', label: 'Artifacts',  path: id => `/project/${id}/artifacts` },
  { view: 'kb',       label: 'Library',     path: id => `/project/${id}/kb` },
  { view: 'settings', label: 'Settings',    path: id => `/project/${id}/settings` },
]

/** The header of every project page: project name and code, its views, and
 *  the page's own actions. My work's tab carries the "waiting on you" count. */
export function ProjectNav({ projectId, rightSlot, project: given, meta }: ProjectNavProps) {
  const [loaded, setProject] = useState<Project | null>(null)
  const project = given ?? loaded
  const waiting = projectCounts(useWaitCounts(), projectId)?.waiting

  useEffect(() => {
    api.getProject(projectId).then(setProject).catch(() => null)
  }, [projectId])

  return (
    <PageHeader
      crumb={<>Projects{project ? <> · <span className="amp-num tracking-normal">{project.code}</span></> : null}</>}
      title={project ? project.name : <span className="text-ink-3">Loading…</span>}
      meta={meta}
      actions={rightSlot}
      tabs={TABS.map(t => t.view === 'my-work'
        ? { to: t.path(projectId), label: t.label, count: waiting, hot: (waiting ?? 0) > 0, countLabel: waiting != null ? 'waiting on you' : undefined }
        : { to: t.path(projectId), label: t.label })}
    />
  )
}
