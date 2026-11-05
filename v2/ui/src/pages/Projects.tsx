import { PageHeader } from '../components/ui/PageHeader'
import { useState, useEffect, useCallback, useRef } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { Loader2, Clock, Layers, Archive, RotateCcw, ChevronDown, ChevronUp, Download, Upload, Plus } from 'lucide-react'
import { api } from '../api/client'
import { useSSE } from '../hooks/useSSE'
import { CrudModal, FieldError, inputCls, labelCls } from '../components/CrudModal'
import { CODE_MAX, cleanCode, suggestCode } from '../lib/projectCode'
import { ManagerAgent } from '../components/ManagerAgent'
import { ProjectHomeFields, type ProjectHome } from '../components/ProjectHomeFields'
import type { Org, Project, SSEEvent } from '../types'
import { BandLoader } from '../components/brand/BandLoader'

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
  // /projects?new=1 (the Get started checklist's link) opens New project once the orgs are in.
  const [params, setParams] = useSearchParams()
  const [orgsLoaded, setOrgsLoaded] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [createForm, setCreateForm] = useState({ name: '', code: '', description: '' })
  const [createHome, setCreateHome] = useState<ProjectHome>(NO_HOME)
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)
  // Inline, per field, cleared as you type (#128).
  const [fieldErrors, setFieldErrors] = useState<{ name?: string; code?: string }>({})
  // The code follows the name until you type one yourself.
  const [codeTyped, setCodeTyped] = useState(false)
  const [importModalOpen, setImportModalOpen] = useState(false)
  const [importHome, setImportHome] = useState<ProjectHome>(NO_HOME)
  const importHomeRef = useRef<ProjectHome>(NO_HOME)

  useEffect(() => {
    api.listOrgs().then(setOrgs).catch(() => setOrgs([])).finally(() => setOrgsLoaded(true))
  }, [])

  // ?new opens the dialog as the New project button does, with the same default home.
  const wantsNew = orgsLoaded && params.has('new')
  useEffect(() => {
    if (!wantsNew) return
    openCreate()
    setParams(p => { const n = new URLSearchParams(p); n.delete('new'); return n }, { replace: true })
    // openCreate reads orgs, which have loaded once wantsNew is true
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wantsNew])

  const defaultHome = (): ProjectHome => ({ orgId: orgs.length > 1 ? orgs[0].id : null, teamId: null })

  const openCreate = () => {
    setCreateForm({ name: '', code: '', description: '' })
    setCreateHome(defaultHome())
    setCreateError(null)
    setFieldErrors({})
    setCodeTyped(false)
    setCreateOpen(true)
  }

  const handleCreate = async () => {
    const missing = {
      ...(!createForm.name.trim() ? { name: 'Give the project a name.' } : {}),
      ...(!createForm.code.trim() ? { code: 'Give it a code: letters A-Z and digits.' } : {}),
    }
    setFieldErrors(missing)
    if (missing.name || missing.code) {
      document.getElementById(missing.name ? 'new-project-name' : 'new-project-code')?.focus()
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
      <PageHeader crumb="Overview" title="Projects"
        meta={<>{projects.length} {projects.length === 1 ? 'project' : 'projects'} you can see · <span className={live ? 'text-success-ink' : 'text-ink-3'}>{live ? 'live' : 'connecting…'}</span></>}
        actions={<>
          <button onClick={handleImportClick} disabled={importing} className="amp-btn">
            {importing ? <Loader2 size={12} className="animate-spin" /> : <Upload size={12} />}Import
          </button>
          <button onClick={openCreate} className="amp-btn amp-btn-primary"><Plus size={12} />New project</button>
          <input ref={fileInputRef} type="file" accept=".json" onChange={handleFileSelected} className="hidden" aria-label="Import project file" />
        </>} />

      <main className="px-7 py-5">

        {loading && (
          <div className="flex justify-center py-20">
            <BandLoader label="Loading projects" />
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
            <div className="text-center grid justify-items-center gap-3">
              <p className="m-0 text-base font-semibold text-ink">No projects yet</p>
              <button onClick={openCreate} className="amp-btn amp-btn-primary"><Plus size={12} />Create your first project</button>
              <p className="m-0 text-sm text-ink-2 max-w-xs leading-relaxed">
                Or describe what you want to your <ManagerAgent /> and it creates the project. It appears here at once.
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
            <div><label htmlFor="new-project-name" className={labelCls}>Name *</label>
              <input id="new-project-name" autoFocus value={createForm.name}
                onChange={e => {
                  const name = e.target.value
                  setCreateForm(f => ({ ...f, name, code: codeTyped ? f.code : suggestCode(name) }))
                  setFieldErrors(fe => ({ ...fe, name: undefined, ...(codeTyped ? {} : { code: undefined }) }))
                }}
                aria-invalid={!!fieldErrors.name} aria-describedby={fieldErrors.name ? 'new-project-name-error' : undefined}
                className={inputCls} placeholder="Project name" />
              <FieldError id="new-project-name-error">{fieldErrors.name}</FieldError></div>
            <div><label htmlFor="new-project-code" className={labelCls}>Code *</label>
              <input id="new-project-code" value={createForm.code}
                onChange={e => { setCodeTyped(true); setCreateForm(f => ({ ...f, code: cleanCode(e.target.value) })); setFieldErrors(fe => ({ ...fe, code: undefined })) }}
                aria-invalid={!!fieldErrors.code} aria-describedby={fieldErrors.code ? 'new-project-code-error' : 'new-project-code-hint'}
                className={`${inputCls} font-mono`} placeholder="e.g. AMP" maxLength={CODE_MAX} />
              {fieldErrors.code
                ? <FieldError id="new-project-code-error">{fieldErrors.code}</FieldError>
                : <p id="new-project-code-hint" className="text-[11px] text-ink-3 mt-1.5">Letters A-Z and digits, up to {CODE_MAX}.{codeTyped ? '' : ' Suggested from the name.'}</p>}</div>
            <div><label htmlFor="new-project-description" className={labelCls}>Description</label>
              <textarea id="new-project-description" value={createForm.description} onChange={e => setCreateForm(f => ({ ...f, description: e.target.value }))} rows={3} className={`${inputCls} resize-none`} placeholder="Optional description" /></div>
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
    <div className="group amp-panel amp-panel-top flex flex-col hover:border-line-str transition-colors">
      <div className="flex items-start gap-3 px-4 pt-4">
        <span className="amp-bands-v h-[34px]" aria-hidden="true" />
        <Link to={`/project/${p.id}`} className="flex-1 min-w-0">
          <span className="amp-label">Project · <span className="amp-num tracking-normal">{p.code}</span></span>
          <h3 className="amp-heading text-[16px] leading-tight text-ink mt-1.5 mb-0 group-hover:underline underline-offset-2">{p.name}</h3>
        </Link>
        <div className="flex items-center gap-1 opacity-0 group-hover:opacity-100 focus-within:opacity-100 transition-opacity">
          <button onClick={(e) => onExport(e, p.id)} disabled={exporting} className="amp-btn amp-btn-ghost amp-btn-sm" title="Export project" aria-label="Export project">
            {exporting ? <Loader2 size={13} className="animate-spin" /> : <Download size={13} />}
          </button>
          <button onClick={onArchive} disabled={archiving} className="amp-btn amp-btn-ghost amp-btn-sm" title="Archive project" aria-label="Archive project">
            <Archive size={13} />
          </button>
        </div>
      </div>
      <Link to={`/project/${p.id}`} className="block px-4 pb-4 pt-3 flex-1">
        {p.description && <p className="text-[13px] text-ink-2 mb-3 line-clamp-2 leading-relaxed">{p.description}</p>}
        <span className="amp-num text-[11px] text-ink-3 inline-flex items-center gap-1.5"><Clock size={10} />updated {relativeTime(p.updated_at)}</span>
      </Link>
      {exportError && <div className="mx-4 mb-4 text-[12px] text-danger-ink border border-danger px-3 py-2">{exportError}</div>}
    </div>
  )
}
