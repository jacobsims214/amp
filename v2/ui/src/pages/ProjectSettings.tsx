import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Archive, BookOpen, Building2, Loader2, Lock, RotateCcw, Save, Users } from 'lucide-react'
import { api } from '../api/client'
import { ProjectNav } from '../components/ProjectNav'
import { TeamAccessPanel } from '../components/TeamAccessModal'
import { inputCls, labelCls } from '../components/CrudModal'
import { canManageProject } from '../lib/people'
import type { KBSource, Org, Project, ProjectAccess } from '../types'

function Section({ title, description, children }: { title: string; description?: string; children: React.ReactNode }) {
  return (
    <section className="rounded-2xl p-5" style={{ background: '#0D1726', border: '1px solid #1E2C45' }}>
      <h2 className="text-sm font-bold text-[#DDE6F0]">{title}</h2>
      {description && <p className="text-xs text-[#7E91A8] mt-1 mb-4 leading-relaxed">{description}</p>}
      {!description && <div className="mb-4" />}
      {children}
    </section>
  )
}

// /project/:projectId/settings — everything about a project that isn't its
// work: name and description, who has access, ticket rules, its knowledge
// bases, and archiving. Project owners (and org/global admins) can change
// it; everyone else sees it read-only.
export function ProjectSettings() {
  const { projectId } = useParams<{ projectId: string }>()
  const pid = Number(projectId)
  const [project, setProject] = useState<Project | null>(null)
  const [org, setOrg] = useState<Org | null>(null)
  const [myAccess, setMyAccess] = useState<ProjectAccess | null>(null)
  const [sources, setSources] = useState<KBSource[]>([])
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)
  const canManage = canManageProject(myAccess)

  useEffect(() => {
    api.getProject(pid).then(p => {
      setProject(p); setName(p.name); setDescription(p.description ?? '')
      if (p.org_id) api.getOrg(p.org_id).then(setOrg).catch(() => setOrg(null))
    }).catch(e => setError(e instanceof Error ? e.message : 'Failed to load the project'))
    api.listProjectMembers(pid).then(r => setMyAccess(r.my_access)).catch(() => setMyAccess(null))
    api.kbSources(pid).then(setSources).catch(() => setSources([]))
  }, [pid])

  const dirty = project != null && (name.trim() !== project.name || description !== (project.description ?? ''))

  const save = async () => {
    if (!project) return
    setSaving(true); setError(null); setSaved(false)
    try {
      const p = await api.updateProjectSettings(pid, { name: name.trim(), description })
      setProject(p); setName(p.name); setDescription(p.description ?? ''); setSaved(true)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const setArchived = async (archive: boolean) => {
    if (archive && !window.confirm('Archive this project? It will be hidden from the active list.')) return
    setError(null)
    try {
      setProject(archive ? await api.archiveProject(pid) : await api.restoreProject(pid))
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Update failed')
    }
  }

  return (
    <div className="flex flex-col h-screen" style={{ background: '#08101F' }}>
      <ProjectNav projectId={pid} currentView="settings" project={project} />
      <div className="flex-1 overflow-y-auto">
        <div className="max-w-2xl mx-auto px-4 py-8 space-y-5">
          <div>
            <h1 className="text-xl font-bold text-[#DDE6F0] tracking-tight">Project settings</h1>
            {!canManage && myAccess && (
              <p className="text-xs text-[#7E91A8] mt-1 flex items-center gap-1.5">
                <Lock size={11} />Only project owners and org admins can change these settings.
              </p>
            )}
          </div>
          {error && <p className="text-xs text-[#F87171]">{error}</p>}

          {!project ? (
            <div className="flex justify-center py-10"><Loader2 size={18} className="text-[#6366F1] animate-spin" /></div>
          ) : (
            <>
              <Section title="General">
                <div className="space-y-4">
                  <div>
                    <label className={labelCls}>Name</label>
                    <input value={name} onChange={e => { setName(e.target.value); setSaved(false) }} disabled={!canManage} className={inputCls} />
                  </div>
                  <div>
                    <label className={labelCls}>Description</label>
                    <textarea value={description} onChange={e => { setDescription(e.target.value); setSaved(false) }} disabled={!canManage} rows={3}
                      className={`${inputCls} resize-none`} />
                  </div>
                  <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-[#7E91A8]">
                    <span>Code <span className="font-mono text-[#DDE6F0]">{project.code}</span></span>
                    {org && <span>Org <span className="text-[#DDE6F0]">{org.name}</span></span>}
                    <span>State <span className="text-[#DDE6F0] capitalize">{project.state}</span></span>
                  </div>
                  {canManage && (
                    <div className="flex items-center gap-3">
                      <button onClick={save} disabled={!dirty || saving || !name.trim()}
                        className="flex items-center gap-1.5 px-3 py-2 bg-[#6366F1] hover:bg-[#4F46E5] disabled:opacity-50 disabled:cursor-not-allowed text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer">
                        {saving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
                        Save
                      </button>
                      {saved && !dirty && <span className="text-xs text-[#34D399]">Saved</span>}
                    </div>
                  )}
                </div>
              </Section>

              <Section title="Team access" description="Teams reach the project through these links. Owner teams manage the project; member teams work in it.">
                <TeamAccessPanel projectId={pid} myAccess={myAccess} onProjectUpdated={setProject} />
              </Section>

              <Section title="Knowledge bases"
                description="Agents read and write the project KB by default. Team and org KBs hold knowledge shared beyond this project; agents use them when asked.">
                <div className="space-y-1">
                  {sources.map(s => {
                    const Icon = s.scope === 'org' ? Building2 : s.scope === 'team' ? Users : BookOpen
                    const to = s.scope === 'project' ? `/project/${pid}/kb` : s.scope === 'org' ? `/orgs/${s.scope_id}/kb` : `/teams/${s.scope_id}/kb`
                    const kind = s.scope === 'project' ? 'Project KB' : s.scope === 'org' ? 'Org KB' : `Team KB · ${s.team_role ?? 'member'} team`
                    const body = (
                      <>
                        <Icon size={14} className="flex-shrink-0 text-[#818CF8]" />
                        <span className="flex-1 min-w-0">
                          <span className="block text-sm text-[#DDE6F0] truncate">{s.scope === 'project' ? project.name : s.name}</span>
                          <span className="block text-[11px] text-[#3D5068]">{kind}</span>
                        </span>
                        {s.can_read
                          ? <span className="text-xs text-[#7E91A8]">{s.docs ?? 0} doc{s.docs === 1 ? '' : 's'}</span>
                          : <span className="flex items-center gap-1 text-[11px] text-[#3D5068]"><Lock size={10} />team members only</span>}
                      </>
                    )
                    return s.can_read ? (
                      <Link key={`${s.scope}-${s.scope_id}`} to={to}
                        className="flex items-center gap-3 px-3 py-2.5 rounded-xl hover:bg-[#172540] transition-colors">{body}</Link>
                    ) : (
                      <div key={`${s.scope}-${s.scope_id}`} className="flex items-center gap-3 px-3 py-2.5 opacity-70">{body}</div>
                    )
                  })}
                </div>
              </Section>

              {canManage && (
                <Section title={project.state === 'archived' ? 'Restore' : 'Archive'}
                  description={project.state === 'archived'
                    ? 'This project is archived: hidden from the active list and read-only for agents.'
                    : 'Archiving hides the project from the active list. Nothing is deleted, and you can restore it.'}>
                  {project.state === 'archived' ? (
                    <button onClick={() => setArchived(false)}
                      className="flex items-center gap-1.5 px-3 py-2 rounded-lg text-xs font-semibold cursor-pointer transition-all"
                      style={{ border: '1px solid #283A57', color: '#DDE6F0' }}>
                      <RotateCcw size={12} />Restore project
                    </button>
                  ) : (
                    <button onClick={() => setArchived(true)}
                      className="flex items-center gap-1.5 px-3 py-2 rounded-lg text-xs font-semibold cursor-pointer transition-all"
                      style={{ border: '1px solid rgba(239,68,68,0.3)', color: '#F87171' }}>
                      <Archive size={12} />Archive project
                    </button>
                  )}
                </Section>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  )
}
