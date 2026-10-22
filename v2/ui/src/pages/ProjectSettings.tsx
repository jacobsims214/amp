import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Archive, BookOpen, Building2, Loader2, Lock, RotateCcw, Save, Users } from 'lucide-react'
import { api } from '../api/client'
import { TrashPanel } from '../components/TrashPanel'
import { ProjectNav } from '../components/ProjectNav'
import { TeamAccessPanel } from '../components/TeamAccessModal'
import { ProjectOrgPicker } from '../components/ProjectOrgPicker'
import { MetadataEditor } from '../components/MetadataEditor'
import { metadataPatch } from '../lib/metadata'
import { inputCls, labelCls } from '../components/CrudModal'
import { canManageProject } from '../lib/people'
import { WebhooksPanel } from '../components/WebhooksPanel'
import { AuditLogPanel } from '../components/AuditLogPanel'
import { ConfirmButton } from '../components/ui/ConfirmButton'
import type { KBSource, Metadata, Org, Project, ProjectAccess } from '../types'
import { BandLoader } from '../components/brand/BandLoader'
import { LastApproved } from '../components/PlanApprovalBanner'
import { useProjectPeople } from '../hooks/useProjectPeople'
import { planPath } from '../lib/approval'

const slug = (t: string) => t.toLowerCase().replace(/[^a-z]+/g, '-')

