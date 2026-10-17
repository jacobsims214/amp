import { useEffect, useMemo, useState } from 'react'
import { AlertTriangle, Building2, Loader2 } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { inputCls, labelCls } from './CrudModal'
import type { Org, Project, ProjectTeam, Team } from '../types'

const isOrgAdmin = (o?: Org) => o?.my_role === 'owner' || o?.my_role === 'admin'

// The project's org, and moving it. Global admins can place any project,
// including one from before orgs existed (no org: nobody else can reach it).
// Org owners and admins can move a project between orgs they administer.
// Moving unlinks teams from the old org; an owner team from the new org can
// be linked in the same step.
export function ProjectOrgPicker({ project, org, onMoved }: { project: Project; org: Org | null; onMoved: (p: Project) => void }) {
  const { me, isAdmin } = useAuth()
  const myOrgs = useMemo(() => me?.orgs ?? [], [me])
  const canMove = isAdmin || (project.org_id != null && isOrgAdmin(myOrgs.find(o => o.id === project.org_id)))
  const [open, setOpen] = useState(false)
  const [targets, setTargets] = useState<Org[]>([])
  const [orgId, setOrgId] = useState<number | ''>('')
  const [teams, setTeams] = useState<Team[]>([])
  const [ownerTeam, setOwnerTeam] = useState<number | ''>('')
  const [linked, setLinked] = useState<ProjectTeam[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Orgs it can go to: every org for a global admin, else the ones you administer.
  useEffect(() => {
    if (!open) return
    const pick = (all: Org[]) => setTargets(all.filter(o => o.id !== project.org_id && (isAdmin || isOrgAdmin(o))))
    if (isAdmin) api.listOrgs().then(pick).catch(() => pick(myOrgs))
    else pick(myOrgs)
    api.listProjectTeams(project.id).then(setLinked).catch(() => setLinked([]))
  }, [open, isAdmin, myOrgs, project.id, project.org_id])

  useEffect(() => {
    if (orgId === '') return
    let cancelled = false
    api.listOrgTeams(orgId).then(ts => { if (!cancelled) { setTeams(ts); setOwnerTeam(ts.length === 1 ? ts[0].id : '') } })
      .catch(() => { if (!cancelled) setTeams([]) })
    return () => { cancelled = true }
  }, [orgId])

  const pickOrg = (v: string) => { setOrgId(v ? Number(v) : ''); setTeams([]); setOwnerTeam('') }
  const close = () => { setOpen(false); setOrgId(''); setTeams([]); setOwnerTeam(''); setError(null) }
  const move = async () => {
    if (orgId === '') return
    setBusy(true); setError(null)
    try {
      const r = await api.setProjectOrg(project.id, orgId, ownerTeam === '' ? undefined : ownerTeam)
      onMoved(r.project)
      close()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not move the project')
    } finally {
      setBusy(false)
    }
  }

  // Every current link is to the old org's teams, so a move unlinks them all.
  const unlinking = orgId === '' ? [] : linked
  const target = targets.find(t => t.id === orgId)

  return (
    <div className="grid gap-3">
      <div className="flex items-center gap-2.5 flex-wrap text-[13px]">
        <Building2 size={14} className="text-ink-3" />
        {project.org_id != null
          ? <span className="text-ink">{org?.name ?? `Org #${project.org_id}`}</span>
          : <span className="text-ink-2">No org</span>}
        {canMove && !open && (
          <button type="button" className="amp-btn amp-btn-sm" onClick={() => setOpen(true)}>
            {project.org_id != null ? 'Move to another org' : 'Put in an org'}
          </button>
        )}
      </div>
      {project.org_id == null && (
        <p className="m-0 flex items-start gap-2 text-[12.5px] text-warning-ink leading-relaxed">
          <AlertTriangle size={13} className="mt-0.5 flex-shrink-0" />
          This project isn't in an org, so no team can be linked to it and only global admins can see it. Put it in an org and pick an owner team to give people access.
        </p>
      )}
      {open && (
        <div className="grid gap-3 border border-line px-4 py-3.5 bg-surface-2">
          {targets.length === 0 ? (
            <p className="m-0 text-[12.5px] text-ink-2">There's no other org you can move it to. You need to be an owner or admin of the org it goes to.</p>
          ) : (
            <>
              <div className="grid gap-3 sm:grid-cols-2">
                <div>
                  <label className={labelCls} htmlFor="project-org-target">Org</label>
                  <select id="project-org-target" className={inputCls} value={orgId} onChange={e => pickOrg(e.target.value)}>
                    <option value="">Choose an org…</option>
                    {targets.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
                  </select>
                </div>
                <div>
                  <label className={labelCls} htmlFor="project-org-owner">Owner team</label>
                  <select id="project-org-owner" className={inputCls} value={ownerTeam} disabled={orgId === ''}
                    onChange={e => setOwnerTeam(e.target.value ? Number(e.target.value) : '')}>
                    <option value="">{orgId === '' ? 'Choose an org first' : teams.length ? 'None for now' : 'This org has no teams'}</option>
                    {teams.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
                  </select>
                </div>
              </div>
              {orgId !== '' && (
                <p className="m-0 text-[12px] text-ink-2 leading-relaxed">
                  {unlinking.length > 0
                    ? <>These teams are in the old org and will be unlinked: <span className="text-ink">{unlinking.map(t => t.team_name).join(', ')}</span>. </>
                    : null}
                  {ownerTeam === ''
                    ? <>Without an owner team, only {target?.name ?? 'the org'}'s owners and admins can reach the project until you link one in Team access.</>
                    : <>{teams.find(t => t.id === ownerTeam)?.name}'s members will manage the project.</>}
                </p>
              )}
            </>
          )}
          {error && <p className="m-0 text-[12.5px] text-danger-ink">{error}</p>}
          <div className="flex gap-2">
            {targets.length > 0 && (
              <button type="button" className="amp-btn amp-btn-primary amp-btn-sm" disabled={busy || orgId === ''} onClick={move}>
                {busy && <Loader2 size={11} className="animate-spin" />}
                {unlinking.length > 0 ? `Move and unlink ${unlinking.length} team${unlinking.length === 1 ? '' : 's'}` : project.org_id != null ? 'Move project' : 'Put in this org'}
              </button>
            )}
            <button type="button" className="amp-btn amp-btn-ghost amp-btn-sm" onClick={close} disabled={busy}>Cancel</button>
          </div>
        </div>
      )}
    </div>
  )
}
