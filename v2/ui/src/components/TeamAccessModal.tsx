import { useEffect, useState } from 'react'
import { Loader2, Trash2, Users } from 'lucide-react'
import { api } from '../api/client'
import { CrudModal, inputCls, labelCls } from './CrudModal'
import { canManageProject } from '../lib/people'
import type { Project, ProjectAccess, ProjectTeam, ProjectTeamRole, Team } from '../types'

interface Props {
  projectId: number
  myAccess: ProjectAccess | null
  /** called after a project setting changes */
  onProjectUpdated?: (project: Project) => void
  onClose: () => void
}

// Lists the teams linked to a project. Project owners (and org/global
// admins) can change each team's role, unlink it, or link another team from
// the project's org, and set who may reassign tickets; everyone else gets a
// read-only view.
export function TeamAccessModal({ projectId, myAccess, onProjectUpdated, onClose }: Props) {
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
    <CrudModal title="Team access" onClose={onClose}>
      {loading ? (
        <div className="flex justify-center py-6"><Loader2 size={18} className="text-[#6366F1] animate-spin" /></div>
      ) : (
        <div className="space-y-5">
          <div>
            <label className={labelCls}>Linked teams</label>
            <div className="rounded-xl border border-[#1E2C45] overflow-hidden">
              {teams.length === 0 && <div className="px-4 py-4 text-xs text-[#3D5068] text-center">No teams linked</div>}
              {teams.map((t, i) => (
                <div key={t.team_id} className={`flex items-center gap-3 px-4 py-2.5 ${i > 0 ? 'border-t border-[#1E2C45]' : ''}`}>
                  <Users size={13} className="text-[#3D5068] flex-shrink-0" />
                  <span className="text-sm text-[#DDE6F0] flex-1 truncate">{t.team_name}</span>
                  {canManage ? (
                    <>
                      <select
                        value={t.role}
                        disabled={busy}
                        onChange={e => run(() => api.setProjectTeam(projectId, t.team_id, e.target.value as ProjectTeamRole))}
                        className="bg-[#08101F] border border-[#1E2C45] rounded-lg px-2 py-1 text-xs text-[#DDE6F0] focus:outline-none focus:border-[#6366F1] cursor-pointer disabled:opacity-50"
                      >
                        <option value="owner">Owner</option>
                        <option value="member">Member</option>
                      </select>
                      <button
                        onClick={() => run(() => api.removeProjectTeam(projectId, t.team_id))}
                        disabled={busy}
                        className="p-1 text-[#7E91A8] hover:text-[#F87171] transition-colors cursor-pointer disabled:opacity-50"
                        title="Remove team from project"
                      >
                        <Trash2 size={13} />
                      </button>
                    </>
                  ) : (
                    <span className="text-xs text-[#7E91A8] capitalize">{t.role}</span>
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
                  className="px-3 py-2 bg-[#6366F1] hover:bg-[#4F46E5] disabled:opacity-50 disabled:cursor-not-allowed text-white text-xs font-semibold rounded-lg transition-all active:scale-[0.97] cursor-pointer flex-shrink-0"
                >
                  Add
                </button>
              </div>
            </div>
          )}

          <div>
            <label className={labelCls}>Ticket reassignment</label>
            <label className={`flex items-start gap-3 rounded-xl border border-[#1E2C45] px-4 py-3 ${canManage ? 'cursor-pointer' : ''}`}>
              <input
                type="checkbox"
                checked={openReassign}
                disabled={!canManage || busy}
                onChange={e => toggleOpenReassign(e.target.checked)}
                className="mt-0.5 accent-[#6366F1] cursor-pointer disabled:cursor-default"
              />
              <span className="text-xs text-[#7E91A8] leading-relaxed">
                <span className="block text-sm text-[#DDE6F0] font-medium">Anyone on the project can reassign tickets</span>
                Members can take over, hand on, or release a ticket that isn't in progress. When off, only
                its owner, a team lead, or a project owner can. In-progress tickets always stay with their owner
                unless a lead or project owner steps in.
              </span>
            </label>
          </div>

          {!canManage && (
            <p className="text-xs text-[#3D5068]">Only project owners and org admins can change team access or settings.</p>
          )}
          {busy && <Loader2 size={14} className="text-[#7E91A8] animate-spin" />}
          {error && <p className="text-xs text-[#F87171]">{error}</p>}
        </div>
      )}
    </CrudModal>
  )
}
