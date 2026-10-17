import { useState, useEffect, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { ArrowLeft, Loader2, ShieldCheck, UserPlus, Trash2, KeyRound, ShieldOff, Power } from 'lucide-react'
import { AccountBadges } from '../components/admin'
import { AuditLogPanel } from '../components/AuditLogPanel'
import { api, authAdminApi } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import type { AmpUser, AuthAdminUser } from '../types'

// Admin-only page: manage Dex login credentials (amp-authadmin) and role
// assignment (amp-api). Two separate systems on purpose — Dex remains the
// sole source of truth for passwords; amp-api only tracks role assignment
// for JIT-provisioned identities.
export function Users() {
  const { me, loading: authLoading, isAdmin } = useAuth()

  const [ampUsers, setAmpUsers] = useState<AmpUser[]>([])
  const [credentials, setCredentials] = useState<AuthAdminUser[]>([])
  const [roleError, setRoleError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const [newEmail, setNewEmail] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [newName, setNewName] = useState('')
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)

  const [busyEmail, setBusyEmail] = useState<string | null>(null)
  const [resetTarget, setResetTarget] = useState<string | null>(null)
  const [resetPassword, setResetPassword] = useState('')

  const reload = useCallback(() => {
    setLoading(true)
    // Loaded separately: one failing (e.g. amp-authadmin down) shouldn't hide the other.
    Promise.allSettled([api.listUsers(), authAdminApi.listCredentials()])
      .then(([users, creds]) => {
        if (users.status === 'fulfilled') setAmpUsers(users.value)
        if (creds.status === 'fulfilled') setCredentials(creds.value)
        const failed = [users, creds].find(r => r.status === 'rejected') as PromiseRejectedResult | undefined
        setError(failed ? String(failed.reason?.message ?? failed.reason) : null)
      })
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    if (isAdmin) reload()
  }, [isAdmin, reload])

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    setCreating(true)
    setCreateError(null)
    try {
      await authAdminApi.createCredential(newEmail.trim().toLowerCase(), newPassword, newName.trim() || undefined)
      setNewEmail('')
      setNewPassword('')
      setNewName('')
      reload()
    } catch (e) {
      setCreateError(e instanceof Error ? e.message : 'Create failed')
    } finally {
      setCreating(false)
    }
  }

  async function handleDelete(email: string) {
    if (!confirm(`Remove login credential for ${email}? They will no longer be able to sign in.`)) return
    setBusyEmail(email)
    try {
      await authAdminApi.deleteCredential(email)
      reload()
    } catch (e) {
      alert(e instanceof Error ? e.message : 'Delete failed')
    } finally {
      setBusyEmail(null)
    }
  }

  async function handleResetSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!resetTarget) return
    setBusyEmail(resetTarget)
    try {
      await authAdminApi.resetPassword(resetTarget, resetPassword)
      setResetTarget(null)
      setResetPassword('')
    } catch (e) {
      alert(e instanceof Error ? e.message : 'Reset failed')
    } finally {
      setBusyEmail(null)
    }
  }

  async function toggleActive(u: AmpUser) {
    const deactivate = !u.disabled_at
    if (deactivate && !confirm(`Deactivate ${u.email}? Every request from this account (sign-ins and API tokens) will be refused, and it can't be given work. Its tickets and history stay.`)) return
    setBusyEmail(u.email)
    setRoleError(null)
    try {
      await (deactivate ? api.deactivateUser(u.id) : api.reactivateUser(u.id))
      reload()
    } catch (e) {
      setRoleError(e instanceof Error ? e.message : 'Update failed')
    } finally {
      setBusyEmail(null)
    }
  }

  async function toggleAdmin(u: AmpUser) {
    const grant = !u.roles.includes('admin')
    setBusyEmail(u.email)
    setRoleError(null)
    try {
      await api.setUserRole(u.id, 'admin', grant)
      reload()
    } catch (e) {
      setRoleError(e instanceof Error ? e.message : 'Update failed')
    } finally {
      setBusyEmail(null)
    }
  }

  if (authLoading) {
    return (
      <div className="min-h-screen flex items-center justify-center" style={{ background: '#08101F' }}>
        <Loader2 size={22} className="text-[#6366F1] animate-spin" />
      </div>
    )
  }

  if (!isAdmin) {
    return (
      <div className="min-h-screen flex flex-col items-center justify-center gap-3" style={{ background: '#08101F' }}>
        <ShieldOff size={28} className="text-[#3D5068]" />
        <p className="text-sm text-[#7E91A8]">
          {me ? 'Your account does not have admin access.' : 'Sign in required.'}
        </p>
        <Link to="/" className="text-xs text-[#818CF8] hover:underline">← Back to projects</Link>
      </div>
    )
  }

  return (
    <div className="min-h-screen" style={{ background: '#08101F' }}>
      <header style={{ background: '#0D1726', borderBottom: '1px solid #1E2C45' }}>
        <div className="max-w-4xl mx-auto px-6 py-4 flex items-center gap-3">
          <Link to="/" className="text-[#3D5068] hover:text-[#DDE6F0] transition-colors">
            <ArrowLeft size={16} />
          </Link>
          <ShieldCheck size={16} className="text-[#818CF8]" />
          <h1 className="text-base font-bold text-[#DDE6F0]">Users &amp; Access</h1>
        </div>
      </header>

      <main className="max-w-4xl mx-auto px-6 py-10 space-y-10">
        {error && (
          <div className="rounded-xl border border-[#EF4444]/20 bg-[#EF4444]/8 px-4 py-3 text-sm text-[#F87171]">
            {error}
          </div>
        )}

        {/* Create new login */}
        <section>
          <h2 className="text-sm font-bold text-[#DDE6F0] mb-3">Add a user</h2>
          <form onSubmit={handleCreate} className="flex flex-wrap items-start gap-2">
            <input
              type="email" required placeholder="email@example.com" value={newEmail}
              onChange={e => setNewEmail(e.target.value)}
              className="px-3 py-2 rounded-lg text-sm bg-[#0D1726] border border-[#1E2C45] text-[#DDE6F0] placeholder-[#3D5068] outline-none focus:border-[#6366F1]"
            />
            <input
              type="text" placeholder="Display name (optional)" value={newName}
              onChange={e => setNewName(e.target.value)}
              className="px-3 py-2 rounded-lg text-sm bg-[#0D1726] border border-[#1E2C45] text-[#DDE6F0] placeholder-[#3D5068] outline-none focus:border-[#6366F1]"
            />
            <input
              type="password" required placeholder="Temporary password (min 8 chars)" value={newPassword}
              onChange={e => setNewPassword(e.target.value)}
              minLength={8}
              className="px-3 py-2 rounded-lg text-sm bg-[#0D1726] border border-[#1E2C45] text-[#DDE6F0] placeholder-[#3D5068] outline-none focus:border-[#6366F1]"
            />
            <button
              type="submit" disabled={creating}
              className="flex items-center gap-1.5 px-3 py-2 rounded-lg text-sm font-semibold text-white disabled:opacity-50"
              style={{ background: '#6366F1' }}
            >
              {creating ? <Loader2 size={14} className="animate-spin" /> : <UserPlus size={14} />}
              Create
            </button>
          </form>
          {createError && <p className="text-xs text-[#F87171] mt-2">{createError}</p>}
          <p className="text-xs text-[#3D5068] mt-2">
            Credentials are stored in Dex, never in amp-api. New users get the <code>member</code> role by default —
            make them a global admin below once they've signed in at least once (most people don't need it — add them to an org and team instead).
          </p>
        </section>

        {/* Credentials table */}
        <section>
          <h2 className="text-sm font-bold text-[#DDE6F0] mb-3">Login credentials (Dex)</h2>
          {loading ? (
            <Loader2 size={18} className="text-[#6366F1] animate-spin" />
          ) : (
            <div className="rounded-xl border border-[#1E2C45] overflow-hidden">
              <table className="w-full text-sm">
                <thead>
                  <tr className="text-left text-[#3D5068] text-xs" style={{ background: '#0D1726' }}>
                    <th className="px-4 py-2 font-semibold">Email</th>
                    <th className="px-4 py-2 font-semibold">Username</th>
                    <th className="px-4 py-2 font-semibold w-40">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {credentials.map(c => (
                    <tr key={c.email} className="border-t border-[#1E2C45]">
                      <td className="px-4 py-2 text-[#DDE6F0]">{c.email}</td>
                      <td className="px-4 py-2 text-[#7E91A8]">{c.username}</td>
                      <td className="px-4 py-2">
                        <div className="flex items-center gap-2">
                          <button
                            onClick={() => { setResetTarget(c.email); setResetPassword('') }}
                            title="Reset password"
                            className="text-[#7E91A8] hover:text-[#818CF8]"
                          >
                            <KeyRound size={14} />
                          </button>
                          <button
                            onClick={() => handleDelete(c.email)}
                            disabled={busyEmail === c.email}
                            title="Remove login"
                            className="text-[#7E91A8] hover:text-[#F87171] disabled:opacity-50"
                          >
                            <Trash2 size={14} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                  {credentials.length === 0 && (
                    <tr><td colSpan={3} className="px-4 py-6 text-center text-[#3D5068] text-sm">No credentials yet</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          )}

          {resetTarget && (
            <form onSubmit={handleResetSubmit} className="mt-3 flex items-center gap-2">
              <span className="text-xs text-[#7E91A8]">Reset password for {resetTarget}:</span>
              <input
                type="password" required minLength={8} placeholder="New password" value={resetPassword}
                onChange={e => setResetPassword(e.target.value)}
                className="px-3 py-1.5 rounded-lg text-sm bg-[#0D1726] border border-[#1E2C45] text-[#DDE6F0] outline-none focus:border-[#6366F1]"
              />
              <button type="submit" className="px-3 py-1.5 rounded-lg text-xs font-semibold text-white" style={{ background: '#6366F1' }}>
                Save
              </button>
              <button type="button" onClick={() => setResetTarget(null)} className="text-xs text-[#3D5068] hover:text-[#7E91A8]">
                Cancel
              </button>
            </form>
          )}
        </section>

        {/* Roles table */}
        <section>
          <h2 className="text-sm font-bold text-[#DDE6F0] mb-3">People, agents &amp; global admins</h2>
          <p className="text-xs text-[#3D5068] mb-3">
            People appear here the first time they sign in. A <strong>global admin</strong> manages the whole platform:
            logins on this page, creating orgs, and every org, team and project — in the UI and through their agents&apos;
            MCP admin tools. It takes effect on their next request; no re-login needed. Org and team roles are set per org
            on the <a href="/admin/orgs" className="text-[#818CF8] hover:underline">Orgs</a> page.
            Deactivating an account refuses everything it does (including its API tokens) without deleting its work;
            agent accounts are created on <a href="/access" className="text-[#818CF8] hover:underline">Agents &amp; tokens</a>.
          </p>
          {roleError && (
            <p className="text-xs text-[#F87171] mb-3">{roleError}</p>
          )}
          <div className="rounded-xl border border-[#1E2C45] overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-[#3D5068] text-xs" style={{ background: '#0D1726' }}>
                  <th className="px-4 py-2 font-semibold">Email</th>
                  <th className="px-4 py-2 font-semibold">Name</th>
                  <th className="px-4 py-2 font-semibold">Access</th>
                  <th className="px-4 py-2 font-semibold w-44">Actions</th>
                </tr>
              </thead>
              <tbody>
                {ampUsers.map(u => (
                  <tr key={u.id} className="border-t border-[#1E2C45]">
                    <td className="px-4 py-2 text-[#DDE6F0]">
                      <span className="flex items-center gap-1.5 flex-wrap">{u.email}<AccountBadges kind={u.kind} disabled={!!u.disabled_at} /></span>
                    </td>
                    <td className="px-4 py-2 text-[#7E91A8]">{u.display_name || '—'}</td>
                    <td className="px-4 py-2">
                      {u.roles.includes('admin') ? (
                        <span
                          className="text-[10px] font-semibold px-1.5 py-0.5 rounded-md"
                          style={{ background: 'rgba(99,102,241,0.12)', color: '#818CF8', border: '1px solid rgba(99,102,241,0.22)' }}
                        >
                          global admin
                        </span>
                      ) : (
                        <span className="text-[10px] text-[#3D5068]">member</span>
                      )}
                    </td>
                    <td className="px-4 py-2">
                      <div className="flex items-center gap-3">
                        {u.kind !== 'agent' && (
                          <button
                            onClick={() => toggleAdmin(u)}
                            disabled={busyEmail === u.email}
                            className="text-xs font-semibold text-[#818CF8] hover:underline disabled:opacity-50 whitespace-nowrap"
                          >
                            {u.roles.includes('admin') ? 'Revoke global admin' : 'Make global admin'}
                          </button>
                        )}
                        {u.id !== me?.user_id && (
                          <button
                            onClick={() => toggleActive(u)}
                            disabled={busyEmail === u.email}
                            title={u.disabled_at ? 'Reactivate' : 'Deactivate'}
                            className={`flex items-center gap-1 text-xs font-semibold disabled:opacity-50 whitespace-nowrap ${u.disabled_at ? 'text-[#34D399]' : 'text-[#7E91A8] hover:text-[#F87171]'}`}
                          >
                            <Power size={12} />{u.disabled_at ? 'Reactivate' : 'Deactivate'}
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                ))}
                {ampUsers.length === 0 && (
                  <tr><td colSpan={4} className="px-4 py-6 text-center text-[#3D5068] text-sm">No one has signed in yet</td></tr>
                )}
              </tbody>
            </table>
          </div>
        </section>

        <section>
          <h2 className="text-sm font-bold text-[#DDE6F0] mb-1">Audit log</h2>
          <p className="text-xs text-[#3D5068] mb-3">
            Every change to access, accounts, API tokens, settings, and webhooks, and every deletion, across all orgs.
            Org admins see their org's on the Orgs page; project owners see their project's in its settings.
          </p>
          <AuditLogPanel />
        </section>
      </main>
    </div>
  )
}
