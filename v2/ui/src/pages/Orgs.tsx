import { useState, useEffect, useCallback } from 'react'
import { Link } from 'react-router-dom'
import {
  ArrowLeft, Loader2, Building2, Plus, Trash2, Pencil, UserPlus, Users, Mail, ChevronDown, ChevronRight, X, Check,
} from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { ConfirmDeleteModal } from '../components/ConfirmDeleteModal'
import { personName } from '../lib/people'
import type { AddMemberResult, Invite, Member, Org, OrgRole, Team, TeamRole } from '../types'

// Orgs → Teams → Users administration. Controls are hidden or disabled based
// on the caller's org role (my_role) and global admin status, but the API
// stays authoritative: every action surfaces its error inline.

const fieldCls =
  'px-3 py-2 rounded-lg text-sm bg-[#0D1726] border border-[#1E2C45] text-[#DDE6F0] placeholder-[#3D5068] outline-none focus:border-[#6366F1]'
const smallSelectCls =
  'bg-[#08101F] border border-[#1E2C45] rounded-lg px-2 py-1 text-xs text-[#DDE6F0] focus:outline-none focus:border-[#6366F1] cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed'
const primaryBtnCls =
  'flex items-center gap-1.5 px-3 py-2 rounded-lg text-sm font-semibold text-white disabled:opacity-50 cursor-pointer'
const iconBtnCls = 'text-[#7E91A8] hover:text-[#F87171] disabled:opacity-50 cursor-pointer transition-colors'

const errMsg = (e: unknown, fallback: string) => e instanceof Error ? e.message : fallback

function addedMessage(email: string, r: AddMemberResult) {
  return r.invited
    ? `Invited ${email} — they'll be added when they first sign in.`
    : `Added ${email}.`
}

function RoleBadge({ role }: { role: string }) {
  return (
    <span
      className="text-[10px] font-semibold px-1.5 py-0.5 rounded-md capitalize"
      style={{ background: 'rgba(99,102,241,0.12)', color: '#818CF8', border: '1px solid rgba(99,102,241,0.22)' }}
    >
      {role || 'none'}
    </span>
  )
}

function Notice({ error, info }: { error?: string | null; info?: string | null }) {
  if (error) return <p className="text-xs text-[#F87171] mt-2">{error}</p>
  if (info) return <p className="text-xs text-[#10B981] mt-2">{info}</p>
  return null
}

