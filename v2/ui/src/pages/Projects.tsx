import { useState, useEffect, useCallback, useRef } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Loader2, LayoutDashboard, Clock, Layers, Wifi, Archive, RotateCcw, ChevronDown, ChevronUp, Download, Upload, ShieldCheck, Building2, Plus, CalendarDays, KeyRound } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
import { useAuth } from '../hooks/useAuth'
import { CrudModal, inputCls, labelCls } from '../components/CrudModal'
import { ProjectHomeFields, type ProjectHome } from '../components/ProjectHomeFields'
import type { Org, Project, SSEEvent } from '../types'

const NO_HOME: ProjectHome = { orgId: null, teamId: null }

function homeParams(home: ProjectHome) {
  return {
    ...(home.orgId != null ? { org_id: home.orgId } : {}),
    ...(home.teamId != null ? { team_id: home.teamId } : {}),
  }
}

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

  // Orgs the caller can place projects in (global admins see all).
  const [orgs, setOrgs] = useState<Org[]>([])
  const [createOpen, setCreateOpen] = useState(false)
  const [createForm, setCreateForm] = useState({ name: '', code: '', description: '' })
  const [createHome, setCreateHome] = useState<ProjectHome>(NO_HOME)
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)
  const [importModalOpen, setImportModalOpen] = useState(false)
  const [importHome, setImportHome] = useState<ProjectHome>(NO_HOME)
  const importHomeRef = useRef<ProjectHome>(NO_HOME)

  useEffect(() => {
    api.listOrgs().then(setOrgs).catch(() => setOrgs([]))
  }, [])

  const defaultHome = (): ProjectHome => ({ orgId: orgs.length > 1 ? orgs[0].id : null, teamId: null })

  const openCreate = () => {
    setCreateForm({ name: '', code: '', description: '' })
    setCreateHome(defaultHome())
    setCreateError(null)
    setCreateOpen(true)
  }

  const handleCreate = async () => {
    if (!createForm.name.trim() || !createForm.code.trim()) {
      setCreateError('Name and code are required')
      return
    }
    setCreating(true)
    setCreateError(null)
    try {
      const p = await api.createProject({
        name: createForm.name.trim(),
        code: createForm.code.trim(),
        description: createForm.description.trim(),
        ...homeParams(createHome),
      })
      setCreateOpen(false)
      navigate(`/project/${p.id}`)
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : 'Create failed')
    } finally {
      setCreating(false)
    }
  }

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

  // With several orgs to choose from, ask where the import should live first.
  const handleImportClick = () => {
    if (orgs.length > 1) {
      setImportHome(defaultHome())
      setImportModalOpen(true)
      return
    }
    importHomeRef.current = NO_HOME
    fileInputRef.current?.click()
  }

  const handleImportChooseFile = () => {
    importHomeRef.current = importHome
    setImportModalOpen(false)
    fileInputRef.current?.click() // must stay synchronous with the click for the file dialog to open
  }

  const handleFileSelected = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    setImporting(true); setImportError(null)
    try {
      const text = await file.text()
      const newProject = await api.importProject(text, homeParams(importHomeRef.current))
      navigate(`/project/${newProject.id}`)
    } catch (err) {
      setImportError(err instanceof Error ? err.message : 'Import failed')
    } finally {
      setImporting(false)
      if (fileInputRef.current) fileInputRef.current.value = ''
    }
  }

  return (
    <div className="min-h-full" style={{ background: 'var(--bg)' }}>
      {/* Header */}
      <header style={{ background: 'var(--surface)', borderBottom: '1px solid var(--line)' }}>
        <div className="max-w-5xl mx-auto px-6 py-4 flex items-center justify-between">
          <div className="flex items-center gap-3">
            {/* Logo mark */}
            <div
              className="w-8 h-8 rounded-xl flex items-center justify-center flex-shrink-0"
              style={{
                background: 'linear-gradient(135deg, var(--primary) 0%, var(--primary) 100%)',
                boxShadow: '0 0 20px color-mix(in srgb, var(--link) 35%, transparent)',
              }}
            >
              <LayoutDashboard size={14} className="text-white" />
            </div>
            <div className="flex items-center gap-2">
              <span className="text-base font-bold text-ink tracking-tight">AMP</span>
              <span
                className="text-[10px] font-semibold text-primary px-1.5 py-0.5 rounded-md font-mono"
                style={{ background: 'color-mix(in srgb, var(--link) 12%, transparent)', border: '1px solid color-mix(in srgb, var(--link) 25%, transparent)' }}
              >
                v2
              </span>
            </div>
          </div>
          <div className="flex items-center gap-4">
            {/* Live status */}
            <div className={`flex items-center gap-1.5 text-xs font-medium ${live ? 'text-success' : 'text-ink-3'}`}>
              <span className={`w-1.5 h-1.5 rounded-full ${live ? 'bg-success' : 'bg-ink-3'}`}
                style={live ? { boxShadow: '0 0 6px var(--success)' } : undefined} />
              <Wifi size={11} />
              <span>{live ? 'Live' : 'Connecting…'}</span>
            </div>
            <span className="text-xs text-ink-3">
              {projects.length} {projects.length !== 1 ? 'projects' : 'project'}
            </span>
            {isAdmin && (
              <Link
                to="/admin/users"
                title="Manage users"
                className="flex items-center gap-1 text-xs font-medium text-ink-2 hover:text-link transition-colors"
              >
                <ShieldCheck size={13} />
                Users
              </Link>
            )}
            {me && (
              <Link
                to="/reports"
                title="End-of-day reports and velocity"
                className="flex items-center gap-1 text-xs font-medium text-ink-2 hover:text-link transition-colors"
              >
                <CalendarDays size={13} />
                Daily reports
              </Link>
            )}
            {me?.user_id != null && (
              <Link
                to="/access"
                title="API tokens and agent accounts"
                className="flex items-center gap-1 text-xs font-medium text-ink-2 hover:text-link transition-colors"
              >
                <KeyRound size={13} />
                Agents &amp; tokens
              </Link>
            )}
            {me && (
              <Link
                to="/admin/orgs"
                title="Organizations and teams"
                className="flex items-center gap-1 text-xs font-medium text-ink-2 hover:text-link transition-colors"
              >
                <Building2 size={13} />
                Orgs
              </Link>
            )}
            {me && (
              <span className="text-xs text-ink-3" title={me.subject}>
                {me.email}
              </span>
            )}
            <button
              onClick={openCreate}
              className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer bg-primary hover:bg-primary text-white"
            >
              <Plus size={12} />
              New project
            </button>
            <button
              onClick={handleImportClick}
              disabled={importing}
              className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed"
              style={{ background: 'color-mix(in srgb, var(--link) 12%, transparent)', border: '1px solid color-mix(in srgb, var(--link) 25%, transparent)', color: 'var(--link)' }}
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
          <h1 className="text-2xl font-bold text-ink tracking-tight">Projects</h1>
          <p className="text-sm text-ink-2 mt-1.5">Select a project to open its board</p>
        </div>

        {loading && (
          <div className="flex justify-center py-20">
            <Loader2 size={22} className="text-primary animate-spin" />
          </div>
        )}

        {error && (
          <div className="rounded-xl border border-danger/20 bg-danger/8 px-4 py-3 text-sm text-danger-ink mb-6">
            {error} — is amp-api running on port 3001?
          </div>
        )}

        {importError && (
          <div className="rounded-xl border border-danger/20 bg-danger/8 px-4 py-3 text-sm text-danger-ink mb-6">
            Import failed: {importError}
          </div>
        )}

        {!loading && !error && projects.length === 0 && (
          <div className="flex flex-col items-center justify-center py-24 gap-5">
            <div
              className="w-16 h-16 rounded-2xl flex items-center justify-center"
              style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}
            >
              <Layers size={26} className="text-line-str" />
            </div>
            <div className="text-center">
              <p className="text-base font-semibold text-ink">No projects yet</p>
              <p className="text-sm text-ink-2 mt-2 max-w-xs leading-relaxed">
                Ask the{' '}
                <code
                  className="text-link font-mono text-xs px-1.5 py-0.5 rounded-md"
                  style={{ background: 'color-mix(in srgb, var(--link) 10%, transparent)' }}
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
              className="flex items-center gap-2 text-sm font-semibold text-ink hover:text-white transition-colors mb-5 cursor-pointer"
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
                    style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}
                  >
                    <div className="flex items-center gap-3 mb-3">
                      <ProjectAvatar code={p.code} muted />
                      <span className="text-xs text-ink-3 font-mono">#{p.id}</span>
                    </div>
                    <h3 className="text-sm font-semibold text-ink mb-1 leading-snug">{p.name}</h3>
                    <p className="text-xs text-ink-2 font-mono mb-4">{p.code}</p>
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
                      style={{ background: 'var(--surface-3)', color: 'var(--link)', border: '1px solid var(--line-str)' }}
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

      {createOpen && (
        <CrudModal title="New Project" onClose={() => setCreateOpen(false)} onSave={handleCreate} saving={creating} saveLabel="Create">
          <div className="space-y-4">
            <div><label className={labelCls}>Name *</label>
              <input autoFocus value={createForm.name} onChange={e => setCreateForm(f => ({ ...f, name: e.target.value }))} className={inputCls} placeholder="Project name" /></div>
            <div><label className={labelCls}>Code *</label>
              <input value={createForm.code} onChange={e => setCreateForm(f => ({ ...f, code: e.target.value.toUpperCase() }))} className={`${inputCls} font-mono`} placeholder="e.g. AMP" maxLength={12} /></div>
            <div><label className={labelCls}>Description</label>
              <textarea value={createForm.description} onChange={e => setCreateForm(f => ({ ...f, description: e.target.value }))} rows={3} className={`${inputCls} resize-none`} placeholder="Optional description" /></div>
            <ProjectHomeFields orgs={orgs} value={createHome} onChange={setCreateHome} />
            {createError && <p className="text-xs text-danger-ink">{createError}</p>}
          </div>
        </CrudModal>
      )}

      {importModalOpen && (
        <CrudModal title="Import Project" onClose={() => setImportModalOpen(false)} onSave={handleImportChooseFile} saveLabel="Choose file…">
          <div className="space-y-4">
            <p className="text-xs text-ink-2">Choose where the imported project should live, then pick an export bundle (.json).</p>
            <ProjectHomeFields orgs={orgs} value={importHome} onChange={setImportHome} />
          </div>
        </CrudModal>
      )}
    </div>
  )
}

