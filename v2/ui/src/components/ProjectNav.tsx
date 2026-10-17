import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Project } from '../types'
import { PageHeader } from './ui/PageHeader'

export type ProjectView = 'mission' | 'board' | 'kb' | 'dag' | 'report' | 'settings'

interface ProjectNavProps {
  projectId: number
  currentView: ProjectView
  /** Page-specific controls on the right of the header */
  rightSlot?: React.ReactNode
  /** The project, when the page already has a fresher copy (e.g. after a rename) */
  project?: Project | null
}

const TABS: { view: ProjectView; label: string; path: (id: number) => string }[] = [
  { view: 'mission',  label: 'Mission',     path: id => `/project/${id}` },
  { view: 'board',    label: 'Board',       path: id => `/project/${id}/board` },
  { view: 'dag',      label: 'Flight plan', path: id => `/project/${id}/dag` },
  { view: 'report',   label: 'Reports',     path: id => `/project/${id}/report` },
  { view: 'kb',       label: 'Library',     path: id => `/project/${id}/kb` },
  { view: 'settings', label: 'Settings',    path: id => `/project/${id}/settings` },
]

/** The header of every project page: project name and code, its views, and
 *  the page's own actions. */
export function ProjectNav({ projectId, rightSlot, project: given }: ProjectNavProps) {
  const [loaded, setProject] = useState<Project | null>(null)
  const project = given ?? loaded

  useEffect(() => {
    api.getProject(projectId).then(setProject).catch(() => null)
  }, [projectId])

  return (
    <PageHeader
      crumb={<>Projects{project ? <> · <span className="amp-num tracking-normal">{project.code}</span></> : null}</>}
      title={project ? project.name : <span className="text-ink-3">Loading…</span>}
      actions={rightSlot}
      tabs={TABS.map(t => ({ to: t.path(projectId), label: t.label, end: t.view === 'mission' }))}
    />
  )
}
