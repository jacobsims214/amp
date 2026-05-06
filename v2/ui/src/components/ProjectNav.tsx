import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowLeft, LayoutGrid, BookOpen, GitBranch, BarChart2 } from 'lucide-react'
import { api } from '../api/client'
import type { Project } from '../types'

export type ProjectView = 'board' | 'kb' | 'dag' | 'report'

interface ProjectNavProps {
  projectId: number
  currentView: ProjectView
  /** Page-specific controls rendered on the right side of the nav bar */
  rightSlot?: React.ReactNode
}

const TABS: { view: ProjectView; label: string; icon: React.ReactNode; path: (id: number) => string }[] = [
  { view: 'board',  label: 'Board',  icon: <LayoutGrid size={13} />, path: id => `/project/${id}` },
  { view: 'kb',     label: 'KB',     icon: <BookOpen   size={13} />, path: id => `/project/${id}/kb` },
  { view: 'dag',    label: 'DAG',    icon: <GitBranch  size={13} />, path: id => `/project/${id}/dag` },
  { view: 'report', label: 'Report', icon: <BarChart2  size={13} />, path: id => `/project/${id}/report` },
]

export function ProjectNav({ projectId, currentView, rightSlot }: ProjectNavProps) {
  const [project, setProject] = useState<Project | null>(null)

  useEffect(() => {
    api.getProject(projectId).then(setProject).catch(() => null)
  }, [projectId])

  return (
    <header
      className="flex items-center gap-0 flex-shrink-0 select-none"
      style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45', height: 48 }}
    >
      {/* Back to projects */}
      <Link
        to="/"
        className="flex items-center justify-center w-11 h-full text-[#3D5068] hover:text-[#DDE6F0] transition-colors cursor-pointer flex-shrink-0"
        title="All projects"
      >
        <ArrowLeft size={15} />
      </Link>

      {/* Separator */}
      <div className="w-px h-5 bg-[#1E2C45] flex-shrink-0" />

      {/* Project identity */}
      <div className="flex items-center gap-2 px-3 flex-shrink-0 min-w-0">
        {project ? (
          <>
            <span className="text-sm font-bold text-[#DDE6F0] tracking-tight truncate max-w-[160px]">
              {project.name}
            </span>
            <span
              className="text-[10px] font-semibold font-mono flex-shrink-0"
              style={{
                background: 'rgba(99,102,241,0.12)',
                color: '#818CF8',
                border: '1px solid rgba(99,102,241,0.22)',
                padding: '1px 6px',
                borderRadius: 6,
              }}
            >
              {project.code}
            </span>
          </>
        ) : (
          <span className="text-sm font-bold text-[#3D5068]">Loading…</span>
        )}
      </div>

      {/* Separator */}
      <div className="w-px h-5 bg-[#1E2C45] flex-shrink-0" />

      {/* View tabs */}
      <nav className="flex items-center h-full px-2 gap-0.5">
        {TABS.map(tab => {
          const isActive = tab.view === currentView
          return (
            <Link
              key={tab.view}
              to={tab.path(projectId)}
              className="flex items-center gap-1.5 px-3 h-8 rounded-lg text-xs font-semibold transition-all cursor-pointer relative"
              style={{
                color:      isActive ? '#DDE6F0' : '#3D5068',
                background: isActive ? '#172540' : 'transparent',
                border:     isActive ? '1px solid #283A57' : '1px solid transparent',
              }}
              onMouseEnter={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = '#7E91A8' }}
              onMouseLeave={e => { if (!isActive) (e.currentTarget as HTMLElement).style.color = '#3D5068' }}
            >
              <span style={{ color: isActive ? '#818CF8' : 'inherit' }}>{tab.icon}</span>
              {tab.label}
              {/* Active underline dot */}
              {isActive && (
                <span
                  className="absolute bottom-0.5 left-1/2 -translate-x-1/2 w-1 h-1 rounded-full"
                  style={{ background: '#6366F1' }}
                />
              )}
            </Link>
          )
        })}
      </nav>

      {/* Right slot — page-specific controls */}
      {rightSlot && (
        <>
          <div className="w-px h-5 bg-[#1E2C45] flex-shrink-0 ml-1" />
          <div className="flex items-center gap-2 px-3 ml-auto flex-shrink-0">
            {rightSlot}
          </div>
        </>
      )}
      {/* Spacer when no rightSlot */}
      {!rightSlot && <div className="flex-1" />}
    </header>
  )
}
