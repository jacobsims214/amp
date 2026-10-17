import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Archive, BookOpen, Building2, Loader2, Lock, RotateCcw, Save, Users } from 'lucide-react'
import { api } from '../api/client'
import { ProjectNav } from '../components/ProjectNav'
import { TeamAccessPanel } from '../components/TeamAccessModal'
import { MetadataEditor } from '../components/MetadataEditor'
import { metadataPatch } from '../lib/metadata'
import { inputCls, labelCls } from '../components/CrudModal'
import { canManageProject } from '../lib/people'
import { WebhooksPanel } from '../components/WebhooksPanel'
import { AuditLogPanel } from '../components/AuditLogPanel'
import type { KBSource, Metadata, Org, Project, ProjectAccess } from '../types'

function Section({ title, description, children }: { title: string; description?: string; children: React.ReactNode }) {
  return (
    <section className="rounded-2xl p-5" style={{ background: 'var(--surface)', border: '1px solid var(--line)' }}>
      <h2 className="text-sm font-bold text-ink">{title}</h2>
      {description && <p className="text-xs text-ink-2 mt-1 mb-4 leading-relaxed">{description}</p>}
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
  // The edited metadata; null until the user changes something.
  const [meta, setMeta] = useState<{ value: Metadata; error: string | null } | null>(null)
  const [metaSaving, setMetaSaving] = useState(false)
  const [metaSaved, setMetaSaved] = useState(false)
  const metaPatch = project && meta ? metadataPatch(project.metadata, meta.value) : undefined

  const saveMeta = async () => {
    if (!metaPatch || meta?.error) return
    setMetaSaving(true); setError(null); setMetaSaved(false)
    try {
      setProject(await api.updateProjectSettings(pid, { metadata: metaPatch }))
      setMetaSaved(true)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setMetaSaving(false)
    }
  }

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
    <div className="flex flex-col h-full" style={{ background: 'var(--bg)' }}>
      <ProjectNav projectId={pid} currentView="settings" project={project} />
      <div className="flex-1 overflow-y-auto">
        <div className="max-w-2xl mx-auto px-4 py-8 space-y-5">
          <div>
            <h1 className="text-xl font-bold text-ink tracking-tight">Project settings</h1>
            {!canManage && myAccess && (
              <p className="text-xs text-ink-2 mt-1 flex items-center gap-1.5">
                <Lock size={11} />Only project owners and org admins can change these settings.
              </p>
            )}
          </div>
          {error && <p className="text-xs text-danger-ink">{error}</p>}

          {!project ? (
            <div className="flex justify-center py-10"><Loader2 size={18} className="text-primary animate-spin" /></div>
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
                  <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-ink-2">
                    <span>Code <span className="font-mono text-ink">{project.code}</span></span>
                    {org && <span>Org <span className="text-ink">{org.name}</span></span>}
                    <span>State <span className="text-ink capitalize">{project.state}</span></span>
                  </div>
                  {canManage && (
                    <div className="flex items-center gap-3">
                      <button onClick={save} disabled={!dirty || saving || !name.trim()}
                        className="flex items-center gap-1.5 px-3 py-2 bg-primary hover:bg-primary disabled:opacity-50 disabled:cursor-not-allowed text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer">
                        {saving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
                        Save
                      </button>
                      {saved && !dirty && <span className="text-xs text-success-ink">Saved</span>}
                    </div>
                  )}
                </div>
              </Section>

              <Section title="Metadata"
                description="Free-form fields for integrations and reporting, e.g. jira_board, initiative, capitalizable. Every epic, story, and ticket in the project inherits them and can override them.">
                <MetadataEditor key={project.id} initial={project.metadata} readOnly={!canManage}
                  onChange={(value, error) => { setMeta({ value, error }); setMetaSaved(false) }} />
                {canManage && (
                  <div className="flex items-center gap-3 mt-3">
                    <button onClick={saveMeta} disabled={!metaPatch || !!meta?.error || metaSaving}
                      className="flex items-center gap-1.5 px-3 py-2 bg-primary hover:bg-primary disabled:opacity-50 disabled:cursor-not-allowed text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer">
                      {metaSaving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
                      Save fields
                    </button>
                    {metaSaved && !metaPatch && <span className="text-xs text-success-ink">Saved</span>}
                  </div>
                )}
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
                        <Icon size={14} className="flex-shrink-0 text-link" />
                        <span className="flex-1 min-w-0">
                          <span className="block text-sm text-ink truncate">{s.scope === 'project' ? project.name : s.name}</span>
                          <span className="block text-[11px] text-ink-3">{kind}</span>
                        </span>
                        {s.can_read
                          ? <span className="text-xs text-ink-2">{s.docs ?? 0} doc{s.docs === 1 ? '' : 's'}</span>
                          : <span className="flex items-center gap-1 text-[11px] text-ink-3"><Lock size={10} />team members only</span>}
                      </>
                    )
                    return s.can_read ? (
                      <Link key={`${s.scope}-${s.scope_id}`} to={to}
                        className="flex items-center gap-3 px-3 py-2.5 rounded-xl hover:bg-surface-3 transition-colors">{body}</Link>
                    ) : (
                      <div key={`${s.scope}-${s.scope_id}`} className="flex items-center gap-3 px-3 py-2.5 opacity-70">{body}</div>
                    )
                  })}
                </div>
              </Section>

              {canManage && (
                <Section title="Webhooks"
                  description="Send this project's ticket changes to another service or a Slack channel. Org admins can also add webhooks for every project in the org.">
                  <WebhooksPanel projectId={pid} canManage={canManage} />
                </Section>
              )}

              {canManage && (
                <Section title="Audit log"
                  description="Who changed this project's access and settings, and who deleted what. Ticket-level history is on each ticket.">
                  <AuditLogPanel projectId={pid} />
                </Section>
              )}

              {canManage && (
                <Section title={project.state === 'archived' ? 'Restore' : 'Archive'}
                  description={project.state === 'archived'
                    ? 'This project is archived: hidden from the active list and read-only for agents.'
                    : 'Archiving hides the project from the active list. Nothing is deleted, and you can restore it.'}>
                  {project.state === 'archived' ? (
                    <button onClick={() => setArchived(false)}
                      className="flex items-center gap-1.5 px-3 py-2 rounded-lg text-xs font-semibold cursor-pointer transition-all"
                      style={{ border: '1px solid var(--line-str)', color: 'var(--ink)' }}>
                      <RotateCcw size={12} />Restore project
                    </button>
                  ) : (
                    <button onClick={() => setArchived(true)}
                      className="flex items-center gap-1.5 px-3 py-2 rounded-lg text-xs font-semibold cursor-pointer transition-all"
                      style={{ border: '1px solid color-mix(in srgb, var(--danger) 30%, transparent)', color: 'var(--danger-ink)' }}>
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
