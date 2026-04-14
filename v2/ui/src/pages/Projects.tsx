import { useState, useEffect, useCallback, useRef } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Loader2, LayoutDashboard, Clock, Layers, Wifi, Archive, RotateCcw, ChevronDown, ChevronUp, Download, Upload } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
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

  // SSE: add new projects, handle archive/restore
  useSSE(null, useCallback((event: SSEEvent) => {
    if (event.type === 'connected') {
      setLive(true)
      return
    }
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
    e.preventDefault()
    e.stopPropagation()
    
    setExportingId(projectId)
    setExportError(null)
    
    try {
      const { blob, filename } = await api.exportProject(projectId)
      
      // Create a blob URL and trigger download
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = filename
      document.body.appendChild(link)
      link.click()
      document.body.removeChild(link)
      URL.revokeObjectURL(url)
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Export failed'
      setExportError(message)
      console.error('Export error:', err)
    } finally {
      setExportingId(null)
    }
  }

  const handleImportClick = () => {
    fileInputRef.current?.click()
  }

  const handleFileSelected = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return

    setImporting(true)
    setImportError(null)

    try {
      const text = await file.text()
      const newProject = await api.importProject(text)
      
      // Navigate to the new project's board
      navigate(`/project/${newProject.id}`)
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Import failed'
      setImportError(message)
      console.error('Import error:', err)
    } finally {
      setImporting(false)
      // Reset the file input
      if (fileInputRef.current) {
        fileInputRef.current.value = ''
      }
    }
  }

  return (
    <div className="min-h-screen bg-[#0d1117]">
      <header className="border-b border-[#21262d] bg-[#161b22]">
        <div className="max-w-5xl mx-auto px-6 py-4 flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div className="w-7 h-7 rounded-md bg-[#58a6ff]/20 border border-[#58a6ff]/30 flex items-center justify-center">
              <LayoutDashboard size={14} className="text-[#58a6ff]" />
            </div>
            <span className="text-base font-semibold text-[#e6edf3]">AMP</span>
            <span className="text-xs text-[#484f58] bg-[#21262d] px-2 py-0.5 rounded-full font-mono">v2</span>
          </div>
          <div className="flex items-center gap-4">
            <div className={`flex items-center gap-1.5 text-xs ${live ? 'text-[#3fb950]' : 'text-[#484f58]'}`}>
              <Wifi size={12} />
              <span>{live ? 'Live' : 'Connecting…'}</span>
            </div>
            <span className="text-xs text-[#8b949e]">
              {projects.length} active {projects.length !== 1 ? 'projects' : 'project'}
            </span>
            <button
              onClick={handleImportClick}
              disabled={importing}
              className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-[#58a6ff]/10 border border-[#58a6ff]/30 text-[#58a6ff] hover:bg-[#58a6ff]/20 hover:border-[#58a6ff]/50 transition-colors text-xs font-medium disabled:opacity-50 disabled:cursor-not-allowed"
              title="Import project from JSON file"
            >
              {importing ? (
                <Loader2 size={12} className="animate-spin" />
              ) : (
                <Upload size={12} />
              )}
              <span>Import</span>
            </button>
            <input
              ref={fileInputRef}
              type="file"
              accept=".json"
              onChange={handleFileSelected}
              className="hidden"
              aria-label="Import project file"
            />
          </div>
        </div>
      </header>

      <main className="max-w-5xl mx-auto px-6 py-8">
        <div className="mb-6">
          <h1 className="text-xl font-semibold text-[#e6edf3]">Projects</h1>
          <p className="text-sm text-[#8b949e] mt-1">Select a project to open its board</p>
        </div>

        {loading && (
          <div className="flex justify-center py-16">
            <Loader2 size={24} className="text-[#58a6ff] animate-spin" />
          </div>
        )}

        {error && (
          <div className="rounded-lg border border-[#f85149]/30 bg-[#f85149]/10 px-4 py-3 text-sm text-[#f85149]">
            {error} — is amp-api running on port 3001?
          </div>
        )}

        {importError && (
          <div className="rounded-lg border border-[#f85149]/30 bg-[#f85149]/10 px-4 py-3 text-sm text-[#f85149] mb-6">
            Import failed: {importError}
          </div>
        )}

        {!loading && !error && projects.length === 0 && (
          <div className="flex flex-col items-center justify-center py-20 gap-4">
            <div className="w-14 h-14 rounded-xl bg-[#161b22] border border-[#30363d] flex items-center justify-center">
              <Layers size={24} className="text-[#484f58]" />
            </div>
            <div className="text-center">
              <p className="text-sm font-medium text-[#e6edf3]">No projects yet</p>
              <p className="text-xs text-[#8b949e] mt-1.5 max-w-xs">
                Ask the <code className="bg-[#161b22] px-1.5 py-0.5 rounded text-[#58a6ff] font-mono text-xs">amp-manager</code> to create a project — it will appear here instantly.
              </p>
            </div>
          </div>
        )}

        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {projects.map(p => (
            <div
              key={p.id}
              className="group rounded-xl border border-[#30363d] bg-[#161b22] p-5 hover:border-[#58a6ff]/40 hover:bg-[#1c2333] transition-all duration-150 hover:shadow-lg hover:shadow-black/20"
            >
              <div className="flex items-start justify-between gap-3 mb-3">
                <Link
                  to={`/project/${p.id}`}
                  className="flex-1 flex items-start gap-3"
                >
                  <div className="w-9 h-9 rounded-lg bg-[#58a6ff]/10 border border-[#58a6ff]/20 flex items-center justify-center flex-shrink-0">
                    <span className="text-sm font-bold text-[#58a6ff]">
                      {p.code.slice(0, 2).toUpperCase()}
                    </span>
                  </div>
                  <span className="text-xs text-[#484f58] font-mono mt-1">#{p.id}</span>
                </Link>
                <div className="flex items-center gap-1.5">
                  <button
                    onClick={(e) => handleExport(e, p.id)}
                    disabled={exportingId === p.id}
                    className="flex-shrink-0 p-1.5 rounded-lg bg-[#58a6ff]/10 border border-[#58a6ff]/20 text-[#58a6ff] hover:bg-[#58a6ff]/20 hover:border-[#58a6ff]/40 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                    title="Export project"
                  >
                    {exportingId === p.id ? (
                      <Loader2 size={14} className="animate-spin" />
                    ) : (
                      <Download size={14} />
                    )}
                  </button>
                  <button
                    onClick={() => {
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
                    disabled={archivingId === p.id}
                    className="flex-shrink-0 p-1.5 rounded-lg hover:bg-[#30363d] text-[#8b949e] hover:text-[#f85149] transition-colors disabled:opacity-50"
                    title="Archive project"
                  >
                    <Archive size={14} />
                  </button>
                </div>
              </div>
              <Link
                to={`/project/${p.id}`}
                className="block"
              >
                <h3 className="text-sm font-semibold text-[#e6edf3] group-hover:text-white transition-colors mb-1 leading-snug">
                  {p.name}
                </h3>
                <p className="text-xs text-[#8b949e] font-mono mb-3">{p.code}</p>
                {p.description && (
                  <p className="text-xs text-[#8b949e] mb-3 line-clamp-2 leading-relaxed">{p.description}</p>
                )}
                <div className="flex items-center gap-1.5 text-xs text-[#484f58]">
                  <Clock size={11} />
                  <span>Updated {relativeTime(p.updated_at)}</span>
                </div>
              </Link>
              {exportError && exportingId === p.id && (
                <div className="mt-3 text-xs text-[#f85149] bg-[#f85149]/10 border border-[#f85149]/30 rounded px-2 py-1">
                  {exportError}
                </div>
              )}
            </div>
          ))}
        </div>

        {archivedProjects.length > 0 && (
          <div className="mt-12">
            <button
              onClick={() => setArchivedExpanded(!archivedExpanded)}
              className="flex items-center gap-2 text-sm font-semibold text-[#e6edf3] hover:text-white transition-colors mb-4"
            >
              {archivedExpanded ? <ChevronUp size={16} /> : <ChevronDown size={16} />}
              Archived Projects ({archivedProjects.length})
            </button>

            {archivedExpanded && (
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
                {archivedProjects.map(p => (
                  <div
                    key={p.id}
                    className="rounded-xl border border-[#30363d] bg-[#161b22] p-5 opacity-75"
                  >
                    <div className="flex items-start justify-between gap-3 mb-3">
                      <div className="w-9 h-9 rounded-lg bg-[#58a6ff]/10 border border-[#58a6ff]/20 flex items-center justify-center flex-shrink-0">
                        <span className="text-sm font-bold text-[#58a6ff]">
                          {p.code.slice(0, 2).toUpperCase()}
                        </span>
                      </div>
                      <span className="text-xs text-[#484f58] font-mono">#{p.id}</span>
                    </div>
                    <h3 className="text-sm font-semibold text-[#e6edf3] mb-1 leading-snug">
                      {p.name}
                    </h3>
                    <p className="text-xs text-[#8b949e] font-mono mb-3">{p.code}</p>
                    {p.description && (
                      <p className="text-xs text-[#8b949e] mb-4 line-clamp-2 leading-relaxed">{p.description}</p>
                    )}
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
                      className="w-full flex items-center justify-center gap-2 px-3 py-2 rounded-lg bg-[#30363d] hover:bg-[#3d444d] text-[#58a6ff] hover:text-white transition-colors text-xs font-medium disabled:opacity-50"
                    >
                      <RotateCcw size={12} />
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