export function Orgs() {
  const { me, loading: authLoading, isAdmin } = useAuth()
  const [orgs, setOrgs] = useState<Org[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [selectedId, setSelectedId] = useState<number | null>(null)

  const [newOrg, setNewOrg] = useState({ name: '', slug: '', owner: '' })
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)
  const [createInfo, setCreateInfo] = useState<string | null>(null)

  const reload = useCallback(() => {
    return api.listOrgs()
      .then(list => {
        setOrgs(list)
        setError(null)
        setSelectedId(prev => prev !== null && list.some(o => o.id === prev) ? prev : list[0]?.id ?? null)
      })
      .catch(e => setError(errMsg(e, 'Failed to load orgs')))
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    if (!authLoading) reload()
  }, [authLoading, reload])

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    setCreating(true)
    setCreateError(null)
    setCreateInfo(null)
    try {
      const r = await api.createOrg({
        name: newOrg.name.trim(),
        ...(newOrg.slug.trim() ? { slug: newOrg.slug.trim() } : {}),
        ...(newOrg.owner.trim() ? { owner: newOrg.owner.trim().toLowerCase() } : {}),
      })
      setNewOrg({ name: '', slug: '', owner: '' })
      setCreateInfo(r.owner_invite
        ? `Created ${r.org.name}; ${r.owner_invite.email} will become owner when they first sign in.`
        : `Created ${r.org.name}.`)
      await reload()
      setSelectedId(r.org.id)
    } catch (e) {
      setCreateError(errMsg(e, 'Create failed'))
    } finally {
      setCreating(false)
    }
  }

  const selected = orgs.find(o => o.id === selectedId) ?? null

  if (authLoading) {
    return (
      <div className="min-h-screen flex items-center justify-center" style={{ background: '#08101F' }}>
        <Loader2 size={22} className="text-[#6366F1] animate-spin" />
      </div>
    )
  }

  return (
    <div className="min-h-screen" style={{ background: '#08101F' }}>
      <header style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45' }}>
        <div className="max-w-5xl mx-auto px-6 py-4 flex items-center gap-3">
          <Link to="/" className="text-[#3D5068] hover:text-[#DDE6F0] transition-colors">
            <ArrowLeft size={16} />
          </Link>
          <Building2 size={16} className="text-[#818CF8]" />
          <h1 className="text-base font-bold text-[#DDE6F0]">Organizations &amp; Teams</h1>
          {me && <span className="text-xs text-[#3D5068] ml-auto">{me.email}</span>}
        </div>
      </header>

      <main className="max-w-5xl mx-auto px-6 py-10 space-y-10">
        {error && (
          <div className="rounded-xl border border-[#EF4444]/20 bg-[#EF4444]/8 px-4 py-3 text-sm text-[#F87171]">
            {error}
          </div>
        )}

        {isAdmin && (
          <section>
            <h2 className="text-sm font-bold text-[#DDE6F0] mb-3">New organization</h2>
            <form onSubmit={handleCreate} className="flex flex-wrap items-start gap-2">
              <input
                required placeholder="Name" value={newOrg.name}
                onChange={e => setNewOrg(f => ({ ...f, name: e.target.value }))}
                className={fieldCls}
              />
              <input
                placeholder="Slug (optional)" value={newOrg.slug}
                onChange={e => setNewOrg(f => ({ ...f, slug: e.target.value.toLowerCase() }))}
                className={`${fieldCls} font-mono`}
              />
              <input
                type="email" placeholder="Owner email (optional, defaults to you)" value={newOrg.owner}
                onChange={e => setNewOrg(f => ({ ...f, owner: e.target.value }))}
                className={`${fieldCls} min-w-[280px]`}
              />
              <button type="submit" disabled={creating || !newOrg.name.trim()} className={primaryBtnCls} style={{ background: '#6366F1' }}>
                {creating ? <Loader2 size={14} className="animate-spin" /> : <Plus size={14} />}
                Create
              </button>
            </form>
            <Notice error={createError} info={createInfo} />
          </section>
        )}

        <section className="grid grid-cols-1 md:grid-cols-[220px_1fr] gap-6">
          {/* Org list */}
          <div>
            <h2 className="text-sm font-bold text-[#DDE6F0] mb-3">{isAdmin ? 'All organizations' : 'My organizations'}</h2>
            {loading ? (
              <Loader2 size={18} className="text-[#6366F1] animate-spin" />
            ) : orgs.length === 0 ? (
              <p className="text-xs text-[#3D5068]">
                {isAdmin ? 'No organizations yet.' : "You're not a member of any organization yet."}
              </p>
            ) : (
              <div className="space-y-1">
                {orgs.map(o => {
                  const active = o.id === selectedId
                  return (
                    <button
                      key={o.id}
                      onClick={() => setSelectedId(o.id)}
                      className="w-full text-left px-3 py-2 rounded-lg transition-all cursor-pointer"
                      style={{
                        background: active ? '#172540' : 'transparent',
                        border: active ? '1px solid #283A57' : '1px solid transparent',
                      }}
                    >
                      <div className={`text-sm font-semibold truncate ${active ? 'text-[#DDE6F0]' : 'text-[#7E91A8]'}`}>{o.name}</div>
                      <div className="flex items-center gap-1.5 mt-0.5">
                        <span className="text-[10px] text-[#3D5068] font-mono truncate">{o.slug}</span>
                        {o.my_role && <RoleBadge role={o.my_role} />}
                      </div>
                    </button>
                  )
                })}
              </div>
            )}
          </div>

          {/* Selected org */}
          <div className="min-w-0">
            {selected && (
              <OrgDetail
                key={selected.id}
                org={selected}
                isGlobalAdmin={isAdmin}
                meId={me?.user_id}
                onChanged={reload}
              />
            )}
          </div>
        </section>
      </main>
    </div>
  )
}

/* ─── Org detail ─────────────────────────────────────────────────────────── */

type Tab = 'members' | 'teams' | 'invites'

