import { PageHeader } from '../components/ui/PageHeader'
import { useState, useEffect, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { Loader2, Plus, Trash2, Pencil, UserPlus, Users, Mail, ChevronDown, ChevronRight, X, Check, BookOpen } from 'lucide-react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { ConfirmDeleteModal } from '../components/ConfirmDeleteModal'
import { AccountBadges } from '../components/admin'
import { AuditLogPanel } from '../components/AuditLogPanel'
import { WebhooksPanel } from '../components/WebhooksPanel'
import { personName } from '../lib/people'
import type { AddMemberResult, Invite, Member, Org, OrgRole, Team, TeamRole } from '../types'
import { BandLoader } from '../components/brand/BandLoader'

// Orgs → Teams → Users administration. Controls are hidden or disabled based
// on the caller's org role (my_role) and global admin status, but the API
// stays authoritative: every action surfaces its error inline.

const fieldCls =
  'px-3 py-2 rounded-lg text-sm bg-surface border border-line text-ink placeholder-ink-3 outline-none focus:border-primary'
const smallSelectCls =
  'bg-bg border border-line rounded-lg px-2 py-1 text-xs text-ink focus:outline-none focus:border-primary cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed'
const primaryBtnCls =
  'flex items-center gap-1.5 px-3 py-2 rounded-lg text-sm font-semibold text-white disabled:opacity-50 cursor-pointer'
const iconBtnCls = 'text-ink-2 hover:text-danger-ink disabled:opacity-50 cursor-pointer transition-colors'

const errMsg = (e: unknown, fallback: string) => e instanceof Error ? e.message : fallback

function addedMessage(email: string, r: AddMemberResult) {
  return r.invited
    ? `Invited ${email} — they'll be added when they first sign in.`
    : `Added ${email}.`
}

function RoleBadge({ role }: { role: string }) {
  return (
    <span className="font-label text-[10px] font-semibold uppercase tracking-[0.1em] px-1.5 py-[3px] border border-line-str text-ink bg-surface">
      {role || 'none'}
    </span>
  )
}

function Notice({ error, info }: { error?: string | null; info?: string | null }) {
  if (error) return <p className="m-0 text-[12.5px] text-danger-ink mt-2">{error}</p>
  if (info) return <p className="m-0 text-[12.5px] text-success-ink mt-2">{info}</p>
  return null
}

export function Orgs() {
  const { me, loading: authLoading, isAdmin } = useAuth()
  const [orgs, setOrgs] = useState<Org[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [selectedId, setSelectedId] = useState<number | null>(null)

  const [newOrg, setNewOrg] = useState({ name: '', slug: '', owner: '' })
  const [showNew, setShowNew] = useState(false)
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
      <div className="min-h-full flex items-center justify-center" style={{ background: 'var(--bg)' }}>
        <BandLoader label="Loading teams and orgs" />
      </div>
    )
  }

  return (
    <div className="min-h-full" style={{ background: 'var(--bg)' }}>
      <PageHeader crumb={isAdmin ? 'Admin' : 'Organization'} title="Teams & orgs"
        meta="Orgs own projects; teams are how people and agents get access to them."
        actions={isAdmin ? <button type="button" className="amp-btn amp-btn-primary" onClick={() => setShowNew(v => !v)} aria-expanded={showNew}><Plus size={12} />New org</button> : undefined} />

      <main className="px-7 py-5 grid gap-4">
        {error && (
          <div className="amp-panel px-4 py-3 text-[13px] text-danger-ink">
            {error}
          </div>
        )}

        {isAdmin && showNew && (
          <section className="amp-panel p-4 grid gap-3">
            <span className="amp-label">New organization</span>
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
              <button type="submit" disabled={creating || !newOrg.name.trim()} className={primaryBtnCls}>
                {creating ? <Loader2 size={14} className="animate-spin" /> : <Plus size={14} />}
                Create
              </button>
            </form>
            <Notice error={createError} info={createInfo} />
          </section>
        )}

        <section className="grid grid-cols-1 md:grid-cols-[240px_minmax(0,1fr)] gap-4 items-start">
          {/* Org list */}
          <div className="amp-panel">
            <div className="px-4 py-3 border-b border-line"><span className="amp-label">{isAdmin ? 'All organizations' : 'My organizations'}</span></div>
            {loading ? (
              <div className="p-4"><Loader2 size={16} className="text-ink-3 animate-spin" /></div>
            ) : orgs.length === 0 ? (
              <p className="m-0 p-4 text-[12.5px] text-ink-2">
                {isAdmin ? 'No organizations yet.' : "You're not a member of any organization yet."}
              </p>
            ) : (
              <div>
                {orgs.map(o => {
                  const active = o.id === selectedId
                  return (
                    <button
                      key={o.id}
                      onClick={() => setSelectedId(o.id)}
                      aria-current={active ? 'true' : undefined}
                      className={`w-full text-left px-4 py-2.5 border-b border-line last:border-b-0 cursor-pointer ${active ? 'bg-[color-mix(in_srgb,var(--accent)_6%,var(--surface))]' : 'hover:bg-surface-2'}`}
                      style={active ? { boxShadow: 'inset 3px 0 0 var(--accent)' } : undefined}
                    >
                      <div className="text-[13.5px] font-semibold truncate text-ink">{o.name}</div>
                      <div className="flex items-center gap-1.5 mt-0.5">
                        <span className="amp-num text-[10.5px] text-ink-3 truncate">{o.slug}</span>
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

type Tab = 'members' | 'teams' | 'invites' | 'webhooks' | 'audit'

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
    <div className="amp-panel amp-panel-top">
      <div className="flex items-center gap-3 px-5 pt-4 pb-3">
        {renaming ? (
          <div className="flex items-center gap-2 flex-1">
            <input autoFocus value={name} onChange={e => setName(e.target.value)} className={`${fieldCls} flex-1`} />
            <button onClick={rename} disabled={busy || !name.trim()} className="amp-btn amp-btn-primary amp-btn-sm" title="Save" aria-label="Save name">
              <Check size={14} />
            </button>
            <button onClick={() => { setRenaming(false); setName(org.name) }} className="amp-btn amp-btn-ghost amp-btn-sm" title="Cancel" aria-label="Cancel rename">
              <X size={14} />
            </button>
          </div>
        ) : (
          <>
            <div className="min-w-0 flex-1">
              <span className="amp-label">Organization · <span className="amp-num tracking-normal">{org.slug}</span></span>
              <h2 className="amp-heading m-0 mt-1.5 text-[20px] text-ink truncate">{org.name}</h2>
            </div>
            <Link to={`/orgs/${org.id}/kb`} title="The org's knowledge base: brand, process, standards"
              className="amp-btn amp-btn-sm">
              <BookOpen size={12} />Org KB
            </Link>
            {canManage && (
              <button onClick={() => setRenaming(true)} className="amp-btn amp-btn-ghost amp-btn-sm" title="Rename org" aria-label="Rename org">
                <Pencil size={13} />
              </button>
            )}
            {canGrantOwner && (
              <button onClick={() => { setError(null); setConfirmDelete(true) }} className="amp-btn amp-btn-ghost amp-btn-sm hover:!text-danger-ink" title="Delete org" aria-label="Delete org">
                <Trash2 size={13} />
              </button>
            )}
          </>
        )}
      </div>
      {error && !confirmDelete && <p className="m-0 px-5 pb-3 text-[12.5px] text-danger-ink">{error}</p>}

      <div className="flex border-b border-line px-3">
        {(canManage ? ['members', 'teams', 'invites', 'webhooks', 'audit'] as const : ['members', 'teams', 'invites'] as const).map(t => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`font-label text-[12px] font-semibold uppercase tracking-[0.16em] px-3.5 pt-2.5 pb-2 border-b-2 -mb-px cursor-pointer ${
              tab === t ? 'border-accent text-ink' : 'border-transparent text-ink-3 hover:text-ink-2'
            }`}
          >
            {t === 'invites' ? 'Pending invites' : t === 'audit' ? 'Audit log' : t}
          </button>
        ))}
      </div>

      <div className="p-5">
      {tab === 'members' && <OrgMembers org={org} canManage={canManage} canGrantOwner={canGrantOwner} meId={meId} />}
      {tab === 'teams' && <OrgTeams org={org} canManage={canManage} meId={meId} />}
      {tab === 'invites' && <OrgInvites org={org} canManage={canManage} />}
      {tab === 'webhooks' && (
        <div className="space-y-3">
          <p className="text-xs text-ink-2 leading-relaxed">
            Org webhooks fire for every project in {org.name}. Pick "Audit log entries" to stream membership, access, and deletion changes to a SIEM or a Slack channel.
          </p>
          <WebhooksPanel orgId={org.id} canManage={canManage} />
        </div>
      )}
      {tab === 'audit' && <AuditLogPanel orgId={org.id} />}
      </div>

      {confirmDelete && (
        <ConfirmDeleteModal
          title={`Delete "${org.name}"`}
          description="This permanently deletes the organization, its teams, memberships, and their knowledge bases."
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
          <button type="submit" disabled={adding} className={primaryBtnCls}>
            {adding ? <Loader2 size={14} className="animate-spin" /> : <UserPlus size={14} />}
            Add
          </button>
        </form>
      )}
      <Notice error={error} info={info} />

      {loading ? (
        <Loader2 size={18} className="text-primary animate-spin mt-3" />
      ) : (
        <div className="border border-line overflow-hidden mt-3">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left bg-surface-2">
                <th className="amp-label px-4 py-2.5 border-b border-line">Email</th>
                <th className="amp-label px-4 py-2.5 border-b border-line">Name</th>
                <th className="amp-label px-4 py-2.5 border-b border-line w-32">Role</th>
                {canManage && <th className="amp-label px-4 py-2.5 border-b border-line w-12" />}
              </tr>
            </thead>
            <tbody>
              {members.map(m => {
                // Only owners may grant or take away the owner role.
                const editable = canManage && (canGrantOwner || m.role !== 'owner')
                return (
                  <tr key={m.user_id} className="border-t border-line">
                    <td className="px-4 py-2 text-ink">
                      <span className="flex items-center gap-1.5 flex-wrap">
                        <span>{m.email}{m.user_id === meId && <span className="text-ink-3"> (you)</span>}</span>
                        <AccountBadges kind={m.kind} disabled={m.disabled} />
                      </span>
                    </td>
                    <td className="px-4 py-2 text-ink-2">{m.display_name || '—'}</td>
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
                <tr><td colSpan={4} className="px-4 py-6 text-center text-ink-3 text-sm">No members</td></tr>
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
          <button type="submit" disabled={creating || !newName.trim()} className={primaryBtnCls}>
            {creating ? <Loader2 size={14} className="animate-spin" /> : <Plus size={14} />}
            Create team
          </button>
        </form>
      )}
      <Notice error={error} />

      {loading ? (
        <Loader2 size={18} className="text-primary animate-spin mt-3" />
      ) : (
        <div className="border border-line overflow-hidden mt-3">
          {teams.length === 0 && <div className="px-4 py-6 text-center text-ink-3 text-sm">No teams yet</div>}
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
    <div className={first ? '' : 'border-t border-line'}>
      <div className="flex items-center gap-2 px-4 py-2.5">
        <button onClick={onToggle} className="flex items-center gap-2 flex-1 min-w-0 text-left cursor-pointer">
          <span className="text-ink-3">{expanded ? <ChevronDown size={13} /> : <ChevronRight size={13} />}</span>
          <Users size={13} className="text-ink-3 flex-shrink-0" />
          {!renaming && <span className="text-sm text-ink truncate">{team.name}</span>}
        </button>
        {!renaming && (
          <Link to={`/teams/${team.id}/kb`} title="The team's knowledge base (team members only)"
            className="amp-btn amp-btn-ghost amp-btn-sm">
            <BookOpen size={12} />KB
          </Link>
        )}
        {renaming ? (
          <div className="flex items-center gap-1.5">
            <input autoFocus value={name} onChange={e => setName(e.target.value)} className={`${fieldCls} py-1`} />
            <button onClick={rename} disabled={busy || !name.trim()} className="p-1 text-success cursor-pointer disabled:opacity-50" title="Save"><Check size={13} /></button>
            <button onClick={() => { setRenaming(false); setName(team.name) }} className="p-1 text-ink-2 cursor-pointer" title="Cancel"><X size={13} /></button>
          </div>
        ) : canManageOrg && (
          <>
            <button onClick={() => setRenaming(true)} className="p-1 text-ink-2 hover:text-ink cursor-pointer" title="Rename team"><Pencil size={12} /></button>
            <button onClick={() => { setError(null); setConfirmDelete(true) }} className={`p-1 ${iconBtnCls}`} title="Delete team"><Trash2 size={13} /></button>
          </>
        )}
      </div>
      {error && !confirmDelete && <p className="text-xs text-danger-ink px-4 pb-2">{error}</p>}
      {expanded && <TeamMembers teamId={team.id} canManageOrg={canManageOrg} meId={meId} />}
      {confirmDelete && (
        <ConfirmDeleteModal
          title={`Delete team "${team.name}"`}
          description="This also deletes the team's knowledge base. A team that is a project's only owner team can't be deleted."
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
    <div className="px-4 pb-4 pl-10 space-y-3" style={{ background: 'var(--surface-2)' }}>
      {loading ? (
        <Loader2 size={16} className="text-primary animate-spin" />
      ) : (
        <>
          <div className="space-y-1 pt-2">
            {members.length === 0 && <p className="text-xs text-ink-3">No members</p>}
            {members.map(m => (
              <div key={m.user_id} className="flex items-center gap-3 py-1">
                <span className="text-sm text-ink flex-1 truncate" title={m.email}>
                  {personName(m)} <span className="text-xs text-ink-3">{m.email}{m.user_id === meId ? ' (you)' : ''}</span>
                  {' '}<AccountBadges kind={m.kind} disabled={m.disabled} />
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
              <button type="submit" disabled={adding} className={`${primaryBtnCls} py-1.5`} style={{ background: 'var(--primary)' }}>
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
      <p className="text-xs text-ink-3 mb-3">
        Invites are created when you add someone by an email that hasn't signed in yet. They're accepted automatically on first sign-in.
      </p>
      <Notice error={error} />
      {loading ? (
        <Loader2 size={18} className="text-primary animate-spin mt-3" />
      ) : (
        <div className="border border-line overflow-hidden mt-3">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left bg-surface-2">
                <th className="amp-label px-4 py-2.5 border-b border-line">Email</th>
                <th className="amp-label px-4 py-2.5 border-b border-line">Org role</th>
                <th className="amp-label px-4 py-2.5 border-b border-line">Team</th>
                <th className="amp-label px-4 py-2.5 border-b border-line">Sent</th>
                {canManage && <th className="amp-label px-4 py-2.5 border-b border-line w-12" />}
              </tr>
            </thead>
            <tbody>
              {invites.map(inv => (
                <tr key={inv.id} className="border-t border-line">
                  <td className="px-4 py-2 text-ink">
                    <span className="flex items-center gap-1.5"><Mail size={12} className="text-ink-3" />{inv.email}</span>
                  </td>
                  <td className="px-4 py-2"><RoleBadge role={inv.org_role} /></td>
                  <td className="px-4 py-2 text-ink-2 text-xs">
                    {teamName(inv.team_id) ? <>{teamName(inv.team_id)} <span className="text-ink-3">({inv.team_role})</span></> : '—'}
                  </td>
                  <td className="px-4 py-2 text-ink-3 text-xs">{new Date(inv.created_at).toLocaleDateString()}</td>
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
                <tr><td colSpan={5} className="px-4 py-6 text-center text-ink-3 text-sm">No pending invites</td></tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
