import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Loader2, Settings, Trash2, Users } from 'lucide-react'
import { api } from '../api/client'
import { CrudModal, inputCls, labelCls } from './CrudModal'
import { canManageProject } from '../lib/people'
import type { Project, ProjectAccess, ProjectTeam, ProjectTeamRole, Team } from '../types'

interface PanelProps {
  projectId: number
  myAccess: ProjectAccess | null
  /** called after a project setting changes */
  onProjectUpdated?: (project: Project) => void
}

// The board's quick "Team access" dialog; the same panel is on the project
// settings page.
export function TeamAccessModal({ onClose, ...props }: PanelProps & { onClose: () => void }) {
  return (
    <CrudModal title="Team access" onClose={onClose}>
      <TeamAccessPanel {...props} />
      <Link to={`/project/${props.projectId}/settings`}
        className="mt-5 inline-flex items-center gap-1.5 text-xs text-link hover:text-ink transition-colors">
        <Settings size={12} />All project settings
      </Link>
    </CrudModal>
  )
}

// Lists the teams linked to a project. Project owners (and org/global
// admins) can change each team's role, unlink it, or link another team from
// the project's org, and set who may reassign tickets; everyone else gets a
// read-only view.
export function TeamAccessPanel({ projectId, myAccess, onProjectUpdated }: PanelProps) {
  const canManage = canManageProject(myAccess)
  const [teams, setTeams] = useState<ProjectTeam[]>([])
  const [orgTeams, setOrgTeams] = useState<Team[]>([])
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [addTeamId, setAddTeamId] = useState('')
  const [addRole, setAddRole] = useState<ProjectTeamRole>('member')
  const [openReassign, setOpenReassign] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const [linked, project] = await Promise.all([api.listProjectTeams(projectId), api.getProject(projectId)])
        if (cancelled) return
        setTeams(linked)
        setOpenReassign(project.open_reassign ?? false)
        if (canManage && project.org_id) {
          const all = await api.listOrgTeams(project.org_id)
          if (!cancelled) setOrgTeams(all)
        }
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : 'Failed to load teams')
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    load()
    return () => { cancelled = true }
  }, [projectId, canManage])

  const run = async (fn: () => Promise<ProjectTeam[]>) => {
    setBusy(true)
    setError(null)
    try { setTeams(await fn()) }
    catch (e) { setError(e instanceof Error ? e.message : 'Update failed') }
    finally { setBusy(false) }
  }

  const toggleOpenReassign = async (on: boolean) => {
    setBusy(true)
    setError(null)
    try {
      const p = await api.updateProjectSettings(projectId, { open_reassign: on })
      setOpenReassign(p.open_reassign ?? on)
      onProjectUpdated?.(p)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Update failed')
    } finally {
      setBusy(false)
    }
  }

  const unlinked = orgTeams.filter(t => !teams.some(pt => pt.team_id === t.id))

  return (
    <>
      {loading ? (
        <div className="flex justify-center py-6"><Loader2 size={18} className="text-primary animate-spin" /></div>
      ) : (
        <div className="space-y-5">
          <div>
            <label className={labelCls}>Linked teams</label>
            <div className="rounded-xl border border-line overflow-hidden">
              {teams.length === 0 && <div className="px-4 py-4 text-xs text-ink-3 text-center">No teams linked</div>}
              {teams.map((t, i) => (
                <div key={t.team_id} className={`flex items-center gap-3 px-4 py-2.5 ${i > 0 ? 'border-t border-line' : ''}`}>
                  <Users size={13} className="text-ink-3 flex-shrink-0" />
                  <span className="text-sm text-ink flex-1 truncate">{t.team_name}</span>
                  {canManage ? (
                    <>
                      <select
                        value={t.role}
                        disabled={busy}
                        onChange={e => run(() => api.setProjectTeam(projectId, t.team_id, e.target.value as ProjectTeamRole))}
                        className="bg-bg border border-line rounded-lg px-2 py-1 text-xs text-ink focus:outline-none focus:border-primary cursor-pointer disabled:opacity-50"
                      >
                        <option value="owner">Owner</option>
                        <option value="member">Member</option>
                      </select>
                      <button
                        onClick={() => run(() => api.removeProjectTeam(projectId, t.team_id))}
                        disabled={busy}
                        className="p-1 text-ink-2 hover:text-danger-ink transition-colors cursor-pointer disabled:opacity-50"
                        title="Remove team from project"
                      >
                        <Trash2 size={13} />
                      </button>
                    </>
                  ) : (
                    <span className="text-xs text-ink-2 capitalize">{t.role}</span>
                  )}
                </div>
              ))}
            </div>
          </div>

          {canManage && unlinked.length > 0 && (
            <div>
              <label className={labelCls}>Add team</label>
              <div className="flex gap-2">
                <select value={addTeamId} onChange={e => setAddTeamId(e.target.value)} className={inputCls}>
                  <option value="">Select a team…</option>
                  {unlinked.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
                </select>
                <select value={addRole} onChange={e => setAddRole(e.target.value as ProjectTeamRole)} className={`${inputCls.replace('w-full', 'w-32')} flex-shrink-0`}>
                  <option value="member">Member</option>
                  <option value="owner">Owner</option>
                </select>
                <button
                  onClick={() => run(async () => {
                    const r = await api.setProjectTeam(projectId, Number(addTeamId), addRole)
                    setAddTeamId('')
                    return r
                  })}
                  disabled={busy || !addTeamId}
                  className="amp-btn amp-btn-primary flex-shrink-0"
                >
                  Add
                </button>
              </div>
            </div>
          )}

          <div>
            <label className={labelCls}>Ticket reassignment</label>
            <label className={`flex items-start gap-3 rounded-xl border border-line px-4 py-3 ${canManage ? 'cursor-pointer' : ''}`}>
              <input
                type="checkbox"
                checked={openReassign}
                disabled={!canManage || busy}
                onChange={e => toggleOpenReassign(e.target.checked)}
                className="mt-0.5 accent-primary cursor-pointer disabled:cursor-default"
              />
              <span className="text-xs text-ink-2 leading-relaxed">
                <span className="block text-sm text-ink font-medium">Anyone on the project can reassign tickets</span>
                Members can take over, hand on, or release a ticket that isn't in progress. When off, only
                its owner, a team lead, or a project owner can. In-progress tickets always stay with their owner
                unless a lead or project owner steps in.
              </span>
            </label>
          </div>

          {!canManage && (
            <p className="text-xs text-ink-3">Only project owners and org admins can change team access or settings.</p>
          )}
          {busy && <Loader2 size={14} className="text-ink-2 animate-spin" />}
          {error && <p className="text-xs text-danger-ink">{error}</p>}
        </div>
      )}
    </>
  )
}