function OrgDetail({ org, isGlobalAdmin, meId, onChanged }: {
  org: Org
  isGlobalAdmin: boolean
  meId?: number
  onChanged: () => Promise<void>
}) {
  const canManage = isGlobalAdmin || org.my_role === 'owner' || org.my_role === 'admin'
  const canGrantOwner = isGlobalAdmin || org.my_role === 'owner'
  const [tab, setTab] = useState<Tab>('members')
  const [renaming, setRenaming] = useState(false)
  const [name, setName] = useState(org.name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const rename = async () => {
    setBusy(true); setError(null)
    try { await api.renameOrg(org.id, name.trim()); setRenaming(false); await onChanged() }
    catch (e) { setError(errMsg(e, 'Rename failed')) }
    finally { setBusy(false) }
  }

  const remove = async () => {
    setBusy(true); setError(null)
    try { await api.deleteOrg(org.id); setConfirmDelete(false); await onChanged() }
    catch (e) { setError(errMsg(e, 'Delete failed')) }
    finally { setBusy(false) }
  }

  return (
    <div className="rounded-2xl p-5" style={{ background: '#0D1726', border: '1px solid #1E2C45' }}>
      <div className="flex items-center gap-3 mb-4">
        {renaming ? (
          <div className="flex items-center gap-2 flex-1">
            <input autoFocus value={name} onChange={e => setName(e.target.value)} className={`${fieldCls} flex-1`} />
            <button onClick={rename} disabled={busy || !name.trim()} className="p-1.5 text-[#10B981] hover:bg-[#172540] rounded-lg cursor-pointer disabled:opacity-50" title="Save">
              <Check size={14} />
            </button>
            <button onClick={() => { setRenaming(false); setName(org.name) }} className="p-1.5 text-[#7E91A8] hover:bg-[#172540] rounded-lg cursor-pointer" title="Cancel">
              <X size={14} />
            </button>
          </div>
        ) : (
          <>
            <div className="min-w-0 flex-1">
              <h2 className="text-base font-bold text-[#DDE6F0] truncate">{org.name}</h2>
              <p className="text-xs text-[#3D5068] font-mono">{org.slug}</p>
            </div>
            {canManage && (
              <button onClick={() => setRenaming(true)} className="p-1.5 text-[#7E91A8] hover:text-[#DDE6F0] hover:bg-[#172540] rounded-lg cursor-pointer" title="Rename org">
                <Pencil size={13} />
              </button>
            )}
            {canGrantOwner && (
              <button onClick={() => { setError(null); setConfirmDelete(true) }} className="p-1.5 text-[#7E91A8] hover:text-[#F87171] hover:bg-[#EF4444]/10 rounded-lg cursor-pointer" title="Delete org">
                <Trash2 size={13} />
              </button>
            )}
          </>
        )}
      </div>
      {error && !confirmDelete && <p className="text-xs text-[#F87171] mb-3">{error}</p>}

      <div className="flex border-b border-[#1E2C45] mb-5">
        {(['members', 'teams', 'invites'] as const).map(t => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`px-4 py-2.5 text-xs font-semibold capitalize transition-all border-b-2 -mb-px cursor-pointer ${
              tab === t ? 'border-[#6366F1] text-[#818CF8]' : 'border-transparent text-[#7E91A8] hover:text-[#DDE6F0]'
            }`}
          >
            {t === 'invites' ? 'Pending invites' : t}
          </button>
        ))}
      </div>

      {tab === 'members' && <OrgMembers org={org} canManage={canManage} canGrantOwner={canGrantOwner} meId={meId} />}
      {tab === 'teams' && <OrgTeams org={org} canManage={canManage} meId={meId} />}
      {tab === 'invites' && <OrgInvites org={org} canManage={canManage} />}

      {confirmDelete && (
        <ConfirmDeleteModal
          title={`Delete "${org.name}"`}
          description="This permanently deletes the organization, its teams and memberships."
          onClose={() => setConfirmDelete(false)}
          onConfirm={remove}
          deleting={busy}
          error={error}
        />
      )}
    </div>
  )
}

/* ─── Members ────────────────────────────────────────────────────────────── */

