import { useState, useEffect, useCallback, useRef } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Loader2, LayoutDashboard, Clock, Layers, Wifi, Archive, RotateCcw, ChevronDown, ChevronUp, Download, Upload, ShieldCheck } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
import { useAuth } from '../hooks/useAuth'
import type { Project, SSEEvent } from '../types'

function relativeTime(iso: string) {
  const diff = Date.now() - new Date(iso).getTime()
  const days = Math.floor(diff / 86400000)
  const hrs = Math.floor(diff / 3600000)
  const mins = Math.floor(diff / 60000)
  if (days > 0) return `${days}d ago`
  if (hrs > 0) return `${hrs}h ago`
  if (mins > 0) return `${mins}m ago`
  return 'just now'
}

export function Projects() {
  const navigate = useNavigate()
  const { me, isAdmin } = useAuth()
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [projects, setProjects] = useState<Project[]>([])
  const [archivedProjects, setArchivedProjects] = useState<Project[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [live, setLive] = useState(false)
  const [archivedExpanded, setArchivedExpanded] = useState(false)
  const [archivingId, setArchivingId] = useState<number | null>(null)
  const [restoringId, setRestoringId] = useState<number | null>(null)
  const [exportingId, setExportingId] = useState<number | null>(null)
  const [exportError, setExportError] = useState<string | null>(null)
  const [importing, setImporting] = useState(false)
  const [importError, setImportError] = useState<string | null>(null)

  useEffect(() => {
    api.listProjects()
      .then(allProjects => {
        const active = allProjects.filter(p => p.state === 'active')
        const archived = allProjects.filter(p => p.state === 'archived')
        setProjects(active)
        setArchivedProjects(archived)
      })
      .catch(e => setError(e.message))
      .finally(() => setLoading(false))
  }, [])

  useSSE(null, useCallback((event: SSEEvent) => {
    if (event.type === 'connected') { setLive(true); return }
    if (event.type === 'project.created' && event.payload) {
      const p = event.payload as Project
      setProjects(prev => prev.find(x => x.id === p.id) ? prev : [...prev, p])
    }
    if (event.type === 'project.archived' && event.payload) {
      const p = event.payload as Project
      setProjects(prev => prev.filter(x => x.id !== p.id))
      setArchivedProjects(prev => prev.find(x => x.id === p.id) ? prev : [...prev, p])
    }
    if (event.type === 'project.restored' && event.payload) {
      const p = event.payload as Project
      setArchivedProjects(prev => prev.filter(x => x.id !== p.id))
      setProjects(prev => prev.find(x => x.id === p.id) ? prev : [...prev, p])
    }
  }, []))

  const handleExport = async (e: React.MouseEvent, projectId: number) => {
    e.preventDefault(); e.stopPropagation()
    setExportingId(projectId); setExportError(null)
    try {
      const { blob, filename } = await api.exportProject(projectId)
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url; link.download = filename
      document.body.appendChild(link); link.click()
      document.body.removeChild(link); URL.revokeObjectURL(url)
    } catch (err) {
      setExportError(err instanceof Error ? err.message : 'Export failed')
    } finally { setExportingId(null) }
  }

  const handleImportClick = () => fileInputRef.current?.click()

  const handleFileSelected = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    setImporting(true); setImportError(null)
    try {
      const text = await file.text()
      const newProject = await api.importProject(text)
      navigate(`/project/${newProject.id}`)
    } catch (err) {
      setImportError(err instanceof Error ? err.message : 'Import failed')
    } finally {
      setImporting(false)
      if (fileInputRef.current) fileInputRef.current.value = ''
    }
  }

  return (
    <div className="min-h-screen" style={{ background: '#08101F' }}>
      {/* Header */}
      <header style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45' }}>
        <div className="max-w-5xl mx-auto px-6 py-4 flex items-center justify-between">
          <div className="flex items-center gap-3">
            {/* Logo mark */}
            <div
              className="w-8 h-8 rounded-xl flex items-center justify-center flex-shrink-0"
              style={{
                background: 'linear-gradient(135deg, #6366F1 0%, #4338CA 100%)',
                boxShadow: '0 0 20px rgba(99,102,241,0.35)',
              }}
            >
              <LayoutDashboard size={14} className="text-white" />
            </div>
            <div className="flex items-center gap-2">
              <span className="text-base font-bold text-[#DDE6F0] tracking-tight">AMP</span>
              <span
                className="text-[10px] font-semibold text-[#6366F1] px-1.5 py-0.5 rounded-md font-mono"
                style={{ background: 'rgba(99,102,241,0.12)', border: '1px solid rgba(99,102,241,0.25)' }}
              >
                v2
              </span>
            </div>
          </div>
          <div className="flex items-center gap-4">
            {/* Live status */}
            <div className={`flex items-center gap-1.5 text-xs font-medium ${live ? 'text-[#10B981]' : 'text-[#3D5068]'}`}>
              <span className={`w-1.5 h-1.5 rounded-full ${live ? 'bg-[#10B981]' : 'bg-[#3D5068]'}`}
                style={live ? { boxShadow: '0 0 6px #10B981' } : undefined} />
              <Wifi size={11} />
              <span>{live ? 'Live' : 'Connecting…'}</span>
            </div>
            <span className="text-xs text-[#3D5068]">
              {projects.length} {projects.length !== 1 ? 'projects' : 'project'}
            </span>
            {isAdmin && (
              <Link
                to="/admin/users"
                title="Manage users"
                className="flex items-center gap-1 text-xs font-medium text-[#7E91A8] hover:text-[#818CF8] transition-colors"
              >
                <ShieldCheck size={13} />
                Users
              </Link>
            )}
            {me && (
              <span className="text-xs text-[#3D5068]" title={me.subject}>
                {me.email}
              </span>
            )}
            <button
              onClick={handleImportClick}
              disabled={importing}
              className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed"
              style={{ background: 'rgba(99,102,241,0.12)', border: '1px solid rgba(99,102,241,0.25)', color: '#818CF8' }}
            >
              {importing ? <Loader2 size={12} className="animate-spin" /> : <Upload size={12} />}
              Import
            </button>
            <input ref={fileInputRef} type="file" accept=".json" onChange={handleFileSelected} className="hidden" aria-label="Import project file" />
          </div>
        </div>
      </header>

      <main className="max-w-5xl mx-auto px-6 py-10">
        {/* Page title */}
        <div className="mb-8">
          <h1 className="text-2xl font-bold text-[#DDE6F0] tracking-tight">Projects</h1>
          <p className="text-sm text-[#7E91A8] mt-1.5">Select a project to open its board</p>
        </div>

        {loading && (
          <div className="flex justify-center py-20">
            <Loader2 size={22} className="text-[#6366F1] animate-spin" />
          </div>
        )}

        {error && (
          <div className="rounded-xl border border-[#EF4444]/20 bg-[#EF4444]/8 px-4 py-3 text-sm text-[#F87171] mb-6">
            {error} — is amp-api running on port 3001?
          </div>
        )}

        {importError && (
          <div className="rounded-xl border border-[#EF4444]/20 bg-[#EF4444]/8 px-4 py-3 text-sm text-[#F87171] mb-6">
            Import failed: {importError}
          </div>
        )}

        {!loading && !error && projects.length === 0 && (
          <div className="flex flex-col items-center justify-center py-24 gap-5">
            <div
              className="w-16 h-16 rounded-2xl flex items-center justify-center"
              style={{ background: '#0D1726', border: '1px solid #1E2C45' }}
            >
              <Layers size={26} className="text-[#283A57]" />
            </div>
            <div className="text-center">
              <p className="text-base font-semibold text-[#DDE6F0]">No projects yet</p>
              <p className="text-sm text-[#7E91A8] mt-2 max-w-xs leading-relaxed">
                Ask the{' '}
                <code
                  className="text-[#818CF8] font-mono text-xs px-1.5 py-0.5 rounded-md"
                  style={{ background: 'rgba(99,102,241,0.1)' }}
                >
                  amp-manager
                </code>
                {' '}to create a project — it will appear here instantly.
              </p>
            </div>
          </div>
        )}

        {/* Active projects grid */}
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {projects.map(p => (
            <ProjectCard
              key={p.id}
              project={p}
              onExport={handleExport}
              exporting={exportingId === p.id}
              exportError={exportingId === p.id ? exportError : null}
              onArchive={() => {
                if (window.confirm('Archive this project? It will be hidden from the active list.')) {
                  setArchivingId(p.id)
                  api.archiveProject(p.id)
                    .then(archived => {
                      setProjects(prev => prev.filter(x => x.id !== archived.id))
                      setArchivedProjects(prev => prev.find(x => x.id === archived.id) ? prev : [...prev, archived])
                    })
                    .catch(e => setError(e.message))
                    .finally(() => setArchivingId(null))
                }
              }}
              archiving={archivingId === p.id}
            />
          ))}
        </div>

        {/* Archived projects */}
        {archivedProjects.length > 0 && (
          <div className="mt-14">
            <button
              onClick={() => setArchivedExpanded(!archivedExpanded)}
              className="flex items-center gap-2 text-sm font-semibold text-[#DDE6F0] hover:text-white transition-colors mb-5 cursor-pointer"
            >
              {archivedExpanded ? <ChevronUp size={15} /> : <ChevronDown size={15} />}
              Archived ({archivedProjects.length})
            </button>

            {archivedExpanded && (
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
                {archivedProjects.map(p => (
                  <div
                    key={p.id}
                    className="rounded-2xl p-5 opacity-60"
                    style={{ background: '#0D1726', border: '1px solid #1E2C45' }}
                  >
                    <div className="flex items-center gap-3 mb-3">
                      <ProjectAvatar code={p.code} muted />
                      <span className="text-xs text-[#3D5068] font-mono">#{p.id}</span>
                    </div>
                    <h3 className="text-sm font-semibold text-[#DDE6F0] mb-1 leading-snug">{p.name}</h3>
                    <p className="text-xs text-[#7E91A8] font-mono mb-4">{p.code}</p>
                    <button
                      onClick={() => {
                        setRestoringId(p.id)
                        api.restoreProject(p.id)
                          .then(restored => {
                            setArchivedProjects(prev => prev.filter(x => x.id !== restored.id))
                            setProjects(prev => prev.find(x => x.id === restored.id) ? prev : [...prev, restored])
                          })
                          .catch(e => setError(e.message))
                          .finally(() => setRestoringId(null))
                      }}
                      disabled={restoringId === p.id}
                      className="w-full flex items-center justify-center gap-2 px-3 py-2 rounded-lg text-xs font-semibold transition-all cursor-pointer disabled:opacity-50 active:scale-[0.97]"
                      style={{ background: '#172540', color: '#818CF8', border: '1px solid #283A57' }}
                    >
                      <RotateCcw size={11} />
                      Restore
                    </button>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </main>
    </div>
  )
}

/* ─── Project avatar ──────────────────────────────────────────────────────── */
function ProjectAvatar({ code, muted = false }: { code: string; muted?: boolean }) {
  return (
    <div
      className="w-10 h-10 rounded-xl flex items-center justify-center flex-shrink-0 text-sm font-bold"
      style={muted
        ? { background: '#111E30', color: '#3D5068', border: '1px solid #192238' }
        : {
            background: 'linear-gradient(135deg, rgba(99,102,241,0.2) 0%, rgba(67,56,202,0.3) 100%)',
            color: '#818CF8',
            border: '1px solid rgba(99,102,241,0.25)',
            boxShadow: '0 0 14px rgba(99,102,241,0.12)',
          }
      }
    >
      {code.slice(0, 2).toUpperCase()}
    </div>
  )
}

/* ─── Project card ────────────────────────────────────────────────────────── */
interface ProjectCardProps {
  project: Project
  onExport: (e: React.MouseEvent, id: number) => void
  exporting: boolean
  exportError: string | null
  onArchive: () => void
  archiving: boolean
}

function ProjectCard({ project: p, onExport, exporting, exportError, onArchive, archiving }: ProjectCardProps) {
  return (
    <div
      className="group rounded-2xl p-5 transition-all duration-200 hover:-translate-y-0.5 cursor-pointer"
      style={{
        background: '#0D1726',
        border: '1px solid #1E2C45',
      }}
      onMouseEnter={e => {
        (e.currentTarget as HTMLElement).style.borderColor = 'rgba(99,102,241,0.3)'
        ;(e.currentTarget as HTMLElement).style.boxShadow = '0 8px 32px rgba(0,0,0,0.35), 0 0 0 1px rgba(99,102,241,0.1)'
      }}
      onMouseLeave={e => {
        (e.currentTarget as HTMLElement).style.borderColor = '#1E2C45'
        ;(e.currentTarget as HTMLElement).style.boxShadow = 'none'
      }}
    >
      {/* Top row */}
      <div className="flex items-start justify-between gap-3 mb-4">
        <Link to={`/project/${p.id}`} className="flex items-center gap-3 flex-1 min-w-0">
          <ProjectAvatar code={p.code} />
          <span className="text-xs text-[#3D5068] font-mono">#{p.id}</span>
        </Link>
        <div className="flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity">
          <button
            onClick={(e) => onExport(e, p.id)}
            disabled={exporting}
            className="p-1.5 rounded-lg transition-colors cursor-pointer disabled:opacity-50"
            style={{ background: 'rgba(99,102,241,0.1)', border: '1px solid rgba(99,102,241,0.2)', color: '#818CF8' }}
            title="Export project"
          >
            {exporting ? <Loader2 size={13} className="animate-spin" /> : <Download size={13} />}
          </button>
          <button
            onClick={onArchive}
            disabled={archiving}
            className="p-1.5 rounded-lg text-[#7E91A8] hover:text-[#EF4444] hover:bg-[#EF4444]/10 transition-colors cursor-pointer disabled:opacity-50"
            title="Archive project"
          >
            <Archive size={13} />
          </button>
        </div>
      </div>

      <Link to={`/project/${p.id}`} className="block">
        <h3 className="text-sm font-bold text-[#DDE6F0] group-hover:text-white transition-colors mb-1 leading-snug">
          {p.name}
        </h3>
        <p className="text-xs text-[#3D5068] font-mono mb-3">{p.code}</p>
        {p.description && (
          <p className="text-xs text-[#7E91A8] mb-4 line-clamp-2 leading-relaxed">{p.description}</p>
        )}
        <div className="flex items-center gap-1.5 text-xs text-[#3D5068]">
          <Clock size={10} />
          <span>Updated {relativeTime(p.updated_at)}</span>
        </div>
      </Link>

      {exportError && (
        <div className="mt-3 text-xs text-[#F87171] bg-[#EF4444]/8 border border-[#EF4444]/20 rounded-lg px-3 py-2">
          {exportError}
        </div>
      )}
    </div>
  )
}
