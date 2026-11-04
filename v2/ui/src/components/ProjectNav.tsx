import { useEffect, useState } from 'react'
import { useLocation } from 'react-router-dom'
import { api } from '../api/client'
import type { Project } from '../types'
import { PageHeader } from './ui/PageHeader'
import { viewLink } from '../lib/projectLink'
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

const TABS: { view: ProjectView; label: string }[] = [
  { view: 'mission',  label: 'Mission' },
  { view: 'my-work',  label: 'My work' },
  { view: 'board',    label: 'Board' },
  { view: 'plan',     label: 'Flight plan' },
  { view: 'dag',      label: 'Graph' },
  { view: 'report',   label: 'Reports' },
  { view: 'artifacts', label: 'Artifacts' },
  { view: 'kb',       label: 'Library' },
  { view: 'settings', label: 'Settings' },
]

/** The header of every project page: project name and code, its views, and
 *  the page's own actions. My work's tab carries the "waiting on you" count. */
export function ProjectNav({ projectId, rightSlot, project: given, meta }: ProjectNavProps) {
  const [loaded, setProject] = useState<Project | null>(null)
  const project = given ?? loaded
  const waiting = projectCounts(useWaitCounts(), projectId)?.waiting
  // Switching views keeps the open ticket (?focus=) where the view honors it.
  const { search } = useLocation()
  const path = (view: ProjectView) => viewLink(projectId, view, search)

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
        ? { to: path(t.view), label: t.label, count: waiting, hot: (waiting ?? 0) > 0, countLabel: waiting != null ? 'waiting on you' : undefined }
        : { to: path(t.view), label: t.label })}
    />
  )
}