function OrgMembers({ org, canManage, canGrantOwner, meId }: { org: Org; canManage: boolean; canGrantOwner: boolean; meId?: number }) {
  const [members, setMembers] = useState<Member[]>([])
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [info, setInfo] = useState<string | null>(null)
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<OrgRole>('member')
  const [adding, setAdding] = useState(false)

  const reload = useCallback(() => {
    return api.listOrgMembers(org.id)
      .then(setMembers)
      .catch(e => setError(errMsg(e, 'Failed to load members')))
      .finally(() => setLoading(false))
  }, [org.id])

  useEffect(() => { reload() }, [reload])

  const act = async (userId: number, fn: () => Promise<unknown>) => {
    setBusyId(userId); setError(null); setInfo(null)
    try { await fn(); await reload() }
    catch (e) { setError(errMsg(e, 'Update failed')) }
    finally { setBusyId(null) }
  }

  async function handleAdd(e: React.FormEvent) {
    e.preventDefault()
    const who = email.trim().toLowerCase()
    setAdding(true); setError(null); setInfo(null)
    try {
      const r = await api.addOrgMember(org.id, who, role)
      setInfo(addedMessage(who, r))
      setEmail('')
      await reload()
    } catch (e) {
      setError(errMsg(e, 'Add failed'))
    } finally {
      setAdding(false)
    }
  }

  return (
    <div>
      {canManage && (
        <form onSubmit={handleAdd} className="flex flex-wrap items-start gap-2 mb-4">
          <input type="email" required placeholder="email@example.com" value={email} onChange={e => setEmail(e.target.value)} className={`${fieldCls} flex-1 min-w-[220px]`} />
          <select value={role} onChange={e => setRole(e.target.value as OrgRole)} className={fieldCls}>
            <option value="member">Member</option>
            <option value="admin">Admin</option>
            {canGrantOwner && <option value="owner">Owner</option>}
          </select>
          <button type="submit" disabled={adding} className={primaryBtnCls} style={{ background: '#6366F1' }}>
            {adding ? <Loader2 size={14} className="animate-spin" /> : <UserPlus size={14} />}
            Add
          </button>
        </form>
      )}
      <Notice error={error} info={info} />

      {loading ? (
        <Loader2 size={18} className="text-[#6366F1] animate-spin mt-3" />
      ) : (
        <div className="rounded-xl border border-[#1E2C45] overflow-hidden mt-3">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-[#3D5068] text-xs" style={{ background: '#08101F' }}>
                <th className="px-4 py-2 font-semibold">Email</th>
                <th className="px-4 py-2 font-semibold">Name</th>
                <th className="px-4 py-2 font-semibold w-32">Role</th>
                {canManage && <th className="px-4 py-2 font-semibold w-12" />}
              </tr>
            </thead>
            <tbody>
              {members.map(m => {
                // Only owners may grant or take away the owner role.
                const editable = canManage && (canGrantOwner || m.role !== 'owner')
                return (
                  <tr key={m.user_id} className="border-t border-[#1E2C45]">
                    <td className="px-4 py-2 text-[#DDE6F0]">{m.email}{m.user_id === meId && <span className="text-[#3D5068]"> (you)</span>}</td>
                    <td className="px-4 py-2 text-[#7E91A8]">{m.display_name || '—'}</td>
                    <td className="px-4 py-2">
                      {editable ? (
                        <select
                          value={m.role}
                          disabled={busyId === m.user_id}
                          onChange={e => act(m.user_id, () => api.setOrgMemberRole(org.id, m.user_id, e.target.value as OrgRole))}
                          className={smallSelectCls}
                        >
                          <option value="member">Member</option>
                          <option value="admin">Admin</option>
                          {canGrantOwner && <option value="owner">Owner</option>}
                        </select>
                      ) : (
                        <RoleBadge role={m.role} />
                      )}
                    </td>
                    {canManage && (
                      <td className="px-4 py-2">
                        {editable && (
                          <button
                            onClick={() => { if (confirm(`Remove ${m.email} from ${org.name}?`)) act(m.user_id, () => api.removeOrgMember(org.id, m.user_id)) }}
                            disabled={busyId === m.user_id}
                            title="Remove from org"
                            className={iconBtnCls}
                          >
                            <Trash2 size={14} />
                          </button>
                        )}
                      </td>
                    )}
                  </tr>
                )
              })}
              {members.length === 0 && (
                <tr><td colSpan={4} className="px-4 py-6 text-center text-[#3D5068] text-sm">No members</td></tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

/* ─── Teams ──────────────────────────────────────────────────────────────── */

function OrgTeams({ org, canManage, meId }: { org: Org; canManage: boolean; meId?: number }) {
  const [teams, setTeams] = useState<Team[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [newName, setNewName] = useState('')
  const [creating, setCreating] = useState(false)
  const [expanded, setExpanded] = useState<number | null>(null)

  const reload = useCallback(() => {
    return api.listOrgTeams(org.id)
      .then(setTeams)
      .catch(e => setError(errMsg(e, 'Failed to load teams')))
      .finally(() => setLoading(false))
  }, [org.id])

  useEffect(() => { reload() }, [reload])

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    setCreating(true); setError(null)
    try {
      const t = await api.createTeam(org.id, newName.trim())
      setNewName('')
      await reload()
      setExpanded(t.id)
    } catch (e) {
      setError(errMsg(e, 'Create failed'))
    } finally {
      setCreating(false)
    }
  }

  return (
    <div>
      {canManage && (
        <form onSubmit={handleCreate} className="flex items-start gap-2 mb-4">
          <input required placeholder="New team name" value={newName} onChange={e => setNewName(e.target.value)} className={`${fieldCls} flex-1`} />
          <button type="submit" disabled={creating || !newName.trim()} className={primaryBtnCls} style={{ background: '#6366F1' }}>
            {creating ? <Loader2 size={14} className="animate-spin" /> : <Plus size={14} />}
            Create team
          </button>
        </form>
      )}
      <Notice error={error} />

      {loading ? (
        <Loader2 size={18} className="text-[#6366F1] animate-spin mt-3" />
      ) : (
        <div className="rounded-xl border border-[#1E2C45] overflow-hidden mt-3">
          {teams.length === 0 && <div className="px-4 py-6 text-center text-[#3D5068] text-sm">No teams yet</div>}
          {teams.map((t, i) => (
            <TeamRow
              key={t.id}
              team={t}
              first={i === 0}
              canManageOrg={canManage}
              meId={meId}
              expanded={expanded === t.id}
              onToggle={() => setExpanded(expanded === t.id ? null : t.id)}
              onChanged={reload}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function TeamRow({ team, first, canManageOrg, meId, expanded, onToggle, onChanged }: {
  team: Team
  first: boolean
  canManageOrg: boolean
  meId?: number
  expanded: boolean
  onToggle: () => void
  onChanged: () => Promise<void>
}) {
  const [renaming, setRenaming] = useState(false)
  const [name, setName] = useState(team.name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const rename = async () => {
    setBusy(true); setError(null)
    try { await api.renameTeam(team.id, name.trim()); setRenaming(false); await onChanged() }
    catch (e) { setError(errMsg(e, 'Rename failed')) }
    finally { setBusy(false) }
  }

  const remove = async () => {
    setBusy(true); setError(null)
    try { await api.deleteTeam(team.id); setConfirmDelete(false); await onChanged() }
    catch (e) { setError(errMsg(e, 'Delete failed')) }
    finally { setBusy(false) }
  }

  return (
    <div className={first ? '' : 'border-t border-[#1E2C45]'}>
      <div className="flex items-center gap-2 px-4 py-2.5">
        <button onClick={onToggle} className="flex items-center gap-2 flex-1 min-w-0 text-left cursor-pointer">
          <span className="text-[#3D5068]">{expanded ? <ChevronDown size={13} /> : <ChevronRight size={13} />}</span>
          <Users size={13} className="text-[#3D5068] flex-shrink-0" />
          {!renaming && <span className="text-sm text-[#DDE6F0] truncate">{team.name}</span>}
        </button>
        {renaming ? (
          <div className="flex items-center gap-1.5">
            <input autoFocus value={name} onChange={e => setName(e.target.value)} className={`${fieldCls} py-1`} />
            <button onClick={rename} disabled={busy || !name.trim()} className="p-1 text-[#10B981] cursor-pointer disabled:opacity-50" title="Save"><Check size={13} /></button>
            <button onClick={() => { setRenaming(false); setName(team.name) }} className="p-1 text-[#7E91A8] cursor-pointer" title="Cancel"><X size={13} /></button>
          </div>
        ) : canManageOrg && (
          <>
            <button onClick={() => setRenaming(true)} className="p-1 text-[#7E91A8] hover:text-[#DDE6F0] cursor-pointer" title="Rename team"><Pencil size={12} /></button>
            <button onClick={() => { setError(null); setConfirmDelete(true) }} className={`p-1 ${iconBtnCls}`} title="Delete team"><Trash2 size={13} /></button>
          </>
        )}
      </div>
      {error && !confirmDelete && <p className="text-xs text-[#F87171] px-4 pb-2">{error}</p>}
      {expanded && <TeamMembers teamId={team.id} canManageOrg={canManageOrg} meId={meId} />}
      {confirmDelete && (
        <ConfirmDeleteModal
          title={`Delete team "${team.name}"`}
          description="Projects owned only by this team will lose their owning team."
          onClose={() => setConfirmDelete(false)}
          onConfirm={remove}
          deleting={busy}
          error={error}
        />
      )}
    </div>
  )
}

function TeamMembers({ teamId, canManageOrg, meId }: { teamId: number; canManageOrg: boolean; meId?: number }) {
  const [members, setMembers] = useState<Member[]>([])
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [info, setInfo] = useState<string | null>(null)
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<TeamRole>('member')
  const [adding, setAdding] = useState(false)

  const reload = useCallback(() => {
    return api.getTeam(teamId)
      .then(r => setMembers(r.members))
      .catch(e => setError(errMsg(e, 'Failed to load team')))
      .finally(() => setLoading(false))
  }, [teamId])

  useEffect(() => { reload() }, [reload])

  // Team leads manage their own team's members; org admins manage any team.
  const isLead = meId != null && members.some(m => m.user_id === meId && m.role === 'lead')
  const canManage = canManageOrg || isLead

  const act = async (userId: number, fn: () => Promise<unknown>) => {
    setBusyId(userId); setError(null); setInfo(null)
    try { await fn(); await reload() }
    catch (e) { setError(errMsg(e, 'Update failed')) }
    finally { setBusyId(null) }
  }

  async function handleAdd(e: React.FormEvent) {
    e.preventDefault()
    const who = email.trim().toLowerCase()
    setAdding(true); setError(null); setInfo(null)
    try {
      const r = await api.addTeamMember(teamId, who, role)
      setInfo(addedMessage(who, r))
      setEmail('')
      await reload()
    } catch (e) {
      setError(errMsg(e, 'Add failed'))
    } finally {
      setAdding(false)
    }
  }

  return (
    <div className="px-4 pb-4 pl-10 space-y-3" style={{ background: 'rgba(8,16,31,0.4)' }}>
      {loading ? (
        <Loader2 size={16} className="text-[#6366F1] animate-spin" />
      ) : (
        <>
          <div className="space-y-1 pt-2">
            {members.length === 0 && <p className="text-xs text-[#3D5068]">No members</p>}
            {members.map(m => (
              <div key={m.user_id} className="flex items-center gap-3 py-1">
                <span className="text-sm text-[#DDE6F0] flex-1 truncate" title={m.email}>
                  {personName(m)} <span className="text-xs text-[#3D5068]">{m.email}{m.user_id === meId ? ' (you)' : ''}</span>
                </span>
                {canManage ? (
                  <>
                    <select
                      value={m.role}
                      disabled={busyId === m.user_id}
                      onChange={e => act(m.user_id, () => api.setTeamMemberRole(teamId, m.user_id, e.target.value as TeamRole))}
                      className={smallSelectCls}
                    >
                      <option value="member">Member</option>
                      <option value="lead">Lead</option>
                    </select>
                    <button
                      onClick={() => act(m.user_id, () => api.removeTeamMember(teamId, m.user_id))}
                      disabled={busyId === m.user_id}
                      title="Remove from team"
                      className={iconBtnCls}
                    >
                      <Trash2 size={13} />
                    </button>
                  </>
                ) : (
                  <RoleBadge role={m.role} />
                )}
              </div>
            ))}
          </div>
          {canManage && (
            <form onSubmit={handleAdd} className="flex flex-wrap items-start gap-2">
              <input type="email" required placeholder="email@example.com" value={email} onChange={e => setEmail(e.target.value)} className={`${fieldCls} py-1.5 flex-1 min-w-[200px]`} />
              <select value={role} onChange={e => setRole(e.target.value as TeamRole)} className={`${fieldCls} py-1.5`}>
                <option value="member">Member</option>
                <option value="lead">Lead</option>
              </select>
              <button type="submit" disabled={adding} className={`${primaryBtnCls} py-1.5`} style={{ background: '#6366F1' }}>
                {adding ? <Loader2 size={13} className="animate-spin" /> : <UserPlus size={13} />}
                Add
              </button>
            </form>
          )}
          <Notice error={error} info={info} />
        </>
      )}
    </div>
  )
}

/* ─── Invites ────────────────────────────────────────────────────────────── */

function OrgInvites({ org, canManage }: { org: Org; canManage: boolean }) {
  const [invites, setInvites] = useState<Invite[]>([])
  const [teams, setTeams] = useState<Team[]>([])
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)

  const reload = useCallback(() => {
    return Promise.all([api.listOrgInvites(org.id), api.listOrgTeams(org.id).catch(() => [] as Team[])])
      .then(([inv, t]) => { setInvites(inv); setTeams(t) })
      .catch(e => setError(errMsg(e, 'Failed to load invites')))
      .finally(() => setLoading(false))
  }, [org.id])

  useEffect(() => { reload() }, [reload])

  const revoke = async (id: number) => {
    setBusyId(id); setError(null)
    try { await api.revokeInvite(org.id, id); await reload() }
    catch (e) { setError(errMsg(e, 'Revoke failed')) }
    finally { setBusyId(null) }
  }

  const teamName = (id?: number | null) => id == null ? null : teams.find(t => t.id === id)?.name ?? `Team #${id}`

  return (
    <div>
      <p className="text-xs text-[#3D5068] mb-3">
        Invites are created when you add someone by an email that hasn't signed in yet. They're accepted automatically on first sign-in.
      </p>
      <Notice error={error} />
      {loading ? (
        <Loader2 size={18} className="text-[#6366F1] animate-spin mt-3" />
      ) : (
        <div className="rounded-xl border border-[#1E2C45] overflow-hidden mt-3">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-[#3D5068] text-xs" style={{ background: '#08101F' }}>
                <th className="px-4 py-2 font-semibold">Email</th>
                <th className="px-4 py-2 font-semibold">Org role</th>
                <th className="px-4 py-2 font-semibold">Team</th>
                <th className="px-4 py-2 font-semibold">Sent</th>
                {canManage && <th className="px-4 py-2 font-semibold w-12" />}
              </tr>
            </thead>
            <tbody>
              {invites.map(inv => (
                <tr key={inv.id} className="border-t border-[#1E2C45]">
                  <td className="px-4 py-2 text-[#DDE6F0]">
                    <span className="flex items-center gap-1.5"><Mail size={12} className="text-[#3D5068]" />{inv.email}</span>
                  </td>
                  <td className="px-4 py-2"><RoleBadge role={inv.org_role} /></td>
                  <td className="px-4 py-2 text-[#7E91A8] text-xs">
                    {teamName(inv.team_id) ? <>{teamName(inv.team_id)} <span className="text-[#3D5068]">({inv.team_role})</span></> : '—'}
                  </td>
                  <td className="px-4 py-2 text-[#3D5068] text-xs">{new Date(inv.created_at).toLocaleDateString()}</td>
                  {canManage && (
                    <td className="px-4 py-2">
                      <button onClick={() => revoke(inv.id)} disabled={busyId === inv.id} title="Revoke invite" className={iconBtnCls}>
                        <Trash2 size={14} />
                      </button>
                    </td>
                  )}
                </tr>
              ))}
              {invites.length === 0 && (
                <tr><td colSpan={5} className="px-4 py-6 text-center text-[#3D5068] text-sm">No pending invites</td></tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