/* ─── Project avatar ──────────────────────────────────────────────────────── */
function ProjectAvatar({ code, muted = false }: { code: string; muted?: boolean }) {
  return (
    <div
      className="w-10 h-10 rounded-xl flex items-center justify-center flex-shrink-0 text-sm font-bold"
      style={muted
        ? { background: 'var(--surface-2)', color: 'var(--ink-3)', border: '1px solid var(--line-sub)' }
        : {
            background: 'linear-gradient(135deg, color-mix(in srgb, var(--link) 20%, transparent) 0%, color-mix(in srgb, var(--link) 30%, transparent) 100%)',
            color: 'var(--link)',
            border: '1px solid color-mix(in srgb, var(--link) 25%, transparent)',
            boxShadow: '0 0 14px color-mix(in srgb, var(--link) 12%, transparent)',
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
        background: 'var(--surface)',
        border: '1px solid var(--line)',
      }}
      onMouseEnter={e => {
        (e.currentTarget as HTMLElement).style.borderColor = 'color-mix(in srgb, var(--link) 30%, transparent)'
        ;(e.currentTarget as HTMLElement).style.boxShadow = '0 8px 32px color-mix(in srgb, var(--ink) 18%, transparent), 0 0 0 1px color-mix(in srgb, var(--link) 10%, transparent)'
      }}
      onMouseLeave={e => {
        (e.currentTarget as HTMLElement).style.borderColor = 'var(--line)'
        ;(e.currentTarget as HTMLElement).style.boxShadow = 'none'
      }}
    >
      {/* Top row */}
      <div className="flex items-start justify-between gap-3 mb-4">
        <Link to={`/project/${p.id}`} className="flex items-center gap-3 flex-1 min-w-0">
          <ProjectAvatar code={p.code} />
          <span className="text-xs text-ink-3 font-mono">#{p.id}</span>
        </Link>
        <div className="flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity">
          <button
            onClick={(e) => onExport(e, p.id)}
            disabled={exporting}
            className="p-1.5 rounded-lg transition-colors cursor-pointer disabled:opacity-50"
            style={{ background: 'color-mix(in srgb, var(--link) 10%, transparent)', border: '1px solid color-mix(in srgb, var(--link) 20%, transparent)', color: 'var(--link)' }}
            title="Export project"
          >
            {exporting ? <Loader2 size={13} className="animate-spin" /> : <Download size={13} />}
          </button>
          <button
            onClick={onArchive}
            disabled={archiving}
            className="p-1.5 rounded-lg text-ink-2 hover:text-danger hover:bg-danger/10 transition-colors cursor-pointer disabled:opacity-50"
            title="Archive project"
          >
            <Archive size={13} />
          </button>
        </div>
      </div>

      <Link to={`/project/${p.id}`} className="block">
        <h3 className="text-sm font-bold text-ink group-hover:text-white transition-colors mb-1 leading-snug">
          {p.name}
        </h3>
        <p className="text-xs text-ink-3 font-mono mb-3">{p.code}</p>
        {p.description && (
          <p className="text-xs text-ink-2 mb-4 line-clamp-2 leading-relaxed">{p.description}</p>
        )}
        <div className="flex items-center gap-1.5 text-xs text-ink-3">
          <Clock size={10} />
          <span>Updated {relativeTime(p.updated_at)}</span>
        </div>
      </Link>

      {exportError && (
        <div className="mt-3 text-xs text-danger-ink bg-danger/8 border border-danger/20 rounded-lg px-3 py-2">
          {exportError}
        </div>
      )}
    </div>
  )
}
