import { useEffect, useState } from 'react'
import { labelCls, inputCls } from './CrudModal'
import { api } from '../api/client'
import type { Org, Team } from '../types'

export interface ProjectHome {
  orgId: number | null
  teamId: number | null
}

interface Props {
  orgs: Org[]
  value: ProjectHome
  onChange: (home: ProjectHome) => void
}

// Org + owning-team pickers for creating or importing a project. The org
// select only appears when the user can choose between several orgs; the team
// select is optional (the server infers it when the choice is unambiguous and
// replies with the available choices otherwise).
export function ProjectHomeFields({ orgs, value, onChange }: Props) {
  const effectiveOrgId = value.orgId ?? (orgs.length === 1 ? orgs[0].id : null)
  const [teams, setTeams] = useState<{ orgId: number; teams: Team[] } | null>(null)

  useEffect(() => {
    if (effectiveOrgId == null) return
    let cancelled = false
    api.listOrgTeams(effectiveOrgId)
      .then(t => { if (!cancelled) setTeams({ orgId: effectiveOrgId, teams: t }) })
      .catch(() => { if (!cancelled) setTeams({ orgId: effectiveOrgId, teams: [] }) })
    return () => { cancelled = true }
  }, [effectiveOrgId])

  const orgTeams = teams && teams.orgId === effectiveOrgId ? teams.teams : []

  return (
    <>
      {orgs.length > 1 && (
        <div><label className={labelCls}>Organization</label>
          <select
            value={value.orgId ?? ''}
            onChange={e => onChange({ orgId: e.target.value ? Number(e.target.value) : null, teamId: null })}
            className={inputCls}
          >
            <option value="">Select an organization…</option>
            {orgs.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
          </select></div>
      )}
      {orgTeams.length > 0 && (
        <div><label className={labelCls}>Owning team</label>
          <select
            value={value.teamId ?? ''}
            onChange={e => onChange({ ...value, teamId: e.target.value ? Number(e.target.value) : null })}
            className={inputCls}
          >
            <option value="">{orgTeams.length === 1 ? orgTeams[0].name + ' (default)' : 'Let the server choose'}</option>
            {orgTeams.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
          </select></div>
      )}
    </>
  )
}