/** One settings section: a square panel with a title, what it's for, and its controls. */
function Section({ title, description, danger, children }: { title: string; description?: string; danger?: boolean; children: React.ReactNode }) {
  return (
    <section id={slug(title)} className={`bg-surface border scroll-mt-6 ${danger ? 'border-danger' : 'border-line'}`}>
      <div className="px-5 pt-4 pb-3 border-b border-line">
        <h2 className="m-0 text-[15px] font-semibold" style={{ color: danger ? 'var(--danger-ink)' : 'var(--ink)' }}>{title}</h2>
        {description && <p className="m-0 mt-1 text-[12.5px] text-ink-2 leading-relaxed max-w-[68ch]">{description}</p>}
      </div>
      <div className="px-5 py-4">{children}</div>
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
  const { people: approvalPeople } = useProjectPeople(pid)
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
      <div className="flex-1 grid grid-cols-1 lg:grid-cols-[200px_minmax(0,1fr)] gap-6 px-7 py-6 items-start">
        <nav className="hidden lg:grid gap-0.5 sticky top-6" aria-label="Settings sections">
          <span className="amp-label px-2.5 pb-2">Settings</span>
          {['General', 'Plan approval', 'Metadata', 'Team access', 'Knowledge bases', 'Trash', ...(canManage ? ['Webhooks', 'Audit log', 'Danger zone'] : [])].map(t => (
            <a key={t} href={`#${slug(t)}`} className="px-2.5 py-1.5 text-[13px] text-ink-2 hover:text-ink hover:bg-surface-3">{t}</a>
          ))}
        </nav>
        <div className="grid gap-4 max-w-3xl min-w-0">
          {!canManage && myAccess && (
            <p className="m-0 amp-panel px-4 py-3 text-[12.5px] text-ink-2 flex items-center gap-1.5">
              <Lock size={12} />Only project owners and org admins can change these settings.
            </p>
          )}
          {error && <p className="m-0 amp-panel px-4 py-3 text-[12.5px] text-danger-ink">{error}</p>}

          {!project ? (
            <div className="flex justify-center py-10"><BandLoader label="Loading settings" /></div>
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
                    <span>State <span className="text-ink capitalize">{project.state}</span></span>
                  </div>
                  <div>
                    <label className={labelCls}>Org</label>
                    <ProjectOrgPicker project={project} org={org} onMoved={p => {
                      setProject(p)
                      if (p.org_id) api.getOrg(p.org_id).then(setOrg).catch(() => setOrg(null))
                      api.listProjectMembers(pid).then(r => setMyAccess(r.my_access)).catch(() => setMyAccess(null))
                    }} />
                  </div>
                  {canManage && (
                    <div className="flex items-center gap-3">
                      <button onClick={save} disabled={!dirty || saving || !name.trim()}
                        className="amp-btn amp-btn-primary">
                        {saving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
                        Save
                      </button>
                      {saved && !dirty && <span className="text-xs text-success-ink">Saved</span>}
                    </div>
                  )}
                </div>
              </Section>

              <Section title="Plan approval"
                description="Agents plan; a person approves before they start. With this on, tickets planned (or rewritten) after the last approval can't be dispatched until a person approves the plan in Flight plan. Only a person can approve, here in the AMP web UI: agents, and managers acting for you over MCP, can't approve, and an agent saying you approved in chat doesn't count.">
                <label className="flex items-center gap-3 text-[13px] text-ink cursor-pointer select-none">
                  <input type="checkbox" className="w-4 h-4 accent-[var(--ink)]" disabled={!canManage}
                    checked={!!project.require_plan_approval}
                    onChange={async e => {
                      const on = e.target.checked
                      try { setProject(await api.updateProjectSettings(pid, { require_plan_approval: on })) }
                      catch (err) { setError(err instanceof Error ? err.message : 'Could not change the setting') }
                    }} />
                  Require a person's approval before agents start new tickets
                </label>
                <p className="m-0 mt-2 text-[12px] text-ink-2 leading-relaxed">
                  {project.require_plan_approval
                    ? <>On. Turning it off works only from this page; agents asking over MCP are refused.</>
                    : <>Off: agents can start tickets nobody has approved. Turn it back on any time.</>}
                </p>
                <p className="m-0 mt-1 text-[12px] text-ink-2">
                  <LastApproved project={project} people={approvalPeople} />{' '}
                  <Link to={`${planPath(pid)}#approve`} className="text-ink underline underline-offset-2">Review and approve in Flight plan</Link>
                </p>
              </Section>

              <Section title="Metadata"
                description="Free-form fields for integrations and reporting, e.g. jira_board, initiative, capitalizable. Every epic, story, and ticket in the project inherits them and can override them.">
                <MetadataEditor key={project.id} initial={project.metadata} readOnly={!canManage}
                  onChange={(value, error) => { setMeta({ value, error }); setMetaSaved(false) }} />
                {canManage && (
                  <div className="flex items-center gap-3 mt-3">
                    <button onClick={saveMeta} disabled={!metaPatch || !!meta?.error || metaSaving}
                      className="amp-btn amp-btn-primary">
                      {metaSaving ? <Loader2 size={12} className="animate-spin" /> : <Save size={12} />}
                      Save fields
                    </button>
                    {metaSaved && !metaPatch && <span className="text-xs text-success-ink">Saved</span>}
                  </div>
                )}
              </Section>

              <Section title="Team access" description="Teams reach the project through these links. Owner teams manage the project; member teams work in it.">
                <TeamAccessPanel key={`${project.org_id ?? 'none'}-${myAccess}`} projectId={pid} myAccess={myAccess} onProjectUpdated={setProject} />
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
                        <Icon size={14} className="flex-shrink-0 text-ink-2" />
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
                        className="flex items-center gap-3 px-3 py-2.5 border border-line hover:border-ink transition-colors">{body}</Link>
                    ) : (
                      <div key={`${s.scope}-${s.scope_id}`} className="flex items-center gap-3 px-3 py-2.5 opacity-70">{body}</div>
                    )
                  })}
                </div>
              </Section>

              <Section title="Trash"
                description="Tickets deleted in the last 30 days. Restoring brings back their text, owner, state, comments, history, and dependency links.">
                <TrashPanel projectId={pid} />
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
                <Section title="Danger zone" danger
                  description={project.state === 'archived'
                    ? 'This project is archived: hidden from the active list and read-only for agents. Restoring brings it back as it was.'
                    : 'Archiving hides the project from the active list and stops agents changing it. Nothing is deleted, and you can restore it.'}>
                  {project.state === 'archived'
                    ? <button type="button" onClick={() => setArchived(false)} className="amp-btn amp-btn-sm"><RotateCcw size={12} />Restore project</button>
                    : <ConfirmButton label="Archive project" confirmLabel="Archive it" danger icon={<Archive size={12} />} onConfirm={() => setArchived(true)} />}
                </Section>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  )
}
